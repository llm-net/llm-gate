package usage

// iteration-9 Phase 2 的可执行验收（表驱动）：计价公式。
//
// 形态与数值取自两家官方文档（docs-upstream/ark-seedance-video.md、
// minimax-h3-video.md）——**真实厂商 usage 原文样例并不存在**：迭代 8 Phase 4 的
// 真值联调受阻于本机无 configs/*.local.yaml，与迭代 8 的适配器同源同风险。
// 这里的 fixture 因此是「照文档构造的形态」而非「抓包回放」，Key 就位后要回来
// 用真值样例校准档位口径（尤其 Seedance 的最低 token 地板与 flex 折扣）。

import (
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// mustPricing 解析价目表，失败即终止。
func mustPricing(t *testing.T, raw string) Pricing {
	t.Helper()
	p, err := ParsePricing(raw)
	if err != nil {
		t.Fatalf("ParsePricing(%q): %v", raw, err)
	}
	return p
}

// 文本三价：两家相反的 cache 口径、负数钳零、整数向下截断、未定价 = 0、
// 显式 0 价 = 0（且与未定价是两个状态）。
func TestCostText(t *testing.T) {
	// 2 元/百万 输入、8 元/百万 输出、0.5 元/百万 缓存命中。
	const full = `{"in":2000000,"out":8000000,"cache_read":500000}`
	// 只有 in/out：cache_read 缺省按 in 计。
	const noCache = `{"in":2000000,"out":8000000}`

	tests := []struct {
		name    string
		pricing string
		entry   string
		tok     Tokens
		want    int64
	}{{
		name:    "chat：cache 命中已含在 prompt 内，先剥离再各计各价",
		pricing: full,
		entry:   EntryChat,
		tok:     Tokens{Prompt: 1_000_000, CacheRead: 400_000, Completion: 500_000},
		// (1000000-400000)*2 + 400000*0.5 + 500000*8 = 1.2e6 + 0.2e6 + 4e6 微元
		want: 1_200_000 + 200_000 + 4_000_000,
	}, {
		name:    "chat：cache_read 缺省按输入价计",
		pricing: noCache,
		entry:   EntryChat,
		tok:     Tokens{Prompt: 1_000_000, CacheRead: 400_000, Completion: 0},
		// 剥离后 600000*2 + 400000*2 = 恰好等于 1000000*2
		want: 2_000_000,
	}, {
		name:    "chat：cache_write 不参与计价（该口径下缓存写已计在 prompt 里）",
		pricing: full,
		entry:   EntryChat,
		tok:     Tokens{Prompt: 1_000_000, CacheWrite: 999_999_999, Completion: 0},
		want:    2_000_000,
	}, {
		name:    "chat：上游把重叠计数报超，(prompt−cache_read)⁺ 钳零而不是记负数",
		pricing: full,
		entry:   EntryChat,
		tok:     Tokens{Prompt: 100_000, CacheRead: 400_000, Completion: 0},
		// 输入分量钳零，只剩 cache_read 那一项 400000*0.5
		want: 200_000,
	}, {
		name:    "messages：cache 两项不含在 input 内，直接加总；cache_creation 按输入价",
		pricing: full,
		entry:   EntryMessages,
		tok:     Tokens{Prompt: 1_000_000, CacheRead: 400_000, CacheWrite: 100_000, Completion: 500_000},
		// 1e6*2 + 4e5*0.5 + 1e5*2 + 5e5*8
		want: 2_000_000 + 200_000 + 200_000 + 4_000_000,
	}, {
		name:    "claude_code：集中订阅代理仍按 Anthropic cache 口径计名义金额",
		pricing: full,
		entry:   EntryClaudeCode,
		tok:     Tokens{Prompt: 1_000_000, CacheRead: 400_000, CacheWrite: 100_000, Completion: 500_000},
		want:    2_000_000 + 200_000 + 200_000 + 4_000_000,
	}, {
		name:    "同一组 token 在两个入口下金额不同——两家口径相反，不可互换",
		pricing: full,
		entry:   EntryMessages,
		tok:     Tokens{Prompt: 1_000_000, CacheRead: 400_000, Completion: 0},
		// messages 侧不剥离：1e6*2 + 4e5*0.5 = 2.2e6（chat 侧同样输入是 1.4e6）
		want: 2_200_000,
	}, {
		name:    "分量各自向下截断后求和（不是求和后一次截断）",
		pricing: `{"in":3,"out":3}`,
		entry:   EntryChat,
		// 每分量 999999*3/1e6 = 2.999997 → 2；两分量各截断得 4，
		// 若先求和再截断则是 1999998*3/1e6 = 5.999994 → 5。
		tok:  Tokens{Prompt: 999_999, Completion: 999_999},
		want: 4,
	}, {
		name:    "未定价：照常记用量，金额恒 0",
		pricing: "",
		entry:   EntryChat,
		tok:     Tokens{Prompt: 1_000_000, Completion: 1_000_000},
		want:    0,
	}, {
		name:    "显式 0 价（定价为免费）同样是 0，但它是**已定价**状态",
		pricing: `{"in":0,"out":0}`,
		entry:   EntryChat,
		tok:     Tokens{Prompt: 1_000_000, Completion: 1_000_000},
		want:    0,
	}, {
		name:    "零用量零金额",
		pricing: full,
		entry:   EntryChat,
		tok:     Tokens{},
		want:    0,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Cost(mustPricing(t, tc.pricing), Measure{
				Kind: store.ModelKindText, Entry: tc.entry, UpstreamType: config.UpstreamDeepseek,
				Tokens: tc.tok,
			})
			if got != tc.want {
				t.Errorf("Cost = %d 微元, 期望 %d", got, tc.want)
			}
		})
	}
}

func TestClaudeCodeTokenTotalIncludesBothCacheKinds(t *testing.T) {
	tok := Tokens{Prompt: 11, Completion: 7, CacheRead: 3, CacheWrite: 2}
	if got := tok.Total(EntryClaudeCode); got != 23 {
		t.Errorf("claude_code total = %d，期望 23", got)
	}
}

// 「未定价」与「显式 0 价」必须是两个可区分的状态：前者要挂警示徽章，
// 后者不要。判据是 Pricing.Priced()。
func TestPricedDistinguishesUnpricedFromFree(t *testing.T) {
	if mustPricing(t, "").Priced() {
		t.Error("空串应为未定价")
	}
	if !mustPricing(t, `{"in":0,"out":0}`).Priced() {
		t.Error("显式 0 价应为已定价（定价为免费，不该挂未定价徽章）")
	}
}

// Seedance（方舟视频）：计价量恒是 usage.completion_tokens，档位按任务行
// 固化的 has_video_input 二选一。
func TestCostVideoSeedance(t *testing.T) {
	// 28 元/百万（无参考视频）/ 46 元/百万（含参考视频输入）。
	const pricing = `{"ark_video_token":28000000,"ark_video_token_ref":46000000}`
	usageJSON := `{"completion_tokens":246840,"total_tokens":246840}`

	tu, ok := ParseTaskUsage(usageJSON, store.ModelKindVideo, config.UpstreamArk)
	if !ok {
		t.Fatalf("ParseTaskUsage(%s) 未识别", usageJSON)
	}
	if tu.CompletionTokens != 246840 {
		t.Fatalf("completion_tokens = %d, 期望 246840", tu.CompletionTokens)
	}

	for _, tc := range []struct {
		name     string
		hasVideo bool
		want     int64
	}{
		{"无参考视频输入 → 低档单价", false, 246840 * 28},
		{"含参考视频输入 → 高档单价", true, 246840 * 46},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Cost(mustPricing(t, pricing), Measure{
				Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamArk,
				TaskUsage: tu, HasVideoInput: tc.hasVideo,
			})
			if got != tc.want {
				t.Errorf("Cost = %d 微元, 期望 %d", got, tc.want)
			}
		})
	}

	// 只配了一档（半张价目表）：回退到另一档，**不静默记 0**。
	// 半张表下 Priced() 仍是 true，模型页不挂「未定价」徽章——真记 0 就是
	// 「整档消费从账上消失且没有任何信号」，与 minimax 未知分辨率同一处置。
	half := []struct {
		name     string
		pricing  string
		hasVideo bool
	}{
		{"只配了低档、任务却含参考视频", `{"ark_video_token":28000000}`, true},
		{"只配了高档、任务不含参考视频", `{"ark_video_token_ref":46000000}`, false},
	}
	for _, tc := range half {
		t.Run(tc.name, func(t *testing.T) {
			got := Cost(mustPricing(t, tc.pricing), Measure{
				Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamArk,
				TaskUsage: tu, HasVideoInput: tc.hasVideo,
			})
			if got == 0 {
				t.Error("半张价目表下记了 0 元——该回退到已配置的那一档")
			}
		})
	}
	// 两档都没配才是真未定价，照旧 0 元（此时模型页会挂警示徽章）。
	if got := Cost(mustPricing(t, `{"in":1}`), Measure{
		Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamArk,
		TaskUsage: tu, HasVideoInput: true,
	}); got != 0 {
		t.Errorf("两档全缺时 Cost = %d, 期望 0", got)
	}
}

// MiniMax H3：(输出秒 + 输入秒) × 输出档秒价 + 超 5 张的图片附加费。
func TestCostVideoMinimax(t *testing.T) {
	// 2K 0.80 元/秒、768P 0.50 元/秒、超额图片 0.20 元/张。
	const pricing = `{"minimax_video_sec_2k":800000,"minimax_video_sec_768p":500000,"minimax_video_image_extra":200000}`

	tests := []struct {
		name       string
		usageJSON  string
		resolution string
		want       int64
	}{{
		name:       "2K 纯文生视频 5 秒",
		usageJSON:  `{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}`,
		resolution: "2K",
		want:       5 * 800_000,
	}, {
		// 文档 §9 的例子：10 秒参考视频出 5 秒 2K 成片 = ¥12，输入比输出还贵。
		name:       "参考视频按输入秒真金白银计，且按输出档单价",
		usageJSON:  `{"total_seconds":15,"input_seconds":10,"output_seconds":5,"input_image_count":0}`,
		resolution: "2K",
		want:       12 * MicroPerYuan,
	}, {
		name:       "768P 档单价",
		usageJSON:  `{"input_seconds":0,"output_seconds":6,"input_image_count":0}`,
		resolution: "768P",
		want:       6 * 500_000,
	}, {
		name:       "输入图片 5 张以内免费",
		usageJSON:  `{"input_seconds":0,"output_seconds":5,"input_image_count":5}`,
		resolution: "768P",
		want:       5 * 500_000,
	}, {
		name:       "超出 5 张的部分按张附加",
		usageJSON:  `{"input_seconds":0,"output_seconds":5,"input_image_count":8}`,
		resolution: "768P",
		want:       5*500_000 + 3*200_000,
	}, {
		name:       "分辨率大小写不敏感",
		usageJSON:  `{"input_seconds":0,"output_seconds":5,"input_image_count":0}`,
		resolution: "2k",
		want:       5 * 800_000,
	}, {
		// 厂商加了新档、我们还没配价：取已配置档里的最高价，而不是静默记 0
		// ——偏高看得见（用量页会露出来），记 0 是整档消费从账上消失。
		name:       "认不出的档位取已配置的最高档，不记 0",
		usageJSON:  `{"input_seconds":0,"output_seconds":5,"input_image_count":0}`,
		resolution: "4K",
		want:       5 * 800_000,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tu, ok := ParseTaskUsage(tc.usageJSON, store.ModelKindVideo, config.UpstreamMinimax)
			if !ok {
				t.Fatalf("ParseTaskUsage(%s) 未识别", tc.usageJSON)
			}
			got := Cost(mustPricing(t, pricing), Measure{
				Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMinimax,
				TaskUsage: tu, Resolution: tc.resolution,
			})
			if got != tc.want {
				t.Errorf("Cost = %d 微元, 期望 %d", got, tc.want)
			}
		})
	}
}

// MiniMax H3 的第二种计费形态（Phase 4 补齐）：Context-IR（提示词增强）任务
// 不出视频、按 token 计价，输入/输出两个单价（官方 ¥5.80 / ¥23.00 每百万，
// 差 4 倍，合成一个字段必错）。形态判据是厂商 usage 自己的形状——秒形走秒价，
// token 形走 token 价，任务行没有 task_type 可依。
func TestCostVideoMinimaxContextIR(t *testing.T) {
	// ¥5.80 / ¥23.00 每百万 token。
	const pricing = `{"minimax_video_sec_2k":800000,"minimax_context_ir_in":5800000,"minimax_context_ir_out":23000000}`
	const usageJSON = `{"total_tokens":13000,"prompt_tokens":10000,"completion_tokens":3000}`

	tu, ok := ParseTaskUsage(usageJSON, store.ModelKindVideo, config.UpstreamMinimax)
	if !ok {
		t.Fatal("Context-IR 的 token 形 usage 必须判为可得——否则这一族任务恒记 0 元")
	}
	got := Cost(mustPricing(t, pricing), Measure{
		Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMinimax,
		TaskUsage: tu,
	})
	// 10000×5.8/M + 3000×23/M = 58000 + 69000 微元。
	if want := int64(58_000 + 69_000); got != want {
		t.Errorf("Cost = %d 微元，期望 %d", got, want)
	}

	// 同一张价目表下，秒形的 usage 照旧走秒价——两种形态互不干扰。
	sec, ok := ParseTaskUsage(`{"input_seconds":0,"output_seconds":5,"input_image_count":0}`,
		store.ModelKindVideo, config.UpstreamMinimax)
	if !ok {
		t.Fatal("秒形 usage 应可得")
	}
	if got := Cost(mustPricing(t, pricing), Measure{
		Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMinimax,
		TaskUsage: sec, Resolution: "2K",
	}); got != 5*800_000 {
		t.Errorf("秒形 Cost = %d，期望 %d", got, 5*800_000)
	}

	// 反向绊线：token 形的可得性只认 prompt_tokens。方舟 Seedance 的原文
	// （completion_tokens + total_tokens，没有 prompt_tokens）若错配到 minimax
	// 行上，必须判**不可得**——认了它就是按 Context-IR 的价（多半没配 = 0 元）
	// 非估算地一次性清算，钱永久消失且账面看不出异常。
	if _, ok := ParseTaskUsage(`{"completion_tokens":246840,"total_tokens":246840}`,
		store.ModelKindVideo, config.UpstreamMinimax); ok {
		t.Error("方舟形 usage 错配到 minimax 行上必须判不可得")
	}
}

// 同步出图（方舟 Seedream）：张价与 token 价是**相加**关系，按价签只配其一。
func TestCostImage(t *testing.T) {
	usageJSON := `{"generated_images":4,"output_tokens":6000,"total_tokens":6000}`
	tu, ok := ParseTaskUsage(usageJSON, store.ModelKindImage, config.UpstreamArk)
	if !ok {
		t.Fatalf("ParseTaskUsage(%s) 未识别", usageJSON)
	}
	tests := []struct {
		name    string
		pricing string
		want    int64
	}{
		{"按张计价（0.2 元/张 × 4 张）", `{"ark_image_each":200000}`, 800_000},
		{"按 token 计价（30 元/百万 × 6000）", `{"ark_image_token":30000000}`, 180_000},
		{"两项都配则相加", `{"ark_image_each":200000,"ark_image_token":30000000}`, 980_000},
		{"未定价记 0", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Cost(mustPricing(t, tc.pricing), Measure{
				Kind: store.ModelKindImage, Entry: EntryImage, UpstreamType: config.UpstreamArk,
				TaskUsage: tu,
			})
			if got != tc.want {
				t.Errorf("Cost = %d 微元, 期望 %d", got, tc.want)
			}
		})
	}
}

// TestArkPlanBillsLikeArk：方舟**订阅**（ark_plan）与**按量**（ark）在计价上
// 是同一族——同一套官方接口、同一份 usage 形态，只是端点根与结算通道不同。
//
// 这是 2026-08-10 接入方舟 AIGC 时的活雷区：形态判定若按 type 而不是按厂商
// 族做，订阅那条来源的每一笔视频/出图都会「认不出形态 → 0 元 + estimated」，
// 账面上只剩一条 WARN，而模型页因为「有价」连徽章都不挂。四个判定点各钉一遍。
func TestArkPlanBillsLikeArk(t *testing.T) {
	const (
		videoUsage = `{"completion_tokens":128000,"total_tokens":128000}`
		imageUsage = `{"generated_images":1,"output_tokens":16384,"total_tokens":16384}`
	)
	p := mustPricing(t, `{"ark_video_token":28000000,"ark_video_token_ref":56000000,`+
		`"ark_image_each":200000,"ark_image_token":30000000}`)

	for _, tc := range []struct {
		kind, raw string
		entry     string
	}{
		{store.ModelKindVideo, videoUsage, EntryVideo},
		{store.ModelKindImage, imageUsage, EntryImage},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			// ①形态闸门：两种 type 都得认得这份厂商 usage。
			tuArk, okArk := ParseTaskUsage(tc.raw, tc.kind, config.UpstreamArk)
			tuPlan, okPlan := ParseTaskUsage(tc.raw, tc.kind, config.UpstreamArkPlan)
			if !okArk || !okPlan {
				t.Fatalf("ParseTaskUsage 可得性 ark=%v ark_plan=%v，期望都为真", okArk, okPlan)
			}
			// ②③金额：同量同价必须同额，且非零（0 元会让回归悄悄通过）。
			ma := Measure{Kind: tc.kind, Entry: tc.entry, UpstreamType: config.UpstreamArk, TaskUsage: tuArk}
			mp := Measure{Kind: tc.kind, Entry: tc.entry, UpstreamType: config.UpstreamArkPlan, TaskUsage: tuPlan}
			got, want := Cost(p, mp), Cost(p, ma)
			if want == 0 {
				t.Fatalf("按量金额为 0，用例本身失效")
			}
			if got != want {
				t.Errorf("订阅金额 = %d 微元，期望与按量一致 %d", got, want)
			}
			// ④价签可用性：徽章与告警都读它，订阅同样要判「这家上游用得上」。
			if !PricedFor(p, tc.kind, config.UpstreamArkPlan) {
				t.Errorf("PricedFor(ark_plan) = false，期望与 ark 同判")
			}
		})
	}
}

// 认不出的 kind / 上游族恒记 0：没有价签就没有账，绝不按别家的形态瞎算。
func TestCostUnknownShapes(t *testing.T) {
	p := mustPricing(t, `{"in":2000000,"out":8000000,"ark_video_token":28000000}`)
	cases := []struct {
		name string
		m    Measure
	}{
		{"未知 kind", Measure{Kind: "audio", Entry: "audio", UpstreamType: config.UpstreamArk}},
		{"视频形态但上游是 mock", Measure{
			Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMock,
			TaskUsage: TaskUsage{CompletionTokens: 1_000_000},
		}},
		{"图片形态但上游是 minimax（本迭代不服务）", Measure{
			Kind: store.ModelKindImage, Entry: EntryImage, UpstreamType: config.UpstreamMinimax,
			TaskUsage: TaskUsage{GeneratedImages: 10},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cost(p, tc.m); got != 0 {
				t.Errorf("Cost = %d, 期望 0", got)
			}
		})
	}
}

// int64 全程不溢出：极端量 × 极端价（各自都在 store 的入库上限内）不得回绕成
// 负数或小数，只允许饱和到 MaxCostMicro。cost_micro 的 UPSERT 是相加语义，
// 一次溢出会把整列在 SQLite 里悄悄变成 REAL，之后整段区间读数全崩。
func TestCostNeverOverflows(t *testing.T) {
	// store.MaxPricingMicro = 10^12 微元/百万 token，配上 10^12 个 token：
	// 朴素的 n*price 会冲出 int64（10^24）。
	huge := mustPricing(t, `{"in":1000000000000,"out":1000000000000,"ark_video_token":1000000000000,"minimax_video_sec_2k":1000000000000}`)
	cases := []struct {
		name string
		m    Measure
	}{
		{"文本极端量价", Measure{
			Kind: store.ModelKindText, Entry: EntryChat, UpstreamType: config.UpstreamDeepseek,
			Tokens: Tokens{Prompt: 1 << 40, Completion: 1 << 40},
		}},
		{"视频 token 极端量价", Measure{
			Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamArk,
			TaskUsage: TaskUsage{CompletionTokens: 1 << 40},
		}},
		{"秒价极端量价（无除数那一支）", Measure{
			Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMinimax,
			TaskUsage:  TaskUsage{OutputSeconds: 1 << 40, InputSeconds: 1 << 40},
			Resolution: "2K",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Cost(huge, tc.m)
			if got < 0 {
				t.Fatalf("Cost = %d，回绕成负数", got)
			}
			if got > MaxCostMicro {
				t.Fatalf("Cost = %d，超出单笔上限 %d", got, MaxCostMicro)
			}
		})
	}
}

// 小额不得被抹平：一次 300 token 的调用在「元/百万」价签下必须仍有金额
// （先除后乘的实现会把它整个抹成 0，那是系统性漏账）。
func TestCostSmallAmountsSurvive(t *testing.T) {
	p := mustPricing(t, `{"in":2000000,"out":8000000}`)
	got := Cost(p, Measure{
		Kind: store.ModelKindText, Entry: EntryChat, UpstreamType: config.UpstreamDeepseek,
		Tokens: Tokens{Prompt: 300, Completion: 100},
	})
	// 300*2 + 100*8 = 1400 微元
	if got != 1400 {
		t.Errorf("Cost = %d 微元, 期望 1400", got)
	}
}

// 目录价解析：形态无关的不变量（与 store.validatePricing 同一口径），
// 以及「坏价目表不得半张生效」。
func TestParsePricing(t *testing.T) {
	ok := []struct {
		raw   string
		field string
		want  int64
	}{
		{`{"in":1}`, FieldIn, 1},
		{`{"in":0}`, FieldIn, 0},
		{` {"out": 12345 } `, FieldOut, 12345},
	}
	for _, tc := range ok {
		p, err := ParsePricing(tc.raw)
		if err != nil {
			t.Fatalf("ParsePricing(%q): %v", tc.raw, err)
		}
		if p[tc.field] != tc.want {
			t.Errorf("ParsePricing(%q)[%s] = %d, 期望 %d", tc.raw, tc.field, p[tc.field], tc.want)
		}
	}

	bad := []string{
		`{"in":"1000"}`,        // 带引号的数字：管理 API 契约是整数收发
		`{"in":1.5}`,           // 小数
		`{"in":-1}`,            // 负数
		`{"in":1e9}`,           // 科学计数
		`{"in":null}`,          // null
		`{"in":{"a":1}}`,       // 复合值
		`{"in":1}}`,            // 多一个闭括号（dec.More() 会放行，Token() 不会）
		`{"in":1}]`,            //
		`[1,2]`,                // 不是对象
		`{"in":1000000000001}`, // 超 store.MaxPricingMicro
	}
	for _, raw := range bad {
		if _, err := ParsePricing(raw); err == nil {
			t.Errorf("ParsePricing(%q) 应报错", raw)
		} else if strings.Contains(err.Error(), "1000000000001") {
			t.Errorf("ParsePricing 错误文本回显了库里的值: %v", err)
		}
	}

	// 空串与字面量 null / {} 都是「未定价」，不是错误。
	for _, raw := range []string{"", "null", "{}"} {
		p, err := ParsePricing(raw)
		if err != nil {
			t.Errorf("ParsePricing(%q) 应视为未定价而非报错: %v", raw, err)
		}
		if p.Priced() {
			t.Errorf("ParsePricing(%q).Priced() = true, 期望 false", raw)
		}
	}
}

// 厂商 usage 原文解析：认得的键落位，一个都认不得时 ok=false（调用方据此
// 判「厂商 usage 不可得」，按 0 元 + estimated 记账并告警，而不是悄悄记 0）。
func TestParseTaskUsage(t *testing.T) {
	if _, ok := ParseTaskUsage("", store.ModelKindVideo, config.UpstreamMinimax); ok {
		t.Error("空串应为不可得")
	}
	if _, ok := ParseTaskUsage(`{"foo":1}`, store.ModelKindVideo, config.UpstreamMinimax); ok {
		t.Error("一个认得的键都没有时应为不可得")
	}
	if _, ok := ParseTaskUsage(`not json`, store.ModelKindVideo, config.UpstreamMinimax); ok {
		t.Error("非 JSON 应为不可得")
	}
	u, ok := ParseTaskUsage(`{"output_seconds":5,"input_seconds":-3,"input_image_count":7}`,
		store.ModelKindVideo, config.UpstreamMinimax)
	if !ok {
		t.Fatal("应识别 minimax 形态")
	}
	if u.OutputSeconds != 5 || u.InputImages != 7 {
		t.Errorf("解析结果 = %+v", u)
	}
	if u.InputSeconds != 0 {
		t.Errorf("负值应钳零，得 %d", u.InputSeconds)
	}
}

// 各 kind 的合法字段集只此一份（Phase 4 的管理层校验消费它），且三个集合
// 两两不相交——同一个模型可以同时挂 ark 与 minimax 两条来源，两家的价签必须
// 能共存在一张表里而不互相覆盖。
func TestFieldsFor(t *testing.T) {
	seen := map[string]string{}
	for _, kind := range []string{store.ModelKindText, store.ModelKindVideo, store.ModelKindImage} {
		fields := FieldsFor(kind)
		if len(fields) == 0 {
			t.Fatalf("FieldsFor(%s) 为空", kind)
		}
		for _, f := range fields {
			if prev, dup := seen[f]; dup {
				t.Errorf("价格字段 %q 同时属于 %s 与 %s", f, prev, kind)
			}
			seen[f] = kind
		}
	}
	if FieldsFor("audio") != nil {
		t.Error("未知 kind 应返回 nil")
	}
	// 返回的是副本：调用方改不动内部表。
	f := FieldsFor(store.ModelKindText)
	f[0] = "tampered"
	if FieldsFor(store.ModelKindText)[0] == "tampered" {
		t.Error("FieldsFor 返回了内部切片")
	}
}

// 外审遗留（Phase 2）：厂商 usage 里认得的**键存在**不等于**值可用**。
// 字符串型数字 / null / 复合值都要判成「usage 不可得」，否则任务会以
// 0 元 + 非估算被一次性清算掉，那笔钱从账上永久消失且没有任何告警——
// 两家真实 wire 形态至今未经真值验证，这个分支是活的。
func TestParseTaskUsageRejectsNonIntegerValues(t *testing.T) {
	bad := []string{
		`{"output_seconds":"6"}`,        // 字符串型数字
		`{"completion_tokens":null}`,    // null
		`{"completion_tokens":{"a":1}}`, // 复合值
		`{"generated_images":1.5}`,      // 小数
		`{"output_seconds":[1]}`,        // 数组
	}
	for _, raw := range bad {
		kind, upType := store.ModelKindVideo, config.UpstreamMinimax
		if strings.Contains(raw, "completion_tokens") {
			upType = config.UpstreamArk
		} else if strings.Contains(raw, "generated_images") {
			kind, upType = store.ModelKindImage, config.UpstreamArk
		}
		if u, ok := ParseTaskUsage(raw, kind, upType); ok {
			t.Errorf("ParseTaskUsage(%s) = (%+v, true), 期望不可得", raw, u)
		}
	}
	// 混合：一个键不可用、另一个可用 → 仍算可得（可用的那个照常落位）。
	u, ok := ParseTaskUsage(`{"input_seconds":"x","output_seconds":5}`, store.ModelKindVideo, config.UpstreamMinimax)
	if !ok || u.OutputSeconds != 5 || u.InputSeconds != 0 {
		t.Errorf("混合形态 = (%+v, %v), 期望 output_seconds=5 且可得", u, ok)
	}
}

// 缺陷②（Phase 3.5）：厂商 usage 的形态要按 **kind + 上游族**判，不能「见到
// 任一认得的键就算可得」。ark 的价签遇上 minimax 的秒数原文时，旧口径会解析出
// 一份空测量值，任务以 0 元、**非估算**被一次性清算——钱永久消失且无任何告警。
func TestParseTaskUsageRequiresShapeMatch(t *testing.T) {
	const minimaxRaw = `{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}`
	const arkRaw = `{"completion_tokens":246840,"total_tokens":246840}`
	const imageRaw = `{"generated_images":2,"output_tokens":32928}`

	cases := []struct {
		name   string
		raw    string
		kind   string
		upType string
		want   bool
	}{
		{"ark 视频遇 ark 原文", arkRaw, store.ModelKindVideo, config.UpstreamArk, true},
		{"ark 视频遇 minimax 原文 → 不可得", minimaxRaw, store.ModelKindVideo, config.UpstreamArk, false},
		{"minimax 视频遇 minimax 原文", minimaxRaw, store.ModelKindVideo, config.UpstreamMinimax, true},
		{"minimax 视频遇 ark 原文 → 不可得", arkRaw, store.ModelKindVideo, config.UpstreamMinimax, false},
		{"ark 图片遇出图原文", imageRaw, store.ModelKindImage, config.UpstreamArk, true},
		{"ark 图片遇视频原文 → 不可得", arkRaw, store.ModelKindImage, config.UpstreamArk, false},
		{"上游行已删（族未知）→ 不可得", arkRaw, store.ModelKindVideo, "", false},
		{"本形态不由任务 usage 计价（文本）→ 不可得", arkRaw, store.ModelKindText, config.UpstreamArk, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ParseTaskUsage(tc.raw, tc.kind, tc.upType); ok != tc.want {
				t.Errorf("ParseTaskUsage 可得性 = %v, 期望 %v", ok, tc.want)
			}
		})
	}
}

// 缺陷④（Phase 3.5）：单个价格字段上，「显式配成 0（免费）」与「压根没配」
// 是两个状态——判据必须是**键在不在**，不是值等不等于 0。混为一谈时，把某一档
// 明确标成免费会被当成没配而回退到另一档的价，用户凭空被计费。
func TestPriceFieldDistinguishesFreeFromUnset(t *testing.T) {
	tu, ok := ParseTaskUsage(`{"completion_tokens":1000000}`, store.ModelKindVideo, config.UpstreamArk)
	if !ok {
		t.Fatal("ark 形态应可得")
	}
	arkCost := func(pricing string, hasVideoInput bool) int64 {
		return Cost(mustPricing(t, pricing), Measure{
			Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamArk,
			TaskUsage: tu, HasVideoInput: hasVideoInput,
		})
	}
	// 含参考视频那一档明确标成免费：记 0，**不**回退到另一档。
	if got := arkCost(`{"ark_video_token":28000000,"ark_video_token_ref":0}`, true); got != 0 {
		t.Errorf("显式 0 价（免费）被当成未配置而回退计价 = %d 微元, 期望 0", got)
	}
	// 对照：那一档缺失时才回退（半张价目表不该静默记 0）。
	if got := arkCost(`{"ark_video_token":28000000}`, true); got != 1_000_000*28 {
		t.Errorf("缺档时应回退到已配置的那一档, 得 %d 微元", got)
	}

	mmTU, ok := ParseTaskUsage(`{"output_seconds":5,"input_seconds":0,"input_image_count":0}`,
		store.ModelKindVideo, config.UpstreamMinimax)
	if !ok {
		t.Fatal("minimax 形态应可得")
	}
	mmCost := func(pricing, resolution string) int64 {
		return Cost(mustPricing(t, pricing), Measure{
			Kind: store.ModelKindVideo, Entry: EntryVideo, UpstreamType: config.UpstreamMinimax,
			TaskUsage: mmTU, Resolution: resolution,
		})
	}
	// 768P 明确标成免费：记 0，不取 2K 的价。
	if got := mmCost(`{"minimax_video_sec_768p":0,"minimax_video_sec_2k":800000}`, "768P"); got != 0 {
		t.Errorf("显式 0 秒价被当成未配置 = %d 微元, 期望 0", got)
	}
	// 对照：768P 压根没配时回退到已配置的档，而不是静默记 0
	// （半张价目表下 Priced() 仍为 true，模型页不会挂「未定价」徽章）。
	if got := mmCost(`{"minimax_video_sec_2k":800000}`, "768P"); got != 5*800_000 {
		t.Errorf("缺档时应回退到已配置的档, 得 %d 微元", got)
	}
}
