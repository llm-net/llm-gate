package admin_test

// Agent远控端点验收：503 未接入、路由档位、一条从新建对话、提交到取消 / 结束会话 / 删对话 /
// 档案 / 操作日志的完整路径（假引擎经真 MCP 端点向进程内假主机执行命令）、审计不含指令正文，
// 以及新建对话可选项与按密钥解析引擎凭据的校验。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// adminFakeEngine：每条指令经 MCP 端点执行一次 exec，再回一句；文本为 block 时等放行。
// validate 缺省只要密钥 id 为正；用例可换成管理面的真解析。
type adminFakeEngine struct {
	mu       sync.Mutex
	ready    bool
	reason   string
	validate func(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error)
	release  chan struct{}
	closed   int
}

func (e *adminFakeEngine) ID() string { return "fake" }
func (e *adminFakeEngine) Status(context.Context) hostagent.EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return hostagent.EngineStatus{ID: "fake", Label: "Fake", Ready: e.ready, Reason: e.reason}
}
func (e *adminFakeEngine) Validate(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	if e.validate != nil {
		return e.validate(ctx, cfg)
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
func (e *adminFakeEngine) Start(_ context.Context, req hostagent.StartRequest) (hostagent.EngineSession, error) {
	return &adminFakeSession{e: e, tools: req.Tools, done: make(chan struct{})}, nil
}

type adminFakeSession struct {
	e         *adminFakeEngine
	tools     hostagent.ToolEndpoint
	mu        sync.Mutex
	interrupt chan struct{}
	done      chan struct{}
}

func (s *adminFakeSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	s.mu.Lock()
	s.interrupt = make(chan struct{})
	ch := s.interrupt
	s.mu.Unlock()
	if in.Text == "block" {
		select {
		case <-ch:
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
		case <-ctx.Done():
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
		case <-s.e.release:
		}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "exec", "arguments": map[string]any{"command": "uname -a"}}})
	req, _ := http.NewRequest(http.MethodPost, s.tools.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.tools.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: err.Error()}, nil
	}
	resp.Body.Close()
	sink.Message("done: " + in.Text + fmt.Sprintf(" (%d 张图)", len(in.Images)))
	return hostagent.Outcome{Status: hostagent.OutcomeCompleted}, nil
}
func (s *adminFakeSession) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.interrupt:
	default:
		close(s.interrupt)
	}
	return nil
}
func (s *adminFakeSession) Done() <-chan struct{} { return s.done }
func (s *adminFakeSession) Close() error {
	s.e.mu.Lock()
	s.e.closed++
	s.e.mu.Unlock()
	close(s.done)
	return nil
}

type hostAgentEnv struct {
	*env
	fakeHost *agenthosttest.Server
	engine   *adminFakeEngine
	agent    *hostagent.Manager
	hostID   int64
	chatID   string
}

// withHostAgent 接上纳管管理器、假主机、Agent远控（假引擎 + 真 MCP 端点），纳管一台主机并
// 开一个对话；ready 为假时在开好对话之后才把引擎置为未就绪。
func withHostAgent(t *testing.T, e *env, ready bool) *hostAgentEnv {
	t.Helper()
	fake := agenthosttest.New(t, agenthosttest.Options{Username: "admin", Password: hostPassword, SudoNoPasswd: true,
		Command: func(command, stdin string) (string, int, bool) {
			if command == "uname -a" {
				return "Linux fakehost 6.8.0\n", 0, true
			}
			return "", 0, false
		}})
	hosts := agenthost.New(agenthost.Options{Store: e.st, Dial: fake.Dial})
	e.srv.SetAgentHosts(hosts)
	engine := &adminFakeEngine{ready: true, release: make(chan struct{})}
	var mgr *hostagent.Manager
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mgr.MCPHandler().ServeHTTP(w, r) }))
	t.Cleanup(mcp.Close)
	mgr = hostagent.New(hostagent.Options{Store: e.st, Hosts: hosts, Engine: engine, ToolURL: mcp.URL + "/agent-mcp"})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	e.srv.SetHostAgent(mgr)

	root := e.rootSession()
	resp := e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	addr, port := fake.Addr()
	resp = e.do("POST", "/admin/v1/agent-hosts", root,
		`{"name":"板子","kind":"managed","address":"`+addr+`","port":`+strconv.Itoa(port)+`,"username":"admin","password":"`+hostPassword+`","configure_sudo":true}`)
	wantStatus(t, resp, http.StatusOK)
	var one struct {
		Host agentHostBody `json:"host"`
	}
	decodeInto(t, resp, &one)
	h := &hostAgentEnv{env: e, fakeHost: fake, engine: engine, agent: mgr, hostID: one.Host.ID}
	resp = e.do("POST", h.agentPath("/chats"), root, `{"key_id":1}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Chat agentChatBody `json:"chat"`
	}
	decodeInto(t, resp, &created)
	h.chatID = created.Chat.ID
	engine.mu.Lock()
	engine.ready, engine.reason = ready, "引擎没准备好"
	engine.mu.Unlock()
	return h
}

type agentStateBody struct {
	ChatID          string           `json:"chat_id"`
	Revision        int64            `json:"revision"`
	Status          string           `json:"status"`
	EngineStartedAt string           `json:"engine_started_at"`
	Current         *agentRunBody    `json:"current"`
	Queue           []agentRunBody   `json:"queue"`
	Live            *json.RawMessage `json:"live"`
}

type agentRunBody struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	Status     string `json:"status"`
	Text       string `json:"text"`
	ImageCount int    `json:"image_count"`
	Error      string `json:"error"`
}

type agentEventBody struct {
	ID       int64  `json:"id"`
	ChatID   string `json:"chat_id"`
	RunID    string `json:"run_id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	ExitCode *int   `json:"exit_code"`
}

type agentChatBody struct {
	ID           string          `json:"id"`
	HostID       int64           `json:"host_id"`
	Title        string          `json:"title"`
	Engine       string          `json:"engine"`
	KeyID        int64           `json:"key_id"`
	KeyDisplay   string          `json:"key_display"`
	Model        string          `json:"model"`
	Effort       string          `json:"effort"`
	Status       string          `json:"status"`
	Busy         bool            `json:"busy"`
	Instructions json.RawMessage `json:"instructions"`
}

type agentPageBody struct {
	Host   agentHostBody `json:"host"`
	Engine struct {
		ID     string `json:"id"`
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	} `json:"engine"`
	Chats   []agentChatBody `json:"chats"`
	Profile struct {
		Content   string `json:"content"`
		UpdatedBy string `json:"updated_by"`
	} `json:"profile"`
	Ops []agentEventBody `json:"ops"`
}

func (h *hostAgentEnv) agentPath(suffix string) string {
	return "/admin/v1/agent-hosts/" + itoa(h.hostID) + "/agent" + suffix
}

func (h *hostAgentEnv) chatPath(suffix string) string {
	return h.agentPath("/chats/" + h.chatID + suffix)
}

// waitRun 用陪等端点等一条指令到某个状态，顺带带回全部新事件。
func (h *hostAgentEnv) waitRun(t *testing.T, cookie, runID string, done func(status string) bool) {
	t.Helper()
	revision, after := int64(-1), int64(0)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := h.do("GET", h.chatPath(fmt.Sprintf("/wait?revision=%d&after=%d", revision, after)), cookie, "")
		wantStatus(t, resp, http.StatusOK)
		var body struct {
			State  agentStateBody   `json:"state"`
			Events []agentEventBody `json:"events"`
		}
		decodeInto(t, resp, &body)
		revision = body.State.Revision
		if len(body.Events) > 0 {
			after = body.Events[len(body.Events)-1].ID
		}
		run, err := h.st.GetAgentHostRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if done(run.Status) {
			return
		}
	}
	t.Fatalf("指令 %s 未到期望状态", runID)
}

func TestHostAgentUnavailableWithoutManager(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	resp := e.do("GET", "/admin/v1/agent-hosts/1/agent", root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if code := errCode(t, resp); code != "agent_unavailable" {
		t.Fatalf("错误码 = %q", code)
	}
}

func TestHostAgentRouteTiers(t *testing.T) {
	e := newEnv(t)
	lister := e.h.(tunnelctx.RouteLister)
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for _, p := range []string{"GET /admin/v1/agent-hosts/{id}/agent", "GET /admin/v1/agent-hosts/{id}/agent/options",
		"GET /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}", "GET /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/wait",
		"GET /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/events", "GET /admin/v1/agent-hosts/{id}/agent/events",
		"GET /admin/v1/agent-hosts/{id}/agent/profile"} {
		if tiers[p] != tunnelctx.Admin {
			t.Errorf("%s 应是 Admin 档，实际 %v", p, tiers[p])
		}
	}
	for _, p := range []string{"POST /admin/v1/agent-hosts/{id}/agent/chats", "DELETE /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}",
		"POST /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/runs", "DELETE /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/runs/{run_id}",
		"POST /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/stop", "PUT /admin/v1/agent-hosts/{id}/agent/profile"} {
		if tiers[p] != tunnelctx.LANOnly {
			t.Errorf("%s 应只限 LAN，实际 %v", p, tiers[p])
		}
	}
	// 全局引擎设置端点已不存在。
	for _, p := range []string{"GET /admin/v1/system/components/codex-app-server/settings", "PUT /admin/v1/system/components/codex-app-server/settings"} {
		if _, ok := tiers[p]; ok {
			t.Errorf("%s 不该再有", p)
		}
	}
}

func TestHostAgentFlow(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	h := withHostAgent(t, e, true)

	// 进页读数：主机就绪、引擎就绪、一个对话（钉死的密钥 / 模型）、档案为空、没有操作日志。
	resp := e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	var page agentPageBody
	decodeInto(t, resp, &page)
	if page.Host.ID != h.hostID || !page.Engine.Ready || len(page.Chats) != 1 || page.Chats[0].ID != h.chatID || page.Chats[0].Model != "fake-1" ||
		page.Chats[0].KeyDisplay != "sk_fake…0000" || page.Chats[0].Status != "idle" || page.Profile.Content != "" || len(page.Ops) != 0 {
		t.Fatalf("进页读数 = %+v", page)
	}
	if len(page.Chats[0].Instructions) != 0 {
		t.Fatalf("开发者指令不该进响应: %s", page.Chats[0].Instructions)
	}
	// 对话读数：空时间线、空闲。
	resp = e.do("GET", h.chatPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	var chatPage struct {
		Chat   agentChatBody    `json:"chat"`
		State  agentStateBody   `json:"state"`
		Events []agentEventBody `json:"events"`
	}
	decodeInto(t, resp, &chatPage)
	if chatPage.Chat.ID != h.chatID || chatPage.State.Status != "idle" || chatPage.State.ChatID != h.chatID || len(chatPage.Events) != 0 {
		t.Fatalf("对话读数 = %+v", chatPage)
	}
	// 新建对话的校验：没选密钥 400、坏体 400、不存在的主机 404。
	resp = e.do("POST", h.agentPath("/chats"), root, `{"title":"x"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != hostagent.CodeKeyInvalid {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("POST", "/admin/v1/agent-hosts/999/agent/chats", root, `{"key_id":1}`)
	wantStatus(t, resp, http.StatusNotFound)

	// 提交：坏输入 400；正常 202，带图片；没标题的对话拿指令首行当标题。
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"   "}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != hostagent.CodeInvalidInput {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"看看内核","images":["data:image/png;base64,iVBORw0KGgo="]}`)
	wantStatus(t, resp, http.StatusAccepted)
	var accepted struct {
		Run   agentRunBody   `json:"run"`
		State agentStateBody `json:"state"`
	}
	decodeInto(t, resp, &accepted)
	if accepted.Run.Status != "queued" || accepted.Run.ImageCount != 1 || accepted.Run.Text != "看看内核" || accepted.Run.ChatID != h.chatID {
		t.Fatalf("受理 = %+v", accepted)
	}
	h.waitRun(t, root, accepted.Run.ID, func(status string) bool { return status == "succeeded" })

	// 对话时间线：user → session → command（走了假主机）→ assistant，都归这个对话。
	resp = e.do("GET", h.chatPath("/events"), root, "")
	wantStatus(t, resp, http.StatusOK)
	var evs struct {
		Events []agentEventBody `json:"events"`
	}
	decodeInto(t, resp, &evs)
	kinds := []string{}
	for _, ev := range evs.Events {
		kinds = append(kinds, ev.Kind)
		if ev.ChatID != h.chatID {
			t.Fatalf("事件归属 = %+v", ev)
		}
	}
	if strings.Join(kinds, ",") != "user,session,command,assistant" {
		t.Fatalf("事件 = %v", kinds)
	}
	if cmd := evs.Events[2]; cmd.Title != "uname -a" || cmd.ExitCode == nil || *cmd.ExitCode != 0 || !strings.Contains(cmd.Body, "fakehost") {
		t.Fatalf("command 事件 = %+v", cmd)
	}
	if evs.Events[3].Body != "done: 看看内核 (1 张图)" {
		t.Fatalf("assistant 事件 = %+v", evs.Events[3])
	}
	// 主机级操作日志视图不含 assistant，且跨对话。
	resp = e.do("GET", h.agentPath("/events?ops=1&limit=10"), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &evs)
	if len(evs.Events) != 3 {
		t.Fatalf("操作日志 = %+v", evs.Events)
	}
	for _, ev := range evs.Events {
		if ev.Kind == "assistant" {
			t.Fatalf("操作日志不该有回复: %+v", ev)
		}
	}
	if resp := e.do("GET", h.chatPath("/events?before=abc"), root, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏 before 状态 = %d", resp.StatusCode)
	}
	resp = e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &page)
	if page.Chats[0].Title != "看看内核" || len(page.Ops) != 3 {
		t.Fatalf("进页读数（自动标题 + 操作日志）= %+v", page)
	}

	// 排队与取消：一条卡住的指令在跑，后面一条排队；取消排队的立即终态，取消在跑的中止。
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"block"}`)
	wantStatus(t, resp, http.StatusAccepted)
	decodeInto(t, resp, &accepted)
	blocked := accepted.Run.ID
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"后面的"}`)
	wantStatus(t, resp, http.StatusAccepted)
	decodeInto(t, resp, &accepted)
	queued := accepted.Run.ID
	h.waitRun(t, root, blocked, func(status string) bool { return status == "running" })
	resp = e.do("DELETE", h.chatPath("/runs/"+queued), root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	var st struct {
		State agentStateBody `json:"state"`
	}
	decodeInto(t, resp, &st)
	if len(st.State.Queue) != 0 || st.State.Current == nil || st.State.Current.ID != blocked {
		t.Fatalf("取消后读数 = %+v", st.State)
	}
	resp = e.do("DELETE", h.chatPath("/runs/"+queued), root, `{}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != hostagent.CodeRunFinished {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("DELETE", h.chatPath("/runs/nope"), root, `{}`)
	wantStatus(t, resp, http.StatusNotFound)
	resp = e.do("DELETE", h.chatPath("/runs/"+blocked), root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	h.waitRun(t, root, blocked, func(status string) bool { return status == "cancelled" })

	// 第二个对话：带标题、指定模型与档位；两个对话并列，互不看见对方的时间线。
	resp = e.do("POST", h.agentPath("/chats"), root, `{"title":"装 Docker","key_id":1,"model":"fake-2","effort":"high"}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Chat agentChatBody `json:"chat"`
	}
	decodeInto(t, resp, &created)
	if created.Chat.Title != "装 Docker" || created.Chat.Model != "fake-2" || created.Chat.Effort != "high" || created.Chat.HostID != h.hostID {
		t.Fatalf("第二个对话 = %+v", created.Chat)
	}
	first := h.chatID
	h.chatID = created.Chat.ID
	resp = e.do("GET", h.chatPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &chatPage)
	if len(chatPage.Events) != 0 {
		t.Fatalf("新对话不该有事件: %+v", chatPage.Events)
	}
	resp = e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &page)
	if len(page.Chats) != 2 || page.Chats[0].ID != created.Chat.ID {
		t.Fatalf("对话列表 = %+v", page.Chats)
	}
	// 别的主机名下找不到这个对话。
	resp = e.do("GET", "/admin/v1/agent-hosts/999/agent/chats/"+created.Chat.ID, root, "")
	wantStatus(t, resp, http.StatusNotFound)
	// 删对话：陪等与提交都 404；第一个对话与主机级操作日志仍在。
	resp = e.do("DELETE", h.chatPath(""), root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("DELETE", h.chatPath(""), root, `{}`)
	wantStatus(t, resp, http.StatusNotFound)
	if code := errCode(t, resp); code != hostagent.CodeChatNotFound {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("GET", h.chatPath("/wait?revision=-1&after=0"), root, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"x"}`)
	wantStatus(t, resp, http.StatusNotFound)
	h.chatID = first
	resp = e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &page)
	if len(page.Chats) != 1 || page.Chats[0].ID != first || len(page.Ops) < 3 {
		t.Fatalf("删对话后读数 = %+v", page)
	}

	// 档案：管理员改一版，进页读数带回，时间线留 profile 事件（主机级，不归任何对话）。
	resp = e.do("PUT", h.agentPath("/profile"), root, `{"content":"# 板子\n- 手写"}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("GET", h.agentPath("/profile"), root, "")
	wantStatus(t, resp, http.StatusOK)
	var prof struct {
		Profile struct {
			Content   string `json:"content"`
			UpdatedBy string `json:"updated_by"`
		} `json:"profile"`
	}
	decodeInto(t, resp, &prof)
	if prof.Profile.Content != "# 板子\n- 手写\n" || prof.Profile.UpdatedBy != "admin" {
		t.Fatalf("档案 = %+v", prof.Profile)
	}
	resp = e.do("GET", h.agentPath("/events?ops=1"), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &evs)
	if last := evs.Events[len(evs.Events)-1]; last.Kind != "profile" || last.ChatID != "" {
		t.Fatalf("档案事件 = %+v", last)
	}

	// 结束会话：引擎关掉。
	resp = e.do("POST", h.chatPath("/stop"), root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.engine.mu.Lock()
		closed := h.engine.closed
		h.engine.mu.Unlock()
		if closed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.engine.closed != 1 {
		t.Fatalf("结束会话后引擎会话未关闭: %d", h.engine.closed)
	}

	// 审计：有对话 / 指令 / 取消 / 结束 / 档案事件，但不含指令正文。
	events := map[string]bool{}
	for _, row := range e.auditRows() {
		events[row.Event] = true
		if strings.Contains(row.Detail, "看看内核") || strings.Contains(row.Detail, "手写") {
			t.Fatalf("审计泄漏指令正文: %+v", row)
		}
	}
	for _, want := range []string{"agent_host.chat_create", "agent_host.chat_delete", "agent_host.instruction", "agent_host.instruction_cancel", "agent_host.session_stop", "agent_host.profile_update"} {
		if !events[want] {
			t.Fatalf("缺审计事件 %s: %v", want, events)
		}
	}
	// §15.1：指令正文、回复与命令输出不进日志。
	for _, s := range []string{"看看内核", "done: 看看内核", "fakehost"} {
		if strings.Contains(e.buf.String(), s) {
			t.Fatalf("日志里出现了 %q", s)
		}
	}

	// 解除纳管：对话与时间线随主机删除。
	resp = e.do("DELETE", "/admin/v1/agent-hosts/"+itoa(h.hostID), root, `{"remove_key":false}`)
	wantStatus(t, resp, http.StatusOK)
	if evs, err := e.st.ListAgentHostEvents(context.Background(), store.AgentHostEventQuery{HostID: h.hostID}); err != nil || len(evs) != 0 {
		t.Fatalf("删主机后事件 = %+v, %v", evs, err)
	}
	if chats, err := e.st.ListAgentHostChats(context.Background(), h.hostID); err != nil || len(chats) != 0 {
		t.Fatalf("删主机后对话 = %+v, %v", chats, err)
	}
	resp = e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusNotFound)
}

func TestHostAgentSubmitRejectedWhenEngineNotReady(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	h := withHostAgent(t, e, false)
	resp := e.do("POST", h.chatPath("/runs"), root, `{"text":"x"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != hostagent.CodeEngineNotReady {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("POST", h.agentPath("/chats"), root, `{"key_id":1}`)
	wantStatus(t, resp, http.StatusConflict)
	resp = e.do("GET", h.agentPath(""), root, "")
	wantStatus(t, resp, http.StatusOK)
	var page agentPageBody
	decodeInto(t, resp, &page)
	if page.Engine.Ready || page.Engine.Reason != "引擎没准备好" {
		t.Fatalf("引擎读数 = %+v", page.Engine)
	}
}

type agentOptionsBody struct {
	Engine struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	} `json:"engine"`
	Keys []struct {
		ID              int64  `json:"id"`
		Label           string `json:"label"`
		Display         string `json:"display"`
		CodexConfigured bool   `json:"codex_configured"`
		CodexAvailable  bool   `json:"codex_available"`
		CodexEnabled    bool   `json:"codex_enabled"`
		Models          []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"models"`
		DefaultModel string `json:"default_model"`
	} `json:"keys"`
	Efforts []string `json:"efforts"`
}

func (b agentOptionsBody) key(id int64) (int, bool) {
	for i, k := range b.Keys {
		if k.ID == id {
			return i, true
		}
	}
	return -1, false
}

func TestHostAgentOptionsAndChatConfig(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	h := withHostAgent(t, e, true)
	// 新建对话的密钥校验换成管理面的真解析（与 Codex 引擎的 Validate 同一条路）。
	h.engine.validate = func(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
		resolved, err := e.srv.CodexChatConfig(ctx, cfg)
		if err != nil {
			return cfg, err
		}
		cfg.KeyDisplay = resolved.KeyDisplay
		if !resolved.Ready {
			return cfg, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: resolved.Reason}
		}
		cfg.Model = resolved.Model
		return cfg, nil
	}
	// 引擎读数也能来自真 Codex 引擎接空组件目录（未安装）。
	mgr := codexappserver.NewManager(codexappserver.Options{DataDir: e.dir, Settings: e.st, Engine: &casEngine{}, ComponentsDir: t.TempDir()})
	if st := hostagent.NewCodexEngine(hostagent.CodexOptions{Manager: mgr, Config: e.srv.CodexChatConfig}).Status(t.Context()); st.Ready || !strings.Contains(st.Reason, "尚未安装") {
		t.Fatalf("未装组件的引擎读数 = %+v", st)
	}

	path := h.agentPath("/options")
	resp := e.do("GET", path, root, "")
	wantStatus(t, resp, http.StatusOK)
	var body agentOptionsBody
	decodeInto(t, resp, &body)
	if !body.Engine.Ready || len(body.Efforts) != 4 || len(body.Keys) != 0 {
		t.Fatalf("初始读数 = %+v", body)
	}
	cfg, err := e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{})
	if err != nil || cfg.Ready || !strings.Contains(cfg.Reason, "选择一把 API 密钥") {
		t.Fatalf("没选密钥时 = %+v, %v", cfg, err)
	}

	// 三把密钥：钉了 Codex 订阅的、只勾了开发工具可见目录模型的、什么都没授权的。
	plain, plainText := e.createKey(root, "智能体")
	catalogOnly, _ := e.createKey(root, "目录")
	other, _ := e.createKey(root, "没授权")
	codex, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderCodex, Label: "codex", AuthJSON: `{"tokens":{"access_token":"fake"}}`})
	if err != nil {
		t.Fatal(err)
	}
	// 连上订阅后收敛一次：目录数据 agents.codex 段的订阅模型行长出来，才有订阅自带的模型可选。
	e.srv.SyncAgentModels(t.Context())
	up, err := e.st.CreateUpstream(t.Context(), "deepseek", config.UpstreamDeepseek, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	catalogModel, err := e.st.CreateModel(t.Context(), "deepseek-v4-flash", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateModelSource(t.Context(), catalogModel.ID, up.ID, "deepseek-v4-flash", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: plain.ID, CodexAccountID: codex.ID, CatalogModelIDs: []int64{catalogModel.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: catalogOnly.ID, CatalogModelIDs: []int64{catalogModel.ID}}); err != nil {
		t.Fatal(err)
	}

	// 读数：每把密钥各带自己在 Codex 面可见的模型，订阅自带的与目录的并列且标明来源。
	resp = e.do("GET", path, root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &body)
	i, ok := body.key(plain.ID)
	if !ok || !body.Keys[i].CodexConfigured || !body.Keys[i].CodexAvailable || !body.Keys[i].CodexEnabled {
		t.Fatalf("钉了订阅的密钥 = %+v", body.Keys)
	}
	sources := map[string]string{}
	for _, m := range body.Keys[i].Models {
		sources[m.Name] = m.Source
	}
	if sources["gpt-6-astra"] != "subscription" || sources["gpt-5.6-sol"] != "subscription" || sources["deepseek-v4-flash"] != "catalog" {
		t.Fatalf("钉了订阅的密钥可见模型 = %+v", body.Keys[i].Models)
	}
	defaultModel := body.Keys[i].DefaultModel
	if defaultModel == "" || sources[defaultModel] == "" {
		t.Fatalf("缺省模型 = %q，期望是可见模型之一", defaultModel)
	}
	j, ok := body.key(catalogOnly.ID)
	if !ok || body.Keys[j].CodexConfigured || !body.Keys[j].CodexEnabled || len(body.Keys[j].Models) != 1 || body.Keys[j].Models[0].Name != "deepseek-v4-flash" || body.Keys[j].DefaultModel != "deepseek-v4-flash" {
		t.Fatalf("只有目录模型的密钥 = %+v", body.Keys)
	}
	k, ok := body.key(other.ID)
	if !ok || body.Keys[k].CodexConfigured || body.Keys[k].CodexEnabled || len(body.Keys[k].Models) != 0 {
		t.Fatalf("没授权的密钥 = %+v", body.Keys)
	}

	// 新建对话的校验：不存在 / Codex 面没有可用模型 / 名字不可见 → 400 agent_key_invalid。
	chats := h.agentPath("/chats")
	for _, tc := range []struct{ body, code string }{
		{`{"key_id":999}`, hostagent.CodeKeyInvalid},
		{`{"key_id":` + jsonNumber(other.ID) + `}`, hostagent.CodeKeyInvalid},
		{`{"key_id":` + jsonNumber(plain.ID) + `,"model":"not-a-model"}`, hostagent.CodeKeyInvalid},
		{`{"key_id":` + jsonNumber(catalogOnly.ID) + `,"model":"gpt-6-astra"}`, hostagent.CodeKeyInvalid},
	} {
		resp := e.do("POST", chats, root, tc.body)
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != tc.code {
			t.Fatalf("%s: 错误码 = %q，期望 %s", tc.body, code, tc.code)
		}
	}
	// 留空模型：落成缺省并钉死；显式选订阅自带的与目录模型都行。
	resp = e.do("POST", chats, root, `{"key_id":`+jsonNumber(plain.ID)+`,"model":"","effort":"high"}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Chat agentChatBody `json:"chat"`
	}
	decodeInto(t, resp, &created)
	if created.Chat.KeyID != plain.ID || created.Chat.Model != defaultModel || created.Chat.Effort != "high" || created.Chat.KeyDisplay == "" || strings.Contains(created.Chat.KeyDisplay, plainText[3:20]) {
		t.Fatalf("留空模型的对话 = %+v", created.Chat)
	}
	for _, model := range []string{"gpt-5.6-sol", "deepseek-v4-flash"} {
		resp = e.do("POST", chats, root, `{"key_id":`+jsonNumber(plain.ID)+`,"model":"`+model+`","effort":""}`)
		wantStatus(t, resp, http.StatusCreated)
		decodeInto(t, resp, &created)
		if created.Chat.Model != model {
			t.Fatalf("选 %s 后 = %+v", model, created.Chat)
		}
	}
	// 引擎凭据解析就绪：明文解封、模型按对话钉死的。
	cfg, err = e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{KeyID: plain.ID, Model: "gpt-5.6-sol", Effort: "high"})
	if err != nil || !cfg.Ready || !strings.HasPrefix(cfg.KeyPlaintext, "sk_") || cfg.Model != "gpt-5.6-sol" || cfg.Effort != "high" || cfg.KeyDisplay == "" {
		t.Fatalf("就绪配置 = %+v, %v", cfg, err)
	}
	// 订阅账号停用：订阅模型的对话不可用、目录模型的照旧。
	if err := e.st.SetAgentStatus(t.Context(), codex.ID, store.AgentStatusDisabled); err != nil {
		t.Fatal(err)
	}
	cfg, _ = e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{KeyID: plain.ID, Model: "deepseek-v4-flash"})
	if !cfg.Ready || cfg.Model != "deepseek-v4-flash" {
		t.Fatalf("订阅停用后目录模型 = %+v", cfg)
	}
	cfg, _ = e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{KeyID: plain.ID, Model: "gpt-5.6-sol"})
	if cfg.Ready || !strings.Contains(cfg.Reason, "不可用") || cfg.KeyPlaintext != "" {
		t.Fatalf("订阅停用后订阅模型 = %+v", cfg)
	}
	if err := e.st.SetAgentStatus(t.Context(), codex.ID, store.AgentStatusActive); err != nil {
		t.Fatal(err)
	}
	// 只有目录模型的密钥也能开对话。
	cfg, _ = e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{KeyID: catalogOnly.ID})
	if !cfg.Ready || cfg.Model != "deepseek-v4-flash" {
		t.Fatalf("目录密钥 = %+v", cfg)
	}
	// 密钥停用：已开的对话提交被拒（400 agent_key_invalid），原因可读；明文不解封。
	resp = e.patchKey(root, plain.ID, `{"disabled":true}`)
	wantStatus(t, resp, http.StatusOK)
	cfg, err = e.srv.CodexChatConfig(t.Context(), hostagent.ChatConfig{KeyID: plain.ID, Model: "gpt-5.6-sol"})
	if err != nil || cfg.Ready || !strings.Contains(cfg.Reason, "停用") || cfg.KeyPlaintext != "" {
		t.Fatalf("停用后 = %+v, %v", cfg, err)
	}
	h.chatID = created.Chat.ID
	resp = e.do("POST", h.chatPath("/runs"), root, `{"text":"x"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != hostagent.CodeKeyInvalid {
		t.Fatalf("错误码 = %q", code)
	}
	// 审计与日志不含明文。
	for _, row := range e.auditRows() {
		if strings.Contains(row.Detail, plainText) {
			t.Fatalf("审计泄漏: %+v", row)
		}
	}
	if strings.Contains(e.buf.String(), plainText) {
		t.Fatal("日志泄漏明文")
	}
}
