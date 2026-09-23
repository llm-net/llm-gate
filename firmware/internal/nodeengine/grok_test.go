package nodeengine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

// 假 ACP CLI 通过真 SSH 转发访问设备，并验证初始化、权限回调、中止和第二轮输入。
func fakeGrok(args []string) {
	home := os.Getenv("GROK_HOME")
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	find := func(re string) string {
		m := regexp.MustCompile(re).FindSubmatch(cfg)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	base := strings.TrimSuffix(find(`base_url = "([^"]+)"`), "/agents/grok/v1")
	key := find(`api_key = "([^"]+)"`)
	mcpURL, mcpAuth := find(`(?m)^url = "([^"]+)"`), find(`Authorization = "([^"]+)"`)
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	instructions, effort := "", ""
	turns := 0
	var waiting json.RawMessage
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		reply := func(value any) { out.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": value}) }
		update := func(value any) {
			out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess_grok", "update": value}})
		}
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "authMethods": []any{map[string]string{"id": "xai.api_key"}}, "agentCapabilities": map[string]any{"promptCapabilities": map[string]bool{"image": true}}})
		case "authenticate":
			reply(map[string]any{})
		case "session/new":
			var req struct {
				Meta struct {
					Model   string `json:"modelId"`
					Rules   string `json:"rules"`
					Effort  string `json:"reasoningEffort"`
					Profile struct {
						Name  string   `json:"name"`
						Tools []string `json:"tools"`
					} `json:"agentProfile"`
				} `json:"_meta"`
			}
			json.Unmarshal(msg.Params, &req)
			instructions, effort = req.Meta.Rules, req.Meta.Effort
			if req.Meta.Profile.Name != "llmgate-studio" || !strings.Contains(strings.Join(req.Meta.Profile.Tools, ","), "run_terminal_cmd") {
				os.Exit(4)
			}
			reply(map[string]any{"sessionId": "sess_grok", "models": map[string]string{"currentModelId": req.Meta.Model}})
		case "session/prompt":
			turns++
			if strings.Contains(string(msg.Params), `"slow"`) {
				waiting = msg.ID
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "waiting"}})
				continue
			}
			if strings.Contains(string(msg.Params), `"fail"`) {
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32000, "message": "sensitive response " + testKey}})
				continue
			}
			if strings.Contains(string(msg.Params), `"exit"`) {
				return
			}
			waiting = msg.ID
			summary := probeDevice(base, mcpURL, mcpAuth, "/agents/grok/v1/responses", "Authorization", "Bearer "+key)
			summary += fmt.Sprintf(" model=%s effort=%s instr=%s turn=%d flags=%s", find(`(?m)^model = "([^"]+)"`), effort, instructions, turns, strings.Join(args, " "))
			if os.Getenv("GROK_MEMORY") == "0" && os.Getenv("GROK_STORAGE_MODE") == "local" && os.Getenv("GROK_IMAGE_EDIT") == "0" {
				summary += " isolated"
			}
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]string{"type": "text", "text": "检查素材"}})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "run_terminal_command", "rawInput": map[string]string{"command": "ffprobe media/a.mp4"}})
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "kind": "execute", "status": "in_progress"})
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "sess_grok", "options": []any{map[string]string{"kind": "reject_once", "optionId": "no"}, map[string]string{"kind": "allow_once", "optionId": "yes"}}}})
			// 身份与路径不同的会话通知必须忽略。
			out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "other", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "wrong session"}}}})
			// 真实 CLI 将命令输出编码成数值字节数组（不是 base64 字符串）。
			var bytes []int
			for _, b := range []byte("no such file") {
				bytes = append(bytes, int(b))
			}
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed", "rawOutput": map[string]any{"exit_code": 1, "output": bytes}})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "f1", "title": "edit", "kind": "edit", "status": "completed", "locations": []any{map[string]string{"path": "docs/a.md"}}})
			// 重复终态不能重复记执行事实。
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "f1", "status": "completed"})
			for _, text := range []string{"中文回复 ", summary} {
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
			}
		case "session/cancel":
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": waiting, "result": map[string]string{"stopReason": "cancelled"}})
			waiting = nil
		case "":
			if string(msg.ID) == `"permission-1"` {
				if !strings.Contains(string(msg.Result), `"yes"`) {
					os.Exit(3)
				}
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": "unknown-1", "method": "unknown/client_method", "params": map[string]any{}})
			}
			if string(msg.ID) == `"unknown-1"` {
				if !strings.Contains(string(msg.Error), `-32601`) {
					os.Exit(5)
				}
				// 等反向请求应答再结束；最后一帧在 RPC 响应之前必须处理完。
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": " 尾帧"}})
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": waiting, "result": map[string]any{"stopReason": "end_turn", "_meta": map[string]any{
					"inputTokens": 20, "outputTokens": 2,
					"usage": map[string]any{"modelUsage": map[string]any{"grok-model": map[string]int{"inputTokens": 123, "outputTokens": 8}}},
				}}})
				waiting = nil
			}
		}
	}
}

func TestGrokOnNode(t *testing.T) {
	e := newNodeEnv(t)
	eng := NewGrok(e.opts)
	cfg, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 7, Effort: "xhigh"})
	if err != nil || cfg.Model != "grok-model" {
		t.Fatalf("validate: %+v / %v", cfg, err)
	}
	if _, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 7, Effort: "max"}); err == nil {
		t.Fatal("invalid effort accepted")
	}
	if _, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 8}); err == nil {
		t.Fatal("invalid key accepted")
	}
	sess, err := eng.Start(t.Context(), e.request("xhigh"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	gs := sess.(*grokSession)
	dir := gs.r.dir
	if !strings.HasPrefix(dir, "/dev/shm/") {
		t.Fatal("Grok runtime must be in tmpfs")
	}
	stat, err := os.Stat(filepath.Join(dir, "config.toml"))
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions: %v / %v", stat, err)
	}
	for n := 1; n <= 2; n++ {
		var sn sink
		outcome, err := sess.Run(t.Context(), hostagent.Input{Text: "素材分析", Images: []string{"data:image/png;base64,aGVsbG8="}}, &sn)
		if err != nil || outcome.Status != hostagent.OutcomeCompleted {
			t.Fatalf("run: %+v / %v", outcome, err)
		}
		if len(sn.messages) != 1 {
			t.Fatalf("messages=%v", sn.messages)
		}
		summary := sn.messages[0]
		e.checkProbe(summary)
		for _, want := range []string{"中文回复", "尾帧", "model=grok-model", "effort=xhigh", "instr=开发者指令-标记", fmt.Sprintf("turn=%d", n), "isolated"} {
			if !strings.Contains(summary, want) {
				t.Fatalf("missing %q in %s", want, summary)
			}
		}
		if len(sn.deltas) != 3 || len(sn.reasoning) != 1 || len(sn.commands) != 1 || sn.commands[0] != "ffprobe media/a.mp4|1|no such file" || strings.Join(sn.files, ",") != "write:docs/a.md" || sn.in != 123 || sn.out != 8 {
			t.Fatalf("facts: deltas=%v reasoning=%v commands=%v files=%v usage=%d/%d", sn.deltas, sn.reasoning, sn.commands, sn.files, sn.in, sn.out)
		}
	}
	var sn sink
	done := make(chan hostagent.Outcome, 1)
	go func() { o, _ := sess.Run(t.Context(), hostagent.Input{Text: "slow"}, &sn); done <- o }()
	waitFor(t, func() bool { sn.mu.Lock(); defer sn.mu.Unlock(); return len(sn.deltas) > 0 })
	if err := sess.Interrupt(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.Status != hostagent.OutcomeInterrupted {
			t.Fatal(o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel blocked")
	}
	out, err := sess.Run(t.Context(), hostagent.Input{Text: "after cancel"}, &sink{})
	if err != nil || out.Status != hostagent.OutcomeCompleted {
		t.Fatalf("resume: %+v / %v", out, err)
	}
	out, err = sess.Run(t.Context(), hostagent.Input{Text: "fail"}, &sink{})
	if err != nil || out.Status != hostagent.OutcomeFailed || strings.Contains(out.Error, testKey) || strings.Contains(out.Error, "sensitive") {
		t.Fatalf("error privacy: %+v / %v", out, err)
	}
	for _, x := range e.fake.Execs() {
		if strings.Contains(x.Command, testKey) || strings.Contains(x.Command, e.token) {
			t.Fatal("credentials in command")
		}
	}
	sess.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("runtime remains: %v", err)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("Done not closed")
	}
}

func TestGrokCancelledContextAndProcessExit(t *testing.T) {
	for _, mode := range []string{"context", "exit"} {
		t.Run(mode, func(t *testing.T) {
			e := newNodeEnv(t)
			sess, err := NewGrok(e.opts).Start(t.Context(), e.request(""))
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var sn sink
			if mode == "context" {
				go func() {
					for {
						sn.mu.Lock()
						ready := len(sn.deltas) > 0
						sn.mu.Unlock()
						if ready {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(5 * time.Millisecond):
						}
					}
				}()
			}
			prompt := "exit"
			if mode == "context" {
				prompt = "slow"
			}
			out, err := sess.Run(ctx, hostagent.Input{Text: prompt}, &sn)
			want := hostagent.OutcomeFailed
			if mode == "context" {
				want = hostagent.OutcomeInterrupted
			}
			if err != nil || out.Status != want {
				t.Fatalf("outcome=%+v err=%v", out, err)
			}
		})
	}
}

func TestGrokImagesCapability(t *testing.T) {
	e := newNodeEnv(t)
	sess, err := NewGrok(e.opts).Start(t.Context(), e.request(""))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.(*grokSession).images = false
	out, err := sess.Run(t.Context(), hostagent.Input{Images: []string{"data:image/png;base64,aGVsbG8="}}, &sink{})
	if err != nil || out.Status != hostagent.OutcomeFailed || !strings.Contains(out.Error, "图片") {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
}
