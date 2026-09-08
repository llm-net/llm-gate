package codexhelper

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidCLIPath(t *testing.T) {
	ok := []string{
		"install.sh", "install.ps1", "channels/latest",
		"releases/0.149.0/release.json",
		"releases/0.149.0/codex-package_SHA256SUMS",
		"releases/0.149.0/codex-package-x86_64-unknown-linux-musl.tar.gz",
		"releases/0.149.0/codex-package-x86_64-pc-windows-msvc.zip",
		"releases/0.149.0/codex-npm-linux-x64-0.149.0.tgz",
		"releases/0.150.0-alpha.1.2/release.json",
	}
	for _, path := range ok {
		if !ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = false，期望 true", path)
		}
	}
	bad := []string{
		"", "latest", "../channels/latest", "channels/beta",
		"releases/latest/release.json", "releases/0.149/release.json",
		"releases/0.149.0/../../etc/passwd", "releases/0.149.0/random.bin",
		"releases/0.149.0/codex-package-x.tar.gz?x=1",
		"https://evil.example/install.sh", strings.Repeat("a", 241),
	}
	for _, path := range bad {
		if ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = true，期望 false", path)
		}
	}
}

func testCLIProxy(t *testing.T, installer, releases http.HandlerFunc) *CLIProxy {
	t.Helper()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	var installerURL, releasesURL string
	if installer != nil {
		s := httptest.NewServer(installer)
		t.Cleanup(s.Close)
		installerURL = s.URL
	}
	if releases != nil {
		s := httptest.NewServer(releases)
		t.Cleanup(s.Close)
		releasesURL = s.URL
	}
	p.SetBases(installerURL, releasesURL)
	return p
}

func cliReq(t *testing.T, p *CLIProxy, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/codex-helper/cli/"+path, nil)
	r.SetPathValue("path", path)
	p.ServeHTTP(rec, r)
	return rec
}

func TestCLIProxyUsesInstallerAndReleaseOrigins(t *testing.T) {
	var installerPath, releasePath string
	p := testCLIProxy(t,
		func(w http.ResponseWriter, r *http.Request) {
			installerPath = r.URL.Path
			w.Header().Set("Set-Cookie", "should-not-leak")
			_, _ = io.WriteString(w, "official installer")
		},
		func(w http.ResponseWriter, r *http.Request) {
			releasePath = r.URL.Path
			_, _ = io.WriteString(w, `{"tag_name":"rust-v0.149.0"}`)
		},
	)
	installer := cliReq(t, p, http.MethodGet, "install.sh")
	if installer.Code != http.StatusOK || installer.Body.String() != "official installer" {
		t.Fatalf("installer = %d %q", installer.Code, installer.Body.String())
	}
	if installerPath != "/install.sh" || installer.Header().Get("Set-Cookie") != "" {
		t.Fatalf("installer path/header = %q / %q", installerPath, installer.Header().Get("Set-Cookie"))
	}
	release := cliReq(t, p, http.MethodGet, "releases/0.149.0/release.json")
	if release.Code != http.StatusOK || releasePath != "/releases/0.149.0/release.json" {
		t.Fatalf("release = %d path=%q", release.Code, releasePath)
	}
}

func TestCLIProxyRejectsUnknownPathWithoutDial(t *testing.T) {
	dialed := false
	p := testCLIProxy(t, nil, func(http.ResponseWriter, *http.Request) { dialed = true })
	rec := cliReq(t, p, http.MethodGet, "releases/0.149.0/not-allowed")
	if rec.Code != http.StatusNotFound || dialed {
		t.Fatalf("非法路径 = %d, dialed=%v", rec.Code, dialed)
	}
}

func TestCLIProxyDoesNotFollowRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	t.Cleanup(target.Close)
	p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/outside-whitelist", http.StatusFound)
	}, nil)
	rec := cliReq(t, p, http.MethodGet, "install.sh")
	if rec.Code != http.StatusFound || redirected {
		t.Fatalf("重定向 = %d, 白名单外目标已请求=%v", rec.Code, redirected)
	}
}

func TestCLIProxyAllowsOnlyOfficialInstallerRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, name := range []string{"install.sh", "install.ps1"} {
		from := req("https://chatgpt.com/codex/" + name)
		to := req("https://releases.openai.com/codex/" + name)
		if !cliAllowedRedirect(to, []*http.Request{from}) {
			t.Errorf("官方 %s 精确跳转应放行", name)
		}
	}
	bad := []struct {
		from string
		to   string
	}{
		{"https://chatgpt.com/codex/install.sh", "https://evil.example/codex/install.sh"},
		{"https://chatgpt.com/codex/install.sh", "https://releases.openai.com/codex/release.json"},
		{"http://chatgpt.com/codex/install.sh", "https://releases.openai.com/codex/install.sh"},
		{"https://chatgpt.com/codex/install.sh", "https://releases.openai.com/codex/install.sh?x=1"},
	}
	for _, tc := range bad {
		if cliAllowedRedirect(req(tc.to), []*http.Request{req(tc.from)}) {
			t.Errorf("非白名单跳转被放行：%s -> %s", tc.from, tc.to)
		}
	}
}

func TestCLIProxyRangeHeadAndBodyLogging(t *testing.T) {
	var gotRange string
	secret := "SECRET-CODEX-BYTES-SHOULD-NOT-BE-LOGGED"
	var logs bytes.Buffer
	p := NewCLIProxy(slog.New(slog.NewTextHandler(&logs, nil)))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, secret)
	}))
	t.Cleanup(up.Close)
	p.SetBases(up.URL, up.URL)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/codex-helper/cli/releases/0.149.0/codex-package_SHA256SUMS", nil)
	r.SetPathValue("path", "releases/0.149.0/codex-package_SHA256SUMS")
	r.Header.Set("Range", "bytes=0-3")
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusPartialContent || gotRange != "bytes=0-3" || rec.Body.String() != secret {
		t.Fatalf("range = %d %q %q", rec.Code, gotRange, rec.Body.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("日志含响应正文：%s", logs.String())
	}

	head := cliReq(t, p, http.MethodHead, "channels/latest")
	if head.Code != http.StatusPartialContent || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %q", head.Code, head.Body.String())
	}
}

func TestCLIProxyRejectsOversizedContentLength(t *testing.T) {
	p := testCLIProxy(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write([]byte("tiny"))
	})
	rec := cliReq(t, p, http.MethodGet, "channels/latest")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "tiny") {
		t.Fatalf("超限响应 = %d %q", rec.Code, rec.Body.String())
	}
}
