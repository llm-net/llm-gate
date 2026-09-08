package store

import (
	"context"
	"errors"
	"time"
)

// MutateAgentAuth atomically merges a credential component into the latest row.
// The local-only callback may update Status, Label and DefaultModel; an empty
// result skips the write. On a concurrent write it runs again on the new row.
// Callers must compare only their own component and never log plaintext.
func (s *Store) MutateAgentAuth(ctx context.Context, id int64, provider string, refreshed bool, merge func(*AgentAccount, string) (string, error)) (*AgentAccount, error) {
	for attempt := 0; attempt < 8; attempt++ {
		a, previous, err := s.GetAgentCredential(ctx, provider)
		if err != nil {
			return nil, err
		}
		if a.ID != id {
			return nil, ErrNotFound
		}
		revision := fmtTime(a.UpdatedAt)
		next, err := merge(a, previous)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return a, nil
		}
		sealed, err := s.sealAgent(next, provider)
		if err != nil {
			return nil, err
		}
		now := fmtTime(time.Now())
		result, err := s.db.ExecContext(ctx, `UPDATE agent_accounts SET auth_json_sealed=?,status=?,label=?,default_model=?,last_refresh_at=CASE WHEN ? THEN ? ELSE last_refresh_at END,updated_at=? WHERE id=? AND provider=? AND auth_json_sealed=? AND updated_at=?`, sealed, a.Status, a.Label, a.DefaultModel, refreshed, now, now, id, provider, a.AuthJSONSealed, revision)
		if err != nil {
			return nil, errors.New("更新 Agents 凭据失败")
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, errors.New("读取 Agents 更新结果失败")
		}
		if count == 1 {
			return s.GetAgentAccount(ctx, id)
		}
	}
	return nil, errors.New("Agents 凭据正在更新，请重试")
}
