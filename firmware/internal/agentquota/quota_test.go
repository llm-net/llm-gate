package agentquota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderWindows(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		periods              []string
		used                 []int64
	}{
		{"codex weekly primary", "codex", `{"rate_limit":{"primary_window":{"used_percent":36,"limit_window_seconds":604800,"reset_at":1789352877}},"additional_rate_limits":[{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1788847975},"secondary_window":{"used_percent":17,"limit_window_seconds":604800}}}]}`, []string{"week", "session", "week"}, []int64{3600, 0, 1700}},
		{"claude", "claude", `{"five_hour":{"utilization":23.45,"resets_at":"2026-09-08T12:00:00Z"},"seven_day":{"utilization":90,"resets_at":null},"seven_day_opus":null,"extra_usage":{"is_enabled":true,"utilization":12.5}}`, []string{"session", "week", "month"}, []int64{2345, 9000, 1250}},
		{"cursor separate pools", "cursor", `{"billingCycleEnd":"1789573713000","planUsage":{"totalSpend":26999,"limit":40000,"autoPercentUsed":0.18966666666666668,"apiPercentUsed":52.86,"totalPercentUsed":7.714}}`, []string{"month", "month", "month"}, []int64{18, 5286, 771}},
		{"grok unified weekly", "grok", `{"config":{"creditUsagePercent":15,"isUnifiedBillingUser":true,"currentPeriod":{"start":"2026-09-06T15:33:03.703867+00:00","end":"2026-09-13T15:33:03.703867+00:00"}}}`, []string{"week"}, []int64{1500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse(tc.provider, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Windows) != len(tc.used) {
				t.Fatalf("window count %d", len(d.Windows))
			}
			for i, w := range d.Windows {
				if w.Period != tc.periods[i] || w.UsedBPS == nil || *w.UsedBPS != tc.used[i] {
					t.Fatalf("window %d: %+v", i, w)
				}
			}
		})
	}
}

func TestUnknownIsNotZero(t *testing.T) {
	for _, p := range []string{"codex", "claude", "cursor", "grok"} {
		for _, body := range []string{`{}`, `null`, `{"error":"fake-secret-body"}`, `{"five_hour":null}`, `{"planUsage":{}}`} {
			if _, err := Parse(p, []byte(body)); err == nil {
				t.Fatalf("%s accepted unknown %s", p, body)
			}
		}
	}
	for _, n := range []json.Number{"", "-1", "1e99999999", "1/2", "1001", "NaN", "0x10"} {
		if percent(n) != nil {
			t.Fatalf("invalid percent accepted: %s", n)
		}
	}
	if p := percent("0"); p == nil || *p != 0 {
		t.Fatal("explicit zero lost")
	}
}

func TestClaudeScopedWeeklyLimits(t *testing.T) {
	d, err := Parse("claude", []byte(`{
		"five_hour":{"utilization":12.34},
		"seven_day":{"utilization":42},
		"seven_day_sonnet":{"utilization":40},
		"limits":[
			{"kind":"session","percent":12.34},
			{"kind":"weekly_all","percent":42},
			{"kind":"weekly_scoped","group":"weekly","percent":6.25,"resets_at":"2026-09-14T14:59:59.825334+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false},
			{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Sonnet"}}},
			{"kind":"weekly_scoped","percent":99,"scope":{"model":{"display_name":"Fable"}}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Windows) != 4 {
		t.Fatalf("main and model windows must not be duplicated: %+v", d.Windows)
	}
	want := map[string]int64{"five_hour": 1234, "seven_day": 4200, "seven_day_sonnet": 0, "seven_day_fable": 625}
	for _, w := range d.Windows {
		used, ok := want[w.ID]
		if !ok || w.UsedBPS == nil || *w.UsedBPS != used {
			t.Fatalf("wrong window: %+v", w)
		}
		delete(want, w.ID)
		if w.ID == "seven_day_fable" && (w.Label != "Fable 额度" || w.Period != "week" || w.WindowSeconds != 604800 || w.ResetsAt == nil || w.ResetsAt.Format(time.RFC3339Nano) != "2026-09-14T14:59:59.825334Z") {
			t.Fatalf("Fable scope or reset lost: %+v", w)
		}
	}
	if len(want) != 0 {
		t.Fatal("missing quota windows")
	}
}

func TestClaudeScopedLimitsRejectUnknownAndMalformedData(t *testing.T) {
	for _, item := range []string{
		`null`,
		`{"kind":"weekly_scoped","percent":0,"scope":{"model":null}}`,
		`{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"fake-private-marker"}}}`,
		`{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Fable"},"surface":{"id":"fake-private-marker"}}}`,
		`{"kind":"unknown","percent":0,"scope":{"model":{"display_name":"Fable"}}}`,
		`{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}}}`,
		`{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}`,
		`{"kind":"weekly_scoped","percent":-1,"scope":{"model":{"display_name":"Fable"}}}`,
		`{"kind":"weekly_scoped","percent":1001,"scope":{"model":{"display_name":"Fable"}}}`,
		`{"kind":"weekly_scoped","percent":1e999,"scope":{"model":{"display_name":"Fable"}}}`,
	} {
		// One bad optional scope must not hide a valid main subscription window.
		d, err := Parse("claude", []byte(`{"seven_day":{"utilization":0},"limits":[`+item+`],"private":"fake-private-marker"}`))
		if err != nil || len(d.Windows) != 1 || d.Windows[0].ID != "seven_day" || *d.Windows[0].UsedBPS != 0 {
			t.Fatalf("invalid scope changed the main quota: %v", err)
		}
		normalized, _ := json.Marshal(d)
		if strings.Contains(string(normalized), "fake-private-marker") {
			t.Fatal("opaque upstream data exposed")
		}
		if _, err := Parse("claude", []byte(`{"limits":[`+item+`]}`)); err == nil {
			t.Fatal("missing scoped quota was treated as zero")
		}
	}
	// A valid model-only response is useful even without all-model windows.
	d, err := Parse("claude", []byte(`{"limits":[{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Fable"}}}]}`))
	if err != nil || len(d.Windows) != 1 || *d.Windows[0].UsedBPS != 0 {
		t.Fatal("explicit scoped zero lost")
	}
}

func TestClaudeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.2345")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1788868800")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0")
	h.Set("Authorization", "fake-secret")
	d := ClaudeHeaders(h)
	if len(d.Windows) != 2 || *d.Windows[0].UsedBPS != 2345 || *d.Windows[1].UsedBPS != 0 {
		t.Fatalf("wrong windows: %+v", d)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "fake-secret") {
		t.Fatal("header leaked")
	}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "1e99999999")
	if len(ClaudeHeaders(h).Windows) != 1 {
		t.Fatal("malformed header accepted")
	}
}

func TestClientSafeHTTP(t *testing.T) {
	const secret = "fake-quota-access-token"
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing auth")
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/leak")
			w.WriteHeader(302)
			return
		}
		if r.URL.Path == "/forbidden" {
			w.WriteHeader(403)
			io.WriteString(w, `{"message":"`+secret+`"}`)
			return
		}
		if r.URL.Path == "/rate" {
			w.Header().Set("Retry-After", "900")
			w.WriteHeader(429)
			return
		}
		if r.URL.Path == "/large" {
			io.WriteString(w, strings.Repeat("x", (1<<20)+1))
			return
		}
		if r.Method != "POST" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Error("not Cursor JSON RPC")
		}
		io.WriteString(w, `{"planUsage":{"apiPercentUsed":0},"billingCycleEnd":"1789573713000"}`)
	}))
	defer srv.Close()
	c := NewClient(nil)
	for _, path := range []string{"/redirect", "/forbidden", "/rate", "/large"} {
		c.endpoints["cursor"] = srv.URL + path
		before := requests
		_, err := c.Fetch(context.Background(), "cursor", secret, "")
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("unsafe or missing error")
		}
		if requests != before+1 {
			t.Fatal("followed redirect")
		}
		if path == "/rate" && err.(*Error).RetryAfter != 15*time.Minute {
			t.Fatal("lost Retry-After")
		}
	}
	c.endpoints["cursor"] = srv.URL + "/ok"
	d, err := c.Fetch(context.Background(), "cursor", secret, "")
	if err != nil || len(d.Windows) != 1 || *d.Windows[0].UsedBPS != 0 {
		t.Fatalf("zero quota: %v", err)
	}
}

func TestCodexAdditionalModelQuotaPreservesMeaningAndRejectsOpaqueLabels(t *testing.T) {
	for _, name := range []string{"GPT-5.3-Codex-Spark", "fake-secret-token", "https://invalid.test/fake-secret", ""} {
		raw, err := json.Marshal(map[string]any{
			"rate_limit":             map[string]any{"primary_window": map[string]any{"used_percent": 36, "limit_window_seconds": 604800}},
			"additional_rate_limits": []any{map[string]any{"limit_name": name, "metered_feature": "fake-opaque-feature", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 0, "limit_window_seconds": 18000}, "secondary_window": map[string]any{"used_percent": 17, "limit_window_seconds": 604800}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		d, err := Parse("codex", raw)
		if err != nil {
			t.Fatal(err)
		}
		want := "附加额度"
		if name == "GPT-5.3-Codex-Spark" {
			want = name
		}
		if len(d.Windows) != 3 || d.Windows[0].Label != "订阅额度" || d.Windows[0].Period != "week" || d.Windows[1].Label != want || d.Windows[2].Label != want || d.Windows[1].Period != "session" || d.Windows[2].Period != "week" || *d.Windows[1].UsedBPS != 0 || *d.Windows[2].UsedBPS != 1700 {
			t.Fatal("model-specific limits confused with the main subscription")
		}
		normalized, _ := json.Marshal(d)
		if strings.Contains(string(normalized), "fake-") {
			t.Fatal("opaque upstream data exposed")
		}
	}
}
