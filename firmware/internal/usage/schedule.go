package usage

// schedule.go 是**分时段目录价**的形态：厂商按星期与时刻切换单价的价签
// （DeepSeek 的高峰 / 空闲时段、周末全天低价）。官方价目文件里一条模型可带
// schedule 段，形态与本文件的 Schedule 逐字对齐：
//
//	timezone   厂商的计费时区（IANA 名，如 Asia/Shanghai）
//	periods    若干时段，每条 = days（mon…sun）× hours（[从, 到) 的 HH:MM 区间，
//	           「到」可写 24:00）→ 该时段的**完整**价目表（字段集必须与标准价相同）
//
// 不在任何时段内的时刻按标准价（那条的 pricing）计；时段之间不得重叠，所以
// 「某一时刻按哪张表」没有歧义（PricingAt）。周末全天低价就是 days 为 sat/sun、
// hours 为 00:00–24:00 的一条；跨午夜的时段拆成两段写。
//
// 边界：本文件只做解析、校验与选表。设备的记账路径尚未接入分时段价——
// models.pricing 仍是一张扁平表，数据升级只把标准价搬进目录（admin/catalogsync）。
// 接入时 Sample.At 已经是记账时刻，Cost 的调用方按 PricingAt(at) 取表即可，
// Cost 自身的算术一个字节都不必改。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	// 设备是精简的 Linux 根文件系统，/usr/share/zoneinfo 未必在场；时区表
	// 随二进制走，分时段价才不依赖宿主。
	_ "time/tzdata"
)

const (
	// MaxSchedulePeriods 是一条模型可带的时段数上限；MaxScheduleHours 是一个
	// 时段可带的区间数上限。真实价签只有两三条，上限挡的是形态正确但异常巨大
	// 的远端文件。
	MaxSchedulePeriods = 16
	MaxScheduleHours   = 8

	minutesPerDay = 24 * 60
)

// ErrInvalidSchedule 表示分时段价不合形态。错误文本只描述形态与时段序号，
// 不回显远端文件的内容。
var ErrInvalidSchedule = errors.New("usage: 分时段目录价不合法")

func errInvalidSchedulef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSchedule, fmt.Sprintf(format, args...))
}

// Schedule 是解析后的分时段价。nil 表示这条模型没有分时段价（全时段按标准价）。
type Schedule struct {
	Timezone string
	Periods  []SchedulePeriod
	loc      *time.Location
}

// SchedulePeriod 是一个时段：星期集合 × 若干时刻区间 → 这段时间的价目表。
type SchedulePeriod struct {
	Label   string
	Days    []time.Weekday // 升序、去重（Sunday 记作 0，排在最前）
	Hours   []HourRange    // 按 From 升序、互不重叠
	Pricing Pricing
}

// HourRange 是一天里的一个左闭右开区间，单位是自当天 00:00 起的分钟数。
type HourRange struct {
	From int
	To   int // ≤ minutesPerDay；等于 minutesPerDay 即「到 24:00」
}

// scheduleWire 是文件里的形态。
type scheduleWire struct {
	Timezone string `json:"timezone"`
	Periods  []struct {
		Label   string          `json:"label"`
		Days    []string        `json:"days"`
		Hours   [][]string      `json:"hours"`
		Pricing json.RawMessage `json:"pricing"`
	} `json:"periods"`
}

var weekdayNames = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// WeekdayNames 是 days 里认得的写法，按周一到周日排列（供文档与校验工具引用）。
var WeekdayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var clockRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ParseSchedule 解析并校验一条模型的 schedule 段。raw 为空或 null 表示没有分时段
// 价，返回 nil, nil。base 是这条模型的标准价：时段价的字段集必须与它相同，没有
// 标准价就不能有分时段价（不在任何时段内的时刻无价可计）。
func ParseSchedule(raw []byte, base Pricing) (*Schedule, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if !base.Priced() {
		return nil, errInvalidSchedulef("分时段价必须搭配标准价（pricing）")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var wire scheduleWire
	if err := dec.Decode(&wire); err != nil {
		return nil, errInvalidSchedulef("schedule 不是合法的 JSON 对象")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errInvalidSchedulef("schedule 后有多余内容")
	}
	if wire.Timezone == "" || wire.Timezone == "Local" {
		return nil, errInvalidSchedulef("timezone 必须是 IANA 时区名")
	}
	loc, err := time.LoadLocation(wire.Timezone)
	if err != nil {
		return nil, errInvalidSchedulef("timezone 不是可识别的 IANA 时区名")
	}
	if len(wire.Periods) == 0 {
		return nil, errInvalidSchedulef("periods 至少要有一条")
	}
	if len(wire.Periods) > MaxSchedulePeriods {
		return nil, errInvalidSchedulef("periods 超过上限 %d 条", MaxSchedulePeriods)
	}
	baseFields := sortedFields(base)
	out := &Schedule{Timezone: wire.Timezone, loc: loc, Periods: make([]SchedulePeriod, 0, len(wire.Periods))}
	// 按星期归并全部区间，做跨时段的重叠检查。
	perDay := make(map[time.Weekday][]HourRange, 7)
	for i, p := range wire.Periods {
		n := i + 1
		if len(p.Days) == 0 {
			return nil, errInvalidSchedulef("第 %d 条时段没有 days", n)
		}
		days := make([]time.Weekday, 0, len(p.Days))
		seenDay := make(map[time.Weekday]bool, len(p.Days))
		for _, d := range p.Days {
			wd, ok := weekdayNames[d]
			if !ok {
				return nil, errInvalidSchedulef("第 %d 条时段的 days 含不认识的星期写法（只认 %s）", n, strings.Join(WeekdayNames, "/"))
			}
			if seenDay[wd] {
				return nil, errInvalidSchedulef("第 %d 条时段的 days 有重复", n)
			}
			seenDay[wd] = true
			days = append(days, wd)
		}
		sort.Slice(days, func(a, b int) bool { return days[a] < days[b] })
		if len(p.Hours) == 0 {
			return nil, errInvalidSchedulef("第 %d 条时段没有 hours", n)
		}
		if len(p.Hours) > MaxScheduleHours {
			return nil, errInvalidSchedulef("第 %d 条时段的 hours 超过上限 %d 段", n, MaxScheduleHours)
		}
		hours := make([]HourRange, 0, len(p.Hours))
		for _, pair := range p.Hours {
			if len(pair) != 2 {
				return nil, errInvalidSchedulef("第 %d 条时段的 hours 每段须是 [从, 到] 两个 HH:MM", n)
			}
			from, err := parseClock(pair[0], false)
			if err != nil {
				return nil, errInvalidSchedulef("第 %d 条时段的起点 %s", n, err)
			}
			to, err := parseClock(pair[1], true)
			if err != nil {
				return nil, errInvalidSchedulef("第 %d 条时段的终点 %s", n, err)
			}
			if from >= to {
				return nil, errInvalidSchedulef("第 %d 条时段的起点必须早于终点（跨午夜请拆成两段）", n)
			}
			hours = append(hours, HourRange{From: from, To: to})
		}
		sort.Slice(hours, func(a, b int) bool { return hours[a].From < hours[b].From })
		for k := 1; k < len(hours); k++ {
			if hours[k].From < hours[k-1].To {
				return nil, errInvalidSchedulef("第 %d 条时段的 hours 互相重叠", n)
			}
		}
		pricing, err := ParsePricing(string(p.Pricing))
		if err != nil {
			return nil, errInvalidSchedulef("第 %d 条时段的价目表不合法", n)
		}
		if !pricing.Priced() {
			return nil, errInvalidSchedulef("第 %d 条时段没有价目表", n)
		}
		if got := sortedFields(pricing); strings.Join(got, ",") != strings.Join(baseFields, ",") {
			return nil, errInvalidSchedulef("第 %d 条时段的价格字段集必须与标准价相同（%s）", n, strings.Join(baseFields, "、"))
		}
		for _, d := range days {
			perDay[d] = append(perDay[d], hours...)
		}
		out.Periods = append(out.Periods, SchedulePeriod{Label: p.Label, Days: days, Hours: hours, Pricing: pricing})
	}
	for d, ranges := range perDay {
		sort.Slice(ranges, func(a, b int) bool { return ranges[a].From < ranges[b].From })
		for k := 1; k < len(ranges); k++ {
			if ranges[k].From < ranges[k-1].To {
				return nil, errInvalidSchedulef("%s 有两条时段互相重叠", weekdayName(d))
			}
		}
	}
	return out, nil
}

// parseClock 把 HH:MM 折成分钟数。24:00 只允许作为终点。
func parseClock(s string, allowMidnightEnd bool) (int, error) {
	if allowMidnightEnd && s == "24:00" {
		return minutesPerDay, nil
	}
	m := clockRE.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("须是 HH:MM（00:00–23:59，终点另可写 24:00）")
	}
	return atoi2(m[1])*60 + atoi2(m[2]), nil
}

// atoi2 只处理正则放行过的两位十进制。
func atoi2(s string) int { return int(s[0]-'0')*10 + int(s[1]-'0') }

func sortedFields(p Pricing) []string {
	out := make([]string, 0, len(p))
	for f := range p {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func weekdayName(d time.Weekday) string {
	for name, wd := range weekdayNames {
		if wd == d {
			return name
		}
	}
	return d.String()
}

// Location 返回计费时区。
func (s *Schedule) Location() *time.Location {
	if s == nil {
		return nil
	}
	return s.loc
}

// PricingAt 回答「at 这一时刻按哪张表计」：落在某个时段内就用那个时段的价目表，
// 否则用标准价 base。s 为 nil 恒返回 base。判定用 at 换算到计费时区后的星期与
// 时刻；区间左闭右开，所以 09:00 整已经算高峰、24:00 归到次日 00:00。
func (s *Schedule) PricingAt(at time.Time, base Pricing) Pricing {
	if s == nil || s.loc == nil {
		return base
	}
	t := at.In(s.loc)
	wd := t.Weekday()
	minute := t.Hour()*60 + t.Minute()
	for i := range s.Periods {
		p := &s.Periods[i]
		if !p.hasDay(wd) {
			continue
		}
		for _, r := range p.Hours {
			if r.From <= minute && minute < r.To {
				return p.Pricing
			}
		}
	}
	return base
}

func (p *SchedulePeriod) hasDay(d time.Weekday) bool {
	for _, x := range p.Days {
		if x == d {
			return true
		}
	}
	return false
}

// ---- 成对字段（管理端点与校验工具共用的词汇） ----

// PricingPairs 是「要么都填、要么都不填」的价格字段组。
//
// 只填一半是**静默错账**：计价函数的回退是有意的运行期兜底（半张表下
// Priced() 仍为真，模型页不挂「未定价」徽章），于是缺的那一档要么按 0 元
// 计、要么按邻档计，两种都看不出异常。挡在写入侧才是治本——arkVideoTokenPrice
// 的注释点名要求管理端点做这件事。
//
// 不在表里的字段是**有意可选**的，别顺手补进来：
//   - cache_read 缺省按 in 计（缓存命中没单独标价就按输入价，文档口径）；
//   - minimax 的 768P / 2K 秒价互为回退是明写的设计（取已配最高档，偏高看得见）；
//   - minimax_video_image_extra 是超额图片附加费，不配即无此项；
//   - 图片的张价与 token 价「按价签只配其一」。
var PricingPairs = [][2]string{
	{FieldIn, FieldOut},                                 // 文本：只配输入价 = 输出白送
	{FieldArkVideoToken, FieldArkVideoTokenRef},         // Seedance 两档
	{FieldMinimaxContextIRIn, FieldMinimaxContextIROut}, // H3 Context-IR 输入/输出（差价约 4 倍）
}

// CheckPricingPairs 校验成对字段组的完整性。
func CheckPricingPairs(fields Pricing) error {
	for _, pair := range PricingPairs {
		_, hasA := fields[pair[0]]
		_, hasB := fields[pair[1]]
		if hasA == hasB {
			continue
		}
		missing := pair[1]
		if hasB {
			missing = pair[0]
		}
		return fmt.Errorf("价格字段 %q 与 %q 必须成对录入（缺 %q）："+
			"只填一半时另一档会被静默按 0 元或按邻档计费，而模型仍显示为已定价",
			pair[0], pair[1], missing)
	}
	return nil
}
