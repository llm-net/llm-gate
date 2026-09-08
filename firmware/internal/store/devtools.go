package store

// 单把 API 密钥的开发工具策略。策略本身只保存管理员选择；订阅在线状态和模型
// 协议兼容性由调用方按当前设备状态即时计算，不进入 revision。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

const MaxDevToolModels = 64

type DevToolConfig struct {
	KeyID                   int64
	AllowCodexSubscription  bool
	AllowGrokSubscription   bool
	AllowClaudeSubscription bool
	AllowCursorSubscription bool
	Revision                int64
	UpdatedAt               time.Time
	CatalogModelIDs         []int64
}

// GetDevToolConfig 返回一把存在的 Key 的策略。没有配置行是正常的空策略；Key
// 本身不存在才返回 ErrNotFound。
func (s *Store) GetDevToolConfig(ctx context.Context, keyID int64) (DevToolConfig, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, keyID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DevToolConfig{}, fmt.Errorf("查询开发工具策略: %w", ErrNotFound)
		}
		return DevToolConfig{}, fmt.Errorf("查询开发工具策略: %w", err)
	}
	cfg := DevToolConfig{KeyID: keyID, CatalogModelIDs: []int64{}}
	var codex, grok, claude, cursor int
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT allow_codex_subscription, allow_grok_subscription,
		       allow_claude_subscription, allow_cursor_subscription, revision, updated_at
		  FROM api_key_devtool_configs WHERE key_id = ?`, keyID).
		Scan(&codex, &grok, &claude, &cursor, &cfg.Revision, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DevToolConfig{}, fmt.Errorf("查询开发工具策略: %w", err)
	}
	if err == nil {
		cfg.AllowCodexSubscription = codex != 0
		cfg.AllowGrokSubscription = grok != 0
		cfg.AllowClaudeSubscription = claude != 0
		cfg.AllowCursorSubscription = cursor != 0
		cfg.UpdatedAt, _ = parseTime(updated)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id FROM api_key_devtool_models WHERE key_id = ? ORDER BY model_id`, keyID)
	if err != nil {
		return DevToolConfig{}, fmt.Errorf("查询开发工具模型选择: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return DevToolConfig{}, fmt.Errorf("查询开发工具模型选择: %w", err)
		}
		cfg.CatalogModelIDs = append(cfg.CatalogModelIDs, id)
	}
	if err := rows.Err(); err != nil {
		return DevToolConfig{}, fmt.Errorf("查询开发工具模型选择: %w", err)
	}
	return cfg, nil
}

// ReplaceDevToolConfig 在一个事务里整份替换四项订阅开关和模型集合。等值保存
// 不写盘、不递增 revision；第一份非空或显式空配置从 revision=1 开始。
func (s *Store) ReplaceDevToolConfig(ctx context.Context, want DevToolConfig) (DevToolConfig, bool, error) {
	ids := append([]int64(nil), want.CatalogModelIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > MaxDevToolModels {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: 模型数超过上限 %d", MaxDevToolModels)
	}
	for _, id := range ids {
		if id <= 0 {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: 模型 id 必须为正整数")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, want.KeyID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", ErrNotFound)
		}
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}

	current := DevToolConfig{KeyID: want.KeyID, CatalogModelIDs: []int64{}}
	var codex, grok, claude, cursor int
	var updated string
	err = tx.QueryRowContext(ctx, `
		SELECT allow_codex_subscription, allow_grok_subscription,
		       allow_claude_subscription, allow_cursor_subscription, revision, updated_at
		  FROM api_key_devtool_configs WHERE key_id = ?`, want.KeyID).
		Scan(&codex, &grok, &claude, &cursor, &current.Revision, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}
	if err == nil {
		current.AllowCodexSubscription = codex != 0
		current.AllowGrokSubscription = grok != 0
		current.AllowClaudeSubscription = claude != 0
		current.AllowCursorSubscription = cursor != 0
		current.UpdatedAt, _ = parseTime(updated)
	}
	rows, qerr := tx.QueryContext(ctx, `SELECT model_id FROM api_key_devtool_models WHERE key_id = ? ORDER BY model_id`, want.KeyID)
	if qerr != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", qerr)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
		}
		current.CatalogModelIDs = append(current.CatalogModelIDs, id)
	}
	if err := rows.Close(); err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}

	if current.AllowCodexSubscription == want.AllowCodexSubscription &&
		current.AllowGrokSubscription == want.AllowGrokSubscription &&
		current.AllowClaudeSubscription == want.AllowClaudeSubscription &&
		current.AllowCursorSubscription == want.AllowCursorSubscription &&
		slices.Equal(current.CatalogModelIDs, ids) {
		if err := tx.Commit(); err != nil {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
		}
		return current, false, nil
	}

	revision := current.Revision + 1
	if revision == 0 {
		revision = 1
	}
	ts := fmtTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_key_devtool_configs
		       (key_id, allow_codex_subscription, allow_grok_subscription,
		        allow_claude_subscription, allow_cursor_subscription, revision, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET
		       allow_codex_subscription = excluded.allow_codex_subscription,
		       allow_grok_subscription = excluded.allow_grok_subscription,
		       allow_claude_subscription = excluded.allow_claude_subscription,
		       allow_cursor_subscription = excluded.allow_cursor_subscription,
		       revision = excluded.revision,
		       updated_at = excluded.updated_at`,
		want.KeyID, boolToInt(want.AllowCodexSubscription), boolToInt(want.AllowGrokSubscription),
		boolToInt(want.AllowClaudeSubscription), boolToInt(want.AllowCursorSubscription), revision, ts); err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_devtool_models WHERE key_id = ?`, want.KeyID); err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}
	for _, modelID := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_devtool_models (key_id, model_id) VALUES (?, ?)`, want.KeyID, modelID); err != nil {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}
	when, _ := parseTime(ts)
	return DevToolConfig{
		KeyID: want.KeyID, AllowCodexSubscription: want.AllowCodexSubscription,
		AllowGrokSubscription: want.AllowGrokSubscription, AllowClaudeSubscription: want.AllowClaudeSubscription,
		AllowCursorSubscription: want.AllowCursorSubscription,
		Revision:                revision, UpdatedAt: when, CatalogModelIDs: ids,
	}, true, nil
}
