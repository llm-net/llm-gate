package platformcatalog

// 内嵌副本的绊线：能解析、与官网那份逐字节一致、收录的条目都合设备侧词汇。
// 最后一条是本包唯一越过"只做结构校验"那条线的地方，且只对**内嵌**副本生效
// ——内嵌文件与代码同批发布，型号写错了该在 CI 就炸；远端下载来的那份仍按
// 结构校验放行，逐条合法性由 internal/admin 用建模那条路径判定。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func TestUpstreamProtocolDeclarations(t *testing.T) {
	for _, tc := range []struct {
		field string
		valid bool
	}{
		{``, true}, {`,"upstream_protocols":[]`, true},
		{`,"upstream_protocols":["openai_chat","anthropic_messages"]`, true},
		{`,"upstream_protocols":["openai_responses"]`, true},
		{`,"upstream_protocols":["remote_protocol"]`, false},
		{`,"upstream_protocols":["openai_chat","openai_chat"]`, false},
		{`,"upstream_protocols":"openai_chat"`, false},
	} {
		raw := fmt.Sprintf(`{"schema":%q,"version":1,"platforms":[{"id":"opencode_go","type":"opencode_go","models":[{"name":"test","kind":"text"%s}]}]}`, Schema, tc.field)
		doc, err := Parse([]byte(raw))
		if (err == nil) != tc.valid {
			t.Fatalf("%s: err=%v", tc.field, err)
		}
		if err == nil && tc.field == `,"upstream_protocols":[]` && doc.ModelProtocolsFor("opencode_go", "opencode_go", "test", "") == nil {
			t.Fatal("显式空协议声明丢失")
		}
	}
	for _, model := range []string{"longcat-2.0", "mimo-v2.5", "mimo-v2.5-pro"} {
		protocols := Builtin().ModelProtocolsFor("opencode_go", "opencode_go", model, "")
		if len(protocols) != 1 || protocols[0] != "openai_chat" {
			t.Fatalf("%s 应仅接受 Chat 上游: %v", model, protocols)
		}
	}
}

// 内嵌数据自查所用的设备词汇（与 config / store 的常量同值）。
var (
	knownTypes = map[string]bool{
		"deepseek": true, "ark": true, "ark_plan": true,
		"qwen_plan": true, "opencode_go": true, "minimax": true, "openai_compat": true, "anthropic_compat": true,
	}
	knownKinds = map[string]bool{"text": true, "video": true, "image": true}
	kindFamily = map[string][]string{"video": {"ark_video", "minimax_video"}, "image": {"ark_image"}}
	// agents 段的词汇（store.AgentProvider* 同值）。
	knownProviders = map[string]bool{"codex": true, "grok": true, "claude": true, "cursor": true}
)

func TestBuiltinParses(t *testing.T) {
	doc := Builtin()
	if doc.Schema != Schema {
		t.Fatalf("schema = %q, 期望 %q", doc.Schema, Schema)
	}
	if doc.Version <= 0 {
		t.Fatalf("version = %d, 期望正整数", doc.Version)
	}
	if len(BuiltinRaw()) > MaxBytes {
		t.Fatalf("内嵌副本 %d 字节，超过 MaxBytes %d", len(BuiltinRaw()), MaxBytes)
	}
}

func TestBuiltinArkPlanIncludesCurrentCodexModels(t *testing.T) {
	type expectation struct {
		levels    []string
		minimalUp string
	}
	want := map[string]expectation{
		"glm-5.3": {levels: []string{"low", "medium", "high", "xhigh", "max"}, minimalUp: "low"},
		"kimi-k3": {levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, minimalUp: "minimal"},
	}
	found := map[string]bool{}
	for _, p := range Builtin().Platforms {
		if p.ID != "ark_plan" {
			continue
		}
		for _, m := range p.Models {
			expect, ok := want[m.Name]
			if !ok {
				continue
			}
			gotLevels := make([]string, 0, len(m.Capabilities.Codex.SupportedReasoningLevels))
			for _, level := range m.Capabilities.Codex.SupportedReasoningLevels {
				gotLevels = append(gotLevels, level.Effort)
			}
			if m.Kind != "text" || m.Capabilities.ResponsesChat.Profile == "" ||
				m.Capabilities.ResponsesChat.ThinkingType != "enabled" ||
				m.Capabilities.ResponsesChat.EffortMap["minimal"] != expect.minimalUp ||
				m.Capabilities.ResponsesChat.EffortMap["ultra"] != "max" ||
				m.Capabilities.Codex.DefaultReasoningLevel != "high" ||
				strings.Join(gotLevels, ",") != strings.Join(expect.levels, ",") {
				t.Errorf("火山方舟订阅模型 %q 的 Codex effort 能力不完整：%+v", m.Name, m.Capabilities)
				continue
			}
			found[m.Name] = true
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("火山方舟订阅清单缺少 Codex 兼容文本模型 %q", name)
		}
	}
}

// TestBuiltinPriorityAPIKeyProfiles 把 API 密钥接入的两级平台清单钉成产品契约：
// 固定平台必须继续指向核对过的 HTTPS 端点、复用现有通用适配器，而且本批选单
// 只收文本模型。厂商上新仍可走管理台「自定义」，这里防的是目录维护时把平台
// 档案漏掉、改错地域，或顺手带进图片/视频/声音模型。
func TestBuiltinPriorityAPIKeyProfiles(t *testing.T) {
	type expectation struct {
		adapter string
		baseURL string
	}
	want := map[string]expectation{
		"kimi_cn":           {adapter: "openai_compat", baseURL: "https://api.moonshot.cn/v1"},
		"zhipu_cn":          {adapter: "openai_compat", baseURL: "https://open.bigmodel.cn/api/paas/v4"},
		"bailian_cn":        {adapter: "openai_compat", baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		"bailian_sg":        {adapter: "openai_compat", baseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		"minimax_text_cn":   {adapter: "openai_compat", baseURL: "https://api.minimaxi.com/v1"},
		"minimax_text_intl": {adapter: "openai_compat", baseURL: "https://api.minimax.io/v1"},
		"siliconflow_cn":    {adapter: "openai_compat", baseURL: "https://api.siliconflow.cn/v1"},
		"qianfan_cn":        {adapter: "openai_compat", baseURL: "https://qianfan.baidubce.com/v2"},
		"openai":            {adapter: "openai_compat", baseURL: "https://api.openai.com/v1"},
		"anthropic":         {adapter: "anthropic_compat", baseURL: "https://api.anthropic.com/v1"},
		"gemini":            {adapter: "openai_compat", baseURL: "https://generativelanguage.googleapis.com/v1beta/openai"},
		"xai":               {adapter: "openai_compat", baseURL: "https://api.x.ai/v1"},
		"openrouter":        {adapter: "openai_compat", baseURL: "https://openrouter.ai/api/v1"},
		"stepfun_cn":        {adapter: "openai_compat", baseURL: "https://api.stepfun.com/v1"},
		"tencent_tokenhub":  {adapter: "openai_compat", baseURL: "https://tokenhub.tencentmaas.com/v1"},
		"tencent_lkeap":     {adapter: "openai_compat", baseURL: "https://api.lkeap.cloud.tencent.com/v3"},
		"groq":              {adapter: "openai_compat", baseURL: "https://api.groq.com/openai/v1"},
		"mistral":           {adapter: "openai_compat", baseURL: "https://api.mistral.ai/v1"},
		"together":          {adapter: "openai_compat", baseURL: "https://api.together.ai/v1"},
	}
	found := make(map[string]bool, len(want))
	// 内置订阅套餐型（id == type）的文本选单同样非空且只收文本：qwen_plan 与
	// opencode_go 都是「套餐 Key 与按量 Key 分开」的固定端点类型。
	planTextModels := map[string]int{"qwen_plan": 0, "opencode_go": 0}
	for _, p := range Builtin().Platforms {
		if _, isPlan := planTextModels[p.ID]; isPlan {
			if p.Type != p.ID || p.BillingMode != BillingSubscription || p.BaseURL != "" || p.CustomBaseURL {
				t.Errorf("套餐平台 %q 的适配器/计费/端点 = %q/%q/%q，期望内置类型、subscription、无 base_url", p.ID, p.Type, p.BillingMode, p.BaseURL)
			}
			for _, m := range p.Models {
				if m.Kind != "text" || m.Family != "" {
					t.Errorf("%s 选单混入非文本型号 %q（kind=%q family=%q）", p.ID, m.Name, m.Kind, m.Family)
				}
				planTextModels[p.ID]++
			}
		}
		expect, ok := want[p.ID]
		if !ok {
			continue
		}
		found[p.ID] = true
		if p.Type != expect.adapter || p.BaseURL != expect.baseURL {
			t.Errorf("平台 %q 的适配器/端点 = %q/%q，期望 %q/%q", p.ID, p.Type, p.BaseURL, expect.adapter, expect.baseURL)
		}
		if p.CustomBaseURL {
			t.Errorf("固定平台 %q 不该允许管理员改写 base_url", p.ID)
		}
		if len(p.Models) == 0 {
			t.Errorf("平台 %q 的文本型号选单为空", p.ID)
		}
		for _, m := range p.Models {
			if m.Kind != "text" || m.Family != "" {
				t.Errorf("平台 %q 混入非文本型号 %q（kind=%q family=%q）", p.ID, m.Name, m.Kind, m.Family)
			}
		}
	}
	for id := range want {
		if !found[id] {
			t.Errorf("API 密钥优先级目录缺少平台档案 %q", id)
		}
	}
	for id, n := range planTextModels {
		if n == 0 {
			t.Errorf("%s 的文本型号选单为空", id)
		}
	}
}

// TestBuiltinEntriesAreWellFormed：内嵌清单里的每条都得是设备真收得下的形态
// ——上游类型认识、kind 三选一、AIGC 必须声明本 kind 圈内的协议面且 text 不
// 声明、名字不带空白（目录名规范）。
func TestBuiltinEntriesAreWellFormed(t *testing.T) {
	seenID := map[string]bool{}
	for _, p := range Builtin().Platforms {
		if !knownTypes[p.Type] {
			t.Errorf("平台 %q 不是设备认识的上游类型", p.Type)
		}
		if seenID[p.ID] {
			t.Errorf("平台 id %q 出现了两次", p.ID)
		}
		seenID[p.ID] = true
		if p.Source == "" || p.CheckedAt == "" {
			t.Errorf("平台 %q 缺 source/checked_at（收录纪律：没有出处的型号不入表）", p.Type)
		}
		seenName := map[string]bool{}
		for _, m := range p.Models {
			switch {
			case m.Name == "":
				t.Errorf("平台 %q 有一条无名模型", p.Type)
			case strings.ContainsAny(m.Name, " \t\n"):
				t.Errorf("平台 %q 的模型名 %q 带空白，设备目录不收", p.Type, m.Name)
			case seenName[m.Name]:
				t.Errorf("平台 %q 的模型 %q 重复", p.Type, m.Name)
			}
			seenName[m.Name] = true
			if !knownKinds[m.Kind] {
				t.Errorf("平台 %q 的模型 %q kind=%q 不合法", p.Type, m.Name, m.Kind)
				continue
			}
			if m.Kind == "text" {
				if m.Family != "" {
					t.Errorf("平台 %q 的文本模型 %q 不该声明协议面", p.Type, m.Name)
				}
				continue
			}
			if m.Family == "" {
				t.Errorf("平台 %q 的 %s 模型 %q 缺协议面", p.Type, m.Kind, m.Name)
				continue
			}
			ok := false
			for _, f := range kindFamily[m.Kind] {
				if m.Family == f {
					ok = true
				}
			}
			if !ok {
				t.Errorf("平台 %q 的模型 %q 协议面 %q 不属于 %s 种类", p.Type, m.Name, m.Family, m.Kind)
			}
		}
	}
}

// TestBuiltinAgentEntriesAreWellFormed：agents 段的绊线比 platforms 段严——
// 设备照着这一段**建模型行**，一条写错的条目会在每台连了该订阅的设备上凭空
// 长出一行改不掉的模型。除形态外还多两条：模型名在整段里全局唯一（一个名字
// 只有一行，两份订阅抢同一个名字是发布错误），以及 agents 段只收文本模型。
func TestBuiltinAgentEntriesAreWellFormed(t *testing.T) {
	seenProvider := map[string]bool{}
	seenName := map[string]bool{} // 跨 provider 全局
	for _, a := range Builtin().Agents {
		if !knownProviders[a.Provider] {
			t.Errorf("订阅 %q 不是设备认识的 provider", a.Provider)
		}
		if seenProvider[a.Provider] {
			t.Errorf("订阅 %q 出现了两次", a.Provider)
		}
		seenProvider[a.Provider] = true
		if a.Source == "" || a.CheckedAt == "" {
			t.Errorf("订阅 %q 缺 source/checked_at（收录纪律：没有出处的型号不入表）", a.Provider)
		}
		for _, m := range a.Models {
			nameKey := strings.ToLower(m.Name)
			if a.Provider == "cursor" {
				nameKey = "cursor:" + nameKey
			}
			switch {
			case m.Name == "":
				t.Errorf("订阅 %q 有一条无名模型", a.Provider)
			case strings.ContainsAny(m.Name, " \t\n"):
				t.Errorf("订阅 %q 的模型名 %q 带空白，设备目录不收", a.Provider, m.Name)
			case seenName[nameKey]:
				t.Errorf("模型名 %q 在 agents 段里重复（一个名字只能有一行）", m.Name)
			}
			seenName[nameKey] = true
			if m.Kind != "text" {
				t.Errorf("订阅 %q 的模型 %q kind=%q：agents 段只收文本模型", a.Provider, m.Name, m.Kind)
			}
		}
	}
}

// TestBuiltinMatchesWebsiteCopy：官网伺服的那份（website/public/updates/data/）
// 必须与固件内嵌的逐字节一致——同步下来的与开箱自带的是同一份文件，改哪边都
// 要同步另一边（同 codexhelper 的绊线口径）。
func TestBuiltinMatchesWebsiteCopy(t *testing.T) {
	path := filepath.Join("..", "..", "..", "website", "public", "updates", "data", "platform-models.json")
	want, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("website 副本不在场（%s）——仅允许出现在脱离仓库的独立构建里", path)
	}
	if err != nil {
		t.Fatalf("读 website 副本: %v", err)
	}
	if string(want) != string(BuiltinRaw()) {
		t.Fatalf("内嵌的 platform-models.json 与 website/public/updates/data/ 的副本不一致——两份必须逐字节相同")
	}
}

// TestBuiltinAgentTextModelsHavePricing：agents 段的**文本**型号与同目录
// official-pricing.json 里带 agent 标记的条目必须一一对上，名字逐字节相同、
// agent 取值相同。两份文件是一对，但改的是两个地方，最容易出的两种错都在这里
// 兜住：收录了型号却忘了录价（设备照建行，金额恒 0——2026-08-19 之前 claude
// 整条线就是这个症状），或者两边把同一个型号写成了两种写法（运行期按名区分
// 大小写精确匹配，写法不一致等于没录）。
//
// 订阅内含的图片/视频**刻意反着来**：那两族恒记 0 元，价目文件里不该有它们。
func TestBuiltinAgentTextModelsHavePricing(t *testing.T) {
	path := filepath.Join("..", "..", "..", "website", "public", "updates", "data", "official-pricing.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("价目文件不在场（%s）——仅允许出现在脱离仓库的独立构建里", path)
	}
	if err != nil {
		t.Fatalf("读价目文件: %v", err)
	}
	// 只取这条绊线要看的两个字段；价目文件的权威解析在 internal/officialsite。
	var priced struct {
		Models []struct {
			Name  string `json:"name"`
			Agent string `json:"agent"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &priced); err != nil {
		t.Fatalf("解析价目文件: %v", err)
	}
	pricedAgent := map[string]string{}
	for _, m := range priced.Models {
		if m.Agent != "" {
			pricedAgent[m.Name] = m.Agent
		}
	}
	inCatalog := map[string]bool{}
	for _, a := range Builtin().Agents {
		for _, m := range a.Models {
			if a.Provider == "cursor" {
				if _, err := usage.ParseCursorPrice(string(m.Pricing)); err != nil {
					t.Errorf("Cursor %s: %v", m.Name, err)
				}
				continue
			}
			inCatalog[m.Name] = true
			switch m.Kind {
			case "text":
				got, ok := pricedAgent[m.Name]
				if !ok {
					t.Errorf("订阅 %q 的文本型号 %q 在 official-pricing.json 里没有带 agent 标记的同名条目"+
						"——设备会建出一行永远记 0 元的模型", a.Provider, m.Name)
					continue
				}
				if got != a.Provider {
					t.Errorf("型号 %q 的价目 agent = %q，目录里归 %q", m.Name, got, a.Provider)
				}
			default:
				if _, ok := pricedAgent[m.Name]; ok {
					t.Errorf("订阅内含的 %s 型号 %q 不该有目录价（恒记 0 元）", m.Kind, m.Name)
				}
			}
		}
	}
	for name := range pricedAgent {
		if !inCatalog[name] {
			// 不是错：价目文件可以先于目录收录（也是管理员手工建同名行借价那条
			// 旋钮的用法）。记一条，方便发布时看出两边差了什么。
			t.Logf("价目文件里带 agent 标记的 %q 不在目录 agents 段里——设备不会自动为它建行", name)
		}
	}
}

func TestParseRejectsWrongShape(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"非 JSON", "<!doctype html>"},
		{"形态标识不符", `{"schema":"other.platform-models/v1","platforms":[{"type":"deepseek"}]}`},
		{"没有平台条目", `{"schema":"` + Schema + `","platforms":[]}`},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.raw)); err == nil {
			t.Errorf("%s: Parse 应当报错", c.name)
		}
	}
	doc, err := Parse([]byte(`{"schema":"` + Schema + `","version":2,"platforms":[{"type":"minimax","models":[{"name":"MiniMax-H3","kind":"video","family":"minimax_video"}]}]}`))
	if err != nil {
		t.Fatalf("Parse 合法文件: %v", err)
	}
	p, ok := doc.Platform("minimax")
	if !ok || len(p.Models) != 1 || p.Models[0].Name != "MiniMax-H3" {
		t.Fatalf("Platform(minimax) = %+v, %v", p, ok)
	}
	if _, ok := doc.Platform("deepseek"); ok {
		t.Fatalf("Platform(deepseek) 应当没有收录")
	}
}

func TestV2DynamicPlatformAndCapabilities(t *testing.T) {
	raw := `{"schema":"llmgate.platform-models/v2","version":9,"platforms":[{
		"id":"example_ai","type":"openai_compat","vendor":"Example AI",
		"base_url":"https://api.example.invalid/v1","billing_mode":"subscription",
		"models":[{"name":"example-reasoner","kind":"text","capabilities":{
			"responses_chat":{"profile":"reasoning_replay_v1","replay_reasoning_content":true,
				"thinking_type":"enabled","drop_tool_choice":true,"effort_map":{"xhigh":"max"}},
			"codex":{"default_reasoning_level":"xhigh","supported_reasoning_levels":[
				{"effort":"xhigh","description":"Maximum reasoning"}]}
		}}]}]}`
	doc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse dynamic platform: %v", err)
	}
	p, ok := doc.PlatformByID("example_ai", "openai_compat")
	if !ok || p.BaseURL != "https://api.example.invalid/v1" || p.BillingMode != BillingSubscription {
		t.Fatalf("dynamic platform = %+v, %v", p, ok)
	}
	caps, ok := doc.ModelCapabilitiesFor("example_ai", "openai_compat", "example-reasoner", "")
	if !ok || caps.ResponsesChat.EffortMap["xhigh"] != "max" || caps.Codex.DefaultReasoningLevel != "xhigh" {
		t.Fatalf("capabilities = %+v, %v", caps, ok)
	}
}

func TestV2RejectsUnsafeOrUnknownExtension(t *testing.T) {
	platform := func(fields string) []byte {
		return []byte(`{"schema":"llmgate.platform-models/v2","version":9,"platforms":[{` + fields + `}]}`)
	}
	cases := map[string][]byte{
		"specialized adapter for dynamic platform": platform(`"id":"new_deepseek","type":"deepseek","base_url":"https://api.example.invalid/v1","billing_mode":"usage"`),
		"insecure endpoint":                        platform(`"id":"example_ai","type":"openai_compat","base_url":"http://api.example.invalid/v1","billing_mode":"usage"`),
		"unknown adapter":                          platform(`"id":"example_ai","type":"remote_code","base_url":"https://api.example.invalid/v1","billing_mode":"usage"`),
		"unknown profile":                          platform(`"id":"example_ai","type":"openai_compat","base_url":"https://api.example.invalid/v1","billing_mode":"usage","models":[{"name":"m","kind":"text","capabilities":{"responses_chat":{"profile":"run_remote_script"}}}]`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatal("Parse 应拒绝该扩展")
			}
		})
	}
}

// Cursor 数据在接收时整份校验，避免坏价格进入计费；同名订阅按 provider 隔离。
func TestCursorCatalogValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Doc)
	}{
		{"missing price", func(d *Doc) { d.Agents[0].Models[0].Pricing = nil }},
		{"fractional price", func(d *Doc) { d.Agents[0].Models[0].Pricing = json.RawMessage(`{"in":1.5,"out":0}`) }},
		{"negative price", func(d *Doc) { d.Agents[0].Models[0].Pricing = json.RawMessage(`{"in":-1,"out":0}`) }},
		{"wrong field", func(d *Doc) { d.Agents[0].Models[0].Pricing = json.RawMessage(`{"in":0,"out":0,"image":1}`) }},
		{"invalid name", func(d *Doc) { d.Agents[0].Models[0].Name = "bad model" }},
		{"wrong kind", func(d *Doc) { d.Agents[0].Models[0].Kind = "image" }},
		{"duplicate model", func(d *Doc) { d.Agents[0].Models = append(d.Agents[0].Models, d.Agents[0].Models[0]) }},
		{"duplicate provider", func(d *Doc) { d.Agents = append(d.Agents, d.Agents[0]) }},
		{"other provider pricing", func(d *Doc) { d.Agents[0].Provider = "codex" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse(BuiltinRaw())
			if err != nil {
				t.Fatal(err)
			}
			d.Agents = []Agent{{Provider: "cursor", Models: []AgentModel{{Name: "shared-model", Kind: "text", Pricing: json.RawMessage(`{"in":0,"out":0}`)}}}}
			raw, _ := json.Marshal(d)
			if _, err := Parse(raw); err != nil {
				t.Fatalf("valid free price rejected: %v", err)
			}
			tc.change(&d)
			raw, _ = json.Marshal(d)
			if _, err := Parse(raw); err == nil {
				t.Fatal("invalid Cursor catalog accepted")
			}
		})
	}
}
