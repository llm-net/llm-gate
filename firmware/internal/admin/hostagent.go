package admin

// 智能体——Agent远控（internal/hostagent）：「主机/SoC」表里每台主机的「Agent远控」页。
//
//	GET    /admin/v1/agent-hosts/{id}/agent                          进页读数：主机、引擎、对话列表、档案、最近操作日志
//	GET    /admin/v1/agent-hosts/{id}/agent/options                  新建对话的可选项（hostagentoptions.go）
//	POST   /admin/v1/agent-hosts/{id}/agent/chats                    新建对话：标题、API 密钥、模型、推理档位（之后不改）（LAN）
//	GET    /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}          对话读数：对话、会话状态、最近事件
//	DELETE /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}          删除对话（结束会话；操作记录留给主机）        （LAN）
//	GET    /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/wait     陪等（?revision=&after=）：会话 revision 变化或 30 秒即回一帧（零轮询例外）
//	POST   /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/runs     提交一条指令（文本 + 图片），202 受理进队列   （LAN）
//	DELETE /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/runs/{run_id}  取消排队中 / 中止执行中的指令           （LAN）
//	POST   /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/stop     结束会话：清空队列、中止当前、关引擎          （LAN）
//	GET    /admin/v1/agent-hosts/{id}/agent/chats/{chat_id}/events   翻这个对话的历史（before / limit）
//	GET    /admin/v1/agent-hosts/{id}/agent/events                   整台主机的历史（before / limit / ops=1 只取操作日志）
//	GET    /admin/v1/agent-hosts/{id}/agent/profile                  主机档案
//	PUT    /admin/v1/agent-hosts/{id}/agent/profile                  管理员整份替换档案                            （LAN）
//	GET    /admin/v1/agent-hosts/{id}/agent/files[/preview|/raw]     只读浏览主机文件（hostagentfiles.go）          （LAN）
//
// 写端点恒 LANOnly：它们让智能体在另一台机器上执行命令、写文件。读数是 Admin 档。
// 审计只记主机、对话 id、指令 id、长度与动作，不记指令正文、图片、回复与密钥明文（§15.1）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventAgentHostChatCreate  = "agent_host.chat_create"
	EventAgentHostChatDelete  = "agent_host.chat_delete"
	EventAgentHostInstruction = "agent_host.instruction"
	EventAgentHostCancel      = "agent_host.instruction_cancel"
	EventAgentHostStop        = "agent_host.session_stop"
	EventAgentHostProfile     = "agent_host.profile_update"
)

// agentSubmitBodyLimit 是提交指令请求体的上限：至多 5 张 8 MiB 图片的 base64 加正文。
const agentSubmitBodyLimit = 60 << 20

// initialEvents 是进对话一次带回的最近事件数（进页的操作日志同量）。
const initialEvents = 200

// SetHostAgent 注入 Agent远控管理器（gatewayd 装配期调用一次）。
func (s *Server) SetHostAgent(m *hostagent.Manager) { s.hostAgent = m }

func (s *Server) requireHostAgent(w http.ResponseWriter) bool {
	if s.hostAgent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_unavailable", "本进程未接入 Agent远控")
		return false
	}
	return true
}

// agentEventJSON 是时间线事件的响应形态：run / session / error 三种的 body 是给人看的
// 原因（「已中止」「空闲超时…」），复制到 reason 按 Accept-Language 本地化；其余种类的
// body 是指令、回复、命令输出这类原文，不能翻，所以 body 本身不标 i18n。
type agentEventJSON struct {
	store.AgentHostEvent
	Reason string `json:"reason,omitempty" i18n:"text"`
}

func agentEventsJSON(events []store.AgentHostEvent) []agentEventJSON {
	out := make([]agentEventJSON, 0, len(events))
	for _, ev := range events {
		j := agentEventJSON{AgentHostEvent: ev}
		switch ev.Kind {
		case store.AgentEventRun, store.AgentEventSession, store.AgentEventError:
			j.Reason = ev.Body
		}
		out = append(out, j)
	}
	return out
}

// agentPageJSON 是进页读数。
type agentPageJSON struct {
	Host    agentHostJSON          `json:"host"`
	Engine  hostagent.EngineStatus `json:"engine"`
	Chats   []hostagent.Chat       `json:"chats"`
	Profile store.AgentHostProfile `json:"profile"`
	// Ops 是整台主机最近的操作日志（跨全部对话）。
	Ops []agentEventJSON `json:"ops"`
}

// agentChatJSON 是一个对话的读数。
type agentChatJSON struct {
	Chat   hostagent.Chat   `json:"chat"`
	State  hostagent.State  `json:"state"`
	Events []agentEventJSON `json:"events"`
}

type agentWaitJSON struct {
	State  hostagent.State  `json:"state"`
	Events []agentEventJSON `json:"events"`
}

type agentStateJSON struct {
	State hostagent.State `json:"state"`
}

// opsKinds 是「操作日志」视图取的事件种类：对主机的操作与指令 / 会话边界。
var opsKinds = []string{store.AgentEventCommand, store.AgentEventFile, store.AgentEventProfile, store.AgentEventRun, store.AgentEventSession, store.AgentEventUser}

// agentHostRow 取主机行并确认它在（Agent远控端点都挂在一台主机下）。
func (s *Server) agentHostRow(w http.ResponseWriter, r *http.Request) (*store.AgentHost, bool) {
	if !s.requireAgentHosts(w) || !s.requireHostAgent(w) {
		return nil, false
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return nil, false
	}
	host, err := s.hosts.Get(r.Context(), id)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return nil, false
	}
	return host, true
}

// agentChatRow 取主机行与它名下的对话。
func (s *Server) agentChatRow(w http.ResponseWriter, r *http.Request) (*store.AgentHost, *store.AgentHostChat, bool) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return nil, nil, false
	}
	chat, err := s.hostAgent.GetChat(r.Context(), host.ID, r.PathValue("chat_id"))
	if err != nil {
		s.writeHostAgentError(w, r, err)
		return nil, nil, false
	}
	return host, chat, true
}

func (s *Server) handleAgentPage(w http.ResponseWriter, r *http.Request) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return
	}
	cert, err := s.hosts.CurrentCertificate(r.Context())
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	chats, err := s.hostAgent.ListChats(r.Context(), host.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	profile, err := s.st.GetAgentHostProfile(r.Context(), host.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ops, err := s.st.ListAgentHostEvents(r.Context(), store.AgentHostEventQuery{HostID: host.ID, Kinds: opsKinds, Limit: initialEvents})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentPageJSON{
		Host:    hostJSON(*host, cert),
		Engine:  s.hostAgent.EngineStatus(r.Context()),
		Chats:   chats,
		Profile: profile,
		Ops:     agentEventsJSON(ops),
	})
}

func (s *Server) handleAgentChatCreate(w http.ResponseWriter, r *http.Request) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Title  string `json:"title"`
		KeyID  int64  `json:"key_id"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	chat, err := s.hostAgent.CreateChat(r.Context(), host.ID, hostagent.NewChat{Title: req.Title, KeyID: req.KeyID, Model: req.Model, Effort: req.Effort})
	if err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostChatCreate, Entity: agentHostEntity(host.ID),
		Detail:   fmt.Sprintf("在 %s 新建对话 %s（密钥 %s，模型 %s，档位 %s）", hostLabel(*host), chat.ID, chat.KeyDisplay, chat.Model, orDefault(chat.Effort)),
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusCreated, struct {
		Chat hostagent.Chat `json:"chat"`
	}{Chat: hostagent.Chat{AgentHostChat: *chat, Status: hostagent.StatusIdle}})
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func (s *Server) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	_, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	events, err := s.st.ListAgentHostEvents(r.Context(), store.AgentHostEventQuery{HostID: chat.HostID, ChatID: chat.ID, Limit: initialEvents})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	state := s.hostAgent.Snapshot(chat.ID)
	writeJSON(w, http.StatusOK, agentChatJSON{
		Chat:   hostagent.Chat{AgentHostChat: *chat, Status: state.Status, Busy: state.Current != nil || len(state.Queue) > 0},
		State:  state,
		Events: agentEventsJSON(events),
	})
}

func (s *Server) handleAgentChatDelete(w http.ResponseWriter, r *http.Request) {
	host, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	if err := s.hostAgent.DeleteChat(r.Context(), host.ID, chat.ID); err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostChatDelete, Entity: agentHostEntity(host.ID),
		Detail: fmt.Sprintf("删除 %s 的对话 %s", hostLabel(*host), chat.ID), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Deleted string `json:"deleted"`
	}{Deleted: chat.ID})
}

func (s *Server) handleAgentWait(w http.ResponseWriter, r *http.Request) {
	host, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	revision, err1 := queryInt64(r, "revision")
	after, err2 := queryInt64(r, "after")
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "revision 与 after 须是整数")
		return
	}
	state, err := s.hostAgent.Wait(r.Context(), host.ID, chat.ID, revision)
	if err != nil {
		// 页面关了（或对话没了）：不写任何东西。
		var he *hostagent.Error
		if errors.As(err, &he) {
			s.writeHostAgentError(w, r, err)
		}
		return
	}
	events, err := s.st.ListAgentHostEvents(r.Context(), store.AgentHostEventQuery{HostID: host.ID, ChatID: chat.ID, AfterID: after, Limit: 1000})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentWaitJSON{State: state, Events: agentEventsJSON(events)})
}

// queryInt64 读一个可缺省的整数查询参数（缺省 0）。
func queryInt64(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

func (s *Server) handleAgentSubmit(w http.ResponseWriter, r *http.Request) {
	host, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, agentSubmitBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "agent_invalid_input", "指令与图片合计超过大小上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON 或含未知字段")
		return
	}
	run, err := s.hostAgent.Submit(r.Context(), host.ID, chat.ID, hostagent.Input{Text: req.Text, Images: req.Images})
	if err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostInstruction, Entity: agentHostEntity(host.ID),
		Detail:   fmt.Sprintf("向 %s 的对话 %s 提交指令 %s（%d 字，%d 张图片）", hostLabel(*host), chat.ID, run.ID, len([]rune(run.Text)), run.ImageCount),
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusAccepted, struct {
		Run   *store.AgentHostRun `json:"run"`
		State hostagent.State     `json:"state"`
	}{Run: run, State: s.hostAgent.Snapshot(chat.ID)})
}

func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	host, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	if err := s.hostAgent.Cancel(r.Context(), host.ID, chat.ID, runID); err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostCancel, Entity: agentHostEntity(host.ID),
		Detail: fmt.Sprintf("取消 %s 的指令 %s", hostLabel(*host), runID), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, agentStateJSON{State: s.hostAgent.Snapshot(chat.ID)})
}

func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	host, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	if err := s.hostAgent.Stop(r.Context(), host.ID, chat.ID); err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostStop, Entity: agentHostEntity(host.ID),
		Detail: fmt.Sprintf("结束 %s 的对话 %s 的智能体会话", hostLabel(*host), chat.ID), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, agentStateJSON{State: s.hostAgent.Snapshot(chat.ID)})
}

// eventQuery 解析 before / limit 查询参数。
func eventQuery(w http.ResponseWriter, r *http.Request, q *store.AgentHostEventQuery) bool {
	if v := r.URL.Query().Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "before 须是正整数")
			return false
		}
		q.BeforeID = id
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit 须是正整数")
			return false
		}
		q.Limit = n
	}
	return true
}

func (s *Server) writeEvents(w http.ResponseWriter, r *http.Request, q store.AgentHostEventQuery) {
	events, err := s.st.ListAgentHostEvents(r.Context(), q)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Events []agentEventJSON `json:"events"`
	}{Events: agentEventsJSON(events)})
}

// handleAgentChatEvents 翻一个对话的历史。
func (s *Server) handleAgentChatEvents(w http.ResponseWriter, r *http.Request) {
	_, chat, ok := s.agentChatRow(w, r)
	if !ok {
		return
	}
	q := store.AgentHostEventQuery{HostID: chat.HostID, ChatID: chat.ID}
	if !eventQuery(w, r, &q) {
		return
	}
	s.writeEvents(w, r, q)
}

// handleAgentEvents 翻整台主机的历史（ops=1 只取操作日志，跨全部对话）。
func (s *Server) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return
	}
	q := store.AgentHostEventQuery{HostID: host.ID}
	if !eventQuery(w, r, &q) {
		return
	}
	if r.URL.Query().Get("ops") == "1" {
		q.Kinds = opsKinds
	}
	s.writeEvents(w, r, q)
}

func (s *Server) handleAgentProfile(w http.ResponseWriter, r *http.Request) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return
	}
	profile, err := s.st.GetAgentHostProfile(r.Context(), host.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Profile store.AgentHostProfile `json:"profile"`
	}{Profile: profile})
}

func (s *Server) handleAgentProfileUpdate(w http.ResponseWriter, r *http.Request) {
	host, ok := s.agentHostRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Content) > hostagent.MaxProfileBytes {
		writeError(w, http.StatusBadRequest, "agent_invalid_input", fmt.Sprintf("主机档案不能超过 %d KiB", hostagent.MaxProfileBytes>>10))
		return
	}
	profile, err := s.hostAgent.SetProfile(r.Context(), host.ID, req.Content)
	if err != nil {
		s.writeHostAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostProfile, Entity: agentHostEntity(host.ID),
		Detail: fmt.Sprintf("管理员修改 %s 的主机档案（%d 字节）", hostLabel(*host), len(profile.Content)), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Profile store.AgentHostProfile `json:"profile"`
	}{Profile: profile})
}

// writeHostAgentError 把 Agent远控的错误映射成统一错误体。
func (s *Server) writeHostAgentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host_not_found", "主机不存在")
		return
	}
	var he *hostagent.Error
	if errors.As(err, &he) {
		status := http.StatusConflict
		switch he.Code {
		case hostagent.CodeInvalidInput, hostagent.CodeKeyInvalid:
			status = http.StatusBadRequest
		case hostagent.CodeRunNotFound, hostagent.CodeChatNotFound:
			status = http.StatusNotFound
		case hostagent.CodeUnavailable:
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, he.Code, he.Msg)
		return
	}
	s.writeAgentHostError(w, r, err)
}

func hostLabel(h store.AgentHost) string {
	if strings.TrimSpace(h.Name) != "" {
		return h.Name
	}
	return h.Address
}
