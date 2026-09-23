package store

// api_key_devtool_configs（单把 API 密钥的开发工具策略）的 store 层可执行验收。
// admin 层的整份替换契约与审计在 internal/admin/devtools_test.go，这里只钉仓储行为：
//
//   - 缺配置行是四种订阅都未钉账号的正常空策略，不是错误；Key 不存在才 ErrNotFound。
//   - 每种工具钉一个账号行 id：INSERT 与 ON CONFLICT 两条路都写得到、读得回，
//     库里那一列存的是行 id（NULL = 未授权）。
//   - 钉的账号必须存在且 provider 相符，否则整份拒（ErrDevToolAccountInvalid）。
//   - 相等判断把四个钉都算在内：只换一个账号也是一次变更；等值保存不写盘、
//     revision 不虚增。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDevToolConfigPinnedAccountsRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	key := mustKey(t, s, "digest-devtools-1")
	cursor := mustAgent(t, s, AgentProviderCursor, fakeCursorAuthJSON)
	cursor2 := mustAgent(t, s, AgentProviderCursor, fakeCursorAuthJSON)
	claude := mustAgent(t, s, AgentProviderClaude, `{"setup_token":"fake"}`)

	// Key 存在、无配置行：正常空策略，四种订阅都没钉账号、revision=0。
	cfg, err := s.GetDevToolConfig(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetDevToolConfig: %v", err)
	}
	if cfg.CodexAccountID != 0 || cfg.GrokAccountID != 0 || cfg.ClaudeAccountID != 0 || cfg.CursorAccountID != 0 {
		t.Errorf("空策略应四种订阅都未钉账号: %+v", cfg)
	}
	if cfg.Revision != 0 {
		t.Errorf("空策略 revision = %d, 期望 0", cfg.Revision)
	}

	// Key 不存在才是 ErrNotFound（读写两侧同一口径）。
	if _, err := s.GetDevToolConfig(ctx, key.ID+999); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 Key 应 ErrNotFound, 得到 %v", err)
	}
	if _, _, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{KeyID: key.ID + 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 Key 应 ErrNotFound, 得到 %v", err)
	}

	// 钉的账号必须在场且是同一种订阅：不存在的行、别家的行都整份拒，不写盘。
	for _, bad := range []DevToolConfig{
		{KeyID: key.ID, CursorAccountID: cursor.ID + 999},
		{KeyID: key.ID, CodexAccountID: cursor.ID},
		{KeyID: key.ID, CursorAccountID: -1},
	} {
		if _, _, err := s.ReplaceDevToolConfig(ctx, bad); !errors.Is(err, ErrDevToolAccountInvalid) {
			t.Errorf("非法账号 %+v 应 ErrDevToolAccountInvalid, 得到 %v", bad, err)
		}
	}
	if cfg, err := s.GetDevToolConfig(ctx, key.ID); err != nil || cfg.Revision != 0 {
		t.Fatalf("被拒的写入不该留下配置行: %+v err=%v", cfg, err)
	}

	// 首份配置（INSERT 路）：只钉 cursor 也是一次变更，revision 从 1 开始。
	saved, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, CursorAccountID: cursor.ID,
	})
	if err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
	if !changed || saved.Revision != 1 || saved.CursorAccountID != cursor.ID {
		t.Fatalf("首份配置未按变更落库: changed=%v %+v", changed, saved)
	}

	// 落的确实是账号行 id；未钉的列是 NULL 而不是 0。
	var rawCursor, rawCodex sql.NullInt64
	if err := s.db.QueryRow(`SELECT cursor_account_id, codex_account_id FROM api_key_devtool_configs WHERE key_id = ?`, key.ID).Scan(&rawCursor, &rawCodex); err != nil {
		t.Fatalf("读钉死账号列: %v", err)
	}
	if !rawCursor.Valid || rawCursor.Int64 != cursor.ID || rawCodex.Valid {
		t.Errorf("列值 = cursor:%+v codex:%+v, 期望 cursor=%d、codex=NULL", rawCursor, rawCodex, cursor.ID)
	}

	// 等值保存：不算变更、revision 不虚增。
	again, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, CursorAccountID: cursor.ID,
	})
	if err != nil {
		t.Fatalf("等值保存: %v", err)
	}
	if changed || again.Revision != 1 {
		t.Errorf("等值保存不该动 revision: changed=%v revision=%d", changed, again.Revision)
	}

	// 读回与保存一致（其余三种订阅不受牵连）。
	got, err := s.GetDevToolConfig(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CursorAccountID != cursor.ID || got.CodexAccountID != 0 || got.GrokAccountID != 0 || got.ClaudeAccountID != 0 {
		t.Errorf("读回钉死账号不符: %+v", got)
	}
	if got.SubscriptionAccount(AgentProviderCursor) != cursor.ID || got.SubscriptionAccount(AgentProviderCodex) != 0 {
		t.Errorf("SubscriptionAccount 读数不符: %+v", got)
	}

	// ON CONFLICT 更新路：同一种订阅换到另一个账号是一次变更；再钉上 claude。
	next, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, CursorAccountID: cursor2.ID, ClaudeAccountID: claude.ID,
	})
	if err != nil {
		t.Fatalf("更新: %v", err)
	}
	if !changed || next.Revision != 2 || next.CursorAccountID != cursor2.ID || next.ClaudeAccountID != claude.ID {
		t.Fatalf("更新路未生效: changed=%v %+v", changed, next)
	}
	// 解开 cursor：列写回 NULL。
	if _, _, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{KeyID: key.ID, ClaudeAccountID: claude.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT cursor_account_id FROM api_key_devtool_configs WHERE key_id = ?`, key.ID).Scan(&rawCursor); err != nil {
		t.Fatal(err)
	}
	if rawCursor.Valid {
		t.Errorf("解开后列值 = %+v, 期望 NULL", rawCursor)
	}
}
