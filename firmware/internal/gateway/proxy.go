// proxy.go 是 chat / messages / 图片三个同步入口共享的透传核心，分成两段
// （iteration-5 决策 5）：
//
//   - [Server.forward]「发起并取响应」——按优先级逐个候选来源尝试，未向客户端
//     写出任何字节前可切换；
//   - [Server.commitResponse]「提交回写」——状态码/响应头/响应体落到客户端，
//     一旦进入此段就绝不再切换来源。
//
// 上游请求装配（业务头复制 + 逐跳头剥离 + 上游凭证注入）与响应回写（SSE 按
// 响应 Content-Type 前缀判定；已知流式请求还用请求契约兜住上游漏头）都在
// 本文件。流逐行转发并即时 Flush。全程挂在
// r.Context() 上：客户端断开即取消上游请求（§9.4 条 6），且不触发切换。
//
// 故障切换判定只在响应头阶段：连接/传输错误、或 429/5xx 且还有下一个候选时，
// 有界丢弃响应体（≤64KiB 且 ≤2s）后换下一来源重建请求（model 改写为该来源的
// upstream_model_id）；2xx 与非 429 的 4xx 立即提交原样透传；最后一个来源的
// 错误按原契约透传（连接失败 → 502 upstream_unreachable）。
//
// 响应侧唯一的语义改写是把 model 字段改回客户端提交的模型名（iteration-5：
// 响应 model 恒等于请求 model，遮蔽同模型不同来源的上游侧 ID 与上游返回的
// 版本后缀）：非流式 2xx 响应整读后 round-trip 顶层 model；SSE 只对 data: 行
// 尝试 JSON 解析，改写 chat chunk 顶层 model 与 anthropic message_start 的
// message.model。其余内容一概不解析：解析失败或无 model 字段的 body/行原样
// 转发绝不断流，上游 4xx/5xx 错误体整体不改写，[DONE]/ping/空行/注释行不动。
//
// 网关自身错误（装配失败/上游不可达）经 errorStyle 按入口协议成形：
// chat 侧 OpenAI 风格、messages 侧 Anthropic 风格（见 middleware.go）。
//
// 日志遵守 §15.1：本文件绝不把请求/响应 body 或凭证传入 logger，
// body 元数据由访问日志中间件的 BodyMeter 统一记录。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// failoverDiscardLimit 是切换来源前丢弃上游响应体的字节上限：读掉小体量错误体
// 便于连接复用，超出即放弃（连同连接一起关掉）。内容一概不进日志（§15.1）。
//
// failoverDiscardTimeout 是同一动作的时间上限：上游发完响应头就卡住时，
// 单靠字节上限拦不住——读阻塞只受 http.Client 的 overall 超时（缺省 10m）约束，
// 会把一次本该毫秒级的切换拖成分钟级。到点即取消该次尝试的 context，
// 读立即返回。
const (
	failoverDiscardLimit   = 64 << 10
	failoverDiscardTimeout = 2 * time.Second
)

// internalErrorMessage 是网关内部错误的对客文本（两种入口风格共用）。
const internalErrorMessage = "The gateway encountered an internal error."

// hopByHopHeaders 是 RFC 9110 定义与实践中的逐跳头：只对单个 TCP 连接有
// 意义，转发时必须剥离（请求与响应两个方向都适用）。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection", // 非标准但常见的 Proxy-* 逐跳头
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// forwardSpec 是一次选路转发的入口侧参数：入口协议与端点路径决定发往哪个
// 上游端点，body 每来源重建一次（model 改写为该来源的 upstream_model_id），
// model 是客户端提交的模型名（响应 model 回写目标，也用于脱敏日志）。
type forwardSpec struct {
	protocol string
	path     string
	model    string
	body     func(upstreamModelID string) ([]byte, error)
	// bodyFor 在请求体行为还取决于具体平台能力时使用；非 nil 时优先于 body。
	bodyFor func(candidate) ([]byte, error)
	// contentType 覆盖上游请求的 Content-Type；空串沿用客户端原值。
	// 只有 multipart 形提交体用得上：重编码后的 boundary 由设备决定，
	// copyForwardHeaders 抄过来的那份客户端 Content-Type 会指向一个不存在
	// 的分隔串。
	contentType string
	errStyle    errorStyle
	// observe 可选：**已提交的 2xx 响应**里每一个成功解析出的 JSON 对象各回调
	// 一次——非流式是整读的那个 body，SSE 是每条能解析成对象的 data: 行。
	// 拿到的是 model 改写**之前**的解析结果（与上游原字节等价）。
	//
	// 它搭的是 model 回写本来就要做的那一次解析的车（iteration-9 起观察器
	// 要看 SSE 的 usage 帧，逐行再解析一遍纯属重复工作）。非 2xx 与解析不出
	// 对象的载荷不观测：前者是错误体，后者原样透传。
	//
	// 观测是**旁路**：回调不得写响应、不得改载荷（同一个 map 随后要按原样
	// 重序列化回客户端）、不得把载荷或其中任何字段传入 logger（§15.1）。
	observe func(payload map[string]any)
	// commit 可选：覆盖「提交回写」这一段（缺省走 [Server.commitResponse] 的
	// 原样透传）。Responses 目录面（responses_catalog.go）是转换面——上游 chat
	// 响应要合成回 Responses 形——但它的「发起并取响应」（候选顺序、未提交可
	// 切换、有界丢弃）必须与透传入口是同一份，所以差异收在提交段而不是另抄一个
	// 候选循环。约定不变：进入 commit 即视为已提交，实现方负责关闭 resp.Body，
	// 此后任何失败只记日志，绝不再切换来源。设了 commit 时 observe 不生效
	// （转换面自己在解析处观测）。
	commit func(w http.ResponseWriter, r *http.Request, resp *http.Response)
}

// respRewrite 是**已提交的 2xx 响应**里每个解析成功的 JSON 对象的处理规则：
// 先旁路观测（账单事实），再把 model 改回客户端提交的名字。
//
// 之所以把「改哪个位置」做成字段而不是写死：model 字段的位置随入口协议变
// （chat/messages 在顶层与 message.model，Responses 在 response.model），
// 而 SSE 逐行转发那段代码一个字都不该为此复制一份。
type respRewrite struct {
	// model 是客户端提交的模型名（回写目标，也是脱敏日志里的模型维度）。
	model string
	// expectSSE 报告这个已提交响应对应的请求明确带了 stream:true。
	// 绝大多数上游会正确回 text/event-stream；这个提示只用来兜住响应
	// 本身就是 SSE 却漏了 Content-Type 的实现。只对 2xx 生效，错误体仍
	// 按原响应字节透传。
	expectSSE bool
	// rewrite 按入口把载荷里的 model 改成 requested，返回是否真的改了值
	// （没改时调用方保住原字节）。nil 取 [rewriteModelField]。
	rewrite func(payload map[string]any, requested string) bool
	// observe 见 [forwardSpec.observe]：同一份契约，旁路，不得写响应或改载荷。
	observe func(payload map[string]any)
}

// apply 对一个解析成功的载荷跑一遍「观测 + 回写」，返回载荷是否被改动。
func (rw respRewrite) apply(payload map[string]any) bool {
	if rw.observe != nil {
		rw.observe(payload)
	}
	rewrite := rw.rewrite
	if rewrite == nil {
		rewrite = rewriteModelField
	}
	return rewrite(payload, rw.model)
}

// forward 按优先级逐个候选来源尝试并提交第一个可提交的响应（决策 5）：
// 连接/传输错误或 429/5xx 且还有下一个候选时切换（有界丢弃响应体后关闭），
// 其余情况立即提交。客户端断开（ctx 取消）不切换、不回错误体。
// 每来源至多尝试一次——同一来源内的重试留给后续「路由完善」迭代。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, cands []candidate, spec forwardSpec) {
	info := infoFrom(r.Context())
	if len(cands) == 0 { // resolveRoute 保证 routeOK 时非空，防御分支
		s.log.Error("选路候选为空", "request_id", info.id, "model", spec.model)
		spec.errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}

	for i, c := range cands {
		last := i == len(cands)-1
		info.attempts, info.upstream = i+1, c.account.Name
		// 计价形态随上游产品类型（视频/图片两条路都要），不进任何存储列。
		info.bill.upstreamType = c.account.Type

		// 每次尝试挂在自己的可取消 context 上（父仍是 r.Context()，客户端断开
		// 照样传播）：切换时用它给"丢弃响应体"设时间上限。
		attemptCtx, cancel := context.WithCancel(r.Context())
		req, ok := s.buildAttempt(attemptCtx, w, r, c, spec)
		if !ok {
			cancel()
			return // 内部错误已写出（装配失败不是可切换故障）
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				// 客户端已断开：上游请求已随 context 取消，错误体无处可回，
				// 也绝不因此切换来源。
				s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", spec.model)
				return
			}
			// 传输层错误文本只含地址/超时信息，不含 body 与凭证。
			if !last {
				s.log.Warn("上游连接失败，切换下一来源",
					"request_id", info.id, "model", spec.model,
					"upstream", c.account.Name, "attempt", i+1, "err", err.Error())
				continue
			}
			s.log.Warn("上游请求失败", "request_id", info.id, "model", spec.model,
				"upstream", c.account.Name, "attempt", i+1, "err", err.Error())
			spec.errStyle(w, http.StatusBadGateway, "upstream_unreachable",
				"The gateway failed to reach the upstream provider.")
			return
		}
		if !last && shouldFailover(resp.StatusCode) {
			discardBody(resp, cancel)
			s.log.Warn("上游返回可切换状态码，切换下一来源",
				"request_id", info.id, "model", spec.model,
				"upstream", c.account.Name, "attempt", i+1, "upstream_status", resp.StatusCode)
			continue
		}
		// 提交：此后绝不再切换来源。取消推迟到回写完成之后（提前取消会掐断
		// 正在转发的 SSE 长流）。
		defer cancel()
		if spec.commit != nil {
			spec.commit(w, r, resp)
			return
		}
		s.commitResponse(w, r, resp,
			respRewrite{model: spec.model, observe: spec.observe}, spec.errStyle)
		return
	}
}

// buildAttempt 装配发往某个候选来源的上游请求：取端点、按该来源的
// upstream_model_id 重建请求体、复制业务头并注入上游凭证。请求挂在 ctx 上
// （调用方每次尝试给一个可取消的子 context）。装配失败是网关内部错误
// （不是可切换的上游故障）：就地写出 500 并返回 ok=false。
func (s *Server) buildAttempt(ctx context.Context, w http.ResponseWriter, r *http.Request, c candidate, spec forwardSpec) (*http.Request, bool) {
	info := infoFrom(r.Context())
	target, ok := c.account.URL(spec.protocol, spec.path)
	if !ok { // resolveRoute 已按入口协议过滤候选，防御分支
		s.log.Error("候选来源无本入口协议端点",
			"request_id", info.id, "model", spec.model, "source_id", c.sourceID)
		spec.errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, false
	}
	var body []byte
	var err error
	if spec.bodyFor != nil {
		body, err = spec.bodyFor(c)
	} else {
		body, err = spec.body(c.modelID)
	}
	if err != nil { // map 源自合法 JSON，理论不可达；防御分支
		s.log.Error("重序列化请求体失败", "request_id", info.id, "err", err.Error())
		spec.errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, false
	}
	req, err := buildUpstreamRequest(ctx, r, c.account, spec.protocol, target, body)
	if err != nil {
		s.log.Error("构造上游请求失败", "request_id", info.id, "err", err.Error())
		spec.errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, false
	}
	if spec.contentType != "" {
		req.Header.Set("Content-Type", spec.contentType)
	}
	return req, true
}

// shouldFailover 判定上游响应是否值得换下一个来源：429（限流）与 5xx
// （上游侧故障）。非 429 的 4xx 是客户端请求本身的问题，换来源也是同样结果，
// 立即原样透传。
func shouldFailover(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// discardBody 在切换来源前读掉并关闭要丢弃的响应体：字节上限
// failoverDiscardLimit，时间上限 failoverDiscardTimeout（到点 cancel 本次尝试的
// context，卡住的读立即返回）。内容不进日志（§15.1）。cancel 必然被调用一次，
// 该次尝试的连接资源随之释放。
func discardBody(resp *http.Response, cancel context.CancelFunc) {
	timer := time.AfterFunc(failoverDiscardTimeout, cancel)
	io.Copy(io.Discard, io.LimitReader(resp.Body, failoverDiscardLimit))
	timer.Stop()
	resp.Body.Close()
	cancel()
}

// commitResponse 把选定来源的响应回写给客户端：上游状态码/响应头（除逐跳头）
// 原样透传，4xx/5xx 错误体不改写；2xx 响应把 model 字段改回客户端请求的模型名
// （非流式整读 round-trip、SSE 逐 data: 行，见文件头），其余内容不解析——
// [DONE]、anthropic 事件帧与未知 finish_reason/stop_reason 原样到达客户端。
// 2xx 载荷解析成功后还先喂给 rw.observe（若有）：入口各自的账单事实观测点
// （图片入口的厂商 usage、文本入口的 token 计量，见 metering.go）。
// 进入本函数即视为「已提交」：此后的任何失败都只记日志，不切换来源。
//
// 它**不依赖选路**（不取候选、不碰上游账户），所以 Agents 代理那条自己拼请求
// 的路（responses.go）复用的正是本函数——SSE 逐行转发与断流处置只此一份。
func (s *Server) commitResponse(w http.ResponseWriter, r *http.Request,
	resp *http.Response, rw respRewrite, errStyle errorStyle) {

	model := rw.model
	info := infoFrom(r.Context())
	defer resp.Body.Close()

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	headerSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	// Codex 订阅后端的成功流式响应可能不带 Content-Type。只信响应
	// 头会把整段 SSE 当成一个非流式 JSON：整体解析失败后，完成事件里
	// 的 usage（含 cached_tokens）也就无法进入计量器。请求明确要求流式时，
	// 2xx 应答因此照 SSE 处理；非 2xx 不受影响，避免把 JSON 错误体当事件流。
	isSSE := headerSSE || (success && rw.expectSSE)
	// 流式与否要在观测开始之前定下来：长流的中间结算只在流上跑（metering.go）。
	info.bill.sse = isSSE

	// 非流式 2xx：整读后把 model 字段改回客户端请求的模型名，按改写后的
	// 字节重算 Content-Length 再回写；解析失败或无 model 字段时原字节透传。
	if success && !isSSE {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", model)
				return
			}
			// 尚未向客户端写出任何字节，仍可返回完整错误体。
			s.log.Warn("读取上游响应失败", "request_id", info.id, "model", model, "err", err.Error())
			errStyle(w, http.StatusBadGateway, "upstream_read_error",
				"The gateway failed to read the upstream response.")
			return
		}
		out := rewriteJSONBody(respBody, rw)
		copyBackHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(out); err != nil {
			s.log.Info("客户端断开，响应回写终止", "request_id", info.id, "model", model)
		}
		return
	}

	// SSE 与非 2xx：状态码/响应头（除逐跳头）原样回写后逐块转发；
	// 4xx/5xx 错误体保真透传不改写。
	copyBackHeaders(w.Header(), resp.Header)
	if isSSE && !headerSSE {
		// 既然按请求契约识别成了流，对客户端也补上标准类型；不改正
		// 确的上游头，只修复缺失/错误的那一份。
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if isSSE {
		// SSE：先刷出响应头，此后逐行读写即时刷出。
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if success {
			copyErr = forwardSSERewriting(&flushWriter{w: w}, resp.Body, rw)
		} else { // SSE 形态的非 2xx（罕见）：不改写，仍逐块即时转发
			_, copyErr = io.Copy(&flushWriter{w: w}, resp.Body)
		}
	} else {
		_, copyErr = io.Copy(w, resp.Body)
	}
	if copyErr != nil {
		// 响应已开始，错误无法再进响应体；记脱敏日志后终止，
		// 连接中断由 net/http 向客户端表达。
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，转发终止（上游请求已取消）",
				"request_id", info.id, "model", model, "sse", isSSE)
		} else {
			s.log.Warn("上游响应中途断流",
				"request_id", info.id, "model", model, "sse", isSSE, "err", copyErr.Error())
		}
	}
}

// rewriteJSONBody 尝试把非流式成功响应体的 model 字段改回客户端请求的
// 模型名（改哪个位置由 rw.rewrite 定）。解析失败（含 JSON 对象之后有尾随内容）
// 或无需改写时返回原字节：model 字段之外一切以字节保真优先，改写只在必要时发生。
// 解析成功时先把载荷交给 rw.observe（改写之前，见 forwardSpec.observe）。
func rewriteJSONBody(body []byte, rw respRewrite) []byte {
	payload, ok := decodeJSONObject(body)
	if !ok {
		return body
	}
	if !rw.apply(payload) {
		return body
	}
	out, err := encodeJSON(payload)
	if err != nil { // map 源自合法 JSON，理论不可达；防御分支
		return body
	}
	return bytes.TrimSuffix(out, []byte("\n"))
}

// forwardSSERewriting 逐行转发 SSE 流：data: 行经 rewriteSSEDataLine 尝试
// model 回写后写出，其余行（event:/id:/注释/空行）原样写出；dst 是
// flushWriter，每行落地即 Flush，逐事件实时性不受改写影响。上游中途断流时
// 已读到的残行（无行尾）仍然转发。
func forwardSSERewriting(dst io.Writer, src io.Reader, rw respRewrite) error {
	br := bufio.NewReader(src)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(rewriteSSEDataLine(line, rw)); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// rewriteSSEDataLine 对单个 SSE 行尝试 model 回写：只处理 data: 行；载荷
// 不是完整 JSON 对象（如 [DONE]）或没有 model/message.model 字段（如 ping、
// content_block_delta）时原行返回，绝不因改写失败断流。改写后保留原行的
// data: 前缀、冒号后空格与行尾终止符（\n 或 \r\n）。
// 解析成功的载荷同样先交给 observe——usage 帧（chat 的末帧、anthropic 的
// message_delta）恰恰是没有 model 字段、不需要改写的那些行。
func rewriteSSEDataLine(line []byte, rw respRewrite) []byte {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return line
	}
	rest := line[len(prefix):]
	end := len(rest)
	for end > 0 && (rest[end-1] == '\n' || rest[end-1] == '\r') {
		end-- // 行尾终止符改写后原样保留
	}
	pad := 0
	if end > 0 && rest[0] == ' ' {
		pad = 1 // SSE 语法：冒号后一个空格是分隔符，不属于载荷
	}
	payload, ok := decodeJSONObject(rest[pad:end])
	if !ok {
		return line
	}
	if !rw.apply(payload) {
		return line
	}
	encoded, err := encodeJSON(payload)
	if err != nil { // map 源自合法 JSON，理论不可达；防御分支
		return line
	}
	encoded = bytes.TrimSuffix(encoded, []byte("\n"))
	out := make([]byte, 0, len(prefix)+pad+len(encoded)+len(rest)-end)
	out = append(out, prefix...)
	out = append(out, rest[:pad]...)
	out = append(out, encoded...)
	out = append(out, rest[end:]...)
	return out
}

// rewriteModelField 把响应载荷的 model 字段改回客户端提交的模型名：顶层
// model（chat 响应与 chunk、messages 非流式响应）与 message_start 事件
// message.model 两个位置；字段不存在时不动。这遮蔽的是来源侧差异——同一模型
// 不同来源的上游侧 ID、以及上游返回的版本后缀（iteration-5 语义：响应 model
// 恒等于请求 model）。返回是否真的改了值——已相同时返回 false，调用方借此
// 保住原字节。
func rewriteModelField(payload map[string]any, requested string) bool {
	changed := setModelField(payload, requested)
	if msg, ok := payload["message"].(map[string]any); ok {
		changed = setModelField(msg, requested) || changed
	}
	return changed
}

// setModelField 把一个对象里已存在的 model 字段改成 requested，返回是否真的
// 改了值。**字段不存在时不新建**：那会给不带 model 的事件帧凭空加一个字段，
// 破坏字节保真。各入口的回写函数（rewriteModelField / rewriteResponsesModel）
// 只负责说出「该改哪几个对象」。
func setModelField(obj map[string]any, requested string) bool {
	v, has := obj["model"]
	if !has {
		return false
	}
	if s, ok := v.(string); ok && s == requested {
		return false
	}
	obj["model"] = requested
	return true
}

// decodeJSONObject 把 data 严格解析为单个 JSON 对象：UseNumber 让数字以原
// 字面量重序列化；对象之后还有非空白内容时按解析失败处理——改写只处理
// 形态明确的载荷，其余原样透传。
func decodeJSONObject(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return m, true
}

// buildUpstreamRequest 装配到上游的 POST 请求：复制客户端业务头（剥逐跳头
// 与客户端凭证）、注入上游凭证、缺省 Content-Type application/json。请求挂在
// ctx 上——它是 r.Context() 的可取消子 context，客户端断开与切换来源时的
// 主动取消都能终止本次尝试。
func buildUpstreamRequest(ctx context.Context, r *http.Request, acct upstream.Account, protocol, target string, body []byte) (*http.Request, error) {
	// 出站方式覆盖随 ctx 走：本次尝试固定按这个账号的 egress_mode 选路。
	req, err := http.NewRequestWithContext(acct.EgressContext(ctx), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(req.Header, r.Header)
	acct.Authorize(req.Header, protocol) // 鉴权头重写为上游 Key
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// encodeJSON 序列化改写后的请求体。关闭 HTML 转义，让 <>& 保持原字符，
// 尽量贴近客户端原始字节（尾部多出的换行对 JSON body 无影响）。
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copyForwardHeaders 把客户端请求头复制为上游请求头：剥离逐跳头与
// Connection 点名的头，去掉客户端凭证与随 body 重算的长度头；其余业务头
// （含 anthropic-*）原样透传。Host 不在 Header 中，由上游 URL 决定。
func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	stripHopByHop(dst, src)
	// 客户端凭证不得外传；上游凭证由 Account.Authorize 注入（此处删除是
	// 纵深防御，防止未来调用方漏掉 Authorize 时把客户端 Key 透给上游）。
	dst.Del("Authorization")
	dst.Del("x-api-key")
	// body 已重序列化，长度由 http.Client 按新 body 重新设置。
	dst.Del("Content-Length")
	// 100-continue 握手已由网关侧 net/http 完成、body 已整读，不再外传。
	dst.Del("Expect")
	// 响应 model 回写要求网关能读懂响应体：客户端的 Accept-Encoding
	// 不外传，改由 Go Transport 自协商 gzip 并透明解压——上游侧仍享受压缩，
	// 网关侧始终拿到明文字节；否则客户端协商出的 br/zstd 等编码会让 model
	// 改写静默失效，真实上游模型 ID 随压缩体漏给客户端。
	dst.Del("Accept-Encoding")
}

// copyBackHeaders 把上游响应头复制回客户端响应：剥离逐跳头与
// Connection 点名的头，其余（含未知响应头）原样透传。
func copyBackHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	stripHopByHop(dst, src)
}

// stripHopByHop 从 dst 删除固定逐跳头，以及 src 的 Connection 头点名的字段
// （RFC 9110 §7.6.1：Connection 列出的头也是逐跳头）。
func stripHopByHop(dst, src http.Header) {
	for _, h := range hopByHopHeaders {
		dst.Del(h)
	}
	for _, v := range src.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				dst.Del(name)
			}
		}
	}
}

// flushWriter 在每次写出后立即 Flush，把上游到达的每个数据块实时推给客户端。
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		if fl, ok := f.w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	return n, err
}
