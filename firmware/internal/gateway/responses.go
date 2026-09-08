// responses.go 实现 Codex / Grok Build 订阅代理的 Responses 数据面。正式入口
// 是开发工具接入面里各自的那张脸——POST /agents/codex/v1/responses（经
// responses_codex.go 的按 Key 混合分流）与 POST /agents/grok/v1/responses——
// provider 由路径钉死；POST /agents/v1/responses 是兼容别名，按 model 前缀分流
// 两个 provider（agentProviderFor）。路由注册在 server.go。
//
// 用户 PC 上的官方 codex / grok CLI 以「自定义 provider」模式把 Responses 请求
// 发到这台盒子，盒子把它转给管理员关联的那份订阅背后的后端（codex：
// chatgpt.com/backend-api/codex/responses，注入 access_token 与
// ChatGPT-Account-ID；grok：api.x.ai/v1/responses，注入 access_token 与
// X-XAI-Token-Auth）。用户机上自始至终只有他自己的客户端 API 密钥，
// 一个 OpenAI/xAI 凭据都没有（决策 1）。
//
// 与另外三个消费入口最大的不同是**它不进模型目录**（决策 2）：不选路、不查
// models/model_sources、没有多来源故障切换——订阅本就是单账户。这条路要的
// 「上游」只有一个，就是 agent_accounts 里那一行。代价与收益都写在决策 2 里，
// 别顺手把它接回 resolveRoute。
//
// 三段职责：
//
//	取账号   每请求点查 agent_accounts（同 Key 鉴权的无缓存口径：删除/停用/
//	         标失效都在下一个请求生效），按行的状态分岔出两个机读原因——
//	         没有行 agent_not_configured、行在但失效 agent_auth_expired，
//	         **都是 4xx**：「没连订阅」是一种状态，不是设备故障。
//	取令牌   解封后的句柄交给 codexauth.Provider（进程内单飞刷新，见该包）：
//	         库里存的是句柄，内存里缓存的只是当前世代的 access_token。
//	         后端回 401 时作废这一代并重试一次——**只看状态码**，Phase 2 实测
//	         那个后端的 401 体是 {"detail":…} 而不是 {"error":{…}}，按错误体
//	         形状识别会漏。
//	回写     响应侧唯一的语义改写仍是把 model 改回客户端请求名，只是位置不同
//	         （Responses 事件里在 response.model），共用 proxy.go 的 SSE 逐行
//	         转发核心，未知事件/解析不了的 data: 行一律原样过。
//
// §15.1：本文件绝不把请求/响应 body 传入 logger；access_token 只在装配上游
// 请求头时出现在栈上，不进任何日志与错误文本（codexauth 的错误本身只带阶段名
// 与 HTTP 状态）。
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/codexauth"
	"github.com/llm-net/llm-gate/firmware/internal/grokauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// codexBackendURL 是 Codex 订阅后端的 Responses 端点（决策 6）。
//
// 编译进去的常量，**不是配置项**：它是 codex 这个产品的事实地址，与
// codexauth 的 client_id / issuer 同性质（决策 9 明写本迭代零新增必填 env）。
// 开发期要指向假后端走 [Server.SetAgentEndpoints]。
const codexBackendURL = "https://chatgpt.com/backend-api/codex/responses"

// codexAccountHeader 是订阅后端用来选账号的头（决策 6）。值来自句柄里的
// account_id（id_token 的 claim 或 auth.json 显式那一项），空则不发——
// 空 account_id 是允许的状态，后端会按令牌自己的归属处理。
const codexAccountHeader = "ChatGPT-Account-ID"

// codexActorAuthorizationHeader 是 Codex 内核判定「这个 provider 能画图」的
// 客户端开关：内核的 image-generation 扩展只在内建 openai provider、
// requires_openai_auth 的 provider，或 http_headers 带非空该头的 provider 上
// 挂出内置 image_gen 工具（codex-rs ext/image-generation extension.rs 与
// core tools/spec_plan.rs image_generation_available）。设备 provider 属第三种，
// gate 往派生与共享配置写占位值；值对设备与后端都没有意义，转发前一律剥掉，
// 设备自己的订阅令牌是唯一到达后端的凭据形态。
const codexActorAuthorizationHeader = "x-openai-actor-authorization"

// grokBackendURL 是 Grok Build 订阅后端的 Responses 端点。与 codexBackendURL
// 同性质：编译常量、不是配置项；开发期走 [Server.SetGrokEndpoints]。
const grokBackendURL = "https://api.x.ai/v1/responses"

// grokImagineBaseURL 是 Grok Imagine 图片/视频官方 API 的端点根：与订阅后端
// 同一宿主、同一份订阅令牌，设备在其后拼 /images/generations、/images/edits、
// /videos/generations 与 /videos/{request_id}（imagine_grok.go）。同性质的编译
// 常量；开发期与测试走 [Server.SetGrokImagineEndpoint]。
const grokImagineBaseURL = "https://api.x.ai/v1"

// grokTokenAuthHeader / grokTokenAuthValue 是 xAI 区分「用户 OAuth 令牌」与
// 企业部署密钥的 wire 约定（官方 GrokAuthCredentials::apply() 的契约：user
// token 发 Bearer + 该头，deployment key 发裸 Bearer）。少了它，订阅令牌可能
// 被当成 API密钥 鉴权而 401。注入纪律同 codexAccountHeader：转发处无条件
// Del 再 Set（客户端自带的那份不许跟着盒子的令牌进后端），也同样**刻意不进
// copyForwardHeaders**——那是三个选路入口共用的透传核心。
const (
	grokTokenAuthHeader = "X-XAI-Token-Auth"
	grokTokenAuthValue  = "xai-grok-cli"
)

// agentProviderFor 是兼容别名 /agents/v1/responses 的 provider 分流：小写后以
// grok 开头的 model → Grok Build，其余 → Codex。正式接入面按路径钉死 provider，
// 不经这里。
//
// 判据取 model 名而不是路径/头：两家官方 CLI 说的是同一种 Responses 协议，
// 而模型命名空间天然不相交——xAI 的模型就叫 grok-*，OpenAI 侧是 gpt-*/codex-*。
func agentProviderFor(model string) string {
	if strings.HasPrefix(strings.ToLower(model), "grok") {
		return store.AgentProviderGrok
	}
	return store.AgentProviderCodex
}

// agentDisplayName 是错误文案与用量维度里的 provider 名。
func agentDisplayName(provider string) string {
	switch provider {
	case store.AgentProviderGrok:
		return "Grok Build"
	case store.AgentProviderClaude:
		return "Claude Code"
	case store.AgentProviderCursor:
		return "Cursor"
	default:
		return "Codex"
	}
}

// agentUsageUpstreamName 把订阅账号按「provider + 管理名称」写入用量账本。
// 名称是账号在请求发生时的快照，后续改名不重写历史用量。
func agentUsageUpstreamName(acct *store.AgentAccount) string {
	base := agentDisplayName(acct.Provider) + " 订阅"
	label := strings.TrimSpace(acct.Label)
	if label == "" || label == base {
		return base
	}
	return base + " · " + label
}

// agentSession 是一份订阅句柄在**本进程内**的令牌状态：解封后的世代、到期
// 时刻与单飞刷新闸（都在 codexauth.Provider 里）。
//
// rowID/updatedAt 是它对应的那一行的身份与世代。每个请求都会拿库里那一行的
// updated_at 与这里比对：
//
//   - 行还是那一行、世代**没有前进**（绝大多数请求，以及迟到的旧快照）→
//     直接复用，零解析零分配；
//   - 世代**前进了**（管理员重新登录/粘贴导入，或本进程刚刷新过并落了库）→
//     Reset 换上库里那一份。这里不必去分辨「是别人换的还是我自己刚换的」——
//     后者（本进程刚刷新完、OnRotate 抬高了 updated_at）读回来的就是同一代，
//     而 [codexauth.Provider.Reset] 对同一代是**明确的空操作**（它据此保住
//     刷新时算好的到期时刻与 dirty 位；那不是可有可无的细节，理由写在该方法上）；
//   - 换了一行（删掉重连）→ 整个重建，因为回调里钉着旧的行 id。
//
// **只进不退**是这里唯一的硬约束，也是判据必须是「更新」而不是「不相等」的
// 原因：点查（GetAgentCredential）发生在 agentMu 之外，两个并发请求的快照因此
// 可以乱序到达。按「不相等就换」处理时，一份迟到的旧快照会把内存里的世代
// **倒回去**——而 refresh token 是轮换型的（决策 1），倒回去的那一代手上是一份
// 已经被消费掉的 refresh token，下一次刷新必被确定性拒绝，于是整台设备落闩
// （此后每个请求都回 agent_auth_expired），库里明明还躺着一份好句柄，却只有
// 管理员重新登录才解得开。
type agentSession struct {
	provider  string
	rowID     int64
	updatedAt time.Time
	// Quota component fingerprint: setup-token/metadata changes must not reset a
	// provider waiting on an in-flight rotation back to the consumed generation.
	claudeQuotaVersion [32]byte
	// tokens 是共用的 agentauth 状态机；句柄解析与刷新接线按 provider 分岔
	// （agentSessionFor）。
	tokens *agentauth.Provider
}

// SetAgentEndpoints 把 Agents 的两个外部地址指向别处：issuer 是 OpenAI 的账号
// 签发方（codexauth 刷新令牌打它），backendURL 是订阅后端的 Responses 端点。
// 空串各自取内置常量。
//
// **只给开发期与测试用**，生产两个都是编译进去的常量（决策 9：零新增必填
// env）——同 upstream 的 base_url 覆盖一个角色。调用会丢掉已缓存的令牌状态，
// 下一个请求按新地址重建。
func (s *Server) SetAgentEndpoints(issuer, backendURL string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.agentIssuer, s.agentBackend = issuer, backendURL
	s.agentAuth = nil
	s.agentSessions = nil
}

// SetGrokEndpoints 同 [Server.SetAgentEndpoints]，作用于 Grok Build 那一侧
// （issuer 是 auth.x.ai 的替身，backendURL 是 api.x.ai Responses 端点的替身）。
// **只给开发期与测试用**，生产两个都是编译进去的常量。
func (s *Server) SetGrokEndpoints(issuer, backendURL string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.grokIssuer, s.grokBackend = issuer, backendURL
	s.grokAuth = nil
	s.agentSessions = nil
}

// handleResponses 是兼容别名 POST /agents/v1/responses 的入口（已过认证中间件）：
// provider 按 model 前缀猜（agentProviderFor）。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	// 读体 + 通用 map 解析（未知字段保留）+ model 必填：与另外三个入口同一份
	// 解码（entry.go）。Responses 协议的 model 同样必填。
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	s.handleAgentResponsesFor(w, r, agentProviderFor(model), payload, model)
}

// handleGrokResponses 是 POST /agents/grok/v1/responses 的入口（已过认证与
// withDevTool("grok")）：provider 由路径钉死，不看 model 名。
func (s *Server) handleGrokResponses(w http.ResponseWriter, r *http.Request) {
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	s.handleAgentResponsesFor(w, r, store.AgentProviderGrok, payload, model)
}

// handleAgentResponsesFor 承接一份已经解码的订阅 Responses 请求，provider 由
// 调用方给定：Codex 接入面（responses_codex.go）与 /agents/grok/v1/responses
// 按路径钉死，兼容别名 /agents/v1/responses 按 model 前缀猜。三处共用这一段，
// 避免同一个请求读两遍 body。
func (s *Server) handleAgentResponsesFor(w http.ResponseWriter, r *http.Request, provider string, payload map[string]any, model string) {
	if _, ok := s.subscriptionPermitted(w, r, provider, false); !ok {
		return
	}
	// 自此本请求要入账（决策 8）：Responses 是消费入口，成败都记一笔。
	info := beginEntry(r, usage.EntryResponsesAgents, model)
	// 输入侧估算器：只在上游一个 usage 字段都没给时才会被用到（流被截断）。
	// Responses 的输入不叫 messages/system，见 usage.EstimateResponsesInput。
	info.bill.estimateInput = func() int64 { return usage.EstimateResponsesInput(payload) }
	// 预算准入：与另外三个消费入口同一位置（鉴权之后、任何点查之前）。订阅流量
	// 恒 0 元，所以金额档位天然拦不住它——密钥级 RPM 才是这条路唯一有效的闸门
	// （决策 8，AGENTS.md 同步写明）。
	if !s.admit(w, r, openAIErrorStyle) {
		return
	}
	// bill.kind 有意留空：这条路没有模型目录行，kind 由 entry 推出（=text）。
	// bill.pricing / modelKnown 见 applyAgentModelPricing——它在准入**之后**，
	// 与「被拒的请求不该消耗一次点查」同一条纪律（那正是要限流的那条路）。
	s.applyAgentModelPricing(r, info, model)

	// provider 分流发生在取账号这一步；准入、计量、透传核心不分叉
	// （docs/firmware-agents-grok.md）。
	sess, accountID, ok := s.agentCredential(w, r, provider)
	if !ok {
		return
	}
	// 请求体整读在手（已解析成 map，这里重序列化一份）：401 之后要原样重发，
	// 而 r.Body 已经读干净了。model **不改写**——没有来源侧 ID 这回事。
	body, err := encodeJSON(payload)
	if err != nil { // map 源自合法 JSON，理论不可达；防御分支
		s.log.Error("重序列化请求体失败", "request_id", info.id, "err", err.Error())
		openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	stream, _ := payload["stream"].(bool)
	s.forwardResponses(w, r, sess, accountID, body, respRewrite{
		model:     model,
		expectSSE: stream,
		rewrite:   rewriteResponsesModel,
		observe:   s.responsesObserver(info),
	})
}

// applyAgentModelPricing 给不经目录选路的文本 Agent 请求填目录价与
// 「目录里有没有这个名字」。Codex/Grok/Claude 订阅代理
// 共用这一个只按模型名借价的旋钮。
//
// 决策 2 不给 Agents 建模型目录行，所以常态是**查不到**：pricing 空 = 未定价
// → 金额 0（订阅无边际成本，0 元是真值），modelKnown=false → 该名字受账本的
// 未知模型名合并帽约束。这两条都是决策 8 如实接受的推论，不是 bug。
//
// 查得到的那种情况是决策 8 点名允许的**旋钮**：管理员手工在模型目录建一行同名
// text 模型并录价，codex 流量就按名取价记出金额、预算随之生效。那是**名义
// 成本**（订阅并不按次付费），允许但不作为口径——所以这里只做一次窄读数
// （名字 → 价签，不碰来源与凭证），不做任何别的目录语义。
//
// 读库失败一律按未定价继续：为一次价签读不出来而回绝一个合法请求，方向反了
// （宁松勿紧，同 annotatePricing 的降级口径）。
func (s *Server) applyAgentModelPricing(r *http.Request, info *reqInfo, model string) {
	s.applyAgentModelPricingKind(r, info, model, store.ModelKindText)
}

// applyAgentModelPricingKind 是上面那个旋钮按 kind 参数化的形态：只有目录行的
// kind 与调用形态相符时才借价——文本调用只借 text 行，Grok Imagine 画图门只借
// image 行（imagine_grok.go）。同名而 kind 不符的行仍算「目录里有这个名字」
// （modelKnown），但价签不借：按张/按秒的价签套在 token 量上、或反过来，都是
// 一笔算错的账。
func (s *Server) applyAgentModelPricingKind(r *http.Request, info *reqInfo, model, kind string) {
	if s.store == nil { // 只会出现在不装 store 的窄单元测试
		return
	}
	m, err := s.store.GetModelByName(r.Context(), model)
	switch {
	case err == nil:
		info.bill.modelKnown = true
		// 同名而形态不符的行不能把价签借过来（文本 ↔ 按张/按秒）。
		if m.Kind == kind {
			info.bill.pricing = m.Pricing
		}
	case errors.Is(err, store.ErrNotFound): // 常态：目录里没有这个名字
	case r.Context().Err() != nil: // 客户端断开，不是库故障
	default:
		s.log.Warn("读取模型目录价失败，本次按未定价记账",
			"request_id", info.id, "model", model, "err", err.Error())
	}
}

// agentCredential 取「已连接且可用的订阅」（provider 由 model 分流决定）：
// 每请求点查 agent_accounts，按状态分岔出机读原因，再拿到该句柄的进程内令牌
// 状态。返回 ok=false 时错误响应已写出。
//
// 点查而不是缓存，理由与 Key 鉴权同一条：删除、停用、标失效都该在**下一个
// 请求**生效，中间不留一层要失效的缓存。缓存的只有解封之后的令牌世代
// （agentSession），那是内存态，不是权威。
func (s *Server) agentCredential(w http.ResponseWriter, r *http.Request, provider string) (*agentSession, string, bool) {
	info := infoFrom(r.Context())
	acct, authJSON, err := s.store.GetAgentCredential(r.Context(), provider)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeAgentNotConfigured(w, provider)
		return nil, "", false
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		// 设备密钥被换过或密文损坏：对客户端与「登录失效」同一处置——都要
		// 管理员去管理台重新连接一次订阅。
		s.log.Warn("Agents 凭据解不开，请管理员在管理台重新连接订阅",
			"request_id", info.id, "provider", provider, "err", err.Error())
		writeAgentAuthExpired(w, provider)
		return nil, "", false
	case r.Context().Err() != nil: // 客户端中途断开，不是库故障
		s.log.Info("客户端断开，Agents 账号点查已取消", "request_id", info.id)
		return nil, "", false
	default:
		s.log.Error("读取 Agents 账号失败", "request_id", info.id, "provider", provider, "err", err.Error())
		openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, "", false
	}

	// 上游维度在**账号点查成功的这一刻**就钉住，不等会话备好：从这里往下的每一种
	// 拒绝（管理员停用、登录失效、凭据形态不合）说的都是这一个订阅账号自己的事，
	// 账要记在它头上。等成功之后再填，这些请求会掉进用量页「按上游账号」的空名字
	// 那一格，管理员只看到一行「— ¥0.00 1 次」，读不出是哪份订阅在拒人。同
	// forward()：上游维度在尝试开始时就填，不是提交响应之后才填。
	//
	// 空名字那一格因此只剩一种含义：请求没有绑上任何上游——这台设备上压根没有
	// 这份订阅、选路没选出来源、或者准入直接把它拦了。那种请求确实无从归属。
	info.upstream = agentUsageUpstreamName(acct)

	switch acct.Status {
	case store.AgentStatusActive:
	case store.AgentStatusAuthExpired:
		writeAgentAuthExpired(w, provider)
		return nil, "", false
	default:
		// 管理员停用（以及将来任何新状态）：对调用方等同于「这台设备上没有
		// 可用的订阅」——使用API页那侧的 available 判定也是这个口径。
		writeAgentNotConfigured(w, provider)
		return nil, "", false
	}

	sess, err := s.agentSessionFor(acct, authJSON)
	if err != nil {
		// 密文解开了但内容不是一份可用的 auth.json（手改过的库、旧格式）：
		// 同样只有重新连接一条路。错误文本来自 codexauth/grokauth，不含原文。
		s.log.Warn("Agents 凭据形态不合，请管理员在管理台重新连接订阅",
			"request_id", info.id, "account", acct, "err", err.Error())
		writeAgentAuthExpired(w, provider)
		return nil, "", false
	}
	// 账号头只有 codex 有（ChatGPT-Account-ID）：grok 的账号归属在令牌里，
	// 没有对应概念，accountID 恒空。
	var accountID string
	if provider == store.AgentProviderCodex {
		accountID = codexauth.ProviderAccountID(sess.tokens)
		if accountID == "" {
			accountID = acct.AccountID // 句柄里没有 claim 时退回库里存的那份
		}
	}
	return sess, accountID, true
}

// agentSessionFor 取（必要时建/换代）一份句柄的进程内令牌状态，见
// [agentSession] 的三种情形。
func (s *Server) agentSessionFor(acct *store.AgentAccount, authJSON string) (*agentSession, error) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()

	if sess := s.agentSessions[acct.Provider]; sess != nil && sess.rowID == acct.ID {
		// 只进不退：同一世代（绝大多数请求）与**迟到的旧快照**都走这一条，
		// 直接复用内存里那一代。理由见 [agentSession] 的「只进不退」一段。
		if !acct.UpdatedAt.After(sess.updatedAt) {
			return sess, nil
		}
		auth, err := s.parseAgentAuthLocked(acct.Provider, authJSON)
		if err != nil {
			return nil, err
		}
		sess.updatedAt = acct.UpdatedAt
		if acct.Provider == store.AgentProviderClaude {
			blob, _ := auth.JSON()
			version := sha256.Sum256([]byte(blob))
			if version == sess.claudeQuotaVersion {
				return sess, nil
			}
			sess.claudeQuotaVersion = version
		}
		// Reset 在 agentMu **之外**做：它自己在 Provider 锁下同代去重，而那把
		// 锁可能正被一次在飞刷新抱着（RefreshTimeout+CallbackTimeout 最长
		// ~40s——管理台重连恰逢慢刷新时就是这个量级）。抱着 agentMu 等它，
		// 会把受管区渲染与另一个 provider 的会话一起冻住。
		// 代价：等 Reset 生效前的并发请求可能还拿旧代令牌撞一次 401，
		// 由各入口既有的 401 失效重试兜住。
		s.agentMu.Unlock()
		sess.tokens.Reset(auth)
		s.agentMu.Lock() // 配平 defer Unlock
		return sess, nil
	}

	auth, err := s.parseAgentAuthLocked(acct.Provider, authJSON)
	if err != nil {
		return nil, err
	}
	sess := &agentSession{provider: acct.Provider, rowID: acct.ID, updatedAt: acct.UpdatedAt}
	opts := agentauth.ProviderOptions{
		OnRotate:      s.persistAgentAuth(acct.ID, acct.Provider),
		OnAuthExpired: s.markAgentAuthExpired(acct.ID, acct.Provider),
	}
	if acct.Provider == store.AgentProviderClaude {
		blob, _ := auth.JSON()
		sess.claudeQuotaVersion = sha256.Sum256([]byte(blob))
		sess.tokens = agentauth.NewProvider(&claudeAccountRefresher{server: s, client: claudeauth.NewClient(s.egress), rowID: acct.ID}, auth, agentauth.ProviderOptions{})
	} else if acct.Provider == store.AgentProviderGrok {
		sess.tokens = agentauth.NewProvider(s.grokClientLocked(), auth, opts)
	} else {
		sess.tokens = agentauth.NewProvider(s.codexClientLocked(), auth, opts)
	}
	if s.agentSessions == nil {
		s.agentSessions = make(map[string]*agentSession, 2)
	}
	s.agentSessions[acct.Provider] = sess
	return sess, nil
}

// parseAgentAuthLocked 按 provider 解析句柄明文；调用方持 agentMu（与会话
// 构建同临界区，纯 CPU 解析，不出网）。显式两步返回是在挡 typed-nil：
// 失败路径必须返回**接口零值** nil，而不是被隐式装箱的 nil 具体指针。
func (s *Server) parseAgentAuthLocked(provider, authJSON string) (agentauth.Handle, error) {
	if provider == store.AgentProviderClaude {
		a, err := claudeauth.Parse(authJSON)
		if err != nil {
			return nil, err
		}
		if !a.IsOAuth() || a.OAuthExpired() {
			return nil, agentauth.ErrAuthExpired
		}
		return a.OAuthCredential(), nil
	}
	if provider == store.AgentProviderGrok {
		a, err := grokauth.ParseAuthJSON(authJSON)
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	a, err := codexauth.ParseAuthJSON(authJSON)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AgentModels 供出「哪个模型名属于哪份已连接的订阅」（生产实现是
// *admin.Server，由 cmd/llmgate 在装配期经 [Server.SetAgentModels] 注入）。
//
// 键是模型行的**原始名**（对外就发这一串），值是订阅 provider（codex|grok|claude）。
//
// 与 admin.AgentTokens 同一手法、方向相反：接口定义在消费方，签名只用标准库
// 类型，于是 gateway 与 admin 两包之间仍然一个 import 都没有。判据留在管理面
// 是有意的——「这个名字是不是订阅带来的」要读模型目录数据的 agents 段并对账，
// 那套解析、降级与所有权判定只该有一份（internal/admin/agentmodels.go），
// 数据面照抄一份必然分叉。
type AgentModels interface {
	AgentSubscriptionModels(ctx context.Context) (map[string]string, error)
}

// SetAgentModels 注入订阅模型读数（装配期一次性，之后只读）。
func (s *Server) SetAgentModels(m AgentModels) { s.agentModels = m }

// AgentSubscriptionModels exposes the currently injected ownership view to
// the per-key development-tool policy projector.
func (s *Server) AgentSubscriptionModels(ctx context.Context) (map[string]string, error) {
	if s.agentModels == nil {
		return map[string]string{}, nil
	}
	return s.agentModels.AgentSubscriptionModels(ctx)
}

// handleAgentModels 实现兼容别名 GET /agents/v1/models：列**已连接订阅带来的文本模型**，
// OpenAI list 形，按名字典序。codex 的模型选择器会打 <base_url>/models
// （实测 0.147.0，带 ?client_version=…），此前这条路落到 /agents/ 兜底、认证
// 通过也只回 404，于是订阅用户的模型列表恒为空。
//
// owned_by 是**订阅 provider**，而不是 /v1/models 那个恒定的 llmgate：
// 这条读数的意义就在于告诉调用方哪个名字背后是哪份订阅。
//
// **不按 provider 过滤**：别名 /agents/v1/responses 按 model 前缀分流、这批名字
// 哪一个都调得到，列表因此与它同域（同一把客户端 API Key 能调到的，就都列出来）。
//
// 两处刻意的降级——模型选择器空着不影响 /responses，把它做成故障点不划算：
// 未接线（agentModels 为 nil）答空列表；读数出错记日志后同样答空列表。
// created 从模型行取，所以还要一次目录读；那一次失败也走同一条降级。
func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
	list := s.agentModelList(r)
	cfg, err := s.store.GetDevToolConfig(r.Context(), infoFrom(r.Context()).keyID)
	if err != nil {
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	accounts, err := s.store.ListAgentAccounts(r.Context())
	if err != nil {
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	allowed := map[string]bool{}
	for _, account := range accounts {
		if account.Status != store.AgentStatusActive {
			continue
		}
		switch account.Provider {
		case store.AgentProviderCodex:
			allowed[account.Provider] = cfg.AllowCodexSubscription
		case store.AgentProviderGrok:
			allowed[account.Provider] = cfg.AllowGrokSubscription
		case store.AgentProviderClaude:
			if cfg.AllowClaudeSubscription {
				_, blob, err := s.store.GetAgentCredential(r.Context(), account.Provider)
				if err == nil {
					cred, err := claudeauth.Parse(blob)
					allowed[account.Provider] = err == nil && cred.HasSetupToken()
				}
			}
		}
	}
	visible := list.Data[:0]
	for _, model := range list.Data {
		if allowed[model.OwnedBy] {
			visible = append(visible, model)
		}
	}
	list.Data = visible
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// agentModelList 组装订阅模型列表但不写响应。独立出来给 Codex 混合入口把
// profile 勾选的普通目录模型并进同一份 /model 读数；原 /agents/v1/models
// 仍只调用本函数本身，返回语义不变。
func (s *Server) agentModelList(r *http.Request) modelList {
	list := modelList{Object: "list", Data: []modelObject{}}
	if s.agentModels == nil {
		return list
	}
	owners, err := s.agentModels.AgentSubscriptionModels(r.Context())
	if err != nil {
		if r.Context().Err() != nil { // 客户端中途断开，不是故障
			return list
		}
		s.log.Warn("列出订阅模型失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		return list
	}
	if len(owners) == 0 {
		return list
	}
	// 每把 Key 只看到自己明确获准的订阅；列表隐藏不是权限边界，responses
	// 入口还会逐请求执行同一开关。
	if s.devTools != nil {
		snapshot, err := s.devTools.Snapshot(r.Context(), infoFrom(r.Context()).keyID)
		if err != nil {
			return list
		}
		for name, provider := range owners {
			if !snapshot.Subscription(provider).Configured {
				delete(owners, name)
			}
		}
	}
	// created 取模型行的建行时刻，与 /v1/models 同一口径。models 是张小表
	// （一台设备几十行），一次全量读比逐名点查便宜得多。
	rows, err := s.store.ListModels(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return list
		}
		s.log.Warn("列出订阅模型时读目录失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		return list
	}
	for _, m := range rows {
		provider, ok := owners[m.Name]
		if !ok {
			continue
		}
		list.Data = append(list.Data, modelObject{
			ID:      m.Name,
			Object:  "model",
			Created: m.CreatedAt.Unix(),
			OwnedBy: provider,
		})
	}
	sort.Slice(list.Data, func(i, j int) bool { return list.Data[i].ID < list.Data[j].ID })
	return list
}

// RefreshAgent 强制换一代订阅令牌，供管理面的「自检」动作调用（迭代 11
// Phase 4；管理面经 admin.AgentTokens 接口注入本方法，两包之间没有 import）。
//
// **为什么这件事必须由数据面来做**：本进程里那份句柄的令牌状态只有一处
// （agentSessions 里的 codexauth.Provider），而盒子是这份轮换型 refresh token
// 的唯一刷新者（决策 1）。管理面若自己造一个 Provider 去刷新，就成了第二个
// 消费者——两边各换各的世代，谁先落库谁把对方手里那一份作废，而被作废的那侧
// 下一次刷新会被确定性拒绝、整台设备落 auth_expired 闩。所以自检走的是与
// /agents/{codex,grok}/v1/responses **同一个** Provider（同一把锁、同一条落库回调）。
//
// 先点查再取会话，与数据面同一条路：库里那一行若比内存里的新（管理员刚重新
// 登录过），agentSessionFor 会先换代再刷新；比内存里旧则忽略（只进不退）。
func (s *Server) RefreshAgent(ctx context.Context, provider string) error {
	// Cursor 是静态 API Key + 上游换发（cursor.go），没有轮换句柄，也就没有
	// agentSession/agentauth.Provider：它的自检 = 重新 exchange 一次，与数据面
	// 共用同一个单飞会话，先分流出去。
	if provider == store.AgentProviderCursor {
		return s.refreshCursorAgent(ctx)
	}
	acct, authJSON, err := s.store.GetAgentCredential(ctx, provider)
	if err != nil {
		return err
	}
	sess, err := s.agentSessionFor(acct, authJSON)
	if err != nil {
		// 密文解开了但内容不是一份可用的 auth.json（手改过的库、旧格式）。
		// **要报成「登录已失效」而不是原样往上抛**：数据面对同一情况就是这个
		// 处置（agentCredential 那条 writeAgentAuthExpired），而管理面把不认识
		// 的错误一律归进「连不上 OpenAI」那一支——于是自检会对着一份形态不合的
		// 句柄说"请检查出网连通性"，把管理员支去查网络。两条路对同一状态必须
		// 说同一句话：重新连接这份订阅。
		return fmt.Errorf("%w（订阅凭据形态不合：%v）", codexauth.ErrAuthExpired, err)
	}
	return sess.tokens.Refresh(ctx)
}

// persistAgentAuth 造 codexauth 的 OnRotate 回调：把新一代句柄重新密封落库。
//
// 轮换型 refresh token 单次消费，不落库的话重启后盒子手上是一份已经作废的
// refresh token，必 401（决策 5）。回调**持锁执行**，所以里面只做一次窄写入，
// 绝不回调 Provider（会死锁）。
//
// 落库失败不让本次请求失败（codexauth 有意丢弃这个返回值），但**必须留一条
// 日志**——codexauth 自己没有 logger，这条线是它唯一的出口。
func (s *Server) persistAgentAuth(id int64, provider string) func(context.Context, string) error {
	return func(ctx context.Context, authJSON string) error {
		if err := s.store.SetAgentAuthJSON(ctx, id, provider, authJSON); err != nil {
			s.log.Error("Agents 新一代凭据落库失败（重启后将拿已作废的凭据去刷新）",
				"provider", provider, "account_row", id, "err", err.Error())
			return err
		}
		s.log.Info("Agents 订阅凭据已刷新", "provider", provider, "account_row", id)
		return nil
	}
}

// markAgentAuthExpired 造 codexauth 的 OnAuthExpired 回调：刷新被**确定性
// 拒绝**时把库里那一行标成 auth_expired，管理台据此显「需重新登录」，
// /agents/{codex,grok}/v1/responses 据此回 agent_auth_expired。同样持锁执行、同样不回调 Provider。
func (s *Server) markAgentAuthExpired(id int64, provider string) func(context.Context, error) {
	return func(ctx context.Context, cause error) {
		s.log.Warn("订阅登录已失效，自动刷新到此为止，需管理员重新登录",
			"provider", provider, "account_row", id, "err", cause.Error())
		if err := s.store.SetAgentStatus(ctx, id, store.AgentStatusAuthExpired); err != nil {
			s.log.Error("标记 Agents 登录失效失败",
				"provider", provider, "account_row", id, "err", err.Error())
		}
	}
}

// codexClientLocked 取（必要时建）与 OpenAI 之间的 OAuth 客户端；调用方必须
// 持 agentMu。issuer 为空时 codexauth 自己取 DefaultIssuer。
func (s *Server) codexClientLocked() *codexauth.Client {
	if s.agentAuth == nil {
		s.agentAuth = codexauth.NewClient(s.agentIssuer, codexauth.WithEgress(s.egress))
	}
	return s.agentAuth
}

// grokClientLocked 同上，xAI 那一侧。
func (s *Server) grokClientLocked() *grokauth.Client {
	if s.grokAuth == nil {
		s.grokAuth = grokauth.NewClient(s.grokIssuer, grokauth.WithEgress(s.egress))
	}
	return s.grokAuth
}

// agentBackendURL 取该 provider 订阅后端的 Responses 端点（开发期可覆盖）。
func (s *Server) agentBackendURL(provider string) string {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if provider == store.AgentProviderGrok {
		if s.grokBackend != "" {
			return s.grokBackend
		}
		return grokBackendURL
	}
	if s.agentBackend != "" {
		return s.agentBackend
	}
	return codexBackendURL
}

// forwardResponses 把请求转给订阅后端并提交响应。至多两次尝试，且第二次只在
// **后端拒绝了这一代 access_token**（401）时发生：作废那一代、刷新、重发。
//
// 为什么值得为 401 专门重试一次：到期时刻不总是可知（应答不带 expires_in 且
// 令牌不是 JWT 时 Provider 按「有效」处理），厂商也随时可以提前吊销一个令牌。
// 判据**只看状态码**——Phase 2 实测该后端的坏令牌 401 体是 {"detail":…}，
// 与 OpenAI 别处的 {"error":{…}} 不同形，按体形状识别必漏。
//
// 作废按世代去重（Invalidate 收下刚才用的那一个）：令牌被吊销时通常是 N 个
// 在飞请求同时 401，无条件置脏会让它们连锁换出 N 代，每代还各带一次落库。
//
// 与选路那条路的一个关键差异：这里**没有下一个来源**。连接失败就是失败，
// 上游的 4xx/5xx 一律原样透传给客户端（透传优先，决策 6）。
func (s *Server) forwardResponses(w http.ResponseWriter, r *http.Request,
	sess *agentSession, accountID string, body []byte, rw respRewrite) {
	s.forwardAgent(w, r, sess, accountID, http.MethodPost, s.agentBackendURL(sess.provider), body, rw)
}

// forwardAgent 是 forwardResponses 的通用形：目标 URL 与方法由调用方给定，
// 令牌注入、401 换代重试与提交段完全同一份。Responses 面传订阅后端的
// Responses 端点；Grok Imagine 画图/视频门（imagine_grok.go）传 xAI 官方
// Imagine 路径（同一份订阅令牌、同一个宿主）。body 为 nil 表示无体请求（GET 轮询）。
func (s *Server) forwardAgent(w http.ResponseWriter, r *http.Request,
	sess *agentSession, accountID, method, target string, body []byte, rw respRewrite) {

	info := infoFrom(r.Context())
	name := agentDisplayName(sess.provider)
	for attempt := 1; attempt <= 2; attempt++ {
		token, err := sess.tokens.Current(r.Context())
		if err != nil {
			s.writeAgentTokenError(w, r, sess.provider, err)
			return
		}
		info.attempts = attempt

		attemptCtx, cancel := context.WithCancel(r.Context())
		req, err := buildAgentRequest(attemptCtx, r, sess.provider, method, target, token, accountID, body)
		if err != nil {
			cancel()
			s.log.Error("构造 Agents 后端请求失败", "request_id", info.id,
				"provider", sess.provider, "err", err.Error())
			openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
			return
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，Agents 后端请求已取消",
					"request_id", info.id, "provider", sess.provider, "model", rw.model)
				return
			}
			// 传输层错误文本只含地址/超时信息，不含 body 与凭证。
			s.log.Warn("Agents 后端请求失败", "request_id", info.id, "provider", sess.provider,
				"model", rw.model, "attempt", attempt, "err", err.Error())
			openAIErrorStyle(w, http.StatusBadGateway, "upstream_unreachable",
				"The gateway failed to reach the "+name+" backend.")
			return
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 1 {
			// 有界丢弃这份 401 体（内容一概不进日志），换一代令牌重发。
			discardBody(resp, cancel)
			s.log.Info("Agents 后端拒绝当前凭据，刷新后重试一次",
				"request_id", info.id, "provider", sess.provider)
			sess.tokens.Invalidate(token)
			continue
		}
		// 提交：此后一律原样透传（取消推迟到回写完成之后，提前取消会掐断
		// 正在转发的 SSE 长流）。
		defer cancel()
		s.commitResponse(w, r, resp, rw, openAIErrorStyle)
		return
	}
}

// writeAgentTokenError 把「取不到可用令牌」翻成对客户端的应答。
//
// 两类失败两种反应（决策 5 在数据面这一侧的落点）：确定性拒绝是**状态**，
// 回 4xx 并指向管理员重新登录；出网失败是**故障**，回 502 并说清是连不上，
// 客户端重试即可（决策 9：受限现场要给明确报错，不静默挂死）。
func (s *Server) writeAgentTokenError(w http.ResponseWriter, r *http.Request, provider string, err error) {
	info := infoFrom(r.Context())
	switch {
	case r.Context().Err() != nil:
		// 客户端在等刷新的队列里走了：响应无处可写，只记一条。
		s.log.Info("客户端断开，Agents 取令牌已取消", "request_id", info.id)
	case errors.Is(err, agentauth.ErrAuthExpired):
		s.log.Warn("订阅登录已失效，需管理员重新登录",
			"request_id", info.id, "provider", provider, "err", err.Error())
		writeAgentAuthExpired(w, provider)
	default:
		// codexauth/grokauth 的错误只含阶段名与 HTTP 状态/错误码，不含令牌（§15.1）。
		s.log.Warn("刷新订阅凭据失败", "request_id", info.id, "provider", provider, "err", err.Error())
		issuer := "OpenAI"
		if provider == store.AgentProviderGrok {
			issuer = "xAI"
		}
		openAIErrorStyle(w, http.StatusBadGateway, "upstream_unreachable",
			"The gateway failed to reach "+issuer+" to refresh the "+agentDisplayName(provider)+
				" subscription credential. Check the device's outbound connectivity and retry.")
	}
}

// writeAgentNotConfigured / writeAgentAuthExpired 是这条路仅有的两个机读原因。
//
// **都是 409 而不是 5xx**：这台设备没连订阅、或者那份订阅要重新登录，都是
// 设备的**状态**而不是故障——回 5xx 会让客户端与现场把它当成网关坏了去查网络
// 也不是 401：客户端自己的 API密钥没有任何问题，说成 401 只会让它去换 Key。
func writeAgentNotConfigured(w http.ResponseWriter, provider string) {
	openAIErrorStyle(w, http.StatusConflict, "agent_not_configured",
		"No "+agentDisplayName(provider)+" subscription is connected on this device. "+
			"Ask the administrator to connect one on the Agents accounts page.")
}

func writeAgentAuthExpired(w http.ResponseWriter, provider string) {
	openAIErrorStyle(w, http.StatusConflict, "agent_auth_expired",
		"The "+agentDisplayName(provider)+" subscription connected on this device needs to be signed in again. "+
			"Ask the administrator to reconnect it on the Agents accounts page.")
}

// buildAgentRequest 装配发往订阅后端的请求：客户端业务头照转发规则复制
// （逐跳头、客户端凭证、Accept-Encoding 都已剥掉，见 copyForwardHeaders），
// 再注入订阅令牌与该 provider 的身份头。
//
// CLI 自带的身份头（codex 的 originator / session_id、grok 的
// x-grok-client-version 等）就在「其余业务头」里原样过去——它们是那个后端
// 认识客户端的依据，逐个列举白名单只会在 CLI 加一个头的那天悄悄坏掉
// （决策 6 的透传口径）。
func buildAgentRequest(ctx context.Context, r *http.Request,
	provider, method, target, token, accountID string, body []byte) (*http.Request, error) {

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+token)
	// 客户端的 actor 头只是它自己内核的画图开关（codexActorAuthorizationHeader），
	// 不许紧挨着设备的订阅令牌一起到任何订阅后端。
	req.Header.Del(codexActorAuthorizationHeader)
	switch provider {
	case store.AgentProviderGrok:
		// **先无条件删掉，再注入设备侧的值**：X-XAI-Token-Auth 是「这个 Bearer
		// 是用户 OAuth 令牌」的 wire 标记，语义钉在**盒子的**订阅令牌上；客户端
		// 自带的那份不许跟着盒子的令牌进后端（同 codex 账号头的纪律）。
		// 刻意不进 copyForwardHeaders，理由同下。
		req.Header.Del(grokTokenAuthHeader)
		req.Header.Set(grokTokenAuthHeader, grokTokenAuthValue)
	default:
		// **先无条件删掉，再按需注入**：上面复制的是客户端的全部业务头，客户端
		// 自己也可以发一个 ChatGPT-Account-ID。只做条件 Set 的话，accountID 为空
		// 那条路（句柄没有 account_id、id_token 也没带 claim——决策 4 那半段至今
		// 未验，粘贴 auth.json 兜底更是常态）会让**客户端那份**紧挨着管理员的订阅
		// 令牌一起到后端：任何客户端 API Key 持有者因此能挑这份订阅令牌去访问哪个
		// ChatGPT 账号/工作区。设备自己的值是唯一能到达后端的值，空则一个都不发
		// （codexAccountHeader 的注释说的就是这条，代码必须与它一致）。
		//
		// 删除放在这里而不是 copyForwardHeaders：那是三个选路入口共用的透传核心，
		// 其口径是「其余业务头一律原样过」，而这两个头只对各自的订阅后端有意义、
		// 也只有这一处注入设备侧的身份——把它们写进共用函数，等于让别的入口
		// 为一条与它们无关的规则少转发头。
		req.Header.Del(codexAccountHeader)
		if accountID != "" {
			req.Header.Set(codexAccountHeader, accountID)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// rewriteResponsesModel 把 Responses 响应里的 model 改回客户端请求的名字。
//
// 位置与 chat 不同，这是本入口需要自己一份回写函数的**唯一**原因：
//
//	非流式  顶层就是那个 response 对象 → model 在顶层；
//	SSE     事件是 {"type":"response.created","response":{…,"model":…}} →
//	        model 在 response 里（created/in_progress/completed/failed 等
//	        带 response 快照的事件都有一份）。
//
// 两处都只改**已存在**的字段（setModelField），别的一概不碰：增量事件、
// 推理摘要、未知事件类型原样透传。
func rewriteResponsesModel(payload map[string]any, requested string) bool {
	changed := setModelField(payload, requested)
	if rsp, ok := payload["response"].(map[string]any); ok {
		changed = setModelField(rsp, requested) || changed
	}
	return changed
}
