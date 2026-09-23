package modeldata

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

func TestParseTagAndOrdering(t *testing.T) {
	for _, bad := range []string{"v1.0.0", "latest", "data-2026.09.16.01", "data-2026.09.16.1-rc1", "data-2026.13.01.1", "data-2026.02.30.1", "data-2026.09.16"} {
		if _, ok := ParseTag(bad); ok {
			t.Errorf("%s 不该被当作正式版本", bad)
		}
	}
	a, _ := ParseTag("data-2026.09.16.9")
	b, _ := ParseTag("data-2026.09.16.10")
	c, _ := ParseTag("data-2026.10.01.1")
	if !a.Less(b) || !b.Less(c) || b.Less(a) {
		t.Fatalf("排序不对：%v %v %v", a, b, c)
	}
	v, err := b.Version()
	if err != nil || v != 20260916010 {
		t.Fatalf("version = %d, %v", v, err)
	}
	if b.Date() != "2026-09-16" {
		t.Fatalf("date = %s", b.Date())
	}
	if _, err := (Tag{Name: "x", Year: 2026, Month: 9, Day: 16, N: 1000}).Version(); err == nil {
		t.Fatal("序号 ≥ 1000 应报错")
	}
}

func str(s string) *string { return &s }

func rates(rs ...Rate) []Rate { return rs }

func rate(meter, amount string, per int64, unit string) Rate {
	return Rate{Meter: meter, Amount: amount, Per: per, Unit: unit}
}

func published(currency string, rs []Rate, tp *TimePricing) Prices {
	return Prices{Status: "published", Rules: []Rule{{Currency: currency, Rates: rs, TimePricing: tp,
		Verification: Verification{Status: "verified", CheckedAt: str("2026-09-16T05:21:18Z"), SourceIDs: []string{"pricing"}}}}}
}

var notApplicable = Prices{Status: "not_applicable"}

func textModel(id, request string, usage, ref Prices, interfaces ...string) Model {
	return Model{ID: id, RequestID: str(request), DisplayName: id, Modalities: []string{"text"}, Availability: "active",
		Verification: Verification{Status: "verified", CheckedAt: str("2026-09-16T05:21:18Z"), SourceIDs: []string{"models"}},
		UsagePrices:  usage, ReferencePrices: ref, Capabilities: Capabilities{Interfaces: interfaces}}
}

// fixture 是一份覆盖主要换算路径的最小输入：DeepSeek（分时价 + 别名）、Anthropic
// 美元价、方舟视频 / 图像、MiniMax 文本 + 视频分家、Cursor 订阅（合成 cursor-auto）、
// 一个未映射的产品。
func fixture(t *testing.T) Input {
	t.Helper()
	tag, _ := ParseTag("data-2026.09.17.2")
	text3 := rates(rate("input_uncached_tokens", "9", 1000000, "token"), rate("input_cached_tokens", "0.30", 1000000, "token"), rate("output_tokens", "27", 1000000, "token"))
	off3 := rates(rate("input_uncached_tokens", "4.5", 1000000, "token"), rate("input_cached_tokens", "0.15", 1000000, "token"), rate("output_tokens", "13.5", 1000000, "token"))
	tp := &TimePricing{Timezone: "Asia/Shanghai", Bands: []Band{
		{ID: "peak", Label: "高峰时段", Days: []int{1, 2, 3, 4, 5}, Hours: []string{"09:00-12:00", "14:00-18:00"}, Rates: text3},
		{ID: "off_peak", Label: "空闲时段", Rates: off3},
	}}
	deepseekFlash := textModel("deepseek-v4-flash", "deepseek-v4-flash", published("CNY", text3, tp), notApplicable, "openai_chat")
	deepseekFlash.AliasOf = str("deepseek-flash")
	deepseekFlash.DisplayName = "DeepSeek Flash（旧请求名）"
	claude := textModel("claude-opus-5", "claude-opus-5", published("USD", rates(
		rate("input_uncached_tokens", "5", 1000000, "token"), rate("input_cached_tokens", "0.5", 1000000, "token"),
		rate("output_tokens", "25", 1000000, "token"), rate("cache_write_5m_tokens", "6.25", 1000000, "token"),
		rate("cache_write_1h_tokens", "10", 1000000, "token")), nil), notApplicable, "openai_chat", "anthropic_messages")
	seedance := Model{ID: "doubao-seedance-2-0", RequestID: str("doubao-seedance-2.0"), DisplayName: "Seedance 2.0", Modalities: []string{"video"}, Availability: "active",
		UsagePrices: published("CNY", rates(rate("video_output_tokens", "46", 1000000, "token")), nil), ReferencePrices: notApplicable,
		Capabilities: Capabilities{Interfaces: []string{"ark_video"}}}
	seedream := Model{ID: "doubao-seedream-5-0", RequestID: str("doubao-seedream-5.0"), DisplayName: "Seedream", Modalities: []string{"image"}, Availability: "active",
		UsagePrices: published("CNY", rates(rate("generated_images", "0.22", 1, "image"), rate("input_images", "0.02", 1, "image")), nil), ReferencePrices: notApplicable,
		Capabilities: Capabilities{Interfaces: []string{"ark_image"}}}
	noRequest := Model{ID: "doubao-seedance-2-0-mini", RequestID: nil, DisplayName: "mini", Modalities: []string{"video"}, Availability: "unknown",
		ReferencePrices: published("CNY", rates(rate("video_output_tokens", "23", 1000000, "token")), nil), Capabilities: Capabilities{Interfaces: []string{"ark_video"}}}
	m3 := textModel("minimax-m3", "MiniMax-M3", published("CNY", rates(rate("input_uncached_tokens", "2.1", 1000000, "token"), rate("output_tokens", "8.4", 1000000, "token")), nil), notApplicable, "openai_chat", "minimax_text")
	h3 := Model{ID: "minimax-h3", RequestID: str("MiniMax-H3"), DisplayName: "H3", Modalities: []string{"video"}, Availability: "active",
		UsagePrices:     published("CNY", rates(rate("video_output_seconds", "0.50", 1, "second"), rate("video_input_seconds", "0.50", 1, "second"), rate("input_images", "0.20", 1, "image")), nil),
		ReferencePrices: notApplicable, Capabilities: Capabilities{Interfaces: []string{"minimax_video"}}}
	speech := Model{ID: "speech-2-8-hd", RequestID: str("speech-2.8-hd"), Modalities: []string{"audio"}, Availability: "active",
		UsagePrices: published("CNY", rates(rate("billable_characters", "3.5", 10000, "character")), nil), Capabilities: Capabilities{Interfaces: []string{"minimax_speech"}}}
	composer := textModel("composer-2-5", "composer-2.5", notApplicable, published("USD", rates(
		rate("input_uncached_tokens", "0.5", 1000000, "token"), rate("input_cached_tokens", "0.2", 1000000, "token"), rate("output_tokens", "2.5", 1000000, "token")), nil), "cursor_agent")
	codexModel := textModel("gpt-5-6-luna", "gpt-5.6-luna", notApplicable, published("USD", rates(
		rate("input_uncached_tokens", "0.2", 1000000, "token"), rate("input_cached_tokens", "0.02", 1000000, "token"), rate("output_tokens", "1.2", 1000000, "token")), nil), "openai_responses")
	codexUnpriced := textModel("gpt-5-3-codex-spark", "gpt-5.3-codex-spark", notApplicable, Prices{Status: "unknown"}, "openai_responses")
	kling := Model{ID: "kling-3-0", RequestID: str("kling-3.0"), Modalities: []string{"video"}, Availability: "active",
		UsagePrices: published("CNY", rates(rate("video_output_seconds", "0.6", 1, "second")), nil), Capabilities: Capabilities{Interfaces: []string{"kling_video"}}}

	src := func(id, url, purpose string) ProviderSource {
		return ProviderSource{ID: id, URL: url, Purpose: purpose}
	}
	return Input{
		Tag: tag, Commit: "bf577ad968a15cadb63d439c93bb7899c74776a0",
		Providers: map[string]Provider{
			"deepseek":  {ID: "deepseek", DisplayName: "DeepSeek", Sources: []ProviderSource{src("models", "https://api-docs.deepseek.com/models", "models"), src("pricing", "https://api-docs.deepseek.com/pricing", "pricing")}},
			"anthropic": {ID: "anthropic", DisplayName: "Anthropic", Sources: []ProviderSource{src("pricing", "https://platform.claude.com/pricing", "pricing")}},
			"ark":       {ID: "ark", DisplayName: "火山方舟", Sources: []ProviderSource{src("models", "https://www.volcengine.com/models", "models")}},
			"minimax":   {ID: "minimax", DisplayName: "MiniMax", Sources: []ProviderSource{src("models", "https://platform.minimaxi.com/models", "models")}},
			"cursor":    {ID: "cursor", DisplayName: "Cursor", Sources: []ProviderSource{src("individual", "https://cursor.com/pricing", "subscription")}},
			"openai":    {ID: "openai", DisplayName: "OpenAI", Sources: []ProviderSource{src("codex", "https://openai.com/codex", "subscription")}},
			"kling":     {ID: "kling", DisplayName: "可灵", Sources: []ProviderSource{src("models", "https://klingai.com/models", "models")}},
		},
		Catalogs: []Catalog{
			{ProviderID: "anthropic", ID: "api-global", Kind: "pay_as_you_go", Models: []Model{claude}},
			{ProviderID: "ark", ID: "api-cn", Kind: "pay_as_you_go", Models: []Model{seedance, seedream, noRequest}},
			{ProviderID: "cursor", ID: "individual", Kind: "tool_subscription", Models: []Model{composer}},
			{ProviderID: "deepseek", ID: "api-cn", Kind: "pay_as_you_go", Models: []Model{deepseekFlash}},
			{ProviderID: "kling", ID: "api-cn", Kind: "pay_as_you_go", Models: []Model{kling}},
			{ProviderID: "minimax", ID: "api-cn", Kind: "pay_as_you_go", Models: []Model{m3, h3, speech}},
			{ProviderID: "openai", ID: "codex", Kind: "tool_subscription", Models: []Model{codexModel, codexUnpriced}},
		},
	}
}

func buildFixture(t *testing.T) (platformcatalog.Doc, Output) {
	t.Helper()
	out, err := Build(fixture(t), Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc, err := platformcatalog.Parse(out.Raw)
	if err != nil {
		t.Fatalf("合成结果不被设备接受：%v\n%s", err, out.Raw)
	}
	return doc, out
}

func pricingOf(t *testing.T, raw json.RawMessage) map[string]int64 {
	t.Helper()
	var p map[string]int64
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("价目不是整数表：%s", raw)
	}
	return p
}

func TestBuildHeaderAndVersion(t *testing.T) {
	doc, out := buildFixture(t)
	if doc.Schema != platformcatalog.Schema || doc.Version != 20260917002 || doc.UpdatedAt != "2026-09-17" {
		t.Fatalf("头部 = %+v", doc)
	}
	if doc.Source.Tag != "data-2026.09.17.2" || doc.Source.Commit == "" || doc.Source.USDCNY != DefaultUSDCNY || doc.Source.Repository != DefaultRepository {
		t.Fatalf("source = %+v", doc.Source)
	}
	if !strings.HasSuffix(string(out.Raw), "}\n") || strings.Contains(string(out.Raw), `&`) {
		t.Error("输出应为规范格式：末尾一个换行、不转义 HTML 字符")
	}
	// 平台按 按量 → 套餐 → 通用适配 排列，与 platformSpecs 同序。
	if doc.Platforms[0].ID != "deepseek" || doc.Platforms[len(doc.Platforms)-1].ID != "anthropic_compat" {
		t.Errorf("平台顺序不对：%s … %s", doc.Platforms[0].ID, doc.Platforms[len(doc.Platforms)-1].ID)
	}
}

func TestBuildTextPricingAndSchedule(t *testing.T) {
	doc, _ := buildFixture(t)
	m, ok := doc.PlatformModel("deepseek", "deepseek", "deepseek-v4-flash", "text")
	if !ok {
		t.Fatal("deepseek 平台缺 deepseek-v4-flash")
	}
	if p := pricingOf(t, m.Pricing); p["in"] != 9_000_000 || p["cache_read"] != 300_000 || p["out"] != 27_000_000 {
		t.Errorf("人民币 token 价换算错：%v", p)
	}
	if len(m.UpstreamProtocols) != 1 || m.UpstreamProtocols[0] != "openai_chat" {
		t.Errorf("upstream_protocols 应只保留 openai_chat：%v", m.UpstreamProtocols)
	}
	if !strings.Contains(m.Note, "别名，同 deepseek-flash") || !strings.Contains(m.Note, "旧请求名") {
		t.Errorf("note 应带展示名与别名：%q", m.Note)
	}
	if m.Capabilities.ResponsesChat.Profile != platformcatalog.ResponsesProfileReasoningReplayV1 {
		t.Errorf("deepseek 的固件行为档没挂上：%+v", m.Capabilities)
	}
	if m.Source != "https://api-docs.deepseek.com/models" || m.CheckedAt != "2026-09-16" {
		t.Errorf("出处 / 核对日期 = %q / %q", m.Source, m.CheckedAt)
	}
	var sched struct {
		Timezone string `json:"timezone"`
		Periods  []struct {
			Label   string           `json:"label"`
			Days    []string         `json:"days"`
			Hours   [][2]string      `json:"hours"`
			Pricing map[string]int64 `json:"pricing"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(m.Schedule, &sched); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if sched.Timezone != "Asia/Shanghai" || len(sched.Periods) != 2 {
		t.Fatalf("schedule = %+v", sched)
	}
	weekday := sched.Periods[0]
	if strings.Join(weekday.Days, ",") != "mon,tue,wed,thu,fri" || len(weekday.Hours) != 3 ||
		weekday.Hours[0] != [2]string{"00:00", "09:00"} || weekday.Hours[1] != [2]string{"12:00", "14:00"} || weekday.Hours[2] != [2]string{"18:00", "24:00"} ||
		weekday.Pricing["in"] != 4_500_000 || weekday.Label != "空闲时段" {
		t.Errorf("工作日空闲时段换算错：%+v", weekday)
	}
	weekend := sched.Periods[1]
	if strings.Join(weekend.Days, ",") != "sat,sun" || len(weekend.Hours) != 1 || weekend.Hours[0] != [2]string{"00:00", "24:00"} || weekend.Pricing["out"] != 13_500_000 {
		t.Errorf("周末时段换算错：%+v", weekend)
	}
}

func TestBuildUSDAndCacheWrite(t *testing.T) {
	doc, out := buildFixture(t)
	m, ok := doc.PlatformModel("anthropic", "anthropic_compat", "claude-opus-5", "text")
	if !ok {
		t.Fatal("anthropic 平台缺 claude-opus-5")
	}
	p := pricingOf(t, m.Pricing)
	if p["in"] != 33_750_000 || p["cache_read"] != 3_375_000 || p["out"] != 168_750_000 || p["cache_write"] != 42_187_500 {
		t.Errorf("美元价按 6.75 折算错：%v", p)
	}
	if _, ok := p["cache_write_1h_tokens"]; ok {
		t.Error("1 小时缓存写入不该进设备价目")
	}
	for _, line := range out.Report {
		if strings.Contains(line, "cache_write_1h_tokens") {
			t.Errorf("有意忽略的分量不该进报告：%s", line)
		}
	}
	if len(m.UpstreamProtocols) != 1 || m.UpstreamProtocols[0] != "anthropic_messages" {
		t.Errorf("anthropic_compat 平台只承载 Messages：%v", m.UpstreamProtocols)
	}
}

func TestBuildAIGCAndModalitySplit(t *testing.T) {
	doc, out := buildFixture(t)
	video, ok := doc.PlatformModel("ark", "ark", "doubao-seedance-2.0", "video")
	if !ok || video.Family != "ark_video" {
		t.Fatalf("ark 平台缺视频模型或协议面不对：%+v", video)
	}
	if p := pricingOf(t, video.Pricing); p["ark_video_token"] != 46_000_000 || p["ark_video_token_ref"] != 46_000_000 {
		t.Errorf("Seedance 两档应按同一基准投影：%v", p)
	}
	image, ok := doc.PlatformModel("ark", "ark", "doubao-seedream-5.0", "image")
	if !ok || image.Family != "ark_image" {
		t.Fatalf("ark 平台缺图像模型：%+v", image)
	}
	if p := pricingOf(t, image.Pricing); p["ark_image_each"] != 220_000 || len(p) != 1 {
		t.Errorf("按张价换算错：%v", p)
	}
	if _, ok := doc.PlatformModel("ark", "ark", "doubao-seedance-2.0-mini", ""); ok {
		t.Error("没有请求名的模型不该进文件")
	}
	// MiniMax：文本进 minimax_text_cn，视频进 minimax，音频哪儿都不进。
	if _, ok := doc.PlatformModel("minimax_text_cn", "openai_compat", "MiniMax-M3", "text"); !ok {
		t.Error("MiniMax 文本模型没进 minimax_text_cn")
	}
	if _, ok := doc.PlatformModel("minimax", "minimax", "MiniMax-M3", ""); ok {
		t.Error("文本模型不该进 minimax 视频平台")
	}
	h3, ok := doc.PlatformModel("minimax", "minimax", "MiniMax-H3", "video")
	if !ok || h3.Family != "minimax_video" {
		t.Fatalf("minimax 平台缺 H3：%+v", h3)
	}
	if p := pricingOf(t, h3.Pricing); p["minimax_video_sec_768p"] != 500_000 || p["minimax_video_image_extra"] != 200_000 || len(p) != 2 {
		t.Errorf("H3 价换算错：%v", p)
	}
	if len(doc.ModelsNamed("speech-2.8-hd", "")) != 0 {
		t.Error("音频模型不该进任何平台")
	}
	joined := strings.Join(out.Report, "\n")
	for _, want := range []string{"input_images 设备价目装不下", "未映射的产品 kling/api-cn", "opencode_go：数据仓库没有对应产品"} {
		if !strings.Contains(joined, want) {
			t.Errorf("报告缺 %q：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "speech-2-8-hd 没有落到") {
		t.Errorf("设备不计价的模态不该报成「没落地」：\n%s", joined)
	}
}

func TestBuildAgents(t *testing.T) {
	doc, out := buildFixture(t)
	cursor, ok := doc.Agent("cursor")
	if !ok || len(cursor.Models) != 2 || cursor.Models[0].Name != cursorAutoName || cursor.Models[1].Name != "composer-2.5" {
		t.Fatalf("Cursor 清单 = %+v", cursor.Models)
	}
	auto := pricingOf(t, cursor.Models[0].Pricing)
	basis := pricingOf(t, cursor.Models[1].Pricing)
	if auto["in"] != basis["in"] || auto["in"] != 3_375_000 || auto["cache_read"] != 1_350_000 || auto["out"] != 16_875_000 {
		t.Errorf("cursor-auto 应按 composer-2.5 的价：%v / %v", auto, basis)
	}
	if cursor.Source != "https://cursor.com/pricing" || cursor.CheckedAt != "2026-09-16" {
		t.Errorf("Cursor 订阅出处 = %q / %q", cursor.Source, cursor.CheckedAt)
	}
	codex, ok := doc.Agent("codex")
	if !ok || len(codex.Models) != 2 {
		t.Fatalf("Codex 清单 = %+v", codex.Models)
	}
	if p := pricingOf(t, codex.Models[0].Pricing); p["in"] != 1_350_000 || p["cache_read"] != 135_000 || p["out"] != 8_100_000 {
		t.Errorf("Codex 名义价换算错：%v", p)
	}
	if len(codex.Models[1].Pricing) != 0 {
		t.Errorf("没有参考价的型号不该带价：%s", codex.Models[1].Pricing)
	}
	if !strings.Contains(strings.Join(out.Report, "\n"), "gpt-5-3-codex-spark 没有已发布的参考价") {
		t.Errorf("报告应点名没价的订阅型号：%v", out.Report)
	}
	for _, provider := range []string{"grok", "claude"} {
		if _, ok := doc.Agent(provider); ok {
			t.Errorf("fixture 没给 %s 数据，不该合成出它", provider)
		}
	}
}

func TestBuildRejectsCursorWithoutBasis(t *testing.T) {
	in := fixture(t)
	for i := range in.Catalogs {
		if in.Catalogs[i].ProviderID == "cursor" {
			in.Catalogs[i].Models[0].ReferencePrices = Prices{Status: "unknown"}
		}
	}
	if _, err := Build(in, Options{}); err == nil || !strings.Contains(err.Error(), cursorAutoBasis) {
		t.Fatalf("没有 composer-2.5 的价时应报错，实际 %v", err)
	}
}

func TestConvertRatesRounding(t *testing.T) {
	f, err := newFX("6.75")
	if err != nil {
		t.Fatal(err)
	}
	// 0.075 USD / 百万 token → 506250 微元；1.35 CNY / 千 token → 1_350_000_000 微元 / 百万 token。
	p, dropped, err := f.convertRates("text", "", "USD", rates(rate("input_uncached_tokens", "0.075", 1000000, "token"), rate("output_tokens", "1", 1000000, "token")))
	if err != nil || len(dropped) != 0 || p["in"] != 506_250 {
		t.Fatalf("p=%v dropped=%v err=%v", p, dropped, err)
	}
	p, _, err = f.convertRates("text", "", "CNY", rates(rate("input_uncached_tokens", "1.35", 1000, "token"), rate("output_tokens", "1", 1000, "token")))
	if err != nil || p["in"] != 1_350_000_000 {
		t.Fatalf("按千 token 报价换算错：%v %v", p, err)
	}
	// 四舍五入：1/3 元 / 百万 token → 333333.33… → 333333；2/3 → 666667。
	p, _, err = f.convertRates("text", "", "CNY", rates(rate("input_uncached_tokens", "0.333333333", 1000000, "token"), rate("output_tokens", "0.666666666", 1000000, "token")))
	if err != nil || p["in"] != 333_333 || p["out"] != 666_667 {
		t.Fatalf("取整不对：%v %v", p, err)
	}
	if _, _, err := f.convertRates("text", "", "EUR", rates(rate("input_uncached_tokens", "1", 1, "token"))); err == nil {
		t.Fatal("没有汇率的币种应报错")
	}
	if _, _, err := f.convertRates("text", "", "CNY", rates(rate("input_uncached_tokens", "1", 1000000, "token"))); err == nil {
		t.Fatal("只有输入价（缺输出价）应被成对校验拦下")
	}
}

func TestBuildInputModalities(t *testing.T) {
	in := fixture(t)
	for i := range in.Catalogs {
		for j := range in.Catalogs[i].Models {
			if in.Catalogs[i].Models[j].RequestID != nil && *in.Catalogs[i].Models[j].RequestID == "claude-opus-5" {
				in.Catalogs[i].Models[j].Capabilities.InputModalities = []string{"text", "image", "file"}
			}
		}
	}
	out, err := Build(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := platformcatalog.Parse(out.Raw)
	if err != nil {
		t.Fatal(err)
	}
	caps, ok := doc.ModelCapabilitiesFor("anthropic", "anthropic", "claude-opus-5", "")
	if !ok || strings.Join(caps.InputModalities, ",") != "text,image,file" {
		t.Fatalf("input modalities lost: %+v", caps)
	}
}
