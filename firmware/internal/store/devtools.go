package store

// 单把 API 密钥的开发工具策略。策略本身只保存管理员选择：每种工具钉死的那一个
// 订阅账号行 id（0 = 未授权该工具的订阅），以及开发工具可见的目录模型集合。
// 订阅在线状态和模型协议兼容性由调用方按当前设备状态即时计算，不进入 revision。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

const MaxDevToolModels = 64

// ErrDevToolAccountInvalid 表示要钉的订阅账号不存在，或它的 provider 与工具不符
// （把一个 Grok 账号钉给 Codex）。管理面据此答 400。
var ErrDevToolAccountInvalid = errors.New("store: 订阅账号不存在或与工具不符")

// devToolAccountColumns 是四个钉死账号列，顺序与 AgentProviders 一致；
// DeleteAgentAccount 解开 Key 策略时按它逐列置空。
var devToolAccountColumns = []string{"codex_account_id", "grok_account_id", "claude_account_id", "cursor_account_id"}

type DevToolConfig struct {
	KeyID int64
	// 每种工具钉死的订阅账号行 id；0 = 这把 Key 未获授权使用该订阅。
	// 一把 Key 每种工具至多一个账号，切换账号即整份替换。
	CodexAccountID  int64
	GrokAccountID   int64
	ClaudeAccountID int64
	CursorAccountID int64
	Revision        int64
	UpdatedAt       time.Time
	CatalogModelIDs []int64
}

// SubscriptionAccount 取某种工具钉死的账号行 id（0 = 未授权）。
func (c DevToolConfig) SubscriptionAccount(provider string) int64 {
	switch provider {
	case AgentProviderCodex:
		return c.CodexAccountID
	case AgentProviderGrok:
		return c.GrokAccountID
	case AgentProviderClaude:
		return c.ClaudeAccountID
	case AgentProviderCursor:
		return c.CursorAccountID
	}
	return 0
}

// SetSubscriptionAccount 设某种工具钉死的账号行 id；未知 provider 忽略。
func (c *DevToolConfig) SetSubscriptionAccount(provider string, id int64) {
	switch provider {
	case AgentProviderCodex:
		c.CodexAccountID = id
	case AgentProviderGrok:
		c.GrokAccountID = id
	case AgentProviderClaude:
		c.ClaudeAccountID = id
	case AgentProviderCursor:
		c.CursorAccountID = id
	}
}

func (c DevToolConfig) sameSubscriptions(o DevToolConfig) bool {
	return c.CodexAccountID == o.CodexAccountID && c.GrokAccountID == o.GrokAccountID &&
		c.ClaudeAccountID == o.ClaudeAccountID && c.CursorAccountID == o.CursorAccountID
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readDevToolConfig 读一把 Key 的策略行与模型选择；没有配置行是正常的空策略。
// Key 是否存在由调用方先查。
func readDevToolConfig(ctx context.Context, q rowQuerier, keyID int64) (DevToolConfig, error) {
	cfg := DevToolConfig{KeyID: keyID, CatalogModelIDs: []int64{}}
	var codex, grok, claude, cursor sql.NullInt64
	var updated string
	err := q.QueryRowContext(ctx, `
		SELECT codex_account_id, grok_account_id, claude_account_id, cursor_account_id,
		       revision, updated_at
		  FROM api_key_devtool_configs WHERE key_id = ?`, keyID).
		Scan(&codex, &grok, &claude, &cursor, &cfg.Revision, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DevToolConfig{}, err
	}
	if err == nil {
		cfg.CodexAccountID = codex.Int64
		cfg.GrokAccountID = grok.Int64
		cfg.ClaudeAccountID = claude.Int64
		cfg.CursorAccountID = cursor.Int64
		cfg.UpdatedAt, _ = parseTime(updated)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT model_id FROM api_key_devtool_models WHERE key_id = ? ORDER BY model_id`, keyID)
	if err != nil {
		return DevToolConfig{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return DevToolConfig{}, err
		}
		cfg.CatalogModelIDs = append(cfg.CatalogModelIDs, id)
	}
	if err := rows.Err(); err != nil {
		return DevToolConfig{}, err
	}
	return cfg, nil
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
	cfg, err := readDevToolConfig(ctx, s.db, keyID)
	if err != nil {
		return DevToolConfig{}, fmt.Errorf("查询开发工具策略: %w", err)
	}
	return cfg, nil
}

// nullableID 把 0 写成 NULL（列上有外键，0 不是合法的账号 id）。
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ReplaceDevToolConfig 在一个事务里整份替换四个钉死账号和模型集合。每个非零账号
// 必须存在且 provider 与工具相符（ErrDevToolAccountInvalid）。等值保存不写盘、
// 不递增 revision；第一份非空或显式空配置从 revision=1 开始。
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
	for _, provider := range AgentProviders {
		if want.SubscriptionAccount(provider) < 0 {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", ErrDevToolAccountInvalid)
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
	// 钉死的账号必须在场且是同一种订阅：把别家的账号钉给这个工具，数据面会
	// 用错 AAD 解不开凭据，宁可在写入时就拒。
	for _, provider := range AgentProviders {
		id := want.SubscriptionAccount(provider)
		if id == 0 {
			continue
		}
		var got string
		err := tx.QueryRowContext(ctx, `SELECT provider FROM agent_accounts WHERE id = ?`, id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && got != provider) {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", ErrDevToolAccountInvalid)
		}
		if err != nil {
			return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
		}
	}

	current, err := readDevToolConfig(ctx, tx, want.KeyID)
	if err != nil {
		return DevToolConfig{}, false, fmt.Errorf("替换开发工具策略: %w", err)
	}
	if current.sameSubscriptions(want) && slices.Equal(current.CatalogModelIDs, ids) {
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
		       (key_id, codex_account_id, grok_account_id, claude_account_id, cursor_account_id,
		        revision, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET
		       codex_account_id = excluded.codex_account_id,
		       grok_account_id = excluded.grok_account_id,
		       claude_account_id = excluded.claude_account_id,
		       cursor_account_id = excluded.cursor_account_id,
		       revision = excluded.revision,
		       updated_at = excluded.updated_at`,
		want.KeyID, nullableID(want.CodexAccountID), nullableID(want.GrokAccountID),
		nullableID(want.ClaudeAccountID), nullableID(want.CursorAccountID), revision, ts); err != nil {
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
		KeyID: want.KeyID, CodexAccountID: want.CodexAccountID, GrokAccountID: want.GrokAccountID,
		ClaudeAccountID: want.ClaudeAccountID, CursorAccountID: want.CursorAccountID,
		Revision: revision, UpdatedAt: when, CatalogModelIDs: ids,
	}, true, nil
}
