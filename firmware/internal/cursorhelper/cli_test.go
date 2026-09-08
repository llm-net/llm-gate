package cursorhelper

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
		"install.sh", "install.ps1",
		"lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/linux/arm64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/darwin/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/darwin/arm64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/windows/x64/agent-cli-package.zip",
		"lab/2026.08.11-e8db854/windows/arm64/agent-cli-package.zip",
		// 版本 hex 段收 5–12 位，两端都要能过。
		"lab/2027.01.02-abcde/linux/x64/agent-cli-package.tar.gz",
		"lab/2027.01.02-0123456789ab/darwin/arm64/agent-cli-package.tar.gz",
	}
	for _, path := range ok {
		if !ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = false，期望 true", path)
		}
	}
	bad := []string{
		"", "install", "install.sh?win32=true", "../install.sh", "lab",
		"lab/2026.08.11/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.8.11-e8db854/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-E8DB854/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-0123456789abc/linux/x64/agent-cli-package.tar.gz",
		"lab/latest/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/freebsd/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/linux/x86/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/linux/x64/agent-cli-package.zip",
		"lab/2026.08.11-e8db854/windows/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/windows/arm64/agent-cli-package.exe",
		"lab/2026.08.11-e8db854/linux/x64/other.tar.gz",
		"lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz/extra",
		"lab/2026.08.11-e8db854/linux/../agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/../../etc/passwd",
		"https://evil.invalid/install.sh",
		strings.Repeat("a", 121),
	}
	for _, path := range bad {
		if ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = true，期望 false", path)
		}
	}
}

// testCLIProxy 起两个假官方源：installer 模拟 cursor.com/install（单文件 URL，
// 基址带 /install 路径），lab 模拟 downloads.cursor.com/lab 归档目录。
func testCLIProxy(t *testing.T, installer, lab http.HandlerFunc) *CLIProxy {
	t.Helper()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	var installerURL, labURL string
	if installer != nil {
		s := httptest.NewServer(installer)
		t.Cleanup(s.Close)
		installerURL = s.URL + "/install"
	}
	if lab != nil {
		s := httptest.NewServer(lab)
		t.Cleanup(s.Close)
		labURL = s.URL + "/lab"
	}
	p.SetBases(installerURL, labURL)
	return p
}

func cliReq(t *testing.T, p *CLIProxy, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/cursor-helper/cli/"+path, nil)
	r.SetPathValue("path", path)
	p.ServeHTTP(rec, r)
	return rec
}

// 两个安装脚本共用同一个官方 URL，靠固定 query 区分：install.sh 无 query、
// install.ps1 恒 ?win32=true；客户端自带的 query 不进上游。响应头只过白名单。
func TestCLIProxyStreamsScriptsWithFixedQueryMapping(t *testing.T) {
	var gotPath, gotQuery string
	p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Set-Cookie", "session=must-not-leak")
		w.Header().Set("ETag", `"fixture"`)
		_, _ = io.WriteString(w, "#!/usr/bin/env bash\nofficial installer\n")
	}, nil)

	rec := cliReq(t, p, http.MethodGet, "install.sh")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "official installer") {
		t.Fatalf("install.sh = %d %q", rec.Code, rec.Body.String())
	}
	if gotPath != "/install" || gotQuery != "" {
		t.Fatalf("install.sh 上游 = %q?%q，应打 /install 且无 query", gotPath, gotQuery)
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("ETag") != `"fixture"` {
		t.Fatalf("响应头白名单被破坏：%#v", rec.Header())
	}

	rec = cliReq(t, p, http.MethodGet, "install.ps1")
	if rec.Code != http.StatusOK || gotPath != "/install" || gotQuery != "win32=true" {
		t.Fatalf("install.ps1 = %d 上游 %q?%q，应映射固定 query win32=true", rec.Code, gotPath, gotQuery)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/cursor-helper/cli/install.sh?win32=true", nil)
	r.SetPathValue("path", "install.sh")
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || gotQuery != "" {
		t.Fatalf("客户端 query 泄漏到上游：%q?%q", gotPath, gotQuery)
	}
}

func TestCLIProxyStreamsAllSixPlatformArchives(t *testing.T) {
	var paths []string
	p := testCLIProxy(t, nil, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.WriteString(w, "archive-bytes")
	})
	for _, rel := range []string{
		"linux/x64/agent-cli-package.tar.gz",
		"linux/arm64/agent-cli-package.tar.gz",
		"darwin/x64/agent-cli-package.tar.gz",
		"darwin/arm64/agent-cli-package.tar.gz",
		"windows/x64/agent-cli-package.zip",
		"windows/arm64/agent-cli-package.zip",
	} {
		rec := cliReq(t, p, http.MethodGet, "lab/2026.08.11-e8db854/"+rel)
		if rec.Code != http.StatusOK || rec.Body.String() != "archive-bytes" {
			t.Fatalf("%s = %d %q", rel, rec.Code, rec.Body.String())
		}
	}
	// 上游路径 = 官方 /lab/<版本>/<os>/<arch>/ 原样，不做任何改写。
	if len(paths) != 6 || paths[0] != "/lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz" {
		t.Fatalf("上游收到 %v", paths)
	}
}

func TestCLIProxyRejectsUnknownPathWithoutDial(t *testing.T) {
	dialed := false
	seen := func(http.ResponseWriter, *http.Request) { dialed = true }
	p := testCLIProxy(t, seen, seen)
	for _, path := range []string{
		"install", "lab/latest/linux/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/freebsd/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/windows/x64/agent-cli-package.tar.gz",
		"lab/2026.08.11-e8db854/linux/../agent-cli-package.tar.gz",
	} {
		rec := cliReq(t, p, http.MethodGet, path)
		if rec.Code != http.StatusNotFound || dialed {
			t.Fatalf("%q = %d dialed=%v，应本地 404 不出网", path, rec.Code, dialed)
		}
	}
}

// 两个官方来源实测都不用 3xx；一律不跟，3xx 按非 2xx 空体透传，跳转目标
// 一个字节都不请求，Location 也不在转发白名单内。
func TestCLIProxyDoesNotFollowRedirect(t *testing.T) {
	dialed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialed = true }))
	t.Cleanup(target.Close)
	p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/outside-whitelist", http.StatusFound)
	}, nil)
	rec := cliReq(t, p, http.MethodGet, "install.sh")
	if rec.Code != http.StatusFound || dialed {
		t.Fatalf("重定向 = %d dialed=%v，一律不跟", rec.Code, dialed)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Location") != "" {
		t.Fatalf("3xx 应空体且不带 Location：%q %q", rec.Body.String(), rec.Header().Get("Location"))
	}
}

func TestCLIProxyNon2xxEmptyBodyNoStore(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Set-Cookie", "err=must-not-leak")
			http.Error(w, "upstream error page", status)
		}, nil)
		rec := cliReq(t, p, http.MethodGet, "install.sh")
		if rec.Code != status || rec.Body.Len() != 0 {
			t.Fatalf("HTTP %d 透传 = %d %q，应保状态空体", status, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("HTTP %d 响应头 = %#v", status, rec.Header())
		}
	}
}

func TestCLIProxyRangeHeadAndOversize(t *testing.T) {
	t.Run("range", func(t *testing.T) {
		var gotRange string
		p := testCLIProxy(t, nil, func(w http.ResponseWriter, r *http.Request) {
			gotRange = r.Header.Get("Range")
			w.Header().Set("Content-Range", "bytes 0-3/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "fake")
		})
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/cursor-helper/cli/lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz", nil)
		r.SetPathValue("path", "lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz")
		r.Header.Set("Range", "bytes=0-3")
		p.ServeHTTP(rec, r)
		if rec.Code != http.StatusPartialContent || gotRange != "bytes=0-3" || rec.Body.String() != "fake" {
			t.Fatalf("range = %d %q %q", rec.Code, gotRange, rec.Body.String())
		}
		if rec.Header().Get("Content-Range") != "bytes 0-3/10" {
			t.Fatalf("Content-Range 未转发：%#v", rec.Header())
		}
	})
	t.Run("head", func(t *testing.T) {
		p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "8")
			_, _ = io.WriteString(w, "ignored")
		}, nil)
		rec := cliReq(t, p, http.MethodHead, "install.sh")
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("HEAD = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("oversize", func(t *testing.T) {
		p := testCLIProxy(t, nil, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "999999999")
			_, _ = io.WriteString(w, "tiny")
		})
		rec := cliReq(t, p, http.MethodGet, "lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz")
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "tiny") {
			t.Fatalf("超限 = %d %q，应 502 且不透正文", rec.Code, rec.Body.String())
		}
	})
}

func TestCLIProxyLogsNoBody(t *testing.T) {
	const marker = "SECRET-CURSOR-BYTES-MUST-NOT-BE-LOGGED"
	var logs bytes.Buffer
	p := NewCLIProxy(slog.New(slog.NewTextHandler(&logs, nil)))
	p.SetBases("http://127.0.0.1:1/install", "")
	_ = cliReq(t, p, http.MethodGet, "install.sh")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(up.Close)
	p.SetBases(up.URL+"/install", "")
	rec := cliReq(t, p, http.MethodGet, "install.sh")
	if rec.Code != http.StatusOK || rec.Body.String() != marker {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("日志含响应正文：%s", logs.String())
	}
}
