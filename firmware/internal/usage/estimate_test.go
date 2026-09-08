package usage

// iteration-9 Phase 2 的可执行验收：估算兜底公式（中英混排样例）。

import "testing"

// 公式：ASCII rune 每 4 个 1 token（向上取整）+ 非 ASCII rune 每个 1 token。
func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"空串", "", 0},
		{"整 4 个 ASCII", "abcd", 1},
		{"不足一组也算一个（向上取整）", "abcde", 2},
		{"纯中文按字计", "你好世界", 4},
		{"中英混排：ASCII 与非 ASCII 各按各的档", "Hello 世界", 2 + 2}, // 6 个 ASCII → 2
		{"标点与空白同样是 ASCII", "a, b. c!", 2},                // 8 个 ASCII → 2
		{"emoji 是单个非 ASCII rune", "🎉", 1},
		{"中文标点也是非 ASCII", "你好，世界。", 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateTokens(tc.in); got != tc.want {
				t.Errorf("EstimateTokens(%q) = %d, 期望 %d", tc.in, got, tc.want)
			}
		})
	}
}

// 存在的理由：对中文比 count_tokens 兜底的纯 /4 准得多。同一段中文，
// 本公式给出的量级应显著高于「rune 数 /4」——那个函数的对外契约不动，
// 这条断言钉住「新公式确实不是它的复制品」。
func TestEstimateTokensBeatsPlainQuarterOnCJK(t *testing.T) {
	cjk := "把每一次调用按目录价折成人民币记到用户名下这件事本身并不复杂"
	runes := int64(len([]rune(cjk)))
	plainQuarter := (runes + 3) / 4
	got := EstimateTokens(cjk)
	if got != runes {
		t.Fatalf("纯中文应逐字计：EstimateTokens = %d, 期望 %d", got, runes)
	}
	if got <= plainQuarter {
		t.Errorf("对中文未比纯 /4 保守：%d vs %d", got, plainQuarter)
	}
}

// 输入侧估算只看 messages 与 system 两个字段，两者相加；有请求就至少 1
// （与 heuristicInputTokens 的「至少 1」同口径）。
func TestEstimateInputTokens(t *testing.T) {
	both := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		"system":   "你是一个助手",
		// 其余字段不参与估算（模型名、采样参数不是输入内容）。
		"model":       "deepseek-chat",
		"temperature": 0.7,
		"stream":      true,
	}
	onlyMessages := map[string]any{"messages": both["messages"]}
	onlySystem := map[string]any{"system": both["system"]}

	sum := EstimateInputTokens(onlyMessages) + EstimateInputTokens(onlySystem)
	if got := EstimateInputTokens(both); got != sum {
		t.Errorf("两字段合计 = %d, 期望 %d（messages + system）", got, sum)
	}

	// 无关字段不该贡献任何 token。
	noisy := map[string]any{
		"messages": both["messages"],
		"model":    "some-extremely-long-model-name-that-should-not-be-counted-at-all",
	}
	if got, want := EstimateInputTokens(noisy), EstimateInputTokens(onlyMessages); got != want {
		t.Errorf("无关字段被计入了：%d vs %d", got, want)
	}

	// 空请求也至少记 1：一次真实调用不该记成 0 token。
	if got := EstimateInputTokens(map[string]any{}); got != 1 {
		t.Errorf("空请求 = %d, 期望 1", got)
	}
	if got := EstimateInputTokens(map[string]any{"messages": nil}); got != 1 {
		t.Errorf("messages 为 null = %d, 期望 1", got)
	}
}
