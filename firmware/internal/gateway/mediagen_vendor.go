// mediagen_vendor.go 是媒体生成的厂商面后端：ark_image（方舟 Seedream，同步出图）、
// ark_video（方舟 Seedance）与 minimax_video（MiniMax H3），后两者由厂商先受理再查询。
//
// 三个后端都复用厂商面数据面那一跳：请求体由后端按厂商官方形状拼装，之后的选路
// （resolveRoute / resolveVideoRoute，含这把 Key 的可用模型范围与模型 / 来源 / 上游三层启停）、
// 故障切换、受理后钉死上游账户（aigc_tasks 行）、任务观察与清算、计量口径与外部客户端走
// /ark、/minimax 时是同一份代码——区别只在调用方是设备自己，响应收进内存而不是回给客户端。
// 准入在内核受理时已经过了，这里从 submitAdmittedAIGCTask / forwardArkImage 进，不重复过闸。
//
// §15.1：厂商错误正文只进任务行的失败原因，不进日志；产物 URL 只交给内核取回。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// mediaKeyRequest 在 mediaRequest 之上补齐这把 Key 的限额快照：厂商面的选路按 reqInfo 里的
// 身份判可用模型范围，任务行与账本也按它记归属。
func (s *Server) mediaKeyRequest(ctx context.Context, method, path string, in mediagen.Request) *http.Request {
	r := mediaRequest(ctx, method, path, in)
	if ka, err := s.store.LookupKeyAuthByID(ctx, in.KeyID); err == nil {
		infoFrom(r.Context()).limits = *ka
	}
	return r
}

// ---- ark_image ----

type arkImageMediaBackend struct{ s *Server }

func (arkImageMediaBackend) Name() string                    { return mediagen.BackendArkImage }
func (arkImageMediaBackend) Validate(mediagen.Request) error { return nil }

// Generate 按 Seedream 官方形状拼请求体（prompt、image 单值 / 数组、size、watermark、seed），
// 经图像入口同一段选路转发同步出图。
func (b arkImageMediaBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	s := b.s
	payload := map[string]any{"model": in.Model.ID, "prompt": in.Prompt, "response_format": "url"}
	switch refs := in.Inputs.ReferenceImages; len(refs) {
	case 0:
	case 1:
		payload["image"] = refs[0]
	default:
		payload["image"] = refs
	}
	if v := in.Params.String("size"); v != "" {
		payload["size"] = v
	}
	if v := in.Params.Bool("watermark"); v != nil {
		payload["watermark"] = *v
	}
	if n, ok := in.Params.Int("seed"); ok {
		payload["seed"] = n
	}
	r := s.mediaKeyRequest(ctx, http.MethodPost, "/ark/api/v3/images/generations", in)
	info := beginEntry(r, usage.EntryImage, in.Model.ID)
	rec := &bufferedResponse{header: http.Header{}}
	started := time.Now()
	defer func() { s.recordUsage(info, rec.status(), time.Since(started)) }()
	s.forwardArkImage(rec, r, info, payload, in.Model.ID)
	result := mediagen.Result{Status: store.MediaStatusSucceeded, MediaType: "image"}
	if rec.status() >= 300 {
		result.Status = store.MediaStatusFailed
		result.Error = s.mediaUpstreamFailure(r, rec, in, "平台图像生成失败")
		return result, nil
	}
	// 组图 / 单图的逐图失败挂在 data[].error、HTTP 仍 200。
	var v struct {
		Data []struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"data"`
	}
	if json.Unmarshal(rec.body.Bytes(), &v) == nil && len(v.Data) > 0 && v.Data[0].Error != nil {
		result.Status = store.MediaStatusFailed
		result.Error = formatMediaPlatformError(v.Data[0].Error.Code, v.Data[0].Error.Message, "平台图像生成失败")
		return result, nil
	}
	url, err := firstImageData(rec.body.Bytes())
	if err != nil {
		return result, err
	}
	result.MediaURL = url
	return result, nil
}

func (arkImageMediaBackend) Refresh(context.Context, store.MediaJob) (mediagen.Result, error) {
	return mediagen.Result{}, fmt.Errorf("该任务不支持刷新")
}

// ---- ark_video / minimax_video ----

// vendorVideoMediaBackend 是一家厂商视频协议面的生成后端。facePrefix 是该协议面在设备上的
// 路径首段（合成请求的路径只用来选网关自产错误的形状）。
type vendorVideoMediaBackend struct {
	s          *Server
	name       string
	adapter    videoAdapter
	facePrefix string
}

func (b vendorVideoMediaBackend) Name() string                  { return b.name }
func (vendorVideoMediaBackend) Validate(mediagen.Request) error { return nil }

// vendorVideoBody 按方舟 Seedance 与 MiniMax H3 共用的官方形状拼提交体：content[] 里一条 text，
// 首帧 / 尾帧 / 参考图各是带 role 的 image_url 元素；参数是顶层字段，键名即能力表里的参数名。
func vendorVideoBody(in mediagen.Request) map[string]any {
	content := []any{}
	if in.Prompt != "" {
		content = append(content, map[string]any{"type": "text", "text": in.Prompt})
	}
	image := func(url, role string) {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}, "role": role})
	}
	if in.Inputs.FirstFrame != "" {
		image(in.Inputs.FirstFrame, "first_frame")
	}
	if in.Inputs.LastFrame != "" {
		image(in.Inputs.LastFrame, "last_frame")
	}
	for _, ref := range in.Inputs.ReferenceImages {
		image(ref, "reference_image")
	}
	body := map[string]any{"model": in.Model.ID, "content": content}
	for k, v := range in.Params {
		// 整数参数折成 json.Number：适配器的 billingFacts 按数据面的解码形态（UseNumber）认
		// duration，给 int64 它就读不到、aigc_tasks 的计费特征列会落空。
		if n, ok := v.(int64); ok {
			v = json.Number(strconv.FormatInt(n, 10))
		}
		body[k] = v
	}
	return body
}

// Generate 走视频任务面同一段提交流程：选路、故障切换、受理后落 aigc_tasks 行（钉死上游账户、
// 固化计费特征）。受理即回 queued + 厂商任务 ID；钱在任务完成后按厂商 usage 清算。
func (b vendorVideoMediaBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	s := b.s
	r := s.mediaKeyRequest(ctx, http.MethodPost, b.facePrefix+"/", in)
	info := beginEntry(r, usage.EntryVideo, in.Model.ID)
	rec := &bufferedResponse{header: http.Header{}}
	started := time.Now()
	defer func() { s.recordUsage(info, rec.status(), time.Since(started)) }()
	s.submitAdmittedAIGCTask(rec, r, b.adapter, b.adapter.submitPath(), jsonSubmitBody{vendorVideoBody(in)}, in.Model.ID)
	result := mediagen.Result{MediaType: "video"}
	if rec.status() >= 300 {
		result.Status = store.MediaStatusFailed
		result.Error = s.mediaUpstreamFailure(r, rec, in, "平台视频生成失败")
		return result, nil
	}
	id, err := b.adapter.parseSubmitResponse(rec.body.Bytes())
	if err != nil {
		result.Status, result.Error = store.MediaStatusFailed, "平台未返回任务 ID"
		return result, nil
	}
	result.Status, result.VendorID = store.MediaStatusQueued, id
	return result, nil
}

// Refresh 走视频任务面同一段查询：按厂商任务 ID + 归属密钥定位 aigc_tasks 行，用它钉死的上游
// 账户查询，旁路观测与终态清算照常发生；查询不计模型消费。HTTP 层的拒绝按查询侧故障处理
// （下一轮再查），404 是厂商记录已不存在；任务层的 failed / cancelled / expired 才是任务的结局。
func (b vendorVideoMediaBackend) Refresh(ctx context.Context, t store.MediaJob) (mediagen.Result, error) {
	s := b.s
	if t.VendorID == "" {
		return mediagen.Result{}, fmt.Errorf("该任务不支持刷新")
	}
	in := mediagen.Request{KeyID: t.KeyID, KeyDisplay: t.KeyDisplay}
	r := mediaRequest(ctx, http.MethodGet, b.facePrefix+"/", in)
	r.SetPathValue("task_id", t.VendorID)
	rec := &bufferedResponse{header: http.Header{}}
	s.queryAIGCTask(rec, r, b.adapter.protocol())
	base := mediagen.Result{VendorID: t.VendorID, MediaType: "video"}
	if code := rec.status(); code >= 300 {
		s.log.Warn("查询平台生成任务被拒", "job", t.ID, "backend", t.Backend, "kind", t.Kind, "model", t.Model, "status", code)
		if code == http.StatusNotFound || code == http.StatusGone {
			base.Status = store.MediaStatusExpired
			base.Error = fmt.Sprintf("平台任务已过期或不可访问（HTTP %d）", code)
			return base, nil
		}
		return mediagen.Result{}, fmt.Errorf("平台查询暂不可用（HTTP %d）", code)
	}
	obs, err := b.adapter.parseTaskResponse(rec.body.Bytes())
	if err != nil {
		return mediagen.Result{}, fmt.Errorf("平台响应无法解析")
	}
	res := base
	res.Status = store.MediaStatusQueued
	switch strings.ToLower(obs.Status) {
	case aigcStatusSucceeded:
		if obs.VideoURL == "" {
			res.Status, res.Error = store.MediaStatusFailed, "平台未返回视频"
			break
		}
		res.Status, res.MediaURL = store.MediaStatusSucceeded, obs.VideoURL
	case aigcStatusFailed, aigcStatusCancelled:
		res.Status = store.MediaStatusFailed
		res.Error = formatMediaPlatformError(obs.ErrorCode, obs.ErrorMessage, "平台视频生成失败")
	case aigcStatusExpired:
		res.Status = store.MediaStatusExpired
		res.Error = formatMediaPlatformError(obs.ErrorCode, obs.ErrorMessage, "平台任务已过期")
	}
	return res, nil
}
