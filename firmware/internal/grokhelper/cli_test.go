package grokhelper

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidCLIName(t *testing.T) {
	ok := []string{
		"stable", "alpha", "enterprise",
		"grok-1.0.5-linux-x86_64",
		"grok-1.0.5-linux-aarch64",
		"grok-1.0.5-macos-aarch64",
		"grok-1.0.5-macos-x86_64",
		"grok-1.0.5-windows-x86_64.exe",
		"grok-1.0.5-windows-aarch64.exe",
		"grok-1.0.8-rc.1-linux-x86_64",
	}
	for _, n := range ok {
		if !ValidCLIName(n) {
			t.Errorf("ValidCLIName(%q) = false，期望 true", n)
		}
	}
	bad := []string{
		"", "latest", "../stable", "stable/../x",
		"grok-1.0.5-linux-x86_64.exe", // .exe 只给 windows
		"grok-1.0.5-freebsd-x86_64",
		"grok-1.0.5-linux-armv7",
		"grok-v1-linux-x86_64",
		"install.sh",
		strings.Repeat("a", 97),
		"grok-1.0.5-linux-x86_64?x=1",
		"http://evil.example/grok",
	}
	for _, n := range bad {
		if ValidCLIName(n) {
			t.Errorf("ValidCLIName(%q) = true，期望 false", n)
		}
	}
}

func testProxy(t *testing.T, primary, fallback http.HandlerFunc) *CLIProxy {
	t.Helper()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	var primURL, fallURL string
	if primary != nil {
		s := httptest.NewServer(primary)
		t.Cleanup(s.Close)
		primURL = s.URL
	}
	if fallback != nil {
		s := httptest.NewServer(fallback)
		t.Cleanup(s.Close)
		fallURL = s.URL
	}
	p.SetBases(primURL, fallURL)
	return p
}

func cliReq(t *testing.T, p *CLIProxy, method, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/grok-helper/cli/"+name, nil)
	r.SetPathValue("name", name)
	p.ServeHTTP(rec, r)
	return rec
}

func TestCLIProxyRejectsUnknownNameWithoutDial(t *testing.T) {
	dialed := false
	p := testProxy(t, func(http.ResponseWriter, *http.Request) {
		dialed = true
	}, nil)
	rec := cliReq(t, p, http.MethodGet, "not-a-real-artifact")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404", rec.Code)
	}
	if dialed {
		t.Fatal("非法名字不该出站")
	}
}

func TestCLIProxyStreamsPrimaryAndStripsCookie(t *testing.T) {
	const body = "fake-grok-binary-payload"
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grok-1.0.5-linux-x86_64" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Set-Cookie", "__cf_bm=should-not-leak")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(body))
	}, nil)
	rec := cliReq(t, p, http.MethodGet, "grok-1.0.5-linux-x86_64")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d", rec.Code)
	}
	if rec.Body.String() != body {
		t.Fatalf("正文 = %q", rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("上游 Set-Cookie 不该透出：%q", rec.Header().Get("Set-Cookie"))
	}
	if rec.Header().Get("ETag") != `"abc"` {
		t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestCLIProxyFallsBackWhenPrimaryDown(t *testing.T) {
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	fall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stable" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("1.0.5\n"))
	}))
	t.Cleanup(fall.Close)
	p.SetBases("http://127.0.0.1:1", fall.URL)

	rec := cliReq(t, p, http.MethodGet, "stable")
	if rec.Code != http.StatusOK || rec.Body.String() != "1.0.5\n" {
		t.Fatalf("回落失败：%d %q", rec.Code, rec.Body.String())
	}
}

func TestCLIProxyFallsBackOnPrimary404(t *testing.T) {
	p := testProxy(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("1.0.8\n"))
		},
	)
	rec := cliReq(t, p, http.MethodGet, "alpha")
	if rec.Code != http.StatusOK || rec.Body.String() != "1.0.8\n" {
		t.Fatalf("主源 404 应改试回落：%d %q", rec.Code, rec.Body.String())
	}
}

func TestCLIProxyBothMissingIs404(t *testing.T) {
	p := testProxy(t,
		func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
	)
	rec := cliReq(t, p, http.MethodGet, "grok-9.9.9-linux-x86_64")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("404 不该带上游错误页：%q", rec.Body.String())
	}
}

func TestCLIProxyRejectsOversizedContentLength(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tiny"))
	}, nil)
	rec := cliReq(t, p, http.MethodGet, "grok-1.0.5-linux-x86_64")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态 = %d，期望 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tiny") {
		t.Fatal("超限件不该把上游正文交给客户端")
	}
}

func TestCLIProxyForwardsRange(t *testing.T) {
	var gotRange string
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abcd"))
	}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/grok-helper/cli/grok-1.0.5-linux-x86_64", nil)
	r.SetPathValue("name", "grok-1.0.5-linux-x86_64")
	r.Header.Set("Range", "bytes=0-3")
	p.ServeHTTP(rec, r)
	if gotRange != "bytes=0-3" {
		t.Fatalf("上游收到 Range = %q", gotRange)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("状态 = %d", rec.Code)
	}
	if rec.Header().Get("Content-Range") != "bytes 0-3/10" {
		t.Fatalf("Content-Range = %q", rec.Header().Get("Content-Range"))
	}
}

func TestCLIProxyHEADHasNoBody(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("上游方法 = %s，期望 HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "12")
		w.Header().Set("Content-Type", "text/plain")
	}, nil)
	rec := cliReq(t, p, http.MethodHead, "stable")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 不该有正文：%q", rec.Body.String())
	}
}

func TestCLIProxyLogOmitsBody(t *testing.T) {
	var buf bytes.Buffer
	p := NewCLIProxy(slog.New(slog.NewTextHandler(&buf, nil)))
	secret := "SECRET-GROK-BYTES-SHOULD-NOT-BE-LOGGED"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, secret)
	}))
	t.Cleanup(up.Close)
	// 先制造一次失败再成功，逼出翻转日志。
	p.SetBases("http://127.0.0.1:1", "")
	_ = cliReq(t, p, http.MethodGet, "stable")
	p.SetBases(up.URL, "")
	rec := cliReq(t, p, http.MethodGet, "stable")
	if rec.Code != http.StatusOK || rec.Body.String() != secret {
		t.Fatalf("透传失败：%d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("日志含响应正文：%s", buf.String())
	}
}

func TestCLIProxyBothDownIs502(t *testing.T) {
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	p.SetBases("http://127.0.0.1:1", "http://127.0.0.1:1")
	rec := cliReq(t, p, http.MethodGet, "stable")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态 = %d，期望 502", rec.Code)
	}
}
