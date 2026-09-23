package agentquota

import (
	"encoding/json"
	"strings"
	"time"
)

// parseGrok reads the unified-billing credits view. The upstream body is
// protobuf JSON: zero-valued scalars are omitted, so right after the weekly
// reset `creditUsagePercent` is absent while the period bounds stay present.
// That absence is an explicit 0% reading, not a missing window; a body without
// a recognizable period is still rejected rather than reported as 0.
func parseGrok(raw []byte) (Data, error) {
	d := Data{Source: "api", Windows: []Window{}}
	var o struct {
		Config *struct {
			Used    json.RawMessage `json:"creditUsagePercent"`
			Start   string          `json:"billingPeriodStart"`
			End     string          `json:"billingPeriodEnd"`
			Current *struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Config == nil {
		return d, errShape
	}
	c := o.Config
	var start, end *time.Time
	periodType := ""
	if c.Current != nil {
		start, end = date(c.Current.Start), date(c.Current.End)
		periodType = c.Current.Type
	}
	if start == nil {
		start = date(c.Start)
	}
	if end == nil {
		end = date(c.End)
	}
	var p *int64
	switch {
	case len(c.Used) > 0:
		// Present but null or malformed is not a reading.
		var n json.Number
		if json.Unmarshal(c.Used, &n) != nil {
			return d, errShape
		}
		if p = percent(n); p == nil {
			return d, errShape
		}
	case start != nil && end != nil:
		zero := int64(0)
		p = &zero
	default:
		return d, errShape
	}
	var seconds int64
	if start != nil && end != nil && end.After(*start) {
		seconds = int64(end.Sub(*start).Seconds())
	}
	periodName := period(seconds)
	// The period type is an enum name such as USAGE_PERIOD_TYPE_WEEKLY.
	switch t := strings.ToLower(periodType); {
	case strings.Contains(t, "week"):
		periodName = "week"
	case strings.Contains(t, "month"):
		periodName = "month"
	}
	d.Windows = append(d.Windows, Window{ID: "grok", Label: "共享订阅额度", Period: periodName, UsedBPS: p, WindowSeconds: seconds, ResetsAt: end})
	return d, nil
}
