// Package agentquota reads upstream subscription allowances, independently of
// the gateway's local token accounting and API-key budgets. No response bodies,
// credentials, account identities or upstream error messages are retained.
package agentquota

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Interval = 5 * time.Minute

// UsedBPS is an integer percentage: 10000 means 100%. Missing is unknown,
// never zero or unlimited. No money conversion is performed here.
type Window struct {
	ID            string     `json:"id"`
	Label         string     `json:"label" i18n:"text"`
	Period        string     `json:"period"`
	UsedBPS       *int64     `json:"used_bps"`
	WindowSeconds int64      `json:"window_seconds,omitempty"`
	ResetsAt      *time.Time `json:"resets_at"`
}

type Data struct {
	Windows []Window `json:"windows"`
	Source  string   `json:"source"`
}

type Error struct {
	Code       string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "subscription quota: " + e.Code }

var errShape = &Error{Code: "invalid_response"}
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})(\.[0-9]{1,25})?$`)

func percent(n json.Number) *int64 {
	if !decimal.MatchString(string(n)) {
		return nil
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 || r.Cmp(big.NewRat(1000, 1)) > 0 {
		return nil
	}
	r.Mul(r, big.NewRat(100, 1))
	i := new(big.Int).Quo(r.Num(), r.Denom())
	v := i.Int64()
	return &v
}
func epoch(n int64) *time.Time {
	if n <= 0 || n > 253402300799 {
		return nil
	}
	t := time.Unix(n, 0).UTC()
	return &t
}
func date(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
func period(seconds int64) string {
	switch {
	case seconds > 0 && seconds <= 86400:
		return "session"
	case seconds == 604800:
		return "week"
	case seconds >= 28*86400 && seconds <= 31*86400:
		return "month"
	default:
		return "other"
	}
}

func Parse(provider string, raw []byte) (Data, error) {
	var d Data
	d.Source = "api"
	d.Windows = []Window{}
	switch provider {
	case "codex":
		type window struct {
			Used    json.Number `json:"used_percent"`
			Seconds int64       `json:"limit_window_seconds"`
			Reset   int64       `json:"reset_at"`
		}
		type limit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		}
		var o struct {
			Rate       *limit `json:"rate_limit"`
			Review     *limit `json:"code_review_rate_limit"`
			Additional []struct {
				Name string `json:"limit_name"`
				Rate *limit `json:"rate_limit"`
			} `json:"additional_rate_limits"`
		}
		if json.Unmarshal(raw, &o) != nil {
			return d, errShape
		}
		add := func(id, label string, l *limit) {
			if l == nil {
				return
			}
			for i, w := range []*window{l.Primary, l.Secondary} {
				if w == nil {
					continue
				}
				p := percent(w.Used)
				if p == nil || w.Seconds <= 0 {
					continue
				}
				d.Windows = append(d.Windows, Window{ID: id + "_" + strconv.Itoa(i), Label: label, Period: period(w.Seconds), UsedBPS: p, WindowSeconds: w.Seconds, ResetsAt: epoch(w.Reset)})
			}
		}
		add("codex", "订阅额度", o.Rate)
		add("review", "代码审查额度", o.Review)
		for i, x := range o.Additional {
			if i >= 16 {
				break
			}
			label := "附加额度"
			// OpenAI's model-specific buckets carry a public model name. Keep
			// arbitrary upstream text out of the normalized account response.
			if codexQuotaModelName.MatchString(x.Name) {
				label = x.Name
			}
			add("additional_"+strconv.Itoa(i), label, x.Rate)
		}
	case "claude":
		type window struct {
			Used  json.Number `json:"utilization"`
			Reset string      `json:"resets_at"`
		}
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil {
			return d, errShape
		}
		for _, s := range []struct {
			id, label, p string
			seconds      int64
		}{{"five_hour", "订阅额度", "session", 18000}, {"seven_day", "订阅额度", "week", 604800}, {"seven_day_sonnet", "Sonnet 额度", "week", 604800}, {"seven_day_opus", "Opus 额度", "week", 604800}, {"seven_day_oauth_apps", "OAuth 应用额度", "week", 604800}, {"seven_day_cowork", "Cowork 额度", "week", 604800}} {
			var w window
			if json.Unmarshal(o[s.id], &w) != nil {
				continue
			}
			p := percent(w.Used)
			if p == nil {
				continue
			}
			d.Windows = append(d.Windows, Window{ID: s.id, Label: s.label, Period: s.p, UsedBPS: p, WindowSeconds: s.seconds, ResetsAt: date(w.Reset)})
		}
		addClaudeScopedWindows(&d, o["limits"])
		// Extra usage is a separate monthly spending allowance, not the plan's
		// included usage. The API supplies a percentage; do not infer currency.
		var extra struct {
			Enabled bool        `json:"is_enabled"`
			Used    json.Number `json:"utilization"`
		}
		if json.Unmarshal(o["extra_usage"], &extra) == nil && extra.Enabled {
			if p := percent(extra.Used); p != nil {
				d.Windows = append(d.Windows, Window{ID: "extra_usage", Label: "额外用量额度", Period: "month", UsedBPS: p})
			}
		}
	case "cursor":
		var o struct {
			End  string `json:"billingCycleEnd"`
			Plan *struct {
				Auto  json.Number `json:"autoPercentUsed"`
				API   json.Number `json:"apiPercentUsed"`
				Total json.Number `json:"totalPercentUsed"`
			} `json:"planUsage"`
		}
		if json.Unmarshal(raw, &o) != nil || o.Plan == nil {
			return d, errShape
		}
		end, _ := strconv.ParseInt(o.End, 10, 64)
		for _, x := range []struct {
			id, label string
			n         json.Number
		}{{"cursor_models", "Cursor 模型额度", o.Plan.Auto}, {"other_models", "其他模型额度", o.Plan.API}, {"total", "总额度", o.Plan.Total}} {
			if p := percent(x.n); p != nil {
				d.Windows = append(d.Windows, Window{ID: x.id, Label: x.label, Period: "month", UsedBPS: p, ResetsAt: epoch(end / 1000)})
			}
		}
	case "grok":
		return parseGrok(raw)
	default:
		return d, &Error{Code: "unsupported"}
	}
	if len(d.Windows) == 0 {
		return d, errShape
	}
	return d, nil
}

var codexQuotaModelName = regexp.MustCompile(`(?i)^gpt-[a-z0-9][a-z0-9._-]{0,63}$`)

// Claude's model-specific weekly limits are also carried in limits[], with
// percent rather than utilization. A model ID can be null; is_active does not
// indicate whether the allowance exists. Only public, recognized model labels
// enter the account response; arbitrary upstream text is never forwarded.
func addClaudeScopedWindows(d *Data, raw json.RawMessage) {
	var limits []json.RawMessage
	if json.Unmarshal(raw, &limits) != nil {
		return
	}
	seen := map[string]bool{}
	for i, item := range limits {
		if i >= 64 {
			break
		}
		var l struct {
			Kind    string      `json:"kind"`
			Percent json.Number `json:"percent"`
			Reset   string      `json:"resets_at"`
			Scope   struct {
				Model *struct {
					Name string `json:"display_name"`
				} `json:"model"`
				Surface json.RawMessage `json:"surface"`
			} `json:"scope"`
		}
		if json.Unmarshal(item, &l) != nil || l.Kind != "weekly_scoped" || l.Scope.Model == nil {
			continue
		}
		if surface := strings.TrimSpace(string(l.Scope.Surface)); surface != "" && surface != "null" {
			continue
		}
		var id, label string
		switch strings.ToLower(strings.TrimSpace(l.Scope.Model.Name)) {
		case "fable":
			id, label = "seven_day_fable", "Fable 额度"
		case "sonnet":
			id, label = "seven_day_sonnet", "Sonnet 额度"
		case "opus":
			id, label = "seven_day_opus", "Opus 额度"
		default:
			continue
		}
		p := percent(l.Percent)
		if p == nil || seen[id] {
			continue
		}
		seen[id] = true
		w := Window{ID: id, Label: label, Period: "week", UsedBPS: p, WindowSeconds: 604800, ResetsAt: date(l.Reset)}
		// A scoped entry supersedes its legacy flat field without a duplicate bar.
		found := false
		for i := range d.Windows {
			if d.Windows[i].ID == id {
				d.Windows[i], found = w, true
				break
			}
		}
		if !found {
			d.Windows = append(d.Windows, w)
		}
	}
}

// ClaudeHeaders observes only the documented CLI's quota header names. It never
// reads model content or retains arbitrary headers. Utilization is a ratio.
func ClaudeHeaders(h http.Header) Data {
	d := Data{Source: "response_headers", Windows: []Window{}}
	for _, x := range []struct {
		name, p string
		seconds int64
	}{{"5h", "session", 18000}, {"7d", "week", 604800}} {
		prefix := "anthropic-ratelimit-unified-" + x.name
		raw := h.Get(prefix + "-utilization")
		if !decimal.MatchString(raw) {
			continue
		}
		r, ok := new(big.Rat).SetString(raw)
		if !ok || r.Sign() < 0 || r.Cmp(big.NewRat(10, 1)) > 0 {
			continue
		}
		r.Mul(r, big.NewRat(10000, 1))
		v := new(big.Int).Quo(r.Num(), r.Denom()).Int64()
		reset, _ := strconv.ParseInt(h.Get(prefix+"-reset"), 10, 64)
		d.Windows = append(d.Windows, Window{ID: x.name, Label: "订阅额度", Period: x.p, UsedBPS: &v, WindowSeconds: x.seconds, ResetsAt: epoch(reset)})
	}
	return d
}

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "unavailable"
}
func retryAfter(h string, now time.Time) time.Duration {
	if n, err := strconv.ParseInt(strings.TrimSpace(h), 10, 32); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
