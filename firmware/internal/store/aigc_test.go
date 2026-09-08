package store

// iteration-8 Phase 1 的可执行验收：迁移 0005（upstreams 重建 + models.kind +
// aigc_tasks）在带 0004 存量数据的库上前向迁移无损；模型 kind 各视图透出；
// aigc_tasks CRUD 与归属查询。
//
// 存量库的构造方式：用与 Open 相同的 DSN 直接建库，逐个应用版本 ≤4 的
// embed 迁移并手写 schema_migrations，再以原生 SQL 插入存量行——密文列用
// 与 Open 同一把设备密钥现封（先落 device-key 再造只有 aead 的 Store 封存），
// 这样迁移后 last4/路由解密能验证密文逐字节无损。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// legacyTS 是存量行的固定时间戳（合法 timeLayout 文本），迁移后必须原样读回。
const legacyTS = "2026-08-01T08:30:00.000Z"

// openLegacyDB 在 dir 下建一个停在版本 maxVersion 的库并返回原生句柄。
// DSN 经共用的 dbDSN 与 Open 恒一致，调用方用完必须 Close。
//
// 重建型迁移（带 rebuildDirective 的，首例 0005）在这里同样需要外键关闭。
// PRAGMA foreign_keys 是**连接态**，所以照产品路径（applyRebuildMigration）
// 的做法用 db.Conn 钉住一条物理连接跑完全部迁移——不能只靠 SetMaxOpenConns(1)：
// 那限的是并发度而非「后续 Exec 落在同一条物理连接上」，连接一旦因空闲回收
// 被换掉，关外键就静默失效，而本助手建的是空库、0005 的 DROP 照样成功，于是
// 它会**不再证明它自称在证明的事**却依旧全绿。产品路径另有提交前的全库外键
// 自证，本助手只需把存量库造到指定版本，不重复那套保障。
func openLegacyDB(t *testing.T, dir string, maxVersion int64) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbDSN(filepath.Join(dir, DBFileName)))
	if err != nil {
		t.Fatalf("打开存量库: %v", err)
	}
	ctx := t.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("独占连接: %v", err)
	}
	// 无论中途哪一步 t.Fatalf，都恢复外键并归还连接；失败路径也不漏 db 句柄。
	defer func() {
		conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`) //nolint:errcheck // 收尾尽力而为
		conn.Close()
		if t.Failed() {
			db.Close()
		}
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("建 schema_migrations: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	applied := 0
	for _, m := range migs {
		if m.version > maxVersion {
			continue
		}
		rebuild := hasRebuildDirective(m.sql)
		if rebuild {
			if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
				t.Fatalf("存量迁移 %s 关闭外键: %v", m.name, err)
			}
		}
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("应用存量迁移 %s: %v", m.name, err)
		}
		if rebuild {
			if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
				t.Fatalf("存量迁移 %s 恢复外键: %v", m.name, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, legacyTS); err != nil {
			t.Fatalf("记录存量迁移 %s: %v", m.name, err)
		}
		applied++
	}
	if int64(applied) != maxVersion {
		t.Fatalf("存量迁移应用了 %d 个, 期望 %d（版本 ≤%d 的迁移文件缺失?）", applied, maxVersion, maxVersion)
	}
	return db
}

// legacySealer 先落好 device-key 再返回只带 aead 的 Store，用于给存量行封存
// 凭证；随后的 Open(dir) 读的是同一把密钥。
func legacySealer(t *testing.T, dir string) *Store {
	t.Helper()
	key, err := loadOrCreateDeviceKey(dir)
	if err != nil {
		t.Fatalf("准备设备密钥: %v", err)
	}
	aead, err := newAEAD(key)
	if err != nil {
		t.Fatalf("构造 AEAD: %v", err)
	}
	return &Store{aead: aead}
}

// countRows 数一张表的行数。
func countRows(t *testing.T, db interface {
	QueryRow(query string, args ...any) *sql.Row
}, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("数 %s 行数: %v", table, err)
	}
	return n
}

// 迁移 0005 在带 0004 存量数据（含外键引用与密文）的库上前向迁移成功且无损：
// upstreams 重建后行数据、密文、外键、唯一索引与 model_sources 引用完好；
// models.kind 回填 text；CHECK 扩入 minimax。
func TestMigration0005RebuildPreservesLegacyData(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0001")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}
	sealedPlan, err := sealer.sealKey("sk-legacy-arkplan-9876")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 4)
	// 存量数据：三个上游（一个停用）、两个模型（一个停用）、三条来源（横跨
	// 两个上游）、账号侧各表一行——覆盖 0005 重建会波及的整个外键网。
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', ?, '', 0, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (2, 'ark-plan', 'ark_plan', ?, '', 0, ?, ?)`, []any{sealedPlan, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (3, 'mock-up', 'mock', '', 'http://127.0.0.1:18080/v1', 1, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, disabled, created_at, updated_at) VALUES (1, 'deepseek-chat', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, disabled, created_at, updated_at) VALUES (2, 'retired-model', 1, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (1, 1, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (2, 1, 2, 'ds-on-plan', 200, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (3, 2, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO users (id, name, role, password_hash, created_at, updated_at) VALUES (1, 'alice', 'admin', '$argon2id$fake', ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO api_keys (id, user_id, label, key_digest, created_at) VALUES (1, 1, 'legacy', 'digest-legacy-1', ?)`, []any{legacyTS}},
		{`INSERT INTO sessions (token_digest, user_id, created_at, expires_at) VALUES ('sess-digest-1', 1, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO audit_events (at, actor_name, event) VALUES (?, 'alice', 'user.login')`, []any{legacyTS}},
		{`INSERT INTO settings (key, value, updated_at) VALUES ('migration_sentinel', 'preserved', ?)`, []any{legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
	}
	// 只对**跨越 0019 幸存下来的表**比行数：账号侧四张表（users / api_keys /
	// sessions / audit_events）由 0019 清空重建，那正是它要做的事，另行断言。
	// 它们仍然照原样插进存量库——0005 的重建要在完整的外键网下跑才算数。
	tables := []string{"upstreams", "models", "model_sources", "settings"}
	before := map[string]int64{}
	for _, tb := range tables {
		before[tb] = countRows(t, db, tb)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	// Open 应用 0005（含 upstreams 重建），随后逐项验证无损。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var v5 int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&v5); err != nil || v5 != 1 {
		t.Fatalf("schema_migrations 缺版本 5 (n=%d, err=%v)", v5, err)
	}
	for _, tb := range tables {
		want := before[tb]
		if tb == "settings" {
			// Open 为没有名称的存量设备初始化一行 device_name；原设置仍须保留。
			want++
		}
		if got := countRows(t, s.db, tb); got != want {
			t.Errorf("迁移后 %s 行数 %d, 期望 %d", tb, got, want)
		}
	}

	// upstreams 重建后行数据无损：id/name/type/base_url/disabled/时间戳原值，
	// 密文逐字节无损（能用同一把设备密钥解出 last4）。
	ups, err := s.ListUpstreams(ctx)
	if err != nil || len(ups) != 3 {
		t.Fatalf("ListUpstreams = %d 行 (err=%v), 期望 3", len(ups), err)
	}
	wantUp := map[int64]struct {
		name, typ, last4, baseURL string
		disabled                  bool
	}{
		1: {"ds-main", "deepseek", "0001", "", false},
		2: {"ark-plan", "ark_plan", "9876", "", false},
		3: {"mock-up", "mock", "", "http://127.0.0.1:18080/v1", true},
	}
	for _, u := range ups {
		w, ok := wantUp[u.ID]
		if !ok {
			t.Errorf("重建后出现未知上游 id=%d", u.ID)
			continue
		}
		if u.Name != w.name || u.Type != w.typ || u.APIKeyLast4 != w.last4 || u.BaseURL != w.baseURL || u.Disabled != w.disabled {
			t.Errorf("上游 %d 重建后失真: %+v, 期望 %+v", u.ID, u, w)
		}
		if fmtTime(u.CreatedAt) != legacyTS || fmtTime(u.UpdatedAt) != legacyTS {
			t.Errorf("上游 %d 时间戳失真: created=%s updated=%s", u.ID, fmtTime(u.CreatedAt), fmtTime(u.UpdatedAt))
		}
	}

	// models.kind 缺省回填 text，其余字段无损。
	for id, wantName := range map[int64]string{1: "deepseek-chat", 2: "retired-model"} {
		m, err := s.GetModelByID(ctx, id)
		if err != nil {
			t.Fatalf("GetModelByID(%d): %v", id, err)
		}
		if m.Kind != ModelKindText {
			t.Errorf("存量模型 %d kind = %q, 期望回填 %q", id, m.Kind, ModelKindText)
		}
		if m.Name != wantName {
			t.Errorf("存量模型 %d name = %q, 期望 %q", id, m.Name, wantName)
		}
	}
	if m, _ := s.GetModelByID(ctx, 2); m == nil || !m.Disabled {
		t.Error("存量模型 2 的禁用位丢失")
	}

	// model_sources 对重建后 upstreams 的引用完好：路由联查解出候选与凭证明文。
	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if route.Model.Kind != ModelKindText {
		t.Errorf("路由视图 kind = %q, 期望 %q", route.Model.Kind, ModelKindText)
	}
	if len(route.Candidates) != 2 {
		t.Fatalf("候选 %d 条, 期望 2", len(route.Candidates))
	}
	if c := route.Candidates[0]; c.Upstream.Name != "ds-main" || c.Upstream.APIKey != "sk-legacy-deepseek-0001" {
		t.Errorf("候选 0 失真: upstream=%s keyLen=%d", c.Upstream.Name, len(c.Upstream.APIKey))
	}
	if c := route.Candidates[1]; c.Upstream.Name != "ark-plan" || c.UpstreamModelID != "ds-on-plan" {
		t.Errorf("候选 1 失真: upstream=%s upstreamModelID=%s", c.Upstream.Name, c.UpstreamModelID)
	}

	// 外键在重建后的表上仍然生效：悬空引用被拒、被引用上游禁止删除。
	if _, err := s.CreateModelSource(ctx, 1, 9999, "", 300); !errors.Is(err, ErrConflict) {
		t.Errorf("引用不存在上游应返回 ErrConflict（外键失效?）, got %v", err)
	}
	if err := s.DeleteUpstream(ctx, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("删除被引用上游应返回 ErrConflict（外键失效?）, got %v", err)
	}
	// 唯一索引（name 内联 UNIQUE）随重建原样存在。
	if _, err := s.CreateUpstream(ctx, "ds-main", "deepseek", "sk-dup-00000000", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("重名上游应返回 ErrConflict（唯一索引失效?）, got %v", err)
	}
	// CHECK 扩入 minimax；非法类型仍被拒。
	if _, err := s.CreateUpstream(ctx, "mm", "minimax", "sk-minimax-plaintext-7777", ""); err != nil {
		t.Errorf("重建后 minimax 类型应可用: %v", err)
	}
	if _, err := s.CreateUpstream(ctx, "bogus-up", "bogus", "", ""); err == nil {
		t.Error("非法上游类型应被 CHECK 拒绝")
	}

	// settings 原样可读（重建不波及旁表；0019 也不碰它——上游凭据与模型目录
	// 才是这台设备真正的配置）。
	if got, err := s.GetSetting(ctx, "migration_sentinel"); err != nil || got != "preserved" {
		t.Errorf("settings 失真: %q (err=%v)", got, err)
	}

	// 0019 的契约：users 整张表消失，账号侧另外三张清空重建。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("0019 之后 users 表仍在 (n=%d, err=%v)", n, err)
	}
	for _, tb := range []string{"api_keys", "sessions", "audit_events"} {
		if got := countRows(t, s.db, tb); got != 0 {
			t.Errorf("0019 之后 %s 应清空，实有 %d 行", tb, got)
		}
	}

	// 重建后的 schema 能被再次 Open 完整解析（sqlite_schema 里的新表定义与
	// 外键字面都健全），且不再重复应用 0005。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重建后再次 Open: %v", err)
	}
	defer s2.Close()
	if route, err := s2.ResolveModelRoute(ctx, "deepseek-chat"); err != nil || len(route.Candidates) != 2 {
		t.Errorf("重开后路由失真 (err=%v): %+v", err, route)
	}
}

// 空的 0004 存量库（连一行数据都没有）也能升级：重建路径对空表同样成立。
func TestMigration0005OnEmptyLegacyDB(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 4)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	if _, err := s.CreateUpstream(context.Background(), "mm", "minimax", "sk-minimax-plaintext-7777", ""); err != nil {
		t.Errorf("空库升级后 minimax 类型应可用: %v", err)
	}
}

// 重建标记只认独占一行的精确形态：普通注释里**提到**标记不触发重建路径。
func TestHasRebuildDirective(t *testing.T) {
	if !hasRebuildDirective("-- 0005\n-- migrate:foreign_keys=off\nCREATE TABLE x(a);") {
		t.Error("独占一行的标记应被识别")
	}
	if !hasRebuildDirective("\t-- migrate:foreign_keys=off  \r\n") {
		t.Error("首尾空白应被容忍")
	}
	if hasRebuildDirective("-- 不同于 0005，本迁移不用 -- migrate:foreign_keys=off 标记\nALTER TABLE x ADD COLUMN b;") {
		t.Error("普通注释里提到标记不应触发重建路径")
	}
}

// 全新库直接建到 0005：minimax 类型可用、非法类型仍被拒、管理视图往返无损。
func TestUpstreamTypeMinimax(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	mm := mustUpstream(t, s, "minimax-main", "minimax", "sk-minimax-plaintext-7777", "")
	got, err := s.GetUpstreamByID(ctx, mm.ID)
	if err != nil || got.Type != "minimax" || got.APIKeyLast4 != "7777" {
		t.Fatalf("minimax 上游往返失真 (err=%v): %+v", err, got)
	}
	if _, err := s.CreateUpstream(ctx, "bad", "hailuo", "", ""); err == nil {
		t.Error("非法上游类型应被 CHECK 拒绝")
	}
}

// 模型 kind：创建三种 kind 往返；非法与空 kind 被 CHECK 拒绝；各视图
// （GetModelByID / ListModelsWithSources / ResolveModelRoute / GetSourceRoute）
// 都透出 kind；两条 servable 读数是文本口径、视频模型不进（Phase 2 谓词钉
// kind=text，集合不变的绊线在 store_test.TestServableModelsTextOnly）；
// 改名/启停不动 kind（建后不可改，store 层不提供任何 kind 写方法）。
func TestModelKind(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	text := mustModel(t, s, "deepseek-chat")
	if text.Kind != ModelKindText {
		t.Errorf("mustModel kind = %q, 期望 %q", text.Kind, ModelKindText)
	}
	video, err := s.CreateModel(ctx, "doubao-seedance", ModelKindVideo, "")
	if err != nil || video.Kind != ModelKindVideo {
		t.Fatalf("CreateModel(video) = %+v (err=%v)", video, err)
	}
	image, err := s.CreateModel(ctx, "doubao-seedream", ModelKindImage, "")
	if err != nil || image.Kind != ModelKindImage {
		t.Fatalf("CreateModel(image) = %+v (err=%v)", image, err)
	}
	if _, err := s.CreateModel(ctx, "bad-kind", "audio", ""); err == nil {
		t.Error("非法 kind 应被 CHECK 拒绝")
	}
	if _, err := s.CreateModel(ctx, "empty-kind", "", ""); err == nil {
		t.Error("空 kind 应被 CHECK 拒绝（调用方必须显式选择）")
	}

	if got, err := s.GetModelByID(ctx, video.ID); err != nil || got.Kind != ModelKindVideo {
		t.Errorf("GetModelByID kind = %v (err=%v), 期望 video", got, err)
	}

	// 挂一条来源让 video 模型进入各联查视图。
	mm := mustUpstream(t, s, "minimax-main", "minimax", "sk-minimax-plaintext-7777", "")
	src := mustSource(t, s, video.ID, mm.ID, "MiniMax-H3", 100)

	route, err := s.ResolveModelRoute(ctx, "doubao-seedance")
	if err != nil || route.Model.Kind != ModelKindVideo {
		t.Errorf("ResolveModelRoute kind = %v (err=%v), 期望 video", route, err)
	}
	sr, err := s.GetSourceRoute(ctx, src.ID)
	if err != nil || sr.Model.Kind != ModelKindVideo {
		t.Errorf("GetSourceRoute kind = %v (err=%v), 期望 video", sr, err)
	}
	withSources, err := s.ListModelsWithSources(ctx)
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	kinds := map[string]string{}
	for _, m := range withSources {
		kinds[m.Name] = m.Kind
	}
	if kinds["deepseek-chat"] != ModelKindText || kinds["doubao-seedance"] != ModelKindVideo || kinds["doubao-seedream"] != ModelKindImage {
		t.Errorf("ListModelsWithSources kind 失真: %v", kinds)
	}
	// servable 读数是文本口径（/v1/models 的契约）：唯一挂了来源的模型是
	// video，所以这里必须是空集——video 模型有可用来源也不进文本读数。
	servable, err := s.ListServableModels(ctx)
	if err != nil || len(servable) != 0 {
		t.Errorf("ListServableModels = %+v (err=%v), 期望空（video 模型不进文本口径）", servable, err)
	}

	// 改名/启停都不动 kind；store 层没有改 kind 的方法。
	if err := s.RenameModel(ctx, video.ID, "doubao-seedance-2"); err != nil {
		t.Fatalf("RenameModel: %v", err)
	}
	if err := s.SetModelDisabled(ctx, video.ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}
	if got, _ := s.GetModelByID(ctx, video.ID); got == nil || got.Kind != ModelKindVideo {
		t.Errorf("改名/停用后 kind 失真: %+v", got)
	}
}

// mustAIGCTask 落一条任务行，失败即终止测试。
func mustAIGCTask(t *testing.T, s *Store, nt NewAIGCTask) *AIGCTask {
	t.Helper()
	task, err := s.CreateAIGCTask(context.Background(), nt)
	if err != nil {
		t.Fatalf("CreateAIGCTask(%s): %v", nt.ID, err)
	}
	return task
}

// aigc_tasks CRUD 与归属：创建往返、别的密钥与不存在同回 ErrNotFound
// （无 oracle）、设备 id 与 (vendor_task_id, upstream_id) 唯一、观测更新仅在
// 变化时写、按保留期清理。**归属主语是签发请求的那把密钥**（0019 起）。
func TestAIGCTaskCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	const (
		aliceKey = int64(3)
		bobKey   = int64(4)
	)

	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	nt := NewAIGCTask{
		ID: "agt-alpha0001", VendorTaskID: "cgt-2026-alpha", UpstreamID: 7, UpstreamName: "ark-main",
		ModelName: "doubao-seedance-pro", Kind: ModelKindVideo,
		KeyID: aliceKey, KeyDisplay: "sk_abcd1234",
		Status: "queued", HasVideoInput: true, GenerateAudio: true, ServiceTier: "default",
		ReqResolution: "720p", ReqDuration: "5", CreatedAt: base,
	}
	created := mustAIGCTask(t, s, nt)
	if created.ID != nt.ID || created.Status != "queued" || !created.HasVideoInput || !created.GenerateAudio ||
		created.ServiceTier != "default" || created.ReqResolution != "720p" || created.ReqDuration != "5" ||
		!created.CreatedAt.Equal(base) || !created.UpdatedAt.Equal(base) {
		t.Errorf("CreateAIGCTask 返回不完整: %+v", created)
	}

	got, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", aliceKey)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if got.ID != "agt-alpha0001" || got.VendorTaskID != "cgt-2026-alpha" || got.UpstreamID != 7 || got.UpstreamName != "ark-main" ||
		got.ModelName != "doubao-seedance-pro" || got.Kind != ModelKindVideo ||
		got.KeyID != aliceKey || got.KeyDisplay != "sk_abcd1234" ||
		got.ErrorCode != "" || got.ErrorMessage != "" || got.UsageJSON != "" ||
		got.ContentURL != "" || got.LastFrameURL != "" || !got.CreatedAt.Equal(base) {
		t.Errorf("GetAIGCTaskByVendorIDForKey 往返失真: %+v", got)
	}

	// 归属：别的密钥与不存在的任务一律 ErrNotFound，不区分两种情况。
	if _, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", bobKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("他人任务应返回 ErrNotFound, got %v", err)
	}
	if _, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-nonexistent", aliceKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在任务应返回 ErrNotFound, got %v", err)
	}

	// 内部主键唯一；同一上游的同一厂商任务唯一，跨上游同 id 互不相干。
	dup := nt
	dup.VendorTaskID = "cgt-2026-other"
	if _, err := s.CreateAIGCTask(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("重复内部主键应返回 ErrConflict, got %v", err)
	}
	dupVendor := nt
	dupVendor.ID = "agt-alpha0002"
	if _, err := s.CreateAIGCTask(ctx, dupVendor); !errors.Is(err, ErrConflict) {
		t.Errorf("重复 (vendor_task_id, upstream_id) 应返回 ErrConflict, got %v", err)
	}
	// 观测更新：变化才写；等值观测不写也不动 updated_at；不存在的行报未写。
	// 观测按内部主键定位（调用方手里已有整行），读回按厂商 id（客户端视角）。
	ob := AIGCObservation{
		Status: "succeeded", UsageJSON: `{"output_seconds":5}`,
		ContentURL: "https://cdn.example.com/v.mp4", LastFrameURL: "https://cdn.example.com/f.png",
	}
	wrote, err := s.UpdateAIGCTaskObserved(ctx, "agt-alpha0001", ob)
	if err != nil || !wrote {
		t.Fatalf("UpdateAIGCTaskObserved = %v (err=%v), 期望写入", wrote, err)
	}
	after, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", aliceKey)
	if err != nil || after.Status != "succeeded" || after.UsageJSON != `{"output_seconds":5}` ||
		after.ContentURL != "https://cdn.example.com/v.mp4" || after.LastFrameURL != "https://cdn.example.com/f.png" {
		t.Fatalf("观测更新未生效 (err=%v): %+v", err, after)
	}
	if after.UpdatedAt.Equal(after.CreatedAt) {
		t.Errorf("观测更新应改写 updated_at: created=%v updated=%v", after.CreatedAt, after.UpdatedAt)
	}
	wrote, err = s.UpdateAIGCTaskObserved(ctx, "agt-alpha0001", ob)
	if err != nil || wrote {
		t.Errorf("等值观测应跳过写入, wrote=%v err=%v", wrote, err)
	}
	if again, _ := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", aliceKey); again == nil || !again.UpdatedAt.Equal(after.UpdatedAt) {
		t.Error("等值观测不应推进 updated_at")
	}
	if wrote, err := s.UpdateAIGCTaskObserved(ctx, "agt-nonexistent", ob); err != nil || wrote {
		t.Errorf("不存在的任务观测应报未写入, wrote=%v err=%v", wrote, err)
	}

	// 失败观测（任务级错误载入）。
	third := nt
	third.ID = "agt-alpha0004"
	third.VendorTaskID = "cgt-2026-third"
	third.CreatedAt = base.Add(2 * time.Minute)
	mustAIGCTask(t, s, third)
	failOb := AIGCObservation{Status: "failed", ErrorCode: "1026", ErrorMessage: "输出内容审核未通过"}
	if wrote, err := s.UpdateAIGCTaskObserved(ctx, "agt-alpha0004", failOb); err != nil || !wrote {
		t.Fatalf("失败观测应写入, wrote=%v err=%v", wrote, err)
	}
	if got, _ := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-third", aliceKey); got == nil ||
		got.ErrorCode != "1026" || got.ErrorMessage != "输出内容审核未通过" {
		t.Errorf("失败观测往返失真: %+v", got)
	}

	// 跨上游撞同名厂商 id 的病理情形：按厂商 id 检索取**最新**一行。
	crossUpstream := nt
	crossUpstream.ID = "agt-alpha0003"
	crossUpstream.UpstreamID = 8
	crossUpstream.UpstreamName = "minimax-main"
	crossUpstream.CreatedAt = base.Add(time.Minute)
	if _, err := s.CreateAIGCTask(ctx, crossUpstream); err != nil {
		t.Errorf("跨上游的同厂商 id 应可入库: %v", err)
	}
	if newest, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", aliceKey); err != nil || newest.ID != "agt-alpha0003" {
		t.Errorf("同名厂商 id 应取最新行 (err=%v): %+v", err, newest)
	}

	// 归属边界再压一条：bob 的行不影响 alice 的检索。
	bobTask := nt
	bobTask.ID = "agt-bravo0001"
	bobTask.VendorTaskID = "cgt-2026-bravo"
	bobTask.KeyID = bobKey
	bobTask.UpstreamID = 9
	bobTask.CreatedAt = base.Add(3 * time.Minute)
	mustAIGCTask(t, s, bobTask)
	if _, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-bravo", aliceKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("他人厂商 id 应返回 ErrNotFound, got %v", err)
	}

	// 清理：早于保留线的行删除并返回条数，其余原样保留。
	n, err := s.PruneAIGCTasks(ctx, base.Add(90*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("PruneAIGCTasks = %d (err=%v), 期望删除 2 行（base 与 base+1min）", n, err)
	}
	if _, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-alpha", aliceKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("已清理任务应不可见, got %v", err)
	}
	if kept, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-2026-third", aliceKey); err != nil || kept.ID != "agt-alpha0004" {
		t.Errorf("清理后剩余失真 (err=%v): %+v", err, kept)
	}
	if n, err := s.PruneAIGCTasks(ctx, base.Add(90*time.Second)); err != nil || n != 0 {
		t.Errorf("重复清理应为 0 行 (err=%v): %d", err, n)
	}
}

// GetRouteUpstreamByID（任务面钉死上游的路由视图，迭代 8 Phase 3）：按主键
// 解密凭证；disabled 位放行并原样带回（停用只挡新提交，已受理任务照常查询/
// 取消）；密文解不开报 ErrKeyUnreadable；未命中报 ErrNotFound。
func TestGetRouteUpstreamByID(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const key = "sk-pinned-route-3456"
	up := mustUpstream(t, s, "pinned-up", upstreamTypeDeepseek, key, "")

	ru, err := s.GetRouteUpstreamByID(ctx, up.ID)
	if err != nil {
		t.Fatalf("GetRouteUpstreamByID: %v", err)
	}
	if ru.ID != up.ID || ru.Name != "pinned-up" || ru.Type != upstreamTypeDeepseek {
		t.Errorf("上游行异常: %+v", ru)
	}
	if ru.APIKey != key {
		t.Errorf("APIKey = %q，期望解密明文", ru.APIKey)
	}

	// 停用后照样可取，启停事实原样带回（任务面靠这一点保住已受理任务）。
	if err := s.SetUpstreamDisabled(ctx, up.ID, true); err != nil {
		t.Fatal(err)
	}
	ru, err = s.GetRouteUpstreamByID(ctx, up.ID)
	if err != nil {
		t.Fatalf("停用后 GetRouteUpstreamByID: %v", err)
	}
	if !ru.Disabled || ru.APIKey != key {
		t.Errorf("停用行应带回 disabled 与解密凭证: %+v", ru)
	}

	// 密文损坏 → ErrKeyUnreadable。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upstreams SET api_key_sealed = ? WHERE id = ?`, "bm90LWEtdmFsaWQtY2lwaGVy", up.ID); err != nil {
		t.Fatalf("注入损坏密文: %v", err)
	}
	if _, err := s.GetRouteUpstreamByID(ctx, up.ID); !errors.Is(err, ErrKeyUnreadable) {
		t.Errorf("损坏密文应报 ErrKeyUnreadable, got %v", err)
	}

	if _, err := s.GetRouteUpstreamByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知上游应报 ErrNotFound, got %v", err)
	}
}
