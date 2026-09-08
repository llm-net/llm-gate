package agentquota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type fakeAccounts struct {
	mu      sync.Mutex
	a       store.AgentAccount
	deleted bool
}

func (f *fakeAccounts) GetAgentAccount(context.Context, int64) (*store.AgentAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted {
		return nil, store.ErrNotFound
	}
	a := f.a
	return &a, nil
}
func (f *fakeAccounts) ListAgentAccounts(ctx context.Context) ([]store.AgentAccount, error) {
	a, e := f.GetAgentAccount(ctx, 1)
	if e != nil {
		return nil, e
	}
	return []store.AgentAccount{*a}, nil
}
func (f *fakeAccounts) change(fn func(*store.AgentAccount)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.a)
}
func quotaData() Data {
	v := int64(1250)
	return Data{Source: "api", Windows: []Window{{ID: "week", Period: "week", UsedBPS: &v}}}
}
func managerEnv(fetch Fetcher) (*Manager, *fakeAccounts, *store.AgentAccount) {
	a := store.AgentAccount{ID: 1, Provider: "codex", Status: "active", UpdatedAt: time.Unix(1700000000, 0)}
	f := &fakeAccounts{a: a}
	return NewManager(f, fetch), f, &a
}

func TestSyncIntervalsFailuresAndRateLimit(t *testing.T) {
	var calls int
	var failure error
	m, _, a := managerEnv(func(context.Context, *store.AgentAccount) (Data, error) { calls++; return quotaData(), failure })
	now := a.UpdatedAt
	m.now = func() time.Time { return now }
	s := m.Sync(context.Background(), a, false)
	if calls != 1 || s.Status != "ok" || s.Stale {
		t.Fatalf("initial %+v", s)
	}
	m.Sync(context.Background(), a, false)
	m.Sync(context.Background(), a, true)
	if calls != 1 {
		t.Fatal("cooldown bypass")
	}
	now = now.Add(Interval)
	failure = &Error{Code: "rate_limited", RetryAfter: time.Hour}
	s = m.Sync(context.Background(), a, false)
	if s.Status != "partial" || s.LastSuccessAt == nil || len(s.Windows) != 1 || s.NextSyncAt.Sub(now) != time.Hour {
		t.Fatalf("failed read lost previous sample: %+v", s)
	}
	now = now.Add(Interval)
	s = m.Sync(context.Background(), a, true)
	if calls != 2 {
		t.Fatal("manual sync bypassed Retry-After")
	}
	now = now.Add(time.Second)
	if !m.Read(a).Stale {
		t.Fatal("old sample not marked stale")
	}
	now = now.Add(time.Hour)
	failure = errors.New("fake-secret-arbitrary-error")
	s = m.Sync(context.Background(), a, false)
	if s.ErrorCode != "unavailable" {
		t.Fatal("raw error escaped")
	}
}

func TestConcurrentSyncAndReconnect(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m, f, a := managerEnv(func(ctx context.Context, _ *store.AgentAccount) (Data, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return Data{}, ctx.Err()
			}
		}
		return quotaData(), nil
	})
	done := make(chan Snapshot, 2)
	go func() { done <- m.Sync(context.Background(), a, true) }()
	<-started
	go func() { done <- m.Sync(context.Background(), a, true) }()
	f.change(func(a *store.AgentAccount) {
		a.UpdatedAt = a.UpdatedAt.Add(time.Second)
		a.AccountID = "different-account"
	})
	current, _ := f.GetAgentAccount(context.Background(), 1)
	if s := m.Read(current); len(s.Windows) != 0 {
		t.Fatal("previous identity exposed")
	}
	close(release)
	<-done
	<-done
	if n := calls.Load(); n > 2 {
		t.Fatalf("duplicate queries %d", n)
	}
	if m.Read(current).Status != "ok" {
		t.Fatal("new revision did not synchronize")
	}
	f.change(func(a *store.AgentAccount) { a.Status = "disabled" })
	current, _ = f.GetAgentAccount(context.Background(), 1)
	if s := m.Read(current); s.Status != "paused" || len(s.Windows) != 0 {
		t.Fatal("disabled quota exposed")
	}
}

func TestHeadersPermissionFailureAndReset(t *testing.T) {
	m, _, a := managerEnv(func(context.Context, *store.AgentAccount) (Data, error) {
		return Data{}, &Error{Code: "permission_required"}
	})
	now := a.UpdatedAt
	m.now = func() time.Time { return now }
	m.Sync(context.Background(), a, false)
	d := quotaData()
	d.Source = "response_headers"
	reset := now.Add(time.Hour)
	d.Windows[0].ResetsAt = &reset
	m.Observe(a, d)
	s := m.Read(a)
	if s.Status != "partial" || s.Stale || s.ErrorCode != "permission_required" {
		t.Fatalf("lost header fallback: %+v", s)
	}
	now = reset
	if !m.Read(a).Stale {
		t.Fatal("expired window is current")
	}
	newAccount := *a
	newAccount.UpdatedAt = now
	m.Observe(&newAccount, quotaData())
	m.Observe(a, d)
	if s := m.Read(&newAccount); s.Source != "api" {
		t.Fatal("late old account observation replaced new sample")
	}
}

func TestRunStartupAndCancellation(t *testing.T) {
	started := make(chan struct{})
	m, _, _ := managerEnv(func(ctx context.Context, _ *store.AgentAccount) (Data, error) {
		close(started)
		<-ctx.Done()
		return Data{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestPublishedAttemptTimestampIsImmutable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	m, _, a := managerEnv(func(context.Context, *store.AgentAccount) (Data, error) {
		close(started)
		<-release
		return quotaData(), nil
	})
	var seconds atomic.Int64
	seconds.Store(a.UpdatedAt.Unix())
	m.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	done := make(chan struct{})
	go func() { m.Sync(context.Background(), a, false); close(done) }()
	<-started
	published := m.Read(a)
	want := *published.LastAttemptAt
	seconds.Add(20)
	close(release)
	<-done
	if !published.LastAttemptAt.Equal(want) {
		t.Fatal("a previously returned snapshot was mutated on completion")
	}
	if m.Read(a).LastSuccessAt.Equal(want) {
		t.Fatal("completion did not receive its own timestamp")
	}
}
