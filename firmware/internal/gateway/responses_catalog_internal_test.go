// responses_catalog_internal_test.go 直测转换面的纯函数（包内）：请求侧
// Responses→chat 的映射矩阵与契约拒绝，响应侧 chat→response 对象的成形。
// 走 HTTP 的端到端（事件序、故障切换、计量）在 responses_catalog_test.go。
package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

// decodeAny 把 JSON 字面量解成 map（测试专用，语法错误即失败）。
func decodeAny(t *testing.T, s string) map[string]any {
	t.Helper()
	m, ok := decodeJSONObject([]byte(s))
	if !ok {
		t.Fatalf("测试载荷不是合法 JSON 对象: %s", s)
	}
	return m
}

func TestResponsesToChatPayloadCodexShape(t *testing.T) {
	p := decodeAny(t, `{
		"model": "kimi-k3",
		"store": false,
		"stream": true,
		"instructions": "You are helpful.",
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"rule"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":"world"}]},
			{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"role":"assistant","content":"prior answer"}
		],
		"tools": [
			{"type":"function","name":"shell","description":"run","parameters":{"type":"object"},"strict":false},
			{"type":"namespace","name":"multi_agent_v1","description":"sub agents","tools":[{"type":"function","name":"spawn_agent"}]},
			{"type":"web_search","external_web_access":false}
		],
		"tool_choice": "auto",
		"parallel_tool_calls": false,
		"reasoning": {"effort":"medium","summary":"auto"},
		"include": ["reasoning.encrypted_content"],
		"prompt_cache_key": "abc",
		"metadata": {"a":"b"},
		"max_output_tokens": 128,
		"temperature": 1
	}`)
	out, cerr := responsesToChatPayload(p)
	if cerr != nil {
		t.Fatalf("转换失败: %s %s", cerr.code, cerr.message)
	}
	msgs, _ := out["messages"].([]any)
	wantRoles := []string{"system", "system", "user", "assistant", "tool", "assistant"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("messages 条数 = %d，期望 %d：%v", len(msgs), len(wantRoles), msgs)
	}
	for i, want := range wantRoles {
		m := msgs[i].(map[string]any)
		if m["role"] != want {
			t.Fatalf("messages[%d].role = %v，期望 %s", i, m["role"], want)
		}
	}
	if c := msgs[0].(map[string]any)["content"]; c != "You are helpful." {
		t.Fatalf("instructions 未落 system: %v", c)
	}
	if c := msgs[2].(map[string]any)["content"]; c != "hello\nworld" {
		t.Fatalf("多文本块未拼接: %v", c)
	}
	// function_call → assistant.tool_calls；function_call_output → tool 消息。
	tc := msgs[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "call_1" || tc["function"].(map[string]any)["name"] != "shell" {
		t.Fatalf("tool_calls 映射不对: %v", tc)
	}
	if m := msgs[4].(map[string]any); m["tool_call_id"] != "call_1" || m["content"] != "ok" {
		t.Fatalf("function_call_output 映射不对: %v", m)
	}
	// 工具嵌套形 + strict 丢弃；非 function 类型（namespace/web_search——真
	// codex 0.147 缺省就带，2026-08-13 实测）整个丢弃而不是 400。
	if n := len(out["tools"].([]any)); n != 1 {
		t.Fatalf("非 function 工具应被丢弃，只剩 1 个 function 工具，得到 %d 个", n)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "shell" || fn["description"] != "run" {
		t.Fatalf("工具映射不对: %v", tool)
	}
	if _, has := fn["strict"]; has {
		t.Fatal("strict 应被丢弃")
	}
	// 标量映射与丢弃清单。
	if out["max_tokens"] != json.Number("128") {
		t.Fatalf("max_output_tokens 未映射为 max_tokens: %v", out["max_tokens"])
	}
	if out["stream"] != true || out["tool_choice"] != "auto" || out["parallel_tool_calls"] != false {
		t.Fatalf("标量未照传: stream=%v tool_choice=%v", out["stream"], out["tool_choice"])
	}
	for _, banned := range []string{"reasoning", "include", "prompt_cache_key", "metadata",
		"instructions", "input", "store", "max_output_tokens"} {
		if _, has := out[banned]; has {
			t.Fatalf("字段 %q 应被丢弃/转换，不该出现在 chat 请求里", banned)
		}
	}
}

func TestResponsesToChatPayloadInputString(t *testing.T) {
	out, cerr := responsesToChatPayload(decodeAny(t, `{"model":"m","input":"hi"}`))
	if cerr != nil {
		t.Fatalf("转换失败: %v", cerr)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("字符串 input 应折成单条 user 消息: %v", msgs)
	}
}

func TestResponsesToChatPayloadTextFormat(t *testing.T) {
	out, cerr := responsesToChatPayload(decodeAny(t,
		`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object"},"strict":true}}}`))
	if cerr != nil {
		t.Fatalf("转换失败: %v", cerr)
	}
	rf := out["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "s" || js["strict"] != true {
		t.Fatalf("text.format 映射不对: %v", rf)
	}
	// type:text 与认不出的形态丢弃。
	out, _ = responsesToChatPayload(decodeAny(t, `{"model":"m","input":"hi","text":{"format":{"type":"text"}}}`))
	if _, has := out["response_format"]; has {
		t.Fatal("format type=text 不该产生 response_format")
	}
}

func TestResponsesToChatPayloadContractRejections(t *testing.T) {
	cases := []struct {
		name, body, code string
	}{
		{"store真", `{"model":"m","store":true,"input":"hi"}`, "responses_stateful_unsupported"},
		{"续聊", `{"model":"m","previous_response_id":"resp_1","input":"hi"}`, "responses_stateful_unsupported"},
		{"hosted tool_choice", `{"model":"m","input":"hi","tool_choice":{"type":"web_search"}}`, "responses_tool_unsupported"},
		{"媒体输入", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:x"}]}]}`, "responses_input_unsupported"},
		{"未知item", `{"model":"m","input":[{"type":"item_reference","id":"x"}]}`, "responses_input_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cerr := responsesToChatPayload(decodeAny(t, tc.body))
			if cerr == nil || cerr.code != tc.code {
				t.Fatalf("期望拒绝 %s，得到 %+v", tc.code, cerr)
			}
		})
	}
}

func TestResponsesToChatPayloadAllToolsDroppedOmitsKey(t *testing.T) {
	out, cerr := responsesToChatPayload(decodeAny(t,
		`{"model":"m","input":"hi","tools":[{"type":"web_search"},{"type":"namespace","name":"x","tools":[]}]}`))
	if cerr != nil {
		t.Fatalf("非 function 工具应静默丢弃: %v", cerr)
	}
	if _, has := out["tools"]; has {
		t.Fatal("全部工具被丢弃时不该带空 tools 键（部分上游对空数组 400）")
	}
}

func TestResponsesToChatPayloadReasoningItemDropped(t *testing.T) {
	out, cerr := responsesToChatPayload(decodeAny(t,
		`{"model":"m","input":[{"type":"reasoning","encrypted_content":"x"},{"type":"message","role":"user","content":"q"}]}`))
	if cerr != nil {
		t.Fatalf("reasoning item 应静默丢弃而不是拒绝: %v", cerr)
	}
	if msgs := out["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("reasoning item 应被丢弃: %v", msgs)
	}
}

func TestResponsesToChatPayloadDeepSeekV4ThinkingToolReplay(t *testing.T) {
	caps, ok := platformcatalog.Builtin().ModelCapabilitiesFor("deepseek", "deepseek", "deepseek-v4-flash", "deepseek-v4-flash")
	if !ok {
		t.Fatal("内嵌数据目录缺 DeepSeek V4 Flash 能力")
	}
	p := decodeAny(t, `{
		"model":"deepseek-v4-flash",
		"input":[
			{"type":"message","role":"user","content":"inspect the host"},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"I need host facts."}]},
			{"type":"message","role":"assistant","content":"I will inspect it."},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"uname -a\"}"},
			{"type":"function_call","call_id":"call_2","name":"shell","arguments":"{\"cmd\":\"free -h\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"Linux test"}
		],
		"tool_choice":"auto",
		"reasoning":{"effort":"xhigh","summary":"auto"}
	}`)
	out, cerr := responsesToChatPayloadWithCompat(p, responsesChatCompat{capability: caps.ResponsesChat})
	if cerr != nil {
		t.Fatalf("DeepSeek V4 转换失败: %+v", cerr)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 条数 = %d，期望 user + assistant tools + tool output：%v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "I will inspect it." ||
		assistant["reasoning_content"] != "I need host facts." {
		t.Fatalf("assistant thinking 回放不完整: %v", assistant)
	}
	if calls := assistant["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("同一轮的 function_call 没有合成一条 assistant 消息: %v", calls)
	}
	if out["reasoning_effort"] != "max" {
		t.Fatalf("Codex xhigh 应映射为 DeepSeek max，得到 %v", out["reasoning_effort"])
	}
	if thinking := out["thinking"].(map[string]any); thinking["type"] != "enabled" {
		t.Fatalf("thinking 开关不对: %v", thinking)
	}
	if _, has := out["tool_choice"]; has {
		t.Fatal("DeepSeek V4 thinking 请求不应携带 tool_choice")
	}

	p["reasoning"] = map[string]any{"effort": "high"}
	out, cerr = responsesToChatPayloadWithCompat(p, responsesChatCompat{capability: caps.ResponsesChat})
	if cerr != nil || out["reasoning_effort"] != "high" {
		t.Fatalf("Codex high 应保持 DeepSeek high，out=%v err=%+v", out["reasoning_effort"], cerr)
	}
}

func TestChatToResponseShape(t *testing.T) {
	chatObj := decodeAny(t, `{
		"id": "chatcmpl-9", "model": "upstream-side-v2",
		"choices": [{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"think",
			"tool_calls":[{"id":"call_7","type":"function","function":{"name":"shell","arguments":"{}"}}]},
			"finish_reason":"length"}],
		"usage": {"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":25}
	}`)
	resp := chatToResponse(chatObj, "resp_req1", "kimi-k3")
	if resp["model"] != "kimi-k3" || resp["object"] != "response" || resp["store"] != false {
		t.Fatalf("response 头部字段不对: %v", resp)
	}
	if resp["status"] != "incomplete" {
		t.Fatalf("finish=length 应折成 incomplete: %v", resp["status"])
	}
	if inc := resp["incomplete_details"].(map[string]any); inc["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details 不对: %v", inc)
	}
	output := resp["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output 应为 reasoning+message+function_call 三项: %v", output)
	}
	if it := output[0].(map[string]any); it["type"] != "reasoning" ||
		it["summary"].([]any)[0].(map[string]any)["text"] != "think" {
		t.Fatalf("reasoning item 不对: %v", it)
	}
	if it := output[1].(map[string]any); it["type"] != "message" ||
		it["content"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("message item 不对: %v", it)
	}
	if it := output[2].(map[string]any); it["type"] != "function_call" ||
		it["call_id"] != "call_7" || it["name"] != "shell" {
		t.Fatalf("function_call item 不对: %v", it)
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != int64(100) || u["output_tokens"] != int64(7) || u["total_tokens"] != int64(107) {
		t.Fatalf("usage 映射不对: %v", u)
	}
	if d := u["input_tokens_details"].(map[string]any); d["cached_tokens"] != int64(25) {
		t.Fatalf("DeepSeek 平铺缓存命中未映射: %v", d)
	}
}

func TestOutputTextForms(t *testing.T) {
	if got := outputText("plain"); got != "plain" {
		t.Fatalf("字符串形: %q", got)
	}
	blocks := []any{map[string]any{"type": "output_text", "text": "a"}, map[string]any{"type": "output_text", "text": "b"}}
	if got := outputText(blocks); got != "a\nb" {
		t.Fatalf("块数组形: %q", got)
	}
	if got := outputText(map[string]any{"k": "v"}); !strings.Contains(got, `"k":"v"`) {
		t.Fatalf("结构化输出应 JSON 序列化: %q", got)
	}
}
