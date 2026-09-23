package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// Tests a single draft protocol without persisting credentials or response content.
func (s *Server) handleTestUpstreamProtocol(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UpstreamID int64  `json:"upstream_id"`
		Protocol   string `json:"protocol"`
		BaseURL    string `json:"base_url"`
		APIKey     string `json:"api_key"`
		Model      string `json:"model"`
		EgressMode string `json:"egress_mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	urls, err := validateProtocolURLs(map[string]string{req.Protocol: req.BaseURL})
	if err != nil {
		writeError(w, 400, "invalid_protocol_urls", err.Error())
		return
	}
	if err := validateCatalogName(req.Model); err != nil {
		writeError(w, 400, "invalid_model", "请填写有效的测试模型名")
		return
	}
	acct := upstream.Account{Type: config.UpstreamGeneric, ProtocolURLs: urls, APIKey: req.APIKey, EgressMode: "inherit"}
	if req.UpstreamID != 0 {
		saved, err := s.st.GetRouteUpstreamByID(r.Context(), req.UpstreamID)
		if err != nil {
			s.writeCatalogError(w, r, err, "上游不存在")
			return
		}
		acct.EgressMode = saved.EgressMode
		if acct.APIKey == "" {
			oldURL := strings.TrimRight(decodeProtocolURLs(saved.ProtocolURLs)[req.Protocol], "/")
			if saved.Type != config.UpstreamGeneric || oldURL == "" || oldURL != decodeProtocolURLs(urls)[req.Protocol] {
				writeError(w, 400, "base_url_requires_key", "测试新地址必须重新输入上游 Key")
				return
			}
			acct.APIKey = saved.APIKey
		}
	}
	if req.EgressMode != "" {
		switch req.EgressMode {
		case "inherit", "direct", "proxy":
			acct.EgressMode = req.EgressMode
		default:
			writeError(w, 400, "invalid_egress_mode", "出站方式不合法")
			return
		}
	}
	if err := validateAPIKey(acct.APIKey); err != nil {
		writeError(w, 400, "invalid_api_key", "请填写有效的上游 Key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sourceProbeTimeout)
	defer cancel()
	result := upstream.Probe(ctx, s.upstreamClient, acct, req.Protocol, req.Model)
	s.audit(r.Context(), store.AuditEvent{Event: "upstream.test", Entity: entityUpstream(req.UpstreamID), Detail: fmt.Sprintf("protocol=%s status=%d", req.Protocol, result.Status), RemoteIP: remoteIP(r)})
	writeJSON(w, 200, sourceTestResultJSON{Protocol: req.Protocol, OK: result.OK, Status: result.Status, LatencyMS: result.LatencyMS, Message: result.Message, Questions: result.Questions})
}
