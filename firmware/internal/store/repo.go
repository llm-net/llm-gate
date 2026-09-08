package store

// 仓储方法：全部 context 化并走 Open 时准备好的 prepared statement。
// 约定：
//   - 查询未命中与更新/删除未匹配到行 → ErrNotFound；唯一性冲突 → ErrConflict。
//   - 时间字段入库经 fmtTime（UTC 毫秒），返回结构体里的时间已解析回 time.Time；
//     可空时间（last_used_at）用零值 time.Time 表示"从未"。
//   - 本文件不含审计的更新/删除方法，永远不要加（见包文档与反射测试）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// apiKeyColumns 是 api_keys 的 SELECT 列表（与 scanAPIKey 一一对应）。
// plaintext_sealed 刻意只以「是否非空」参与扫描（PlaintextAvailable），密文
// 本体不进 APIKey 结构体——解封走 GetAPIKeyPlaintext 的专用点查。
const apiKeyColumns = `id, label, key_digest, display_prefix, display_last4, project_id,
	disabled, created_at, last_used_at, budget_day_micro, budget_week_micro, budget_month_micro, rpm_limit,
	metered_allowance_micro, plaintext_sealed <> ''`

// SettingAdminPasswordHash 是设备登录口令的 Argon2id PHC 串在 settings 表里的
// 键名。设备只有一个管理员，「用户」这个概念 0019 起整个退场——口令因此是一条
// 单值配置，不再是某一行的列。值本身已是哈希，不必再走 SetSealedSetting。
const SettingAdminPasswordHash = "admin_password_hash"

// APIKey 是 api_keys 表的一行。结构体不携带明文与密文——鉴权走摘要，展示用
// 前缀/末 4 位；封存明文（0012 起）只以 PlaintextAvailable 布尔露头，解封走
// GetAPIKeyPlaintext。LastUsedAt 零值表示从未使用；ProjectID 0 表示未挂项目
// （预留列，本迭代恒为 0）。
type APIKey struct {
	ID            int64
	Label         string
	KeyDigest     string // 明文的 SHA-256 十六进制
	DisplayPrefix string
	DisplayLast4  string
	ProjectID     int64
	Disabled      bool
	// PlaintextAvailable 表示本行封存有明文（plaintext_sealed 非空），自助复制
	// 端点可解封取回；false = 签发于 0012 之前，明文物理上不可恢复。
	PlaintextAvailable bool
	// 密钥级限额，nil = 不限：日/周/月预算（int64 微元，本地时区自然日/
	// 自然周/自然月）与每分钟请求数上限。
	BudgetDayMicro   *int64
	BudgetWeekMicro  *int64
	BudgetMonthMicro *int64
	RPMLimit         *int64
	// MeteredAllowanceMicro 是预算用尽后继续消费的一次性按量额度剩余（微元）。
	// 它不会随日/周/月窗口复位，只能由管理员增减或由计量层扣减。
	MeteredAllowanceMicro int64
	CreatedAt             time.Time
	LastUsedAt            time.Time
}

// Session 是 sessions 表的一行。令牌只存摘要；过期判定由调用方
// （internal/auth）拿 ExpiresAt 做，Get 不隐式过滤过期行。会话不指向任何主体
// ——它只表示「这个浏览器验过设备口令」。
type Session struct {
	ID          int64
	TokenDigest string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RemoteIP    string
}

// KeyAuth 是 LookupKeyByDigest 的结果：数据面鉴权所需的最小事实集。
// 命中但被禁用仍返回行并置 KeyDisabled，是否 401 由调用方裁决。
//
// 它同时是**计量与准入的事实载体**（iteration-9）：归属（KeyID/KeyDisplay）与
// 四个限额列都随这一次点查捎带回来，于是预算准入是纯内存比较、零额外查询。
// 给它加字段的门槛是「必须来自同一条点查」——要额外查询就说明它不该住在这里。
type KeyAuth struct {
	KeyID int64
	// KeyDisplay 是 "前缀…末4位" 的展示串（明文另有封存，热路径刻意不取）；
	// 计量按它给密钥维度留快照，密钥被删后历史账仍可读。
	KeyDisplay  string
	KeyDisabled bool
	// 限额快照，nil = 不限。检查序：RPM → 日 → 周 → 月；预算命中后若按量
	// 额度仍有剩余则继续放行，RPM 永不由额度兜底。
	KeyBudgetDayMicro        *int64
	KeyBudgetWeekMicro       *int64
	KeyBudgetMonthMicro      *int64
	KeyRPMLimit              *int64
	KeyMeteredAllowanceMicro int64
}

// AuditEvent 是一条待追加的审计记录。At 零值由 AppendAudit 以当前时间填充。
// 设备只有一个操作者，审计因此不记 actor——"谁做的"恒等于同一个答案。
// Detail 只放非敏感字段（label/display_prefix 等），密码/令牌/Key 明文一律
// 不得进入。
type AuditEvent struct {
	At       time.Time
	Event    string
	Entity   string
	Detail   string
	RemoteIP string
}

// ---- api_keys ----

// CreateAPIKey 签发 Key 行并返回完整行。keyDigest 为明文的 SHA-256 十六进制
// （鉴权点查的唯一形态）；plaintext 是同一把 Key 的明文，入库前以设备密钥
// 封存（AAD 钉本行摘要），供自助复制端点解封取回——除封存入参外绝不传入
// 本包其他字段。摘要重复返回 ErrConflict。
func (s *Store) CreateAPIKey(ctx context.Context, label, keyDigest, displayPrefix, displayLast4, plaintext string) (*APIKey, error) {
	sealed, err := s.seal(plaintext, apiKeyAAD(keyDigest))
	if err != nil {
		return nil, fmt.Errorf("创建 API密钥: 封存明文: %w", err)
	}
	ts := fmtTime(time.Now())
	res, err := s.stmtCreateAPIKey.ExecContext(ctx, label, keyDigest, displayPrefix, displayLast4, sealed, ts)
	if err != nil {
		return nil, fmt.Errorf("创建 API密钥: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("创建 API密钥: %w", err)
	}
	t, _ := parseTime(ts)
	return &APIKey{
		ID: id, Label: label, KeyDigest: keyDigest,
		DisplayPrefix: displayPrefix, DisplayLast4: displayLast4,
		PlaintextAvailable: sealed != "", CreatedAt: t,
	}, nil
}

// apiKeyAAD 是 api_keys.plaintext_sealed 的 AAD：钉在本行摘要上，密文挪到
// 别的 Key 行（或别的封存场景）都解不开。摘要建后不可变，AAD 因此恒可重算。
func apiKeyAAD(keyDigest string) []byte { return []byte("apikey:" + keyDigest) }

// ErrKeyPlaintextMissing 表示该 Key 行没有封存明文（签发于 0012 之前，库里
// 只有摘要，明文物理上不可恢复）。出路是重新签发一把。
var ErrKeyPlaintextMissing = errors.New("store: Key 明文未留存")

// ErrKeyPlaintextUnreadable 表示封存明文解不开（设备密钥被替换或密文损坏）。
// 与「未留存」刻意分开：一个是历史事实，一个是损坏事故；补救动作同为重签。
var ErrKeyPlaintextUnreadable = errors.New("store: Key 明文不可解密")

// GetAPIKeyPlaintext 解出 Key 封存的明文（自助复制端点专用）。
// 未命中返回 ErrNotFound；无封存明文返回 ErrKeyPlaintextMissing；
// 解封失败返回 ErrKeyPlaintextUnreadable。错误信息不含密钥物料，可安全落日志。
func (s *Store) GetAPIKeyPlaintext(ctx context.Context, id int64) (string, error) {
	var digest, sealed string
	err := s.stmtGetAPIKeyPlaintext.QueryRowContext(ctx, id).Scan(&digest, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询 API密钥 明文: %w", err)
	}
	if sealed == "" {
		return "", ErrKeyPlaintextMissing
	}
	plaintext, err := s.open(sealed, apiKeyAAD(digest))
	if err != nil {
		return "", fmt.Errorf("%w（设备密钥被替换或密文损坏），请重新签发一把替换", ErrKeyPlaintextUnreadable)
	}
	return plaintext, nil
}

// ListAPIKeys 按 id 升序返回全部 Key。
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.stmtListAPIKeys.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出 API密钥: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("列出 API密钥: %w", err)
		}
		keys = append(keys, *k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出 API密钥: %w", err)
	}
	return keys, nil
}

// GetAPIKeyByID 按主键取 Key；未命中返回 ErrNotFound。
func (s *Store) GetAPIKeyByID(ctx context.Context, id int64) (*APIKey, error) {
	k, err := scanAPIKey(s.stmtGetAPIKeyByID.QueryRowContext(ctx, id))
	if err != nil {
		return nil, fmt.Errorf("查询 API密钥: %w", err)
	}
	return k, nil
}

// SetAPIKeyLabel 更新 Key 的展示标签；空串表示清除，目标不存在返回 ErrNotFound。
// 标签不参与鉴权、计量归属或密钥展示串，只供管理界面识别用途。
func (s *Store) SetAPIKeyLabel(ctx context.Context, id int64, label string) error {
	res, err := s.stmtSetAPIKeyLabel.ExecContext(ctx, label, id)
	return execOneRow(res, err, "更新 API密钥 标签")
}

// SetAPIKeyDisabled 置 Key 禁用位；目标不存在返回 ErrNotFound。
// 决策 5（无缓存层）保证禁用对数据面即时生效。
func (s *Store) SetAPIKeyDisabled(ctx context.Context, id int64, disabled bool) error {
	res, err := s.stmtSetAPIKeyDisabled.ExecContext(ctx, boolToInt(disabled), id)
	return execOneRow(res, err, "更新 API密钥 禁用位")
}

// SetAPIKeyLimits 写密钥级日/周/月预算与 RPM 上限（nil = 清除该项限额）；目标
// 不存在返回 ErrNotFound。四项一并写入：管理端点的 PATCH 语义是「只带要改的
// 字段」，未带的那些由调用方以读到的现值回填，本层不做部分更新的组合爆炸。
// 无缓存的每请求点查（决策 5）保证改限额对数据面下一个请求即时生效。
func (s *Store) SetAPIKeyLimits(ctx context.Context, id int64, dayMicro, weekMicro, monthMicro, rpmLimit *int64) error {
	res, err := s.stmtSetAPIKeyLimits.ExecContext(ctx,
		int64OrNull(dayMicro), int64OrNull(weekMicro), int64OrNull(monthMicro), int64OrNull(rpmLimit), id)
	return execOneRow(res, err, "更新 API密钥 限额")
}

// AdjustAPIKeyMeteredAllowance 原子调整单把密钥的按量额度剩余：deltaMicro 正数
// 增加、负数扣减，结果钳在 [0, MaxInt64]。目标不存在返回 ErrNotFound。
//
// 这是增量接口而不是「设置为某值」：计量层可能同时批量扣减，读后再按绝对值
// 覆盖会把那段并发消费静默吃掉。
func (s *Store) AdjustAPIKeyMeteredAllowance(ctx context.Context, id, deltaMicro int64) (int64, error) {
	// 一条 UPDATE ... RETURNING 完成读、算、写，避免先 SELECT 再 UPDATE 与后台
	// 扣减交错时出现丢更新。正增量先和 MaxInt64-delta 比较，避免在 SQL 内做
	// 可能上溢的加法；负增量与非负当前值相加不会下溢。
	var remaining int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE api_keys
		   SET metered_allowance_micro = CASE
		         WHEN ? > 0 AND metered_allowance_micro > ? - ? THEN ?
		         WHEN ? < 0 AND metered_allowance_micro + ? < 0 THEN 0
		         ELSE metered_allowance_micro + ?
		       END
		 WHERE id = ?
		 RETURNING metered_allowance_micro`,
		deltaMicro, int64(math.MaxInt64), deltaMicro, int64(math.MaxInt64),
		deltaMicro, deltaMicro, deltaMicro, id).Scan(&remaining)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("调整 API密钥 按量额度: %w", ErrNotFound)
		}
		return 0, fmt.Errorf("调整 API密钥 按量额度: %w", err)
	}
	return remaining, nil
}

// DrainAPIKeyMeteredAllowances 批量落地计量层判定的按量额度扣减。每把密钥
// `剩余 = max(0, 剩余 - 扣减)`；已经删除的密钥静默跳过。整批在一个事务中，
// 失败时分文不动，调用方可以把待扣原样放回内存重试。
func (s *Store) DrainAPIKeyMeteredAllowances(ctx context.Context, drains map[int64]int64) error {
	if len(drains) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("扣减 API密钥 按量额度: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是空操作
	stmt := tx.StmtContext(ctx, s.stmtDrainKeyAllowance)
	for id, amount := range drains {
		if amount <= 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, amount, amount, id); err != nil {
			return fmt.Errorf("扣减 API密钥 按量额度: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("扣减 API密钥 按量额度: %w", err)
	}
	return nil
}

// DeleteAPIKey 删除 Key 行；目标不存在返回 ErrNotFound。删除即摘要点查落空，
// 数据面下一个请求就是 401（决策 5：无缓存层）。封存明文随行一并删除，这把
// Key 再无恢复可能。
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteAPIKey.ExecContext(ctx, id)
	return execOneRow(res, err, "删除 API密钥")
}

// LookupKeyByDigest 是数据面鉴权热路径的摘要点查。未命中返回 ErrNotFound；
// 命中恒返回行，禁用与否由 KeyAuth.KeyDisabled 表达。**一次点查供齐鉴权 +
// 计量归属 + 预算准入**——准入因此零额外查询。0019 删掉用户概念后它退回单表
// 点查，不再 JOIN 任何东西。
func (s *Store) LookupKeyByDigest(ctx context.Context, keyDigest string) (*KeyAuth, error) {
	var (
		a                         KeyAuth
		prefix, last4             string
		keyDisabled               int
		keyDay, keyWeek, keyMonth sql.NullInt64
		rpm                       sql.NullInt64
	)
	err := s.stmtLookupKeyByDigest.QueryRowContext(ctx, keyDigest).
		Scan(&a.KeyID, &prefix, &last4, &keyDisabled, &keyDay, &keyWeek, &keyMonth, &rpm,
			&a.KeyMeteredAllowanceMicro)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询 Key 摘要: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查询 Key 摘要: %w", err)
	}
	a.KeyDisplay = keyDisplay(prefix, last4)
	a.KeyDisabled = keyDisabled != 0
	a.KeyBudgetDayMicro = nullableInt64(keyDay)
	a.KeyBudgetWeekMicro = nullableInt64(keyWeek)
	a.KeyBudgetMonthMicro = nullableInt64(keyMonth)
	a.KeyRPMLimit = nullableInt64(rpm)
	return &a, nil
}

// keyDisplay 拼出密钥的展示串（形如 sk_abcdefghi…wxyz）。两段都空
// （YAML 导入的历史 Key 没有展示字段）时返回空串，而不是一个只有省略号的怪串。
//
// aigc_tasks.key_display 的任务行快照与账本的密钥维度都取 KeyAuth.KeyDisplay
// （即本函数的产出）：两张表的密钥维度必须能对上。gateway 侧曾有一份逐字相同
// 的副本，2026-08-14 已退役。
func keyDisplay(prefix, last4 string) string {
	if prefix == "" && last4 == "" {
		return ""
	}
	return prefix + "…" + last4
}

// TouchKeyLastUsed 把 Key 的 last_used_at 刷到当前时间；节流（60s 合并写）
// 由调用方（internal/gateway）在内存里做，本方法每调用必写。
func (s *Store) TouchKeyLastUsed(ctx context.Context, id int64) error {
	res, err := s.stmtTouchKeyLastUsed.ExecContext(ctx, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新 Key 最近使用时间")
}

// ---- sessions ----

// CreateSession 落一条会话行。tokenDigest 为 32B 随机令牌的 SHA-256 十六进制
// （令牌明文只在登录响应的 Set-Cookie 里出现一次）；摘要重复返回 ErrConflict。
func (s *Store) CreateSession(ctx context.Context, tokenDigest string, expiresAt time.Time, remoteIP string) (*Session, error) {
	created := fmtTime(time.Now())
	expires := fmtTime(expiresAt)
	res, err := s.stmtCreateSession.ExecContext(ctx, tokenDigest, created, expires, remoteIP)
	if err != nil {
		return nil, fmt.Errorf("创建会话: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("创建会话: %w", err)
	}
	ct, _ := parseTime(created)
	et, _ := parseTime(expires)
	return &Session{ID: id, TokenDigest: tokenDigest, CreatedAt: ct, ExpiresAt: et, RemoteIP: remoteIP}, nil
}

// GetSessionByTokenDigest 按令牌摘要取会话；未命中返回 ErrNotFound。
// 过期行照常返回（过期判定与清理是调用方的事），避免"查不到"与"已过期"混淆。
func (s *Store) GetSessionByTokenDigest(ctx context.Context, tokenDigest string) (*Session, error) {
	var (
		sess             Session
		created, expires string
	)
	err := s.stmtGetSessionByDigest.QueryRowContext(ctx, tokenDigest).
		Scan(&sess.ID, &sess.TokenDigest, &created, &expires, &sess.RemoteIP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询会话: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查询会话: %w", err)
	}
	if sess.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("查询会话: created_at 非法: %w", err)
	}
	if sess.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, fmt.Errorf("查询会话: expires_at 非法: %w", err)
	}
	return &sess, nil
}

// DeleteSession 删除单个会话（登出）。目标已不存在视为成功（幂等）。
func (s *Store) DeleteSession(ctx context.Context, tokenDigest string) error {
	if _, err := s.stmtDeleteSession.ExecContext(ctx, tokenDigest); err != nil {
		return fmt.Errorf("删除会话: %w", err)
	}
	return nil
}

// DeleteAllSessions 清空会话表（改密联动踢会话：口令换了，所有已登录的
// 浏览器都得重新验一次）。
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	if _, err := s.stmtDeleteAllSessions.ExecContext(ctx); err != nil {
		return fmt.Errorf("清空会话: %w", err)
	}
	return nil
}

// DeleteExpiredSessions 删除 expires_at ≤ now 的行，返回删除条数
// （启动与每小时清理用；固定宽度时间文本保证 SQL 字符串比较正确）。
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.stmtDeleteExpiredSessions.ExecContext(ctx, fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("清理过期会话: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理过期会话: %w", err)
	}
	return n, nil
}

// ---- audit ----

// AppendAudit 追加一条审计记录——审计表唯一的写入口，本包永不提供
// 更新/删除审计的方法。ev.At 零值补当前时间。
func (s *Store) AppendAudit(ctx context.Context, ev AuditEvent) error {
	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.stmtAppendAudit.ExecContext(ctx,
		fmtTime(at), ev.Event, ev.Entity, ev.Detail, ev.RemoteIP); err != nil {
		return fmt.Errorf("追加审计: %w", err)
	}
	return nil
}

// ---- 内部工具 ----

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的 Scan（本包各 scanXxx 共用）。
type rowScanner interface{ Scan(dest ...any) error }

// scanAPIKey 从行扫描 APIKey；未命中返回 ErrNotFound。
func scanAPIKey(r rowScanner) (*APIKey, error) {
	var (
		k                                  APIKey
		project                            sql.NullInt64
		created                            string
		lastUsed                           sql.NullString
		disabled, plaintextAvail           int
		budgetDay, budgetWeek, budgetMonth sql.NullInt64
		rpmLimit                           sql.NullInt64
	)
	err := r.Scan(&k.ID, &k.Label, &k.KeyDigest, &k.DisplayPrefix,
		&k.DisplayLast4, &project, &disabled, &created, &lastUsed,
		&budgetDay, &budgetWeek, &budgetMonth, &rpmLimit,
		&k.MeteredAllowanceMicro, &plaintextAvail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.ProjectID = project.Int64
	k.Disabled = disabled != 0
	k.PlaintextAvailable = plaintextAvail != 0
	k.BudgetDayMicro = nullableInt64(budgetDay)
	k.BudgetWeekMicro = nullableInt64(budgetWeek)
	k.BudgetMonthMicro = nullableInt64(budgetMonth)
	k.RPMLimit = nullableInt64(rpmLimit)
	if k.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if lastUsed.Valid {
		if k.LastUsedAt, err = parseTime(lastUsed.String); err != nil {
			return nil, fmt.Errorf("last_used_at 非法: %w", err)
		}
	}
	return &k, nil
}

// execOneRow 收敛"更新必须命中一行"的错误处理：未命中 → ErrNotFound。
func execOneRow(res sql.Result, err error, op string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return nil
}

// ---- 单值配置（settings 表） ----

// GetSetting 读单值配置；键不存在返回空串而非 ErrNotFound——调用方把「从未
// 设置过」与「显式设为空」当作同一状态（口令未播种时读到空串，正是
// EnsureDefaultPassword 的判据）。
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	switch err := s.stmtGetSetting.QueryRowContext(ctx, key).Scan(&value); {
	case err == nil:
		return value, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	default:
		return "", fmt.Errorf("读取配置项 %s: %w", key, err)
	}
}

// SetSetting 写单值配置（存在即覆盖）。空 value 合法，表示把该项清空——语义
// 上等同删除，读取端一律把空串当未设置。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.stmtSetSetting.ExecContext(ctx, key, value, fmtTime(time.Now())); err != nil {
		return fmt.Errorf("写入配置项 %s: %w", key, err)
	}
	return nil
}

// SetSealedSetting 写一条**密封的**单值配置：值用设备密钥 AES-256-GCM 封存，
// AAD 是 setting 键名（密文因此钉死在这个键上，搬到别的键解不开）。
//
// 用途是设备自己的密钥物料。展示用的配置照旧走 [Store.SetSetting]：封存不是
// 越多越好，它换来的是「单独外流的 llmgate.db 副本里没有明文」，代价是设备密钥
// 一旦换掉这些值就读不回来了。
func (s *Store) SetSealedSetting(ctx context.Context, key, value string) error {
	sealed, err := s.seal(value, []byte(key))
	if err != nil {
		return fmt.Errorf("封存配置项 %s: %w", key, err)
	}
	return s.SetSetting(ctx, key, sealed)
}

// GetSealedSetting 读一条密封的单值配置；键不存在或值为空返回空串
// （与 [Store.GetSetting] 同口径）。
//
// 解不开时返回错误而不是空串：那两件事的处置不同——「没设过」是正常状态，
// 「解不开」是设备密钥换过或密文损坏，那一项必须重新写入才能恢复。
// 错误文本不含密文、不含键值。
func (s *Store) GetSealedSetting(ctx context.Context, key string) (string, error) {
	sealed, err := s.GetSetting(ctx, key)
	if err != nil {
		return "", err
	}
	plaintext, err := s.open(sealed, []byte(key))
	if err != nil {
		return "", fmt.Errorf("配置项 %s 解封失败：%w（%s 被替换或密文损坏，需重新写入该项）",
			key, err, DeviceKeyFileName)
	}
	return plaintext, nil
}

// SetSettingsAtomic 在一个事务里写若干明文项（plain）与若干密封项（sealed，值经设备
// 密钥封存、AAD 为键名，口径同 SetSealedSetting）。任一项失败整组不落库——出站代理的
// profile、各分类出口与凭据必须同时生效或同时不生效，不能留下「路由指向代理、凭据还是旧的」
// 这种中间态。空值合法，表示清空该项。
func (s *Store) SetSettingsAtomic(ctx context.Context, plain, sealed map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入配置项: %w", err)
	}
	defer tx.Rollback()
	ts := fmtTime(time.Now())
	stmt := tx.StmtContext(ctx, s.stmtSetSetting)
	for key, value := range plain {
		if _, err := stmt.ExecContext(ctx, key, value, ts); err != nil {
			return fmt.Errorf("写入配置项 %s: %w", key, err)
		}
	}
	for key, value := range sealed {
		enc, err := s.seal(value, []byte(key))
		if err != nil {
			return fmt.Errorf("封存配置项 %s: %w", key, err)
		}
		if _, err := stmt.ExecContext(ctx, key, enc, ts); err != nil {
			return fmt.Errorf("写入配置项 %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("写入配置项: %w", err)
	}
	return nil
}

// nullIfEmpty 把空串映射为 NULL（member 的 password_hash）。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt64 把可空整数列读成 *int64（NULL → nil）。限额列一律用它：
// 「未设置 = 不限」与「设置为 0 = 一分钱都不许花」是两个状态，零值不可混用。
func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// int64OrNull 是 nullableInt64 的写入侧逆运算（nil → SQL NULL）。
func int64OrNull(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// boolToInt 把 bool 映射为 SQLite 的 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
