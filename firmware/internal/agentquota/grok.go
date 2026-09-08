package agentquota

import "encoding/json"

func parseGrok(raw []byte) (Data, error) {
	d := Data{Source: "api", Windows: []Window{}}
	var o struct {
		Config *struct {
			Used    json.Number `json:"creditUsagePercent"`
			Start   string      `json:"billingPeriodStart"`
			End     string      `json:"billingPeriodEnd"`
			Current struct {
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
	p := percent(c.Used)
	if p == nil {
		return d, errShape
	}
	start, end := date(c.Current.Start), date(c.Current.End)
	if start == nil {
		start = date(c.Start)
	}
	if end == nil {
		end = date(c.End)
	}
	var seconds int64
	if start != nil && end != nil && end.After(*start) {
		seconds = int64(end.Sub(*start).Seconds())
	}
	periodName := period(seconds)
	switch c.Current.Type {
	case "week", "weekly":
		periodName = "week"
	case "month", "monthly":
		periodName = "month"
	}
	d.Windows = append(d.Windows, Window{ID: "grok", Label: "共享订阅额度", Period: periodName, UsedBPS: p, WindowSeconds: seconds, ResetsAt: end})
	return d, nil
}
