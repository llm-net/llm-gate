package hostagent_test

// Agent远控验收（假引擎 + 进程内假 SSH 主机，全程离线）：
//   - 对话：新建时钉死密钥 / 模型 / 档位并注入主机档案与最近操作，自动标题，删除只带走
//     对话内容、操作记录留给主机，引擎重启时附上本对话先前的记录；
//   - 提交校验、主机 / 引擎 / 密钥未就绪的拒绝；
//   - 队列按提交顺序逐条执行、不插队；陪等按 revision 回帧；
//   - 引擎经 MCP 端点调用主机工具：命令走 SSH 到假主机、写文件经 stdin、档案读写，
//     每一步都落时间线（操作日志）；令牌 / 回环校验；
//   - 取消排队中与执行中的指令、结束会话、空闲超时关引擎、重启恢复、解除纳管。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const hostPassword = "n0t-a-real-password"

// fakeEngine 是可编程的引擎：script 决定一条指令做什么（可经 MCP 端点调用工具）。
type fakeEngine struct {
	mu       sync.Mutex
	ready    bool
	reason   string
	keyErr   error
	startErr error
	script   func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, interrupt <-chan struct{}) hostagent.Outcome
	sessions []*fakeSession
}

func (e *fakeEngine) ID() string { return "fake" }

func (e *fakeEngine) Status(context.Context) hostagent.EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return hostagent.EngineStatus{ID: "fake", Label: "Fake Engine", Ready: e.ready, Reason: e.reason}
}

// Validate：密钥 id 为正即可；留空的模型落成 fake-1。
func (e *fakeEngine) Validate(_ context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.keyErr != nil {
		return cfg, e.keyErr
	}
	if cfg.KeyID <= 0 {
		return cfg, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: "请为对话选择一把 API 密钥"}
	}
	cfg.KeyDisplay = "sk_fake…0000"
	if cfg.Model == "" {
		cfg.Model = "fake-1"
	}
	return cfg, nil
}

func (e *fakeEngine) Start(_ context.Context, req hostagent.StartRequest) (hostagent.EngineSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.startErr != nil {
		return nil, e.startErr
	}
	s := &fakeSession{e: e, tools: req.Tools, instructions: req.Instructions, chat: req.Chat, done: make(chan struct{})}
	e.sessions = append(e.sessions, s)
	return s, nil
}

type fakeSession struct {
	e            *fakeEngine
	tools        hostagent.ToolEndpoint
	instructions string
	chat         store.AgentHostChat
	mu           sync.Mutex
	interrupt    chan struct{}
	runs         int
	closed       bool
	done         chan struct{}
}

func (s *fakeSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	s.mu.Lock()
	s.runs++
	s.interrupt = make(chan struct{})
	ch := s.interrupt
	s.mu.Unlock()
	return s.e.script(ctx, in, sink, s.tools, ch), nil
}

func (s *fakeSession) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupt == nil {
		return errors.New("no turn")
	}
	select {
	case <-s.interrupt:
	default:
		close(s.interrupt)
	}
	return nil
}

func (s *fakeSession) Done() <-chan struct{} { return s.done }

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

// mcpCall 从测试里像引擎那样调一次工具。
func mcpCall(t *testing.T, tools hostagent.ToolEndpoint, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, tools.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tools.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("MCP %s: %v", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("MCP %s 状态 = %d: %s", method, resp.StatusCode, raw)
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码 MCP 应答: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("MCP %s 错误: %s", method, out.Error.Message)
	}
	return out.Result
}

func toolText(res map[string]any) (string, bool) {
	content, _ := res["content"].([]any)
	text := ""
	if len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text, _ = m["text"].(string)
		}
	}
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

type env struct {
	t      *testing.T
	st     *store.Store
	hosts  *agenthost.Manager
	fake   *agenthosttest.Server
	engine *fakeEngine
	m      *hostagent.Manager
	mcp    *httptest.Server
	host   *store.AgentHost
	// chat 是缺省对话（newEnv 新建）；用例按需再开别的。
	chat *store.AgentHostChat
	// files 记录假主机上经 write_file 写下的内容（按命令里的路径归档）。
	filesMu sync.Mutex
	files   map[string]string
}

func newEnv(t *testing.T, opt hostagent.Options) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, files: map[string]string{}}
	e.fake = agenthosttest.New(t, agenthosttest.Options{Password: hostPassword, Username: "admin", SudoNoPasswd: true,
		Command: func(command, stdin string) (string, int, bool) {
			switch {
			case command == "uname -a":
				return "Linux fakehost 6.8.0 aarch64\n", 0, true
			case command == "false":
				return "", 1, true
			case command == "printf secret":
				return "top-secret-output", 0, true
			case strings.Contains(command, "sh -c "):
				e.filesMu.Lock()
				e.files[command] = stdin
				e.filesMu.Unlock()
				return "", 0, true
			}
			return "", 0, false
		}})
	e.hosts = agenthost.New(agenthost.Options{Store: st, Dial: e.fake.Dial})
	if _, err := e.hosts.GenerateCertificate(t.Context()); err != nil {
		t.Fatalf("GenerateCertificate: %v", err)
	}
	addr, port := e.fake.Addr()
	host, err := e.hosts.Enroll(t.Context(), agenthost.EnrollRequest{Name: "板子", Kind: store.AgentHostKindManaged, Address: addr, Port: port, Username: "admin", Password: hostPassword, ConfigureSudo: true})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	e.host = host
	e.engine = &fakeEngine{ready: true}
	opt.Store, opt.Hosts, opt.Engine = st, e.hosts, e.engine
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	e.m = hostagent.New(opt)
	e.mcp = httptest.NewServer(e.m.MCPHandler())
	t.Cleanup(e.mcp.Close)
	// ToolURL 在 New 之后才知道：测试经 hostagent 的导出钩子改它。
	hostagent.SetToolURLForTest(e.m, e.mcp.URL+"/agent-mcp")
	t.Cleanup(func() { e.m.Shutdown(context.Background()) })
	e.chat = e.newChat(hostagent.NewChat{KeyID: 1})
	return e
}

// newChat 在缺省主机上新建一个对话。
func (e *env) newChat(in hostagent.NewChat) *store.AgentHostChat {
	e.t.Helper()
	chat, err := e.m.CreateChat(context.Background(), e.host.ID, in)
	if err != nil {
		e.t.Fatalf("CreateChat: %v", err)
	}
	return chat
}

// submit 向缺省对话提交一条指令。
func (e *env) submit(ctx context.Context, in hostagent.Input) (*store.AgentHostRun, error) {
	return e.m.Submit(ctx, e.host.ID, e.chat.ID, in)
}

// waitUntil 陪等到条件满足（沿用产品的 Wait 陪等，不轮询数据库）。
func (e *env) waitUntil(cond func(st hostagent.State) bool) hostagent.State {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	rev := int64(-1)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, _ := e.m.Wait(ctx, e.host.ID, e.chat.ID, rev)
		cancel()
		if cond(st) {
			return st
		}
		rev = st.Revision
	}
	e.t.Fatalf("条件未满足；最后读数 = %+v", e.m.Snapshot(e.chat.ID))
	return hostagent.State{}
}

func (e *env) events(kinds ...string) []store.AgentHostEvent {
	e.t.Helper()
	evs, err := e.st.ListAgentHostEvents(context.Background(), store.AgentHostEventQuery{HostID: e.host.ID, Kinds: kinds})
	if err != nil {
		e.t.Fatalf("ListAgentHostEvents: %v", err)
	}
	return evs
}

func (e *env) run(id string) *store.AgentHostRun {
	e.t.Helper()
	r, err := e.st.GetAgentHostRun(context.Background(), id)
	if err != nil {
		e.t.Fatalf("GetAgentHostRun: %v", err)
	}
	return r
}

func TestSubmitValidation(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	ctx := context.Background()
	for _, in := range []hostagent.Input{
		{}, {Text: "   "},
		{Text: "x", Images: []string{"https://example.com/a.png"}},
		{Text: "x", Images: []string{"data:text/plain;base64,QUJD"}},
		{Text: strings.Repeat("字", hostagent.MaxTextRunes+1)},
	} {
		_, err := e.submit(ctx, in)
		var he *hostagent.Error
		if !errors.As(err, &he) || he.Code != hostagent.CodeInvalidInput {
			t.Fatalf("输入 %+v: err = %v", in, err)
		}
	}
	var he *hostagent.Error
	if _, err := e.m.Submit(ctx, 999, e.chat.ID, hostagent.Input{Text: "x"}); !errors.As(err, &he) || he.Code != hostagent.CodeChatNotFound {
		t.Fatalf("别的主机的对话 err = %v", err)
	}
	if _, err := e.m.Submit(ctx, e.host.ID, "nope", hostagent.Input{Text: "x"}); !errors.As(err, &he) || he.Code != hostagent.CodeChatNotFound {
		t.Fatalf("不存在的对话 err = %v", err)
	}
	e.engine.mu.Lock()
	e.engine.ready, e.engine.reason = false, "组件没装"
	e.engine.mu.Unlock()
	_, err := e.submit(ctx, hostagent.Input{Text: "x"})
	if !errors.As(err, &he) || he.Code != hostagent.CodeEngineNotReady || he.Msg != "组件没装" {
		t.Fatalf("引擎未就绪 err = %v", err)
	}
	e.engine.mu.Lock()
	e.engine.ready = true
	e.engine.keyErr = &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: "密钥已停用"}
	e.engine.mu.Unlock()
	// 对话钉死的密钥此刻不可用：拒绝提交，也起不了新对话。
	_, err = e.submit(ctx, hostagent.Input{Text: "x"})
	if !errors.As(err, &he) || he.Code != hostagent.CodeKeyInvalid || he.Msg != "密钥已停用" {
		t.Fatalf("密钥不可用 err = %v", err)
	}
	if _, err := e.m.CreateChat(ctx, e.host.ID, hostagent.NewChat{KeyID: 1}); !errors.As(err, &he) || he.Code != hostagent.CodeKeyInvalid {
		t.Fatalf("密钥不可用时新建对话 err = %v", err)
	}
	e.engine.mu.Lock()
	e.engine.keyErr = nil
	e.engine.mu.Unlock()
	if _, err := e.m.CreateChat(ctx, e.host.ID, hostagent.NewChat{Title: strings.Repeat("长", hostagent.MaxTitleRunes+1), KeyID: 1}); !errors.As(err, &he) || he.Code != hostagent.CodeInvalidInput {
		t.Fatalf("超长标题 err = %v", err)
	}
	if _, err := e.m.CreateChat(ctx, 999, hostagent.NewChat{KeyID: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的主机新建对话 err = %v", err)
	}
	// 主机不可用（最近一次连接失败）也拒。
	if _, err := e.st.SetAgentHostState(ctx, store.AgentHostState{ID: e.host.ID, Status: store.AgentHostStatusError, LastError: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.submit(ctx, hostagent.Input{Text: "x"})
	if !errors.As(err, &he) || he.Code != hostagent.CodeHostNotReady {
		t.Fatalf("主机未就绪 err = %v", err)
	}
	if evs := e.events(); len(evs) != 0 {
		t.Fatalf("被拒的提交不该留事件: %+v", evs)
	}
}

func TestQueueRunsInOrderAndToolsLeaveAuditTrail(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	ctx := context.Background()
	var order []string
	var orderMu sync.Mutex
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		orderMu.Lock()
		order = append(order, in.Text)
		orderMu.Unlock()
		switch in.Text {
		case "看看内核":
			sink.Activity("思考中…")
			res := mcpCall(t, tools, "tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": "uname -a"}})
			text, isErr := toolText(res)
			if isErr || !strings.Contains(text, "exit_code: 0") || !strings.Contains(text, "Linux fakehost") {
				t.Errorf("exec 结果 = %q %v", text, isErr)
			}
			res = mcpCall(t, tools, "tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": "false"}})
			if text, isErr := toolText(res); isErr || !strings.Contains(text, "exit_code: 1") {
				t.Errorf("false 结果 = %q %v", text, isErr)
			}
			sink.Delta("内核是 ")
			sink.Delta("6.8")
			sink.Usage(120, 30)
			sink.Message("内核是 6.8")
			return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
		case "写配置":
			res := mcpCall(t, tools, "tools/call", map[string]any{"name": "write_file", "arguments": map[string]any{"path": "/etc/app.conf", "content": "a=1\n", "mode": "0644", "sudo": true}})
			if text, isErr := toolText(res); isErr || !strings.Contains(text, "wrote /etc/app.conf") {
				t.Errorf("write_file 结果 = %q %v", text, isErr)
			}
			res = mcpCall(t, tools, "tools/call", map[string]any{"name": "update_profile", "arguments": map[string]any{"content": "# 板子\n- 内核 6.8"}})
			if text, isErr := toolText(res); isErr || text != "host profile updated" {
				t.Errorf("update_profile 结果 = %q %v", text, isErr)
			}
			res = mcpCall(t, tools, "tools/call", map[string]any{"name": "read_profile"})
			if text, _ := toolText(res); !strings.Contains(text, "内核 6.8") {
				t.Errorf("read_profile 结果 = %q", text)
			}
			sink.Reasoning("先写文件再更新档案")
			sink.Message("配置已写好")
			return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
		default:
			return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "不认识的指令"}
		}
	}
	// 三条连着提交：第一条在跑时后两条都在队列里。
	r1, err := e.submit(ctx, hostagent.Input{Text: "看看内核", Images: []string{"data:image/png;base64,iVBORw0KGgo="}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	r2, err := e.submit(ctx, hostagent.Input{Text: "写配置"})
	if err != nil {
		t.Fatalf("Submit(2): %v", err)
	}
	r3, err := e.submit(ctx, hostagent.Input{Text: "乱来"})
	if err != nil {
		t.Fatalf("Submit(3): %v", err)
	}
	if r1.Status != store.AgentRunQueued || r1.ImageCount != 1 || r1.Engine != "fake" || r1.Model != "fake-1" || r1.ChatID != e.chat.ID {
		t.Fatalf("受理的指令 = %+v", r1)
	}
	final := e.waitUntil(func(st hostagent.State) bool {
		return st.Status == hostagent.StatusIdle && st.Current == nil && len(st.Queue) == 0 && e.run(r3.ID).Status == store.AgentRunFailed
	})
	if final.EngineStartedAt == nil {
		t.Fatalf("会话应仍开着（空闲超时未到）: %+v", final)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if strings.Join(gotOrder, ",") != "看看内核,写配置,乱来" {
		t.Fatalf("执行顺序 = %v", gotOrder)
	}
	if got := e.run(r1.ID); got.Status != store.AgentRunSucceeded || got.InputTokens != 120 || got.OutputTokens != 30 || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("r1 = %+v", got)
	}
	if got := e.run(r2.ID); got.Status != store.AgentRunSucceeded {
		t.Fatalf("r2 = %+v", got)
	}
	if got := e.run(r3.ID); got.Status != store.AgentRunFailed || got.Error != "不认识的指令" {
		t.Fatalf("r3 = %+v", got)
	}
	if len(e.engine.sessions) != 1 || e.engine.sessions[0].runs != 3 {
		t.Fatalf("三条指令应共用一段会话: %d 段", len(e.engine.sessions))
	}
	if ins := e.engine.sessions[0].instructions; !strings.Contains(ins, "admin@") || !strings.Contains(ins, "还没有档案") || !strings.Contains(ins, "sudo -n") {
		t.Fatalf("开发者指令缺内容:\n%s", ins)
	}

	// 时间线：user ×3、command ×2（带退出码与输出）、assistant、file、profile、reasoning、
	// assistant、run failed；session started 落在第一条 user 之后、第一条 command 之前
	//（工作协程与后两次提交并发，确切位置不定）。
	all := e.events()
	evs := make([]store.AgentHostEvent, 0, len(all))
	sessionAt := -1
	for i, ev := range all {
		if ev.Kind == store.AgentEventSession {
			sessionAt = i
			continue
		}
		evs = append(evs, ev)
	}
	kinds := make([]string, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	want := "user,user,user,command,command,assistant,file,profile,reasoning,assistant,run"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("事件序列 = %v\n期望 %s", kinds, want)
	}
	if sessionAt < 1 || all[sessionAt].Title != "started" || sessionAt > 3 {
		t.Fatalf("session 事件位置 = %d / %+v", sessionAt, all)
	}
	if evs[0].Body != "看看内核" || evs[0].Meta != `{"images":1}` || evs[0].RunID != r1.ID || evs[0].ChatID != e.chat.ID {
		t.Fatalf("user 事件 = %+v", evs[0])
	}
	cmd := evs[3]
	if cmd.Title != "uname -a" || cmd.ExitCode == nil || *cmd.ExitCode != 0 || !strings.Contains(cmd.Body, "Linux fakehost") || cmd.RunID != r1.ID {
		t.Fatalf("command 事件 = %+v", cmd)
	}
	if evs[4].ExitCode == nil || *evs[4].ExitCode != 1 {
		t.Fatalf("false 的事件 = %+v", evs[4])
	}
	if evs[5].Body != "内核是 6.8" {
		t.Fatalf("assistant 事件 = %+v", evs[5])
	}
	if f := evs[6]; f.Title != "/etc/app.conf" || f.Body != "a=1\n" || !strings.Contains(f.Meta, `"sudo":true`) || f.ExitCode == nil || *f.ExitCode != 0 {
		t.Fatalf("file 事件 = %+v", f)
	}
	if p := evs[7]; p.Title != "agent" || !strings.Contains(p.Body, "内核 6.8") {
		t.Fatalf("profile 事件 = %+v", p)
	}
	if evs[10].Title != store.AgentRunFailed || evs[10].Body != "不认识的指令" {
		t.Fatalf("run 事件 = %+v", evs[10])
	}
	// 写文件真的经 stdin、经 sudo 到了主机上。
	e.filesMu.Lock()
	defer e.filesMu.Unlock()
	found := false
	for command, stdin := range e.files {
		if strings.HasPrefix(command, "sudo -n sh -c ") && strings.Contains(command, "/etc/app.conf") && strings.Contains(command, "chmod 0644") && stdin == "a=1\n" {
			found = true
		}
	}
	if !found {
		t.Fatalf("假主机上没看到写文件命令: %v", e.files)
	}
	profile, err := e.st.GetAgentHostProfile(ctx, e.host.ID)
	if err != nil || profile.UpdatedBy != "agent" || !strings.HasSuffix(profile.Content, "\n") {
		t.Fatalf("档案 = %+v, %v", profile, err)
	}
}

func TestMCPEndpointGuards(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	started := make(chan hostagent.ToolEndpoint, 1)
	release := make(chan struct{})
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		started <- tools
		<-release
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	if _, err := e.submit(context.Background(), hostagent.Input{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	tools := <-started
	// 握手与工具清单。
	init := mcpCall(t, tools, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %+v", init)
	}
	list := mcpCall(t, tools, "tools/list", nil)
	names := []string{}
	for _, tl := range list["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "exec,write_file,read_profile,update_profile" {
		t.Fatalf("工具清单 = %v", names)
	}
	// 未知工具与坏参数是工具级错误，不是协议错误。
	if text, isErr := toolText(mcpCall(t, tools, "tools/call", map[string]any{"name": "rm_rf"})); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("未知工具 = %q %v", text, isErr)
	}
	if text, isErr := toolText(mcpCall(t, tools, "tools/call", map[string]any{"name": "write_file", "arguments": map[string]any{"path": "/x", "content": "", "mode": "abc"}})); !isErr || !strings.Contains(text, "mode") {
		t.Fatalf("坏 mode = %q %v", text, isErr)
	}
	// 通知答 202；错令牌 401；GET 405。
	post := func(auth string, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, tools.URL, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post("Bearer "+tools.Token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("通知状态 = %d", resp.StatusCode)
	}
	if resp := post("Bearer wrong", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错令牌状态 = %d", resp.StatusCode)
	}
	if resp := post("", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌状态 = %d", resp.StatusCode)
	}
	if resp, _ := http.Get(tools.URL); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态 = %d", resp.StatusCode)
	}
	// 非回环来源一律 403（直接打处理器，伪造远端地址）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent-mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.RemoteAddr = "192.168.1.9:41000"
	req.Header.Set("Authorization", "Bearer "+tools.Token)
	e.m.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非回环状态 = %d", rec.Code)
	}
	close(release)
	e.waitUntil(func(st hostagent.State) bool { return st.Current == nil })
	// 会话结束后令牌作废。
	if err := e.m.Stop(context.Background(), e.host.ID, e.chat.ID); err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return st.EngineStartedAt == nil && st.Status == hostagent.StatusIdle })
	if resp := post("Bearer "+tools.Token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("会话结束后令牌仍可用: %d", resp.StatusCode)
	}
}

func TestCancelAndStop(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	ctx := context.Background()
	running := make(chan struct{}, 4)
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, interrupt <-chan struct{}) hostagent.Outcome {
		running <- struct{}{}
		select {
		case <-interrupt:
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}
		case <-ctx.Done():
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}
		case <-time.After(20 * time.Second):
			return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
		}
	}
	r1, _ := e.submit(ctx, hostagent.Input{Text: "一"})
	r2, _ := e.submit(ctx, hostagent.Input{Text: "二"})
	r3, _ := e.submit(ctx, hostagent.Input{Text: "三"})
	<-running
	// 取消排队中的第二条：直接终态，不影响第一条。
	if err := e.m.Cancel(ctx, e.host.ID, e.chat.ID, r2.ID); err != nil {
		t.Fatalf("Cancel(queued): %v", err)
	}
	if got := e.run(r2.ID); got.Status != store.AgentRunCancelled || got.Error != "管理员取消" {
		t.Fatalf("r2 = %+v", got)
	}
	st := e.m.Snapshot(e.chat.ID)
	if st.Current == nil || st.Current.ID != r1.ID || len(st.Queue) != 1 || st.Queue[0].ID != r3.ID || st.Status != hostagent.StatusRunning {
		t.Fatalf("读数 = %+v", st)
	}
	// 取消正在执行的第一条：引擎被中止，指令记为取消，第三条接着跑。
	if err := e.m.Cancel(ctx, e.host.ID, e.chat.ID, r1.ID); err != nil {
		t.Fatalf("Cancel(running): %v", err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(r1.ID).Status == store.AgentRunCancelled })
	<-running
	e.waitUntil(func(st hostagent.State) bool { return st.Current != nil && st.Current.ID == r3.ID })
	// 已结束的与不存在的指令。
	var he *hostagent.Error
	if err := e.m.Cancel(ctx, e.host.ID, e.chat.ID, r1.ID); !errors.As(err, &he) || he.Code != hostagent.CodeRunFinished {
		t.Fatalf("取消已结束 err = %v", err)
	}
	if err := e.m.Cancel(ctx, e.host.ID, e.chat.ID, "nope"); !errors.As(err, &he) || he.Code != hostagent.CodeRunNotFound {
		t.Fatalf("取消不存在 err = %v", err)
	}
	// 结束会话：当前的中止、引擎关闭、令牌作废、时间线留 session stopped。
	r4, _ := e.submit(ctx, hostagent.Input{Text: "四"})
	if err := e.m.Stop(ctx, e.host.ID, e.chat.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	e.waitUntil(func(st hostagent.State) bool { return st.Status == hostagent.StatusIdle && st.EngineStartedAt == nil })
	if got := e.run(r3.ID); got.Status != store.AgentRunCancelled {
		t.Fatalf("r3 = %+v", got)
	}
	if got := e.run(r4.ID); got.Status != store.AgentRunCancelled || got.Error != "会话已结束" {
		t.Fatalf("r4 = %+v", got)
	}
	if !e.engine.sessions[0].closed {
		t.Fatal("引擎会话没关")
	}
	sessions := e.events(store.AgentEventSession)
	if len(sessions) != 2 || sessions[0].Title != "started" || sessions[1].Title != "stopped" {
		t.Fatalf("session 事件 = %+v", sessions)
	}
	// 再提交会起新的一段。
	r5, err := e.submit(ctx, hostagent.Input{Text: "五"})
	if err != nil {
		t.Fatal(err)
	}
	<-running
	e.waitUntil(func(st hostagent.State) bool { return st.Current != nil && st.Current.ID == r5.ID })
	if len(e.engine.sessions) != 2 {
		t.Fatalf("会话段数 = %d", len(e.engine.sessions))
	}
}

func TestIdleTimeoutClosesEngine(t *testing.T) {
	e := newEnv(t, hostagent.Options{IdleTimeout: 200 * time.Millisecond})
	e.engine.script = func(context.Context, hostagent.Input, hostagent.Sink, hostagent.ToolEndpoint, <-chan struct{}) hostagent.Outcome {
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	r, err := e.submit(context.Background(), hostagent.Input{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool {
		return e.run(r.ID).Status == store.AgentRunSucceeded && st.Status == hostagent.StatusIdle && st.EngineStartedAt == nil
	})
	if !e.engine.sessions[0].closed {
		t.Fatal("空闲超时后引擎会话没关")
	}
	sessions := e.events(store.AgentEventSession)
	if len(sessions) != 2 || !strings.Contains(sessions[1].Body, "空闲") {
		t.Fatalf("session 事件 = %+v", sessions)
	}
}

func TestEngineStartFailureFailsRun(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	e.engine.startErr = errors.New("启动 Codex App Server 失败：假的")
	r, err := e.submit(context.Background(), hostagent.Input{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(r.ID).Status == store.AgentRunFailed })
	if got := e.run(r.ID); !strings.Contains(got.Error, "假的") {
		t.Fatalf("run = %+v", got)
	}
}

func TestRecoverAndHostRemoved(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	ctx := context.Background()
	// 模拟上次进程留下的未完成指令。
	r, err := e.st.CreateAgentHostRun(ctx, store.NewAgentHostRun{HostID: e.host.ID, Text: "遗留"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SetAgentHostRunStatus(ctx, r.ID, store.AgentRunRunning, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	e.m.Recover(ctx)
	if got := e.run(r.ID); got.Status != store.AgentRunFailed || !strings.Contains(got.Error, "重启") {
		t.Fatalf("恢复后 = %+v", got)
	}
	// 解除纳管：会话结束，行随主机级联删除。
	block := make(chan struct{})
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, interrupt <-chan struct{}) hostagent.Outcome {
		select {
		case <-interrupt:
		case <-ctx.Done():
		case <-block:
		}
		return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}
	}
	if _, err := e.submit(ctx, hostagent.Input{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return st.Status == hostagent.StatusRunning })
	e.m.HostRemoved(ctx, e.host.ID)
	if !e.engine.sessions[0].closed {
		t.Fatal("解除纳管后引擎会话没关")
	}
	if _, err := e.hosts.Remove(ctx, e.host.ID, false); err != nil {
		t.Fatal(err)
	}
	if evs, err := e.st.ListAgentHostEvents(ctx, store.AgentHostEventQuery{HostID: e.host.ID}); err != nil || len(evs) != 0 {
		t.Fatalf("删主机后事件 = %+v, %v", evs, err)
	}
}

func TestAdminProfileUpdateLeavesEvent(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	p, err := e.m.SetProfile(context.Background(), e.host.ID, "# 手写\r\n- a")
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "# 手写\n- a\n" || p.UpdatedBy != "admin" {
		t.Fatalf("档案 = %+v", p)
	}
	evs := e.events(store.AgentEventProfile)
	if len(evs) != 1 || evs[0].Title != "admin" || evs[0].Body != "# 手写\n- a\n" || evs[0].ChatID != "" {
		t.Fatalf("profile 事件 = %+v", evs)
	}
	if _, err := e.m.SetProfile(context.Background(), 999, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的主机 err = %v", err)
	}
}

func TestChatsLifecycle(t *testing.T) {
	e := newEnv(t, hostagent.Options{})
	ctx := context.Background()
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		if strings.HasPrefix(in.Text, "跑一下") {
			mcpCall(t, tools, "tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": "uname -a"}})
			mcpCall(t, tools, "tools/call", map[string]any{"name": "update_profile", "arguments": map[string]any{"content": "# 板子\n- 内核 6.8"}})
		}
		sink.Message("回复：" + in.Text)
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	// 第一个对话：没标题，第一条指令的首行成为标题；开发者指令说「还没有档案」。
	r1, err := e.submit(ctx, hostagent.Input{Text: "跑一下\n第二行不进标题"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(r1.ID).Status == store.AgentRunSucceeded })
	chats, err := e.m.ListChats(ctx, e.host.ID)
	if err != nil || len(chats) != 1 || chats[0].ID != e.chat.ID || chats[0].Title != "跑一下" || chats[0].Model != "fake-1" || chats[0].KeyDisplay != "sk_fake…0000" {
		t.Fatalf("对话列表 = %+v, %v", chats, err)
	}
	if ins := e.engine.sessions[0].instructions; !strings.Contains(ins, "还没有档案") || strings.Contains(ins, "先前的记录") {
		t.Fatalf("第一段会话的开发者指令:\n%s", ins)
	}
	if got := e.engine.sessions[0].chat; got.ID != e.chat.ID || got.Model != "fake-1" {
		t.Fatalf("起会话带的对话 = %+v", got)
	}

	// 第二个对话：新建时注入的是此刻的主机档案与最近操作；标题按给定的；模型 / 档位钉死。
	second, err := e.m.CreateChat(ctx, e.host.ID, hostagent.NewChat{Title: "  装 Docker ", KeyID: 2, Model: "fake-2", Effort: "high"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if second.Title != "装 Docker" || second.KeyID != 2 || second.Model != "fake-2" || second.Effort != "high" || second.Engine != "fake" {
		t.Fatalf("第二个对话 = %+v", second)
	}
	e.chat = second
	r2, err := e.submit(ctx, hostagent.Input{Text: "看看"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(r2.ID).Status == store.AgentRunSucceeded })
	if r2.Model != "fake-2" || r2.ChatID != second.ID {
		t.Fatalf("第二个对话的指令 = %+v", r2)
	}
	if ins := e.engine.sessions[1].instructions; !strings.Contains(ins, "内核 6.8") || !strings.Contains(ins, "exec: `uname -a`") || strings.Contains(ins, "先前的记录") {
		t.Fatalf("第二个对话的开发者指令缺注入内容:\n%s", ins)
	}
	chats, _ = e.m.ListChats(ctx, e.host.ID)
	if len(chats) != 2 || chats[0].ID != second.ID || chats[0].Title != "装 Docker" || chats[0].Status != hostagent.StatusIdle {
		t.Fatalf("对话列表 = %+v", chats)
	}
	// 各对话的时间线互不混：第一个对话的事件不出现在第二个对话里。
	evs, _ := e.st.ListAgentHostEvents(ctx, store.AgentHostEventQuery{HostID: e.host.ID, ChatID: second.ID})
	for _, ev := range evs {
		if ev.ChatID != second.ID || ev.Body == "回复：跑一下" {
			t.Fatalf("第二个对话里混进了别的事件: %+v", ev)
		}
	}

	// 结束会话后再提交：引擎重启，开发者指令附上本对话先前的记录（不含正在执行的这条）。
	if err := e.m.Stop(ctx, e.host.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return st.EngineStartedAt == nil && st.Status == hostagent.StatusIdle })
	r3, err := e.submit(ctx, hostagent.Input{Text: "再看看"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitUntil(func(st hostagent.State) bool { return e.run(r3.ID).Status == store.AgentRunSucceeded })
	if len(e.engine.sessions) != 3 {
		t.Fatalf("会话段数 = %d", len(e.engine.sessions))
	}
	ins := e.engine.sessions[2].instructions
	if !strings.Contains(ins, "先前的记录") || !strings.Contains(ins, "管理员：看看") || !strings.Contains(ins, "你：回复：看看") || strings.Contains(ins, "再看看") {
		t.Fatalf("重启后的开发者指令:\n%s", ins)
	}
	if !strings.HasPrefix(ins, second.Instructions) {
		t.Fatal("重启后的开发者指令应以新建时注入的那份开头")
	}

	// 删对话：指令与回复没了，对主机的操作记录留在主机名下；另一个对话不受影响。
	var he *hostagent.Error
	if err := e.m.DeleteChat(ctx, e.host.ID, "nope"); !errors.As(err, &he) || he.Code != hostagent.CodeChatNotFound {
		t.Fatalf("删不存在的对话 err = %v", err)
	}
	if err := e.m.DeleteChat(ctx, e.host.ID, second.ID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if !e.engine.sessions[2].closed {
		t.Fatal("删对话后引擎会话没关")
	}
	if _, err := e.st.GetAgentHostRun(ctx, r2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删对话后指令 err = %v", err)
	}
	if _, err := e.m.Submit(ctx, e.host.ID, second.ID, hostagent.Input{Text: "x"}); !errors.As(err, &he) || he.Code != hostagent.CodeChatNotFound {
		t.Fatalf("向已删对话提交 err = %v", err)
	}
	chats, _ = e.m.ListChats(ctx, e.host.ID)
	if len(chats) != 1 || chats[0].ID != r1.ChatID {
		t.Fatalf("删后列表 = %+v", chats)
	}
	all := e.events()
	for _, ev := range all {
		if ev.Kind == store.AgentEventAssistant && ev.ChatID == "" {
			t.Fatalf("删对话后回复仍在: %+v", ev)
		}
		if ev.ChatID == second.ID {
			t.Fatalf("删对话后仍有归属它的事件: %+v", ev)
		}
	}
	ops := e.events(store.AgentEventUser, store.AgentEventSession)
	detached := 0
	for _, ev := range ops {
		if ev.ChatID == "" {
			detached++
		}
	}
	if detached == 0 {
		t.Fatalf("删对话后操作记录应留在主机名下: %+v", ops)
	}
	if got := e.run(r1.ID); got.Status != store.AgentRunSucceeded {
		t.Fatalf("第一个对话的指令受影响: %+v", got)
	}
}
