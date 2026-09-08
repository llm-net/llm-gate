package usage

// 分时段目录价的可执行验收：DeepSeek 形态的高峰 / 空闲 + 周末全天低价能按
// 时刻选对表（含时区换算与左闭右开边界），形态错误逐类拒收。

import (
	"errors"
	"testing"
	"time"
)

// deepSeekSchedule 照官方价签构造：高峰为北京时间周一至周五 9:00–12:00、
// 14:00–18:00，其余（含周末全天）为空闲时段，空闲价为高峰价的一半。
const deepSeekSchedule = `{
  "timezone": "Asia/Shanghai",
  "periods": [
    {"label": "工作日空闲", "days": ["mon","tue","wed","thu","fri"],
     "hours": [["00:00","09:00"],["12:00","14:00"],["18:00","24:00"]],
     "pricing": {"in": 4500000, "cache_read": 150000, "out": 13500000}},
    {"label": "周末全天", "days": ["sat","sun"],
     "hours": [["00:00","24:00"]],
     "pricing": {"in": 4500000, "cache_read": 150000, "out": 13500000}}
  ]
}`

var deepSeekBase = Pricing{"in": 9_000_000, "cache_read": 300_000, "out": 27_000_000}

func TestSchedulePricingAtDeepSeekShape(t *testing.T) {
	s, err := ParseSchedule([]byte(deepSeekSchedule), deepSeekBase)
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name    string
		at      time.Time
		offPeak bool
	}{
		{"周一 08:59 空闲", time.Date(2026, 9, 7, 8, 59, 0, 0, cst), true},
		{"周一 09:00 整进入高峰（左闭）", time.Date(2026, 9, 7, 9, 0, 0, 0, cst), false},
		{"周一 11:59 高峰", time.Date(2026, 9, 7, 11, 59, 0, 0, cst), false},
		{"周一 12:00 午间空闲（右开）", time.Date(2026, 9, 7, 12, 0, 0, 0, cst), true},
		{"周一 13:59 午间空闲", time.Date(2026, 9, 7, 13, 59, 59, 0, cst), true},
		{"周一 14:00 高峰", time.Date(2026, 9, 7, 14, 0, 0, 0, cst), false},
		{"周一 17:59 高峰", time.Date(2026, 9, 7, 17, 59, 0, 0, cst), false},
		{"周一 18:00 空闲", time.Date(2026, 9, 7, 18, 0, 0, 0, cst), true},
		{"周五 23:59 空闲", time.Date(2026, 9, 11, 23, 59, 0, 0, cst), true},
		{"周六 15:00 周末全天空闲", time.Date(2026, 9, 12, 15, 0, 0, 0, cst), true},
		{"周日 10:00 周末全天空闲", time.Date(2026, 9, 13, 10, 0, 0, 0, cst), true},
		// 时区换算：UTC 01:00 = 北京 09:00（高峰）；UTC 周日 16:00 = 北京周一 00:00（空闲）。
		{"UTC 周一 01:00 = 北京 09:00 高峰", time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC), false},
		{"UTC 周日 16:00 = 北京周一 00:00 空闲", time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC), true},
		{"UTC 周五 10:30 = 北京周五 18:30 空闲", time.Date(2026, 9, 11, 10, 30, 0, 0, time.UTC), true},
	}
	for _, tc := range cases {
		got := s.PricingAt(tc.at, deepSeekBase)
		wantIn := deepSeekBase["in"]
		if tc.offPeak {
			wantIn = 4_500_000
		}
		if got["in"] != wantIn || got["out"] != wantIn*3 {
			t.Errorf("%s: in=%d out=%d，期望 in=%d", tc.name, got["in"], got["out"], wantIn)
		}
	}
	if s.Location() == nil || s.Location().String() != "Asia/Shanghai" {
		t.Errorf("Location = %v", s.Location())
	}
}

func TestScheduleNilAndEmpty(t *testing.T) {
	for _, raw := range []string{"", "null", "  null  "} {
		s, err := ParseSchedule([]byte(raw), deepSeekBase)
		if err != nil || s != nil {
			t.Errorf("%q: 应视为没有分时段价，得到 %v, %v", raw, s, err)
		}
	}
	var s *Schedule
	if got := s.PricingAt(time.Now(), deepSeekBase); got["in"] != deepSeekBase["in"] {
		t.Error("nil Schedule 应恒返回标准价")
	}
}

func TestScheduleRejects(t *testing.T) {
	period := func(days, hours, pricing string) string {
		return `{"timezone":"Asia/Shanghai","periods":[{"days":` + days + `,"hours":` + hours + `,"pricing":` + pricing + `}]}`
	}
	okPricing := `{"in":1,"cache_read":1,"out":1}`
	cases := map[string]string{
		"不是对象":      `[]`,
		"没有时区":      `{"periods":[{"days":["mon"],"hours":[["00:00","01:00"]],"pricing":` + okPricing + `}]}`,
		"Local 时区":  `{"timezone":"Local","periods":[{"days":["mon"],"hours":[["00:00","01:00"]],"pricing":` + okPricing + `}]}`,
		"不认识的时区":    `{"timezone":"Mars/Olympus","periods":[{"days":["mon"],"hours":[["00:00","01:00"]],"pricing":` + okPricing + `}]}`,
		"没有时段":      `{"timezone":"UTC","periods":[]}`,
		"days 为空":   period(`[]`, `[["00:00","01:00"]]`, okPricing),
		"星期写法":      period(`["Monday"]`, `[["00:00","01:00"]]`, okPricing),
		"星期重复":      period(`["mon","mon"]`, `[["00:00","01:00"]]`, okPricing),
		"hours 为空":  period(`["mon"]`, `[]`, okPricing),
		"区间不是两个值":   period(`["mon"]`, `[["00:00"]]`, okPricing),
		"时刻格式":      period(`["mon"]`, `[["0:00","01:00"]]`, okPricing),
		"起点写 24:00": period(`["mon"]`, `[["24:00","24:00"]]`, okPricing),
		"终点 24:30":  period(`["mon"]`, `[["00:00","24:30"]]`, okPricing),
		"起点晚于终点":    period(`["mon"]`, `[["22:00","02:00"]]`, okPricing),
		"同一时段内重叠":   period(`["mon"]`, `[["00:00","10:00"],["09:00","12:00"]]`, okPricing),
		"时段价为空":     period(`["mon"]`, `[["00:00","01:00"]]`, `{}`),
		"时段价字段集不同":  period(`["mon"]`, `[["00:00","01:00"]]`, `{"in":1,"out":1}`),
		"时段价不是整数":   period(`["mon"]`, `[["00:00","01:00"]]`, `{"in":"1","cache_read":1,"out":1}`),
		"跨时段重叠": `{"timezone":"UTC","periods":[
			{"days":["sat","sun"],"hours":[["00:00","24:00"]],"pricing":` + okPricing + `},
			{"days":["sun"],"hours":[["23:00","24:00"]],"pricing":` + okPricing + `}]}`,
		"尾随内容": period(`["mon"]`, `[["00:00","01:00"]]`, okPricing) + `}`,
	}
	for name, raw := range cases {
		if _, err := ParseSchedule([]byte(raw), deepSeekBase); !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("%s: 应拒收，得到 %v", name, err)
		}
	}
	// 没有标准价就不能有分时段价。
	if _, err := ParseSchedule([]byte(period(`["mon"]`, `[["00:00","01:00"]]`, okPricing)), nil); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("无标准价: 应拒收，得到 %v", err)
	}
	// 相邻不算重叠：[00:00,09:00) 与 [09:00,12:00)。
	touching := `{"timezone":"UTC","periods":[
		{"days":["mon"],"hours":[["00:00","09:00"]],"pricing":` + okPricing + `},
		{"days":["mon"],"hours":[["09:00","12:00"]],"pricing":` + okPricing + `}]}`
	if _, err := ParseSchedule([]byte(touching), deepSeekBase); err != nil {
		t.Errorf("相邻区间不该判重叠: %v", err)
	}
	// 时段数上限。
	many := `{"timezone":"UTC","periods":[`
	for i := 0; i <= MaxSchedulePeriods; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"days":["mon"],"hours":[["00:00","00:01"]],"pricing":` + okPricing + `}`
	}
	many += `]}`
	if _, err := ParseSchedule([]byte(many), deepSeekBase); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("超过时段上限: 应拒收，得到 %v", err)
	}
}

func TestCheckPricingPairs(t *testing.T) {
	if err := CheckPricingPairs(Pricing{FieldIn: 1, FieldOut: 2, FieldCacheRead: 0}); err != nil {
		t.Errorf("成对齐全不该报错: %v", err)
	}
	if err := CheckPricingPairs(Pricing{FieldIn: 1}); err == nil {
		t.Error("只配输入价应报错")
	}
	if err := CheckPricingPairs(Pricing{FieldArkVideoTokenRef: 1}); err == nil {
		t.Error("Seedance 只配一档应报错")
	}
}
