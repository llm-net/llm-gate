package agentquota

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type Accounts interface {
	ListAgentAccounts(context.Context) ([]store.AgentAccount, error)
	GetAgentAccount(context.Context, int64) (*store.AgentAccount, error)
}
type Fetcher func(context.Context, *store.AgentAccount) (Data, error)

type Snapshot struct {
	Data
	Status        string     `json:"status"`
	ErrorCode     string     `json:"error_code,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	NextSyncAt    *time.Time `json:"next_sync_at"`
	Stale         bool       `json:"stale"`
}
type entry struct {
	revision time.Time
	snapshot Snapshot
	failures int
	done     chan struct{}
}

// Manager caches only normalized readings in RAM; restart triggers a fresh read.
// Comparing the account revision on both publish and read prevents a reconnect,
// deletion or disable from exposing a previous account's allowance.
type Manager struct {
	mu       sync.Mutex
	accounts Accounts
	fetch    Fetcher
	entries  map[int64]*entry
	now      func() time.Time
}

func NewManager(accounts Accounts, fetch Fetcher) *Manager {
	return &Manager{accounts: accounts, fetch: fetch, entries: map[int64]*entry{}, now: time.Now}
}
func pending(a *store.AgentAccount) Snapshot {
	s := "pending"
	if !SyncEnabled(a) {
		s = "paused"
	}
	return Snapshot{Data: Data{Windows: []Window{}}, Status: s, Stale: true}
}
func (m *Manager) Read(a *store.AgentAccount) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[a.ID]
	if !SyncEnabled(a) || e == nil || !e.revision.Equal(a.UpdatedAt) {
		return pending(a)
	}
	s := e.snapshot
	s.Windows = append([]Window{}, s.Windows...)
	s.Stale = s.LastSuccessAt == nil || m.now().Sub(*s.LastSuccessAt) > 2*Interval
	for _, w := range s.Windows {
		if w.ResetsAt != nil && !w.ResetsAt.After(m.now()) {
			s.Stale = true
		}
	}
	return s
}

// Sync coalesces concurrent callers. Manual sync respects Retry-After and a
// short cooldown; regular sync respects the full next-sync deadline.
func (m *Manager) Sync(ctx context.Context, a *store.AgentAccount, manual bool) Snapshot {
	for attempt := 0; attempt < 2; attempt++ {
		currentAccount, readErr := m.accounts.GetAgentAccount(ctx, a.ID)
		if readErr != nil {
			return pending(a)
		}
		a = currentAccount
		if ctx.Err() != nil || !SyncEnabled(a) {
			return m.Read(a)
		}
		m.mu.Lock()
		e := m.entries[a.ID]
		if e == nil || !e.revision.Equal(a.UpdatedAt) {
			e = &entry{revision: a.UpdatedAt, snapshot: pending(a)}
			m.entries[a.ID] = e
		}
		if e.done != nil {
			done := e.done
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return m.Read(a)
			case <-done:
				return m.Read(a)
			}
		}
		now := m.now()
		wait := e.snapshot.NextSyncAt != nil && now.Before(*e.snapshot.NextSyncAt)
		if manual && e.snapshot.ErrorCode != "rate_limited" {
			wait = e.snapshot.LastAttemptAt != nil && now.Sub(*e.snapshot.LastAttemptAt) < 30*time.Second
		}
		if wait {
			m.mu.Unlock()
			return m.Read(a)
		}
		e.done = make(chan struct{})
		attemptAt := now
		e.snapshot.LastAttemptAt = &attemptAt
		m.mu.Unlock()
		fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		data, err := m.fetch(fetchCtx, a)
		cancel()
		current, readErr := m.accounts.GetAgentAccount(ctx, a.ID)
		m.mu.Lock()
		changed := readErr != nil || !SyncEnabled(current) || !current.UpdatedAt.Equal(a.UpdatedAt)
		if !changed && m.entries[a.ID] == e && ctx.Err() == nil {
			now = m.now()
			delay := Interval
			if err == nil {
				e.snapshot.Data = data
				e.snapshot.LastSuccessAt = &now
				e.snapshot.Status = "ok"
				e.snapshot.ErrorCode = ""
				e.failures = 0
			} else {
				e.failures++
				e.snapshot.ErrorCode = codeOf(err)
				e.snapshot.Status = "error"
				if len(e.snapshot.Windows) > 0 {
					e.snapshot.Status = "partial"
				}
				delay = Interval * time.Duration(1<<min(e.failures-1, 3))
				if delay > 30*time.Minute {
					delay = 30 * time.Minute
				}
				var qe *Error
				if errors.As(err, &qe) && qe.RetryAfter > delay {
					delay = qe.RetryAfter
				}
				if delay > 24*time.Hour {
					delay = 24 * time.Hour
				}
			}
			next := now.Add(delay)
			e.snapshot.NextSyncAt = &next
		}
		close(e.done)
		e.done = nil
		m.mu.Unlock()
		if changed && readErr == nil {
			a = current
			continue
		}
		if readErr != nil {
			return pending(a)
		}
		return m.Read(a)
	}
	return m.Read(a)
}

// Observe is used by the Claude data path. It accepts only pre-parsed quota
// headers and never triggers traffic or writes to disk.
func (m *Manager) Observe(a *store.AgentAccount, d Data) {
	if !SyncEnabled(a) || len(d.Windows) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[a.ID]
	if e != nil && e.revision.After(a.UpdatedAt) {
		return
	}
	if e == nil || !e.revision.Equal(a.UpdatedAt) {
		e = &entry{revision: a.UpdatedAt, snapshot: pending(a)}
		m.entries[a.ID] = e
	}
	now := m.now()
	e.snapshot.Data = d
	e.snapshot.LastSuccessAt = &now
	e.snapshot.Status = "partial"
}

func (m *Manager) Run(ctx context.Context) {
	// Local scans discover newly connected accounts within 30 seconds. Only due
	// accounts cause upstream requests; each provider runs independently.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		accts, err := m.accounts.ListAgentAccounts(ctx)
		if err == nil {
			seen := map[int64]bool{}
			var wg sync.WaitGroup
			for i := range accts {
				a := accts[i]
				seen[a.ID] = true
				if !SyncEnabled(&a) {
					continue
				}
				wg.Add(1)
				go func() { defer wg.Done(); m.Sync(ctx, &a, false) }()
			}
			wg.Wait()
			m.mu.Lock()
			for id := range m.entries {
				if !seen[id] {
					delete(m.entries, id)
				}
			}
			m.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Claude's account auth_expired state describes setup-token only. Quota OAuth
// remains usable independently; explicit disable pauses every provider.
func SyncEnabled(a *store.AgentAccount) bool {
	return a.Status == store.AgentStatusActive || a.Provider == store.AgentProviderClaude && a.Status == store.AgentStatusAuthExpired
}
