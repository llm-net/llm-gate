package store

// 2026-08-09 扩展的可执行验收：迁移 0009（upstreams 第二次重建，CHECK 扩入
// 'openai_compat'）在带 0008 存量数据的库上前向迁移无损；openai_compat 行的
// CRUD 与路由视图；UpdateUpstreamAddressAndKey 的原子改址换 Key。
// 存量库构造复用 aigc_test.go 的 openLegacyDB / legacySealer。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 迁移 0009 在带 0008 存量数据（含外键引用与密文）的库上前向迁移成功且无损；
// 旧 CHECK 拒绝 openai_compat、新 CHECK 接受——重建确实发生了。
func TestMigration0009RebuildPreservesLegacyData(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealedDS, err := sealer.sealKey("sk-legacy-deepseek-0009")
	if err != nil {
		t.Fatalf("封存存量凭证: %v", err)
	}

	db := openLegacyDB(t, dir, 8)
	// 存量数据横跨重建会波及的外键网：两个上游（一个带 base_url 的 minimax）、
	// 一个模型、两条来源。0008 库上插 openai_compat 必须被旧 CHECK 拒绝。
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (1, 'ds-main', 'deepseek', ?, '', 0, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO upstreams (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at) VALUES (2, 'mm-intl', 'minimax', ?, 'https://api.minimax.io', 1, ?, ?)`, []any{sealedDS, legacyTS, legacyTS}},
		{`INSERT INTO models (id, name, kind, pricing, disabled, created_at, updated_at) VALUES (1, 'deepseek-chat', 'text', '', 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (1, 1, 1, '', 100, 0, ?, ?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id, model_id, upstream_id, upstream_model_id, priority, disabled, created_at, updated_at) VALUES (2, 1, 2, 'mm-side', 200, 0, ?, ?)`, []any{legacyTS, legacyTS}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("插入存量行 %q: %v", stmt.sql, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO upstreams (name, type, api_key_sealed, created_at, updated_at) VALUES ('oc', 'openai_compat', '', ?, ?)`, legacyTS, legacyTS); err == nil {
		t.Fatal("0008 的 CHECK 不应接受 openai_compat（旧约束失效?）")
	} else if !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("期望 CHECK 约束错误，得到: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	// Open 应用 0009（upstreams 第二次重建），逐项验证无损。
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
	if ups[0].Name != "ds-main" || ups[0].Type != "deepseek" || ups[0].APIKeyLast4 != "0009" {
		t.Errorf("重建后 deepseek 行异常（密文应逐字节无损）: %+v", ups[0])
	}
	if ups[1].Name != "mm-intl" || ups[1].BaseURL != "https://api.minimax.io" || !ups[1].Disabled {
		t.Errorf("重建后 minimax 行异常: %+v", ups[1])
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
		route.Candidates[0].Upstream.APIKey != "sk-legacy-deepseek-0009" {
		t.Fatalf("重建后路由候选异常: %+v", route.Candidates)
	}

	// 新 CHECK 接受 openai_compat；行为与其他类型一致地进各视图。
	oc, err := s.CreateUpstream(ctx, "oc-main", "openai_compat", "sk-compat-key-2233", "http://10.0.0.5:8000/v1")
	if err != nil {
		t.Fatalf("创建 openai_compat 上游: %v", err)
	}
	if oc.Type != "openai_compat" || oc.BaseURL != "http://10.0.0.5:8000/v1" || oc.APIKeyLast4 != "2233" {
		t.Errorf("openai_compat 行异常: %+v", oc)
	}
}

// TestUpdateUpstreamAddressAndKey：改址与换 Key 一条 UPDATE 原子生效；
// 重名 ErrConflict、未命中 ErrNotFound；不相关行不受影响。
func TestUpdateUpstreamAddressAndKey(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	oc, err := s.CreateUpstream(ctx, "oc", "openai_compat", "sk-compat-old-0001", "http://old.example.com/v1")
	if err != nil {
		t.Fatalf("创建上游: %v", err)
	}
	if _, err := s.CreateUpstream(ctx, "other", "openai_compat", "sk-compat-oth-0002", "http://other.example.com/v1"); err != nil {
		t.Fatalf("创建对照上游: %v", err)
	}

	if err := s.UpdateUpstreamAddressAndKey(ctx, oc.ID, "oc-renamed", "http://new.example.com/v1", "sk-compat-new-9944"); err != nil {
		t.Fatalf("UpdateUpstreamAddressAndKey: %v", err)
	}
	ru, err := s.GetRouteUpstreamByID(ctx, oc.ID)
	if err != nil {
		t.Fatalf("GetRouteUpstreamByID: %v", err)
	}
	if ru.Name != "oc-renamed" || ru.BaseURL != "http://new.example.com/v1" || ru.APIKey != "sk-compat-new-9944" {
		t.Errorf("原子改址换 Key 后行异常: %+v", ru)
	}

	if err := s.UpdateUpstreamAddressAndKey(ctx, oc.ID, "other", "http://x.example.com/v1", "sk-compat-new-9944"); !errors.Is(err, ErrConflict) {
		t.Errorf("重名应 ErrConflict，得到: %v", err)
	}
	if err := s.UpdateUpstreamAddressAndKey(ctx, 9999, "nope", "http://x.example.com/v1", "sk-compat-new-9944"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未命中应 ErrNotFound，得到: %v", err)
	}
	// 失败的调用不得波及目标行与对照行。
	if ru, err := s.GetRouteUpstreamByID(ctx, oc.ID); err != nil || ru.APIKey != "sk-compat-new-9944" {
		t.Errorf("冲突失败后目标行被改动: %+v err=%v", ru, err)
	}
}
