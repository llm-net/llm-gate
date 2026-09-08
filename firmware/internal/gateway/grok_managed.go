// grok_managed.go 实现 GET /grok-helper/managed-config——Grok Build 接入脚本的
// 受管区渲染面（契约整节见 docs/firmware-agents-grok.md「订阅模型目录与受管区」，
// 挂载见 docs/firmware-gateway.md 入口清单）。
//
// 它解决的是 grok CLI 的 /model 目录**在线拉取**够不到盒子的问题：盒子拿订阅
// 令牌代理 CCP 的模型目录，把每个模型渲染成 config.toml 里一个受管条目回给
// 安装脚本。不选路、不计量、不准入——它产出的是接入配置文本，不是模型调用。
//
// 降级是 200 不是 4xx/5xx：订阅未连接/失效/CCP 不可达等，一律降级渲染仅含
// 默认条目的受管区并留日志——「没连订阅」是状态不是故障，安装命令该照样完成。
// 唯一的例外是客户端 API 密钥被拒（中间件 401/403），脚本据此中止且一个字不写。
//
// §15.1：CCP 响应体只做内存解析，字段值只进渲染结果不进日志；access_token
// 只在装配上游请求头时出现在栈上。回填的 api_key 从本次请求的 Authorization
// 头来、回到属主自己的响应里去（产品决定同「属主自助复制」），不进日志。
package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/grokhelper"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// grokModelsURL 是订阅模型目录的上游（CCP——官方 CLI 会话拉取打的同一端点，
// 2026-08-13 已用真实订阅令牌验证最小头集与响应形态）。与 grokBackendURL
// 同性质：编译常量、不是配置项；开发期与测试走 [Server.SetGrokModelsEndpoint]。
const grokModelsURL = "https://cli-chat-proxy.grok.com/v1/models"

// grokCatalogTimeout 是目录拉取的整体超时。共享 client 的响应头超时是给
// 长流转发用的（5 分钟），一次安装期的目录 GET 挂住不该让安装命令吊着等。
const grokCatalogTimeout = 15 * time.Second

// grokCatalogMaxBytes 是目录响应体的读取上限（防御性：正常目录不到 2 KiB）。
const grokCatalogMaxBytes = 4 << 20

// managedModelParamRe 是 model 查询参数的字符集（与安装脚本的参数体检一致）。
var managedModelParamRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// forwardedProto 取 X-Forwarded-Proto 的第一跳，只认 http/https。
func forwardedProto(raw string) string {
	if raw == "" {
		return ""
	}
	proto := strings.ToLower(strings.TrimSpace(strings.Split(raw, ",")[0]))
	if proto == "http" || proto == "https" {
		return proto
	}
	return ""
}

// SetGrokModelsEndpoint 把订阅模型目录的上游指向别处（httptest 假 CCP）。
// **只给开发期与测试用**，生产恒是编译常量；空串恢复内置常量。令牌状态与
// 本设置无关。
func (s *Server) SetGrokModelsEndpoint(u string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.grokModels = u
}

// grokModelsEndpoint 取目录上游地址（开发期可覆盖）。
func (s *Server) grokModelsEndpoint() string {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.grokModels != "" {
		return s.grokModels
	}
	return grokModelsURL
}

// handleGrokManagedConfig 是 GET /grok-helper/managed-config 的入口
// （已过认证中间件）。
func (s *Server) handleGrokManagedConfig(w http.ResponseWriter, r *http.Request) {
	defModel := r.URL.Query().Get("model")
	if defModel != "" && !managedModelParamRe.MatchString(defModel) {
		openAIErrorStyle(w, http.StatusBadRequest, "invalid_model_name",
			"The model query parameter contains characters this device does not accept.")
		return
	}
	// 回填属主自己的那把密钥：withAuth 已验真，这里只是原样取用。
	apiKey := clientKey(r)
	// 脚本从哪个地址拉到区，区里就指回哪个地址。scheme 先看本监听是否 TLS；
	// 前面有反代终结 TLS 时 r.TLS 为 nil，再认一跳合法的 X-Forwarded-Proto
	// （只收 http/https，其它值忽略）。写成 http 而公网只开 https 时，
	// grok 跟 301 会丢掉 Authorization，表现为「session expired」。
	// 这里写的是客户端要回访的公开地址；转发头只用于生成地址，不参与鉴权。
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := forwardedProto(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	base := scheme + "://" + r.Host

	models, acctDefault := s.grokCatalogModels(r)
	if defModel == "" {
		defModel = acctDefault // 仍可为空：渲染器退 FallbackDefaultModel
	}

	body := grokhelper.RenderManagedRegion(base, apiKey, defModel, models)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write([]byte(body))
}

// grokCatalogModels 尽力取订阅模型目录：任何一步不可用都返回 nil（降级由
// 调用方渲染仅默认条目的区），第二个返回值是管理员设的 default_model（只要
// 账号行在场就取得到，与目录拉取成败无关）。
//
// 取令牌与 401 处置照订阅代理同一套：agentSessionFor 句柄、401 作废该代
// 重试一次、只看状态码（responses.go 的纪律，理由不重抄）。
func (s *Server) grokCatalogModels(r *http.Request) ([]grokhelper.CatalogModel, string) {
	info := infoFrom(r.Context())
	acct, authJSON, err := s.store.GetAgentCredential(r.Context(), store.AgentProviderGrok)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		s.log.Info("Grok 订阅未连接，受管区降级为仅默认条目", "request_id", info.id)
		return nil, ""
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		s.log.Warn("Grok 订阅凭据解不开，受管区降级为仅默认条目（请管理员重新连接订阅）",
			"request_id", info.id, "err", err.Error())
		return nil, ""
	case r.Context().Err() != nil:
		return nil, ""
	default:
		s.log.Error("读取 Grok 订阅账号失败，受管区降级为仅默认条目",
			"request_id", info.id, "err", err.Error())
		return nil, ""
	}
	if acct.Status != store.AgentStatusActive {
		s.log.Warn("Grok 订阅当前不可用，受管区降级为仅默认条目",
			"request_id", info.id, "status", acct.Status)
		return nil, acct.DefaultModel
	}
	sess, err := s.agentSessionFor(acct, authJSON)
	if err != nil {
		s.log.Warn("Grok 订阅凭据形态不合，受管区降级为仅默认条目（请管理员重新连接订阅）",
			"request_id", info.id, "err", err.Error())
		return nil, acct.DefaultModel
	}

	target := s.grokModelsEndpoint()
	for attempt := 1; attempt <= 2; attempt++ {
		token, err := sess.tokens.Current(r.Context())
		if err != nil {
			if r.Context().Err() == nil && !errors.Is(err, agentauth.ErrAuthExpired) {
				s.log.Warn("刷新 Grok 订阅凭据失败，受管区降级为仅默认条目",
					"request_id", info.id, "err", err.Error())
			} else if errors.Is(err, agentauth.ErrAuthExpired) {
				s.log.Warn("Grok 订阅登录已失效，受管区降级为仅默认条目（需管理员重新登录）",
					"request_id", info.id)
			}
			return nil, acct.DefaultModel
		}
		attemptCtx, cancel := context.WithTimeout(r.Context(), grokCatalogTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			s.log.Error("构造模型目录请求失败", "request_id", info.id, "err", err.Error())
			return nil, acct.DefaultModel
		}
		// 2026-08-13 真机验证的最小头集：Bearer + 用户令牌标记。官方 CLI 另带
		// x-userid/x-email/x-grok-client-version，实测非必需，刻意不发。
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(grokTokenAuthHeader, grokTokenAuthValue)
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				return nil, acct.DefaultModel
			}
			s.log.Warn("模型目录拉取失败，受管区降级为仅默认条目",
				"request_id", info.id, "attempt", attempt, "err", err.Error())
			return nil, acct.DefaultModel
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 1 {
			discardBody(resp, cancel)
			s.log.Info("模型目录上游拒绝当前凭据，刷新后重试一次", "request_id", info.id)
			sess.tokens.Invalidate(token)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			discardBody(resp, cancel)
			s.log.Warn("模型目录上游非 2xx，受管区降级为仅默认条目",
				"request_id", info.id, "status", resp.StatusCode)
			return nil, acct.DefaultModel
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, grokCatalogMaxBytes))
		resp.Body.Close()
		cancel()
		if err != nil {
			s.log.Warn("读取模型目录响应失败，受管区降级为仅默认条目",
				"request_id", info.id, "err", err.Error())
			return nil, acct.DefaultModel
		}
		models, ok := parseGrokCatalog(raw)
		if !ok {
			s.log.Warn("模型目录响应不是可解析的目录形态，受管区降级为仅默认条目",
				"request_id", info.id)
			return nil, acct.DefaultModel
		}
		return models, acct.DefaultModel
	}
	return nil, acct.DefaultModel // 第二次仍 401：Invalidate 已触发确定性处置
}

// parseGrokCatalog 解析 CCP 目录响应（OpenAI 信封 {object,data:[…]}，字段
// snake_case——2026-08-13 真机钉死的形态）。逐条尽力读：没有 slug 的条目跳过、
// 非 grok 前缀（小写比较）的 slug 过滤（前缀分流契约决定它到不了 grok 那行
// 订阅）、hidden 条目不写；上游的 base_url/api_key/env_key/extra_headers 等
// 凭据与端点字段**有意不读**——受管区里这些恒是盒子的值。
func parseGrokCatalog(raw []byte) ([]grokhelper.CatalogModel, bool) {
	obj, ok := decodeJSONObject(raw)
	if !ok {
		return nil, false
	}
	data := jsonArray(obj["data"])
	models := make([]grokhelper.CatalogModel, 0, len(data))
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		slug := jsonString(m["model"])
		if slug == "" {
			slug = jsonString(m["id"])
		}
		if slug == "" || !strings.HasPrefix(strings.ToLower(slug), "grok") {
			continue
		}
		if hidden, _ := m["hidden"].(bool); hidden {
			continue
		}
		cm := grokhelper.CatalogModel{
			Slug:              slug,
			Name:              jsonString(m["name"]),
			Description:       jsonString(m["description"]),
			SystemPromptLabel: jsonString(m["system_prompt_label"]),
			ReasoningEffort:   jsonString(m["reasoning_effort"]),
		}
		setNonZero(&cm.ContextWindow, m["context_window"])
		setNonZero(&cm.AutoCompactThresholdPercent, m["auto_compact_threshold_percent"])
		cm.SupportsBackendSearch, _ = m["supports_backend_search"].(bool)
		cm.SupportsReasoningEffort, _ = m["supports_reasoning_effort"].(bool)
		for _, rawOpt := range jsonArray(m["reasoning_efforts"]) {
			switch opt := rawOpt.(type) {
			case string: // 官方 wire 类型也认裸档位串
				if opt != "" {
					cm.Efforts = append(cm.Efforts, grokhelper.EffortOption{Value: opt})
				}
			case map[string]any:
				e := grokhelper.EffortOption{
					ID:          jsonString(opt["id"]),
					Value:       jsonString(opt["value"]),
					Label:       jsonString(opt["label"]),
					Description: jsonString(opt["description"]),
				}
				e.Default, _ = opt["default"].(bool)
				if e.Value != "" { // value 是官方类型的唯一必填项
					cm.Efforts = append(cm.Efforts, e)
				}
			}
		}
		models = append(models, cm)
	}
	return models, true
}
