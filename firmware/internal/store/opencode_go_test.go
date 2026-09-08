package store

// 迁移 0033（upstreams 第五次重建，CHECK 扩入 'opencode_go'——OpenCode Go 包月
// 套餐）的可执行验收：在带 0032 存量数据的库上前向迁移无损（含 0029 追加的
// egress_mode 与 0022 的 catalog_id / billing_mode 快照列）；opencode_go 行进各
// 视图；billingRankSQL 把它与 ark_plan / qwen_plan 一样判为订阅先行。存量库构造
// 复用 aigc_test.go 的 openLegacyDB / legacySealer。

import (
	"context"
	"strings"
	"testing"
)

// 迁移 0033 在带 0032 存量数据（含外键引用、密文与非缺省 egress_mode）的库上
// 前向迁移成功且无损；旧 CHECK 拒绝 opencode_go、新 CHECK 接受——重建确实发生了。
func TestMigration0033RebuildPreservesLegacyData(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0033")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 32)
	// 存量数据横跨重建会波及的外键网：两个上游（一个走代理出口的 deepseek、
	// 一个带 base_url 与目录快照的 openai_compat）、一个模型、两条来源。
	// 0032 库上插 opencode_go 必须被旧 CHECK 拒绝。
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, egress_mode, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', 'deepseek', 'usage', ?, '', 0, 'proxy', ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, egress_mode, created_at, updated_at) VALUES (2, 'kimi', 'openai_compat', 'kimi_cn', 'usage', ?, 'https://api.moonshot.cn/v1', 1, 'inherit', ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, kind, pricing, disabled, created_at, updated_at) VALUES (1, 'deepseek-chat', 'text', '', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (1, 1, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (2, 1, 2, 'kimi-k3', 200, 0, ?, ?)`, []any{legacyTS, legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO upstreams (name, type, api_key_sealed, created_at, updated_at) VALUES ('ocg', 'opencode_go', '', ?, ?)`, legacyTS, legacyTS); err == nil {
		t.Fatal("0032 的 CHECK 不应接受 opencode_go（旧约束失效?）")
	} else if !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("期望 CHECK 约束错误，得到: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	// Open 应用 0033（upstreams 重建），逐项验证无损。
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
	if ups[0].Name != "ds-main" || ups[0].Type != "deepseek" || ups[0].CatalogID != "deepseek" ||
		ups[0].BillingMode != "usage" || ups[0].EgressMode != "proxy" || ups[0].APIKeyLast4 != "0033" {
		t.Errorf("重建后 deepseek 行异常（密文与 egress_mode 应逐字节无损）: %+v", ups[0])
	}
	if ups[1].Name != "kimi" || ups[1].Type != "openai_compat" || ups[1].CatalogID != "kimi_cn" ||
		ups[1].BaseURL != "https://api.moonshot.cn/v1" || !ups[1].Disabled || ups[1].EgressMode != "inherit" {
		t.Errorf("重建后 openai_compat 行异常: %+v", ups[1])
	}
	if ups[0].CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00") != legacyTS {
		t.Errorf("重建后时间戳漂移: %v", ups[0].CreatedAt)
	}

	// 外键引用完好：两条来源仍指向原上游（含停用的那条，ResolveModelRoute 全收），
	// 凭证解密、地址快照与出站覆盖照常。
	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != 2 || route.Candidates[0].Upstream.ID != 1 ||
		route.Candidates[0].Upstream.APIKey != "sk-legacy-deepseek-0033" ||
		route.Candidates[0].Upstream.EgressMode != "proxy" ||
		route.Candidates[1].Upstream.BaseURL != "https://api.moonshot.cn/v1" || !route.Candidates[1].Upstream.Disabled {
		t.Errorf("重建后路由候选异常: %+v", route.Candidates)
	}

	// 新 CHECK 接受 opencode_go：建行、快照 billing_mode=subscription、进各视图。
	ocg, err := s.CreateUpstream(ctx, "opencode-go", upstreamTypeOpenCode, "sk-ocg-plaintext-not-real", "")
	if err != nil {
		t.Fatalf("重建后创建 opencode_go 上游: %v", err)
	}
	if ocg.BillingMode != "subscription" || ocg.CatalogID != "opencode_go" {
		t.Errorf("opencode_go 行的目录快照 = %q/%q，期望 opencode_go/subscription", ocg.CatalogID, ocg.BillingMode)
	}
	got, err := s.GetUpstreamByID(ctx, ocg.ID)
	if err != nil {
		t.Fatalf("GetUpstream(opencode_go): %v", err)
	}
	if got.Type != upstreamTypeOpenCode || got.APIKeyLast4 != "real" {
		t.Errorf("opencode_go 行读回异常: %+v", got)
	}
}

// billingRankSQL 的订阅集含 opencode_go：同优先级下它与 ark_plan / qwen_plan 一样
// 先于按量，订阅之间再按 id 定序（IN 列表只给出「是不是订阅」，不排订阅内部次序）。
func TestSourceOrderOpenCodeGoIsSubscription(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-ds-ocg-rank-plaintext-1111", "")
	ocg := mustUpstream(t, s, "opencode-go", upstreamTypeOpenCode, "sk-ocg-rank-plaintext-2222", "")
	qwen := mustUpstream(t, s, "qwen-plan", upstreamTypeQwenPlan, "sk-qwen-rank-plaintext-3333", "")

	m := mustModel(t, s, "rank-model")
	// 同优先级 100，按量的 ds 先建（id 最小）：订阅的两条仍须排在它前面。
	srcDS := mustSource(t, s, m.ID, ds.ID, "", 100)
	srcOCG := mustSource(t, s, m.ID, ocg.ID, "kimi-k3", 100)
	srcQwen := mustSource(t, s, m.ID, qwen.ID, "qwen3.8-max", 100)

	wantOrder := []int64{srcOCG.ID, srcQwen.ID, srcDS.ID}

	route, err := s.ResolveModelRoute(ctx, "rank-model")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != len(wantOrder) {
		t.Fatalf("候选数 = %d, 期望 %d", len(route.Candidates), len(wantOrder))
	}
	for i, want := range wantOrder {
		if route.Candidates[i].SourceID != want {
			t.Errorf("候选[%d].SourceID = %d, 期望 %d（opencode_go 须判为订阅先行）",
				i, route.Candidates[i].SourceID, want)
		}
	}
}
