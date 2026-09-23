package nodeengine

// 节点端引擎的离线验收：假工作节点（agenthosttest：真跑 SSH 协议、真开远程转发）把准备脚本、
// 包装脚本与收尾命令交给本机 /bin/sh 在临时家目录里跑；「节点上的 CLI」是本测试二进制按
// helper-process 分身出来的假 codex app-server / 假 claude（协议形态按真机抓取）。假 CLI 经远程
// 转发访问「设备」（一个 httptest 服务器），验证：
//   - 转发只放行开发工具接入面与这段会话的 MCP 端点，请求带对话钉死的密钥与会话令牌；
//   - 密钥与令牌不在 CLI 的环境变量里，也不在 argv 里；Claude 走 --bare 与空的 --setting-sources；
//   - 本地命令 / 改文件经 ExecSink 报回；中止、会话关闭后实例目录删干净、遗留目录被清掉。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	testKey      = "sk_node_test_secret"
	testPassword = "n0t-a-real-password"
)

// ---- 假 CLI（helper process） ----

// TestHelperProcess 是节点上「CLI」的分身：GO_WANT_HELPER_PROCESS=1 时按参数扮演 codex app-server
// 或 claude，平时什么也不做。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 0 && args[0] == "app-server" {
		fakeCodex()
	} else if strings.Contains(strings.Join(args, " "), "agent stdio") {
		fakeGrok(args)
	} else {
		fakeClaude(args)
	}
	os.Exit(0)
}

// probeDevice 从节点上经转发访问设备：MCP（带令牌）、模型接入面（带密钥）、管理面（应被挡）。
// 结果是一行摘要，假 CLI 把它写进回复里交给测试断言。
func probeDevice(base, mcpURL, mcpAuth, modelPath, keyHeader, key string) string {
	var parts []string
	do := func(method, url string, headers map[string]string) int {
		req, _ := http.NewRequest(method, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	parts = append(parts, fmt.Sprintf("mcp=%d", do("POST", mcpURL, map[string]string{"Authorization": mcpAuth})))
	parts = append(parts, fmt.Sprintf("model=%d", do("POST", base+modelPath, map[string]string{keyHeader: key})))
	parts = append(parts, fmt.Sprintf("admin=%d", do("GET", base+"/admin/v1/session", nil)))
	parts = append(parts, fmt.Sprintf("dotdot=%d", do("GET", base+"/agents/claude/../../admin/v1/session", nil)))
	leak := "env-clean"
	for _, kv := range os.Environ() {
		if strings.Contains(kv, testKey) {
			leak = "env-leak"
		}
	}
	parts = append(parts, leak)
	wd, _ := os.Getwd()
	parts = append(parts, "cwd="+wd)
	return strings.Join(parts, " ")
}

func fakeCodex() {
	home := os.Getenv("CODEX_HOME")
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	find := func(re string) string {
		m := regexp.MustCompile(re).FindSubmatch(cfg)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	base := strings.TrimSuffix(find(`base_url = "([^"]+)"`), "/agents/codex/v1")
	mcpURL := find(`(?m)^url = "([^"]+)"`)
	mcpAuth := find(`Authorization = "([^"]+)"`)
	key := find(`experimental_bearer_token = "([^"]+)"`)
	// 「stubborn」模型：扮演不按 stdin EOF 退出的 CLI，还带一个子进程（模型起的 shell），
	// 两个 pid 写进工作空间给测试核对它们最后有没有被杀掉。
	stubborn := strings.Contains(find(`(?m)^model = "([^"]+)"`), "stubborn")
	if stubborn {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err == nil {
			os.WriteFile("stubborn.pid", []byte(fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)), 0o600)
		}
	}
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	threadCwd := ""
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		reply := func(result any) { out.Encode(map[string]any{"id": msg.ID, "result": result}) }
		notify := func(method string, params any) { out.Encode(map[string]any{"method": method, "params": params}) }
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fake-codex/0.1", "codexHome": home})
		case "thread/start":
			threadCwd, _ = msg.Params["cwd"].(string)
			if msg.Params["sandbox"] != "danger-full-access" {
				out.Encode(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32600, "message": "want danger-full-access"}})
				continue
			}
			reply(map[string]any{"thread": map[string]any{"id": "thr_1"}})
		case "turn/start":
			reply(map[string]any{"turn": map[string]any{"id": "turn_1"}})
			summary := probeDevice(base, mcpURL, mcpAuth, "/agents/codex/v1/responses", "Authorization", "Bearer "+key)
			notify("item/started", map[string]any{"item": map[string]any{"type": "commandExecution", "command": "/bin/bash -lc 'ffprobe media/a.mp4'"}})
			exit := 1
			notify("item/completed", map[string]any{"item": map[string]any{"type": "commandExecution", "command": "/bin/bash -lc 'ffprobe media/a.mp4'",
				"aggregatedOutput": "no such file", "exitCode": exit, "durationMs": 42}})
			notify("item/completed", map[string]any{"item": map[string]any{"type": "fileChange", "changes": []any{
				map[string]any{"path": threadCwd + "/docs/a.md", "kind": map[string]any{"type": "add"}},
				map[string]any{"path": threadCwd + "/docs/b.md", "kind": map[string]any{"type": "delete"}},
			}}})
			notify("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "text": summary}})
			notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 120, "outputTokens": 7}}})
			notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
		}
	}
	if stubborn {
		time.Sleep(300 * time.Second)
	}
}

func fakeClaude(args []string) {
	joined := strings.Join(args, " ")
	value := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	var settings struct {
		APIKeyHelper string `json:"apiKeyHelper"`
	}
	_ = json.Unmarshal([]byte(value("--settings")), &settings)
	key, _ := exec.Command("/bin/sh", "-c", settings.APIKeyHelper).Output()
	var mcp struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(value("--mcp-config"))
	_ = json.Unmarshal(raw, &mcp)
	instructions, _ := os.ReadFile(value("--append-system-prompt-file"))
	base := strings.TrimSuffix(os.Getenv("ANTHROPIC_BASE_URL"), "/agents/claude")
	flags := []string{}
	for _, want := range []string{"--bare", "--no-session-persistence", "--strict-mcp-config", "--permission-prompt-tool stdio"} {
		if strings.Contains(joined, want) {
			flags = append(flags, want)
		}
	}
	if v := value("--setting-sources"); v == "" && strings.Contains(joined, "--setting-sources") {
		flags = append(flags, "no-setting-sources")
	}
	out := json.NewEncoder(os.Stdout)
	pending := map[string]chan struct{}{}
	var mu sync.Mutex
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	lines := make(chan []byte)
	go func() {
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(lines)
	}()
	interrupted := make(chan struct{}, 1)
	for line := range lines {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					Behavior string `json:"behavior"`
				} `json:"response"`
			} `json:"response"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "control_request":
			out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": msg.RequestID, "response": map[string]any{}}})
			if msg.Request.Subtype == "interrupt" {
				interrupted <- struct{}{}
			}
		case "control_response":
			mu.Lock()
			ch := pending[msg.Response.RequestID]
			mu.Unlock()
			if ch != nil && msg.Response.Response.Behavior == "allow" {
				close(ch)
			}
		case "user":
			text := ""
			if len(msg.Message.Content) > 0 {
				text = msg.Message.Content[0].Text
			}
			go func(text string) {
				if text == "slow" {
					out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_s", "name": "Bash", "input": map[string]any{"command": "sleep 60"}}}}})
					<-interrupted
					out.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "usage": map[string]any{}})
					return
				}
				// 要用 Bash：先问设备，放行后才「执行」。
				ok := make(chan struct{})
				mu.Lock()
				pending["perm1"] = ok
				mu.Unlock()
				out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
					map[string]any{"type": "thinking", "thinking": "先看看文件"},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ffmpeg -i media/a.mp4"}},
				}}})
				out.Encode(map[string]any{"type": "control_request", "request_id": "perm1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ffmpeg -i media/a.mp4"}}})
				select {
				case <-ok:
				case <-time.After(10 * time.Second):
					return
				}
				out.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": true, "content": "Exit code 2\nboom"}}}})
				out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Edit", "input": map[string]any{"file_path": "/ws/docs/a.md"}}}}})
				out.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": []any{map[string]any{"type": "text", "text": "ok"}}}}}})
				summary := probeDevice(base, mcp.MCPServers["studio"].URL, mcp.MCPServers["studio"].Headers["Authorization"], "/agents/claude/v1/messages", "x-api-key", strings.TrimSpace(string(key)))
				summary += " flags=" + strings.Join(flags, ",") + " model=" + value("--model") + " effort=" + value("--effort") + " instr=" + strings.TrimSpace(string(instructions))
				summary += " configdir=" + fmt.Sprint(strings.HasSuffix(os.Getenv("CLAUDE_CONFIG_DIR"), "/claude"))
				out.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "part"}}})
				out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": summary}}}})
				out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": summary,
					"usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": 90, "cache_creation_input_tokens": 5, "output_tokens": 3}})
			}(text)
		}
	}
}

// ---- 假工作节点与设备 ----

type device struct {
	srv *httptest.Server
	mu  sync.Mutex
	// hits 按路径记设备收到的请求与鉴权头。
	hits map[string]string
}

func newDevice(t *testing.T, token *string) *device {
	d := &device{hits: map[string]string{}}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization") + r.Header.Get("x-api-key")
		d.mu.Lock()
		d.hits[r.URL.Path] = auth
		d.mu.Unlock()
		switch r.URL.Path {
		case "/studio-mcp":
			if auth != "Bearer "+*token {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
		case "/agents/codex/v1/responses", "/agents/claude/v1/messages", "/agents/grok/v1/responses":
			if !strings.Contains(auth, testKey) {
				http.Error(w, "bad key", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

type nodeEnv struct {
	t      *testing.T
	home   string
	fake   *agenthosttest.Server
	hosts  *agenthost.Manager
	hostID int64
	dev    *device
	token  string
	bin    string
	opts   Options
	ws     string
}

// newNodeEnv 起一台假工作节点并纳管：它的流式命令与准备 / 收尾脚本交给本机 /bin/sh，在临时家目录
// 里跑（HOME 指过去），PATH 上什么引擎也没有——程序是一段转调本测试二进制的脚本。
func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	ws := filepath.Join(home, "workspaces", "poster")
	for _, d := range []string{ws + "/media", ws + "/docs", ws + "/agent"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "cli")
	script := "#!/bin/sh\nGO_WANT_HELPER_PROCESS=1 exec '" + self + "' -test.run='^TestHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME=")
	shell := func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Env, cmd.Dir = env, home
		cmd.Stdout, cmd.Stderr = stdout, stderr
		// stdin 自己搬：进程退出时不等这一侧（真 sshd 在命令退出时就关通道，不管设备还开着 stdin）。
		w, err := cmd.StdinPipe()
		if err != nil {
			return 127
		}
		go func() {
			io.Copy(w, stdin)
			w.Close()
		}()
		err = cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			return 127
		}
		return 0
	}
	fake := agenthosttest.New(t, agenthosttest.Options{Password: testPassword, Username: "dev",
		CommandStderr: func(command, stdin string) (string, string, int, bool) {
			if command != "/bin/sh -s" && !strings.HasPrefix(command, "rm -rf '") {
				return "", "", 0, false
			}
			var out, errOut strings.Builder
			code := shell(command, strings.NewReader(stdin), &out, &errOut)
			return out.String(), errOut.String(), code, true
		},
		Stream: func(command string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
			if !strings.HasPrefix(command, "/bin/sh -c ") {
				return 0, false
			}
			return shell(command, stdin, stdout, stderr), true
		},
	})
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hosts := agenthost.New(agenthost.Options{Store: st, Dial: fake.Dial})
	ctx := context.Background()
	if _, err := hosts.GenerateCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	addr, port := fake.Addr()
	host, err := hosts.Enroll(ctx, agenthost.EnrollRequest{Name: "node", Kind: store.AgentHostKindWorker, Address: addr, Port: port, Username: "dev", Password: testPassword})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	e := &nodeEnv{t: t, home: home, fake: fake, hosts: hosts, hostID: host.ID, bin: bin, ws: ws, token: "tok_node_session"}
	e.dev = newDevice(t, &e.token)
	e.opts = Options{Hosts: hosts, Upstream: e.dev.srv.URL, Version: "test",
		Resolve: func(_ context.Context, tool string, cfg hostagent.ChatConfig) (hostagent.KeyConfig, error) {
			if cfg.KeyID != 7 {
				return hostagent.KeyConfig{Reason: "密钥不存在"}, nil
			}
			return hostagent.KeyConfig{Ready: true, KeyPlaintext: testKey, KeyDisplay: "sk_node…cret", Model: tool + "-model", Effort: cfg.Effort}, nil
		}}
	return e
}

func (e *nodeEnv) request(effort string) Request {
	return Request{HostID: e.hostID, Binary: e.bin, Workdir: e.ws, Chat: hostagent.ChatConfig{KeyID: 7, Effort: effort},
		Instructions: "开发者指令-标记", Tools: hostagent.ToolEndpoint{URL: "/studio-mcp", Token: e.token, Server: "studio"}}
}

// instanceDirs 是家目录下还在的实例目录。
func (e *nodeEnv) instanceDirs() []string {
	dirs, _ := filepath.Glob(filepath.Join(e.home, ".cache", "llmgate", "engine", "s.*"))
	return dirs
}

// sink 记下引擎报来的全部事实。
type sink struct {
	mu        sync.Mutex
	deltas    []string
	messages  []string
	reasoning []string
	commands  []string
	files     []string
	tools     []string
	in, out   int64
}

func (s *sink) Delta(text string) { s.mu.Lock(); s.deltas = append(s.deltas, text); s.mu.Unlock() }
func (s *sink) Message(text string) {
	s.mu.Lock()
	s.messages = append(s.messages, text)
	s.mu.Unlock()
}
func (s *sink) Reasoning(text string) {
	s.mu.Lock()
	s.reasoning = append(s.reasoning, text)
	s.mu.Unlock()
}
func (s *sink) Activity(string)     {}
func (s *sink) Usage(in, out int64) { s.mu.Lock(); s.in, s.out = in, out; s.mu.Unlock() }
func (s *sink) Command(command, output string, code *int, dur time.Duration) {
	c := "nil"
	if code != nil {
		c = fmt.Sprint(*code)
	}
	s.mu.Lock()
	s.commands = append(s.commands, fmt.Sprintf("%s|%s|%s", command, c, output))
	s.mu.Unlock()
}
func (s *sink) FileChange(path, action string) {
	s.mu.Lock()
	s.files = append(s.files, action+":"+path)
	s.mu.Unlock()
}
func (s *sink) ToolUse(name, target string) {
	s.mu.Lock()
	s.tools = append(s.tools, name+":"+target)
	s.mu.Unlock()
}

// checkProbe 断言假 CLI 从节点上探到的设备可达性。
func (e *nodeEnv) checkProbe(summary string) {
	e.t.Helper()
	for _, want := range []string{"mcp=200", "model=200", "admin=404", "dotdot=404", "env-clean", "cwd=" + e.ws} {
		if !strings.Contains(summary, want) {
			e.t.Fatalf("节点上的探测结果缺 %q：%s", want, summary)
		}
	}
	e.dev.mu.Lock()
	defer e.dev.mu.Unlock()
	if _, hit := e.dev.hits["/admin/v1/session"]; hit {
		e.t.Fatal("管理面不该经转发到达设备")
	}
}

func TestCodexOnNode(t *testing.T) {
	e := newNodeEnv(t)
	eng := NewCodex(e.opts)
	if _, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 7, Effort: "max"}); err == nil {
		t.Fatal("Codex 不该接受 max 档位")
	}
	if _, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 8}); err == nil {
		t.Fatal("不就绪的密钥应被拒")
	}
	// 上一次异常退出留下的实例目录（pid 已不在）：准备脚本顺手清掉。
	stale := filepath.Join(e.home, ".cache", "llmgate", "engine", "s.stale000")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stale, "pid"), []byte("999999\n"), 0o600)
	sess, err := eng.Start(t.Context(), e.request("high"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("遗留的实例目录没被清掉")
	}
	dirs := e.instanceDirs()
	if len(dirs) != 1 {
		t.Fatalf("实例目录 = %v", dirs)
	}
	if info, err := os.Stat(filepath.Join(dirs[0], "config.toml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.toml 权限 = %v / %v", info, err)
	}
	cfgBytes, _ := os.ReadFile(filepath.Join(dirs[0], "config.toml"))
	for _, want := range []string{"web_search = \"disabled\"\n", "[features]\nmulti_agent = false\n", "[history]\npersistence = \"none\"\n", "[analytics]\nenabled = false\n"} {
		if !strings.Contains(string(cfgBytes), want) {
			t.Fatalf("config.toml 缺 %q：\n%s", want, cfgBytes)
		}
	}
	var s sink
	out, err := sess.Run(t.Context(), hostagent.Input{Text: "处理素材"}, &s)
	if err != nil || out.Status != hostagent.OutcomeCompleted {
		t.Fatalf("Run = %+v / %v", out, err)
	}
	if len(s.messages) != 1 {
		t.Fatalf("回复 = %v", s.messages)
	}
	e.checkProbe(s.messages[0])
	if len(s.commands) != 1 || s.commands[0] != "ffprobe media/a.mp4|1|no such file" {
		t.Fatalf("命令 = %v", s.commands)
	}
	if strings.Join(s.files, ",") != "write:"+e.ws+"/docs/a.md,delete:"+e.ws+"/docs/b.md" {
		t.Fatalf("改文件 = %v", s.files)
	}
	if s.in != 120 || s.out != 7 {
		t.Fatalf("用量 = %d / %d", s.in, s.out)
	}
	// 密钥不在任何一条命令的 argv 里。
	for _, x := range e.fake.Execs() {
		if strings.Contains(x.Command, testKey) || strings.Contains(x.Command, e.token) {
			t.Fatalf("密钥或令牌进了 argv：%q", x.Command)
		}
	}
	sess.Close()
	waitFor(t, func() bool { return len(e.instanceDirs()) == 0 })
}

func TestClaudeOnNode(t *testing.T) {
	e := newNodeEnv(t)
	eng := NewClaude(e.opts)
	if _, err := eng.Validate(t.Context(), hostagent.ChatConfig{KeyID: 7, Effort: "max"}); err != nil {
		t.Fatalf("Claude 应接受 max 档位: %v", err)
	}
	sess, err := eng.Start(t.Context(), e.request("max"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	dirs := e.instanceDirs()
	if len(dirs) != 1 {
		t.Fatalf("实例目录 = %v", dirs)
	}
	for _, name := range []string{"key", "mcp.json", "instructions.md"} {
		if info, err := os.Stat(filepath.Join(dirs[0], name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s 权限 = %v / %v", name, info, err)
		}
	}
	var s sink
	out, err := sess.Run(t.Context(), hostagent.Input{Text: "处理素材", Images: []string{"data:image/png;base64,iVBORw0KGgo="}}, &s)
	if err != nil || out.Status != hostagent.OutcomeCompleted {
		t.Fatalf("Run = %+v / %v", out, err)
	}
	if len(s.messages) != 1 {
		t.Fatalf("回复 = %v", s.messages)
	}
	summary := s.messages[0]
	e.checkProbe(summary)
	for _, want := range []string{"flags=--bare,--no-session-persistence,--strict-mcp-config,--permission-prompt-tool stdio,no-setting-sources",
		"model=claude-model", "effort=max", "instr=开发者指令-标记", "configdir=true"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("回复缺 %q：%s", want, summary)
		}
	}
	if len(s.deltas) != 1 || len(s.reasoning) != 1 || s.reasoning[0] != "先看看文件" {
		t.Fatalf("增量 / 推理 = %v / %v", s.deltas, s.reasoning)
	}
	if len(s.commands) != 1 || s.commands[0] != "ffmpeg -i media/a.mp4|2|boom" {
		t.Fatalf("命令 = %v", s.commands)
	}
	if len(s.files) != 1 || s.files[0] != "write:/ws/docs/a.md" {
		t.Fatalf("改文件 = %v", s.files)
	}
	if s.in != 105 || s.out != 3 {
		t.Fatalf("用量 = %d / %d", s.in, s.out)
	}
	// 中止：这一轮以 interrupted 收尾，会话还能用。
	done := make(chan hostagent.Outcome, 1)
	go func() {
		o, _ := sess.Run(t.Context(), hostagent.Input{Text: "slow"}, &sink{})
		done <- o
	}()
	time.Sleep(200 * time.Millisecond)
	if err := sess.Interrupt(t.Context()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case o := <-done:
		if o.Status != hostagent.OutcomeInterrupted {
			t.Fatalf("中止后的结局 = %+v", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("中止后这一轮没有收尾")
	}
	for _, x := range e.fake.Execs() {
		if strings.Contains(x.Command, testKey) || strings.Contains(x.Command, e.token) {
			t.Fatalf("密钥或令牌进了 argv：%q", x.Command)
		}
	}
	sess.Close()
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("关闭后会话没有结束")
	}
	waitFor(t, func() bool { return len(e.instanceDirs()) == 0 })
}

// CLI 不按 stdin EOF 退出时：关闭会话须把包装脚本、CLI 与它起的子进程整棵 KILL，实例目录删净。
// 假 sshd 不转发信号，所以这里只能靠收尾脚本按 pid 文件点名。
func TestCloseKillsStubbornCLI(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("需要 /proc 判断进程是否还在")
	}
	e := newNodeEnv(t)
	resolve := e.opts.Resolve
	e.opts.Resolve = func(ctx context.Context, tool string, cfg hostagent.ChatConfig) (hostagent.KeyConfig, error) {
		out, err := resolve(ctx, tool, cfg)
		out.Model = "stubborn-model"
		return out, err
	}
	sess, err := NewCodex(e.opts).Start(t.Context(), e.request(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pidFile := filepath.Join(e.ws, "stubborn.pid")
	var pids []int
	waitFor(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pids = pids[:0]
		for _, f := range strings.Fields(string(raw)) {
			var pid int
			fmt.Sscan(f, &pid)
			pids = append(pids, pid)
		}
		return len(pids) == 2
	})
	alive := func(pid int) bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// 已被 KILL 但还没被收割的僵尸不算活着。
		return !strings.Contains(string(raw), ") Z ")
	}
	for _, pid := range pids {
		if !alive(pid) {
			t.Fatalf("假 CLI 或它的子进程 %d 起来前就没了", pid)
		}
	}
	started := time.Now()
	sess.Close()
	select {
	case <-sess.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("关闭后会话没有结束")
	}
	if time.Since(started) > 12*time.Second {
		t.Fatalf("关闭耗时 %v，收尾脚本没起作用", time.Since(started))
	}
	waitFor(t, func() bool { return !alive(pids[0]) && !alive(pids[1]) })
	waitFor(t, func() bool { return len(e.instanceDirs()) == 0 })
}

// 节点上的程序不在 / 工作空间目录不在：握手失败并说明节点那一侧的原因，实例目录收干净。
func TestStartFailuresOnNode(t *testing.T) {
	e := newNodeEnv(t)
	req := e.request("")
	req.Binary = filepath.Join(e.home, "no-such-cli")
	_, err := NewCodex(e.opts).Start(t.Context(), req)
	var he *hostagent.Error
	if !errors.As(err, &he) || he.Code != hostagent.CodeEngineNotReady || !strings.Contains(he.Msg, "找不到或执行不了") {
		t.Fatalf("程序不在 = %v", err)
	}
	req = e.request("")
	req.Workdir = filepath.Join(e.home, "workspaces", "gone")
	_, err = NewClaude(e.opts).Start(t.Context(), req)
	if !errors.As(err, &he) || !strings.Contains(he.Msg, "工作空间目录在节点上不存在") {
		t.Fatalf("目录不在 = %v", err)
	}
	waitFor(t, func() bool { return len(e.instanceDirs()) == 0 })
}

// 转发只放行开发工具接入面与本会话的 MCP 端点，路径须是规范形态。
func TestProxyAllowlist(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"|"+r.Header.Get("X-Forwarded-For"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	o := Options{Upstream: upstream.URL}
	h, err := o.proxy("/studio-mcp")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{
		"/agents/codex/v1/responses":        http.StatusNoContent,
		"/agents/claude/v1/messages":        http.StatusNoContent,
		"/studio-mcp":                       http.StatusNoContent,
		"/agent-mcp":                        http.StatusNotFound,
		"/admin/v1/session":                 http.StatusNotFound,
		"/v1/chat/completions":              http.StatusNotFound,
		"/agents/claude/../../admin/v1/x":   http.StatusNotFound,
		"/agents/codex//v1/responses":       http.StatusNotFound,
		"/agents/grok/v1/responses":         http.StatusNoContent,
		"/agents/grok/../../admin/v1/x":     http.StatusNotFound,
		"/studio-mcp/../admin/v1/session":   http.StatusNotFound,
		"/agents/claude/./v1/messages":      http.StatusNotFound,
		"/agents/claude/v1/messages/../x/y": http.StatusNotFound,
	} {
		r := httptest.NewRequest(http.MethodPost, "http://node"+path, nil)
		r.URL.Path = path
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s = %d，期望 %d", path, w.Code, want)
		}
	}
	for _, g := range got {
		if !strings.HasSuffix(g, "|") {
			t.Errorf("转发不该带 X-Forwarded-For：%s", g)
		}
	}
}

func TestSetupScriptQuoting(t *testing.T) {
	script, err := setupScript(map[string]string{"a.md": "含 $HOME、`反引号`、'单引号' 与 EOF\nEOF\n", "b": "no newline"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME=")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("准备脚本: %v", err)
	}
	dir := strings.TrimSpace(strings.TrimPrefix(string(out), "dir="))
	if !instanceDirRE.MatchString(dir) {
		t.Fatalf("实例目录 = %q", dir)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(data) != "含 $HOME、`反引号`、'单引号' 与 EOF\nEOF\n" {
		t.Fatalf("a.md = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "b")); string(data) != "no newline\n" {
		t.Fatalf("b = %q", data)
	}
	if _, err := setupScript(map[string]string{"../x": ""}); err == nil {
		t.Fatal("非法文件名应被拒")
	}
}

func TestUnwrapShellCommand(t *testing.T) {
	for in, want := range map[string]string{
		"/bin/bash -lc 'ls -la'":                   "ls -la",
		"bash -c 'echo '\\''hi'\\'''":              "echo 'hi'",
		"/usr/bin/zsh -lc 'ffmpeg -i a.mp4 b.gif'": "ffmpeg -i a.mp4 b.gif",
		"ls": "ls",
	} {
		if got := hostagent.UnwrapShellCommand(in); got != want {
			t.Errorf("UnwrapShellCommand(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("条件未在期限内成立")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
