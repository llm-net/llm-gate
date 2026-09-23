package store

// 0034 迁移的可执行验收：存量库里每种订阅至多一行、Key 策略是四个布尔开关；
// 迁移后同一种订阅可有多行，开关为 1 的 Key 钉到当时那唯一一行，开关为 1 而
// 没有账号可钉的按未授权处理，revision 与 updated_at 原样保留。

import (
	"context"
	"testing"
)

func TestMigration0034PinsExistingSubscriptions(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 33)
	if _, err := db.Exec(`
		INSERT INTO api_keys (
			id, label, key_digest, display_prefix, display_last4, disabled, created_at,
			budget_day_micro, budget_week_micro, budget_month_micro, rpm_limit, plaintext_sealed
		) VALUES (7, 'existing', 'digest-existing', 'sk_existing', 'ting', 0, ?, NULL, NULL, NULL, NULL, '')`,
		legacyTS); err != nil {
		t.Fatalf("写入存量密钥: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO agent_accounts (id, provider, label, account_id, default_model, auth_json_sealed, status, created_at, updated_at)
		VALUES (3, 'codex', '旧账号', 'acct_fake', 'gpt-5-codex', 'sealed-placeholder', 'active', ?, ?)`,
		legacyTS, legacyTS); err != nil {
		t.Fatalf("写入存量订阅账号: %v", err)
	}
	// 旧 schema 的 UNIQUE(provider) 仍在：同 provider 第二行插不进去。
	if _, err := db.Exec(`
		INSERT INTO agent_accounts (provider, auth_json_sealed, status, created_at, updated_at)
		VALUES ('codex', 'x', 'active', ?, ?)`, legacyTS, legacyTS); err == nil {
		t.Fatal("0033 schema 应拒绝同 provider 第二行")
	}
	if _, err := db.Exec(`
		INSERT INTO api_key_devtool_configs (key_id, allow_codex_subscription, allow_grok_subscription,
		        allow_claude_subscription, allow_cursor_subscription, revision, updated_at)
		VALUES (7, 1, 1, 0, 0, 5, ?)`, legacyTS); err != nil {
		t.Fatalf("写入存量策略: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("迁移存量库: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	cfg, err := s.GetDevToolConfig(ctx, 7)
	if err != nil {
		t.Fatalf("读取迁移后策略: %v", err)
	}
	if cfg.CodexAccountID != 3 || cfg.GrokAccountID != 0 || cfg.ClaudeAccountID != 0 || cfg.CursorAccountID != 0 {
		t.Fatalf("回填不符: %+v（期望 codex 钉到 3，grok 没账号可钉即未授权）", cfg)
	}
	if cfg.Revision != 5 || cfg.UpdatedAt.IsZero() {
		t.Fatalf("revision/updated_at 未原样保留: %+v", cfg)
	}
	old, err := s.GetAgentAccount(ctx, 3)
	if err != nil || old.Label != "旧账号" || old.DefaultModel != "gpt-5-codex" || old.AccountID != "acct_fake" {
		t.Fatalf("存量账号失真: %+v err=%v", old, err)
	}
	// 去掉 UNIQUE(provider) 之后同一种订阅可以再录一个账号。
	second, err := s.UpsertAgentAccount(ctx, NewAgentAccount{Provider: AgentProviderCodex, Label: "新账号", AuthJSON: fakeAuthJSON})
	if err != nil {
		t.Fatalf("迁移后第二个 codex 账号: %v", err)
	}
	if second.ID == old.ID {
		t.Fatal("第二个账号覆盖了存量行")
	}
	// 钉死列上的外键：删掉账号，钉随之置空。
	if err := s.DeleteAgentAccount(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if cfg, err = s.GetDevToolConfig(ctx, 7); err != nil || cfg.CodexAccountID != 0 || cfg.Revision != 6 {
		t.Fatalf("删除账号后策略 = %+v err=%v，期望钉置空、revision 递增", cfg, err)
	}
	var applied int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 34`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("schema_migrations 缺版本 34 (n=%d, err=%v)", applied, err)
	}
}
