package usage

// report.go 是读数出口。**管理台的任何区间查询都走这里**，看到的恒是
// 「已落盘的小时行 + 尚未冲刷的内存增量」之和——冲刷节奏对 UI 不可见，
// 也就永远不用为了「看到最新的数」而提前落盘（那正是 SD 卡纪律要避免的）。
//
// 只有一种视图：设备只有一个管理员，账也就只有一本——按密钥/模型/上游/入口/
// kind 五个维度分解 + 按日序列 + 全量明细环。
//
// 按日折算与播种用同一条口径：桶按其**起始时刻**归属本地日期，不做拆分
// （理由见 meter.go 包头的取整方向裁决）。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 读数区间关键词（管理台两个用量端点的 range 参数）。窗口一律按**设备本地
// 时区**裁，与预算窗口同一套坐标（见 meter.go 包头）：管理员心里的「今天」
// 是墙上钟表的今天，不是 UTC 的。
const (
	RangeToday = "today" // 本地今日零点 → 此刻
	RangeMonth = "month" // 本地本月 1 日零点 → 此刻
	Range7d    = "7d"    // 含今天在内的 7 个自然日
	Range30d   = "30d"   // 含今天在内的 30 个自然日
)

// SubscriptionTrafficDimension 是报表汇总键，不是账本入口或计价协议。
// 各订阅的原始 entry 保留在账本和请求明细中。
const SubscriptionTrafficDimension = "subscription"

func reportEntryDimension(entry string) string {
	switch entry {
	case EntryResponsesAgents, EntryClaudeCode, EntryCursorAgent, EntryImagineImage, EntryImagineVideo:
		return SubscriptionTrafficDimension
	default:
		return entry
	}
}

// RangeWindow 把区间关键词折成 [from, to) 窗口；ok 为 false 表示关键词不认识
// （调用方回 400，**别静默换成缺省区间**——那会让人以为看到的是自己要的那段）。
//
// 终点恒取此刻而不是「今日 24:00」：读数看的是已经花掉的钱，未来的空桶只会在
// 图上拖一条长尾。起点按自然日/自然月边界对齐，于是 7d/30d 的按日序列恰好是
// 7/30 个日期，当天那格随时间长出来。
//
// 30d 的跨度恒在保留期内：usage_days 的下限是 31 天（config.MinUsageDays）。
func (m *Meter) RangeWindow(name string) (from, to time.Time, ok bool) {
	now := m.now()
	local := now.In(m.loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, m.loc)
	switch name {
	case RangeToday:
		return dayStart, now, true
	case RangeMonth:
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, m.loc), now, true
	case Range7d:
		return dayStart.AddDate(0, 0, -6), now, true
	case Range30d:
		return dayStart.AddDate(0, 0, -29), now, true
	}
	return time.Time{}, time.Time{}, false
}

// Summary 是一组行的合计。金额为主读数，token 为辅（cache 命中率与「未定价
// 模型用了多少」都靠它们回答）。
type Summary struct {
	CostMicro           int64 `json:"cost_micro"`
	Requests            int64 `json:"requests"`
	Errors              int64 `json:"errors"`
	RejectedRequests    int64 `json:"rejected_requests"`
	EstimatedRequests   int64 `json:"estimated_requests"`
	UnavailableRequests int64 `json:"unavailable_requests"`
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheWriteTokens    int64 `json:"cache_write_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	// VideoSeconds / ImageCount 是非 token 形态的计费量（H3 按秒 + 参考图张数、
	// Seedream 按出图张数）。功能边界的「金额为主、token/**秒**为辅」就靠这两列
	// 兑现：没有它们，每一行视频消费在页面上都是「Token 0」。
	VideoSeconds  int64 `json:"video_seconds"`
	ImageCount    int64 `json:"image_count"`
	DurationMsSum int64 `json:"duration_ms_sum"`
}

// add 累加一行（UsageRow 与 UsageDelta 字段同名同序，取值语义不同而已）。
func (s *Summary) add(d store.UsageDelta) {
	s.CostMicro += d.CostMicro
	s.Requests += d.Requests
	s.Errors += d.Errors
	s.RejectedRequests += d.RejectedRequests
	s.EstimatedRequests += d.EstimatedRequests
	s.UnavailableRequests += d.UnavailableRequests
	s.PromptTokens += d.PromptTokens
	s.CompletionTokens += d.CompletionTokens
	s.CacheReadTokens += d.CacheReadTokens
	s.CacheWriteTokens += d.CacheWriteTokens
	s.TotalTokens += d.TotalTokens
	s.VideoSeconds += d.VideoSeconds
	s.ImageCount += d.ImageCount
	s.DurationMsSum += d.DurationMsSum
}

// DimRow 是某个维度取值的合计。ID 只在用户/密钥两个维度有意义（其余为 0）；
// Key 恒是展示用的名字快照（用户名 / 密钥展示串 / 模型名 / 上游账户名 /
// 入口 / kind）。
type DimRow struct {
	ID  int64  `json:"id,omitempty"`
	Key string `json:"key"`
	// Label 只在按 API 密钥维度出现，取密钥当前的管理标签。标签不参与
	// 分组：管理员改标签后历史用量仍归在同一个 key_id 下，只更新展示名。
	Label string `json:"label,omitempty"`
	// Priced 只在**按模型**这一个维度出现，且只对目录里真的有这一行的名字给值：
	// true = 已录目录价，false = 目录里有这个模型但没录价（页面挂「未定价」
	// 徽章），缺省（nil）= 无从判断——客户端随口给的模型名、被并进
	// UnknownModelDim 的哨兵行、以及其余五个维度。
	//
	// 它是**读目录得来的真值**，不是从金额反推的：一个只被 count_tokens 打过的
	// 已定价模型金额同样是 0（会被反推成"未定价"），而窗口内混着一笔已定价调用的
	// 未定价模型金额又大于 0（会被反推成"已定价"）——两个方向都错过。
	Priced *bool `json:"priced,omitempty"`
	Summary
}

// KeyRef 是用量页的 API 密钥选择项。它同时包含当前存在但本区间没有用量的
// 密钥，以及本区间有历史用量但密钥行已经不存在的密钥；后者没有 Label。
// Display 恒为脱敏展示串，不含明文或摘要。
type KeyRef struct {
	ID      int64  `json:"id"`
	Display string `json:"display"`
	Label   string `json:"label,omitempty"`
}

// DayPoint 是按日序列的一个点（Day 为设备**本地**日期，YYYY-MM-DD）。
type DayPoint struct {
	Day string `json:"day"`
	Summary
}

// Report 是一次区间读数的全部结果。
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Total 是区间合计。
	Total      Summary  `json:"total"`
	ByKey      []DimRow `json:"by_key"`
	ByModel    []DimRow `json:"by_model"`
	ByUpstream []DimRow `json:"by_upstream"`
	ByEntry    []DimRow `json:"by_entry"`
	ByKind     []DimRow `json:"by_kind"`
	// Keys 给页面切换「全部 / 单 Key」统计对象；即使当前报表按一把密钥
	// 过滤，它仍保留完整选择项，切换无需再发一趟密钥列表请求。
	Keys []KeyRef `json:"keys"`
	// Days 按本地日期升序，**只含有数据的那些天**（补零留给展示层——它才知道
	// 图上要画多宽）。
	Days []DayPoint `json:"days"`
	// Recent 是明细环（新的在前）。
	Recent []Event `json:"recent"`
}

// Report 汇总 [from, to) 区间的用量。
//
// 区间按小时桶取整：起点向下、终点向上——宁可多含桶两端各不到一小时的量，
// 也不少含（读数是给人看趋势的，边界少一段比多一段更容易被误读成掉量）。
func (m *Meter) Report(ctx context.Context, from, to time.Time) (*Report, error) {
	return m.report(ctx, from, to, 0)
}

// ReportForKey 汇总 [from, to) 区间内单把 API 密钥的用量。keyID 必须为正；
// 管理端在调用前已经校验参数，这里的防御保证其他调用方不会把 0 误当成全量。
func (m *Meter) ReportForKey(ctx context.Context, from, to time.Time, keyID int64) (*Report, error) {
	if keyID <= 0 {
		return nil, fmt.Errorf("usage: key_id 必须为正整数")
	}
	return m.report(ctx, from, to, keyID)
}

// report 的 keyID=0 表示全量，正数表示只把该 Key 的行并入报表。Keys 始终从
// 过滤前的区间行构造，因此单 Key 视图仍能直接切换到同区间的其他密钥。
func (m *Meter) report(ctx context.Context, from, to time.Time, keyID int64) (*Report, error) {
	fromBucket := floorDiv(from.Unix(), 3600)
	toBucket := ceilDiv(to.Unix(), 3600)

	rows, err := m.st.QueryUsageRange(ctx, fromBucket, toBucket)
	if err != nil {
		return nil, err
	}

	rep := &Report{From: from, To: to}
	byKey := newDimAgg()
	byModel := newDimAgg()
	byUpstream := newDimAgg()
	byEntry := newDimAgg()
	byKind := newDimAgg()
	keyRefs := newKeyRefAgg()
	cursorModels, otherModels := make(map[string]bool), make(map[string]bool)
	days := make(map[string]*Summary)

	// 日标签按小时桶备忘：行按桶有序到达（内存增量也按桶聚过），同一桶的
	// 几十行只格式化一次日期——30 天报表是数千行 × 每行一次 time.Format 的
	// 差别。
	lastBucket, lastDay := int64(-1), ""
	consume := func(d store.UsageDelta) {
		// cursor_agent 里只有 Run/RunSSE 是模型消费；辅助 RPC 不应进入
		// 总计、任何维度或按日数据。这里只过滤报表，不改写原始账本。
		model, include := reportModelDimension(d.Entry, d.ModelName)
		if !include {
			return
		}
		d.ModelName = model
		keyRefs.add(d.KeyID, d.KeyDisplay)
		if keyID != 0 && d.KeyID != keyID {
			return
		}
		rep.Total.add(d)
		if d.Entry == EntryCursorAgent {
			cursorModels[d.ModelName] = true
		} else {
			otherModels[d.ModelName] = true
		}
		byKey.addByID(d.KeyID, d.KeyDisplay, d)
		byModel.add(d.ModelName, d)
		byUpstream.add(d.UpstreamName, d)
		byEntry.add(reportEntryDimension(d.Entry), d)
		byKind.add(d.Kind, d)
		if d.BucketHour != lastBucket {
			lastBucket = d.BucketHour
			lastDay = time.Unix(d.BucketHour*3600, 0).In(m.loc).Format(dayLayout)
		}
		day := lastDay
		s := days[day]
		if s == nil {
			s = &Summary{}
			days[day] = s
		}
		s.add(d)
	}
	for _, r := range rows {
		consume(store.UsageDelta(r))
	}
	// 叠加尚未冲刷的内存增量：同一区间、同一归属过滤条件。
	for _, d := range m.pendingDeltas(fromBucket, toBucket) {
		consume(d)
	}

	rep.ByKey = byKey.rows()
	rep.ByModel = byModel.rows()
	m.annotatePricing(ctx, rep.ByModel)
	if len(cursorModels) > 0 {
		var prices map[string]Pricing
		if m.cursorPrices != nil {
			prices = m.cursorPrices.CursorModelPrices(ctx)
		}
		for i := range rep.ByModel {
			row := &rep.ByModel[i]
			if !cursorModels[row.Key] || row.Key == CursorAgentModelDimension {
				continue
			}
			priced := PricedFor(prices[row.Key], store.ModelKindText, "")
			if otherModels[row.Key] {
				priced = priced && row.Priced != nil && *row.Priced
			}
			row.Priced = &priced
		}
	}
	rep.ByEntry = byEntry.rows()
	rep.ByKind = byKind.rows()
	rep.ByUpstream = byUpstream.rows()
	rep.Keys = keyRefs.rows()
	m.annotateKeys(ctx, rep)
	rep.Days = daySeries(days)
	rep.Recent = m.recentForKey(keyID)
	return rep, nil
}

// annotateKeys 把 API 密钥当前标签补到按密钥汇总与选择项，并把当前存在但本
// 区间没有用量的密钥追加到选择项。读取失败只让标签与闲置项缺席，不影响账本
// 主读数；与 annotatePricing 的降级方向一致。
func (m *Meter) annotateKeys(ctx context.Context, rep *Report) {
	keys, err := m.st.ListAPIKeys(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("读取 API 密钥标签失败，用量页本次只显示脱敏 Key", "err", err.Error())
		}
		return
	}
	rowsByID := make(map[int64]*DimRow, len(rep.ByKey))
	for i := range rep.ByKey {
		rowsByID[rep.ByKey[i].ID] = &rep.ByKey[i]
	}
	// 这里只存下标，不存 &rep.Keys[i]：下面会 append 当前存在但本区间没有
	// 用量的 Key，切片扩容后旧元素指针会指向废弃的 backing array，后续标签
	// 看似写入成功、最终 JSON 却仍是空值。下标在扩容前后保持稳定。
	refsByID := make(map[int64]int, len(rep.Keys))
	for i := range rep.Keys {
		refsByID[rep.Keys[i].ID] = i
	}
	for _, k := range keys {
		display := k.DisplayPrefix + "…" + k.DisplayLast4
		if row := rowsByID[k.ID]; row != nil {
			row.Label = k.Label
			if row.Key == "" {
				row.Key = display
			}
		}
		if i, ok := refsByID[k.ID]; ok {
			rep.Keys[i].Label = k.Label
			if rep.Keys[i].Display == "" {
				rep.Keys[i].Display = display
			}
			continue
		}
		rep.Keys = append(rep.Keys, KeyRef{ID: k.ID, Display: display, Label: k.Label})
		refsByID[k.ID] = len(rep.Keys) - 1
	}
	sort.Slice(rep.Keys, func(i, j int) bool {
		left, right := rep.Keys[i], rep.Keys[j]
		if left.Label != right.Label {
			if left.Label == "" {
				return false
			}
			if right.Label == "" {
				return true
			}
			return left.Label < right.Label
		}
		if left.Display != right.Display {
			return left.Display < right.Display
		}
		return left.ID < right.ID
	})
}

// annotatePricing 给按模型维度的行标上「这个模型录没录目录价」。
//
// 读的是目录（一次 name → pricing 的全表窄读数），**不从金额反推**——反推的
// 两个方向都在收口走查里真实复现过：已定价模型在只有 count_tokens 流量的区间
// 里金额为 0（被误标未定价），未定价模型因为窗口内另有一笔已定价调用而金额
// 大于 0（漏标）。目录里没有的名字（客户端拼错的、并进哨兵的）保持 nil：
// 那不是"未定价"，是压根没有这个模型。
//
// 失败只降级不报错：徽章没了页面照常，而为一个提示把整张用量报表变成 500
// 是本末倒置。区间读数本身失败时 Report 早已提前返回，走不到这里。
func (m *Meter) annotatePricing(ctx context.Context, rows []DimRow) {
	if len(rows) == 0 {
		return
	}
	catalog, err := m.st.ListModelPricing(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("读取模型目录价失败，用量页本次不显示「未定价」标记", "err", err.Error())
		}
		return
	}
	for i := range rows {
		raw, ok := catalog[rows[i].Key]
		if !ok {
			continue
		}
		priced := raw != ""
		rows[i].Priced = &priced
	}
}

// pendingDeltas 快照尚未落盘的增量（按桶区间与归属过滤）。
//
// 已知窗口，不修：flush 把 map 换出来之后是在**锁外**写库的，恰好落在这中间
// 的一次读数会既看不到内存里的（已换出）也看不到库里的（未提交），短暂少算
// 一个冲刷窗口的量。
//
// **别用「先读内存后读库」去修它**（外审提过，这条是反例）：那样一来，读内存
// 时批次还在 map 里、读库时它已提交，同一笔账被数两遍——把少算换成了多算，
// 而多算的金额会让用量页显示「超预算」这种更糟的假象。真要根治得留一份
// 「正在冲刷中」的影子副本，为一次亚秒级的展示误差多养一份状态，不值。
//
// 金额只少不多，与本包处处的「宁可少记」同向，且下一次刷新就对上了；
// 预算计数器走的是另一条路（内存直加），完全不受这个窗口影响。
func (m *Meter) pendingDeltas(fromBucket, toBucket int64) []store.UsageDelta {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.UsageDelta, 0, len(m.deltas))
	for k, d := range m.deltas {
		if k.bucket < fromBucket || k.bucket >= toBucket {
			continue
		}
		out = append(out, *d)
	}
	return out
}

// recent 取明细环（新的在前）。
//
// 从写指针往回走：未满时 ringNext == len(ring)，退化成 n-1…0；满环时最新的
// 一条在 ringNext-1，绕回取满一圈。
func (m *Meter) recent() []Event { return m.recentForKey(0) }

// recentForKey 的 keyID=0 表示完整明细环，正数只保留该密钥。过滤发生在复制
// 时，环内仍只保存一份事件，不为每把密钥另养缓冲。
func (m *Meter) recentForKey(keyID int64) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.ring)
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		ev := m.ring[(m.ringNext-1-i+n)%n]
		if keyID != 0 && ev.KeyID != keyID {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// dimAgg 是一个维度的累加器，按分组键聚合。
type dimAgg struct {
	order []string
	rowsM map[string]*DimRow
}

func newDimAgg() *dimAgg { return &dimAgg{rowsM: make(map[string]*DimRow)} }

// keyRefAgg 从过滤前的区间行收集可选密钥。按 ID 去重，展示串取最后一个非空
// 快照，与按密钥维度的分组规则一致。
type keyRefAgg struct {
	order []int64
	refs  map[int64]*KeyRef
}

func newKeyRefAgg() *keyRefAgg { return &keyRefAgg{refs: make(map[int64]*KeyRef)} }

func (a *keyRefAgg) add(id int64, display string) {
	if id <= 0 {
		return
	}
	ref := a.refs[id]
	if ref == nil {
		ref = &KeyRef{ID: id}
		a.refs[id] = ref
		a.order = append(a.order, id)
	}
	if display != "" {
		ref.Display = display
	}
}

func (a *keyRefAgg) rows() []KeyRef {
	out := make([]KeyRef, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, *a.refs[id])
	}
	return out
}

// add 按取值本身分组（模型 / 上游 / 入口 / kind）。
//
// **空取值也进表**（key 为空串）：准入被拒的行没有上游名，未挂来源的模型
// 同样没有——把它们静默丢掉，各维度的合计就对不上总计了。展示层按空串渲染
// 成「—」即可。
func (a *dimAgg) add(key string, d store.UsageDelta) { a.group(key, 0, key, d) }

// addByID 按主键分组、名字只作展示（用户 / 密钥两个维度）。
//
// 刻意不按名字分组：同一个主体的历史行里，名字快照未必处处都在（早期行、
// 或者调用方漏填），按名字分组会把同一个人劈成两行；反过来，用户被删后同名
// 新建又会把两段历史并成一行。按 id 分组两种失真都没有——名字取**最后一个
// 非空**的（同 usage_hourly 非键快照列「取最后写入者」的口径）。
func (a *dimAgg) addByID(id int64, label string, d store.UsageDelta) {
	a.group("#"+strconv.FormatInt(id, 10), id, label, d)
}

func (a *dimAgg) group(groupKey string, id int64, label string, d store.UsageDelta) {
	r := a.rowsM[groupKey]
	if r == nil {
		r = &DimRow{ID: id, Key: label}
		a.rowsM[groupKey] = r
		a.order = append(a.order, groupKey)
	}
	if label != "" {
		r.Key = label
	}
	r.add(d)
}

// rows 输出维度行：金额降序，同额按取值升序（确定性排序，翻页与截图可复现）。
func (a *dimAgg) rows() []DimRow {
	out := make([]DimRow, 0, len(a.order))
	for _, k := range a.order {
		out = append(out, *a.rowsM[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostMicro != out[j].CostMicro {
			return out[i].CostMicro > out[j].CostMicro
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// daySeries 把按日合计排成升序序列。
func daySeries(m map[string]*Summary) []DayPoint {
	out := make([]DayPoint, 0, len(m))
	for day, s := range m {
		out = append(out, DayPoint{Day: day, Summary: *s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// ceilDiv 是向上取整的整除（b > 0）。
func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a > 0) == (b > 0) {
		q++
	}
	return q
}
