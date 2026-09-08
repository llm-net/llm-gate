package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/auth"
	"github.com/llm-net/llm-gate/firmware/internal/i18n"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// reqInfo 由最外层中间件放入 context，访问日志统一读取。同一请求单 goroutine
// 读写，无并发问题（与数据面同构）。
//
// 它只剩 request-id 一个字段：设备只有一个管理员，「是谁在操作」恒等于同一个
// 答案，往每行访问日志里塞一次没有信息量。
type reqInfo struct {
	id string // request-id，已回写响应头 X-Request-Id
}

type reqInfoKey struct{}

func infoFrom(ctx context.Context) *reqInfo {
	if info, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return info
	}
	return &reqInfo{}
}

// withRequestID 生成 request-id、回写响应头并放入 context（链路最外层，
// 保证 401/404/500 等短路响应也带 id）。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &reqInfo{id: newRequestID()}
		w.Header().Set("X-Request-Id", info.id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
	})
}

func newRequestID() string {
	var b [8]byte
	rand.Read(b[:]) // 自 Go 1.24 起 crypto/rand.Read 保证不失败
	return hex.EncodeToString(b[:])
}

// withAccessLog 记录访问日志。§15.1 硬规则：body 只经 logging.BodyMeter
// 记长度/哈希前缀/content-type，绝不记内容（密码、初始化码、Key 明文都在
// body 里）；只记 URL path 不记 query。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqMeter := logging.NewBodyMeter()
		r.Body = &meteredBody{rc: r.Body, m: reqMeter}
		rec := &responseRecorder{
			ResponseWriter: w,
			meter:          logging.NewBodyMeter(),
			lang:           i18n.Negotiate(r.Header.Get("Accept-Language")),
		}

		next.ServeHTTP(rec, r)

		s.log.Info("access",
			"request_id", infoFrom(r.Context()).id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status(),
			"duration_ms", float64(time.Since(start).Microseconds())/1e3,
			slog.Group("req", reqMeter.Attr(r.Header.Get("Content-Type"))),
			slog.Group("resp", rec.meter.Attr(rec.Header().Get("Content-Type"))),
		)
	})
}

type meteredBody struct {
	rc io.ReadCloser
	m  *logging.BodyMeter
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.m.Write(p[:n])
	}
	return n, err
}

func (b *meteredBody) Close() error { return b.rc.Close() }

// responseRecorder 记录状态码并把写出的字节喂给 BodyMeter，顺带记住这次
// 请求协商出的界面语言：writeError / writeJSON 只拿到 w，语言从这里取。
type responseRecorder struct {
	http.ResponseWriter
	meter      *logging.BodyMeter
	statusCode int       // 0 表示尚未写出头
	lang       i18n.Lang // Accept-Language 协商结果；零值等于源语言
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.statusCode == 0 {
		w.statusCode = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK // net/http 首次 Write 隐式发 200
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.meter.Write(p[:n])
	}
	return n, err
}

// Flush 透传给底层 writer；官方 grok CLI 二进制约 160 MB，不刷出会整件攒在
// 盒子里再一次性吐出。
func (w *responseRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿透本包装找到底层 writer。
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseRecorder) status() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

// withRecovery 捕获 handler panic：记脱敏日志（panic 值与栈来自代码缺陷，
// 代码中禁止携带 body/凭证 panic），响应统一 JSON 500。
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && err == http.ErrAbortHandler {
				panic(rec)
			}
			info := infoFrom(r.Context())
			s.log.Error("panic recovered",
				"request_id", info.id,
				"method", r.Method,
				"path", r.URL.Path,
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		}()
		next.ServeHTTP(w, r)
	})
}

// withCSRF 对变更方法强制两道显式防线（设计决策 3，与 SameSite=Strict
// Cookie 共同构成三重简版）：自定义头 X-LlmGate-CSRF: 1（跨站表单与顶层导航
// 无法携带自定义头）+ Content-Type 必须 application/json（排除表单可发的
// 类型）。对无 body 的变更请求（如 logout）同样成立，客户端由 API client
// 统一挂头。
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if r.Header.Get("X-LlmGate-CSRF") != "1" {
				writeError(w, http.StatusForbidden, "csrf_required", "变更请求必须携带 X-LlmGate-CSRF: 1 请求头")
				return
			}
			ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			// 固件包与组件制品上传是管理面仅有的非 JSON 变更请求（裸二进制字节，
			// docs/firmware-update.md）。放行的是**另一个同样表单发不出的**
			// Content-Type，CSRF 头照旧强制——防线一道没少，只换了体裁。
			if r.Method == http.MethodPost && octetStreamPaths[r.URL.Path] {
				if err != nil || ct != "application/octet-stream" {
					writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "二进制上传的 Content-Type 必须为 application/octet-stream")
					return
				}
				break
			}
			if err != nil || ct != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "变更请求的 Content-Type 必须为 application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withSession 对 /admin/v1/* 业务端点做会话认证：Cookie 令牌 → 会话点查 →
// 放行。豁免 login 与非 API 路径。未知 /admin/v1 子路径同样先认证再 404
// （与数据面 /v1 约定一致，不给未认证方探测面）。
//
// 这里**不再有权限判定**：设备只有一个管理员，一条会话就是全部权限。
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "未登录或会话已过期")
			return
		}
		if _, err := s.auth.ValidateSession(r.Context(), c.Value); err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "未登录或会话已过期")
			} else {
				s.internalError(w, r, err)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requiresSession 报告请求是否须持有效会话：/admin/v1/ 下**除白名单外全部需要**。
//
// 设备出厂即带默认登录口令，管理台第一屏是登录页，白名单因此只有两条，且都是
// 登录页自己要用的：登录本身，以及规格铭牌上那个固件版本（version.go；会话
// 之前取不到任何业务读数，那一格只能走免会话的口子）。
//
// **这份白名单是唯一的挂载表**：新增免会话端点只能改这里，不许由某个 handler
// 自己「记得」放行。
func requiresSession(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/admin/v1/") {
		return false
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /admin/v1/login", "GET /admin/v1/version":
		return false
	}
	return true
}

// ---- 统一 JSON 响应 ----

// errorBody 是统一 JSON 错误体 {"error":{"code","message"}}（message zh-CN）。
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// langOf 取这次请求协商出的界面语言：沿 Unwrap 找到链上的 responseRecorder。
// 找不到（测试里直接给 httptest.ResponseRecorder）就是源语言，不翻。
func langOf(w http.ResponseWriter) i18n.Lang {
	for w != nil {
		if rec, ok := w.(*responseRecorder); ok {
			return rec.lang
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	return i18n.Source
}

// writeError 写出统一错误体；响应已开始写出时不再追加（与数据面同规则）。
// message 是中文原文，按 Accept-Language 经 internal/i18n 本地化后写出。
func writeError(w http.ResponseWriter, status int, code, message string) {
	if rec, ok := w.(*responseRecorder); ok && rec.statusCode != 0 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: i18n.T(langOf(w), message)}})
}

// writeJSON 写出 JSON 成功响应。带 `i18n:"text"` 标记的字段按 Accept-Language
// 本地化（副本，不动调用方的值）；其余字段原样。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(i18n.Localize(langOf(w), v))
}

// decodeJSON 解析请求 body 到 dst：上限 1 MiB，未知字段报错（尽早暴露
// 客户端字段拼写问题）。失败时已写出 400，调用方直接 return。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON 或含未知字段")
		return false
	}
	return true
}

// internalError 记录服务端错误并写统一 500。store/auth 的错误信息只含
// 表名/约束名/操作名，不含凭证与请求内容，可安全落日志。
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("管理接口内部错误",
		"request_id", infoFrom(r.Context()).id,
		"method", r.Method,
		"path", r.URL.Path,
		"error", err.Error(),
	)
	writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
}

// writeAuthError 把 internal/auth 与 store 的哨兵错误映射为统一错误体。
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrLocked):
		// 这把锁是**严格锁**：锁定期内连正确的口令也一起拒。所以必须把等待时长
		// 如实说出来——一句不说时长的「请稍后再试」只会让人对着表单反复点
		// （2026-08-18 走查）。
		w.Header().Set("Retry-After", strconv.Itoa(int(auth.LockDuration.Seconds())))
		writeError(w, http.StatusTooManyRequests, "locked",
			fmt.Sprintf("连续失败次数过多，请等待 %.0f 秒后重试", auth.LockDuration.Seconds()))
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "密码错误")
	case errors.Is(err, auth.ErrPasswordPolicy):
		writeError(w, http.StatusBadRequest, "password_policy",
			fmt.Sprintf("密码长度须为 %d–%d 个字符", auth.PasswordMinRunes, auth.PasswordMaxRunes))
	default:
		s.internalError(w, r, err)
	}
}

// audit 追加管理面变更审计；失败不阻断动作本身但必须可见（与 auth 包同策）。
func (s *Server) audit(ctx context.Context, ev store.AuditEvent) {
	if err := s.st.AppendAudit(ctx, ev); err != nil {
		s.log.Error("审计写入失败", "event", ev.Event, "error", err.Error())
	}
}

// octetStreamPaths 是 withCSRF 放行 application/octet-stream 的变更路径（中间件
// 与路由表共用同一组常量，防两处漂移）。
var octetStreamPaths = map[string]bool{
	FirmwareUploadPath:  true,
	ComponentUploadPath: true,
	MihomoUploadPath:    true,
}

// remoteIP 取审计与登录退避用的客户端 IP：经可信 Tunnel 入口到达的请求用闸门
// 从单值 CF-Connecting-IP 解析出的地址（缺失或非法记 unknown），其余一律取
// 直连对端 IP——LAN listener 不看任何转发头。
func remoteIP(r *http.Request) string {
	if info, ok := tunnelctx.From(r.Context()); ok {
		return info.ClientIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
