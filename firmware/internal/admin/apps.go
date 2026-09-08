package admin

// 公共页「推荐应用」的设备侧：
//
//	GET  /admin/v1/apps   任何角色（会话必需）：只报告官网客户端是否已装配。
//	GET|HEAD /apps/*      无会话：匿名读取官网同路径静态内容，关进沙箱送回浏览器。
//	                      设备不内嵌、不落库、不缓存页面内容。
//
// 三条口径，改动前先读：
//
//  1. **透传路由无会话，是有意的**。沙箱把云上页面放在 opaque origin 里，它自己发的子资源
//     请求（JS/CSS/图片）带不上 SameSite=Strict 的会话 Cookie——要求会话就等于页面永远
//     加载不出样式。而这条路转的是全体设备同一份的公开静态内容，与登录页资源、
//     /gate-helper/install.sh 同性质：只读、无入参、无用户数据。路由只接受
//     GET/HEAD、/apps/ 前缀，并有字节与并发上限。
//  2. **每个透传响应都关进沙箱**（CSP `sandbox` + 只放行本设备 /apps/ 前缀的资源来源 +
//     `frame-ancestors` 只许本设备）。官网静态页是可执行内容，沙箱保证它拿不到
//     管理台会话、调不了 /admin/v1/*、连不到设备以外的任何地方——哪怕官网内容
//     哪天被换掉。放松任何一条 CSP 之前先回去改那份文档。
//  3. **HTML 文档过标记闸**：2xx 的 text/html 整份读入（≤1 MiB）查
//     officialsite.AppsPageMarker，缺席即按「官网没在提供推荐应用页」处置。防的是介绍站
//     根路径的 SPA fallback 把「页面还
//     没发布」变成 200 的介绍站首页——那一页的资源都在 /assets/，透传不出去，会在 iframe 里
//     长成一页裸 HTML。子资源与 304 不查。
//
// §15.1：响应内容不进日志（access log 只记路径 / 状态 / 字节）；上游失败只在状态翻转时各记
// 一条，不因客户反复刷新而刷屏。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// GET /admin/v1/apps 的两态（对外是稳定的机读值，zh-CN 文案在管理台一侧）。
const (
	appsStateReady              = "ready"
	appsStateWebsiteUnavailable = "website_unavailable"
)

// 透传路由自己渲染提示页时的状态（响应头 X-LlmGate-Apps；透传成功恒为 passthrough）。
const (
	appsHintUnavailable = "unavailable" // 没接官网客户端
	appsHintUnreachable = "unreachable" // 连不上官网 / 超时
	appsHintInvalid     = "invalid"     // 官网答了但不是推荐应用页
	appsPassthrough     = "passthrough"
)

const (
	// appsDocMaxBytes 是 HTML 文档的上限：为了查标记要整份读进内存，一页推荐应用的 HTML
	// 不到 2 KiB，1 MiB 是几百倍余量；超了按 invalid 处置。
	appsDocMaxBytes = 1 << 20
	// appsAssetMaxBytes 是其余资源（流式转发）的上限：图片可能大，但一个 16 MiB 以上的
	// 文件不该出现在这一页上。超限即掐断连接。
	appsAssetMaxBytes = 16 << 20
	// appsMaxInflight 是同时在飞的上游请求数：一次开页十来个子资源、浏览器每主机最多 6 条
	// 并发，16 够两三个人同时开页；满了排队等（请求方走了就放弃），不是拒绝。
	appsMaxInflight = 16
	// appsRelayTimeout 是整条透传（含正文传输）的期限。
	appsRelayTimeout = 60 * time.Second
)

// AppsSource 是「推荐应用」页透传的出站来源。生产实现是 *officialsite.Client。
type AppsSource interface {
	OpenApps(ctx context.Context, req officialsite.AppsRequest) (*http.Response, error)
}

// SetAppsSource 注入透传来源。走注入而不是 New 的入参，理由同 SetCatalogSource
// （nil 具体类型塞进接口形参会得到"非 nil 接口装着 nil 指针"）。
func (s *Server) SetAppsSource(src AppsSource) { s.apps = src }

// appsPassState 是透传的进程内状态：并发闸 + 上游失败的翻转记录（日志只在成功↔失败
// 翻转时各记一条）。零值不可用（信号量要 make），由 New 构造。
type appsPassState struct {
	sem chan struct{}

	mu     sync.Mutex
	failed bool
}

func newAppsPassState() appsPassState {
	return appsPassState{sem: make(chan struct{}, appsMaxInflight)}
}

// flip 记下这一次的成败，回答「该不该记日志」（同 catalogAutoState.flip 的口径）。
func (a *appsPassState) flip(failed bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed == failed {
		return false
	}
	a.failed = failed
	return true
}

// appsStatusJSON 是 GET /admin/v1/apps 的响应：只有 state。**没有的东西是契约的一部分**
// ——不带官网地址或其他运行信息（这条端点对 member 开放）。
type appsStatusJSON struct {
	State string `json:"state"`
}

// handleApps 处理 GET /admin/v1/apps（任何角色，会话必需）。零出站、零点读。
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, appsStatusJSON{State: s.appsState()})
}

// appsState 判定官网客户端是否已接线。
// ready 不承诺"连得上"——网络通不通留给 iframe 里那次真正的透传去发现，这里不做探针。
func (s *Server) appsState() string {
	if s.apps == nil {
		return appsStateWebsiteUnavailable
	}
	return appsStateReady
}

// handleAppsPassthrough 处理 GET|HEAD /apps/*（无会话）。
func (s *Server) handleAppsPassthrough(w http.ResponseWriter, r *http.Request) {
	doc := appsIsDocumentPath(r.URL.Path)
	if s.apps == nil {
		s.appsHint(w, r, doc, appsHintUnavailable, http.StatusServiceUnavailable)
		return
	}
	// 并发闸：满了就排队等一个位子，请求方（浏览器）先走了就算了。
	select {
	case s.appsPass.sem <- struct{}{}:
		defer func() { <-s.appsPass.sem }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appsRelayTimeout)
	defer cancel()

	resp, err := s.apps.OpenApps(ctx, officialsite.AppsRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header,
	})
	if err != nil {
		switch {
		case errors.Is(err, officialsite.ErrAppsPath):
			// mux 已经清过路径，走到这里只可能是绕过 mux 的调用；按 404 处置。
			writeError(w, http.StatusNotFound, "not_found", "路径不存在")
		default:
			s.appsNote(fmt.Errorf("连不上 LLM Gate官网: %w", err))
			status := http.StatusBadGateway
			if appsIsTimeout(err) {
				status = http.StatusGatewayTimeout
			}
			s.appsHint(w, r, doc, appsHintUnreachable, status)
		}
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// 浏览器缓存里那份早就过过标记闸；304 一路透回，缓存头照旧。
		s.appsNote(nil)
		s.appsHeaders(w, r, resp.Header, appsPassthrough)
		w.WriteHeader(http.StatusNotModified)
		return
	case resp.StatusCode/100 == 3:
		// 只放行相对且仍在 /apps/ 内的跳转（nginx 对目录路径补斜杠的那种）；
		// 任何绝对地址都不透——那会把浏览器直接送到别处。
		loc := resp.Header.Get("Location")
		if !strings.HasPrefix(loc, officialsite.AppsPath) || strings.HasPrefix(loc, "//") {
			s.appsNote(fmt.Errorf("官网把 %s 跳转到 /apps/ 之外", r.URL.Path))
			s.appsHint(w, r, doc, appsHintInvalid, http.StatusBadGateway)
			return
		}
		s.appsNote(nil)
		s.appsHeaders(w, r, resp.Header, appsPassthrough)
		w.Header().Set("Location", loc)
		w.WriteHeader(resp.StatusCode)
		return
	case resp.StatusCode/100 != 2:
		// 文档不在 = 官网没在提供这一页；子资源的 404 就是 404，原状态空体答回。
		// 翻转日志只看入口文档 /apps/：别的文档路径答 404 只是 404（沙箱里的页面
		// 本来也只走 hash 导航），不该把"云上没发布"这个状态翻来翻去。
		if doc {
			if r.URL.Path == officialsite.AppsPath {
				s.appsNote(fmt.Errorf("官网对 %s 答 HTTP %d", r.URL.Path, resp.StatusCode))
			}
			s.appsHint(w, r, doc, appsHintInvalid, http.StatusBadGateway)
			return
		}
		s.appsEmpty(w, appsHintInvalid, resp.StatusCode)
		return
	}

	// 2xx。HTML 文档过标记闸（HEAD 没有正文可查，按头透传）。
	if appsIsHTML(resp.Header.Get("Content-Type")) && r.Method != http.MethodHead {
		body, err := io.ReadAll(io.LimitReader(resp.Body, appsDocMaxBytes+1))
		if err != nil {
			s.appsNote(fmt.Errorf("读取官网页面中断: %w", err))
			s.appsHint(w, r, doc, appsHintUnreachable, http.StatusBadGateway)
			return
		}
		switch {
		case len(body) > appsDocMaxBytes:
			s.appsNote(fmt.Errorf("官网页面 %s 超过 %d 字节", r.URL.Path, appsDocMaxBytes))
		case !bytes.Contains(body, []byte(officialsite.AppsPageMarker)):
			s.appsNote(fmt.Errorf("官网对 %s 答的 HTML 没有推荐应用页标记（多半是介绍站首页 fallback）", r.URL.Path))
		default:
			s.appsNote(nil)
			s.appsHeaders(w, r, resp.Header, appsPassthrough)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			return
		}
		if doc {
			s.appsHint(w, r, doc, appsHintInvalid, http.StatusBadGateway)
		} else {
			s.appsEmpty(w, appsHintInvalid, http.StatusBadGateway)
		}
		return
	}

	// 其余资源流式转发（上限之内）。
	if resp.ContentLength > appsAssetMaxBytes {
		s.appsNote(fmt.Errorf("官网资源 %s 超过 %d 字节", r.URL.Path, appsAssetMaxBytes))
		s.appsEmpty(w, appsHintInvalid, http.StatusBadGateway)
		return
	}
	s.appsNote(nil)
	s.appsHeaders(w, r, resp.Header, appsPassthrough)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	n, _ := io.Copy(w, io.LimitReader(resp.Body, appsAssetMaxBytes+1))
	if n > appsAssetMaxBytes {
		// 头已经出去了，只能掐断连接：net/http 对 ErrAbortHandler 静默关连接，
		// 浏览器拿到的是一次失败的下载而不是一份被截半的文件。
		panic(http.ErrAbortHandler)
	}
}

// appsForwardedRespHeaders 是上游响应头里唯一会被拷回浏览器的几个：类型、长度与缓存
// 校验头。Set-Cookie、X-Frame-Options、上游自己的 CSP 等一概不过——沙箱与来源策略由
// 设备一手给出（appsHeaders）。
var appsForwardedRespHeaders = [...]string{"Content-Type", "Content-Length", "Cache-Control", "ETag", "Last-Modified"}

// appsHeaders 写出透传响应的头：白名单里的上游头 + 设备加的五个。
func (s *Server) appsHeaders(w http.ResponseWriter, r *http.Request, up http.Header, state string) {
	h := w.Header()
	for _, k := range appsForwardedRespHeaders {
		if v := up.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	appsSecurityHeaders(h, r, state)
	// opaque origin 发出的 <script type="module" crossorigin> / <link crossorigin> 是 CORS
	// 请求（Origin: null），没有这个头浏览器一律拒收；这些是公开静态内容，放开无妨。
	h.Set("Access-Control-Allow-Origin", "*")
}

// appsSecurityHeaders 是透传响应与提示页共用的那几个头：沙箱 CSP、不带 Referer、
// 不嗅探类型、机读状态。
func appsSecurityHeaders(h http.Header, r *http.Request, state string) {
	h.Set("Content-Security-Policy", appsCSP(r))
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-LlmGate-Apps", state)
}

// appsHostRe 圈定能进 CSP 源表达式的 Host 形状（主机名 / IPv4 / 带方括号的 IPv6，可带端口）。
// Host 是请求方自己给的，形状不对就只留 'self'——CSP 头里不能出现请求方写的任意字符串。
var appsHostRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)

// appsCSP 拼透传响应的 CSP。资源来源只放行**本设备**：'self' 与「本次请求的 scheme + Host
// 加 /apps/ 前缀」的并集——两条各补对方的短板：'self' 由浏览器按它看到的地址判定，不受
// 改写 Host 的反代影响，但 sandbox 文档里 'self' 的语义各家浏览器历史上有过
// 分歧；显式来源在 Host 如实传到设备时精确到 /apps/ 前缀。**connect-src 只给显式那条、不给
// 'self'**：宁可让页面里的 fetch 在改写 Host 的反代后面失效（这一页本就不 fetch），也不让
// 沙箱里的脚本打得到 /admin/v1/login 这类无会话端点。frame-ancestors 只许本设备嵌；`sandbox`
// 让文档以 opaque origin 运行——直接打开与嵌在管理台里都一样。
func appsCSP(r *http.Request) string {
	self, prefix := "'self'", ""
	if appsHostRe.MatchString(r.Host) {
		scheme := "http"
		if tunnelctx.Secure(r) {
			scheme = "https"
		}
		origin := scheme + "://" + r.Host
		prefix = origin + officialsite.AppsPath
		self = "'self' " + prefix
	}
	connect := prefix
	if connect == "" {
		connect = "'none'"
	}
	return "default-src 'none'; " +
		"script-src " + self + "; " +
		"style-src " + self + " 'unsafe-inline'; " +
		"img-src " + self + " data:; " +
		"font-src " + self + " data:; " +
		"media-src " + self + "; " +
		"connect-src " + connect + "; " +
		"frame-ancestors 'self'; " +
		"base-uri 'none'; form-action 'none'; " +
		"sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox"
}

// appsIsDocumentPath 判这是不是"文档"请求：目录（/apps/ 或以 / 结尾）或 .html。
// 只有文档请求才渲染提示页；子资源取不到时按状态码空体答回——一张 JS 的 404 不该
// 长出一页 HTML。
func appsIsDocumentPath(p string) bool {
	return p == officialsite.AppsPath || strings.HasSuffix(p, "/") || strings.HasSuffix(p, ".html")
}

// appsIsHTML 判上游 Content-Type 是不是 HTML（媒体类型比对，参数忽略）。
func appsIsHTML(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// appsIsTimeout 判一次出站失败是不是超时（504 与 502 的分界）。
func appsIsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// appsNote 记下这一次透传的成败：只在翻转时各记一条（成功→失败一条、失败→成功一条），
// 不因客户反复刷新而刷屏。err 里只有连接层信息与路径，没有响应内容。
func (s *Server) appsNote(err error) {
	if !s.appsPass.flip(err != nil) {
		return
	}
	if err != nil {
		s.log.Warn("推荐应用页透传失败（官网不是设备的运行依赖，页面只给提示）", "err", err.Error())
		return
	}
	s.log.Info("推荐应用页透传已恢复")
}

// appsEmpty 对子资源答一个空体状态（带机读状态头，no-store）。
func (s *Server) appsEmpty(w http.ResponseWriter, state string, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-LlmGate-Apps", state)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// appsHint 对文档请求渲染设备自己的提示页（子资源仍是空体）。提示页与透传页同一套
// 安全头（同样在沙箱里、同样只许本设备嵌），no-store——它说的是"此刻"。
func (s *Server) appsHint(w http.ResponseWriter, r *http.Request, doc bool, state string, status int) {
	if !doc {
		s.appsEmpty(w, state, status)
		return
	}
	body := appsHintPage(state)
	h := w.Header()
	appsSecurityHeaders(h, r, state)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// appsHintCopy 是提示文案；不出现任何远端地址。
var appsHintCopy = map[string]struct {
	title, body string
	retry       bool
}{
	appsHintUnavailable: {"推荐应用不可用", "此设备未接入 LLM Gate官网读取客户端，暂时无法显示推荐应用。", false},
	appsHintUnreachable: {"暂时连不上官网", "设备此刻无法读取 LLM Gate官网。请检查设备的外网连接，稍后重试。", true},
	appsHintInvalid:     {"官网暂未提供推荐应用页", "设备连上了 LLM Gate官网，但没有取到推荐应用页。请稍后重试；如果持续出现，请联系维护方。", true},
}

// appsHintPage 渲染提示页：极简、无脚本、无外部资源，样式内联（与管理台同一套瓷白 / 石墨蓝
// 调子）。文案全部来自上面的常量表，没有任何请求方输入进入页面。
func appsHintPage(state string) string {
	c, ok := appsHintCopy[state]
	if !ok {
		c = appsHintCopy[appsHintUnreachable]
	}
	retry := ""
	if c.retry {
		retry = `<p class="act"><a href="` + officialsite.AppsPath + `">重试</a></p>`
	}
	return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(c.title) + ` · 推荐应用</title>
<style>
html,body{margin:0;background:#f5f7fb;color:#131f33;font:14px/1.65 system-ui,-apple-system,"PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif}
.wrap{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:32px 20px;box-sizing:border-box}
.card{max-width:520px;background:#fff;border:1px solid #e3e9f2;border-radius:16px;padding:22px 24px;box-shadow:0 1px 2px rgb(16 24 40/.04),0 12px 32px -20px rgb(16 24 40/.12)}
.legend{margin:0 0 6px;font-size:11px;letter-spacing:.16em;text-transform:uppercase;color:#a6b2c6;font-weight:600}
h1{margin:0 0 8px;font-size:17px;font-weight:600;letter-spacing:-.01em}
p{margin:0;color:#64748f}
.act{margin-top:14px}
.act a{display:inline-block;padding:6px 12px;border:1px solid #cbd5e4;border-radius:6px;color:#131f33;text-decoration:none;font-weight:500}
.act a:hover{background:#eef2f8}
</style></head><body><div class="wrap"><div class="card" role="status">
<p class="legend">LLM Gate · 推荐应用</p>
<h1>` + html.EscapeString(c.title) + `</h1>
<p>` + html.EscapeString(c.body) + `</p>
` + retry + `
</div></div></body></html>
`
}
