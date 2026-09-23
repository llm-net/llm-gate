package modeldata

// 数据仓库的循环分时价（rule.time_pricing：按序首个匹配的档，末档无条件兜底）
// → 设备的分时段价（schedule：若干明示时段各带完整价表，时段之外按标准价）。
//
// 两种表达的差别只在「兜底」怎么写：数据仓库把兜底档写成一条无 days / hours 的
// band，设备把兜底写成「不在任何时段内」。换算就是按星期逐分钟求出每个档实际
// 覆盖的区间（扣掉更早的档），再把非首档的区间按星期分组写成设备时段。首档必须
// 等于 rule.rates（数据仓库自己的约束），它就是设备的标准价。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const minutesPerDay = 24 * 60

type span struct{ from, to int } // [from, to) 分钟

// schedulePeriod 是设备形态的一个时段（json 键与 usage.scheduleWire 逐字对齐）。
type schedulePeriod struct {
	Label   string           `json:"label,omitempty"`
	Days    []string         `json:"days"`
	Hours   [][2]string      `json:"hours"`
	Pricing map[string]int64 `json:"pricing"`
}

type scheduleDoc struct {
	Timezone string           `json:"timezone"`
	Periods  []schedulePeriod `json:"periods"`
}

// convertSchedule 换算一条模型的分时价。base 是已换算的标准价（用于字段集校验）。
func (f fx) convertSchedule(kind, face, currency string, tp *TimePricing, baseRates []Rate, base map[string]int64) (*scheduleDoc, error) {
	if tp == nil {
		return nil, nil
	}
	if len(tp.Bands) < 2 {
		return nil, errors.New("time_pricing 至少两档")
	}
	if !sameRates(tp.Bands[0].Rates, baseRates) {
		return nil, errors.New("time_pricing 首档与 rule.rates 不一致")
	}
	// 每档在每个星期几覆盖的区间（扣掉更早的档）。
	type dayCover map[int][]span // ISO 星期 → 区间
	claimed := dayCover{}
	out := &scheduleDoc{Timezone: tp.Timezone}
	for i, b := range tp.Bands {
		own := dayCover{}
		for d := 1; d <= 7; d++ {
			if len(b.Days) > 0 && !containsInt(b.Days, d) {
				continue
			}
			spans := []span{{0, minutesPerDay}}
			if len(b.Hours) > 0 {
				spans = spans[:0]
				for _, h := range b.Hours {
					s, err := parseSpan(h)
					if err != nil {
						return nil, fmt.Errorf("第 %d 档：%v", i+1, err)
					}
					spans = append(spans, s)
				}
			}
			free := subtract(spans, claimed[d])
			if len(free) > 0 {
				own[d] = free
				claimed[d] = merge(append(claimed[d], free...))
			}
		}
		if i == 0 {
			continue // 首档 = 标准价
		}
		pricing, _, err := f.convertRates(kind, face, currency, b.Rates)
		if err != nil {
			return nil, fmt.Errorf("第 %d 档：%v", i+1, err)
		}
		if !sameFields(pricing, base) {
			return nil, fmt.Errorf("第 %d 档的价目字段集与标准价不同", i+1)
		}
		// 按「区间列表相同」把星期分组，一组一个时段。
		groups := map[string][]int{}
		for d := 1; d <= 7; d++ {
			if spans := own[d]; len(spans) > 0 {
				key := spanKey(spans)
				groups[key] = append(groups[key], d)
			}
		}
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, c int) bool { return groups[keys[a]][0] < groups[keys[c]][0] })
		for _, k := range keys {
			days := groups[k]
			p := schedulePeriod{Label: b.Label, Pricing: pricing}
			for _, d := range days {
				p.Days = append(p.Days, usage.WeekdayNames[d-1])
			}
			for _, s := range own[days[0]] {
				p.Hours = append(p.Hours, [2]string{clock(s.from), clock(s.to)})
			}
			out.Periods = append(out.Periods, p)
		}
	}
	if len(out.Periods) == 0 {
		return nil, nil // 后续各档没覆盖到任何时刻：全时段标准价
	}
	if len(out.Periods) > usage.MaxSchedulePeriods {
		return nil, fmt.Errorf("换算出 %d 个时段，超过设备上限 %d", len(out.Periods), usage.MaxSchedulePeriods)
	}
	return out, nil
}

func parseSpan(h string) (span, error) {
	parts := strings.Split(h, "-")
	if len(parts) != 2 {
		return span{}, fmt.Errorf("时段 %q 不是 HH:MM-HH:MM", h)
	}
	from, err := parseClock(parts[0])
	if err != nil {
		return span{}, err
	}
	to, err := parseClock(parts[1])
	if err != nil {
		return span{}, err
	}
	if from >= to {
		return span{}, fmt.Errorf("时段 %q 起止颠倒或为空", h)
	}
	return span{from, to}, nil
}

func parseClock(s string) (int, error) {
	hm := strings.Split(s, ":")
	if len(hm) != 2 {
		return 0, fmt.Errorf("时刻 %q 不是 HH:MM", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("时刻 %q 不合法", s)
	}
	return h*60 + m, nil
}

func clock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// subtract 从 spans 里扣掉 taken 覆盖的部分。
func subtract(spans, taken []span) []span {
	out := spans
	for _, t := range taken {
		var next []span
		for _, s := range out {
			switch {
			case t.to <= s.from || t.from >= s.to:
				next = append(next, s)
			default:
				if s.from < t.from {
					next = append(next, span{s.from, t.from})
				}
				if t.to < s.to {
					next = append(next, span{t.to, s.to})
				}
			}
		}
		out = next
	}
	return merge(out)
}

// merge 排序并合并相邻 / 重叠区间。
func merge(spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	out := []span{spans[0]}
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s.from <= last.to {
			if s.to > last.to {
				last.to = s.to
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func spanKey(spans []span) string {
	parts := make([]string, 0, len(spans))
	for _, s := range spans {
		parts = append(parts, fmt.Sprintf("%d-%d", s.from, s.to))
	}
	return strings.Join(parts, ",")
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func sameRates(a, b []Rate) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(rs []Rate) string {
		parts := make([]string, 0, len(rs))
		for _, r := range rs {
			parts = append(parts, r.Meter+"="+r.Amount+"/"+strconv.FormatInt(r.Per, 10)+r.Unit)
		}
		sort.Strings(parts)
		return strings.Join(parts, ";")
	}
	return key(a) == key(b)
}

func sameFields(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
