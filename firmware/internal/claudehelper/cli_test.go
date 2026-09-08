package claudehelper

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
	valid := []string{
		"latest", "2.1.241/manifest.json",
		"2.1.241/darwin-arm64/claude", "2.1.241/darwin-x64/claude",
		"2.1.241/linux-arm64/claude", "2.1.241/linux-x64/claude",
		"2.1.241/linux-arm64-musl/claude", "2.1.241/linux-x64-musl/claude",
		"2.1.241/win32-arm64/claude.exe", "2.1.241/win32-x64/claude.exe",
	}
	for _, path := range valid {
		if !ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = false", path)
		}
	}
	invalid := []string{
		"", "stable", "../latest", "latest/manifest.json", "2.1/manifest.json",
		"2.1.241/linux-x64/claude.exe", "2.1.241/win32-x64/claude",
		"2.1.241/freebsd-x64/claude", "2.1.241/linux-x64/other",
		"2.1.241/../../etc/passwd", "https://evil.invalid/claude",
		strings.Repeat("a", 161),
	}
	for _, path := range invalid {
		if ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = true", path)
		}
	}
}

func testCLIProxy(t *testing.T, upstream http.HandlerFunc) *CLIProxy {
	t.Helper()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	if upstream == nil {
		p.SetBase("")
		return p
	}
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	p.SetBase(srv.URL)
	return p
}

func cliRequest(t *testing.T, p *CLIProxy, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/claude-helper/cli/"+path, nil)
	request.SetPathValue("path", path)
	p.ServeHTTP(recorder, request)
	return recorder
}

func TestCLIProxyStreamsOnlyAllowedArtifacts(t *testing.T) {
	var requestedPath, requestedRange string
	p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath, requestedRange = r.URL.Path, r.Header.Get("Range")
		w.Header().Set("Set-Cookie", "must-not-leak")
		w.Header().Set("ETag", `"fixture"`)
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "fake")
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/claude-helper/cli/2.1.241/linux-x64/claude", nil)
	request.SetPathValue("path", "2.1.241/linux-x64/claude")
	request.Header.Set("Range", "bytes=0-3")
	p.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "fake" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if requestedPath != "/2.1.241/linux-x64/claude" || requestedRange != "bytes=0-3" {
		t.Fatalf("upstream = %q range=%q", requestedPath, requestedRange)
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("ETag") != `"fixture"` {
		t.Fatalf("forwarded headers = %#v", recorder.Header())
	}
}

func TestCLIProxyRejectsUnknownPathWithoutDial(t *testing.T) {
	dialed := false
	p := testCLIProxy(t, func(http.ResponseWriter, *http.Request) { dialed = true })
	recorder := cliRequest(t, p, http.MethodGet, "2.1.241/linux-x64/not-claude")
	if recorder.Code != http.StatusNotFound || dialed {
		t.Fatalf("invalid path = %d dialed=%v", recorder.Code, dialed)
	}
}

func TestCLIProxyHEADOversizeAndRedirect(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "8")
			_, _ = io.WriteString(w, "ignored")
		})
		recorder := cliRequest(t, p, http.MethodHead, "latest")
		if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
			t.Fatalf("HEAD = %d %q", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("oversize", func(t *testing.T) {
		p := testCLIProxy(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "999999999")
			_, _ = io.WriteString(w, "tiny")
		})
		recorder := cliRequest(t, p, http.MethodGet, "latest")
		if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "tiny") {
			t.Fatalf("oversize = %d %q", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		dialed := false
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialed = true }))
		t.Cleanup(target.Close)
		p := testCLIProxy(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/outside", http.StatusFound)
		})
		recorder := cliRequest(t, p, http.MethodGet, "latest")
		if recorder.Code != http.StatusFound || dialed {
			t.Fatalf("redirect = %d dialed=%v", recorder.Code, dialed)
		}
	})
}

func TestCLIProxyLogsNoBody(t *testing.T) {
	const marker = "SECRET-CLAUDE-BYTES-MUST-NOT-BE-LOGGED"
	var logs bytes.Buffer
	p := NewCLIProxy(slog.New(slog.NewTextHandler(&logs, nil)))
	p.SetBase("http://127.0.0.1:1")
	_ = cliRequest(t, p, http.MethodGet, "latest")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(upstream.Close)
	p.SetBase(upstream.URL)
	recorder := cliRequest(t, p, http.MethodGet, "latest")
	if recorder.Code != http.StatusOK || recorder.Body.String() != marker {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("logs contain body: %s", logs.String())
	}
}
