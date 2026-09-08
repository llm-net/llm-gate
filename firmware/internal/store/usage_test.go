package store

// iteration-9 Phase 1 的可执行验收：迁移 0006（usage_hourly + 定价/预算/清算
// 加列）在带 0005 存量数据的库上前向迁移无损且行为不变；usage_hourly 的相加
// 语义、区间读数、播种汇总与保留清理；aigc_tasks 清算的一次性语义；
// models.pricing 的形态无关校验与各视图透出；预算列随 KeyAuth 捎带。
//
// 存量库仍用 openLegacyDB（见 aigc_test.go）——它已能把库造到任意版本，
// 包含重建型迁移。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// bucketOf 把时刻折成小时桶号（与 internal/usage 将来的折算口径一致：
// unix 秒 / 3600，恒 UTC）。
func bucketOf(t time.Time) int64 { return t.UTC().Unix() / 3600 }

// sampleDelta 造一条维度齐全的增量，便于各用例只改自己关心的字段。
//
// **十三个计数列的取值必须两两不同且全部非零**：TestAddUsageDeltasAccumulates
// 整体比较结构体，只有互不相同的非零值才能同时揪出「某列漏进 DO UPDATE」
// （该列不再翻倍）与「usageColumns/usageArgs/scanUsageRow 三处列序对错位」
// （两列的值互换）。曾经 Errors/RejectedRequests/EstimatedRequests 三列恒为 0，
// 于是把它们从 UPSERT 里整段删掉，测试照样全绿——别再把任何一列写回 0
// （0007 的 VideoSeconds/ImageCount 同规矩，哪怕文本样本本身不会有秒和张）。
func sampleDelta(bucket int64) UsageDelta {
	return UsageDelta{
		BucketHour: bucket,
		KeyID:      7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: "deepseek-chat", UpstreamName: "ds-main",
		Entry: "chat", Kind: ModelKindText,
		Requests: 1, Errors: 3, RejectedRequests: 5, EstimatedRequests: 7,
		PromptTokens: 100, CompletionTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 5,
		TotalTokens: 120, VideoSeconds: 11, ImageCount: 13,
		CostMicro: 4200, DurationMsSum: 850,
	}
}

// mustAdd 冲刷一批增量，失败即终止。
func mustAdd(t *testing.T, s *Store, deltas ...UsageDelta) {
	t.Helper()
	if err := s.AddUsageDeltas(context.Background(), deltas); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}
}

// 迁移 0006 在带 0005 存量数据的库上前向迁移成功且**行为不变**：
// 存量行无损、新列取「不限/未定价/未清算」的缺省值、新表可用。
func TestMigration0006PreservesLegacyData(t *testing.T) {
	// 0006 走普通单事务路径：它的注释里**提到**了重建标记（说明自己为何不用），
	// 误判成重建型会让它跑在外键关闭的独占连接上——这条断言把那个陷阱钉死。
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == 6 && hasRebuildDirective(m.sql) {
			t.Fatalf("%s 被误判为重建型迁移", m.name)
		}
	}

	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0001")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 5)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', ?, '', 0, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, kind, disabled, created_at, updated_at) VALUES (1, 'deepseek-chat', 'text', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, kind, disabled, created_at, updated_at) VALUES (2, 'doubao-seedance', 'video', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (1, 1, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO users (id, name, role, password_hash, created_at, updated_at) VALUES (1, 'alice', 'admin', '$argon2id$fake', ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO api_keys (id, user_id, label, key_digest, display_prefix, display_last4, created_at) VALUES (1, 1, 'legacy', 'digest-legacy-1', 'sk_legacypre', 'zzzz', ?)`, []any{legacyTS}},
		{`INSERT INTO aigc_tasks (id, vendor_task_id, upstream_id, upstream_name, model_name, kind,
			user_id, user_name, key_id, key_display, status, usage_json, has_video_input,
			created_at, updated_at) VALUES ('agt-legacy', 'cgt-legacy', 1, 'ds-main', 'doubao-seedance', 'video',
			1, 'alice', 1, 'sk_legacypre…zzzz', 'succeeded', '{"completion_tokens":90396}', 1, ?, ?)`, []any{legacyTS, legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
	}
	// 只比**跨越 0019 幸存下来的表**（账号侧四表与 aigc_tasks 由 0019 清空
	// 重建，另行断言）。存量行照原样插进去——0006 要在完整的存量库上跑才算数。
	tables := []string{"upstreams", "models", "model_sources"}
	before := map[string]int64{}
	for _, tb := range tables {
		before[tb] = countRows(t, db, tb)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var v6 int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&v6); err != nil || v6 != 1 {
		t.Fatalf("schema_migrations 缺版本 6 (n=%d, err=%v)", v6, err)
	}
	for _, tb := range tables {
		if got := countRows(t, s.db, tb); got != before[tb] {
			t.Errorf("迁移后 %s 行数 %d, 期望 %d（数据丢失）", tb, got, before[tb])
		}
	}

	// 存量模型：未定价（空串），其余字段无损；路由视图同样带出 pricing 列。
	m, err := s.GetModelByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetModelByID: %v", err)
	}
	if m.Pricing != "" || m.Name != "deepseek-chat" || m.Kind != ModelKindText {
		t.Errorf("存量模型失真: %+v", m)
	}
	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil || route.Model.Pricing != "" || len(route.Candidates) != 1 {
		t.Fatalf("路由视图失真 (err=%v): %+v", err, route)
	}
	if got := route.Candidates[0].Upstream.APIKey; got != "sk-legacy-deepseek-0001" {
		t.Errorf("重建后凭证失真: len=%d", len(got))
	}

	// 0019 的契约：账号侧与账本侧的存量行整批退场（升级即清空重建）。
	for _, tb := range []string{"api_keys", "aigc_tasks", "usage_hourly"} {
		if got := countRows(t, s.db, tb); got != 0 {
			t.Errorf("0019 之后 %s 应清空，实有 %d 行", tb, got)
		}
	}
	if _, err := s.LookupKeyByDigest(ctx, "digest-legacy-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("0019 之后存量密钥应已清空, got %v", err)
	}

	// 新签的密钥：限额列一律 NULL = 不限（新库里谁都不会被突然拦住）。
	k := mustKey(t, s, "digest-fresh")
	if k.BudgetDayMicro != nil || k.BudgetWeekMicro != nil || k.BudgetMonthMicro != nil || k.RPMLimit != nil {
		t.Errorf("新签密钥限额应全为 nil（不限）: %+v", k)
	}
	ka, err := s.LookupKeyByDigest(ctx, "digest-fresh")
	if err != nil {
		t.Fatalf("LookupKeyByDigest: %v", err)
	}
	if ka.KeyBudgetDayMicro != nil || ka.KeyBudgetMonthMicro != nil || ka.KeyRPMLimit != nil {
		t.Errorf("鉴权快照的限额应全为 nil: %+v", ka)
	}
	if ka.KeyDisplay != "sk_prefix12…wxyz" {
		t.Errorf("KeyAuth.KeyDisplay = %q", ka.KeyDisplay)
	}

	// 新表可用：写入→读回一条即可（细粒度语义各有专门用例）。
	mustAdd(t, s, sampleDelta(bucketOf(time.Now())))
	rows, err := s.QueryUsageRange(ctx, 0, bucketOf(time.Now())+1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("升级后 usage_hourly 不可用: %d 行 (err=%v)", len(rows), err)
	}

	// 再次 Open 不重复应用 0006，数据也不受影响（迁移幂等，照 0005 的先例验）。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("升级后再次 Open: %v", err)
	}
	defer s2.Close()
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&v6); err != nil || v6 != 1 {
		t.Fatalf("重开后版本 6 记录数 = %d (err=%v)", v6, err)
	}
	if rows, err := s2.QueryUsageRange(ctx, 0, bucketOf(time.Now())+1); err != nil || len(rows) != 1 {
		t.Errorf("重开后用量行失真: %d 行 (err=%v)", len(rows), err)
	}
}

// AddUsageDeltas 的相加语义：同一维度键两次冲刷累加而非覆盖；不同维度键各成
// 一行；一批里重复出现同一键等价于分两次冲刷；空批是空操作。
func TestAddUsageDeltasAccumulates(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	bucket := bucketOf(time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC))

	if err := s.AddUsageDeltas(ctx, nil); err != nil {
		t.Fatalf("空批应是空操作: %v", err)
	}

	d := sampleDelta(bucket)
	mustAdd(t, s, d)
	mustAdd(t, s, d) // 第二次冲刷：同一维度键

	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同一维度键应合成 1 行, got %d", len(rows))
	}
	got := rows[0]
	want := UsageRow{
		BucketHour: bucket,
		KeyID:      7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: "deepseek-chat", UpstreamName: "ds-main",
		Entry: "chat", Kind: ModelKindText,
		Requests: 2, Errors: 6, RejectedRequests: 10, EstimatedRequests: 14,
		PromptTokens: 200, CompletionTokens: 40,
		CacheReadTokens: 60, CacheWriteTokens: 10, TotalTokens: 240,
		VideoSeconds: 22, ImageCount: 26,
		CostMicro: 8400, DurationMsSum: 1700,
	}
	if got != want {
		t.Errorf("累加结果 = %+v\n期望 %+v", got, want)
	}

	// 一批里重复同一键 = 分两次冲刷；同批的另一个维度键独立成行。
	other := sampleDelta(bucket)
	other.ModelName = "deepseek-reasoner"
	other.CostMicro = 100
	mustAdd(t, s, d, d, other)
	rows, err = s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("两个维度键应有 2 行, got %d", len(rows))
	}
	byModel := map[string]UsageRow{}
	for _, r := range rows {
		byModel[r.ModelName] = r
	}
	if n := byModel["deepseek-chat"].Requests; n != 4 {
		t.Errorf("deepseek-chat requests = %d, 期望 4", n)
	}
	if n := byModel["deepseek-reasoner"].CostMicro; n != 100 {
		t.Errorf("deepseek-reasoner cost = %d, 期望 100", n)
	}

	// 冲正：金额增量可以为负（长流中间结算按估算先记、流终按真值校平）。
	neg := sampleDelta(bucket)
	neg.Requests, neg.Errors, neg.RejectedRequests, neg.EstimatedRequests = 0, 0, 0, 0
	neg.PromptTokens, neg.CompletionTokens = 0, 0
	neg.CacheReadTokens, neg.CacheWriteTokens, neg.TotalTokens, neg.DurationMsSum = 0, 0, 0, 0
	neg.CostMicro = -400
	mustAdd(t, s, neg)
	rows, err = s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	for _, r := range rows {
		if r.ModelName == "deepseek-chat" && r.CostMicro != 8400*2-400 {
			t.Errorf("负增量冲正后 cost = %d, 期望 %d", r.CostMicro, 8400*2-400)
		}
	}
}

// 一批增量是**一个事务**：中途失败整批回滚，不留半截的账。
// 用一条只在测试里存在的触发器制造确定性的中途失败——库里没有任何 CHECK
// 能让合法 int64 写失败，而「批中第 k 条炸掉」正是要验的那条路径。
func TestAddUsageDeltasIsOneTransaction(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	bucket := bucketOf(time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC))

	if _, err := s.db.Exec(`CREATE TRIGGER usage_boom BEFORE INSERT ON usage_hourly
		WHEN NEW.model_name = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("建测试触发器: %v", err)
	}
	defer s.db.Exec(`DROP TRIGGER usage_boom`) //nolint:errcheck // 测试收尾

	good := sampleDelta(bucket)
	boom := sampleDelta(bucket)
	boom.ModelName = "boom"
	after := sampleDelta(bucket)
	after.ModelName = "after-boom"

	if err := s.AddUsageDeltas(ctx, []UsageDelta{good, boom, after}); err == nil {
		t.Fatal("批中失败应整批报错")
	}
	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("失败的批次不得留下任何行, got %+v", rows)
	}

	// 去掉炸弹后同一批照常整批落地（证明上面的空表不是「压根没写」）。
	if _, err := s.db.Exec(`DROP TRIGGER usage_boom`); err != nil {
		t.Fatalf("删测试触发器: %v", err)
	}
	mustAdd(t, s, good, after)
	if rows, err := s.QueryUsageRange(ctx, bucket, bucket+1); err != nil || len(rows) != 2 {
		t.Fatalf("重试应整批落地: %d 行 (err=%v)", len(rows), err)
	}
}

// 非键的维度快照列（key_display/kind）取**最后写入者**：首个写入者
// 若带了空的或过期的名字，后续冲刷能把它修回来，而不是整小时钉死在错值上。
func TestUsageSnapshotColumnsTakeLastWriter(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	bucket := bucketOf(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))

	first := sampleDelta(bucket)
	first.KeyDisplay, first.Kind = "", ""
	mustAdd(t, s, first)

	second := sampleDelta(bucket) // 同一维度键，快照列齐全
	mustAdd(t, s, second)

	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("QueryUsageRange = %d 行 (err=%v)", len(rows), err)
	}
	got := rows[0]
	if got.KeyDisplay != "sk_prefix12…wxyz" || got.Kind != ModelKindText {
		t.Errorf("快照列未被后写者修复: %+v", got)
	}
	if got.Requests != 2 { // 计数列照旧相加，不受快照覆盖影响
		t.Errorf("requests = %d, 期望 2", got.Requests)
	}
}

// 维度键的六个字段各自参与唯一性：任一不同即另起一行（kind 不在键内，
// 因为它由 entry 唯一决定）。
func TestUsageDimensionKey(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	bucket := bucketOf(time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC))

	base := sampleDelta(bucket)
	variants := []UsageDelta{base}
	for _, mut := range []func(d UsageDelta) UsageDelta{
		func(d UsageDelta) UsageDelta { d.BucketHour++; return d },
		func(d UsageDelta) UsageDelta { d.KeyID = 8; return d },
		func(d UsageDelta) UsageDelta { d.ModelName = "other-model"; return d },
		func(d UsageDelta) UsageDelta { d.UpstreamName = "other-up"; return d },
		func(d UsageDelta) UsageDelta { d.Entry = "messages"; return d },
	} {
		variants = append(variants, mut(base))
	}
	mustAdd(t, s, variants...)

	rows, err := s.QueryUsageRange(ctx, bucket, bucket+2)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != len(variants) {
		t.Fatalf("维度键各不相同应有 %d 行, got %d", len(variants), len(rows))
	}
}

// QueryUsageRange 的区间是左闭右开、按桶号升序，覆盖全部密钥。
func TestQueryUsageRangeWindow(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	base := bucketOf(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC))

	var deltas []UsageDelta
	for i := int64(0); i < 4; i++ {
		d := sampleDelta(base + i)
		d.CostMicro = 1000 + i
		deltas = append(deltas, d)
		other := sampleDelta(base + i)
		other.KeyID, other.KeyDisplay = 9, "sk_other…9999"
		other.CostMicro = 5000 + i
		deltas = append(deltas, other)
	}
	mustAdd(t, s, deltas...)

	rows, err := s.QueryUsageRange(ctx, base+1, base+3)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 4 { // 桶 base+1、base+2 各两把密钥；base、base+3 在窗外
		t.Fatalf("左闭右开区间应回 4 行, got %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].BucketHour < rows[i-1].BucketHour {
			t.Fatalf("结果未按桶号升序: %+v", rows)
		}
	}
	for _, r := range rows {
		if r.BucketHour < base+1 || r.BucketHour >= base+3 {
			t.Errorf("窗外的桶 %d 混入结果", r.BucketHour)
		}
	}
}

// 已知边界的绊线（不是期望行为，是**已记录的缺口**）：api_keys 的主键是不带
// AUTOINCREMENT 的 INTEGER PRIMARY KEY，删掉表中 id 最大的那把密钥后，下一把
// 新签的密钥复用同一个 id，于是继承前任在 usage_hourly 里的历史。
//
// 本用例把这个事实钉死，好让后续迭代改动删除语义/主键策略时立刻看见它，
// 也避免有人把「新密钥的账里混着旧账」当成新引入的回归。裁决（历史行随密钥
// 删除如何改名/归零）属于产品决定。
func TestUsageIDReuseInheritsHistory(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	bucket := bucketOf(time.Date(2026, 8, 8, 7, 0, 0, 0, time.UTC))

	old := mustKey(t, s, "digest-old-1")

	spent := sampleDelta(bucket)
	spent.KeyID, spent.KeyDisplay = old.ID, "sk_oldprefix…oooo"
	spent.CostMicro = 5_000_000
	mustAdd(t, s, spent)

	if err := s.DeleteAPIKey(ctx, old.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	fresh := mustKey(t, s, "digest-fresh-1")
	if fresh.ID != old.ID {
		t.Skipf("本次未复用 id（old=%d fresh=%d），绊线不适用", old.ID, fresh.ID)
	}

	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 || rows[0].KeyID != fresh.ID || rows[0].CostMicro != 5_000_000 {
		t.Fatalf("缺口形态已变（若已修复请连同本用例一起改写）: %+v", rows)
	}
	t.Logf("已知缺口：新密钥 id=%d 继承了前任的 %d 微元历史（key_display=%q）",
		fresh.ID, rows[0].CostMicro, rows[0].KeyDisplay)
}

// SumUsageSince 按 (桶号, 密钥) 汇总消费额并保留桶时刻——本地时区窗口的
// 折算归调用方，所以这里不能提前把桶合并掉。
func TestSumUsageSince(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	base := bucketOf(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC))

	// 同一桶、同一 (用户,密钥) 的两个模型要被合并；不同密钥要分开；
	// 早于 since 的桶不出现。
	old := sampleDelta(base - 1)
	old.CostMicro = 999
	a1 := sampleDelta(base)
	a1.CostMicro = 100
	a2 := sampleDelta(base)
	a2.ModelName, a2.CostMicro = "deepseek-reasoner", 200
	b1 := sampleDelta(base)
	b1.KeyID, b1.KeyDisplay, b1.CostMicro = 8, "sk_other…abcd", 300
	next := sampleDelta(base + 1)
	next.CostMicro = 400
	mustAdd(t, s, old, a1, a2, b1, next)

	sums, err := s.SumUsageSince(ctx, base)
	if err != nil {
		t.Fatalf("SumUsageSince: %v", err)
	}
	want := map[UsageCostBucket]bool{
		{BucketHour: base, KeyID: 7, CostMicro: 300}:     true,
		{BucketHour: base, KeyID: 8, CostMicro: 300}:     true,
		{BucketHour: base + 1, KeyID: 7, CostMicro: 400}: true,
	}
	if len(sums) != len(want) {
		t.Fatalf("SumUsageSince = %d 行 (%+v), 期望 %d 行", len(sums), sums, len(want))
	}
	for _, got := range sums {
		if !want[got] {
			t.Errorf("非预期的汇总行 %+v", got)
		}
	}
}

// PruneUsage 只删截止桶**之前**的行：谓词是 bucket_hour < before，
// 截止桶本身留下（保留期换算成桶号后直接当参数用，不必再 ±1）。
func TestPruneUsage(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	base := bucketOf(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC))

	var deltas []UsageDelta
	for i := int64(0); i < 5; i++ {
		deltas = append(deltas, sampleDelta(base+i))
	}
	mustAdd(t, s, deltas...)

	n, err := s.PruneUsage(ctx, base+2)
	if err != nil {
		t.Fatalf("PruneUsage: %v", err)
	}
	if n != 2 {
		t.Errorf("PruneUsage 删了 %d 行, 期望 2", n)
	}
	rows, err := s.QueryUsageRange(ctx, 0, base+100)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("清理后应剩 3 行, got %d", len(rows))
	}
	for _, r := range rows {
		if r.BucketHour < base+2 {
			t.Errorf("截止桶之前的行 %d 未被清理", r.BucketHour)
		}
	}
	// 幂等：同一截止点再删一次是 0 行。
	if n, err := s.PruneUsage(ctx, base+2); err != nil || n != 0 {
		t.Errorf("重复清理应删 0 行, got %d (err=%v)", n, err)
	}
}

// mustTask 造一条任务行，失败即终止。
func mustTask(t *testing.T, s *Store, id string, createdAt time.Time) *AIGCTask {
	t.Helper()
	task, err := s.CreateAIGCTask(context.Background(), NewAIGCTask{
		ID: id, VendorTaskID: "cgt-" + id, UpstreamID: 1, UpstreamName: "ark-main",
		ModelName: "doubao-seedance", Kind: ModelKindVideo,
		KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
		Status: "succeeded", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("CreateAIGCTask(%s): %v", id, err)
	}
	return task
}

// SettleAIGCTask 的一次性语义：条件更新只在 cost_micro IS NULL 时命中，
// 已清算行的二次清算是空操作（防「查询路径观测」与「对账协程」重叠入账），
// 行不存在同样返回 false。
func TestSettleAIGCTaskOnce(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	mustTask(t, s, "agt-settle", time.Now())

	settled, err := s.SettleAIGCTask(ctx, "agt-settle", 1_234_000, false)
	if err != nil {
		t.Fatalf("SettleAIGCTask: %v", err)
	}
	if !settled {
		t.Fatal("首次清算应命中")
	}
	task, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-agt-settle", 7)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.CostMicro == nil || *task.CostMicro != 1_234_000 || task.Estimated {
		t.Fatalf("清算后金额失真: cost=%v estimated=%v", task.CostMicro, task.Estimated)
	}
	updatedAfterSettle := task.UpdatedAt

	// 二次清算（另一条路径拿着不同金额来）：空操作，金额一个字节都不变。
	settled, err = s.SettleAIGCTask(ctx, "agt-settle", 9_999_999, true)
	if err != nil {
		t.Fatalf("SettleAIGCTask(二次): %v", err)
	}
	if settled {
		t.Error("已清算行的二次清算必须是空操作")
	}
	task, err = s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-agt-settle", 7)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if *task.CostMicro != 1_234_000 || task.Estimated {
		t.Errorf("二次清算改写了金额: cost=%d estimated=%v", *task.CostMicro, task.Estimated)
	}
	// 清算刻意不动 updated_at（那是「最近一次观测厂商」的时刻）。
	if !task.UpdatedAt.Equal(updatedAfterSettle) {
		t.Errorf("清算改动了 updated_at: %v → %v", updatedAfterSettle, task.UpdatedAt)
	}

	// 清算 0 元同样是一次真清算（厂商 usage 缺失 → 0 元 + estimated 标）：
	// 之后不会被对账协程反复捞起来重算。
	mustTask(t, s, "agt-zero", time.Now())
	if settled, err := s.SettleAIGCTask(ctx, "agt-zero", 0, true); err != nil || !settled {
		t.Fatalf("0 元清算应命中: settled=%v err=%v", settled, err)
	}
	if settled, err := s.SettleAIGCTask(ctx, "agt-zero", 5, false); err != nil || settled {
		t.Errorf("0 元已清算行不得再次入账: settled=%v err=%v", settled, err)
	}

	// 行不存在（已过保留期被清掉）也是 false，而不是错误。
	if settled, err := s.SettleAIGCTask(ctx, "agt-missing", 1, false); err != nil || settled {
		t.Errorf("不存在的任务清算应回 false: settled=%v err=%v", settled, err)
	}
}

// ListUnsettledAIGCTasks 只回「未清算 且 最近观测早于阈值」的行：已清算的、
// 以及刚被客户端轮询过的都不进对账协程的取件口。
func TestListUnsettledAIGCTasks(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustTask(t, s, "agt-stale-1", now.Add(-2*time.Hour))
	mustTask(t, s, "agt-stale-2", now.Add(-90*time.Minute))
	mustTask(t, s, "agt-settled", now.Add(-3*time.Hour))
	mustTask(t, s, "agt-fresh", now)

	// 两条陈旧行的 updated_at 随创建时间落定；agt-settled 先清算掉。
	if settled, err := s.SettleAIGCTask(ctx, "agt-settled", 42, false); err != nil || !settled {
		t.Fatalf("准备已清算行: settled=%v err=%v", settled, err)
	}

	tasks, err := s.ListUnsettledAIGCTasks(ctx, now.Add(-10*time.Minute), 100)
	if err != nil {
		t.Fatalf("ListUnsettledAIGCTasks: %v", err)
	}
	var ids []string
	for _, task := range tasks {
		ids = append(ids, task.ID)
		if task.CostMicro != nil {
			t.Errorf("已清算行 %s 混入未清算清单", task.ID)
		}
	}
	if fmt.Sprint(ids) != "[agt-stale-1 agt-stale-2]" { // 创建时间升序
		t.Errorf("未清算清单 = %v, 期望 [agt-stale-1 agt-stale-2]", ids)
	}

	// limit 是硬闸门：limit ≤ 0 回空（不是"不设限"），limit=1 只取最早那条。
	if got, err := s.ListUnsettledAIGCTasks(ctx, now.Add(-10*time.Minute), 0); err != nil || len(got) != 0 {
		t.Errorf("limit=0 应回空, got %d 行 (err=%v)", len(got), err)
	}
	if got, err := s.ListUnsettledAIGCTasks(ctx, now.Add(-10*time.Minute), 1); err != nil || len(got) != 1 || got[0].ID != "agt-stale-1" {
		t.Errorf("limit=1 应只回最早一条, got %+v (err=%v)", got, err)
	}

	// **谓词判的是「观测值多久没变过」，不是「多久没被轮询」**：等值观测
	// （迭代 8 的零写放大）不刷 updated_at，所以刚被轮询过、状态没变的任务
	// 依然留在取件口。这条与下一段刚好是一体两面，改任一侧都会撞上它。
	if wrote, err := s.UpdateAIGCTaskObserved(ctx, "agt-stale-2", AIGCObservation{Status: "succeeded"}); err != nil || wrote {
		t.Fatalf("等值观测不应写库: wrote=%v err=%v", wrote, err)
	}
	tasks, err = s.ListUnsettledAIGCTasks(ctx, now.Add(-10*time.Minute), 100)
	if err != nil {
		t.Fatalf("ListUnsettledAIGCTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("等值观测后仍应是 2 条（谓词不是「没被轮询」）, got %d", len(tasks))
	}

	// 客户端刚轮询过**且观测值有变化**（写回刷新 updated_at）才退出取件口——留给客户端
	// 自己的查询路径清算，不去和它抢上游配额。
	if _, err := s.UpdateAIGCTaskObserved(ctx, "agt-stale-1", AIGCObservation{Status: "running"}); err != nil {
		t.Fatalf("UpdateAIGCTaskObserved: %v", err)
	}
	tasks, err = s.ListUnsettledAIGCTasks(ctx, now.Add(-10*time.Minute), 100)
	if err != nil {
		t.Fatalf("ListUnsettledAIGCTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "agt-stale-2" {
		t.Errorf("刚观测过的任务应退出取件口, got %+v", tasks)
	}
}

// models.pricing：形态无关校验（合法 JSON 对象 + 值为非负整数）、空串=未定价、
// 建/改往返无损，且在管理与路由两类视图里都能读到。
func TestModelPricingRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const textPricing = `{"in":1000000,"out":2000000,"cache_read":100000}`
	m, err := s.CreateModel(ctx, "deepseek-chat", ModelKindText, textPricing)
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if m.Pricing != textPricing {
		t.Errorf("CreateModel 回值 pricing = %q", m.Pricing)
	}
	if got, err := s.GetModelByID(ctx, m.ID); err != nil || got.Pricing != textPricing {
		t.Errorf("GetModelByID pricing = %q (err=%v)", got.Pricing, err)
	}

	up, err := s.CreateUpstream(ctx, "ds-main", "deepseek", "sk-plain-0001", "")
	if err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	src, err := s.CreateModelSource(ctx, m.ID, up.ID, "", 100)
	if err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	// 三个联查视图都带出 pricing——计价发生在记账时点，路由视图带着价格
	// 回来才不用为它多查一次库。
	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil || route.Model.Pricing != textPricing {
		t.Errorf("ResolveModelRoute pricing = %q (err=%v)", route.Model.Pricing, err)
	}
	sr, err := s.GetSourceRoute(ctx, src.ID)
	if err != nil || sr.Model.Pricing != textPricing {
		t.Errorf("GetSourceRoute pricing = %q (err=%v)", sr.Model.Pricing, err)
	}
	list, err := s.ListModelsWithSources(ctx)
	if err != nil || len(list) != 1 || list[0].Pricing != textPricing {
		t.Errorf("ListModelsWithSources pricing 失真 (err=%v): %+v", err, list)
	}
	servable, err := s.ListServableModels(ctx)
	if err != nil || len(servable) != 1 || servable[0].Pricing != textPricing {
		t.Errorf("ListServableModels pricing 失真 (err=%v): %+v", err, servable)
	}

	// 改价：新价即刻可读（记账时点价，历史金额不重算——那是聚合表的事）。
	const videoPricing = `{"tier_with_video":46000000,"tier_no_video":28000000}`
	if err := s.SetModelPricing(ctx, m.ID, videoPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	if got, _ := s.GetModelByID(ctx, m.ID); got.Pricing != videoPricing {
		t.Errorf("改价后 pricing = %q", got.Pricing)
	}
	// 清为未定价。
	if err := s.SetModelPricing(ctx, m.ID, ""); err != nil {
		t.Fatalf("SetModelPricing(清空): %v", err)
	}
	if got, _ := s.GetModelByID(ctx, m.ID); got.Pricing != "" {
		t.Errorf("清空后 pricing = %q", got.Pricing)
	}
	if err := s.SetModelPricing(ctx, 9999, `{"in":1}`); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在模型的价格应回 ErrNotFound, got %v", err)
	}
}

// 形态无关校验拒绝的都是「计价函数没法安全消费」的输入：非对象、非整数、
// 负数、坏 JSON。形态字段集本身不在本层校验（那按 kind 在管理层做）。
func TestValidatePricingRejects(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	// 公用的干净模型：非法输入要在**改价**这条写入口上也被挡回去，
	// 而且挡回去之后库里的旧价格必须一个字节没变。
	victim, err := s.CreateModel(ctx, "pricing-victim", ModelKindText, `{"in":123}`)
	if err != nil {
		t.Fatalf("CreateModel(victim): %v", err)
	}

	for _, tc := range []struct {
		name    string
		pricing string
		ok      bool
	}{
		{"空串即未定价", "", true},
		{"显式 0 价", `{"in":0}`, true},
		{"上限值本身", `{"in":1000000000000}`, true},
		{"负数", `{"in":-1}`, false},
		{"小数", `{"in":1.5}`, false},
		{"科学计数", `{"in":1e6}`, false},
		{"字符串值", `{"in":"1000"}`, false},
		{"嵌套对象", `{"in":{"a":1}}`, false},
		{"数组值", `{"in":[1]}`, false},
		{"顶层数组", `[1,2]`, false},
		{"顶层数字", `42`, false},
		{"字面 null", `null`, false},
		{"坏 JSON", `{"in":`, false},
		{"尾随内容", `{"in":1} {"out":2}`, false},
		{"尾随垃圾", `{"in":1}garbage`, false},
		// dec.More() 对 '}' / ']' 返回 false，这几条正是它放行、Token() 才挡得住
		// 的形态：手输价格表多打一个闭括号是最常见的脏输入。
		{"多一个右花括号", `{"in":1000000}}`, false},
		{"多一个右方括号", `{"in":1000000}]`, false},
		{"空白后跟右括号", `{"in":1}   ]  x`, false},
		{"连串右括号加垃圾", `{"in":1}}}}garbage`, false},
		// 空对象 = 「定了价却没有任何字段」：会永远记 0 元又不挂未定价徽章。
		{"空对象", "{}", false},
		// 量级闸门：防 cost_micro 的相加溢出把整列悄悄转成 REAL。
		{"超过上限一微元", `{"in":1000000000001}`, false},
		{"接近 int64 上限", `{"in":9007199254740991}`, false},
		{"溢出 int64", `{"in":92233720368547758080}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePricing(tc.pricing)
			if tc.ok && err != nil {
				t.Fatalf("validatePricing(%q) = %v, 期望通过", tc.pricing, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validatePricing(%q) 应被拒绝", tc.pricing)
			}
			// 非法值绝不入库：建模型与改价**两条**写入口都要挡。
			// （只测 CreateModel 时，把 SetModelPricing 里的校验整段删掉
			// 测试依然全绿——这一半是补上的那道绊线。）
			if !tc.ok && !errors.Is(validatePricing(tc.pricing), ErrInvalidPricing) {
				t.Errorf("拒绝路径必须带 ErrInvalidPricing 哨兵，否则管理端点只能回 500")
			}
			name := "m-" + tc.name
			m, createErr := s.CreateModel(ctx, name, ModelKindText, tc.pricing)
			if tc.ok != (createErr == nil) {
				t.Fatalf("CreateModel(pricing=%q) err=%v, 期望 ok=%v", tc.pricing, createErr, tc.ok)
			}
			if !tc.ok {
				if !errors.Is(createErr, ErrInvalidPricing) {
					t.Errorf("CreateModel 的拒绝错误未带哨兵: %v", createErr)
				}
				setErr := s.SetModelPricing(ctx, victim.ID, tc.pricing)
				if !errors.Is(setErr, ErrInvalidPricing) {
					t.Fatalf("SetModelPricing(pricing=%q) 应带哨兵拒绝, got %v", tc.pricing, setErr)
				}
				if got, _ := s.GetModelByID(ctx, victim.ID); got.Pricing != `{"in":123}` {
					t.Fatalf("被拒的改价污染了旧价格: %q", got.Pricing)
				}
				return
			}
			if err := s.SetModelPricing(ctx, m.ID, tc.pricing); err != nil {
				t.Fatalf("SetModelPricing(pricing=%q) 应通过: %v", tc.pricing, err)
			}
		})
	}
}

// 预算列的往返：写入、清除（nil）与随 KeyAuth 捎带——准入是纯内存比较，
// 限额必须搭这一次点查的便车。
func TestBudgetColumnsRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	k := mustKey(t, s, "digest-alice-1")

	day, week, month, rpm := int64(50_000_000), int64(200_000_000), int64(900_000_000), int64(60)
	_ = month
	if err := s.SetAPIKeyLimits(ctx, k.ID, &day, &week, nil, &rpm); err != nil {
		t.Fatalf("SetAPIKeyLimits: %v", err)
	}

	gotKey, err := s.GetAPIKeyByID(ctx, k.ID)
	if err != nil {
		t.Fatalf("GetAPIKeyByID: %v", err)
	}
	if gotKey.BudgetDayMicro == nil || *gotKey.BudgetDayMicro != day ||
		gotKey.BudgetWeekMicro == nil || *gotKey.BudgetWeekMicro != week ||
		gotKey.BudgetMonthMicro != nil || gotKey.RPMLimit == nil || *gotKey.RPMLimit != rpm {
		t.Errorf("密钥限额失真: %+v", gotKey)
	}
	// 列表视图同样带限额（管理台要显示各密钥的预算）。
	keys, err := s.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].RPMLimit == nil || *keys[0].RPMLimit != rpm {
		t.Errorf("ListAPIKeys 限额失真 (err=%v): %+v", err, keys)
	}

	// 鉴权点查一次带回归属 + 启停 + 四个限额列。
	ka, err := s.LookupKeyByDigest(ctx, "digest-alice-1")
	if err != nil {
		t.Fatalf("LookupKeyByDigest: %v", err)
	}
	if ka.KeyID != k.ID || ka.KeyDisplay != "sk_prefix12…wxyz" {
		t.Errorf("KeyAuth 归属失真: %+v", ka)
	}
	if ka.KeyBudgetDayMicro == nil || *ka.KeyBudgetDayMicro != day ||
		ka.KeyBudgetWeekMicro == nil || *ka.KeyBudgetWeekMicro != week ||
		ka.KeyBudgetMonthMicro != nil ||
		ka.KeyRPMLimit == nil || *ka.KeyRPMLimit != rpm {
		t.Errorf("KeyAuth 限额失真: %+v", ka)
	}

	// 0 与 nil 是两个状态：0 = 一分钱都不许花，nil = 不限。
	zero := int64(0)
	if err := s.SetAPIKeyLimits(ctx, k.ID, &zero, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits(0): %v", err)
	}
	ka, err = s.LookupKeyByDigest(ctx, "digest-alice-1")
	if err != nil {
		t.Fatalf("LookupKeyByDigest: %v", err)
	}
	if ka.KeyBudgetDayMicro == nil || *ka.KeyBudgetDayMicro != 0 ||
		ka.KeyBudgetWeekMicro != nil || ka.KeyRPMLimit != nil {
		t.Errorf("0 与 nil 被混为一谈: %+v", ka)
	}

	// 清除：全部回 nil（不限）。
	if err := s.SetAPIKeyLimits(ctx, k.ID, nil, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits(清除): %v", err)
	}
	if got, _ := s.GetAPIKeyByID(ctx, k.ID); got.BudgetDayMicro != nil ||
		got.BudgetWeekMicro != nil || got.BudgetMonthMicro != nil || got.RPMLimit != nil {
		t.Errorf("清除后密钥限额应为 nil: %+v", got)
	}
	if err := s.SetAPIKeyLimits(ctx, 9999, &day, nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在密钥的限额应回 ErrNotFound, got %v", err)
	}
}

// SettleAIGCTaskWithUsage 的原子性（iteration-9 Phase 2）：**清算与入账在同一
// 个事务里**。分成两个事务的旧写法有一道致命窗口——进程在窗口里退出，那笔钱
// 既没进账本、行又已标清算，ListUnsettledAIGCTasks 的谓词从此过滤掉它，
// 没有任何一条路径会回来补账。
func TestSettleAIGCTaskWithUsageIsAtomic(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	mustTask(t, s, "agt-atomic", time.Now())

	bucket := bucketOf(time.Now())
	delta := UsageDelta{
		BucketHour: bucket, KeyID: 7, KeyDisplay: "sk_p…z",
		ModelName: "vid", UpstreamName: "ark-main", Entry: "video", Kind: ModelKindVideo,
		CostMicro: 6_909_520,
	}
	settled, err := s.SettleAIGCTaskWithUsage(ctx, "agt-atomic", delta.CostMicro, false, delta)
	if err != nil {
		t.Fatalf("SettleAIGCTaskWithUsage: %v", err)
	}
	if !settled {
		t.Fatal("首次清算应命中")
	}
	task, err := s.GetAIGCTaskByVendorIDForKey(ctx, "cgt-agt-atomic", 7)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.CostMicro == nil || *task.CostMicro != delta.CostMicro {
		t.Fatalf("任务行金额 = %v, 期望 %d", task.CostMicro, delta.CostMicro)
	}
	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 || rows[0].CostMicro != delta.CostMicro {
		t.Fatalf("账本行 = %+v, 期望一行 %d 微元", rows, delta.CostMicro)
	}

	// 二次清算：条件更新落空 → 返回 false **且一个增量都不写**。
	// 这里是重复入账最可能的入口（查询路径与对账协程天然重叠）。
	settled, err = s.SettleAIGCTaskWithUsage(ctx, "agt-atomic", delta.CostMicro, false, delta)
	if err != nil {
		t.Fatalf("SettleAIGCTaskWithUsage(二次): %v", err)
	}
	if settled {
		t.Error("已清算行的二次清算必须是空操作")
	}
	rows, _ = s.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != delta.CostMicro {
		t.Errorf("二次清算重复入账了: %+v", rows)
	}

	// 行不存在（已过保留期被清掉）同样 false 且不写增量。
	if settled, err := s.SettleAIGCTaskWithUsage(ctx, "agt-missing", 1, false, delta); err != nil || settled {
		t.Errorf("不存在的任务清算应回 false: settled=%v err=%v", settled, err)
	}
	rows, _ = s.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != delta.CostMicro {
		t.Errorf("行不存在时仍写了增量: %+v", rows)
	}
}

// GetModelPricing 是计价的窄读数：只回价签一列（清算路径不该经 ResolveModelRoute
// 把上游凭证明文拉进后台协程），模型已删返回 ErrNotFound。
func TestGetModelPricing(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	const pricing = `{"in":2000000,"out":8000000}`
	m, err := s.CreateModel(ctx, "priced-model", ModelKindText, pricing)
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := s.CreateModel(ctx, "free-model", ModelKindText, ""); err != nil {
		t.Fatalf("CreateModel(未定价): %v", err)
	}

	got, err := s.GetModelPricing(ctx, "priced-model")
	if err != nil {
		t.Fatalf("GetModelPricing: %v", err)
	}
	if got != pricing {
		t.Errorf("GetModelPricing = %q, 期望 %q", got, pricing)
	}
	if got, err := s.GetModelPricing(ctx, "free-model"); err != nil || got != "" {
		t.Errorf("未定价模型 = (%q, %v), 期望 (\"\", nil)", got, err)
	}
	if _, err := s.GetModelPricing(ctx, "no-such-model"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的模型 err = %v, 期望 ErrNotFound", err)
	}
	// 改价后读到的是新价（记账时点价的前提）。
	if err := s.SetModelPricing(ctx, m.ID, `{"in":1}`); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	if got, _ := s.GetModelPricing(ctx, "priced-model"); got != `{"in":1}` {
		t.Errorf("改价后 GetModelPricing = %q", got)
	}
}

// ---- iteration-9 Phase 7：账本补计费量（迁移 0007） ----

// 迁移 0007 在带 0006 存量数据（含已有账本行）的库上前向迁移成功：ALTER 在
// 非空表上跑得过、两个新列参与 UPSERT 相加，且 usage_hourly **仍然只有维度
// 唯一索引这一个索引**（0006 定的写入纪律，加列不该顺手加索引）。
func TestMigration0007AddsQuantityColumns(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 6)
	bucket := bucketOf(time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC))
	// 存量账本行：0006 时代写下的，没有秒也没有张。
	if _, err := db.Exec(`INSERT INTO usage_hourly
		(bucket_hour, user_id, user_name, key_id, key_display, model_name, upstream_name,
		 entry, kind, requests, total_tokens, cost_micro, duration_ms_sum)
		VALUES (?, 1, 'alice', 7, 'sk_prefix12…wxyz', 'doubao-seedance', 'ark-main',
		        'video', 'video', 1, 0, 4200, 900)`, bucket); err != nil {
		t.Fatalf("插入存量账本行: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var v7 int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&v7); err != nil || v7 != 1 {
		t.Fatalf("schema_migrations 缺版本 7 (n=%d, err=%v)", v7, err)
	}
	// **账本行不跨 0019**：那一版清空重建了 usage_hourly（用户维度整列退场，
	// 逐行合并搬运不如清空来得干净）。存量行照旧插进去——0007 的 ALTER 要在
	// 非空表上跑过才算数——但升级完就该是空的。
	if got := countRows(t, s.db, "usage_hourly"); got != 0 {
		t.Errorf("0019 之后 usage_hourly 应清空，实有 %d 行", got)
	}

	// 新列参与相加：同一维度键连冲两次，秒/张从 0 长到 12/4，其余计数照旧累加。
	d := sampleDelta(bucket)
	d.ModelName, d.UpstreamName, d.Entry, d.Kind = "doubao-seedance", "ark-main", "video", ModelKindVideo
	d.VideoSeconds, d.ImageCount = 6, 2
	mustAdd(t, s, d, d)
	rows, err := s.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同维度键应合成 1 行, got %d: %+v", len(rows), rows)
	}
	vid := rows[0]
	if vid.VideoSeconds != 12 || vid.ImageCount != 4 {
		t.Errorf("新列未参与 DO UPDATE 相加: %+v（秒/张会永远停在第一次冲刷的值）", vid)
	}
	if vid.Requests != 2 || vid.CostMicro != 2*4200 {
		t.Errorf("其余计数未累加: %+v", vid)
	}

	// 唯一索引仍是唯一的一个：加列不该顺手加索引（0006 的写入纪律）。
	idx, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'usage_hourly'`)
	if err != nil {
		t.Fatalf("列索引: %v", err)
	}
	defer idx.Close()
	var names []string
	for idx.Next() {
		var n string
		if err := idx.Scan(&n); err != nil {
			t.Fatalf("扫描索引名: %v", err)
		}
		names = append(names, n)
	}
	if len(names) != 1 || names[0] != "idx_usage_hourly_dim" {
		t.Errorf("usage_hourly 的索引 = %v, 期望只有 idx_usage_hourly_dim", names)
	}
}

// ListModelPricing 是「未定价」徽章的真值来源：整份目录的名字 → 价签，空串
// 表示未定价。**读目录而不是从金额反推**——反推的两个方向都会误判（见
// usage.Meter.annotatePricing 的注释）。
func TestListModelPricing(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	if _, err := s.CreateModel(ctx, "priced-model", ModelKindText, `{"in":2000000,"out":8000000}`); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := s.CreateModel(ctx, "unpriced-model", ModelKindText, ""); err != nil {
		t.Fatalf("CreateModel(未定价): %v", err)
	}
	// 已禁用的模型照样在目录里：账本里可能还有它这一区间的行。
	m, err := s.CreateModel(ctx, "disabled-model", ModelKindVideo, `{"ark_video_token":1}`)
	if err != nil {
		t.Fatalf("CreateModel(禁用): %v", err)
	}
	if err := s.SetModelDisabled(ctx, m.ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}

	got, err := s.ListModelPricing(ctx)
	if err != nil {
		t.Fatalf("ListModelPricing: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("目录 %d 项, 期望 3: %v", len(got), got)
	}
	if got["priced-model"] != `{"in":2000000,"out":8000000}` {
		t.Errorf("已定价模型 = %q", got["priced-model"])
	}
	if v, ok := got["unpriced-model"]; !ok || v != "" {
		t.Errorf("未定价模型 = (%q, %v), 期望 (\"\", true)——键必须在，空串才是「未定价」", v, ok)
	}
	if _, ok := got["disabled-model"]; !ok {
		t.Error("已禁用的模型不在目录读数里：它的历史账本行会被当成「目录里没有这个模型」")
	}
	if _, ok := got["no-such-model"]; ok {
		t.Error("凭空多出一个模型")
	}
}
