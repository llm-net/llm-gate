package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 「媒体生成」页的管理端点：内核在 internal/mediagen（与数据面凭 Key 自证的
// /gate-helper/v1/media、创作工作空间共用同一份 Service），这里只做会话之内的编码、归属
// 裁决与审计（并发闸在内核 Submit）。管理员看得到页面提交的全部任务（含各把 Key 发起的，任务行带
// key_display 快照）；创作工作空间与 gate media 的任务不进列表。
//
// 提交必须带一把 API 密钥（请求体 key_id）：这次生成以那把密钥的名义调用平台——哪些模型
// 可用按它裁决（订阅按开发工具策略钉死的账号，按量按可用模型范围），准入、并发上限与用量
// 也都算它的。管理员因此没有「不属于任何密钥」的调用，账本里也就没有对不上号的一笔。

// mediaEntitlements 按开发工具策略快照给出一把 Key 对各订阅的授权（mediagen.Entitlements）：
// 与数据面 /agents/<tool>/ 同一份判据，账号也钉同一行。
func (s *Server) mediaEntitlements(ctx context.Context, keyID int64) (map[string]mediagen.Entitlement, error) {
	snapshot, err := s.devToolResolver().Snapshot(ctx, keyID)
	if err != nil {
		return nil, err
	}
	out := map[string]mediagen.Entitlement{}
	for _, provider := range []string{store.AgentProviderGrok, store.AgentProviderCodex} {
		sub := snapshot.Subscription(provider)
		out[provider] = mediagen.Entitlement{Configured: sub.Configured, Available: sub.Available, AccountID: sub.AccountID}
	}
	return out, nil
}

// handleMediaModels 回能力表；带 key_id 时逐模型给出那把密钥的可用性（页面表单据此渲染）。
func (s *Server) handleMediaModels(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"running_per_key": mediagen.RunningPerKey}
	if raw := r.URL.Query().Get("key_id"); raw != "" {
		keyID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || keyID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "请求格式不正确")
			return
		}
		k, ok := s.mediaKey(w, r, keyID)
		if !ok {
			return
		}
		models, err := s.media.Available(r.Context(), k.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out["models"] = models
		writeJSON(w, http.StatusOK, out)
		return
	}
	catalog, err := s.media.Catalog(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	models := make([]mediagen.Availability, 0, len(catalog))
	for _, m := range catalog {
		models = append(models, mediagen.Availability{Model: m})
	}
	out["models"] = models
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMediaJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListPageMediaJobs(r.Context())
	if err != nil {
		writeError(w, 500, "media_unavailable", "读取生成任务失败")
		return
	}
	if jobs == nil {
		jobs = []store.MediaJob{}
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetMediaJob(w http.ResponseWriter, r *http.Request) {
	t, err := s.media.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, 200, *t)
}

// handleCreateMediaJob 只做校验与落库，立刻以 202 返回同批的 running 任务；真正的平台请求在
// 后台跑，页面按任务陪等。
func (s *Server) handleCreateMediaJob(w http.ResponseWriter, r *http.Request) {
	sub, err := mediagen.DecodeSubmission(w, r)
	if err != nil {
		writeError(w, 400, "invalid_request", "请求格式不正确")
		return
	}
	if sub.KeyID <= 0 {
		writeError(w, http.StatusBadRequest, "media_key_required", "请先选择发起本次生成的 API 密钥")
		return
	}
	k, ok := s.mediaKey(w, r, sub.KeyID)
	if !ok {
		return
	}
	sub.KeyDisplay, sub.Origin = store.KeyDisplay(k.DisplayPrefix, k.DisplayLast4), store.MediaOriginPage
	prepared, err := s.media.Check(r.Context(), sub)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	jobs, err := s.media.Submit(r.Context(), prepared)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"batch_id": jobs[0].BatchID, "jobs": jobs})
}

// mediaKey 点查提交里选定的 API 密钥：必须存在、未归档、未停用。不合格时响应已写出。
func (s *Server) mediaKey(w http.ResponseWriter, r *http.Request, keyID int64) (*store.APIKey, bool) {
	k, err := s.st.GetAPIKeyByID(r.Context(), keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
			return nil, false
		}
		s.internalError(w, r, err)
		return nil, false
	}
	if k.Archived() {
		writeError(w, http.StatusConflict, "key_archived", "这把 API 密钥已归档")
		return nil, false
	}
	if k.Disabled {
		writeError(w, http.StatusConflict, "key_disabled", "这把 API 密钥已停用")
		return nil, false
	}
	return k, true
}

// handleWaitMediaJob 陪等一个任务离开 running / queued：任务一落终态或陪等上限到了就回当前
// 任务行；仍未到终态时页面继续下一轮陪等。
func (s *Server) handleWaitMediaJob(w http.ResponseWriter, r *http.Request) {
	t, err := s.media.Wait(r.Context(), r.PathValue("id"))
	if err != nil {
		if r.Context().Err() != nil {
			return // 管理员关了页面：生成不中断，结果落任务行。
		}
		writeMediaError(w, err)
		return
	}
	writeJSON(w, 200, *t)
}

func (s *Server) handleRefreshMediaJob(w http.ResponseWriter, r *http.Request) {
	t, err := s.media.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMediaError(w, err)
		return
	}
	next, err := s.media.Refresh(r.Context(), *t)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, 200, *next)
}

func (s *Server) handleDeleteMediaJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.media.Delete(r.Context(), id); err != nil {
		writeMediaError(w, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: "media.job.delete", Entity: "media:" + id, RemoteIP: remoteIP(r)})
	writeJSON(w, 200, map[string]any{"deleted": 1})
}

// handleClearMediaJobs 清空页面提交的全部任务记录，回删除条数。
func (s *Server) handleClearMediaJobs(w http.ResponseWriter, r *http.Request) {
	n, err := s.media.Clear(r.Context())
	if err != nil {
		writeError(w, 500, "media_delete_failed", "清空生成任务失败")
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: "media.jobs.clear", Entity: "media:*", Detail: fmt.Sprintf("count=%d", n), RemoteIP: remoteIP(r)})
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// handleDownloadMediaJob 以附件下载结果；handleMediaJobMedia 以内联方式提供同一份结果给页面的
// <img> / <video>（本地文件走 http.ServeContent，视频可拖动）。
func (s *Server) handleDownloadMediaJob(w http.ResponseWriter, r *http.Request) {
	s.serveMediaJob(w, r, "attachment")
}

func (s *Server) handleMediaJobMedia(w http.ResponseWriter, r *http.Request) {
	s.serveMediaJob(w, r, "inline")
}

// handleMediaJobThumb 内联提供结果缩略图（短边 480 的 JPEG）：列表与任务信息只画它；
// handleUploadMediaJobThumb 收页面回传的封面帧（视频第一帧，或设备解不开的图像格式），体为
// {"image": "<base64>"}，设备重新解码缩放后保存，回更新后的任务行。
func (s *Server) handleMediaJobThumb(w http.ResponseWriter, r *http.Request) {
	t, err := s.media.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "media_not_found", "媒体不存在或已过期")
		return
	}
	m, err := s.media.OpenThumb(t)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	defer m.Close()
	mediagen.ServeMedia(w, r, m, "inline")
}

func (s *Server) handleUploadMediaJobThumb(w http.ResponseWriter, r *http.Request) {
	data, ok := decodeThumbUpload(w, r)
	if !ok {
		writeError(w, 400, "invalid_request", "请求格式不正确")
		return
	}
	t, err := s.media.SaveThumb(r.Context(), r.PathValue("id"), data)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, 200, *t)
}

func (s *Server) serveMediaJob(w http.ResponseWriter, r *http.Request, disposition string) {
	t, err := s.media.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "media_not_found", "媒体不存在或已过期")
		return
	}
	m, err := s.media.OpenMedia(r.Context(), t)
	if err != nil {
		writeMediaError(w, err)
		return
	}
	defer m.Close()
	mediagen.ServeMedia(w, r, m, disposition)
}

// writeMediaError 把内核的错误翻成管理面统一错误体（mediagen.HTTPError，与 Key 端点同一张表）。
func writeMediaError(w http.ResponseWriter, err error) {
	status, code, msg, retryAfter, ok := mediagen.HTTPError(err)
	if !ok {
		return
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeError(w, status, code, msg)
}

// decodeThumbUpload 解析封面帧回传体 {"image": "<base64>"}（体上限 12 MiB：8 MiB 图像的
// base64），回解码后的字节。
func decodeThumbUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var in struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&in); err != nil || in.Image == "" {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(in.Image)
	if err != nil {
		return nil, false
	}
	return data, true
}
