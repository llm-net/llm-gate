// video.go 实现视频任务面：每个视频模型固定一种 API 格式——**源头厂商的官方
// 接口**——设备只做转发，不改路径语义、不改请求/响应形态、不签发自己的任务
// id。首个接入的协议面是 MiniMax 视频（minimax_video，适配器在 video_minimax.go）：
//
//	POST   /minimax/v2/video_generation             创建视频生成任务
//	POST   /minimax/v2/h3_context_ir                创建 Context-IR 任务（提示词增强）
//	GET    /minimax/v2/query/video_generation/{id}  查询任务
//	DELETE /minimax/v2/video_generation/{id}        取消 / 删除任务
//
// 首段 `/minimax` 是**厂商协议面的路径段**（config.ProtocolFaceMinimax，路由块
// 在 server.go）：设备只有一个主机名，分不出子域名，厂商面只能落在路径首段
// 上。段之后仍是厂商站点根之后的原样那一段，设备→厂商那一跳不带这个前缀
// （适配器的 submitPath / queryPath / cancelPath 都是上游路径）。
//
// 转发要点（docs-upstream/minimax-h3-video.md 是路径与形态的契约出处）：
//
//   - **任务 id 就是厂商 task_id**：受理响应原样回给客户端，aigc_tasks 行把
//     厂商 id 映射回受理它的上游账户（多上游钉死）与归属密钥；行的 agt- id
//     降为纯内部主键。仍然隐藏的是上游**账户**：有几个账户、哪个账户服务了
//     本次请求、来源侧模型 ID（查询响应的 model 回显按 chat 口径改写回
//     客户端可见名）。
//   - **响应逐字节透传**：受理/查询/取消的厂商响应（含错误）原样到达客户端，
//     设备只旁路解析一份副本做观测（状态/usage/产物 URL 进任务行、终态就地
//     清算）；解析不动的响应照样透传，观测绝不影响转发。厂商 401/5xx 也原样
//     透传——文本面「非 429 4xx 原样提交」的既有口径，旧信封面把它们拍成
//     502 的遮蔽逻辑随信封一起删除。
//   - **网关自产错误按 MiniMax v2 形态**（{"type":"error","error":{...}}），
//     由 entryErrorStyle 按 /minimax/ 厂商段前缀选择（middleware.go）。
//   - **故障切换只存在于提交阶段**（复用 shouldFailover 口径，未受理前可换
//     来源）；受理后任务钉死上游，查询/取消一律走任务行所钉的账户。
//   - **多上游同格式**：一个视频模型的全部来源必须同一协议面——管理 API 在
//     来源写入时校验（internal/admin 的 source_family_mismatch），选路这里
//     按入口协议逐候选过滤只是兜底。
//   - 刻意**不挂载**的厂商端点：任务列表 GET /minimax/v2/query/video_generation
//     （厂商侧是全账户视角，转发会把别的用户的任务泄给同设备的任何 Key）、
//     POST /minimax/v1/files/upload（素材 file_id 是账户内状态，与多上游故障
//     切换互斥；素材走公网 URL 或 data URI）、POST /minimax/v2/video_regeneration
//     （再生成的单价档目录价词汇表还没有，转发了也只能记错账）。未挂载路径
//     统一 404（minimax 形态）。
//
// usage_json 存厂商 usage **原文**（秒/张形或 Context-IR 的 token 形），
// iteration-9 的计价按 kind+上游类型解读；观测到什么存什么，不折算不归一。
//
// §15.1：本文件绝不把请求/响应 body、厂商错误信息或产物 URL 传入 logger
// （产物 URL 的查询串是签名凭证）；日志只记 id/状态码/上游名。
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	// 任务行状态词汇（**内部记账用**，客户端看到的恒是厂商响应原文）：
	// minimax /v2 的词汇 queued/running/succeeded/failed/cancelled 直通入库；
	// expired 只由「厂商记录已查不到」的设备侧归一产生（驱动 0 元清算与对账
	// 停查）。词汇表之外的厂商状态原样入库并视同未终结。
	aigcStatusQueued = "queued"
	// aigcStatusRunning 是「在跑」的内部词汇：minimax/方舟直通同名状态。
	aigcStatusRunning   = "running"
	aigcStatusSucceeded = "succeeded"
	aigcStatusFailed    = "failed"
	aigcStatusCancelled = "cancelled"
	aigcStatusExpired   = "expired"

	// vendorJSONLimit 是设备旁路解析厂商 JSON 响应的字节上限：受理响应只有
	// 一个 id，任务响应含 usage 与产物 URL，1 MiB 绰绰有余。超限的 2xx 响应
	// 照样透传（前缀 + 余量拼接），只是跳过观测。
	vendorJSONLimit = 1 << 20

	// aigcErrorMessageLimit 是任务级错误信息入库的截断长度（rune 计）。
	aigcErrorMessageLimit = 500

	// 任务行保留期与清理节奏（节奏对齐 auth.RunSessionCleanup 的每小时）。
	// 14 天 = 厂商 7 天记录窗口的两倍：行只是路由锚点与账单事实，厂商侧记录
	// 消失后再留一倍时间供排障。
	aigcTaskRetention  = 14 * 24 * time.Hour
	aigcTaskPruneEvery = time.Hour

	// vendorTaskCallTimeout 是设备→厂商任务操作（查询/取消/提交后读体）单轮
	// 调用的整体上限：小 JSON 调用，30s 已然宽裕。没有它，厂商发完响应头就
	// 卡住时，读体只受共享客户端 overall（缺省 10m）约束。
	vendorTaskCallTimeout = 30 * time.Second
)

// aigcTerminal 判定内部状态是否终结：终态行不再进懒对账的回查（结果与 usage
// 已固化）。
func aigcTerminal(status string) bool {
	switch status {
	case aigcStatusSucceeded, aigcStatusFailed, aigcStatusCancelled, aigcStatusExpired:
		return true
	}
	return false
}

// ---- 适配器接口 ----

// taskObservation 是适配器从一次厂商回查里提炼的内部观测（写库前经
// observeVideoTask 与既有行合并）。它只喂任务行与清算，不参与客户端响应——
// 客户端拿到的是厂商响应原文。
type taskObservation struct {
	Status       string
	ErrorCode    string
	ErrorMessage string
	UsageJSON    string // 厂商 usage 原文 JSON（不改写不换算，iteration-9 计价只读它）
	VideoURL     string // 厂商产物 URL（时效副本：仅供排障，不参与任何响应）
	LastFrameURL string
}

// aigcBillingFacts 是提交时即固化的计费特征（aigc_tasks 的特征列：Seedance
// 时代按「是否含视频输入」定档的先例保留——minimax 参考视频按输入秒数计费，
// 判定输入提交时就有，此刻不存以后就得猜）。
type aigcBillingFacts struct {
	hasVideoInput bool
	generateAudio bool
	serviceTier   string
	reqResolution string
	reqDuration   string
}

// videoAdapter 是一家厂商视频协议面的适配器：官方路径、受理/任务响应的旁路
// 解析、提交时的计费特征提取、查询响应的 model 回显改写。适配器只做形态
// 认知不发请求——HTTP 往返统一在本文件。厂商的两层错误（HTTP 层 + 任务级
// status/error）分别由调用方的状态码分流与 parseTaskResponse 的观测承载。
type videoAdapter interface {
	// protocol 返回内置端点表里本厂商视频协议面的协议标识。
	protocol() string
	// submitPath/queryPath/cancelPath 是相对端点根的官方请求路径（提交侧的
	// 变体端点——如 Context-IR——由路由层显式传给 submitAIGCTask）。
	submitPath() string
	queryPath(vendorTaskID string) string
	cancelPath(vendorTaskID string) string
	// parseSubmitResponse 从受理（2xx）响应体取厂商任务 id。
	parseSubmitResponse(body []byte) (vendorTaskID string, err error)
	// parseTaskResponse 把查询（2xx）响应体映射为内部观测。
	parseTaskResponse(body []byte) (taskObservation, error)
	// parseCancelResponse 从取消（2xx）响应体判定任务是否被本次操作取消
	// （厂商语义 queued → cancelled；终态删记录不算）。
	parseCancelResponse(body []byte) (cancelled bool)
	// rewriteTaskModel 把查询响应里回显的来源侧模型 ID 改写回客户端可见名
	// （chat 侧 model 回写的同一口径：来源侧差异属于上游账户，不外显）。
	// payload 是已解出的响应对象，返回是否发生改写。
	rewriteTaskModel(payload map[string]any, model string) bool
	// billingFacts 从客户端请求体提取计费特征。
	billingFacts(payload map[string]any) aigcBillingFacts
}

// videoAdapters 是上游 type → 视频适配器注册表；有条目的 type 才能进视频
// 入口的候选。与 upstream 的内置端点表联动：注册的协议必须在端点表里有
// 条目，resolveVideoRoute 对两表都过滤。两个协议面：MiniMax 视频（/minimax）
// 与火山方舟 视频（/ark，Seedance）——**方舟按量与套餐共用同一个适配器**
// （同一套官方路径与报文，只是端点根不同），所以同一个视频模型可以同时挂套餐
// 与按量两条来源，按优先级套餐先行、按量兜底，同面校验（source_family_mismatch）
// 天然放行。
var videoAdapters = map[string]videoAdapter{
	config.UpstreamMinimax: minimaxVideoAdapter{},
	config.UpstreamArk:     arkVideoAdapter{},
	config.UpstreamArkPlan: arkVideoAdapter{},
}

// ---- 提交体 ----

// aigcSubmitBody 抽象「可按候选来源重新编码的提交体」。当前两个厂商面都是
// JSON（jsonSubmitBody）；接口保留 contentType，给 multipart 之类重编码后要换
// Content-Type 的形态用。故障切换要给每个候选重新编码一份带该来源模型 ID 的
// 体，所以编码是按候选调用的，不是一次性的。
type aigcSubmitBody interface {
	// encodeFor 把 model 改写为来源侧 ID 后重新编码整个提交体。
	encodeFor(upstreamModelID string) ([]byte, error)
	// contentType 是重编码后体的 Content-Type；空串表示沿用客户端原值
	// （JSON 形态重编码后仍是同一个 media type，不必覆盖）。
	contentType() string
	// fields 是给 videoAdapter.billingFacts 看的字段视图（提交时固化计费
	// 特征用）；JSON 形态就是解析出的 map。
	fields() map[string]any
}

// jsonSubmitBody 是 JSON 形提交体：通用 map 保留未知字段，按候选重序列化。
type jsonSubmitBody struct{ payload map[string]any }

func (b jsonSubmitBody) encodeFor(upstreamModelID string) ([]byte, error) {
	b.payload["model"] = upstreamModelID
	return encodeJSON(b.payload)
}

func (jsonSubmitBody) contentType() string      { return "" }
func (b jsonSubmitBody) fields() map[string]any { return b.payload }

// decodeJSONSubmitBody 读取 JSON 形提交体（体上限与错误契约见 entry.go）。
// 失败时已写出错误响应。
func decodeJSONSubmitBody(w http.ResponseWriter, r *http.Request) (aigcSubmitBody, string, bool) {
	// 视频体可合法地大到几十 MB（base64 素材，minimax 自身上限 64 MB），
	// 在读取侧硬拦（超限 413）；解码直读 body 流式进行（entry.go）。
	r.Body = http.MaxBytesReader(w, r.Body, videoSubmitBodyLimit)
	payload, model, ok := decodeEntryPayload(w, r, entryErrorStyle(r))
	if !ok {
		return nil, "", false
	}
	return jsonSubmitBody{payload}, model, true
}

// ---- 选路 ----

// videoCandidate 是视频入口的候选来源。upstreamID 随行进任务行，作为来源
// 锁定的钉子。
type videoCandidate struct {
	candidate
	upstreamID int64
	protocol   string
	adapter    videoAdapter
}

// resolveVideoRoute 按 kind=video 选路，且只收服务 protocol 所指协议面的
// 来源。kind 闸门与三层启停同 resolveRoute；「模型在但它是别的协议面」与
// 「没有可用来源」同归 model_not_found——一个模型固定一种 API 格式，走错
// 厂商面的客户端该得到的就是「此处无此模型」，不解释内部原因。
func (s *Server) resolveVideoRoute(ctx context.Context, model, protocol string) ([]videoCandidate, routeStatus) {
	route, status := s.fetchModelRoute(ctx, model)
	if status != routeOK {
		return nil, status
	}
	if route.Model.Kind != store.ModelKindVideo || route.Model.Disabled {
		return nil, routeModelNotFound
	}
	// API模型策略同 resolveRoute：范围外的模型按 model_not_found 处置。
	if ok, status := s.keyAPIModelAllowed(ctx, route.Model.ID); !ok {
		return nil, status
	}
	var cands []videoCandidate
	for _, c := range route.Candidates {
		if c.SourceDisabled || c.Upstream.Disabled {
			continue
		}
		ad, ok := videoAdapters[c.Upstream.Type]
		if !ok || ad.protocol() != protocol {
			continue
		}
		acct := upstream.Account{
			Name:       c.Upstream.Name,
			Type:       c.Upstream.Type,
			APIKey:     c.Upstream.APIKey,
			BaseURL:    c.Upstream.BaseURL,
			EgressMode: c.Upstream.EgressMode,
		}
		if _, ok := acct.Endpoint(ad.protocol()); !ok { // 适配器与端点表联动，防御分支
			continue
		}
		cands = append(cands, videoCandidate{
			candidate:  candidate{account: acct, modelID: c.UpstreamModelID, sourceID: c.SourceID},
			upstreamID: c.Upstream.ID,
			protocol:   ad.protocol(),
			adapter:    ad,
		})
	}
	if len(cands) == 0 {
		return nil, routeModelNotFound
	}
	return cands, routeOK
}

// ---- 辅助 ----

// newAIGCTaskID 签发任务行的内部主键：agt- 前缀 + 16 字节 crypto/rand 十六
// 进制。纯内部标识（用量环的清算行以它标识任务），不进任何客户端响应。
func newAIGCTaskID() string {
	var b [16]byte
	rand.Read(b[:]) // 自 Go 1.24 起 crypto/rand.Read 保证不失败
	return "agt-" + hex.EncodeToString(b[:])
}

// contentHasVideoInput 判定请求体 content[] 是否含视频输入（type=video_url
// 元素）——minimax 的参考视频按「输入秒数 × 输出档单价」计费，是提交时就得
// 固化的计费特征。
func contentHasVideoInput(payload map[string]any) bool {
	content, ok := payload["content"].([]any)
	if !ok {
		return false
	}
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if t, _ := m["type"].(string); t == "video_url" {
				return true
			}
		}
	}
	return false
}

// relayVendorBytes 把已整读的厂商响应原样回给客户端：状态码、业务头（剥逐跳
// 头）、字节。体可能经 model 回写，Content-Length 以实际字节为准。
func relayVendorBytes(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyBackHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// prefixedBody 把已读前缀拼回未读余量，充当响应体继续透传（超过解析上限的
// 2xx 走这条路）；Close 只关原响应体。
type prefixedBody struct {
	io.Reader
	io.Closer
}

// readVendorBody 有界整读一份要原样回写的厂商响应体（vendorJSONLimit）。
// ok=true：体已整读且在限内（resp.Body 已关），调用方旁路解析后经
// relayVendorBytes 回写。ok=false：响应已写出——超限时前缀拼回余量流式
// 透传（跳过旁路观测，账等下一次观测或懒对账再补），读失败时按上游读失败
// 处置。绝不把截断的体配上自洽的 Content-Length 回给客户端（字节保真）。
func (s *Server) readVendorBody(w http.ResponseWriter, r *http.Request, task *store.AIGCTask, resp *http.Response) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, vendorJSONLimit+1))
	if err != nil {
		resp.Body.Close()
		s.writeVendorTaskFailure(w, r, task, err)
		return nil, false
	}
	if len(body) > vendorJSONLimit {
		s.log.Warn("任务响应超过解析上限，跳过观测直接透传",
			"request_id", infoFrom(r.Context()).id, "task_id", task.ID)
		resp.Body = prefixedBody{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		s.passthroughResponse(w, r, resp, task.ModelName)
		return nil, false
	}
	resp.Body.Close()
	return body, true
}

// ---- 提交 ----

// handleMinimaxVideoSubmit 是 POST /v2/video_generation（已过认证中间件）。
func (s *Server) handleMinimaxVideoSubmit(w http.ResponseWriter, r *http.Request) {
	body, model, ok := decodeJSONSubmitBody(w, r)
	if !ok {
		return
	}
	s.submitAIGCTask(w, r, minimaxVideoAdapter{}, minimaxVideoAdapter{}.submitPath(), body, model)
}

// handleMinimaxContextIRSubmit 是 POST /v2/h3_context_ir：Context-IR（提示词
// 增强）与视频生成共用受理/查询/计费机制，只是提交路径不同、usage 是 token
// 形（iteration-9 按 prompt_tokens 在场识别并按 minimax_context_ir_* 计价）。
func (s *Server) handleMinimaxContextIRSubmit(w http.ResponseWriter, r *http.Request) {
	body, model, ok := decodeJSONSubmitBody(w, r)
	if !ok {
		return
	}
	s.submitAIGCTask(w, r, minimaxVideoAdapter{}, minimaxContextIRPath, body, model)
}

// handleArkVideoSubmit 是 POST /api/v3/contents/generations/tasks（方舟
// Seedance 官方创建路径）。与 minimax 族共用整条提交流程，只换适配器。
func (s *Server) handleArkVideoSubmit(w http.ResponseWriter, r *http.Request) {
	body, model, ok := decodeJSONSubmitBody(w, r)
	if !ok {
		return
	}
	s.submitAIGCTask(w, r, arkVideoAdapter{}, arkVideoAdapter{}.submitPath(), body, model)
}

// handleArkVideoQuery / handleArkVideoCancel 是方舟任务的查询与取消/删除
// （GET / DELETE /api/v3/contents/generations/tasks/{task_id}）。查询与取消
// 的实现与 minimax 族共用——任务行钉死了受理它的上游，适配器由那一行的上游
// 类型反查（pinnedVideoUpstream），路由这层只需告诉它「客户端是从哪一族的
// 路径来的」，好让跨族的任务 id 得到 404 而不是一份错形态的响应。
func (s *Server) handleArkVideoQuery(w http.ResponseWriter, r *http.Request) {
	s.queryAIGCTask(w, r, config.ProtocolArkVideo)
}

func (s *Server) handleArkVideoCancel(w http.ResponseWriter, r *http.Request) {
	s.cancelAIGCTask(w, r, config.ProtocolArkVideo)
}

// submitAIGCTask 是提交侧的公共流程：读体取 model → kind=video 按协议面选路
// → 逐候选提交（未受理前可故障切换）→ 厂商受理后落任务行 → 受理响应原样回
// 给客户端。厂商同步非 2xx（一切 4xx 与末候选的 429/5xx）原样透传。
func (s *Server) submitAIGCTask(w http.ResponseWriter, r *http.Request, ad videoAdapter, path string,
	body aigcSubmitBody, model string) {

	errStyle := entryErrorStyle(r)
	// 自此本请求要入账（iteration-9）：**提交这一笔只计一次请求**，金额 0——
	// 视频的钱要等厂商 usage 出来才知道，由查询路径的顺手清算或懒对账在
	// 任务完成后补记。
	beginEntry(r, usage.EntryVideo, model)
	// 预算准入（iteration-9）：只有**提交**这一条路过闸——查询/取消是「查已经
	// 花掉的钱」，预算再紧也不该把已提交的任务锁在设备里。
	if !s.admit(w, r, errStyle) {
		return
	}
	cands, status := s.resolveVideoRoute(r.Context(), model, ad.protocol())
	switch status {
	case routeModelNotFound:
		errStyle(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
		return
	case routeStoreError:
		errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}

	info := infoFrom(r.Context())
	if len(cands) == 0 { // resolveVideoRoute 保证 routeOK 时非空，防御分支
		s.log.Error("选路候选为空", "request_id", info.id, "model", model)
		errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	for i, c := range cands {
		last := i == len(cands)-1
		info.attempts, info.upstream = i+1, c.account.Name

		attemptCtx, cancel := context.WithCancel(r.Context())
		reqBody, err := body.encodeFor(c.modelID)
		if err != nil { // 体源自已解析成功的请求，理论不可达；防御分支
			cancel()
			s.log.Error("重序列化请求体失败", "request_id", info.id, "err", err.Error())
			errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
			return
		}
		target, ok := c.account.URL(c.protocol, path)
		if !ok { // resolveVideoRoute 已按协议过滤，防御分支
			cancel()
			s.log.Error("候选来源无视频协议端点", "request_id", info.id, "model", model, "source_id", c.sourceID)
			errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
			return
		}
		req, err := buildUpstreamRequest(attemptCtx, r, c.account, c.protocol, target, reqBody)
		if err != nil {
			cancel()
			s.log.Error("构造上游请求失败", "request_id", info.id, "err", err.Error())
			errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
			return
		}
		// 提交体重编码后要换 Content-Type 的形态在这里覆盖（copyForwardHeaders
		// 抄过来的客户端值会失效）；JSON 形态返回空串，保持原样。
		if ct := body.contentType(); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", model)
				return
			}
			if !last {
				s.log.Warn("视频任务提交连接失败，切换下一来源",
					"request_id", info.id, "model", model,
					"upstream", c.account.Name, "attempt", i+1, "err", err.Error())
				continue
			}
			s.log.Warn("视频任务提交失败", "request_id", info.id, "model", model,
				"upstream", c.account.Name, "attempt", i+1, "err", err.Error())
			errStyle(w, http.StatusBadGateway, "upstream_unreachable",
				"The gateway failed to reach the upstream provider.")
			return
		}
		if !last && shouldFailover(resp.StatusCode) {
			discardBody(resp, cancel)
			s.log.Warn("视频任务提交返回可切换状态码，切换下一来源",
				"request_id", info.id, "model", model,
				"upstream", c.account.Name, "attempt", i+1, "upstream_status", resp.StatusCode)
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			// 未受理（一切 4xx 与末候选的 429/5xx）：原样透传，不建任务行。
			defer cancel()
			s.passthroughResponse(w, r, resp, model)
			return
		}
		s.finishAIGCSubmit(w, r, c, model, body, resp, cancel)
		return
	}
}

// finishAIGCSubmit 处理厂商受理（2xx）后的收尾：读响应取厂商任务 id、落任务
// 行（含计费特征）、把受理响应原样回给客户端。进入本函数后一切失败都不再
// 切换来源——任务可能已在厂商创建，重试等于双重扣费。
func (s *Server) finishAIGCSubmit(w http.ResponseWriter, r *http.Request, c videoCandidate,
	model string, body aigcSubmitBody, resp *http.Response, cancel context.CancelFunc) {

	errStyle := entryErrorStyle(r)
	info := infoFrom(r.Context())
	vb, rerr := io.ReadAll(io.LimitReader(resp.Body, vendorJSONLimit))
	resp.Body.Close()
	cancel()
	if rerr != nil {
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", model)
			return
		}
		s.log.Error("读取视频任务受理响应失败", "request_id", info.id, "model", model,
			"upstream", c.account.Name, "err", rerr.Error())
		errStyle(w, http.StatusBadGateway, "upstream_protocol_error",
			"The upstream provider accepted the task but the gateway failed to read its response.")
		return
	}
	vendorID, perr := c.adapter.parseSubmitResponse(vb)
	if perr != nil {
		// 受理了却认不出任务 id：没有 id 就没有路由锚点，透传出去的是一个
		// 设备永远代理不了的任务——如实报协议错误。错误只记形态描述，不记
		// 响应内容（§15.1）。
		s.log.Error("解析视频任务受理响应失败", "request_id", info.id, "model", model,
			"upstream", c.account.Name, "err", perr.Error())
		errStyle(w, http.StatusBadGateway, "upstream_protocol_error",
			"The upstream provider accepted the task but the gateway could not identify it.")
		return
	}
	facts := c.adapter.billingFacts(body.fields())
	nt := store.NewAIGCTask{
		ID:           newAIGCTaskID(),
		VendorTaskID: vendorID,
		UpstreamID:   c.upstreamID,
		UpstreamName: c.account.Name,
		ModelName:    model,
		Kind:         store.ModelKindVideo,
		KeyID:        info.keyID,
		// 鉴权点查带回的展示串（与管理台、账本同一素材）：不再从原始 Key
		// 重新推导——store 侧 keyDisplay 的文档钉过这两处必须逐字相同。
		KeyDisplay:    info.bill.keyDisplay,
		Status:        aigcStatusQueued,
		HasVideoInput: facts.hasVideoInput,
		GenerateAudio: facts.generateAudio,
		ServiceTier:   facts.serviceTier,
		ReqResolution: facts.reqResolution,
		ReqDuration:   facts.reqDuration,
	}
	// 厂商已受理：行必须落库，客户端此刻断开也不例外（WithoutCancel）——
	// 否则任务成了既扣费又无主的孤儿。
	if _, err := s.store.CreateAIGCTask(context.WithoutCancel(r.Context()), nt); err != nil {
		// 行没落成，设备侧无从代理这个任务；厂商任务 id 记入日志供人工对账
		// （不透明任务句柄，不是 §15.1 的密钥物料）。
		s.log.Error("视频任务行落库失败", "request_id", info.id, "model", model,
			"upstream", c.account.Name, "vendor_task_id", vendorID, "err", err.Error())
		errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	relayVendorBytes(w, resp, vb)
}

// ---- 查询 / 取消 ----

// handleMinimaxVideoQuery 是 GET /v2/query/video_generation/{task_id}：按
// 厂商任务 id + 归属定位任务行 → 转发厂商查询 → 响应原样透传，旁路观测
// （状态/usage/产物 URL 进任务行，终态就地清算）。**终态行也照转**——厂商
// 每次查询都重签产物 URL，客户端按官方文档「链接失效就重查」时，设备必须
// 把新链接原样带回，而不是用行里的旧快照拼装答案。
func (s *Server) handleMinimaxVideoQuery(w http.ResponseWriter, r *http.Request) {
	s.queryAIGCTask(w, r, config.ProtocolMinimaxVideo)
}

// queryAIGCTask 是任务查询的公共实现；protocol 是**客户端所用路径**的接口
// 族，用于拒绝跨族的任务 id（见 pinnedVideoUpstream）。
func (s *Server) queryAIGCTask(w http.ResponseWriter, r *http.Request, protocol string) {
	task := s.aigcTaskForRequest(w, r)
	if task == nil {
		return
	}
	acct, ad, ok := s.pinnedVideoUpstream(w, r, task, protocol)
	if !ok {
		return
	}
	req, cancel, err := buildVendorTaskProxy(r, acct, ad.protocol(), ad.queryPath(task.VendorTaskID))
	if err != nil { // 端点在 pinnedVideoUpstream 已成立过，防御分支
		s.log.Error("构造任务查询请求失败", "request_id", infoFrom(r.Context()).id,
			"task_id", task.ID, "err", err.Error())
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	defer cancel()
	resp, err := s.client.Do(req)
	if err != nil {
		s.writeVendorTaskFailure(w, r, task, err)
		return
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// 厂商记录已不存在（超 7 天保留期或已删）：响应原样透传，内部把行
		// 归一为 expired 并清算（0 元 + 估算标——最终用量无从得知）。
		if !aigcTerminal(task.Status) {
			s.observeVideoTask(r, task, expiredObservation())
			s.settleTaskIfTerminal(r, task)
		}
		body, ok := s.readVendorBody(w, r, task, resp)
		if !ok {
			return
		}
		relayVendorBytes(w, resp, body)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		// 其余非 2xx（含厂商 401/429/5xx）：原样透传，不观测。那是厂商对这次
		// 操作的回答，设备无权改写；账单事实等下一次成功观测或懒对账。
		s.passthroughResponse(w, r, resp, task.ModelName)
	default:
		s.relayObservedTaskResponse(w, r, task, ad, resp)
	}
}

// relayObservedTaskResponse 消化查询 2xx：整读（有界）→ 旁路观测 → 终态就地
// 清算 → model 回显改写 → 原样透传。解析不动的响应照样透传（观测是旁路，
// 绝不影响转发）；超过解析上限的响应流式拼接透传并跳过观测。
func (s *Server) relayObservedTaskResponse(w http.ResponseWriter, r *http.Request, task *store.AIGCTask, ad videoAdapter, resp *http.Response) {
	info := infoFrom(r.Context())
	body, ok := s.readVendorBody(w, r, task, resp)
	if !ok {
		return
	}
	if obs, perr := ad.parseTaskResponse(body); perr == nil {
		s.observeVideoTask(r, task, obs)
		s.settleTaskIfTerminal(r, task)
	} else {
		// 解析失败只记形态描述（§15.1），响应照样透传——观测是旁路。
		s.log.Warn("任务查询响应无法旁路解析", "request_id", info.id,
			"task_id", task.ID, "upstream", task.UpstreamName, "err", perr.Error())
	}
	out := body
	if m, ok := decodeJSONObject(body); ok && ad.rewriteTaskModel(m, task.ModelName) {
		if nb, err := encodeJSON(m); err == nil {
			out = nb
		}
	}
	relayVendorBytes(w, resp, out)
}

// handleMinimaxVideoCancel 是 DELETE /v2/video_generation/{task_id}：语义直通
// 厂商（queued → 取消 / 终态 → 删厂商侧记录 / running → 厂商 4xx 拒绝），
// 响应原样透传。转发前对非终态行先做一次**设备自己的**回查（best-effort）：
// 终态任务的 DELETE 是删厂商记录，usage 若未先观测到就永远丢了（账单事实）；
// 回查失败不阻塞客户端的删除指令。本地任务行从不因 DELETE 删除。
func (s *Server) handleMinimaxVideoCancel(w http.ResponseWriter, r *http.Request) {
	s.cancelAIGCTask(w, r, config.ProtocolMinimaxVideo)
}

// cancelAIGCTask 是任务取消/删除的公共实现；protocol 同 queryAIGCTask。
func (s *Server) cancelAIGCTask(w http.ResponseWriter, r *http.Request, protocol string) {
	task := s.aigcTaskForRequest(w, r)
	if task == nil {
		return
	}
	acct, ad, ok := s.pinnedVideoUpstream(w, r, task, protocol)
	if !ok {
		return
	}
	info := infoFrom(r.Context())
	if !aigcTerminal(task.Status) {
		qctx, qcancel := context.WithTimeout(r.Context(), vendorTaskCallTimeout)
		res, qerr := s.queryVendorTask(qctx, acct, ad, task.VendorTaskID)
		if qerr == nil {
			switch {
			case res.gone:
				s.observeVideoTask(r, task, expiredObservation())
			case res.relay != nil:
				drainBody(res.relay) // 回查是设备自己的旁路，厂商拒绝就算了
			default:
				s.observeVideoTask(r, task, res.obs)
			}
			s.settleTaskIfTerminal(r, task)
		} else if r.Context().Err() != nil {
			qcancel()
			s.log.Info("客户端断开，任务回查已取消", "request_id", info.id, "task_id", task.ID)
			return
		}
		qcancel()
	}
	req, cancel, err := buildVendorTaskProxy(r, acct, ad.protocol(), ad.cancelPath(task.VendorTaskID))
	if err != nil { // 端点在 pinnedVideoUpstream 已成立过，防御分支
		s.log.Error("构造任务取消请求失败", "request_id", info.id, "task_id", task.ID, "err", err.Error())
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	defer cancel()
	resp, err := s.client.Do(req)
	if err != nil {
		s.writeVendorTaskFailure(w, r, task, err)
		return
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		if !aigcTerminal(task.Status) {
			s.observeVideoTask(r, task, expiredObservation())
			s.settleTaskIfTerminal(r, task)
		}
		body, ok := s.readVendorBody(w, r, task, resp)
		if !ok {
			return
		}
		relayVendorBytes(w, resp, body)
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		body, ok := s.readVendorBody(w, r, task, resp)
		if !ok {
			return
		}
		if !aigcTerminal(task.Status) && ad.parseCancelResponse(body) {
			// queued → cancelled；终态任务的 DELETE 只删厂商侧记录，本地行
			// 保持终态与账单事实不动。
			s.observeVideoTask(r, task, taskObservation{Status: aigcStatusCancelled})
			s.settleTaskIfTerminal(r, task)
		}
		relayVendorBytes(w, resp, body)
	default:
		// 厂商拒绝（如 running 不可取消的 4xx）：原样透传，不观测。
		s.passthroughResponse(w, r, resp, task.ModelName)
	}
}

// ---- 任务行与钉死上游 ----

// aigcTaskForRequest 按路径 {task_id}（厂商任务 id）与本次请求的鉴权身份取
// 任务行。**归属主语是签发这次请求的那把密钥**：归属不符与不存在同回 404
// （GetAIGCTaskByVendorIDForKey 已并同两种情况——查询/取消全压在这上面，
// 没有存在性 oracle）；行已过 14 天保留期同样 404，与厂商自己的 7 天窗口
// 口径一致（invalid task_id）。失败时已写出错误响应。
func (s *Server) aigcTaskForRequest(w http.ResponseWriter, r *http.Request) *store.AIGCTask {
	info := infoFrom(r.Context())
	id := r.PathValue("task_id")
	task, err := s.store.GetAIGCTaskByVendorIDForKey(r.Context(), id, info.keyID)
	if err == nil {
		return task
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		entryErrorStyle(r)(w, http.StatusNotFound, "invalid_task_id",
			fmt.Sprintf("Task %q does not exist or you do not have access to it.", id))
	case r.Context().Err() != nil:
		s.log.Info("客户端断开，任务点查已取消", "request_id", info.id)
	default:
		s.log.Error("任务点查失败", "request_id", info.id, "err", err.Error())
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
	}
	return nil
}

// pinnedVideoUpstream 取任务行钉死的上游（路由视图，解密凭证）与其适配器。
// 停用位有意放行（见 store.GetRouteUpstreamByID）：停用只挡新提交的选路，
// 已受理的任务必须始终可查可取消。失败时已写出错误响应。
//
// protocol 是客户端所用路径的协议面：**跨面的任务 id 一律 404
// invalid_task_id**，与「不存在」「不属于你」同一响应（沿用
// GetAIGCTaskByVendorIDForUser 的无 oracle 口径）。不这么挡的话，拿方舟的
// 任务 id 走 minimax 的查询路径会拿回一份方舟形态的响应——客户端按它请求的
// 那一族文档去解析，必然对不上；而「一个模型固定一种 API 格式」的前提也就
// 破了。同族内不区分按量/订阅：那是上游账户差异，本就不该外显。
func (s *Server) pinnedVideoUpstream(w http.ResponseWriter, r *http.Request, task *store.AIGCTask, protocol string) (upstream.Account, videoAdapter, bool) {
	info := infoFrom(r.Context())
	ru, err := s.store.GetRouteUpstreamByID(r.Context(), task.UpstreamID)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			s.log.Info("客户端断开，任务上游点查已取消", "request_id", info.id, "task_id", task.ID)
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrKeyUnreadable):
			// 上游行已删 / 凭证解不开（设备密钥换过）：设备侧无从代理，如实报
			// 网关↔厂商这段不可用；不向客户端解释上游账户层面的内部原因。
			s.log.Warn("任务钉死的上游不可用", "request_id", info.id, "task_id", task.ID,
				"upstream_id", task.UpstreamID, "err", err.Error())
			entryErrorStyle(r)(w, http.StatusBadGateway, "upstream_unavailable",
				"The gateway can no longer reach the provider that served this task.")
		default:
			s.log.Error("任务上游点查失败", "request_id", info.id, "task_id", task.ID, "err", err.Error())
			entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		}
		return upstream.Account{}, nil, false
	}
	ad, ok := videoAdapters[ru.Type]
	if !ok { // 行由适配器写入，类型必有适配器；防御分支
		s.log.Error("任务上游类型无适配器", "request_id", info.id, "task_id", task.ID, "type", ru.Type)
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return upstream.Account{}, nil, false
	}
	if ad.protocol() != protocol {
		// 跨族访问：与「任务不存在」同响应，不解释这个 id 在别的路径下活着。
		entryErrorStyle(r)(w, http.StatusNotFound, "invalid_task_id",
			fmt.Sprintf("Task %q does not exist or you do not have access to it.", task.VendorTaskID))
		return upstream.Account{}, nil, false
	}
	info.attempts, info.upstream = 1, ru.Name
	return upstream.Account{Name: ru.Name, Type: ru.Type, APIKey: ru.APIKey, BaseURL: ru.BaseURL, EgressMode: ru.EgressMode}, ad, true
}

// ---- 厂商回查（设备自取，供懒对账与取消前的账单保全） ----

// vendorTaskResult 是一次厂商任务回查的分类结果：obs 有效当且仅当 gone 与
// relay 都为零值；gone = 厂商 404（任务记录已不存在——超 7 天保留期或已删）；
// relay 携带其余非 2xx 响应（body 未读，由调用方消费并关闭）。
type vendorTaskResult struct {
	obs   taskObservation
	gone  bool
	relay *http.Response
}

// errVendorProtocol 标记「厂商 2xx 但响应无法解析」——与连不上分开映射
// （upstream_protocol_error vs upstream_unreachable）。
var errVendorProtocol = errors.New("上游任务响应无法解析")

// vendorTaskRequest 构造设备→厂商的任务操作请求：裸请求 + 上游凭证。不带
// 客户端头——这是设备自己的调用（懒对账、取消前回查），不是透传。
func vendorTaskRequest(ctx context.Context, method string, acct upstream.Account, protocol, path string) (*http.Request, error) {
	target, ok := acct.URL(protocol, path)
	if !ok {
		return nil, fmt.Errorf("上游类型 %s 不服务协议 %s", acct.Type, protocol)
	}
	req, err := http.NewRequestWithContext(acct.EgressContext(ctx), method, target, nil)
	if err != nil {
		return nil, err
	}
	acct.Authorize(req.Header, protocol)
	return req, nil
}

// buildVendorTaskProxy 装配客户端任务操作（查询/取消）的**透传**请求：方法与
// 客户端一致、无体，客户端业务头照透传规则复制（剥逐跳头与客户端凭证）、
// 注入上游凭证。整轮（含读厂商响应体）挂在 vendorTaskCallTimeout 的子 ctx 上。
func buildVendorTaskProxy(r *http.Request, acct upstream.Account, protocol, path string) (*http.Request, context.CancelFunc, error) {
	target, ok := acct.URL(protocol, path)
	if !ok {
		return nil, nil, fmt.Errorf("上游类型 %s 不服务协议 %s", acct.Type, protocol)
	}
	ctx, cancel := context.WithTimeout(acct.EgressContext(r.Context()), vendorTaskCallTimeout)
	req, err := http.NewRequestWithContext(ctx, r.Method, target, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	copyForwardHeaders(req.Header, r.Header)
	acct.Authorize(req.Header, protocol)
	return req, cancel, nil
}

// queryVendorTask 现查厂商任务并分类结果（HTTP 层错误在此分流；任务级
// status/error 在观测里，两层都判）。
func (s *Server) queryVendorTask(ctx context.Context, acct upstream.Account, ad videoAdapter, vendorTaskID string) (vendorTaskResult, error) {
	req, err := vendorTaskRequest(ctx, http.MethodGet, acct, ad.protocol(), ad.queryPath(vendorTaskID))
	if err != nil {
		return vendorTaskResult{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return vendorTaskResult{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		drainBody(resp)
		return vendorTaskResult{gone: true}, nil
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return vendorTaskResult{relay: resp}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, vendorJSONLimit))
	resp.Body.Close()
	if err != nil {
		return vendorTaskResult{}, err
	}
	obs, err := ad.parseTaskResponse(body)
	if err != nil {
		return vendorTaskResult{}, fmt.Errorf("%w: %v", errVendorProtocol, err)
	}
	return vendorTaskResult{obs: obs}, nil
}

// drainBody 有界读尽并关闭响应体（错误体读掉便于连接复用；内容不进日志）。
func drainBody(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, failoverDiscardLimit))
	resp.Body.Close()
}

// expiredObservation 是「厂商已查不到任务记录」的内部归一观测：厂商任务记录
// 只保留约 7 天（或已被删除），最终状态无从得知——行按 expired 记账（0 元 +
// 估算标）并停止回查。客户端看到的是厂商 404 响应原文，这份观测不外显。
func expiredObservation() taskObservation {
	return taskObservation{
		Status:    aigcStatusExpired,
		ErrorCode: "task_record_expired",
		ErrorMessage: "The upstream provider no longer holds this task's record " +
			"(task records are kept for about 7 days); its final status is unknown.",
	}
}

// observeVideoTask 把观测合并进内存行并写库。合并策略：status/error 取观测值
// （error 信息经 upstream.CleanMessage 消毒 + 截断——脏字节不落库）；usage
// 仅在观测非空时覆盖——厂商过期后不再回这些字段，旧值留作排障副本。产物 URL
// 还多一层判定：仅在指向**新资源**时覆盖（sameURLIgnoringQuery）——厂商每次
// 查询都重签 URL 的签名查询串，照单全收会让 store 的等值检测永远不命中、每次
// 回查都落一次 SD 卡写。写库失败只记日志：观测可从厂商再取，读路径不因写失败
// 而 500。
func (s *Server) observeVideoTask(r *http.Request, task *store.AIGCTask, obs taskObservation) {
	mergeTaskObservation(task, obs)
	// 客户端此刻断开也要把观测落库（usage 是账单事实，WithoutCancel）。
	if err := s.persistTaskObservation(context.WithoutCancel(r.Context()), task); err != nil {
		s.log.Warn("任务观测写库失败", "request_id", infoFrom(r.Context()).id,
			"task_id", task.ID, "err", err.Error())
	}
}

// mergeTaskObservation 把一次观测合并进内存行（合并策略见 observeVideoTask）。
// 与写库分开，好让请求路径之外的调用方（懒对账的探针，taskprober.go）复用同一
// 套口径——两处若各写一份，厂商换签名的 URL 迟早只在一边被过滤掉。
func mergeTaskObservation(task *store.AIGCTask, obs taskObservation) {
	task.Status = obs.Status
	task.ErrorCode = obs.ErrorCode
	task.ErrorMessage = upstream.CleanMessage(obs.ErrorMessage, aigcErrorMessageLimit)
	if obs.UsageJSON != "" {
		task.UsageJSON = obs.UsageJSON
	}
	if obs.VideoURL != "" && !sameURLIgnoringQuery(task.ContentURL, obs.VideoURL) {
		task.ContentURL = obs.VideoURL
	}
	if obs.LastFrameURL != "" && !sameURLIgnoringQuery(task.LastFrameURL, obs.LastFrameURL) {
		task.LastFrameURL = obs.LastFrameURL
	}
	task.UpdatedAt = time.Now()
}

// persistTaskObservation 把合并后的行写库（store 侧等值观测零写，不会白白
// 敲 SD 卡）。
func (s *Server) persistTaskObservation(ctx context.Context, task *store.AIGCTask) error {
	_, err := s.store.UpdateAIGCTaskObserved(ctx, task.ID, store.AIGCObservation{
		Status:       task.Status,
		ErrorCode:    task.ErrorCode,
		ErrorMessage: task.ErrorMessage,
		UsageJSON:    task.UsageJSON,
		ContentURL:   task.ContentURL,
		LastFrameURL: task.LastFrameURL,
	})
	return err
}

// sameURLIgnoringQuery 判定两个 URL 是否指向同一资源：scheme/host/path 相同、
// 只有查询串（签名参数）与片段不同。解析失败按「不同」处理——照常覆盖，
// 宁多写一次也不错存。
func sameURLIgnoringQuery(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host && ua.Path == ub.Path
}

// writeVendorTaskFailure 把任务透传的传输错误映射为客户端响应：客户端断开
// 静默返回；其余（连接/读取失败）→ 502 upstream_unreachable。
func (s *Server) writeVendorTaskFailure(w http.ResponseWriter, r *http.Request, task *store.AIGCTask, err error) {
	info := infoFrom(r.Context())
	if r.Context().Err() != nil {
		s.log.Info("客户端断开，任务转发已取消", "request_id", info.id, "task_id", task.ID)
		return
	}
	s.log.Warn("任务转发失败", "request_id", info.id, "task_id", task.ID,
		"upstream", task.UpstreamName, "err", err.Error())
	entryErrorStyle(r)(w, http.StatusBadGateway, "upstream_unreachable",
		"The gateway failed to reach the upstream provider.")
}

// ---- 保留期清理 ----

// RunAIGCTaskPrune 先立即清理一次、此后每小时清理一次超过保留期（14 天）的
// aigc 任务行，直到 ctx 结束（节奏对齐 auth.RunSessionCleanup；行数被保留期
// 钉住，全表扫描 DELETE 对板上规模可忽略——有意不给 created_at 建单独索引）。
// 装配层（runGatewayd）在独立 goroutine 中调用。
func RunAIGCTaskPrune(ctx context.Context, st *store.Store, logger *slog.Logger) {
	prune := func() {
		n, err := st.PruneAIGCTasks(ctx, time.Now().Add(-aigcTaskRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("清理过期任务行失败", "err", err.Error())
		case n > 0:
			logger.Info("已清理过期任务行", "count", n)
		}
	}
	prune()
	t := time.NewTicker(aigcTaskPruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
