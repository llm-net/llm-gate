package store

// 迁移 0026 的可执行验收：带 0025 存量数据的库里，Grok Build 订阅承载的
// xAI Imagine 行（xai_image/xai_video 模型行、挂在 grok_agent 锚点上游上的
// 来源行、锚点上游本身）在前向迁移时被整组清掉；其余上游、模型、来源逐字节
// 无损，订阅文本计价行（无来源）也原样留下。存量库构造复用 aigc_test.go 的
// openLegacyDB / legacySealer。

import (
	"context"
	"testing"
)

func TestMigration0026DropsGrokAgentRows(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0026")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 25)
	const insertModel = `INSERT INTO models (id, name, kind, family, pricing, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, '', 0, ?, ?)`
	const insertSource = `INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (?, ?, ?, '', 100, 0, ?, ?)`
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', 'deepseek', 'usage', ?, '', 0, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (2, 'Grok Build', 'grok_agent', 'grok_agent', 'subscription', '', '', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{insertModel, []any{1, "deepseek-chat", "text", "", legacyTS, legacyTS}},
		{insertModel, []any{2, "grok-4.6", "text", "", legacyTS, legacyTS}},
		{insertModel, []any{3, "grok-imagine-image", "image", "xai_image", legacyTS, legacyTS}},
		{insertModel, []any{4, "grok-imagine-video", "video", "xai_video", legacyTS, legacyTS}},
		{insertSource, []any{1, 1, 1, legacyTS, legacyTS}},
		{insertSource, []any{2, 3, 2, legacyTS, legacyTS}},
		{insertSource, []any{3, 4, 2, legacyTS, legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
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

	ups, err := s.ListUpstreams(ctx)
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	if len(ups) != 1 || ups[0].Name != "ds-main" || ups[0].Type != upstreamTypeDeepseek || ups[0].APIKeyLast4 != "0026" {
		t.Fatalf("迁移后上游异常（deepseek 行应无损、grok_agent 行应清掉）: %+v", ups)
	}

	models, err := s.ListModelsWithSources(ctx)
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	byName := make(map[string]*ModelWithSources, len(models))
	for i := range models {
		byName[models[i].Name] = &models[i]
	}
	if len(models) != 2 {
		t.Fatalf("迁移后模型行 = %+v，期望只剩 deepseek-chat 与 grok-4.6", models)
	}
	if m := byName["deepseek-chat"]; m == nil || len(m.Sources) != 1 || m.Sources[0].UpstreamID != 1 {
		t.Errorf("deepseek-chat 及其来源应无损: %+v", m)
	}
	if m := byName["grok-4.6"]; m == nil || m.Kind != ModelKindText || len(m.Sources) != 0 {
		t.Errorf("订阅文本计价行 grok-4.6 应原样留下: %+v", m)
	}
	for _, name := range []string{"grok-imagine-image", "grok-imagine-video"} {
		if byName[name] != nil {
			t.Errorf("xai 族模型行 %s 应被清掉: %+v", name, byName[name])
		}
	}
}
