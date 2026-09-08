package store

// api_key_devtool_configs（单把 API 密钥的开发工具策略）的 store 层可执行验收
//（迭代 3 Phase 1 随第四个订阅开关 allow_cursor_subscription 引入）。admin 层
// 的整份替换契约与审计在 internal/admin/devtools_test.go，这里只钉仓储行为：
//
//   - 缺配置行是四开关全关的正常空策略，不是错误；Key 不存在才 ErrNotFound。
//   - cursor 开关全链落库：INSERT 与 ON CONFLICT 两条路都写得到、读得回，
//     库里那一列存的确实是这一位（0024 迁移的落点）。
//   - 相等判断把 cursor 算在内：只翻 cursor 也是一次变更；等值保存不写盘、
//     revision 不虚增。

import (
	"context"
	"errors"
	"testing"
)

func TestDevToolConfigCursorRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	key := mustKey(t, s, "digest-devtools-1")

	// Key 存在、无配置行：正常空策略，四开关全关、revision=0。
	cfg, err := s.GetDevToolConfig(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetDevToolConfig: %v", err)
	}
	if cfg.AllowCodexSubscription || cfg.AllowGrokSubscription ||
		cfg.AllowClaudeSubscription || cfg.AllowCursorSubscription {
		t.Errorf("空策略应四开关全关: %+v", cfg)
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

	// 首份配置（INSERT 路）：只勾 cursor 也是一次变更，revision 从 1 开始。
	saved, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, AllowCursorSubscription: true,
	})
	if err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
	if !changed || saved.Revision != 1 || !saved.AllowCursorSubscription {
		t.Fatalf("首份配置未按变更落库: changed=%v %+v", changed, saved)
	}

	// 落的确实是新列存的这一位。
	var raw int
	if err := s.db.QueryRow(`SELECT allow_cursor_subscription FROM api_key_devtool_configs WHERE key_id = ?`, key.ID).Scan(&raw); err != nil {
		t.Fatalf("读 allow_cursor_subscription 列: %v", err)
	}
	if raw != 1 {
		t.Errorf("allow_cursor_subscription 列 = %d, 期望 1", raw)
	}

	// 等值保存：不算变更、revision 不虚增。
	again, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, AllowCursorSubscription: true,
	})
	if err != nil {
		t.Fatalf("等值保存: %v", err)
	}
	if changed || again.Revision != 1 {
		t.Errorf("等值保存不该动 revision: changed=%v revision=%d", changed, again.Revision)
	}

	// 读回与保存一致（其余三开关不受牵连）。
	got, err := s.GetDevToolConfig(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowCursorSubscription || got.AllowCodexSubscription ||
		got.AllowGrokSubscription || got.AllowClaudeSubscription {
		t.Errorf("读回开关不符: %+v", got)
	}
	if got.Revision != 1 {
		t.Errorf("读回 revision = %d, 期望 1", got.Revision)
	}

	// ON CONFLICT 更新路：翻掉 cursor、勾上 claude 是一次变更；cursor 写回 0。
	next, changed, err := s.ReplaceDevToolConfig(ctx, DevToolConfig{
		KeyID: key.ID, AllowClaudeSubscription: true,
	})
	if err != nil {
		t.Fatalf("更新: %v", err)
	}
	if !changed || next.Revision != 2 || next.AllowCursorSubscription || !next.AllowClaudeSubscription {
		t.Fatalf("更新路未生效: changed=%v %+v", changed, next)
	}
	if err := s.db.QueryRow(`SELECT allow_cursor_subscription FROM api_key_devtool_configs WHERE key_id = ?`, key.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 0 {
		t.Errorf("翻掉后列值 = %d, 期望 0", raw)
	}
}
