package store

// 单把 API 密钥经 API 调用可用的模型范围（API模型策略）。策略只保存管理员的
// 选择：模型此刻能不能调通（停用、来源掉线）由调用方按当前设备状态即时判断，
// 不进入 revision。
//
// 两态语义：Restricted=false（缺配置行的缺省）= 不限制，该 Key 能调目录里
// 全部 API密钥接入 模型；Restricted=true = 只能调 ModelIDs 里的那些，空集合
// 因此是「一个 API 模型都不能调」的合法表达。订阅接入模型不在这套策略里——
// 它们本来就没有可路由的来源，走不通 API 入口。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// MaxKeyAPIModels 是单把 Key 可选模型数的上限。目录本身可以很长（一个平台
// 几十个模型），所以比开发工具那份（MaxDevToolModels）宽得多——它拦的是灌入，
// 不是正常配置。
const MaxKeyAPIModels = 512

type KeyAPIModelConfig struct {
	KeyID      int64
	Restricted bool
	Revision   int64
	UpdatedAt  time.Time
	ModelIDs   []int64
}

// KeyAPIModelScope 是数据面判定要的最小读数：不限制时连模型集合都不查。
type KeyAPIModelScope struct {
	Restricted bool
	allowed    map[int64]bool
}

// Allows 判定某个目录模型是否在本 Key 的 API 调用范围内。
func (s KeyAPIModelScope) Allows(modelID int64) bool {
	return !s.Restricted || s.allowed[modelID]
}

// GetKeyAPIModelConfig 返回一把存在的 Key 的策略。没有配置行是正常的「不限制」
// 策略；Key 本身不存在才返回 ErrNotFound（读写两侧同一口径）。
func (s *Store) GetKeyAPIModelConfig(ctx context.Context, keyID int64) (KeyAPIModelConfig, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, keyID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KeyAPIModelConfig{}, fmt.Errorf("查询API模型策略: %w", ErrNotFound)
		}
		return KeyAPIModelConfig{}, fmt.Errorf("查询API模型策略: %w", err)
	}
	cfg := KeyAPIModelConfig{KeyID: keyID, ModelIDs: []int64{}}
	var restricted int
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT restricted, revision, updated_at
		  FROM api_key_api_model_configs WHERE key_id = ?`, keyID).
		Scan(&restricted, &cfg.Revision, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KeyAPIModelConfig{}, fmt.Errorf("查询API模型策略: %w", err)
	}
	if err == nil {
		cfg.Restricted = restricted != 0
		cfg.UpdatedAt, _ = parseTime(updated)
	}
	ids, err := s.keyAPIModelIDs(ctx, keyID)
	if err != nil {
		return KeyAPIModelConfig{}, err
	}
	cfg.ModelIDs = ids
	return cfg, nil
}

// LookupKeyAPIModelScope 是数据面每请求的点查（无缓存层，与鉴权同一决策：
// 改策略下一个请求即时生效）。不限制时只查一行配置。
func (s *Store) LookupKeyAPIModelScope(ctx context.Context, keyID int64) (KeyAPIModelScope, error) {
	var restricted int
	err := s.db.QueryRowContext(ctx,
		`SELECT restricted FROM api_key_api_model_configs WHERE key_id = ?`, keyID).Scan(&restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyAPIModelScope{}, nil
	}
	if err != nil {
		return KeyAPIModelScope{}, fmt.Errorf("查询API模型策略: %w", err)
	}
	if restricted == 0 {
		return KeyAPIModelScope{}, nil
	}
	ids, err := s.keyAPIModelIDs(ctx, keyID)
	if err != nil {
		return KeyAPIModelScope{}, err
	}
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	return KeyAPIModelScope{Restricted: true, allowed: allowed}, nil
}

func (s *Store) keyAPIModelIDs(ctx context.Context, keyID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_id FROM api_key_api_models WHERE key_id = ? ORDER BY model_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("查询API模型选择: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("查询API模型选择: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("查询API模型选择: %w", err)
	}
	return out, nil
}

// ReplaceKeyAPIModelConfig 在一个事务里整份替换开关与模型集合。等值保存不写盘、
// 不递增 revision；第一份非缺省配置从 revision=1 开始。
func (s *Store) ReplaceKeyAPIModelConfig(ctx context.Context, want KeyAPIModelConfig) (KeyAPIModelConfig, bool, error) {
	ids := append([]int64(nil), want.ModelIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > MaxKeyAPIModels {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: 模型数超过上限 %d", MaxKeyAPIModels)
	}
	for _, id := range ids {
		if id <= 0 {
			return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: 模型 id 必须为正整数")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, want.KeyID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", ErrNotFound)
		}
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}

	current := KeyAPIModelConfig{KeyID: want.KeyID, ModelIDs: []int64{}}
	var restricted int
	var updated string
	err = tx.QueryRowContext(ctx, `
		SELECT restricted, revision, updated_at
		  FROM api_key_api_model_configs WHERE key_id = ?`, want.KeyID).
		Scan(&restricted, &current.Revision, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}
	if err == nil {
		current.Restricted = restricted != 0
		current.UpdatedAt, _ = parseTime(updated)
	}
	rows, qerr := tx.QueryContext(ctx, `SELECT model_id FROM api_key_api_models WHERE key_id = ? ORDER BY model_id`, want.KeyID)
	if qerr != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", qerr)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
		}
		current.ModelIDs = append(current.ModelIDs, id)
	}
	if err := rows.Close(); err != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}

	if current.Restricted == want.Restricted && slices.Equal(current.ModelIDs, ids) {
		if err := tx.Commit(); err != nil {
			return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
		}
		return current, false, nil
	}

	revision := current.Revision + 1
	if revision == 0 {
		revision = 1
	}
	ts := fmtTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_key_api_model_configs (key_id, restricted, revision, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET
		       restricted = excluded.restricted,
		       revision = excluded.revision,
		       updated_at = excluded.updated_at`,
		want.KeyID, boolToInt(want.Restricted), revision, ts); err != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_api_models WHERE key_id = ?`, want.KeyID); err != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}
	for _, modelID := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_api_models (key_id, model_id) VALUES (?, ?)`, want.KeyID, modelID); err != nil {
			return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return KeyAPIModelConfig{}, false, fmt.Errorf("替换API模型策略: %w", err)
	}
	when, _ := parseTime(ts)
	return KeyAPIModelConfig{
		KeyID: want.KeyID, Restricted: want.Restricted,
		Revision: revision, UpdatedAt: when, ModelIDs: ids,
	}, true, nil
}
