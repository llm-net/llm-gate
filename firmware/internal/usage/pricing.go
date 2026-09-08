package usage

// pricing.go 是**全仓唯一的「用量 → 微元」换算点**（方案 §2.5/§2.6）：目录价
// 的解析、按形态选公式、以及全部整数算术。别处要金额一律调 Cost。
//
// 形态定字段：models.pricing 是一张扁平的 `字段名 → 整数微元` 表，一个模型只
// 需带它实际用得上的键。字段名**全局唯一（带厂商前缀）**是有意的——一个模型
// 可以同时挂 ark 与 minimax 两条来源，两家的价签必须能共存在同一张表里而不
// 互相覆盖；解析时也就不必先知道是谁在服务这次请求。
//
// 为什么形态写死在代码里、只有数值可配（照 (type, protocol) 内置端点表的
// 先例）：形态是上游的产品事实（方舟按视频 token 计、H3 按秒计），不是运营
// 参数；数值才是。上游改计费形态是一次代码变更，改价只是管理台里改个数。
//
// 取整口径：每个分量各自 `量 × 单价 ÷ 除数` **向下取整**后求和（不是先求和
// 再取整）。这条与方案 §2.5 逐字一致，别顺手"优化"成后者——两者相差最多几个
// 微元，但口径一变，历史账与新账就对不上了。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// MicroPerYuan 是 1 元的微元数。金额一律 int64 微元。
const MicroPerYuan int64 = 1_000_000

// tokensPerPriceUnit 是「元/百万 token」价签的除数：价格字段的单位是
// 微元 / 百万 token，所以 金额 = token 数 × 单价 ÷ 10⁶。
const tokensPerPriceUnit int64 = 1_000_000

// minimaxFreeImages 是 MiniMax H3 免费的输入图片张数，超出部分按张附加计费
// （文档 §9：「5 张以内免费」）。这是厂商的计费形态，不是可配参数。
const minimaxFreeImages int64 = 5

// MaxCostMicro 是**单笔样本**金额的上限：10¹⁵ 微元 = 10 亿元。
//
// 它不是业务限制，是算术护栏。store.MaxPricingMicro 只管住单个价格字段
// （≤ 10¹² 微元），而金额 = 量 × 单价：一个越界的厂商 usage 配上一个手滑的
// 价格，乘积能冲出 int64。cost_micro 的入库是 `列 = 列 + excluded.列`，
// 一旦某次相加溢出，SQLite 会把整列悄悄转成 REAL，之后整段区间的读数都扫不
// 进 int64 而整体报错——用量页从此打不开。把单笔钳在 10¹⁵，即便连加十万笔
// 也还在 int64 的三分之一以内。
const MaxCostMicro int64 = 1_000_000_000_000_000

// ---- 价格字段名（models.pricing 的键） ----
const (
	// 文本四价（各文本入口共用字段），单位 微元 / 百万 token。
	// FieldCacheRead 缺省按 FieldIn 计（缓存命中没单独标价就按输入价）。
	FieldIn         = "in"
	FieldOut        = "out"
	FieldCacheRead  = "cache_read"
	FieldCacheWrite = "cache_write"

	// 方舟 Seedance 视频，单位 微元 / 百万 token；计价量是任务 usage 的
	// completion_tokens（官方明示的对账依据）。两档**二选一**，判据是任务行
	// 固化的 has_video_input。**哪一档更贵不由这里假定**：Seedance 2.0 的
	// 公开价目里含视频输入（视频编辑/续写）反而更便宜，与直觉相反——所以
	// 代码只负责按判据选字段，档位高低完全交给管理员录的数。
	FieldArkVideoToken    = "ark_video_token"     // 不含参考视频输入
	FieldArkVideoTokenRef = "ark_video_token_ref" // 含参考视频输入

	// MiniMax H3 视频，秒价单位 微元 / 秒，按**输出**分辨率档二选一；
	// 输入秒（参考视频）同样按输出档单价计（文档 §9 明写，别按输入分辨率算）。
	// 图片附加价单位 微元 / 张，只对超出 minimaxFreeImages 的部分计。
	FieldMinimaxSec768p    = "minimax_video_sec_768p"
	FieldMinimaxSec2K      = "minimax_video_sec_2k"
	FieldMinimaxImageExtra = "minimax_video_image_extra"

	// MiniMax H3 的 Context-IR（提示词增强）任务：**同一个上游族的第二种
	// 计费形态**。它不出视频，官方按 token 计价（输入/输出两个单价，官方
	// 文档 §9；差价约 4 倍，合成一个字段必错），厂商 usage 相应回
	// prompt_tokens / completion_tokens 而不是秒数。单位 微元 / 百万 token。
	//
	// 补这两个字段是 Phase 3.5 点出的账面窟窿：没有它们，一个受支持的任务
	// 类型恒按 0 元 + estimated 记账——账上看得见「有这么一笔」，却永远收不
	// 到钱，而管理台连补价的地方都没有。
	FieldMinimaxContextIRIn  = "minimax_context_ir_in"
	FieldMinimaxContextIROut = "minimax_context_ir_out"

	// 方舟 Seedream 图片。两项是**相加**关系而非二选一：按价签只配其一，
	// 另一项留空即 0。之所以两项都留着——迭代 8 Phase 5 实测该接口的 usage
	// 同时回 generated_images 与 output_tokens，而厂商的价签口径（按张还是
	// 按 token）在本机无真值 Key 时无法确证；两项并存让管理员照价签直接录，
	// 不必先猜我们内部选了哪一种。
	FieldArkImageEach  = "ark_image_each"  // 微元 / 张（× usage.generated_images）
	FieldArkImageToken = "ark_image_token" // 微元 / 百万 token（× usage.output_tokens）

)

// 各 kind 允许出现的字段集（管理层按 kind 校验形态字段集时消费——词汇只此
// 一份，别在 admin 里另抄一遍）。切片顺序即管理台表单的建议排列顺序。
var fieldsByKind = map[string][]string{
	store.ModelKindText: {FieldIn, FieldOut, FieldCacheRead, FieldCacheWrite},
	store.ModelKindVideo: {
		FieldArkVideoToken, FieldArkVideoTokenRef,
		FieldMinimaxSec768p, FieldMinimaxSec2K, FieldMinimaxImageExtra,
		FieldMinimaxContextIRIn, FieldMinimaxContextIROut,
	},
	store.ModelKindImage: {FieldArkImageEach, FieldArkImageToken},
}

// FieldsFor 返回某 kind 的合法价格字段集（未知 kind 返回 nil）。返回的是
// 副本，调用方改不动内部表。
func FieldsFor(kind string) []string {
	src := fieldsByKind[kind]
	if len(src) == 0 {
		return nil
	}
	return append([]string(nil), src...)
}

// ErrInvalidPricing 表示目录价原文不合形态。它只可能来自被手工改过的库
// （管理端点写入前已由 store.validatePricing 挡过一道），所以调用方的正确
// 反应是「按未定价处理 + 告警」，不是 500。错误文本**只描述形态、不回显
// 字段名与值**：那是库里的内容，而这条错误会进日志。
var ErrInvalidPricing = errors.New("usage: 模型目录价不合法")

// errInvalidPricingf 拼一条带哨兵的形态错误。
func errInvalidPricingf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPricing, fmt.Sprintf(format, args...))
}

// Pricing 是解析后的目录价：字段名 → 整数微元。nil 或空表示**未定价**
// （照常转发、金额记 0、管理台挂警示徽章）；与显式 0 价（定价为免费，不警示）
// 是两个状态——前者 len == 0，后者有键值为 0。
type Pricing map[string]int64

// Priced 报告这张表是否定过价。
func (p Pricing) Priced() bool { return len(p) > 0 }

// 计价用的**上游族**：AIGC 的计价形态由厂商决定，而同一家厂商可以有不止一
// 个上游 type。方舟按量（ark）与方舟订阅（ark_plan）走的是同一套官方接口、
// 回同一份 usage 形态，只是端点根与结算通道不同——形态判定必须把两者视为
// 同一族，否则订阅那条来源的每一笔 AIGC 消费都会「认不出形态 → 0 元 +
// estimated」，账面上只剩一条 WARN。族名取厂商，不取 type。
const (
	familyArk     = "ark"
	familyMinimax = "minimax"
)

// aigcFamily 把上游 type 归到计价族；不服务 AIGC 的类型返回空串。文本请求
// 根本走不到这个函数（applicablePriceFields / Cost 都先按 kind 分流）。
func aigcFamily(upstreamType string) string {
	switch upstreamType {
	case config.UpstreamArk, config.UpstreamArkPlan:
		return familyArk
	case config.UpstreamMinimax:
		return familyMinimax
	}
	return ""
}

// applicablePriceFields 返回某（kind, 上游族）计价时**真正会读到**的字段。
// 文本面与上游族无关（两家 cache 口径的差异在公式里，不在字段名上）。
func applicablePriceFields(kind, upstreamType string) []string {
	if kind == store.ModelKindText {
		return []string{FieldIn, FieldOut, FieldCacheRead, FieldCacheWrite}
	}
	switch fam := aigcFamily(upstreamType); {
	case kind == store.ModelKindVideo && fam == familyArk:
		return []string{FieldArkVideoToken, FieldArkVideoTokenRef}
	case kind == store.ModelKindVideo && fam == familyMinimax:
		return []string{FieldMinimaxSec768p, FieldMinimaxSec2K, FieldMinimaxImageExtra,
			FieldMinimaxContextIRIn, FieldMinimaxContextIROut}
	case kind == store.ModelKindImage && fam == familyArk:
		return []string{FieldArkImageEach, FieldArkImageToken}
	}
	return nil
}

// PricedFor 报告这张价目表**对这一家上游**是否真的定过价（至少有一个用得上
// 的字段）。
//
// 为什么 Priced() 不够：字段名带厂商前缀是有意的——一个模型可以同时挂 ark 与
// minimax 两条来源。管理员只填了其中一家时 Priced() 仍为真，于是另一家服务的
// 那些消费会以 0 元、**非估算**入账；对异步任务更糟，一次性清算之后那笔钱
// 永久消失，而模型页因为「有价」也不挂警示徽章，账面上看不出任何异常。
// 这与 Phase 3.5 修掉的「usage 形态错配」是同一个失效类，只是从价签这一侧发生。
//
// 空表（未定价）返回 false，但调用方要先用 Priced() 把两者分开：未定价的 0 元
// 是**已知**的（政策如此，管理台有警示徽章），而「有价却没这家的字段」的 0 元
// 是**不知道**，该打估算标并告警。
func PricedFor(p Pricing, kind, upstreamType string) bool {
	for _, f := range applicablePriceFields(kind, upstreamType) {
		if _, ok := p[f]; ok {
			return true
		}
	}
	return false
}

// ParsePricing 解析 models.pricing 原文。空串 → nil, nil（未定价）。
//
// 入库前 store.validatePricing 已保证「非空扁平 JSON 对象 + 非负整数 +
// ≤ MaxPricingMicro」，这里仍完整校验一遍：库文件是可以被手工改的，而计价是
// 金额的唯一来源——它不该假设自己的输入干净。任一字段不合形态即整张表作废
// 并报错（调用方按「未定价」处理并告警），不做逐字段挑拣：半张能用的价目表
// 算出来的账比记 0 更难发现。
func ParsePricing(raw string) (Pricing, error) {
	if raw == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	// 目标是 map[string]any 而非 map[string]json.Number：后者的底层类型是
	// string，会把带引号的 "1000" 也照收（同 store.validatePricing 的理由）。
	var raws map[string]any
	if err := dec.Decode(&raws); err != nil {
		return nil, errInvalidPricingf("目录价不是 JSON 对象: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		// Token() 而非 dec.More()：后者对多余的 } / ] 会放行。
		return nil, errInvalidPricingf("目录价 JSON 后有多余内容")
	}
	if len(raws) == 0 {
		return nil, nil
	}
	out := make(Pricing, len(raws))
	for field, v := range raws {
		num, ok := v.(json.Number)
		if !ok {
			return nil, errInvalidPricingf("目录价字段值不是整数微元")
		}
		n, err := num.Int64()
		if err != nil {
			return nil, errInvalidPricingf("目录价字段值不是 int64 内的整数")
		}
		if n < 0 || n > store.MaxPricingMicro {
			return nil, errInvalidPricingf("目录价字段值越界（0–%d 微元）", store.MaxPricingMicro)
		}
		out[field] = n
	}
	return out, nil
}

// Measure 是「这次消费用掉了多少东西」的形态化描述。哪些字段有效由
// Kind + UpstreamType 决定：文本看 Tokens，视频/图片看 TaskUsage 与两个
// 计费特征。构造它的是 Sample.measure()，读它的只有 Cost。
type Measure struct {
	Kind         string // store.ModelKind*
	Entry        string // Entry*（文本两口径的分流依据）
	UpstreamType string // config.Upstream*（视频/图片选形态的依据）

	Tokens    Tokens
	TaskUsage TaskUsage

	// HasVideoInput 是 Seedance 的档位判据（含参考视频输入 → 高档单价）；
	// Resolution 是 H3 秒价的档位判据（"768P" / "2K"，客户端原样提交的值）。
	HasVideoInput bool
	Resolution    string
}

// TaskUsage 是厂商 usage 原文解析出的任务面用量。三家的字段集**刻意不归一**
// （aigc_tasks.usage_json 存的就是原文），本结构体只是把用得上的数字拆出来。
type TaskUsage struct {
	// 方舟 Seedance（视频）：官方对账依据。
	CompletionTokens int64
	// MiniMax H3（视频）：官方记账依据。InputSeconds 不是回声——参考视频按
	// 输入时长真金白银计费，漏了它会严重低估（文档 §9 的警告）。
	OutputSeconds int64
	InputSeconds  int64
	InputImages   int64
	// MiniMax H3 的 Context-IR 任务按 token 计费（PromptTokens 走输入价，
	// CompletionTokens 走输出价——后者与方舟 Seedance 复用同一个 usage 键，
	// 落位相同、计价形态由上游族区分）。
	PromptTokens int64
	// 方舟 Seedream（图片，同步入口）。
	GeneratedImages int64
	OutputTokens    int64
}

// 厂商 usage 原文里认得的键，位掩码——形态校验要知道**哪一个**键真解析出了
// 值，光有「解析出了某个键」不够（见 ParseTaskUsage）。
const (
	tuCompletionTokens = 1 << iota
	tuOutputSeconds
	tuInputSeconds
	tuInputImages
	tuGeneratedImages
	tuOutputTokens
	tuPromptTokens
)

// requiredTaskUsage 是每个（kind, 上游族）形态的计价量所在的键集合：解析结果
// 里**至少命中其中一个**才算「厂商 usage 可得」。表里没有的组合（上游族未知、
// 文本形态、本迭代不服务的组合）返回 0，即一律判不可得。
//
// 为什么要有这张表：三家的键集刻意不归一，而 aigc_tasks 的行只钉住 upstream_id
// ——同一份原文放到别家的价目表下，认得的键一个都用不上却仍「认得」。少了这道
// 形态闸门，一次错配就是「解析出一份空测量值 → 0 元 + 非估算 → 一次性清算」，
// 钱永久消失且账面上看不出任何异常。
func requiredTaskUsage(kind, upstreamType string) int {
	switch fam := aigcFamily(upstreamType); {
	case kind == store.ModelKindVideo && fam == familyArk:
		return tuCompletionTokens
	case kind == store.ModelKindVideo && fam == familyMinimax:
		// 两种形态并列（秒形的视频生成 / token 形的 Context-IR）：任务行没有
		// task_type 列，usage 自己的形状就是唯一的判据，所以两套键都算「可得」，
		// 由 costVideo 再按形状选公式。
		//
		// token 形只认 prompt_tokens，**刻意不认 completion_tokens**：后者正是
		// 方舟 Seedance 的计价量（{"completion_tokens":…,"total_tokens":…}），
		// 认了它，一份错配到 minimax 行上的方舟原文就会被判"可得"、按 Context-IR
		// 的价（多半没配 = 0 元）非估算地一次性清算掉——钱永久消失且账面无异常。
		// Context-IR 的官方 usage 三个键齐发（total/prompt/completion），
		// prompt_tokens 因此是它独有且必现的形态指纹。
		return tuOutputSeconds | tuInputSeconds | tuInputImages | tuPromptTokens
	case kind == store.ModelKindImage && fam == familyArk:
		return tuGeneratedImages | tuOutputTokens
	}
	return 0
}

// ParseTaskUsage 解析厂商 usage 原文并按 **kind + 上游族**校验形态。认得的键
// 各自落位，认不得的忽略；只有该形态计价真正要用的键解析出了值，才返回
// ok=true。否则调用方据此判「厂商 usage 不可得」，按 0 元 + estimated 记账
// 并告警，而不是悄悄记一笔 0。
//
// 负值与非整数按 0 计（厂商不该报，报了也不是计价该纠结的事）。
func ParseTaskUsage(raw, kind, upstreamType string) (TaskUsage, bool) {
	var u TaskUsage
	if raw == "" {
		return u, false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return u, false
	}
	seen := 0
	// seen 只在**真的解析出一个整数**时才置位，不是「键存在就算」：
	// 厂商回一个 {"output_seconds":"6"}（字符串型数字）或
	// {"completion_tokens":null}，键存在但值取不出来——按「键存在即认得」
	// 判定，任务会以 0 元 + 非估算被一次性清算掉，那笔钱从账上永久消失且
	// 没有任何告警。两家的真实 wire 形态至今未经真值验证（AGENTS.md 记
	// 「Real-vendor verification is still pending」），这个分支是活的。
	pick := func(key string, bit int, dst *int64) {
		n, ok := m[key].(json.Number)
		if !ok {
			return
		}
		i, err := n.Int64()
		if err != nil {
			return
		}
		seen |= bit
		*dst = nonNeg(i)
	}
	pick("completion_tokens", tuCompletionTokens, &u.CompletionTokens)
	pick("prompt_tokens", tuPromptTokens, &u.PromptTokens)
	pick("output_seconds", tuOutputSeconds, &u.OutputSeconds)
	pick("input_seconds", tuInputSeconds, &u.InputSeconds)
	pick("input_image_count", tuInputImages, &u.InputImages)
	pick("generated_images", tuGeneratedImages, &u.GeneratedImages)
	pick("output_tokens", tuOutputTokens, &u.OutputTokens)

	req := requiredTaskUsage(kind, upstreamType)
	return u, req != 0 && seen&req != 0
}

// Cost 把一次消费折成金额（int64 微元）。**全仓唯一的换算点。**
//
// 未定价（p 为空）恒返回 0——照常转发、照常记用量，只是这笔消费对预算不可见
// （方案 §5.1 明确接受的代价，由管理台的「未定价」警示徽章补位）。
// 认不出的 kind / 上游类型同样返回 0：没有价签就没有账，绝不瞎猜一个数。
func Cost(p Pricing, m Measure) int64 {
	if !p.Priced() {
		return 0
	}
	switch m.Kind {
	case store.ModelKindText:
		return clampCost(costText(p, m))
	case store.ModelKindVideo:
		return clampCost(costVideo(p, m))
	case store.ModelKindImage:
		return clampCost(costImage(p, m))
	}
	return 0
}

// costText 折算文本面。两家 cache 口径**相反**，公式因此按 entry 分流
// （方案 §2.5，逐字实现，别合并成一条）：
//
//	chat / responses（OpenAI 形） (prompt − cache_read)⁺ × P_in + cache_read × P_cache + completion × P_out
//	messages / claude_code（Anthropic 形） input × P_in + cache_read × P_cache + cache_creation × P_in + output × P_out
//
// chat 侧**没有** cache_creation 项不是漏写：那一口径下缓存写就是普通输入，
// 已经计在 prompt_tokens 里了，再加一次就是双重计费。
//
// 两个 Responses 入口（EntryResponses 目录面 / EntryResponsesAgents 订阅代理）
// 都走 OpenAI 形那一支：input_tokens 同样已含 input_tokens_details.cached_tokens。
// 分流写成「是不是 messages」而不是逐个入口枚举，正是为了让同形态的新入口
// 不必改这里。
func costText(p Pricing, m Measure) int64 {
	pin, pout := p[FieldIn], p[FieldOut]
	pcache, ok := p[FieldCacheRead]
	if !ok {
		pcache = pin
	}
	pwrite, ok := p[FieldCacheWrite]
	if !ok {
		pwrite = pin
	}
	t := m.Tokens
	var total int64
	if m.Entry == EntryMessages || m.Entry == EntryClaudeCode {
		total = addSat(total, perMillion(t.Prompt, pin))
		total = addSat(total, perMillion(t.CacheRead, pcache))
		total = addSat(total, perMillion(t.CacheWrite, pwrite))
	} else if m.Entry == EntryCursorAgent {
		// Cursor's input includes both cache components. Each component is
		// charged exactly once, with integer arithmetic through perMillion.
		total = addSat(total, perMillion(nonNeg(nonNeg(t.Prompt-t.CacheRead)-t.CacheWrite), pin))
		total = addSat(total, perMillion(t.CacheRead, pcache))
		total = addSat(total, perMillion(t.CacheWrite, pwrite))
	} else {
		// (·)⁺：上游把重叠计数报超时 prompt − cache_read 会为负，钳零。
		total = addSat(total, perMillion(nonNeg(t.Prompt-t.CacheRead), pin))
		total = addSat(total, perMillion(t.CacheRead, pcache))
	}
	return addSat(total, perMillion(t.Completion, pout))
}

// costVideo 折算视频任务面。形态按上游族分流；量全部来自厂商 usage，
// 档位判据来自任务行固化的计费特征。
func costVideo(p Pricing, m Measure) int64 {
	switch aigcFamily(m.UpstreamType) {
	case familyArk:
		// Seedance：单一计价量（completion_tokens）× 两档之一的单价。
		return perMillion(m.TaskUsage.CompletionTokens, arkVideoTokenPrice(p, m.HasVideoInput))
	case familyMinimax:
		// H3 有**两种**计费形态，判据是厂商 usage 自己的形状——任务行没存
		// task_type，而形状是官方契约的一部分（视频任务回秒数，Context-IR
		// 回 token）。秒形优先判：视频生成是主路径，且它的 usage 里不会出现
		// token 键。
		tu := m.TaskUsage
		if tu.OutputSeconds > 0 || tu.InputSeconds > 0 || tu.InputImages > 0 {
			// (输出秒 + 输入秒) × 输出档秒价 + 超出免费额度的图片 × 附加价。
			sec := minimaxSecondPrice(p, m.Resolution)
			total := addSat(mulSat(tu.OutputSeconds, sec), mulSat(tu.InputSeconds, sec))
			if extra := tu.InputImages - minimaxFreeImages; extra > 0 {
				total = addSat(total, mulSat(extra, p[FieldMinimaxImageExtra]))
			}
			return total
		}
		// Context-IR（提示词增强，不出视频）：输入/输出两个单价各算各的。
		return addSat(perMillion(tu.PromptTokens, p[FieldMinimaxContextIRIn]),
			perMillion(tu.CompletionTokens, p[FieldMinimaxContextIROut]))
	}
	return 0
}

// arkVideoTokenPrice 按「是否含参考视频输入」选 Seedance 的档位单价。哪一档
// 数值更大不作假定（见字段注释）——本函数只做选择，不做比较。
//
// 只配了一档时**回退到另一档**，与 minimaxSecondPrice 同一理由：半张价目表下
// `Priced()` 仍是 true，模型页不会挂「未定价」徽章，于是「含参考视频的任务恒记
// 0 元」既没有账、也没有警示——整档消费从账上消失，而管理员看不到任何信号。
// 回退偏高是看得见的（用量页会露出来）。两档都缺就是真未定价，照旧记 0
// （Cost 的 Priced 闸门另有兜底）。
//
// 回退的判据是**键在不在**，不是值等不等于 0：把某一档明确标成免费（0 元）
// 是一个已配置状态，照记 0；只有压根没配才回退（Phase 2 的「空串 = 未定价」
// 口径延伸到单个字段——Phase 3.5 加固）。
//
// 这是运行期的兜底，不是许可：管理端点（Phase 4）仍应要求 Seedance 的两档
// 成对录入——回退价终究不是那一档的真价。
func arkVideoTokenPrice(p Pricing, hasVideoInput bool) int64 {
	if hasVideoInput {
		return pickPrice(p, FieldArkVideoTokenRef, FieldArkVideoToken)
	}
	return pickPrice(p, FieldArkVideoToken, FieldArkVideoTokenRef)
}

// minimaxSecondPrice 按**输出**分辨率选秒价档。
//
// 认不出的档位取**已配置档位里的最高价**，不取 0：H3 的 resolution 是必填
// 顶层参数，出现新值只可能是厂商加了新档——那时按最高已知档记账会偏高、能被
// 用量页看出来并驱动补配，而静默记 0 会让整档消费从账上消失。认得的档没配价
// 同样回退（半张价目表，与 arkVideoTokenPrice 同一处置）。
func minimaxSecondPrice(p Pricing, resolution string) int64 {
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "768P":
		return pickPrice(p, FieldMinimaxSec768p, FieldMinimaxSec2K)
	case "2K":
		return pickPrice(p, FieldMinimaxSec2K, FieldMinimaxSec768p)
	}
	return maxPrice(p, FieldMinimaxSec768p, FieldMinimaxSec2K)
}

// pickPrice 取 field 的单价；**只有键不存在**才回退到 fallbacks 里已配置的
// 最高价。显式配成 0（定价为免费）是已配置状态，照记 0——「免费」与「未配」
// 是两个状态，混为一谈会让一档免费价被悄悄按另一档收费。
func pickPrice(p Pricing, field string, fallbacks ...string) int64 {
	if v, ok := p[field]; ok {
		return v
	}
	return maxPrice(p, fallbacks...)
}

// maxPrice 取给定字段里**已配置**的最高价（都没配得 0）。
func maxPrice(p Pricing, fields ...string) int64 {
	var best int64
	for _, f := range fields {
		if v, ok := p[f]; ok && v > best {
			best = v
		}
	}
	return best
}

// costImage 折算同步出图（方舟 Seedream）。两个价格字段是**相加**关系，
// 按价签只配其一即可（另一项为 0，那一项就不产生金额）。
//
// Grok Build 订阅侧的画图门（EntryImagineImage）没有上游族——它不经目录选路，
// 这里按入口而不是按族分流：只取 ark_image_each × 张数当**名义**金额（管理员
// 在目录建同名 image 行录价才会走到；没录价时 Priced() 为假，Cost 早已返回 0）。
func costImage(p Pricing, m Measure) int64 {
	if m.Entry == EntryImagineImage {
		return mulSat(m.TaskUsage.GeneratedImages, p[FieldArkImageEach])
	}
	if aigcFamily(m.UpstreamType) != familyArk {
		return 0
	}
	total := mulSat(m.TaskUsage.GeneratedImages, p[FieldArkImageEach])
	return addSat(total, perMillion(m.TaskUsage.OutputTokens, p[FieldArkImageToken]))
}

// ---- 整数算术（全程 int64，无浮点） ----

// perMillion 计算 `n × 单价 ÷ 10⁶` 并向下取整（「元/百万 token」形态的价签）。
func perMillion(n, price int64) int64 { return mulDivFloor(n, price, tokensPerPriceUnit) }

// mulDivFloor 计算 n × price ÷ div 并向下取整；任一因子 ≤ 0 得 0，
// 乘积超出 64 位时饱和到 MaxCostMicro（外层 clampCost 兜同一个上限）。
//
// 用 bits.Mul64 做 128 位中间积而不是先除后乘：先除会把小于除数的量整个抹掉
// （一次 300 token 的调用在「元/百万」价签下直接变 0 元），那是系统性漏账。
func mulDivFloor(n, price, div int64) int64 {
	if n <= 0 || price <= 0 || div <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(n), uint64(price))
	if hi >= uint64(div) { // 商超出 64 位
		return MaxCostMicro
	}
	q, _ := bits.Div64(hi, lo, uint64(div))
	if q > uint64(MaxCostMicro) {
		return MaxCostMicro
	}
	return int64(q)
}

// mulSat 计算 n × price（「元/秒」「元/张」形态的价签，没有除数），
// 溢出饱和到 MaxCostMicro。
func mulSat(n, price int64) int64 { return mulDivFloor(n, price, 1) }

// addSat 相加并饱和到 MaxCostMicro（两个加数都非负）。
func addSat(a, b int64) int64 {
	sum := a + b
	if sum < a || sum > MaxCostMicro { // 溢出或越界
		return MaxCostMicro
	}
	return sum
}

// clampCost 把单笔金额钳进 [0, MaxCostMicro]。
func clampCost(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > MaxCostMicro {
		return MaxCostMicro
	}
	return v
}
