package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestCursorCacheBillingAndPersistence(t *testing.T) {
	at := time.Now()
	m, st := newTestMeter(t, time.UTC, at)
	s := textSample(at)
	s.Entry, s.ModelName = EntryCursorAgent, "cursor-test-model"
	s.Pricing = `{"in":2000000,"out":8000000,"cache_read":200000,"cache_write":2500000}`
	s.Tokens = Tokens{Prompt: 1000, Completion: 20, CacheRead: 700, CacheWrite: 100}
	// 200*2 + 20*8 + 700*0.2 + 100*2.5 = 950 micro-yuan.
	m.Record(s)
	s.UsageUnavailable = true
	m.Record(s)
	if spend := m.Spend(s.KeyID); spend.DayMicro != 950 {
		t.Fatalf("spend %+v", spend)
	}
	check := func() {
		r, err := m.Report(t.Context(), at.Add(-time.Hour), at.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if r.Total.Requests != 2 || r.Total.CostMicro != 950 || r.Total.UnavailableRequests != 1 || r.Total.EstimatedRequests != 0 || r.Total.TotalTokens != 2040 {
			t.Fatalf("report %+v", r.Total)
		}
		if len(r.ByModel) != 1 || r.ByModel[0].Key != s.ModelName || len(r.ByEntry) != 1 || r.ByEntry[0].Key != SubscriptionTrafficDimension {
			t.Fatal("lost model/subscription dimensions")
		}
		if len(r.Recent) != 2 || !r.Recent[0].UsageUnavailable || r.Recent[1].CacheWriteTokens != 100 || r.Recent[1].CacheReadTokens != 700 {
			t.Fatal("recent usage breakdown missing")
		}
	}
	check()
	m.flush(t.Context())
	check()
	rows, err := st.QueryUsageRange(t.Context(), at.Unix()/3600, at.Unix()/3600+1)
	if err != nil || len(rows) != 1 || rows[0].UnavailableRequests != 1 || rows[0].CostMicro != 950 {
		t.Fatalf("persisted %+v %v", rows, err)
	}
	measure := Measure{Kind: store.ModelKindText, Entry: EntryCursorAgent, Tokens: s.Tokens}
	if got := Cost(mustPricing(t, `{"in":2000000,"out":8000000}`), measure); got != 2160 {
		t.Fatalf("cache fallback=%d", got)
	}
	if got := Cost(mustPricing(t, `{"in":2000000,"out":8000000,"cache_read":0,"cache_write":0}`), measure); got != 560 {
		t.Fatalf("explicit free cache=%d", got)
	}
}

func TestCursorPriceValidation(t *testing.T) {
	for _, raw := range []string{`{"in":0,"out":0}`, `{"in":1000000,"out":2000000,"cache_write":3000000}`} {
		if _, err := ParseCursorPrice(raw); err != nil {
			t.Fatalf("valid price: %v", err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"fake-secret":1}`, `{"in":1}`, `{"in":null,"out":0}`, `{"in":1.5,"out":0}`, `{"in":1e3,"out":0}`, `{"in":-1,"out":0}`, `{"in":1000000000001,"out":0}`, `{"in":0,"out":0,"image":1}`} {
		if _, err := ParseCursorPrice(raw); err == nil {
			t.Fatalf("invalid price accepted: %s", raw)
		} else if strings.Contains(err.Error(), "fake-secret") {
			t.Fatal("error echoed input")
		}
	}
}
