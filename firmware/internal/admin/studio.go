package admin

// 智能体——创作工作空间（internal/studio）：「工作空间管理」里 kind = studio 的空间打开后的目录与对话。
// 文件以「子目录/文件名」引用，子目录只有 agent / docs / media（studio.Dirs）；路径参数拆成两段
// {dir}/{name}，上传与改名的目标用 path 给。
//
//	GET    /admin/v1/workspaces/{id}/files                        三个子目录的清单（带附注、有无缩略图）
//	POST   /admin/v1/workspaces/{id}/files?path=<目录/文件名>     上传一个文件（裸二进制体，application/octet-stream；目录须与种类相配）（LAN）
//	GET    /admin/v1/workspaces/{id}/files/{dir}/{name}           内联读取（<img> / <video>，支持 Range）
//	GET    /admin/v1/workspaces/{id}/files/{dir}/{name}/download  附件下载
//	GET    /admin/v1/workspaces/{id}/files/{dir}/{name}/thumb     缩略图（缺图时按需生成，解不开即 404）
//	POST   /admin/v1/workspaces/{id}/files/{dir}/{name}/thumb     页面回传封面帧 {"image": "<base64>"}，?v=原件版本（LAN）
//	PATCH  /admin/v1/workspaces/{id}/files/{dir}/{name}           改名 / 挪目录 {"path": "<新目录/新名>"}                    （LAN）
//	DELETE /admin/v1/workspaces/{id}/files/{dir}/{name}           删除                                                    （LAN）
//	GET    /admin/v1/workspaces/{id}/agent                        进页读数：工作空间、能否对话、对话列表、目录清单
//	GET    /admin/v1/workspaces/{id}/agent/options                新建对话的可选项：引擎（工作节点上有没有它的 CLI，现探）、密钥（各带每种引擎可见的模型与这个空间启用的生成模型）
//	GET    /admin/v1/workspaces/{id}/agent/media                  媒体生成能力：设备上的全部生成模型，各带是否启用与使用场景说明
//	PUT    /admin/v1/workspaces/{id}/agent/media                  整份替换启用的模型 {"models":[{"model","usage"}]}（LAN）
//	POST   /admin/v1/workspaces/{id}/agent/chats                  新建对话（LAN）
//	GET    /admin/v1/workspaces/{id}/agent/chats/{chat_id}        对话读数：对话、会话状态、最近事件
//	DELETE /admin/v1/workspaces/{id}/agent/chats/{chat_id}        删除对话（LAN）
//	GET    …/chats/{chat_id}/wait?revision=&after=                陪等（零轮询例外）
//	POST   …/chats/{chat_id}/runs                                 提交指令（文本 + 图片），202（LAN）
//	DELETE …/chats/{chat_id}/runs/{run_id}                        取消 / 中止（LAN）
//	POST   …/chats/{chat_id}/stop                                 结束会话（LAN）
//	POST   …/chats/{chat_id}/archive                              归档：结束会话，之后只能查看（LAN）
//	GET    …/chats/{chat_id}/events?before=&limit=                翻历史
//
// 写端点恒 LANOnly（它们改设备上的文件、让智能体调用订阅生成）。审计只记工作空间、对话 id、
// 指令 id、文件名、字节数与动作，不记指令正文、图片、回复、提示词与密钥明文（§15.1）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

// 审计事件名。
const (
	EventWorkspaceFileUpload = "workspace.file_upload"
	EventWorkspaceFileDelete = "workspace.file_delete"
	EventWorkspaceFileRename = "workspace.file_rename"
	EventStudioChatCreate    = "studio.chat_create"
	EventStudioChatDelete    = "studio.chat_delete"
	EventStudioChatArchive   = "studio.chat_archive"
	EventStudioMediaConfig   = "studio.media_config"
	EventStudioInstruction   = "studio.instruction"
	EventStudioCancel        = "studio.instruction_cancel"
	EventStudioStop          = "studio.session_stop"
)

// SetStudio 注入创作工作空间管理器（gatewayd 装配期调用一次）。
func (s *Server) SetStudio(m *studio.Manager) { s.studio = m }

func (s *Server) requireStudio(w http.ResponseWriter) bool {
	if s.studio == nil {
		writeError(w, http.StatusServiceUnavailable, "studio_unavailable", "本进程未接入创作工作空间")
		return false
	}
	return true
}

// studioWorkspaceRow 取工作空间行并确认它是创作工作空间。
func (s *Server) studioWorkspaceRow(w http.ResponseWriter, r *http.Request) (*store.Workspace, bool) {
	if !s.requireStudio(w) {
		return nil, false
	}
	ws, err := s.st.GetWorkspace(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return nil, false
	}
	if !ws.IsStudio() {
		writeError(w, http.StatusConflict, "workspace_kind_mismatch", "这不是创作工作空间")
		return nil, false
	}
	return ws, true
}

func (s *Server) studioChatRow(w http.ResponseWriter, r *http.Request) (*store.Workspace, *store.StudioChat, bool) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return nil, nil, false
	}
	chat, err := s.studio.GetChat(r.Context(), ws.ID, r.PathValue("chat_id"))
	if err != nil {
		s.writeStudioError(w, r, err)
		return nil, nil, false
	}
	return ws, chat, true
}

// studioFilePath 是路由里 {dir}/{name} 两段拼成的路径（校验在 studio 包）。
func studioFilePath(r *http.Request) string {
	return studio.JoinPath(r.PathValue("dir"), r.PathValue("name"))
}

// studioFileJSON 是一个文件的读数：附注 + 有无缩略图 + 补图时校验的原件版本。
type studioFileJSON struct {
	store.StudioFile
	HasThumb       bool   `json:"has_thumb"`
	SourceRevision string `json:"source_revision"`
}

func (s *Server) studioFilesJSON(ws *store.Workspace, files []store.StudioFile) []studioFileJSON {
	out := make([]studioFileJSON, 0, len(files))
	for _, f := range files {
		out = append(out, studioFileJSON{StudioFile: f, HasThumb: s.studio.HasThumb(ws, f.Name), SourceRevision: studio.SourceRevision(f)})
	}
	return out
}

func (s *Server) handleStudioFiles(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	files, err := s.studio.ListFiles(r.Context(), ws)
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Files []studioFileJSON `json:"files"`
	}{Files: s.studioFilesJSON(ws, files)})
}

// handleStudioUpload 收一个裸二进制体，目标路径在查询串 path（「子目录/文件名」）。
func (s *Server) handleStudioUpload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if _, _, err := studio.ValidatePath(p); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, studio.MaxUploadBytes+(1<<20))
	f, err := s.studio.SaveFile(r.Context(), ws, p, r.Body, studio.MaxUploadBytes, studio.FileMeta{Origin: store.StudioFileOriginUpload})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, studio.CodeFileTooLarge, "文件超过大小上限")
			return
		}
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceFileUpload, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("向工作空间 %s 上传 %s（%d 字节）", ws.Name, f.Name, f.Bytes), RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusCreated, struct {
		File studioFileJSON `json:"file"`
	}{File: studioFileJSON{StudioFile: *f, HasThumb: s.studio.HasThumb(ws, f.Name), SourceRevision: studio.SourceRevision(*f)}})
}

func (s *Server) handleStudioFile(w http.ResponseWriter, r *http.Request) {
	s.serveStudioFile(w, r, "inline")
}

func (s *Server) handleStudioFileDownload(w http.ResponseWriter, r *http.Request) {
	s.serveStudioFile(w, r, "attachment")
}

func (s *Server) serveStudioFile(w http.ResponseWriter, r *http.Request, disposition string) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	if err := s.studio.ServeFile(w, r, ws, studioFilePath(r), disposition); err != nil {
		s.writeStudioError(w, r, err)
	}
}

func (s *Server) handleStudioThumb(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	if err := s.studio.EnsureThumb(r.Context(), ws, studioFilePath(r)); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	m, err := s.studio.OpenThumb(ws, studioFilePath(r))
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	defer m.Close()
	mediagen.ServeMedia(w, r, m, "inline")
}

func (s *Server) handleStudioThumbUpload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	data, ok := decodeThumbUpload(w, r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "请求格式不正确")
		return
	}
	if err := s.studio.SaveThumb(r.Context(), ws, studioFilePath(r), data, r.URL.Query().Get("v")); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStudioFileRename(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	from, to := studioFilePath(r), strings.TrimSpace(req.Path)
	if err := s.studio.RenameFile(r.Context(), ws, from, to); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceFileRename, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("工作空间 %s：%s 改名为 %s", ws.Name, from, to), RemoteIP: remoteIP(r)})
	f, err := s.st.GetStudioFile(r.Context(), ws.ID, to)
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		File studioFileJSON `json:"file"`
	}{File: studioFileJSON{StudioFile: *f, HasThumb: s.studio.HasThumb(ws, to), SourceRevision: studio.SourceRevision(*f)}})
}

func (s *Server) handleStudioFileDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	name := studioFilePath(r)
	if err := s.studio.DeleteFile(r.Context(), ws, name); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceFileDelete, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("删除工作空间 %s 的文件 %s", ws.Name, name), RemoteIP: remoteIP(r)})
	w.WriteHeader(http.StatusNoContent)
}

// ---- 对话 ----

// studioEventJSON 是时间线事件的响应形态：run / session / error 三种的 body 是给人看的原因，
// 复制到 reason 按 Accept-Language 本地化；其余种类的 body 是指令、回复、提示词这类原文。
type studioEventJSON struct {
	store.StudioEvent
	Reason string `json:"reason,omitempty" i18n:"text"`
}

func studioEventsJSON(events []store.StudioEvent) []studioEventJSON {
	out := make([]studioEventJSON, 0, len(events))
	for _, ev := range events {
		j := studioEventJSON{StudioEvent: ev}
		switch ev.Kind {
		case store.StudioEventRun, store.StudioEventSession, store.StudioEventError:
			j.Reason = ev.Body
		}
		out = append(out, j)
	}
	return out
}

type studioPageJSON struct {
	Workspace workspaceJSON `json:"workspace"`
	// ChatsEnabled 为假时这个空间不能新建对话与提交指令（设备上的空间），ChatsReason 说明原因。
	ChatsEnabled bool             `json:"chats_enabled"`
	ChatsReason  string           `json:"chats_reason,omitempty" i18n:"text"`
	Chats        []studio.Chat    `json:"chats"`
	Files        []studioFileJSON `json:"files"`
}

type studioChatJSON struct {
	Chat   studio.Chat       `json:"chat"`
	State  studio.State      `json:"state"`
	Events []studioEventJSON `json:"events"`
}

type studioWaitJSON struct {
	State  studio.State      `json:"state"`
	Events []studioEventJSON `json:"events"`
}

type studioStateJSON struct {
	State studio.State `json:"state"`
}

// studioEngineKeyJSON 是一把密钥在一种引擎对应的开发工具接入面上的读数：Enabled = 钉了该订阅
// 账号或有开发工具可见的目录模型；Models 是此刻可见的模型，DefaultModel 是留空时会落成的模型。
type studioEngineKeyJSON struct {
	Enabled      bool             `json:"enabled"`
	Configured   bool             `json:"configured"`
	Available    bool             `json:"available"`
	Models       []agentModelJSON `json:"models"`
	DefaultModel string           `json:"default_model"`
}

// studioKeyJSON 是新建对话时密钥选择器的一项：每种引擎上的可见模型（按引擎 id）加这把密钥能用的
// 生成模型与各自的可用性（工具用，内核的同一份裁决）。只有未停用且留有封存明文的能选。
type studioKeyJSON struct {
	ID                 int64                          `json:"id"`
	Label              string                         `json:"label"`
	Display            string                         `json:"display"`
	Disabled           bool                           `json:"disabled"`
	PlaintextAvailable bool                           `json:"plaintext_available"`
	Engines            map[string]studioEngineKeyJSON `json:"engines"`
	MediaModels        []mediagen.Availability        `json:"media_models"`
}

type studioOptionsJSON struct {
	Engines []studio.EngineOption `json:"engines"`
	Keys    []studioKeyJSON       `json:"keys"`
}

func (s *Server) handleStudioPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	chats, err := s.studio.ListChats(r.Context(), ws.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	files, err := s.studio.ListFiles(r.Context(), ws)
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	hosts, creds, err := s.workspaceContext(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	enabled, reason := studio.ChatsEnabled(ws)
	writeJSON(w, http.StatusOK, studioPageJSON{
		Workspace:    workspaceView(*ws, hosts, creds),
		ChatsEnabled: enabled,
		ChatsReason:  reason,
		Chats:        chats,
		Files:        s.studioFilesJSON(ws, files),
	})
}

func (s *Server) handleStudioOptions(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := studioOptionsJSON{Engines: s.studio.EngineOptions(ctx, ws), Keys: []studioKeyJSON{}}
	keys, err := s.st.ListAPIKeys(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	resolver := s.devToolResolver()
	for i := range keys {
		k := &keys[i]
		snapshot, err := resolver.Snapshot(ctx, k.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		media, err := s.studio.KeyModels(ctx, ws.ID, k.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		row := studioKeyJSON{ID: k.ID, Label: k.Label, Display: keyDisplayOf(k), Disabled: k.Disabled, PlaintextAvailable: k.PlaintextAvailable,
			Engines: make(map[string]studioEngineKeyJSON, len(out.Engines)), MediaModels: media}
		for _, e := range out.Engines {
			sub := snapshot.Subscription(e.Tool)
			surface := snapshot.Tools[e.Tool]
			models := make([]agentModelJSON, 0, len(surface.Models))
			for _, m := range surface.Models {
				models = append(models, agentModelJSON{Name: m.Name, Source: m.Source})
			}
			row.Engines[e.ID] = studioEngineKeyJSON{Enabled: snapshot.ToolEnabled(e.Tool), Configured: sub.Configured, Available: sub.Available,
				Models: models, DefaultModel: surface.DefaultModel}
		}
		out.Keys = append(out.Keys, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// studioMediaJSON 是媒体生成能力的读数：能力表里的全部模型各带启用状态与说明。
type studioMediaJSON struct {
	Models []studio.MediaModel `json:"models"`
}

func (s *Server) writeStudioMedia(w http.ResponseWriter, r *http.Request, ws *store.Workspace) {
	models, err := s.studio.MediaConfig(r.Context(), ws)
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, studioMediaJSON{Models: models})
}

func (s *Server) handleStudioMedia(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	s.writeStudioMedia(w, r, ws)
}

func (s *Server) handleStudioMediaUpdate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Models []store.StudioMediaModel `json:"models"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.studio.SetMediaConfig(r.Context(), ws, req.Models); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	names := make([]string, 0, len(req.Models))
	for _, m := range req.Models {
		names = append(names, strings.TrimSpace(m.Model))
	}
	// 只记启用了哪些模型；使用场景说明是管理员写给智能体的正文，不进审计。
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioMediaConfig, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("更新工作空间 %s 的媒体生成能力：启用 %d 个模型（%s）", ws.Name, len(names), strings.Join(names, "、")), RemoteIP: remoteIP(r)})
	s.writeStudioMedia(w, r, ws)
}

func (s *Server) handleStudioChatCreate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.studioWorkspaceRow(w, r)
	if !ok {
		return
	}
	var req struct {
		Title  string `json:"title"`
		Engine string `json:"engine"`
		KeyID  int64  `json:"key_id"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	chat, err := s.studio.CreateChat(r.Context(), ws, studio.NewChat{Title: req.Title, Engine: req.Engine, KeyID: req.KeyID, Model: req.Model, Effort: req.Effort})
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioChatCreate, Entity: workspaceEntity(ws.ID),
		Detail:   fmt.Sprintf("在工作空间 %s 新建对话 %s（引擎 %s，密钥 %s，模型 %s，档位 %s）", ws.Name, chat.ID, chat.Engine, chat.KeyDisplay, chat.Model, orDefault(chat.Effort)),
		RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusCreated, struct {
		Chat studio.Chat `json:"chat"`
	}{Chat: studio.Chat{StudioChat: *chat, Status: hostagent.StatusIdle}})
}

func (s *Server) handleStudioChat(w http.ResponseWriter, r *http.Request) {
	_, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	events, err := s.st.ListStudioEvents(r.Context(), store.StudioEventQuery{ChatID: chat.ID, Limit: initialEvents})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	state := s.studio.Snapshot(chat.ID)
	writeJSON(w, http.StatusOK, studioChatJSON{
		Chat:   studio.Chat{StudioChat: *chat, Status: state.Status, Busy: state.Current != nil || len(state.Queue) > 0},
		State:  state,
		Events: studioEventsJSON(events),
	})
}

func (s *Server) handleStudioChatDelete(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	if err := s.studio.DeleteChat(r.Context(), ws.ID, chat.ID); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioChatDelete, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("删除工作空间 %s 的对话 %s", ws.Name, chat.ID), RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusOK, struct {
		Deleted string `json:"deleted"`
	}{Deleted: chat.ID})
}

func (s *Server) handleStudioChatArchive(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	archived, err := s.studio.ArchiveChat(r.Context(), ws.ID, chat.ID)
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	if chat.ArchivedAt == nil {
		s.audit(r.Context(), store.AuditEvent{Event: EventStudioChatArchive, Entity: workspaceEntity(ws.ID),
			Detail: fmt.Sprintf("归档工作空间 %s 的对话 %s", ws.Name, chat.ID), RemoteIP: remoteIP(r)})
	}
	state := s.studio.Snapshot(chat.ID)
	writeJSON(w, http.StatusOK, struct {
		Chat  studio.Chat  `json:"chat"`
		State studio.State `json:"state"`
	}{Chat: studio.Chat{StudioChat: *archived, Status: state.Status}, State: state})
}

func (s *Server) handleStudioWait(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	revision, err1 := queryInt64(r, "revision")
	after, err2 := queryInt64(r, "after")
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "revision 与 after 须是整数")
		return
	}
	state, err := s.studio.Wait(r.Context(), ws.ID, chat.ID, revision)
	if err != nil {
		var he *hostagent.Error
		if errors.As(err, &he) {
			s.writeStudioError(w, r, err)
		}
		return
	}
	events, err := s.st.ListStudioEvents(r.Context(), store.StudioEventQuery{ChatID: chat.ID, AfterID: after, Limit: 1000})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, studioWaitJSON{State: state, Events: studioEventsJSON(events)})
}

func (s *Server) handleStudioSubmit(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
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
	run, err := s.studio.Submit(r.Context(), ws, chat.ID, hostagent.Input{Text: req.Text, Images: req.Images})
	if err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioInstruction, Entity: workspaceEntity(ws.ID),
		Detail:   fmt.Sprintf("向工作空间 %s 的对话 %s 提交指令 %s（%d 字，%d 张图片）", ws.Name, chat.ID, run.ID, len([]rune(run.Text)), run.ImageCount),
		RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusAccepted, struct {
		Run   *store.StudioRun `json:"run"`
		State studio.State     `json:"state"`
	}{Run: run, State: s.studio.Snapshot(chat.ID)})
}

func (s *Server) handleStudioCancel(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	if err := s.studio.Cancel(r.Context(), ws.ID, chat.ID, runID); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioCancel, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("取消工作空间 %s 的指令 %s", ws.Name, runID), RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusOK, studioStateJSON{State: s.studio.Snapshot(chat.ID)})
}

func (s *Server) handleStudioStop(w http.ResponseWriter, r *http.Request) {
	ws, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	if err := s.studio.Stop(r.Context(), ws.ID, chat.ID); err != nil {
		s.writeStudioError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventStudioStop, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("结束工作空间 %s 的对话 %s 的智能体会话", ws.Name, chat.ID), RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusOK, studioStateJSON{State: s.studio.Snapshot(chat.ID)})
}

func (s *Server) handleStudioChatEvents(w http.ResponseWriter, r *http.Request) {
	_, chat, ok := s.studioChatRow(w, r)
	if !ok {
		return
	}
	hq := store.AgentHostEventQuery{}
	if !eventQuery(w, r, &hq) {
		return
	}
	events, err := s.st.ListStudioEvents(r.Context(), store.StudioEventQuery{ChatID: chat.ID, BeforeID: hq.BeforeID, Limit: hq.Limit})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Events []studioEventJSON `json:"events"`
	}{Events: studioEventsJSON(events)})
}

// writeStudioError 把创作工作空间的错误映射成统一错误体。
func (s *Server) writeStudioError(w http.ResponseWriter, r *http.Request, err error) {
	var se *hostagent.Error
	if errors.As(err, &se) {
		status := http.StatusConflict
		switch se.Code {
		case hostagent.CodeInvalidInput, hostagent.CodeKeyInvalid, studio.CodeFileInvalid:
			status = http.StatusBadRequest
		case hostagent.CodeRunNotFound, hostagent.CodeChatNotFound, studio.CodeFileNotFound:
			status = http.StatusNotFound
		case studio.CodeFileTooLarge:
			status = http.StatusRequestEntityTooLarge
		case studio.CodeFileExists, studio.CodeFileChanged, studio.CodeChatArchived, hostagent.CodeEngineNotReady, hostagent.CodeRunFinished:
			status = http.StatusConflict
		case hostagent.CodeUnavailable:
			status = http.StatusServiceUnavailable
		default:
			// 主机上的空间：守护进程 / 透传层的错误码沿用工作节点那一套状态映射。
			if code, st, ok := devhost.StatusFromError(&devhost.Error{Code: se.Code, Msg: se.Msg}); ok {
				writeError(w, st, code, se.Msg)
				return
			}
		}
		writeError(w, status, se.Code, se.Msg)
		return
	}
	if _, _, ok := devhost.StatusFromError(err); ok {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeWorkspaceError(w, r, err)
}
