package store

// 2026-08-09 扩展的可执行验收：迁移 0010（upstreams 第三次重建，CHECK 扩入
// 'qwen_plan'——阿里云百炼通义千问 Token Plan 订阅套餐）在带 0009 存量数据的
// 库上前向迁移无损；qwen_plan 行进各视图；billingRankSQL 把它与 ark_plan 一样
// 判为订阅先行。存量库构造复用 aigc_test.go 的 openLegacyDB / legacySealer。

import (
	"context"
	"strings"
	"testing"
)

// 迁移 0010 在带 0009 存量数据（含外键引用与密文）的库上前向迁移成功且无损；
// 旧 CHECK 拒绝 qwen_plan、新 CHECK 接受——重建确实发生了。
func TestMigration0010RebuildPreservesLegacyData(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0010")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 9)
	// 存量数据横跨重建会波及的外键网：两个上游（一个带 base_url 的
	// openai_compat——0009 才有的类型，正好验证上一次重建的产物被完整继承）、
	// 一个模型、两条来源。0009 库上插 qwen_plan 必须被旧 CHECK 拒绝。
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', ?, '', 0, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (2, 'oc-lab', 'openai_compat', ?, 'http://10.0.0.5:8000/v1', 1, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, kind, pricing, disabled, created_at, updated_at) VALUES (1, 'deepseek-chat', 'text', '', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (1, 1, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (2, 1, 2, 'oc-side', 200, 0, ?, ?)`, []any{legacyTS, legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO upstreams (name, type, api_key_sealed, created_at, updated_at) VALUES ('qp', 'qwen_plan', '', ?, ?)`, legacyTS, legacyTS); err == nil {
		t.Fatal("0009 的 CHECK 不应接受 qwen_plan（旧约束失效?）")
	} else if !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("期望 CHECK 约束错误，得到: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	// Open 应用 0010（upstreams 第三次重建），逐项验证无损。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	ups, err := s.ListUpstreams(ctx)
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	if len(ups) != 2 {
		t.Fatalf("上游行数 = %d，期望 2", len(ups))
	}
	if ups[0].Name != "ds-main" || ups[0].Type != "deepseek" || ups[0].APIKeyLast4 != "0010" {
		t.Errorf("重建后 deepseek 行异常（密文应逐字节无损）: %+v", ups[0])
	}
	if ups[1].Name != "oc-lab" || ups[1].Type != "openai_compat" ||
		ups[1].BaseURL != "http://10.0.0.5:8000/v1" || !ups[1].Disabled {
		t.Errorf("重建后 openai_compat 行异常: %+v", ups[1])
	}
	if ups[0].CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00") != legacyTS {
		t.Errorf("重建后时间戳漂移: %v", ups[0].CreatedAt)
	}

	// 外键引用完好：来源仍指向原上游，路由解密照常。
	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != 2 || route.Candidates[0].Upstream.Name != "ds-main" ||
		route.Candidates[0].Upstream.APIKey != "sk-legacy-deepseek-0010" ||
		route.Candidates[1].Upstream.BaseURL != "http://10.0.0.5:8000/v1" {
		t.Fatalf("重建后路由候选异常: %+v", route.Candidates)
	}

	// 新 CHECK 接受 qwen_plan；行为与其他内置端点类型一致（无 base_url）。
	qp, err := s.CreateUpstream(ctx, "qwen-plan", upstreamTypeQwenPlan, "sk-qwen-plan-key-5566", "")
	if err != nil {
		t.Fatalf("创建 qwen_plan 上游: %v", err)
	}
	if qp.Type != upstreamTypeQwenPlan || qp.BaseURL != "" || qp.APIKeyLast4 != "5566" {
		t.Errorf("qwen_plan 行异常: %+v", qp)
	}
}

// billingRankSQL 的订阅集含 qwen_plan：同优先级下它与 ark_plan 一样先于按量，
// 两者之间再按 id 定序（IN 列表只给出「是不是订阅」，不排订阅内部次序）。
func TestSourceOrderQwenPlanIsSubscription(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-ds-qwen-rank-plaintext-1111", "")
	qwen := mustUpstream(t, s, "qwen-plan", upstreamTypeQwenPlan, "sk-qwen-rank-plaintext-2222", "")
	plan := mustUpstream(t, s, "ark-plan", upstreamTypeArkPlan, "sk-plan-rank-plaintext-3333", "")

	m := mustModel(t, s, "rank-model")
	// 同优先级 100，按量的 ds 先建（id 最小）：订阅的两条仍须排在它前面。
	srcDS := mustSource(t, s, m.ID, ds.ID, "", 100)
	srcQwen := mustSource(t, s, m.ID, qwen.ID, "qwen3-max", 100)
	srcPlan := mustSource(t, s, m.ID, plan.ID, "plan-chat", 100)

	wantOrder := []int64{srcQwen.ID, srcPlan.ID, srcDS.ID}

	route, err := s.ResolveModelRoute(ctx, "rank-model")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != len(wantOrder) {
		t.Fatalf("候选数 = %d, 期望 %d", len(route.Candidates), len(wantOrder))
	}
	for i, want := range wantOrder {
		if route.Candidates[i].SourceID != want {
			t.Errorf("候选[%d].SourceID = %d, 期望 %d（qwen_plan 须判为订阅先行）",
				i, route.Candidates[i].SourceID, want)
		}
	}
}
