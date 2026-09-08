// apps_test.go 钉住公共页「推荐应用」设备侧的契约（docs/firmware-recommended-apps.md）：
// 状态端点 GET /admin/v1/apps 的作用域与三态（响应里除了 state 什么都没有），以及
// /apps/* 透传路由——无会话可达、只转出去三个请求头、响应关进沙箱（CSP / CORS /
// 不带 Referer / 上游的 Set-Cookie 与 X-Frame-Options 不过）、304 与 /apps/ 内的
// 3xx 原样透回、HTML 文档过标记闸（官网首页 fallback 必须被拦下）、
// 官网不可达时给提示页、子资源失败按状态空体、上游失败日志只在翻转时记一条。
package admin_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
)

const appsStatusPath = "/admin/v1/apps"

// appsPage 是桩官网上的合规推荐应用页（带标记）。
const appsPage = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="llmgate-apps" content="1"><title>推荐应用</title>
<script type="module" crossorigin src="/apps/assets/index-abc.js"></script>
<link rel="stylesheet" crossorigin href="/apps/assets/index-abc.css"></head><body><div id="root"></div></body></html>`

// introPage 是介绍站首页——SPA fallback 会把它当 200 答给任何未知路径；没有标记。
const introPage = `<!doctype html><html><head><title>SOC AGENT by ChenYu</title>
<script type="module" crossorigin src="/assets/index-xyz.js"></script></head><body><div id="root"></div></body></html>`

// appsWebsite 是推荐应用页的桩官网：记住每个请求的头，按路径给不同的响应。
type appsWebsite struct {
	ts *httptest.Server
	mu sync.Mutex
	// reqs 是收到的请求（方法 + 路径 + 头）。
	reqs []*http.Request
	// pages 是路径 → 响应体；缺席可按 SPA fallback 答 introPage。
	pages map[string]string
	// etag 非空时对文档做条件 GET（If-None-Match 相符即 304）。
	etag string
	// spa 为真时未知路径答 200 introPage（介绍站根路径的 fallback）；否则 404。
	spa bool
	// redirectTo 非空时对 /apps/r 答 301 到它。
	redirectTo string
}

func (c *appsWebsite) handle(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r.Clone(context.Background()))
	page, ok := c.pages[r.URL.Path]
	etag, spa, redirectTo := c.etag, c.spa, c.redirectTo
	c.mu.Unlock()

	if r.URL.Path == "/apps/r" && redirectTo != "" {
		w.Header().Set("Location", redirectTo)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	// 上游总想塞进来的东西：Cookie 与 XFO 都不该透到浏览器。
	w.Header().Set("Set-Cookie", "website_session=abc; Path=/")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src *")
	if !ok {
		if spa {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(introPage))
			return
		}
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, ".js"):
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "public, immutable")
	case strings.HasSuffix(r.URL.Path, ".css"):
		w.Header().Set("Content-Type", "text/css")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	_, _ = w.Write([]byte(page))
}

func (c *appsWebsite) requests() []*http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*http.Request(nil), c.reqs...)
}

// serveApps 起一个推荐应用页的桩官网并把它注入管理面。
func (e *env) serveApps(spa bool) *appsWebsite {
	e.t.Helper()
	c := &appsWebsite{
		pages: map[string]string{
			"/apps/":                     appsPage,
			"/apps/assets/index-abc.js":  "console.log('apps')",
			"/apps/assets/index-abc.css": "body{}",
		},
		spa: spa,
	}
	c.ts = httptest.NewServer(http.HandlerFunc(c.handle))
	e.t.Cleanup(c.ts.Close)
	client := officialsite.NewClient(c.ts.URL, nil)
	e.srv.SetAppsSource(client)
	return c
}

// appsStatus 读一次状态端点。
func (e *env) appsStatus(cookie string) string {
	e.t.Helper()
	resp := e.do("GET", appsStatusPath, cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out struct {
		State string `json:"state"`
	}
	raw := readAll(e.t, resp)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		e.t.Fatalf("解析响应 %q: %v", raw, err)
	}
	// 响应里除了 state 什么都没有：官网地址与其他运行配置都不返回。
	if strings.Contains(raw, "http") || strings.Contains(raw, "enabled") || strings.Contains(raw, "credential") {
		e.t.Errorf("状态端点泄了不该出门的东西：%s", raw)
	}
	return out.State
}

// passthrough 打一次透传路由（无 cookie）。
func (e *env) passthrough(method, path string, hdr http.Header) *http.Response {
	e.t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Result()
}

// TestAppsStatusScope：状态端点要会话，未登录 401；三态各归其位。
func TestAppsStatusScope(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 没接官网客户端（测试缺省）：unavailable。
	if got := e.appsStatus(root); got != "website_unavailable" {
		t.Errorf("未接官网客户端时 state = %q，期望 website_unavailable", got)
	}
	resp := e.do("GET", appsStatusPath, "", "")
	wantStatus(t, resp, http.StatusUnauthorized)

	e.serveApps(false)
	if got := e.appsStatus(root); got != "ready" {
		t.Errorf("官网客户端已装配时 state = %q，期望 ready", got)
	}
}

// TestAppsPassthroughRelaysPage：文档与子资源都原样透回，且响应被关进沙箱：CSP 只放行
// 本设备 /apps/ 前缀 + sandbox、CORS 放开、不带 Referer、上游的 Set-Cookie / XFO / CSP 不过。
func TestAppsPassthroughRelaysPage(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(true)

	resp := e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps/ = %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	if body != appsPage {
		t.Errorf("正文没有原样透回:\n%s", body)
	}
	h := resp.Header
	if got := h.Get("X-LlmGate-Apps"); got != "passthrough" {
		t.Errorf("X-LlmGate-Apps = %q", got)
	}
	csp := h.Get("Content-Security-Policy")
	// httptest 的缺省 Host 是 example.com、明文：来源表达式必须是它加 /apps/。
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self' http://example.com/apps/;",
		"style-src 'self' http://example.com/apps/ 'unsafe-inline'",
		// connect-src 只给显式那条：沙箱里的脚本不该打得到 /admin/v1/login 这类无会话端点。
		"connect-src http://example.com/apps/;",
		"frame-ancestors 'self';",
		"sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP 缺 %q：%s", want, csp)
		}
	}
	if strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "*") || strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP 放松了沙箱：%s", csp)
	}
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q", h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("安全头不全：%v", h)
	}
	if h.Get("Set-Cookie") != "" || h.Get("X-Frame-Options") != "" {
		t.Errorf("上游的 Set-Cookie / X-Frame-Options 透过来了：%v", h)
	}
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Cache-Control") != "no-cache" {
		t.Errorf("类型 / 缓存头没透回：%v", h)
	}

	// 子资源：类型与缓存头透回，同一套安全头。
	resp = e.passthrough("GET", "/apps/assets/index-abc.js", nil)
	if resp.StatusCode != http.StatusOK || readAll(t, resp) != "console.log('apps')" {
		t.Fatalf("子资源没透回：%d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "text/javascript" || resp.Header.Get("Cache-Control") != "public, immutable" {
		t.Errorf("子资源头：%v", resp.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("子资源少了安全头：%v", resp.Header)
	}

	// HEAD 也走同一条路（GET 模式在 Go 1.22 mux 里兼收 HEAD）。
	resp = e.passthrough("HEAD", "/apps/", nil)
	if resp.StatusCode != http.StatusOK || readAll(t, resp) != "" {
		t.Errorf("HEAD /apps/ = %d，正文长度 %d", resp.StatusCode, resp.ContentLength)
	}

	// 上游只收到设备愿意转的东西：路径原样、没有 Cookie / Referer / 浏览器 UA。
	reqs := site.requests()
	if len(reqs) != 3 {
		t.Fatalf("上游收到 %d 个请求，期望 3", len(reqs))
	}
	if reqs[0].URL.Path != "/apps/" || reqs[1].URL.Path != "/apps/assets/index-abc.js" || reqs[2].Method != http.MethodHead {
		t.Errorf("上游收到的请求不对：%s %s / %s %s / %s", reqs[0].Method, reqs[0].URL.Path, reqs[1].Method, reqs[1].URL.Path, reqs[2].Method)
	}
}

// TestAppsPassthroughForwardsOnlyThreeHeaders：浏览器请求头里只有 Accept /
// If-None-Match / If-Modified-Since 出站；Cookie（管理台会话！）、UA、语言、Referer 一律不带。
func TestAppsPassthroughForwardsOnlyThreeHeaders(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	site := e.serveApps(false)
	hdr := http.Header{}
	hdr.Set("Accept", "text/html")
	hdr.Set("If-None-Match", `"e1"`)
	hdr.Set("If-Modified-Since", "Sat, 15 Aug 2026 00:00:00 GMT")
	hdr.Set("Cookie", "llmgate_admin_session="+root)
	hdr.Set("User-Agent", "Mozilla/5.0 (customer)")
	hdr.Set("Accept-Language", "zh-CN")
	hdr.Set("Referer", "http://example.com/admin/")
	resp := e.passthrough("GET", "/apps/", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
	got := site.requests()[0].Header
	if got.Get("Accept") != "text/html" || got.Get("If-None-Match") != `"e1"` || got.Get("If-Modified-Since") == "" {
		t.Errorf("该转的没转：%v", got)
	}
	for _, k := range []string{"Cookie", "Accept-Language", "Referer"} {
		if got.Get(k) != "" {
			t.Errorf("%s 出站了：%q", k, got.Get(k))
		}
	}
	if strings.Contains(got.Get("User-Agent"), "Mozilla") {
		t.Errorf("浏览器 UA 出站了：%q", got.Get("User-Agent"))
	}
	if strings.Contains(e.buf.String(), root) {
		t.Error("会话令牌进了日志")
	}
}

// TestAppsPassthroughNotModified：浏览器带 If-None-Match、云上没变 → 304 一路透回（带 ETag）。
func TestAppsPassthroughNotModified(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(false)
	site.etag = `"v7"`
	resp := e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != `"v7"` {
		t.Fatalf("首取 = %d etag=%q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	_ = readAll(t, resp)
	hdr := http.Header{}
	hdr.Set("If-None-Match", `"v7"`)
	resp = e.passthrough("GET", "/apps/", hdr)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("条件 GET = %d，期望 304", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != `"v7"` || resp.Header.Get("X-LlmGate-Apps") != "passthrough" {
		t.Errorf("304 的头：%v", resp.Header)
	}
}

// TestAppsPassthroughMarkerGate：介绍站根路径的 SPA fallback 把「页面还没发布」变成 200 的
// 介绍站首页——文档请求必须被标记闸拦下（502 + 提示页 invalid），子资源路径拿到没标记的
// HTML 同样不透（502 空体）；文档 404 也是提示页；子资源 404 按状态空体。
func TestAppsPassthroughMarkerGate(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(true)
	site.mu.Lock()
	delete(site.pages, "/apps/") // 官网还没发布这一页 → fallback 回介绍站首页
	site.mu.Unlock()

	resp := e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-LlmGate-Apps") != "invalid" {
		t.Fatalf("介绍站首页 fallback：status=%d state=%q，期望 502 invalid", resp.StatusCode, resp.Header.Get("X-LlmGate-Apps"))
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "推荐应用") || strings.Contains(body, "SOC AGENT by ChenYu") {
		t.Errorf("提示页不对（要设备自己的提示，不是介绍站首页）：%s", body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("提示页的头：%v", resp.Header)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("提示页也该带沙箱 CSP")
	}

	// 子资源路径拿回没标记的 HTML：不透，空体。
	resp = e.passthrough("GET", "/apps/assets/missing.js", nil)
	if resp.StatusCode != http.StatusBadGateway || readAll(t, resp) != "" {
		t.Errorf("子资源拿到介绍站首页：status=%d，期望 502 空体", resp.StatusCode)
	}

	// 云上不做 fallback（正确配置）：文档 404 → 提示页 invalid；子资源 404 → 404 空体。
	site.mu.Lock()
	site.spa = false
	site.mu.Unlock()
	resp = e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-LlmGate-Apps") != "invalid" {
		t.Errorf("文档 404：status=%d state=%q", resp.StatusCode, resp.Header.Get("X-LlmGate-Apps"))
	}
	_ = readAll(t, resp)
	resp = e.passthrough("GET", "/apps/assets/missing.js", nil)
	if resp.StatusCode != http.StatusNotFound || readAll(t, resp) != "" {
		t.Errorf("子资源 404：status=%d，期望 404 空体", resp.StatusCode)
	}
}

// TestAppsPassthroughRedirects：/apps/ 内的相对跳转原样透回；跳出去的一律不透（提示页 invalid）。
func TestAppsPassthroughRedirects(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(false)

	site.redirectTo = "/apps/r/"
	resp := e.passthrough("GET", "/apps/r", nil)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/apps/r/" {
		t.Errorf("站内跳转：status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("跳转响应透了 Set-Cookie")
	}
	for _, bad := range []string{"https://evil.example/", "//evil.example/apps/", "/admin/", "/appsx/"} {
		site.mu.Lock()
		site.redirectTo = bad
		site.mu.Unlock()
		resp = e.passthrough("GET", "/apps/r", nil)
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
			t.Errorf("跳到 %q：status=%d location=%q，期望 502 且无 Location", bad, resp.StatusCode, resp.Header.Get("Location"))
		}
		_ = readAll(t, resp)
	}
}

// TestAppsPassthroughUnavailable：未接官网客户端时返回本地提示页，且零出站。
func TestAppsPassthroughUnavailable(t *testing.T) {
	e := newEnv(t)
	e.rootSession()

	// 没接云链路。
	resp := e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-LlmGate-Apps") != "unavailable" {
		t.Errorf("未接云链路：status=%d state=%q", resp.StatusCode, resp.Header.Get("X-LlmGate-Apps"))
	}
	if body := readAll(t, resp); !strings.Contains(body, "未接入") {
		t.Errorf("提示页文案：%s", body)
	}

}

// TestAppsPassthroughUnreachable：连不上云 → 提示页 unreachable（502），日志只在翻转时记一条。
func TestAppsPassthroughUnreachable(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	// 一个已关闭的端口：连接拒绝。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	client := officialsite.NewClient("http://"+addr, nil)
	e.srv.SetAppsSource(client)

	for i := 0; i < 3; i++ {
		resp := e.passthrough("GET", "/apps/", nil)
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-LlmGate-Apps") != "unreachable" {
			t.Fatalf("第 %d 次：status=%d state=%q", i+1, resp.StatusCode, resp.Header.Get("X-LlmGate-Apps"))
		}
		body := readAll(t, resp)
		if !strings.Contains(body, "连不上") || !strings.Contains(body, `href="/apps/"`) {
			t.Errorf("提示页要说清连不上并给重试：%s", body)
		}
	}
	if n := strings.Count(e.buf.String(), "推荐应用页透传失败"); n != 1 {
		t.Errorf("三次失败记了 %d 条 Warn，期望翻转时只记 1 条", n)
	}
	// 子资源：空体 502。
	resp := e.passthrough("GET", "/apps/assets/x.js", nil)
	if resp.StatusCode != http.StatusBadGateway || readAll(t, resp) != "" {
		t.Errorf("子资源连不上：status=%d，期望 502 空体", resp.StatusCode)
	}
}

// TestAppsPassthroughOnlyGetHead：变更方法进不来（mux 405），/apps 无斜杠被引到 /apps/。
func TestAppsPassthroughOnlyGetHead(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(false)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		resp := e.passthrough(m, "/apps/", nil)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s /apps/ 不该成功", m)
		}
		_ = readAll(t, resp)
	}
	if n := len(site.requests()); n != 0 {
		t.Errorf("变更方法出站了 %d 次", n)
	}
	resp := e.passthrough("GET", "/apps", nil)
	if resp.StatusCode/100 != 3 || resp.Header.Get("Location") != "/apps/" {
		t.Errorf("/apps → status=%d location=%q，期望引到 /apps/", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestAppsPassthroughDocTooLarge：HTML 文档超过 1 MiB 按 invalid 处置（要整份读进内存查标记，
// 上限是硬的）。
func TestAppsPassthroughDocTooLarge(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	site := e.serveApps(false)
	site.mu.Lock()
	site.pages["/apps/"] = `<html><meta name="llmgate-apps" content="1">` + strings.Repeat("x", 1<<20) + "</html>"
	site.mu.Unlock()
	resp := e.passthrough("GET", "/apps/", nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-LlmGate-Apps") != "invalid" {
		t.Errorf("超大文档：status=%d state=%q", resp.StatusCode, resp.Header.Get("X-LlmGate-Apps"))
	}
	_ = readAll(t, resp)
}
