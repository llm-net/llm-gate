package hostagent_test

// Codex 引擎验收：测试二进制按 argv[0] 分身为假 app-server（协议形态按真机 v0.154 抓取：
// thread/start / turn/start / item 通知 / mcpServer/elicitation/request / turn/completed），
// 装进一棵真实布局的槽位树，经 codexappserver.Manager.Launch 拉起。假 app-server 收到
// 指令后真的去打设备的 MCP 端点（HTTP + 承载令牌）执行 exec，再把结果作为回复吐回来——
// 端到端覆盖：配置渲染（provider 指回环、Key 走环境变量）、握手、开线程、审批放行、工具
// 往返、流式回复、用量、中止、引擎侧失败与进程意外退出。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "codex-app-server":
		os.Exit(fakeAppServer())
	case "bwrap":
		fmt.Println("bubblewrap 0.11.0-fake")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var (
	mcpURLRE = regexp.MustCompile(`(?m)^url = "([^"]+)"`)
	baseRE   = regexp.MustCompile(`(?m)^base_url = "([^"]+)"`)
	modelRE  = regexp.MustCompile(`(?m)^model = "([^"]+)"`)
)

// fakeAppServer 按真机抓到的形态模拟 app-server。指令文本决定剧本：
//
//	run: <cmd>   经 MCP 端点执行 exec，回复里带结果与诊断（provider、Key、模型）
//	hang         不结束，等 turn/interrupt
//	fail         error(willRetry=false) + turn/completed failed
//	die          中途退出进程
//	local        先发本地命令审批（期望被拒），再正常结束
func fakeAppServer() int {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-app-server 0.154.0-fake")
		return 0
	}
	home := os.Getenv("CODEX_HOME")
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	mcpURL, base, model := "", "", ""
	if m := mcpURLRE.FindSubmatch(cfg); m != nil {
		mcpURL = string(m[1])
	}
	if m := baseRE.FindSubmatch(cfg); m != nil {
		base = string(m[1])
	}
	if m := modelRE.FindSubmatch(cfg); m != nil {
		model = string(m[1])
	}
	var outMu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		raw, _ := json.Marshal(v)
		outMu.Lock()
		out.Write(raw)
		out.WriteByte('\n')
		out.Flush()
		outMu.Unlock()
	}
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }

	var pendMu sync.Mutex
	pending := map[string]chan json.RawMessage{}
	nextSrv := 0
	ask := func(method string, params any) json.RawMessage {
		pendMu.Lock()
		nextSrv++
		id := fmt.Sprintf("srv-%d", nextSrv)
		ch := make(chan json.RawMessage, 1)
		pending[id] = ch
		pendMu.Unlock()
		send(map[string]any{"id": id, "method": method, "params": params})
		return <-ch
	}
	interrupt := make(chan struct{}, 4)
	mcpCall := func(method string, params any) (json.RawMessage, error) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, mcpURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+os.Getenv("LLMGATE_MCP_TOKEN"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("mcp %d: %s", resp.StatusCode, raw)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(raw, &env)
		return env.Result, nil
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	threadID := "01a0a071-1646-7961-bf06-1c1b89482964"
	turnN := 0
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "" && len(msg.ID) > 0 {
			var id string
			if json.Unmarshal(msg.ID, &id) == nil {
				pendMu.Lock()
				ch := pending[id]
				delete(pending, id)
				pendMu.Unlock()
				if ch != nil {
					ch <- msg.Result
				}
			}
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{"id": msg.ID, "result": map[string]any{"userAgent": "fake", "codexHome": home, "platformFamily": "unix", "platformOs": "linux"}})
		case "initialized":
		case "thread/start":
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			if p["ephemeral"] != true || p["sandbox"] != "read-only" || p["approvalPolicy"] != "on-request" || p["developerInstructions"] == "" || p["cwd"] == "" {
				send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32600, "message": fmt.Sprintf("bad thread/start params: %v", p)}})
				continue
			}
			send(map[string]any{"id": msg.ID, "result": map[string]any{"thread": map[string]any{"id": threadID, "ephemeral": true, "model": p["model"]}}})
		case "turn/interrupt":
			send(map[string]any{"id": msg.ID, "result": map[string]any{}})
			select {
			case interrupt <- struct{}{}:
			default:
			}
		case "turn/start":
			turnN++
			turnID := fmt.Sprintf("turn-%d", turnN)
			var p struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					URL  string `json:"url"`
				} `json:"input"`
				Effort string `json:"effort"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			send(map[string]any{"id": msg.ID, "result": map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}})
			text, images := "", 0
			for _, in := range p.Input {
				if in.Type == "text" {
					text = in.Text
				}
				if in.Type == "image" && strings.HasPrefix(in.URL, "data:image/") {
					images++
				}
			}
			go func() {
				tp := map[string]any{"threadId": threadID, "turnId": turnID}
				notify("turn/started", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "inProgress"}})
				done := func(status string, errMsg any) {
					turn := map[string]any{"id": turnID, "status": status, "error": errMsg}
					notify("turn/completed", map[string]any{"threadId": threadID, "turn": turn})
				}
				switch {
				case text == "hang":
					<-interrupt
					done("interrupted", nil)
					return
				case text == "fail":
					notify("error", map[string]any{"error": map[string]any{"message": "上游拒绝了请求"}, "willRetry": false})
					done("failed", map[string]any{"message": "上游拒绝了请求"})
					return
				case text == "die":
					os.Exit(3)
				case text == "local":
					reply := ask("item/commandExecution/requestApproval", map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "cmd-1"})
					notify("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m-local", "text": "local:" + string(reply)}, "threadId": threadID, "turnId": turnID})
					done("completed", nil)
					return
				case text == "question":
					// 模型向用户提问（request_user_input）：没人在场，设备须答空的 answers 让这一轮往下走。
					reply := ask("item/tool/requestUserInput", map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "q-1", "isBlocking": true,
						"questions": []map[string]any{{"id": "q1", "header": "Which", "question": "哪一个？"}}})
					notify("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m-question", "text": "question:" + string(reply)}, "threadId": threadID, "turnId": turnID})
					done("completed", nil)
					return
				}
				cmd := strings.TrimPrefix(text, "run: ")
				notify("item/started", map[string]any{"item": map[string]any{"type": "reasoning", "id": "r1", "summary": []any{}}, "threadId": threadID, "turnId": turnID})
				notify("item/completed", map[string]any{"item": map[string]any{"type": "reasoning", "id": "r1", "summary": []map[string]any{{"type": "summary_text", "text": "先看看主机"}}}, "threadId": threadID, "turnId": turnID})
				notify("item/started", map[string]any{"item": map[string]any{"type": "mcpToolCall", "id": "call_1", "server": "host", "tool": "exec", "status": "inProgress", "arguments": map[string]any{"command": cmd}}, "threadId": threadID, "turnId": turnID})
				reply := ask("mcpServer/elicitation/request", map[string]any{"threadId": threadID, "turnId": turnID, "serverName": "host", "mode": "form",
					"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_params": map[string]any{"command": cmd}}, "message": "Allow?"})
				var decision struct {
					Action string `json:"action"`
				}
				_ = json.Unmarshal(reply, &decision)
				result := "(rejected)"
				if decision.Action == "accept" {
					res, err := mcpCall("tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": cmd}})
					if err != nil {
						result = "mcp error: " + err.Error()
					} else {
						var r struct {
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						}
						_ = json.Unmarshal(res, &r)
						if len(r.Content) > 0 {
							result = r.Content[0].Text
						}
					}
				}
				notify("item/completed", map[string]any{"item": map[string]any{"type": "mcpToolCall", "id": "call_1", "server": "host", "tool": "exec", "status": "completed"}, "threadId": threadID, "turnId": turnID})
				diag := fmt.Sprintf("|base=%s|key=%s|model=%s|effort=%s|images=%d|", base, os.Getenv("LLMGATE_API_KEY"), model, p.Effort, images)
				full := result + diag
				notify("item/started", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m1", "text": ""}, "threadId": threadID, "turnId": turnID})
				for _, part := range []string{full[:len(full)/2], full[len(full)/2:]} {
					dp := map[string]any{"itemId": "m1", "delta": part}
					for k, v := range tp {
						dp[k] = v
					}
					notify("item/agentMessage/delta", dp)
				}
				notify("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m1", "text": full}, "threadId": threadID, "turnId": turnID})
				notify("thread/tokenUsage/updated", map[string]any{"threadId": threadID, "turnId": turnID, "tokenUsage": map[string]any{
					"total": map[string]any{"inputTokens": 100 * turnN, "outputTokens": 10 * turnN}, "last": map[string]any{"inputTokens": 100, "outputTokens": 10}}})
				done("completed", nil)
			}()
		default:
			send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "unknown method " + msg.Method}})
		}
	}
	return 0
}

// installFakeSlot 铺一棵真实布局的槽位树：入口与 bwrap 是本测试二进制的副本。
func installFakeSlot(t *testing.T, componentsDir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(componentsDir, codexappserver.ComponentName, "slots", "a")
	files := map[string]string{
		codexappserver.EntrypointPath:  "self",
		"bin/codex-code-mode-host":     "placeholder",
		codexappserver.PackageMetaFile: `{"layoutVersion":1,"version":"0.154.0","entrypoint":"bin/codex-app-server"}`,
		"codex-path/rg":                "placeholder",
		"codex-resources/bwrap":        "self",
		"codex-resources/zsh/bin/zsh":  "placeholder",
	}
	for rel, src := range files {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte(src)
		if src == "self" {
			data = raw
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(componentsDir, codexappserver.ComponentName, "current")
	if err := os.Symlink(filepath.Join("slots", "a"), link); err != nil {
		t.Fatal(err)
	}
}

// nullEngine 是槽位引擎的空实现：运行期只读目录，不经引擎。
type nullEngine struct{}

func (nullEngine) ComponentStatus(context.Context, string) (*updated.ComponentStatus, error) {
	return &updated.ComponentStatus{}, nil
}
func (nullEngine) ComponentInstall(context.Context, string, updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	return nil, errors.New("unsupported")
}
func (nullEngine) ComponentRollback(context.Context, string) (*updated.ComponentStatus, error) {
	return nil, errors.New("unsupported")
}
func (nullEngine) ComponentRemove(context.Context, string) (*updated.ComponentStatus, error) {
	return nil, errors.New("unsupported")
}

// codexEnv 把假 app-server 接进真的主机智能体（假主机 + 真 MCP 端点）。
func codexEnv(t *testing.T, cfg hostagent.KeyConfig) (*env, *codexappserver.Manager) {
	t.Helper()
	root := t.TempDir()
	comps := filepath.Join(root, "components")
	installFakeSlot(t, comps)
	var cfgMu sync.Mutex
	current := cfg
	e := newEnv(t, hostagent.Options{})
	mgr := codexappserver.NewManager(codexappserver.Options{DataDir: filepath.Join(root, "data"), Settings: e.st, Engine: nullEngine{},
		ComponentsDir: comps, ClientVersion: "test", Logger: slog.New(slog.DiscardHandler)})
	eng := hostagent.NewCodexEngine(hostagent.CodexOptions{Manager: mgr, BaseURL: "http://127.0.0.1:8099", Config: func(context.Context, hostagent.ChatConfig) (hostagent.KeyConfig, error) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return current, nil
	}})
	hostagent.SetEngineForTest(e.m, eng)
	// 换了引擎后重开一个对话：模型 / 档位由 Codex 引擎的 Validate 落实。
	if cfg.Ready {
		e.chat = e.newChat(hostagent.NewChat{KeyID: 1, Model: cfg.Model, Effort: cfg.Effort})
	}
	return e, mgr
}

func TestCodexEngineStatus(t *testing.T) {
	e, _ := codexEnv(t, hostagent.KeyConfig{Ready: false, Reason: "密钥已停用", KeyDisplay: "sk_x…0000"})
	// 组件装好即就绪；密钥不可用只让新建对话失败（带密钥展示串与原因）。
	st := e.m.EngineStatus(context.Background())
	if st.ID != hostagent.CodexEngineID || !st.Ready || st.Reason != "" {
		t.Fatalf("status = %+v", st)
	}
	var he *hostagent.Error
	if _, err := e.m.CreateChat(context.Background(), e.host.ID, hostagent.NewChat{KeyID: 1}); !errors.As(err, &he) || he.Code != hostagent.CodeKeyInvalid || he.Msg != "密钥已停用" {
		t.Fatalf("密钥不可用时新建对话 err = %v", err)
	}
	for _, in := range []hostagent.NewChat{{KeyID: 0}, {KeyID: 1, Effort: "max"}} {
		if _, err := e.m.CreateChat(context.Background(), e.host.ID, in); !errors.As(err, &he) || (he.Code != hostagent.CodeKeyInvalid && he.Code != hostagent.CodeInvalidInput) {
			t.Fatalf("新建对话 %+v err = %v", in, err)
		}
	}
	// 组件没装：原因换成安装提示。
	e2 := newEnv(t, hostagent.Options{})
	mgr := codexappserver.NewManager(codexappserver.Options{DataDir: t.TempDir(), Settings: e2.st, Engine: nullEngine{}, ComponentsDir: t.TempDir(), Logger: slog.New(slog.DiscardHandler)})
	hostagent.SetEngineForTest(e2.m, hostagent.NewCodexEngine(hostagent.CodexOptions{Manager: mgr, Config: func(context.Context, hostagent.ChatConfig) (hostagent.KeyConfig, error) {
		return hostagent.KeyConfig{Ready: true}, nil
	}}))
	if st := e2.m.EngineStatus(context.Background()); st.Ready || !strings.Contains(st.Reason, "尚未安装") {
		t.Fatalf("未装 status = %+v", st)
	}
	if got := hostagent.LoopbackBaseURL("0.0.0.0:80"); got != "http://127.0.0.1:80" {
		t.Fatalf("LoopbackBaseURL = %q", got)
	}
	if got := hostagent.LoopbackBaseURL("192.168.1.10:8080"); got != "http://192.168.1.10:8080" {
		t.Fatalf("LoopbackBaseURL(具体地址) = %q", got)
	}
	if got := hostagent.LoopbackBaseURL("[::]:80"); got != "http://127.0.0.1:80" {
		t.Fatalf("LoopbackBaseURL(::) = %q", got)
	}
}

func TestCodexEngineRunsInstructionThroughMCP(t *testing.T) {
	e, mgr := codexEnv(t, hostagent.KeyConfig{Ready: true, KeyPlaintext: "sk_test_plain", KeyDisplay: "sk_test…lain", Model: "gpt-5.5", Effort: "high"})
	ctx := context.Background()
	r1, err := e.submit(ctx, hostagent.Input{Text: "run: uname -a", Images: []string{"data:image/png;base64,iVBORw0KGgo="}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	r2, err := e.submit(ctx, hostagent.Input{Text: "run: false"})
	if err != nil {
		t.Fatalf("Submit(2): %v", err)
	}
	e.waitUntil(func(st hostagent.State) bool {
		return !store.AgentRunActive(e.run(r1.ID).Status) && !store.AgentRunActive(e.run(r2.ID).Status)
	})
	if got := e.run(r1.ID); got.Status != store.AgentRunSucceeded || got.InputTokens != 100 || got.OutputTokens != 10 || got.Engine != hostagent.CodexEngineID || got.Model != "gpt-5.5" {
		t.Fatalf("r1 = %+v", got)
	}
	if got := e.run(r2.ID); got.Status != store.AgentRunSucceeded || got.InputTokens != 100 || got.OutputTokens != 10 {
		t.Fatalf("r2（按差值记用量）= %+v", got)
	}
	if mgr.RunningCount() != 1 {
		t.Fatalf("会话未结束前应有 1 个实例在跑，实际 %d", mgr.RunningCount())
	}
	msgs := e.events(store.AgentEventAssistant)
	if len(msgs) != 2 {
		t.Fatalf("assistant 事件 = %+v", msgs)
	}
	// 回复里带着假 app-server 看到的配置：provider 指回环 Codex 面、Key 经环境变量、模型与档位、图片入参。
	if body := msgs[0].Body; !strings.Contains(body, "exit_code: 0") || !strings.Contains(body, "Linux fakehost") ||
		!strings.Contains(body, "|base=http://127.0.0.1:8099/agents/codex/v1|") || !strings.Contains(body, "|key=sk_test_plain|") ||
		!strings.Contains(body, "|model=gpt-5.5|effort=high|images=1|") {
		t.Fatalf("回复 = %q", body)
	}
	if body := msgs[1].Body; !strings.Contains(body, "exit_code: 1") || !strings.Contains(body, "images=0") {
		t.Fatalf("回复 2 = %q", body)
	}
	cmds := e.events(store.AgentEventCommand)
	if len(cmds) != 2 || cmds[0].Title != "uname -a" || cmds[1].Title != "false" || cmds[0].RunID != r1.ID || cmds[1].RunID != r2.ID {
		t.Fatalf("command 事件 = %+v", cmds)
	}
	if rs := e.events(store.AgentEventReasoning); len(rs) != 2 || rs[0].Body != "先看看主机" {
		t.Fatalf("reasoning 事件 = %+v", rs)
	}
	// 实例目录里的 config.toml 不含 Key 与令牌。
	homes, _ := filepath.Glob(filepath.Join(mgr.RuntimeInfo().Entrypoint, "..", "..", "..", "..", "..", "data", "components", "codex-app-server", "homes", "home-*", "config.toml"))
	for _, h := range homes {
		raw, _ := os.ReadFile(h)
		if bytes.Contains(raw, []byte("sk_test_plain")) || bytes.Contains(raw, []byte("LLMGATE_MCP_TOKEN=")) {
			t.Fatalf("config.toml 泄漏凭据: %s", raw)
		}
	}
	if err := e.m.Stop(ctx, e.host.ID, e.chat.ID); err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return st.EngineStartedAt == nil && st.Status == hostagent.StatusIdle })
	if mgr.RunningCount() != 0 {
		t.Fatalf("结束会话后实例应已收尾，实际 %d", mgr.RunningCount())
	}
}

func TestCodexEngineInterruptFailureAndCrash(t *testing.T) {
	e, mgr := codexEnv(t, hostagent.KeyConfig{Ready: true, KeyPlaintext: "sk_x", Model: "gpt-5.5"})
	ctx := context.Background()
	// 中止：turn/interrupt → interrupted → 指令记为取消。
	hang, _ := e.submit(ctx, hostagent.Input{Text: "hang"})
	e.waitUntil(func(st hostagent.State) bool {
		return st.Status == hostagent.StatusRunning && st.Current != nil && st.Current.ID == hang.ID
	})
	if err := e.m.Cancel(ctx, e.host.ID, e.chat.ID, hang.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(hang.ID).Status == store.AgentRunCancelled })
	// 本地命令审批被拒。
	local, _ := e.submit(ctx, hostagent.Input{Text: "local"})
	e.waitUntil(func(st hostagent.State) bool { return e.run(local.ID).Status == store.AgentRunSucceeded })
	msgs := e.events(store.AgentEventAssistant)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, `"decision":"decline"`) {
		t.Fatalf("本地审批应被拒: %+v", msgs)
	}
	// 模型提问：答空 answers，这一轮照常结束而不是挂住。
	question, _ := e.submit(ctx, hostagent.Input{Text: "question"})
	e.waitUntil(func(st hostagent.State) bool { return e.run(question.ID).Status == store.AgentRunSucceeded })
	msgs = e.events(store.AgentEventAssistant)
	if len(msgs) != 2 || !strings.Contains(msgs[1].Body, `question:{"answers":{}}`) {
		t.Fatalf("提问应答空 answers: %+v", msgs)
	}
	// 引擎侧失败：error(willRetry=false) + failed。
	fail, _ := e.submit(ctx, hostagent.Input{Text: "fail"})
	e.waitUntil(func(st hostagent.State) bool { return e.run(fail.ID).Status == store.AgentRunFailed })
	if got := e.run(fail.ID); got.Error != "上游拒绝了请求" {
		t.Fatalf("fail = %+v", got)
	}
	// 进程意外退出：指令失败、会话关闭、下一条指令起新实例。
	die, _ := e.submit(ctx, hostagent.Input{Text: "die"})
	e.waitUntil(func(st hostagent.State) bool {
		return e.run(die.ID).Status == store.AgentRunFailed && st.EngineStartedAt == nil
	})
	if got := e.run(die.ID); !strings.Contains(got.Error, "退出") {
		t.Fatalf("die = %+v", got)
	}
	sessions := e.events(store.AgentEventSession)
	if len(sessions) != 2 || sessions[1].Title != "stopped" || !strings.Contains(sessions[1].Body, "退出") {
		t.Fatalf("session 事件 = %+v", sessions)
	}
	again, _ := e.submit(ctx, hostagent.Input{Text: "run: uname -a"})
	e.waitUntil(func(st hostagent.State) bool { return e.run(again.ID).Status == store.AgentRunSucceeded })
	if mgr.RunningCount() != 1 {
		t.Fatalf("新会话应有 1 个实例，实际 %d", mgr.RunningCount())
	}
	e.m.Shutdown(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for mgr.RunningCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.RunningCount() != 0 {
		t.Fatalf("Shutdown 后仍有实例: %d", mgr.RunningCount())
	}
}

// TestCodexRealBinary 用真的 app-server 整包（环境变量 LLMGATE_CODEX_APP_SERVER_DIR 指向解开的目录，
// 含 bin/codex-app-server 等）对一个假的 Responses 端点跑一遍：假模型先调 MCP 的 exec，再回一句。
// 验证的是与真二进制的接线（config.toml 渲染、MCP 令牌、审批往返、通知形态），不出网；缺省 skip。
func TestCodexRealBinary(t *testing.T) {
	dir := os.Getenv("LLMGATE_CODEX_APP_SERVER_DIR")
	if dir == "" {
		t.Skip("LLMGATE_CODEX_APP_SERVER_DIR 未设置")
	}
	root := t.TempDir()
	comps := filepath.Join(root, "components")
	slot := filepath.Join(comps, codexappserver.ComponentName, "slots", "a")
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, slot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("slots", "a"), filepath.Join(comps, codexappserver.ComponentName, "current")); err != nil {
		t.Fatal(err)
	}
	// 假 Responses 端点：第一轮回一个调用 mcp__host.exec 的 function_call，第二轮回消息。
	var reqs []map[string]any
	var reqMu sync.Mutex
	responses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqMu.Lock()
		reqs = append(reqs, req)
		n := len(reqs)
		reqMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk_real_test" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		hasOutput := false
		if in, ok := req["input"].([]any); ok {
			for _, it := range in {
				if m, ok := it.(map[string]any); ok && m["type"] == "function_call_output" {
					hasOutput = true
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(ev string, v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
			w.(http.Flusher).Flush()
		}
		emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("resp_%d", n)}})
		var item map[string]any
		if !hasOutput {
			item = map[string]any{"type": "function_call", "name": "exec", "namespace": "mcp__host", "arguments": `{"command":"uname -a"}`, "call_id": "call_1", "id": "fc_1"}
		} else {
			item = map[string]any{"type": "message", "role": "assistant", "id": "msg_1", "content": []any{map[string]any{"type": "output_text", "text": "内核已查到。"}}}
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "item": item})
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
		emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", n),
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}})
	}))
	t.Cleanup(responses.Close)

	e := newEnv(t, hostagent.Options{})
	mgr := codexappserver.NewManager(codexappserver.Options{DataDir: filepath.Join(root, "data"), Settings: e.st, Engine: nullEngine{},
		ComponentsDir: comps, ClientVersion: "test", Logger: slog.New(slog.DiscardHandler)})
	hostagent.SetEngineForTest(e.m, hostagent.NewCodexEngine(hostagent.CodexOptions{Manager: mgr, BaseURL: responses.URL,
		Config: func(context.Context, hostagent.ChatConfig) (hostagent.KeyConfig, error) {
			return hostagent.KeyConfig{Ready: true, KeyPlaintext: "sk_real_test", KeyDisplay: "sk_real…test", Model: "gpt-5-codex", Effort: "medium"}, nil
		}}))
	e.chat = e.newChat(hostagent.NewChat{KeyID: 1, Model: "gpt-5-codex", Effort: "medium"})
	ctx := context.Background()
	run, err := e.submit(ctx, hostagent.Input{Text: "看看内核", Images: []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && store.AgentRunActive(e.run(run.ID).Status) {
		time.Sleep(200 * time.Millisecond)
	}
	got := e.run(run.ID)
	if got.Status != store.AgentRunSucceeded {
		t.Fatalf("run = %+v；事件 = %+v", got, e.events())
	}
	cmds := e.events(store.AgentEventCommand)
	if len(cmds) != 1 || cmds[0].Title != "uname -a" || cmds[0].ExitCode == nil || *cmds[0].ExitCode != 0 {
		t.Fatalf("command 事件 = %+v", cmds)
	}
	msgs := e.events(store.AgentEventAssistant)
	if len(msgs) != 1 || msgs[0].Body != "内核已查到。" {
		t.Fatalf("assistant 事件 = %+v", msgs)
	}
	reqMu.Lock()
	defer reqMu.Unlock()
	if len(reqs) != 2 || reqs[0]["model"] != "gpt-5-codex" {
		t.Fatalf("Responses 请求 = %d 条, 模型 %v", len(reqs), reqs[0]["model"])
	}
	if reasoning, ok := reqs[0]["reasoning"].(map[string]any); !ok || reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %v", reqs[0]["reasoning"])
	}
	e.m.Stop(ctx, e.host.ID, e.chat.ID)
	e.waitUntil(func(st hostagent.State) bool { return st.EngineStartedAt == nil })
}
