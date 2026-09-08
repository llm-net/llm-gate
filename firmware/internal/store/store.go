// Package store 是 llmgate 的 SQLite 持久层（modernc.org/sqlite，纯 Go，
// 满足 CGO_ENABLED=0 静态编译硬约束），承载 API密钥/会话/审计三张表、
// 上游账户/模型/模型来源三张表、单值配置 settings（登录口令哈希也在这里，
// 键名 SettingAdminPasswordHash）、异步任务 aigc_tasks、
// 用量小时聚合 usage_hourly（计量的唯一持久层，见 usage.go）与 Agents 订阅
// 凭据 agent_accounts（见 agents.go）。
//
// 设计要点：
//
//   - 库文件 <data_dir>/llmgate.db，权限 0600；父目录必须已存在（创建目录是
//     部署的职责，不是本包的）。
//   - 同目录的 <data_dir>/device-key（0600）是上游凭证的封存密钥，首次
//     Open 时生成，细节见 seal.go。
//   - 每个连接经 DSN PRAGMA 统一设置 journal_mode=WAL、foreign_keys=ON、
//     busy_timeout=5000。
//   - 迁移前向单向：embed 的 migrations/*.sql 按版本号升序逐个在独立事务中
//     应用，schema_migrations 记录版本与应用时间；不支持回滚迁移。带
//     rebuildDirective 标记的重建型迁移在外键关闭的独占连接上执行
//     （见 applyRebuildMigration；首例 0005，之后 0009/0010 各扩一次
//     upstreams.type 的 CHECK 集合）。0019 删掉「用户」概念时是清空重建：
//     子表先 DROP，父表 users 随后即无引用可挡，普通单事务路径就够。
//   - 审计表追加式（架构 §12）：本包只提供 AppendAudit，凡含 "Audit" 的
//     更新/删除方法一律不得添加（store_test.go 以反射断言此约束）。
//   - 时间戳存 UTC 固定宽度毫秒文本（timeLayout），字符串比较即时间比较，
//     过期清理的 SQL 比较依赖此性质。
//
// §15.1 纪律：本包入库的是摘要与哈希（key_digest/token_digest、settings 里的
// 登录口令 Argon2id 串）
// 与密文（api_key_sealed/plaintext_sealed 等，清单见 seal.go），错误信息只含
// 约束名与列名，不回显参数值；调用方不得把明文凭证写进 label/detail 等自由
// 文本字段。上游凭证明文只在 ResolveModelRoute 的返回值里出现，仅供进程内
// 路由注入出站请求头；客户端 API密钥明文只在 GetAPIKeyPlaintext 的返回值里
// 出现，仅供自助复制端点。
package store

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

// DBFileName 是数据目录下的库文件名；板上即 /var/lib/llmgate/llmgate.db。
const DBFileName = "llmgate.db"

var (
	// ErrNotFound 表示目标行不存在（查询未命中，或更新/删除未匹配到行）。
	ErrNotFound = errors.New("store: 记录不存在")
	// ErrConflict 表示唯一性冲突（Key/会话摘要重复、同一模型在
	// 同一上游重复挂来源）或引用冲突（外键：引用了不存在的父行，或父行仍被
	// 引用而不能删——见 mapErr）。
	ErrConflict = errors.New("store: 唯一性冲突")
	// ErrKeyUnreadable 表示上游凭证密文解不开（设备密钥被替换或密文损坏），
	// 补救动作是在管理台重新录入该上游的 Key。只有明确要拿明文的读取路径
	// （GetSourceRoute）返回它；列表视图对同一情况留空 last4 不报错。
	ErrKeyUnreadable = errors.New("store: 上游凭证不可解密")
	// ErrInvalidPricing 表示模型目录价 JSON 不合法（见 validatePricing）。
	// 它是**调用方输入错误**而非故障：管理端点必须据此回 400 并把原因转述给
	// 管理员，绝不能落进 internalError 的 500 + ERROR 日志那条通道——一个把
	// 价格写成小数的管理员不该看见「服务内部错误」。
	ErrInvalidPricing = errors.New("store: 模型目录价不合法")
)

// timeLayout 是入库时间戳的固定宽度格式：UTC 毫秒精度，恒以 "Z" 结尾。
// 固定宽度保证字典序等于时间序；写入方必须先转 UTC（见 fmtTime）。
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// fmtTime 把 t 规格化为入库文本（UTC + timeLayout）。
func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// parseTime 解析入库文本回 time.Time；库内容合法时不失败。
func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 是打开的数据库句柄与全套 prepared statement。
// 方法均并发安全（database/sql 保证），Close 后不可再用。
type Store struct {
	db *sql.DB
	// aead 是设备密钥派生的 AES-256-GCM，封存/解出上游凭证（见 seal.go）。
	aead cipher.AEAD
	// stmts 是 prepare 建好的全部语句，Close 时统一释放——新增语句只需在
	// prepare 里登记一次，不会漏关。
	stmts []*sql.Stmt

	stmtCreateAPIKey       *sql.Stmt
	stmtListAPIKeys        *sql.Stmt
	stmtGetAPIKeyByID      *sql.Stmt
	stmtGetAPIKeyPlaintext *sql.Stmt
	stmtSetAPIKeyLabel     *sql.Stmt
	stmtSetAPIKeyDisabled  *sql.Stmt
	stmtSetAPIKeyLimits    *sql.Stmt
	stmtDrainKeyAllowance  *sql.Stmt
	stmtDeleteAPIKey       *sql.Stmt
	stmtLookupKeyByDigest  *sql.Stmt
	stmtTouchKeyLastUsed   *sql.Stmt

	stmtCreateSession         *sql.Stmt
	stmtGetSessionByDigest    *sql.Stmt
	stmtDeleteSession         *sql.Stmt
	stmtDeleteAllSessions     *sql.Stmt
	stmtDeleteExpiredSessions *sql.Stmt

	stmtAppendAudit *sql.Stmt

	stmtGetSetting *sql.Stmt
	stmtSetSetting *sql.Stmt

	stmtCreateUpstream        *sql.Stmt
	stmtListUpstreams         *sql.Stmt
	stmtGetUpstreamByID       *sql.Stmt
	stmtGetRouteUpstreamByID  *sql.Stmt
	stmtUpdateUpstream        *sql.Stmt
	stmtUpdateUpstreamWithKey *sql.Stmt
	stmtSetUpstreamDisabled   *sql.Stmt
	stmtSetUpstreamKey        *sql.Stmt
	stmtSetUpstreamEgressMode *sql.Stmt
	stmtDeleteUpstream        *sql.Stmt

	stmtCreateModel      *sql.Stmt
	stmtGetModelByID     *sql.Stmt
	stmtGetModelByName   *sql.Stmt
	stmtRenameModel      *sql.Stmt
	stmtSetModelDisabled *sql.Stmt
	stmtSetModelPricing  *sql.Stmt
	stmtSetModelEntries  *sql.Stmt
	stmtSetModelFamily   *sql.Stmt
	stmtDeleteModel      *sql.Stmt

	stmtCreateModelSource      *sql.Stmt
	stmtGetModelSourceByID     *sql.Stmt
	stmtUpdateModelSource      *sql.Stmt
	stmtSetModelSourceDisabled *sql.Stmt
	stmtDeleteModelSource      *sql.Stmt

	stmtListModelsWithSources   *sql.Stmt
	stmtGetModelWithSources     *sql.Stmt
	stmtListModels              *sql.Stmt
	stmtResolveModelRoute       *sql.Stmt
	stmtGetSourceRoute          *sql.Stmt
	stmtListServableModels      *sql.Stmt
	stmtListServableModelSource *sql.Stmt
	stmtListServableAIGCSources *sql.Stmt

	stmtCreateAIGCTask            *sql.Stmt
	stmtGetAIGCTaskByVendorForKey *sql.Stmt
	stmtUpdateAIGCTaskObserved    *sql.Stmt
	stmtPruneAIGCTasks            *sql.Stmt
	stmtListUnsettledAIGCTasks    *sql.Stmt
	stmtSettleAIGCTask            *sql.Stmt

	stmtUpsertAgentAccount        *sql.Stmt
	stmtListAgentAccounts         *sql.Stmt
	stmtGetAgentAccountByID       *sql.Stmt
	stmtGetAgentAccountByProvider *sql.Stmt
	stmtGetAgentCredential        *sql.Stmt
	stmtSetAgentAuthJSON          *sql.Stmt
	stmtUpdateAgentAccount        *sql.Stmt
	stmtSetAgentStatus            *sql.Stmt
	stmtDeleteAgentAccount        *sql.Stmt

	stmtAddUsageDelta    *sql.Stmt
	stmtQueryUsageRange  *sql.Stmt
	stmtSumUsageSince    *sql.Stmt
	stmtPruneUsage       *sql.Stmt
	stmtGetModelPricing  *sql.Stmt
	stmtListModelPricing *sql.Stmt
}

// Open 打开（必要时创建）dataDir 下的 llmgate.db 与 device-key，应用未应用的
// 迁移并准备全部语句。dataDir 必须已存在；库文件以 0600 创建，已存在时也会
// 被收紧到 0600（WAL/SHM 附属文件继承库文件权限）。
func Open(dataDir string) (*Store, error) {
	fi, err := os.Stat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("数据目录 %s 不可用（须由部署预先创建）: %w", dataDir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("数据目录 %s 不是目录", dataDir)
	}

	// 设备密钥先于库文件就位：密钥不可用时不该留下半开的库句柄。
	deviceKey, err := loadOrCreateDeviceKey(dataDir)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(deviceKey)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dataDir, DBFileName)
	// 预创建空文件以钉死 0600：SQLite 视零长文件为全新库；
	// 已存在的库文件同样收紧权限（chmod 幂等）。
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建库文件: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("收紧库文件权限: %w", err)
	}

	db, err := sql.Open("sqlite", dbDSN(path))
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	// 板上规模（决策 5：每请求一次摘要点查）远用不满默认无上限的连接池；
	// 设小上限以约束内存并降低写锁竞争面。
	db.SetMaxOpenConns(4)

	s := &Store{db: db, aead: aead}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.prepare(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	if _, err := s.DeviceName(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// dbDSN 是库文件的连接串。DSN PRAGMA 对连接池里每个新连接生效
// （journal_mode 落库持久，foreign_keys/busy_timeout 是连接态，必须逐连接
// 设置）。假设：path 不含 '?' 与 '#'（部署约定 /var/lib/llmgate）。
// 测试构造存量库时也用它，保证升级测试跑在与产品完全相同的连接语义下。
func dbDSN(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
}

// Close 释放全部 prepared statement 并关闭数据库。
func (s *Store) Close() error {
	var errs []error
	for _, st := range s.stmts {
		if err := st.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.db.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// migrate 建 schema_migrations 表并按版本号升序应用未应用的迁移，
// 每个迁移一个事务（SQLite 的 DDL 可回滚，失败即整版本回退）。
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
		     version    INTEGER PRIMARY KEY,
		     applied_at TEXT NOT NULL
		 )`); err != nil {
		return fmt.Errorf("创建 schema_migrations: %w", err)
	}

	applied := map[int64]bool{}
	var maxApplied int64
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("读取已应用迁移: %w", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("读取已应用迁移: %w", err)
		}
		applied[v] = true
		if v > maxApplied {
			maxApplied = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取已应用迁移: %w", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		// 前向单向：未应用的版本号必须大于已应用的最大版本，
		// 否则说明迁移文件被乱序插入（历史被改写），拒绝启动。
		if m.version < maxApplied {
			return fmt.Errorf("迁移 %s 版本号小于已应用的最大版本 %d：迁移必须前向追加", m.name, maxApplied)
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
		maxApplied = m.version
	}
	return nil
}

// migration 是一个待应用的迁移文件。
type migration struct {
	version int64
	name    string
	sql     string
}

// loadMigrations 读 embed 的 migrations/*.sql，按文件名前导数字解析版本号
// 并升序排序；版本号重复视为打包错误。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}
	migs := make([]migration, 0, len(entries))
	seen := map[int64]string{}
	for _, e := range entries {
		name := e.Name()
		numPart, _, found := strings.Cut(name, "_")
		if !found {
			return nil, fmt.Errorf("迁移文件名 %s 非法：须为 <版本号>_<描述>.sql", name)
		}
		v, err := strconv.ParseInt(numPart, 10, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("迁移文件名 %s 非法：版本号须为正整数", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("迁移版本 %d 重复：%s 与 %s", v, prev, name)
		}
		seen[v] = name
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %s: %w", name, err)
		}
		migs = append(migs, migration{version: v, name: name, sql: string(raw)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

// rebuildDirective 标记「重建型迁移」（SQLite 不能修改既有表的 CHECK/约束，
// 只能新表建立→拷贝→DROP→改名，而 DROP 被引用的旧表要求外键关闭）：迁移
// 文件带此标记即走 applyRebuildMigration。首例是 0005 的 upstreams 重建，
// 处置细节的完整说明写在那份迁移的头注释里。
const rebuildDirective = "-- migrate:foreign_keys=off"

// hasRebuildDirective 只认独占一行的精确标记（除首尾空白外整行只有标记
// 本身）：后续迁移的普通注释里**提到**这个标记（比如「不同于 0005，本迁移
// 不用 …」）不会被误判成重建路径。
func hasRebuildDirective(sqlText string) bool {
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.TrimSpace(line) == rebuildDirective {
			return true
		}
	}
	return false
}

// applyMigration 应用一个迁移：带 rebuildDirective 的走外键关闭的重建路径，
// 其余在共用执行体里单事务完成。
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	if hasRebuildDirective(m.sql) {
		return s.applyRebuildMigration(ctx, m)
	}
	return runMigrationTx(ctx, s.db, m, false)
}

// txStarter 抽象 *sql.DB 与 *sql.Conn 的 BeginTx（两条迁移路径共用执行体）。
type txStarter interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// runMigrationTx 是两条迁移路径共用的执行体：单事务里执行迁移 SQL、
// （重建路径）全库外键校验、记录版本、提交；失败整体回滚。
func runMigrationTx(ctx context.Context, db txStarter, m migration, fkCheck bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("迁移 %s 开启事务: %w", m.name, err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是空操作
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("应用迁移 %s: %w", m.name, err)
	}
	if fkCheck {
		// 外键关闭期间 SQLite 不做任何引用校验，提交前全库自证：重建若拷丢
		// 了父行或引用，在这里失败回滚，而不是留一个悬空引用的库。
		violations, err := countFKViolations(ctx, tx)
		if err != nil {
			return fmt.Errorf("迁移 %s 外键校验: %w", m.name, err)
		}
		if violations > 0 {
			return fmt.Errorf("迁移 %s 外键校验失败：%d 行悬空引用，已回滚", m.name, violations)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, fmtTime(time.Now())); err != nil {
		return fmt.Errorf("记录迁移 %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 %s: %w", m.name, err)
	}
	return nil
}

// applyRebuildMigration 在 foreign_keys=OFF 下执行重建型迁移。
//
// PRAGMA foreign_keys 是连接态、且**在事务内是 no-op**，所以必须独占一条
// 连接（*sql.Conn 钉住物理连接）、在 BEGIN 之前关闭、COMMIT/ROLLBACK 之后
// 恢复；执行体（含提交前的全库外键校验）与普通路径共用 runMigrationTx。
// 恢复经 restoreForeignKeys 读回验证，失败即废弃该连接（driver.ErrBadConn
// 让连接池丢弃它）——绝不能让一条外键关闭的连接回池服务业务查询。
func (s *Store) applyRebuildMigration(ctx context.Context, m migration) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("迁移 %s 独占连接: %w", m.name, err)
	}
	defer func() {
		// 无论成败都恢复连接态；ctx 已取消也要恢复（WithoutCancel）。
		if perr := restoreForeignKeys(context.WithoutCancel(ctx), conn); perr != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn }) //nolint:errcheck // 只为标记连接损坏
			if err == nil {
				err = fmt.Errorf("迁移 %s 恢复 foreign_keys 失败（连接已废弃）: %w", m.name, perr)
			}
		}
		conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("迁移 %s 关闭 foreign_keys: %w", m.name, err)
	}
	return runMigrationTx(ctx, conn, m, true)
}

// restoreForeignKeys 把连接的 foreign_keys 恢复为 ON 并**读回验证**。
// 只信读回、不信 SET 的返回值：若执行体因回滚失败把事务留在打开状态
// （驱动级故障下可能发生），事务内的 PRAGMA foreign_keys=ON 是静默 no-op
// 且报成功——只有读回能揭穿，此时由调用方废弃整条连接。
func restoreForeignKeys(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	var fk int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return err
	}
	if fk != 1 {
		return fmt.Errorf("foreign_keys 读回为 %d（连接上可能残留未回滚的事务）", fk)
	}
	return nil
}

// countFKViolations 数 PRAGMA foreign_key_check 的结果行数（0 即全库引用完好）。
// 行内容（表名/rowid/父表/外键序号）只用于计数，不逐列解读。
func countFKViolations(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// billingRankSQL 是来源排序里的计费模式裁决位：同优先级下「订阅套餐」型
// 上游先于按量付费——订阅是已付费额度先用满，用量按 token 计费作兜底。
// billing_mode 是账号创建时的平台目录快照；空值分支只兼容没有快照的存量行。
const billingRankSQL = `CASE
	WHEN u.billing_mode = 'subscription' THEN 0
	WHEN u.billing_mode = '' AND u.type IN ('ark_plan', 'qwen_plan', 'opencode_go') THEN 0
	ELSE 1 END`

// prepare 一次性准备全部仓储语句；任何一条失败都视为 schema 与代码脱节。
func (s *Store) prepare(ctx context.Context) error {
	for _, p := range []struct {
		dst   **sql.Stmt
		query string
	}{
		{&s.stmtCreateAPIKey, `INSERT INTO api_keys (label, key_digest, display_prefix, display_last4, plaintext_sealed, created_at) VALUES (?, ?, ?, ?, ?, ?)`},
		{&s.stmtListAPIKeys, `SELECT ` + apiKeyColumns + ` FROM api_keys ORDER BY id`},
		{&s.stmtGetAPIKeyByID, `SELECT ` + apiKeyColumns + ` FROM api_keys WHERE id = ?`},
		// 自助复制端点的点查：只取解封所需两列（摘要作 AAD）。刻意独立于
		// apiKeyColumns——密文不进 APIKey 结构体，不随列表在库外流转。
		{&s.stmtGetAPIKeyPlaintext, `SELECT key_digest, plaintext_sealed FROM api_keys WHERE id = ?`},
		{&s.stmtSetAPIKeyLabel, `UPDATE api_keys SET label = ? WHERE id = ?`},
		{&s.stmtSetAPIKeyDisabled, `UPDATE api_keys SET disabled = ? WHERE id = ?`},
		{&s.stmtSetAPIKeyLimits, `UPDATE api_keys SET budget_day_micro = ?, budget_week_micro = ?, budget_month_micro = ?, rpm_limit = ? WHERE id = ?`},
		{&s.stmtDrainKeyAllowance, `UPDATE api_keys SET metered_allowance_micro =
				CASE WHEN metered_allowance_micro > ? THEN metered_allowance_micro - ? ELSE 0 END WHERE id = ?`},
		{&s.stmtDeleteAPIKey, `DELETE FROM api_keys WHERE id = ?`},
		// 数据面鉴权热路径：一次点查同时带回归属、启停位、四个限额列与
		// 按量额度剩余。准入因此零额外查询——加列不加查询次数是
		// 这条语句的设计约束，别把限额拆去第二条 SQL。0019 删掉用户概念后
		// 它退回单表点查，不再 JOIN。
		{&s.stmtLookupKeyByDigest, `SELECT id, display_prefix, display_last4, disabled,
				       budget_day_micro, budget_week_micro, budget_month_micro, rpm_limit,
				       metered_allowance_micro
				  FROM api_keys WHERE key_digest = ?`},
		{&s.stmtTouchKeyLastUsed, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`},

		{&s.stmtCreateSession, `INSERT INTO sessions (token_digest, created_at, expires_at, remote_ip) VALUES (?, ?, ?, ?)`},
		{&s.stmtGetSessionByDigest, `SELECT id, token_digest, created_at, expires_at, remote_ip FROM sessions WHERE token_digest = ?`},
		{&s.stmtDeleteSession, `DELETE FROM sessions WHERE token_digest = ?`},
		{&s.stmtDeleteAllSessions, `DELETE FROM sessions`},
		{&s.stmtDeleteExpiredSessions, `DELETE FROM sessions WHERE expires_at <= ?`},

		{&s.stmtAppendAudit, `INSERT INTO audit_events (at, event, entity, detail, remote_ip) VALUES (?, ?, ?, ?, ?)`},

		{&s.stmtCreateUpstream, `INSERT INTO upstreams (name, type, catalog_id, billing_mode, api_key_sealed, base_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`},
		{&s.stmtListUpstreams, `SELECT id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, egress_mode, created_at, updated_at FROM upstreams ORDER BY id`},
		{&s.stmtGetUpstreamByID, `SELECT id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, egress_mode, created_at, updated_at FROM upstreams WHERE id = ?`},
		{&s.stmtGetRouteUpstreamByID, `SELECT id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, egress_mode FROM upstreams WHERE id = ?`},
		{&s.stmtUpdateUpstream, `UPDATE upstreams SET name = ?, base_url = ?, updated_at = ? WHERE id = ?`},
		// 改址与换 Key 同一条 UPDATE（openai_compat 改址路径）：不许出现
		// 「旧 Key 配新地址」的中间态，见 UpdateUpstreamAddressAndKey。
		{&s.stmtUpdateUpstreamWithKey, `UPDATE upstreams SET name = ?, base_url = ?, api_key_sealed = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetUpstreamDisabled, `UPDATE upstreams SET disabled = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetUpstreamKey, `UPDATE upstreams SET api_key_sealed = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetUpstreamEgressMode, `UPDATE upstreams SET egress_mode = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtDeleteUpstream, `DELETE FROM upstreams WHERE id = ?`},

		{&s.stmtCreateModel, `INSERT INTO models (name, kind, pricing, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`},
		{&s.stmtGetModelByID, `SELECT ` + modelColumns + ` FROM models WHERE id = ?`},
		{&s.stmtGetModelByName, `SELECT ` + modelColumns + ` FROM models WHERE name = ?`},
		{&s.stmtRenameModel, `UPDATE models SET name = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetModelDisabled, `UPDATE models SET disabled = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetModelPricing, `UPDATE models SET pricing = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetModelEntries, `UPDATE models SET entry_openai = ?, entry_responses = ?, entry_anthropic = ?, updated_at = ? WHERE id = ?`},
		// family = '' 守卫即「建后不可改」（SetModelFamilyIfUnset 的文档）。
		{&s.stmtSetModelFamily, `UPDATE models SET family = ?, updated_at = ? WHERE id = ? AND family = ''`},
		{&s.stmtDeleteModel, `DELETE FROM models WHERE id = ?`},

		{&s.stmtCreateModelSource, `INSERT INTO model_sources (model_id, upstream_id, upstream_model_id, priority, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`},
		{&s.stmtGetModelSourceByID, `SELECT id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at FROM model_sources WHERE id = ?`},
		{&s.stmtUpdateModelSource, `UPDATE model_sources SET upstream_model_id = ?, priority = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtSetModelSourceDisabled, `UPDATE model_sources SET disabled = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtDeleteModelSource, `DELETE FROM model_sources WHERE id = ?`},

		// 管理侧嵌套列表：模型字典序，来源按 (priority, 订阅先行, id)；
		// 左联查保证无来源的模型也出现。排序与路由热路径同一口径，界面上的
		// 来源顺序就是真实调度顺序。
		{&s.stmtListModelsWithSources, `
			SELECT ` + modelColumnsPrefixed + `,
			       s.id, s.upstream_id, s.upstream_model_id, s.priority, s.disabled, s.created_at, s.updated_at,
			       u.name, u.type, u.catalog_id, u.billing_mode, u.base_url, u.disabled, u.egress_mode
			  FROM models m
			  LEFT JOIN model_sources s ON s.model_id = m.id
			  LEFT JOIN upstreams     u ON u.id = s.upstream_id
			 ORDER BY m.name, s.priority, ` + billingRankSQL + `, s.id`},
		// 单模型的嵌套视图：列形态与 stmtListModelsWithSources 完全一致
		// （共用 collectModelsWithSources 扫描），只加主键过滤。
		{&s.stmtGetModelWithSources, `
			SELECT ` + modelColumnsPrefixed + `,
			       s.id, s.upstream_id, s.upstream_model_id, s.priority, s.disabled, s.created_at, s.updated_at,
			       u.name, u.type, u.catalog_id, u.billing_mode, u.base_url, u.disabled, u.egress_mode
			  FROM models m
			  LEFT JOIN model_sources s ON s.model_id = m.id
			  LEFT JOIN upstreams     u ON u.id = s.upstream_id
			 WHERE m.id = ?
			 ORDER BY s.priority, ` + billingRankSQL + `, s.id`},
		{&s.stmtListModels, `SELECT ` + modelColumns + ` FROM models ORDER BY name`},
		// 路由热路径：一次联查取模型 + 有序候选来源 + 上游全字段（含密文）。
		{&s.stmtResolveModelRoute, `
			SELECT ` + modelColumnsPrefixed + `,
			       s.id, s.upstream_model_id, s.priority, s.disabled,
			       u.id, u.name, u.type, u.catalog_id, u.billing_mode, u.api_key_sealed, u.base_url, u.disabled, u.egress_mode
			  FROM models m
			  LEFT JOIN model_sources s ON s.model_id = m.id
			  LEFT JOIN upstreams     u ON u.id = s.upstream_id
			 WHERE m.name = ?
			 ORDER BY s.priority, ` + billingRankSQL + `, s.id`},
		// 管理面「测试来源」：按来源主键取模型 + 来源 + 上游全字段（含密文）。
		// 内联查（非 LEFT）：外键保证三行都在，未命中即来源不存在。
		{&s.stmtGetSourceRoute, `
			SELECT ` + modelColumnsPrefixed + `,
			       s.id, s.upstream_model_id, s.priority, s.disabled,
			       u.id, u.name, u.type, u.catalog_id, u.billing_mode, u.api_key_sealed, u.base_url, u.disabled, u.egress_mode
			  FROM model_sources s
			  JOIN models    m ON m.id = s.model_id
			  JOIN upstreams u ON u.id = s.upstream_id
			 WHERE s.id = ?`},
		// /v1/models 口径：**文本**模型（迭代 8 起谓词钉死 kind=text——这两条
		// 读数是文本对话面的契约，加了视频/图片模型集合不得变化，store_test
		// 有绊线），启用，且至少一条启用来源挂在启用的上游上。
		{&s.stmtListServableModels, `
			SELECT ` + modelColumnsPrefixed + `
			  FROM models m
			 WHERE m.disabled = 0 AND m.kind = '` + ModelKindText + `'
			   AND (m.entry_openai = 1 OR m.entry_responses = 1 OR m.entry_anthropic = 1)
			   AND EXISTS (SELECT 1
			                 FROM model_sources s
			                 JOIN upstreams u ON u.id = s.upstream_id
			                WHERE s.model_id = m.id AND s.disabled = 0 AND u.disabled = 0)
			 ORDER BY m.name`},
		// 同一口径摊开成「有哪些可用来源」（上一条只判有没有）：谓词逐字
		// 相同（含 kind=text），故两个查询的模型集合恒等。只取上游的
		// type/base_url——调用方拿它算入口协议，凭据与账户名不出这条查询。
		{&s.stmtListServableModelSource, `
			SELECT m.id, m.name, s.upstream_model_id, m.entry_openai, m.entry_responses, m.entry_anthropic, u.type, u.catalog_id, u.base_url, u.billing_mode
			  FROM models m
			  JOIN model_sources s ON s.model_id = m.id
			  JOIN upstreams     u ON u.id = s.upstream_id
			 WHERE m.disabled = 0 AND m.kind = '` + ModelKindText + `'
			   AND (m.entry_openai = 1 OR m.entry_responses = 1 OR m.entry_anthropic = 1)
			   AND s.disabled = 0 AND u.disabled = 0
			 ORDER BY m.name, s.priority, s.id`},
		// 视频/图片模型的对应读数（迭代 8 Phase 6，使用API页的最小可见性）：
		// 与上一条同一启用谓词，kind 反选为显式 IN（不用 != 'text'——将来若加
		// 第四种 kind，它不该未经决定就漂进这份成员可读的清单）。文本读数的
		// 集合恒不受它影响（TestServableModelsTextOnly 是那道绊线）。
		{&s.stmtListServableAIGCSources, `
			SELECT m.id, m.name, m.kind, u.type, u.base_url, u.billing_mode
			  FROM models m
			  JOIN model_sources s ON s.model_id = m.id
			  JOIN upstreams     u ON u.id = s.upstream_id
			 WHERE m.disabled = 0 AND m.kind IN ('` + ModelKindVideo + `', '` + ModelKindImage + `')
			   AND s.disabled = 0 AND u.disabled = 0
			 ORDER BY m.name, s.priority, s.id`},

		{&s.stmtGetSetting, `SELECT value FROM settings WHERE key = ?`},
		{&s.stmtSetSetting, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`},

		{&s.stmtCreateAIGCTask, `INSERT INTO aigc_tasks (id, vendor_task_id, upstream_id, upstream_name,
			model_name, kind, key_id, key_display, status,
			has_video_input, generate_audio, service_tier, req_resolution, req_duration,
			created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`},
		// 归属点查（2026-08-09 起客户端可见的任务标识就是厂商 task_id）：
		// vendor_task_id 与 key_id 同时命中才有行——「不是你的」与「不存在」
		// 在 SQL 层就同一形状，调用方拿到的都是 ErrNotFound（无 oracle）。
		// UNIQUE(vendor_task_id, upstream_id) 的左列即本查询的索引；跨上游账户
		// 撞出同名厂商 id 的病理情形取最新一行（created_at 倒序）。
		{&s.stmtGetAIGCTaskByVendorForKey, `SELECT ` + aigcTaskColumns + ` FROM aigc_tasks
			 WHERE vendor_task_id = ? AND key_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`},
		// 观测快照：仅在任一观测值变化时才写（WHERE 里的不等比较），
		// 等值观测零写放大——SD 卡纪律在轮询代理这条路上的落点。
		{&s.stmtUpdateAIGCTaskObserved, `
			UPDATE aigc_tasks
			   SET status = ?, error_code = ?, error_message = ?, usage_json = ?,
			       content_url = ?, last_frame_url = ?, updated_at = ?
			 WHERE id = ?
			   AND (status != ? OR error_code != ? OR error_message != ? OR usage_json != ?
			        OR content_url != ? OR last_frame_url != ?)`},
		{&s.stmtPruneAIGCTasks, `DELETE FROM aigc_tasks WHERE created_at < ?`},
		// 懒对账的取件口：未清算（cost_micro IS NULL）且 updated_at 早于阈值的
		// 行——注意 updated_at 只在观测值**变化**时才写，所以这是「多久没变过」
		// 而不是「多久没被轮询」（详见 ListUnsettledAIGCTasks 的文档）。
		// 全表扫描是有意的，理由同 PruneAIGCTasks（行数被保留期钉住）；LIMIT
		// 兜住内存、上游配额与升级后的对账洪峰。
		{&s.stmtListUnsettledAIGCTasks, `SELECT ` + aigcTaskColumns + ` FROM aigc_tasks
			 WHERE cost_micro IS NULL AND updated_at < ? ORDER BY created_at, id LIMIT ?`},
		// 一次性清算：条件更新（NULL → 值）保证「查询路径观测」与「对账协程」
		// 重叠时只入账一次。updated_at 有意不动（那是观测时刻，不是记账时刻）。
		{&s.stmtSettleAIGCTask, `UPDATE aigc_tasks SET cost_micro = ?, estimated = ?
			 WHERE id = ? AND cost_micro IS NULL`},

		// Agents（Codex 订阅代理）的账号与凭据。单账户语义：唯一键只有
		// provider，重复连接走 ON CONFLICT 覆盖那一支。覆盖分两组，分界线是
		// **这个值是不是从凭据里来的**：
		//   - 凭据、account_id、状态**无条件覆盖**。account_id 是 id_token 的
		//     claim，跟着凭据走：换一个 ChatGPT 账号重连、而 claim 又恰好解不出来
		//     （粘贴的 auth.json 没有可用 id_token）时，若"空即保持"就会留下
		//     甲账号的 account_id 配乙账号的 token——代理注入 ChatGPT-Account-ID
		//     的那一刻必 401，而管理台还显示着甲。宁可空着。
		//   - label/default_model 是管理员设的展示/配置项，**空即保持**：重登
		//     通常不带这两项，照空值盖过去会把管理员设过的默认模型悄悄抹掉。
		//     Cursor 没有模型配置语义，仓储入口恒把它的 default_model 归零。
		// last_refresh_at 不动：重新登录不是一次刷新。
		{&s.stmtUpsertAgentAccount, `
			INSERT INTO agent_accounts (provider, label, account_id, default_model,
			    auth_json_sealed, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(provider) DO UPDATE SET
			    label            = CASE WHEN excluded.label         = '' THEN agent_accounts.label         ELSE excluded.label END,
			    default_model    = CASE WHEN excluded.default_model = '' THEN agent_accounts.default_model ELSE excluded.default_model END,
			    account_id       = excluded.account_id,
			    auth_json_sealed = excluded.auth_json_sealed,
			    status           = excluded.status,
			    updated_at       = excluded.updated_at`},
		{&s.stmtListAgentAccounts, `SELECT ` + agentAccountColumns + ` FROM agent_accounts
			 ORDER BY provider, id`},
		{&s.stmtGetAgentAccountByID, `SELECT ` + agentAccountColumns + ` FROM agent_accounts WHERE id = ?`},
		{&s.stmtGetAgentAccountByProvider, `SELECT ` + agentAccountColumns + ` FROM agent_accounts WHERE provider = ?`},
		// 取令牌视图：唯一一处把密文列读上来的查询（解封紧随其后，只在进程内）。
		{&s.stmtGetAgentCredential, `SELECT ` + agentAccountColumns + `, auth_json_sealed
			 FROM agent_accounts WHERE provider = ?`},
		// 刷新回写：provider 进 WHERE 而不只是拿来算 AAD——「用 A 的 AAD 封的
		// 密文写进 B 的行」在 SQL 层就落不下去。状态不动（见 SetAgentAuthJSON）。
		{&s.stmtSetAgentAuthJSON, `UPDATE agent_accounts
			    SET auth_json_sealed = ?, last_refresh_at = ?, updated_at = ?
			  WHERE id = ? AND provider = ?`},
		// 管理员改展示/配置两项（label、default_model）。与 Upsert 的「空即保持」
		// 相反：这条是显式赋值，空串就是清空——没有它，清空 label 无从表达。
		{&s.stmtUpdateAgentAccount, `UPDATE agent_accounts
			    SET label = ?,
			        default_model = CASE WHEN provider = 'cursor' THEN '' ELSE ? END,
			        updated_at = ?
			  WHERE id = ?`},
		{&s.stmtSetAgentStatus, `UPDATE agent_accounts SET status = ?, updated_at = ? WHERE id = ?`},
		{&s.stmtDeleteAgentAccount, `DELETE FROM agent_accounts WHERE id = ?`},

		// 用量小时聚合的写入：计数列恒为「列 = 列 + excluded.列」相加语义
		// （见 usage.go 文件头注释）。**非键的维度快照列（key_display/kind）
		// 取最后写入者**——不是「不覆盖」：写入本来就发生，顺手覆盖零成本，
		// 而「首个写入者永久钉死」会让任何一次快照失真都无从修复（本桶内先落
		// 的那条若带了空/过期的名字，整小时都改不回来）。
		{&s.stmtAddUsageDelta, `
			INSERT INTO usage_hourly (` + usageColumns + `)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (bucket_hour, key_id, model_name, upstream_name, entry) DO UPDATE SET
			    key_display        = excluded.key_display,
			    kind               = excluded.kind,
			    requests           = usage_hourly.requests           + excluded.requests,
			    errors             = usage_hourly.errors             + excluded.errors,
			    rejected_requests  = usage_hourly.rejected_requests  + excluded.rejected_requests,
			    estimated_requests = usage_hourly.estimated_requests + excluded.estimated_requests,
			    unavailable_requests = usage_hourly.unavailable_requests + excluded.unavailable_requests,
			    prompt_tokens      = usage_hourly.prompt_tokens      + excluded.prompt_tokens,
			    completion_tokens  = usage_hourly.completion_tokens  + excluded.completion_tokens,
			    cache_read_tokens  = usage_hourly.cache_read_tokens  + excluded.cache_read_tokens,
			    cache_write_tokens = usage_hourly.cache_write_tokens + excluded.cache_write_tokens,
			    total_tokens       = usage_hourly.total_tokens       + excluded.total_tokens,
			    video_seconds      = usage_hourly.video_seconds      + excluded.video_seconds,
			    image_count        = usage_hourly.image_count        + excluded.image_count,
			    cost_micro         = usage_hourly.cost_micro         + excluded.cost_micro,
			    duration_ms_sum    = usage_hourly.duration_ms_sum    + excluded.duration_ms_sum`},
		// 区间读数左闭右开，按桶号升序。
		{&s.stmtQueryUsageRange, `SELECT ` + usageColumns + ` FROM usage_hourly
			 WHERE bucket_hour >= ? AND bucket_hour < ? ORDER BY bucket_hour, key_id`},
		// 预算播种：保留桶时刻，本地时区折算归调用方。
		{&s.stmtSumUsageSince, `SELECT bucket_hour, key_id, SUM(cost_micro)
			   FROM usage_hourly WHERE bucket_hour >= ?
			  GROUP BY bucket_hour, key_id
			  ORDER BY bucket_hour, key_id`},
		{&s.stmtPruneUsage, `DELETE FROM usage_hourly WHERE bucket_hour < ?`},
		// 计价时点价的窄读数（懒对账清算用）：只取价签一列，不碰来源与凭证。
		{&s.stmtGetModelPricing, `SELECT pricing FROM models WHERE name = ?`},
		// 全目录的「名字 → 价签」（用量页的「未定价」徽章用）：同样只取两列，
		// 不碰来源与凭证——这份读数要服务 member 视角，绝不能顺手带出上游。
		{&s.stmtListModelPricing, `SELECT name, pricing FROM models`},
	} {
		st, err := s.db.PrepareContext(ctx, p.query)
		if err != nil {
			return fmt.Errorf("准备语句: %w", err)
		}
		*p.dst = st
		s.stmts = append(s.stmts, st)
	}
	return nil
}

// mapErr 把 SQLite 约束错误映射到本包哨兵错误。SQLite 的约束错误文本只含
// 约束与列名（如 "UNIQUE constraint failed: api_keys.key_digest"），不回显参数值，
// 可安全向上传播与落日志。
//
// 外键违反也归入 ErrConflict：插入侧是"引用了不存在的父行"，删除侧是
// "父行仍被引用"（model_sources → upstreams），两者都是调用方应转 409 的
// 引用冲突；需要区分具体原因的调用方自己包装可读信息（见 DeleteUpstream）。
// 注意外键必须用默认的 NO ACTION：SQLite 用触发器程序实现 ON DELETE RESTRICT，
// 违反时报 SQLITE_CONSTRAINT_TRIGGER 而不是这里认的 SQLITE_CONSTRAINT_FOREIGNKEY。
func mapErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlitelib.SQLITE_CONSTRAINT_UNIQUE,
			sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY,
			sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	return err
}
