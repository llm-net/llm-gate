package admin

// 「测试来源」端点：POST /admin/v1/sources/{id}/test。对单条来源、按它能服务
// 的每个入口协议（与列表页 protocols 徽标同一口径、同一顺序）各发一次网关
// 自造的最小探测请求（"ping"，max_tokens=1），验证地址、凭证、来源侧模型 ID
// 与账户余额这条链路。模型页「逐个测试所有来源」由前端按优先级串行调用本
// 端点实现——服务端保持单来源粒度，便于界面逐条给出进度与结果。
// 只探文本模型的来源（迭代 8 起）：视频/图片来源的探测即真实提交付费任务，
// 归任务适配器迭代，这里对它们回空 results。
//
// 刻意无视三层启停位：测试的对象就包括已停用的行（先测通再启用是常规操作）。
// 模型的调用入口开关同理无视——先测通再决定开哪个入口；探测清单是来源的
// 能力集（servableProtocols），不是模型当下的服务集。
// 凭证解不开（设备密钥被替换/密文损坏）返回 409 upstream_key_unreadable，
// 指引重新录入，而不是拿空凭证去上游换一个误导性的 401。
//
// §15.1：探测结果的失败摘要（上游错误体的 error.message）只进管理 API 响应，
// 不落日志；审计 detail 只记名称与状态码。凭证明文只在进程内流向出站请求头。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// EventSourceTest 是来源测试的审计事件（detail 只含名称与状态码）。
const EventSourceTest = "source.test"

// sourceProbeTimeout 是单次 (来源, 协议) 探测的硬上限。探测走管理员的浏览器
// 请求，不能沿用数据面的分钟级生成超时；max_tokens=1 的正常往返在秒级。
const sourceProbeTimeout = 15 * time.Second

// sourceTestResultJSON 是一次入口协议探测的对外形态。
type sourceTestResultJSON struct {
	Protocol string `json:"protocol"`
	OK       bool   `json:"ok"`
	// Status 是上游 HTTP 状态码；0 表示未收到 HTTP 响应（连接失败/超时）。
	Status    int   `json:"status"`
	LatencyMS int64 `json:"latency_ms"`
	// Message 是失败摘要（上游错误 message 或传输错误归类）；成功为空。
	Message string `json:"message" i18n:"text"`
}

// sourceTestReport 是测试端点的响应：results 与来源的 protocols 徽标同序，
// 空数组表示该来源当前不服务任何入口。upstream_model_id 已解析（空即模型名），
// 让管理员看清实际发往上游的模型 ID。
type sourceTestReport struct {
	SourceID        int64                  `json:"source_id"`
	ModelID         int64                  `json:"model_id"`
	Model           string                 `json:"model"`
	Upstream        string                 `json:"upstream"`
	UpstreamModelID string                 `json:"upstream_model_id"`
	Results         []sourceTestResultJSON `json:"results"`
}

// handleTestModelSource 探测一条来源的全部可服务入口（顺序固定，逐个串行）。
// 请求体无参数，忽略不读（与 logout 同款；api client 统一发空对象）。
func (s *Server) handleTestModelSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型来源不存在")
		return
	}
	route, err := s.st.GetSourceRoute(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrKeyUnreadable) {
			writeError(w, http.StatusConflict, "upstream_key_unreadable",
				"上游 Key 无法解密（设备密钥可能已更换或密文损坏），请在「模型接入」页重新录入该上游的 Key")
			return
		}
		s.writeCatalogError(w, r, err, "模型来源不存在")
		return
	}

	cand := route.Candidate
	acct := upstream.Account{
		Name:       cand.Upstream.Name,
		Type:       cand.Upstream.Type,
		APIKey:     cand.Upstream.APIKey,
		BaseURL:    cand.Upstream.BaseURL,
		EgressMode: cand.Upstream.EgressMode,
	}
	// 只探文本模型的来源：视频/图片入口的"最小探测"意味着真实提交一次付费
	// 生成任务，且提交/取消语义因厂商而异——那是任务适配器（迭代 8 后续
	// phase）的能力，不塞进这个 ping 探针。非文本来源得到空 results，审计
	// detail 记 no_protocol（与「该来源当前不服务任何入口」同一口径）。
	var protocols []string
	if route.Model.Kind == store.ModelKindText {
		doc, _ := s.effectivePlatformModels(r.Context())
		protocols = sourceProtocols(doc, route.Model.Kind, route.Model.Name, cand.UpstreamModelID, cand.Upstream.Type, cand.Upstream.CatalogID, cand.Upstream.BaseURL)
	}
	results := make([]sourceTestResultJSON, 0, len(protocols))
	auditStatuses := make([]string, 0, len(protocols))
	// Chat 与 Responses 共用一次 Chat 上游探测，避免重复产生费用。
	probed := make(map[string]upstream.ProbeResult)
	for _, p := range protocols {
		wire := upstream.CatalogWireProtocol(p)
		res, tested := probed[wire]
		if !tested {
			probeCtx, cancel := context.WithTimeout(r.Context(), sourceProbeTimeout)
			res = upstream.Probe(probeCtx, s.upstreamClient, acct, wire, cand.UpstreamModelID)
			cancel()
			probed[wire] = res
		}
		results = append(results, sourceTestResultJSON{
			Protocol: p, OK: res.OK, Status: res.Status, LatencyMS: res.LatencyMS, Message: res.Message,
		})
		status := "net_error"
		if res.Status != 0 {
			status = strconv.Itoa(res.Status)
		}
		auditStatuses = append(auditStatuses, p+"="+status)
	}

	// 测试打的是客户付费的上游账户，谁在什么时候测过要可追溯；detail 只记
	// 名称与状态码（no_protocol = 该来源当前不服务任何入口），不记错误摘要。
	detail := fmt.Sprintf("model=%s upstream=%s %s",
		route.Model.Name, cand.Upstream.Name, strings.Join(auditStatuses, " "))
	if len(auditStatuses) == 0 {
		detail = fmt.Sprintf("model=%s upstream=%s no_protocol", route.Model.Name, cand.Upstream.Name)
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventSourceTest,
		Entity: entitySource(id), Detail: detail, RemoteIP: remoteIP(r),
	})

	writeJSON(w, http.StatusOK, sourceTestReport{
		SourceID:        cand.SourceID,
		ModelID:         route.Model.ID,
		Model:           route.Model.Name,
		Upstream:        cand.Upstream.Name,
		UpstreamModelID: cand.UpstreamModelID,
		Results:         results,
	})
}
