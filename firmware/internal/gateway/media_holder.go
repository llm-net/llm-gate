// media_holder.go 是媒体生成凭 API 密钥自证的数据面端点（/gate-helper/v1/media/*）：设备界面
// 的 Key 持有人页（/ui/connect）与 `gate media` 子命令共用，来源分别记 page 与 cli（gate 的
// 请求带 X-LLMGate-Client 标识头）。这是 gate 的私有协议，不是协议面，没有公开文档或兼容承诺。
// 内核与管理面共用同一份 mediagen.Service（装配期经 SetMediaJobs 注入）；这里只做四件事：
//
//   - Key 鉴权（withAuth）；模型可用性由内核按这把 Key 裁决（订阅按开发工具策略钉死的账号，
//     按量按可用模型范围与三层启停），与 /agents/<tool>/、/ark、/minimax 同口径；
//   - 归属：任务行记发起 Key 的 id 与展示串，持有人只读、等、删自己的任务，别人的任务一律
//     404（不区分「不存在」与「不是你的」）；列表只列页面提交的，cli 任务只能凭 id 取；
//   - 并发闸：每把 Key 同时最多 mediagen.RunningPerKey 个未到终态的任务，全部来源合计，由内核
//     Submit 在同一把锁里数与落库（429 media_busy，名额不够时整批拒绝）；
//   - 编码：错误体与任务行按管理面的形状（{"error":{"code","message"}}，`i18n:"text"` 字段按
//     Accept-Language 本地化）。
//
// §15.1：不记提示词与媒体。
package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/i18n"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// SetMediaJobs 注入媒体生成的任务内核（装配期一次性；生产由 admin.Server 建）。
// nil = 未接线：整组端点答 503。
func (s *Server) SetMediaJobs(p *mediagen.Service) { s.mediaJobs = p }

func (s *Server) registerMediaRoutes(mux *tunnelctx.Mux) {
	mux.Handle("GET /gate-helper/v1/media/models", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyMediaModels)))
	mux.Handle("GET /gate-helper/v1/media/jobs", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyMediaJobs)))
	mux.Handle("GET /gate-helper/v1/media/jobs/{id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyGetMediaJob)))
	mux.Handle("POST /gate-helper/v1/media/jobs", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyCreateMediaJob)))
	mux.Handle("DELETE /gate-helper/v1/media/jobs", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyClearMediaJobs)))
	mux.Handle("DELETE /gate-helper/v1/media/jobs/{id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyDeleteMediaJob)))
	mux.Handle("POST /gate-helper/v1/media/jobs/{id}/refresh", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyRefreshMediaJob)))
	mux.Handle("POST /gate-helper/v1/media/jobs/{id}/wait", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyWaitMediaJob)))
	mux.Handle("GET /gate-helper/v1/media/jobs/{id}/download", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyDownloadMediaJob)))
	mux.Handle("GET /gate-helper/v1/media/jobs/{id}/media", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyMediaMediaJob)))
	mux.Handle("GET /gate-helper/v1/media/jobs/{id}/thumb", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyThumbMediaJob)))
	mux.Handle("POST /gate-helper/v1/media/jobs/{id}/thumb", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleKeyUploadMediaThumb)))
}

// mediaLang 取这次请求协商出的界面语言（Key 持有人页面每个请求都带 Accept-Language）。
func mediaLang(r *http.Request) i18n.Lang { return i18n.Negotiate(r.Header.Get("Accept-Language")) }

// writeMediaJSON 写出管理面形状的成功响应：no-store，`i18n:"text"` 字段本地化。
func writeMediaJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(i18n.Localize(mediaLang(r), v))
}

// writeMediaErr 写出管理面形状的错误体（message 中文原文，按 Accept-Language 本地化）。
func writeMediaErr(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": i18n.T(mediaLang(r), message)}})
}

// mediaServiceError 把内核错误翻成响应（mediagen.HTTPError，与管理面同一张表）；ctx 已取消时不写。
func (s *Server) mediaServiceError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg, retryAfter, ok := mediagen.HTTPError(err)
	if !ok {
		return
	}
	if status == http.StatusInternalServerError && r.Context().Err() == nil {
		s.log.Error("生成任务操作失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeMediaErr(w, r, status, code, msg)
}

// keyMediaReady 是每个处理器的前置：编排体已接线。
func (s *Server) keyMediaReady(w http.ResponseWriter, r *http.Request) bool {
	if s.mediaJobs == nil {
		writeMediaErr(w, r, http.StatusServiceUnavailable, "media_unavailable", "生成服务不可用")
		return false
	}
	return true
}

// ownedMediaJob 读一条任务并裁决归属：不是这把 Key 发起的与不存在一样答 404。
func (s *Server) ownedMediaJob(w http.ResponseWriter, r *http.Request) (*store.MediaJob, bool) {
	t, err := s.mediaJobs.Get(r.Context(), r.PathValue("id"))
	if err == nil && (t.KeyID == 0 || t.KeyID != infoFrom(r.Context()).keyID) {
		err = mediagen.ErrNotFound
	}
	if err != nil {
		s.mediaServiceError(w, r, err)
		return nil, false
	}
	return t, true
}

func (s *Server) handleKeyMediaJobs(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	tasks, err := s.store.ListPageMediaJobsByKey(r.Context(), infoFrom(r.Context()).keyID)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	if tasks == nil {
		tasks = []store.MediaJob{}
	}
	writeMediaJSON(w, r, http.StatusOK, map[string]any{"jobs": tasks})
}

// handleKeyMediaModels 回这把 Key 的能力表与可用性（页面表单与 `gate media models` 都读它）。
func (s *Server) handleKeyMediaModels(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	models, err := s.mediaJobs.Available(r.Context(), infoFrom(r.Context()).keyID)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, map[string]any{"running_per_key": mediagen.RunningPerKey, "models": models})
}

// mediaOrigin 判这次提交的来源：gate 的请求带 X-LLMGate-Client: gate/<版本>，其余是页面。
func mediaOrigin(r *http.Request) string {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("X-LLMGate-Client")), "gate") {
		return store.MediaOriginCLI
	}
	return store.MediaOriginPage
}

// handleKeyCreateMediaJob 受理一次生成：内核校验（可用性、输入与参数）→ 并发闸 → 准入闸与
// 落库；立刻以 202 回同批的 running 任务。
func (s *Server) handleKeyCreateMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	sub, err := mediagen.DecodeSubmission(w, r)
	if err != nil {
		writeMediaErr(w, r, http.StatusBadRequest, "invalid_request", "请求格式不正确")
		return
	}
	// 归属恒按认证身份覆盖：请求体里的 key_id 是管理面那条路的字段，Key 持有人不能借它把
	// 任务与用量记到别人头上。
	info := infoFrom(r.Context())
	sub.KeyID, sub.KeyDisplay, sub.Origin = info.keyID, info.bill.keyDisplay, mediaOrigin(r)
	prepared, err := s.mediaJobs.Check(r.Context(), sub)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	jobs, err := s.mediaJobs.Submit(r.Context(), prepared)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusAccepted, map[string]any{"batch_id": jobs[0].BatchID, "jobs": jobs})
}

func (s *Server) handleKeyGetMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	if t, ok := s.ownedMediaJob(w, r); ok {
		writeMediaJSON(w, r, http.StatusOK, *t)
	}
}

func (s *Server) handleKeyWaitMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	if _, ok := s.ownedMediaJob(w, r); !ok {
		return
	}
	t, err := s.mediaJobs.Wait(r.Context(), r.PathValue("id"))
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, *t)
}

func (s *Server) handleKeyRefreshMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	t, ok := s.ownedMediaJob(w, r)
	if !ok {
		return
	}
	next, err := s.mediaJobs.Refresh(r.Context(), *t)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, *next)
}

func (s *Server) handleKeyDeleteMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	t, ok := s.ownedMediaJob(w, r)
	if !ok {
		return
	}
	if err := s.mediaJobs.Delete(r.Context(), t.ID); err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, map[string]any{"deleted": 1})
}

func (s *Server) handleKeyClearMediaJobs(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	n, err := s.mediaJobs.ClearKey(r.Context(), infoFrom(r.Context()).keyID)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, map[string]any{"deleted": n})
}

// handleKeyDownloadMediaJob 以附件下载结果；handleKeyMediaMediaJob 以内联方式
// 提供同一份结果（页面凭 Key fetch 成 Blob 再显示）。
func (s *Server) handleKeyDownloadMediaJob(w http.ResponseWriter, r *http.Request) {
	s.serveKeyMediaMedia(w, r, "attachment")
}

func (s *Server) handleKeyMediaMediaJob(w http.ResponseWriter, r *http.Request) {
	s.serveKeyMediaMedia(w, r, "inline")
}

// handleKeyThumbMediaJob 内联提供结果缩略图（页面凭 Key fetch 成 Blob 再显示）；
// handleKeyUploadMediaThumb 收持有人页面回传的封面帧 {"image": "<base64>"}，
// 设备重新解码缩放后保存，回更新后的任务行。归属裁决与其他端点相同。
func (s *Server) handleKeyThumbMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	t, ok := s.ownedMediaJob(w, r)
	if !ok {
		return
	}
	m, err := s.mediaJobs.OpenThumb(t)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	defer m.Close()
	mediagen.ServeMedia(w, r, m, "inline")
}

func (s *Server) handleKeyUploadMediaThumb(w http.ResponseWriter, r *http.Request) {
	if !s.keyMediaReady(w, r) {
		return
	}
	t, ok := s.ownedMediaJob(w, r)
	if !ok {
		return
	}
	var in struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&in); err != nil || in.Image == "" {
		writeMediaErr(w, r, http.StatusBadRequest, "invalid_request", "请求格式不正确")
		return
	}
	data, err := base64.StdEncoding.DecodeString(in.Image)
	if err != nil {
		writeMediaErr(w, r, http.StatusBadRequest, "invalid_request", "请求格式不正确")
		return
	}
	next, err := s.mediaJobs.SaveThumb(r.Context(), t.ID, data)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	writeMediaJSON(w, r, http.StatusOK, *next)
}

func (s *Server) serveKeyMediaMedia(w http.ResponseWriter, r *http.Request, disposition string) {
	if !s.keyMediaReady(w, r) {
		return
	}
	t, ok := s.ownedMediaJob(w, r)
	if !ok {
		return
	}
	m, err := s.mediaJobs.OpenMedia(r.Context(), t)
	if err != nil {
		s.mediaServiceError(w, r, err)
		return
	}
	defer m.Close()
	mediagen.ServeMedia(w, r, m, disposition)
}
