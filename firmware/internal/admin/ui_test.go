// ui_test.go 覆盖挂在管理面链上的界面路由：/ → 302 /ui/、/admin/ 是旧书签的
// 客户端跳转页、界面子树 /ui/（internal/ui）无会话可达且未知子路径回退
// index.html、Cache-Control: no-store。
// /admin/v1 命名空间不回退 HTML 的约定由 TestAuthedUnknownPathIs404（认证后
// JSON 404）与 TestUnauthenticatedRejected（未认证 401）共同钉住。
package admin_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServing(t *testing.T) {
	e := newEnv(t)

	// / → 302 /ui/（精确匹配，无需会话）。
	resp := e.do("GET", "/", "", "")
	wantStatus(t, resp, http.StatusFound)
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q，期望 /ui/", loc)
	}

	// /admin/ 是旧书签的**客户端**跳转页：老书签形如 /admin/#/users，fragment
	// 到不了服务端，302 换不出 hash 里那一段，只能把一张小页面送到浏览器里由它
	// 读 location.hash 再换路径。所以这里必须是 200 HTML，不是 302。
	resp = e.do("GET", "/admin/", "", "")
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q，期望 text/html", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "location.hash") || !strings.Contains(body, `"/ui"`) {
		t.Errorf("跳转页缺少读 hash 换 /ui/ 的脚本：%q", body)
	}
	// 旧 hash 归一化表要在页面里（老书签不落到答非所问的页面上）。
	for _, legacy := range []string{"#/assistant", "#/agents", "#/models"} {
		if !strings.Contains(body, legacy) {
			t.Errorf("跳转页缺少旧 hash %q 的归一化", legacy)
		}
	}

	// /admin/v1 是 API 命名空间：静态层一律统一 JSON 404，**绝不回退 HTML**。
	// 未认证方走到这里先被会话中间件挡成 401（TestUnauthenticatedRejected 钉住），
	// 这里只确认它不吐 HTML。
	resp = e.do("GET", "/admin/v1/not-a-real-endpoint", "", "")
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("/admin/v1 未知路径回退了 HTML，Content-Type = %q", ct)
	}
}

// TestAppUIServing 是板上 Agent 产品面静态子树的绊线：/ui/ 无会话可达、单页
// 未知子路径回退 index.html、资源全在 /ui/ 前缀下，且 /admin/ 一点没被这棵
// 子树影响（两份产物各有各的挂载点：产品面 #root、管理台 #app）。
func TestAppUIServing(t *testing.T) {
	e := newEnv(t)

	// /ui/ 未登录可访问：产品面单页 HTML，且不缓存。
	resp := e.do("GET", "/ui/", "", "")
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q，期望 text/html", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
	idx := readAll(t, resp)
	if !strings.Contains(idx, `id="root"`) {
		t.Errorf("index.html 内容缺少挂载点 #root：%q", idx)
	}

	// 子树根少写斜杠由 ServeMux 自动补（浏览器手敲 /ui 的常态）。Go 1.26 起
	// 这个补斜杠跳转是 307 而不是历史上的 301。
	resp = e.do("GET", "/ui", "", "")
	wantStatus(t, resp, http.StatusTemporaryRedirect)
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q，期望 /ui/", loc)
	}

	// 单页直达/刷新：未知子路径回退 index.html，不 404。
	resp = e.do("GET", "/ui/sessions/xyz", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); !strings.Contains(body, `id="root"`) {
		t.Errorf("未知子路径未回退 index.html：%q", body)
	}

	// 页面引用的资源全在 /ui/ 前缀下，且逐个可取（从 index.html 解析真实
	// hash 文件名）。
	for _, marker := range []string{`src="`, `href="`} {
		rest := idx
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				t.Fatalf("index.html 属性没有闭合引号：%q", idx)
			}
			ref := rest[:end]
			rest = rest[end:]
			if !strings.HasPrefix(ref, "/ui/") {
				t.Errorf("index.html 引用了 /ui/ 之外的资源 %q", ref)
				continue
			}
			wantStatus(t, e.do("GET", ref, "", ""), http.StatusOK)
		}
	}

	// /admin/ 现在是旧书签的跳转页，不再是另一份单页产物。
	resp = e.do("GET", "/admin/", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); !strings.Contains(body, "location.hash") {
		t.Errorf("/admin/ 不是旧书签跳转页：%q", body)
	}

	// 界面收在 /ui/ 一个前缀里的**理由**就在这条：根命名空间是厂商 API 兼容面，
	// 兜底必须是 JSON 404（客户端把 base_url 指到设备根，打错路径要拿 JSON 而
	// 不是 HTML）。单页回退只能发生在 /ui/ 子树内，一步都不许漫出去。
	for _, path := range []string{"/no-such-root-path", "/sessions/xyz", "/ui-not-really"} {
		resp = e.do("GET", path, "", "")
		wantStatus(t, resp, http.StatusNotFound)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s 应为统一 JSON 404，Content-Type = %q", path, ct)
		}
		if body := readAll(t, resp); strings.Contains(body, `id="root"`) {
			t.Errorf("%s 回退了产品面单页，界面漫出了 /ui/ 前缀", path)
		}
	}
}

// TestUIAcceptsArbitraryHost 钉住客户自管路由器、反向代理或网关的接入方式：
// 页面路由不设域名白名单，只要请求被转发到设备监听器，任意合法 Host 都能打开
// 同一棵 /ui/。域名解析、TLS 终止与转发规则由客户自己的网络边界负责。
func TestUIAcceptsArbitraryHost(t *testing.T) {
	e := newEnv(t)
	for _, host := range []string{
		"customer-gateway.example",
		"another-domain.example:8443",
	} {
		r := e.req(http.MethodGet, "/ui/", "", "")
		r.Host = host
		resp := e.send(r)
		wantStatus(t, resp, http.StatusOK)
		if body := readAll(t, resp); !strings.Contains(body, `id="root"`) {
			t.Errorf("Host %q 没有打开设备界面：%q", host, body)
		}
	}
}

// 工具专属安装入口不存在；客户端首次安装只使用 /gate-helper/install.*。
func TestToolSpecificInstallRoutesAbsent(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/codex-helper/install.sh", "/codex-helper/install.ps1",
		"/grok-helper/install.sh", "/grok-helper/install.ps1",
		"/claude-helper/install.sh", "/claude-helper/install.ps1",
		"/opencode-helper/install.sh", "/opencode-helper/install.ps1",
		"/cursor-helper/install.sh", "/cursor-helper/install.ps1",
	} {
		resp := e.do(http.MethodGet, path, "", "")
		wantStatus(t, resp, http.StatusNotFound)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s 应为统一 JSON 404，Content-Type = %q", path, ct)
		}
	}
}

// TestCodexHelperCLIPublic 钉住 Codex 官方安装物也只经 Box 的白名单透传：
// 脚本与 release 走各自固定源、非法路径不出站、上游 Cookie 不回客户端。
func TestCodexHelperCLIPublic(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=should-not-leak")
		switch r.URL.Path {
		case "/install.sh":
			_, _ = w.Write([]byte("#!/bin/sh\nRELEASES_BASE_URL=https://releases.openai.com/codex\n"))
		case "/channels/latest":
			_, _ = w.Write([]byte(`{"tag_name":"rust-v0.149.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	e.srv.SetCodexCLIArtifactBases(up.URL, up.URL)

	resp := e.do("GET", "/codex-helper/cli/install.sh", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); !strings.HasPrefix(body, "#!/bin/sh") {
		t.Errorf("install.sh 正文 = %q", body)
	}
	if ck := resp.Header.Get("Set-Cookie"); ck != "" {
		t.Errorf("上游 Set-Cookie 不该透出：%q", ck)
	}

	resp = e.do("GET", "/codex-helper/cli/channels/latest", "", "")
	wantStatus(t, resp, http.StatusOK)
	_ = readAll(t, resp)

	resp = e.do("GET", "/codex-helper/cli/releases/latest/random.bin", "", "")
	wantStatus(t, resp, http.StatusNotFound)
}

// TestGrokHelperCLIPublic 钉住官方 grok CLI 透传：无会话公共 GET、上游
// Set-Cookie 不过、非法名字不出站（404）、与 install.sh 同挂管理面。
func TestGrokHelperCLIPublic(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "cf=should-not-leak")
		if r.URL.Path == "/stable" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("1.0.5\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)
	e.srv.SetCLIArtifactBases(up.URL, "")

	resp := e.do("GET", "/grok-helper/cli/stable", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); body != "1.0.5\n" {
		t.Errorf("stable 正文 = %q", body)
	}
	if ck := resp.Header.Get("Set-Cookie"); ck != "" {
		t.Errorf("上游 Set-Cookie 不该透出：%q", ck)
	}

	resp = e.do("GET", "/grok-helper/cli/not-a-real-artifact", "", "")
	wantStatus(t, resp, http.StatusNotFound)

}

// TestClaudeHelperCLIPublic 钉住 Claude Code 官方 release 也只经 Box 的
// 固定路径白名单透传；工具专属 install.sh 仍不存在。
func TestClaudeHelperCLIPublic(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=should-not-leak")
		if r.URL.Path == "/latest" {
			_, _ = w.Write([]byte("2.1.241\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)
	e.srv.SetClaudeCLIArtifactBase(up.URL)

	resp := e.do(http.MethodGet, "/claude-helper/cli/latest", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); body != "2.1.241\n" {
		t.Errorf("latest 正文 = %q", body)
	}
	if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
		t.Errorf("上游 Set-Cookie 不该透出：%q", cookie)
	}

	resp = e.do(http.MethodGet, "/claude-helper/cli/2.1.241/linux-x64/random", "", "")
	wantStatus(t, resp, http.StatusNotFound)
}

// TestOpenCodeHelperCLIPublic 钉住 OpenCode 官方 release 元数据与归档只经
// Box 的固定路径白名单透传；桌面制品不在 CLI 白名单内。
func TestOpenCodeHelperCLIPublic(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=should-not-leak")
		if r.URL.Path == "/latest" {
			_, _ = w.Write([]byte(`{"tag_name":"v1.18.22","assets":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)
	e.srv.SetOpenCodeCLIArtifactBases(up.URL, up.URL)

	resp := e.do(http.MethodGet, "/opencode-helper/cli/latest", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); !strings.Contains(body, `"tag_name":"v1.18.22"`) {
		t.Errorf("latest body = %q", body)
	}
	if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
		t.Errorf("upstream Set-Cookie leaked: %q", cookie)
	}

	resp = e.do(http.MethodGet, "/opencode-helper/cli/releases/1.18.22/opencode-desktop-linux-amd64.deb", "", "")
	wantStatus(t, resp, http.StatusNotFound)
}

// TestCursorHelperCLIPublic 钉住 Cursor CLI 官方安装物也只经 Box 的固定路径
// 白名单透传：两个脚本共用官方单文件 URL、install.ps1 映射固定 query
// ?win32=true、白名单外的归档路径不出站。
func TestCursorHelperCLIPublic(t *testing.T) {
	e := newEnv(t)
	var lastQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=should-not-leak")
		if r.URL.Path == "/install" {
			lastQuery = r.URL.RawQuery
			_, _ = w.Write([]byte("#!/usr/bin/env bash\nofficial cursor installer\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)
	e.srv.SetCursorCLIArtifactBases(up.URL+"/install", up.URL+"/lab")

	resp := e.do(http.MethodGet, "/cursor-helper/cli/install.sh", "", "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); !strings.Contains(body, "official cursor installer") {
		t.Errorf("install.sh 正文 = %q", body)
	}
	if lastQuery != "" {
		t.Errorf("install.sh 上游 query = %q，应为空", lastQuery)
	}
	if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
		t.Errorf("上游 Set-Cookie 不该透出：%q", cookie)
	}

	resp = e.do(http.MethodGet, "/cursor-helper/cli/install.ps1", "", "")
	wantStatus(t, resp, http.StatusOK)
	_ = readAll(t, resp)
	if lastQuery != "win32=true" {
		t.Errorf("install.ps1 上游 query = %q，应为固定 win32=true", lastQuery)
	}

	resp = e.do(http.MethodGet, "/cursor-helper/cli/lab/2026.08.11-e8db854/linux/x64/random.bin", "", "")
	wantStatus(t, resp, http.StatusNotFound)
}
