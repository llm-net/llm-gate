package admin

// 「查询余额」端点：POST /admin/v1/upstreams/{id}/balance（2026-08-09）。
// 对一个上游账户向其平台的余额 API 发一次只读查询——特化平台能力的首个落点
// （upstream.SupportsBalance 是支持矩阵的唯一权威，当前仅 deepseek；方舟的
// 余额在火山引擎账号层须 AK/SK 签名、MiniMax 无公开端点，都是配不了而不是
// 漏配，理由记在 upstream/balance.go）。通用 openai_compat 恒不支持——它只
// 知道转发形状，不知道平台。
//
// 与「测试来源」同一批口径：无视启停位（停用的上游照样查得到余额）；凭证
// 解不开回 409 upstream_key_unreadable；查询打的是客户的上游账户，审计留痕。
//
// §15.1 口径（金额从严）：余额金额与失败摘要**只进管理 API 响应**，不落
// 日志；审计 detail 只记名称/类型/状态码，绝不含金额。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// EventUpstreamBalance 是余额查询的审计事件（detail 只含名称/类型/状态码，
// 金额一律不进审计）。
const EventUpstreamBalance = "upstream.balance"

// balanceQueryTimeout 是单次余额查询的硬上限（与来源探测同档：走管理员的
// 浏览器请求，只读小响应在秒级）。
const balanceQueryTimeout = 15 * time.Second

// balanceAmountJSON 是一种币种的余额读数（金额保持厂商十进制字符串原样，
// 设备不换算；granted/topped_up 平台没有对应口径时为空串，UI 按空隐藏）。
type balanceAmountJSON struct {
	Currency string `json:"currency"`
	Total    string `json:"total"`
	Granted  string `json:"granted"`
	ToppedUp string `json:"topped_up"`
}

// balanceReport 是余额端点的响应。ok=false 时 message 是失败摘要（上游错误
// message 或传输层归类）；status=0 表示未收到 HTTP 响应。
type balanceReport struct {
	UpstreamID int64  `json:"upstream_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	OK         bool   `json:"ok"`
	Status     int    `json:"status"`
	LatencyMS  int64  `json:"latency_ms"`
	Message    string `json:"message" i18n:"text"`
	// Available 是平台报告的「余额是否充足可用」。
	Available bool `json:"available"`
	// Balances 逐币种列出，恒非 nil（失败为空数组）。
	Balances []balanceAmountJSON `json:"balances"`
}

// handleUpstreamBalance 查询一个上游账户的平台余额。请求体无参数，忽略不读
// （与来源测试同款；api client 统一发空对象）。
func (s *Server) handleUpstreamBalance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "上游不存在")
		return
	}
	ru, err := s.st.GetRouteUpstreamByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrKeyUnreadable) {
			writeError(w, http.StatusConflict, "upstream_key_unreadable",
				"上游 Key 无法解密（设备密钥可能已更换或密文损坏），请在「模型接入」页重新录入该上游的 Key")
			return
		}
		s.writeCatalogError(w, r, err, "上游不存在")
		return
	}
	if !upstream.SupportsBalance(ru.Type) {
		writeError(w, http.StatusBadRequest, "balance_not_supported",
			"该上游类型不支持余额查询（特化平台能力，当前支持 DeepSeek 官网账户）")
		return
	}

	acct := upstream.Account{Name: ru.Name, Type: ru.Type, APIKey: ru.APIKey, BaseURL: ru.BaseURL, EgressMode: ru.EgressMode}
	queryCtx, cancel := context.WithTimeout(r.Context(), balanceQueryTimeout)
	res := upstream.QueryBalance(queryCtx, s.upstreamClient, acct)
	cancel()

	// 查询打的是客户的上游账户，谁在什么时候查过要可追溯；detail 只记名称/
	// 类型与状态码（net_error = 未收到 HTTP 响应），金额与摘要不进审计。
	status := "net_error"
	if res.Status != 0 {
		status = strconv.Itoa(res.Status)
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventUpstreamBalance,
		Entity:   entityUpstream(id),
		Detail:   fmt.Sprintf("name=%s type=%s status=%s", ru.Name, ru.Type, status),
		RemoteIP: remoteIP(r),
	})

	out := balanceReport{
		UpstreamID: id,
		Name:       ru.Name,
		Type:       ru.Type,
		OK:         res.OK,
		Status:     res.Status,
		LatencyMS:  res.LatencyMS,
		Message:    res.Message,
		Available:  res.Available,
		Balances:   make([]balanceAmountJSON, 0, len(res.Balances)),
	}
	for _, b := range res.Balances {
		out.Balances = append(out.Balances, balanceAmountJSON{
			Currency: b.Currency, Total: b.Total, Granted: b.Granted, ToppedUp: b.ToppedUp,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
