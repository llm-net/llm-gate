package opencodehelper

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
		"latest",
		"releases/1.18.22/opencode-linux-arm64.tar.gz",
		"releases/1.18.22/opencode-linux-arm64-musl.tar.gz",
		"releases/1.18.22/opencode-linux-x64-baseline-musl.tar.gz",
		"releases/1.18.22/opencode-darwin-arm64.zip",
		"releases/1.18.22/opencode-darwin-x64-baseline.zip",
		"releases/1.18.22/opencode-windows-arm64.zip",
		"releases/1.18.22/opencode-windows-x64-baseline.zip",
	}
	for _, path := range valid {
		if !ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = false", path)
		}
	}
	invalid := []string{
		"", "releases/latest/opencode-linux-x64.tar.gz", "releases/1.18/opencode-linux-x64.tar.gz",
		"releases/1.18.22/../../etc/passwd", "releases/1.18.22/opencode-desktop-linux-amd64.deb",
		"releases/1.18.22/opencode-linux-arm64-baseline.tar.gz", "releases/1.18.22/opencode-linux-x64.zip",
		"https://evil.invalid/opencode-linux-x64.tar.gz", strings.Repeat("a", 181),
	}
	for _, path := range invalid {
		if ValidCLIPath(path) {
			t.Errorf("ValidCLIPath(%q) = true", path)
		}
	}
}

func testCLIProxy(t *testing.T, api, releases http.HandlerFunc) *CLIProxy {
	t.Helper()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	var apiURL, releasesURL string
	if api != nil {
		srv := httptest.NewServer(api)
		t.Cleanup(srv.Close)
		apiURL = srv.URL
	}
	if releases != nil {
		srv := httptest.NewServer(releases)
		t.Cleanup(srv.Close)
		releasesURL = srv.URL
	}
	p.SetBases(apiURL, releasesURL)
	return p
}

func cliRequest(t *testing.T, p *CLIProxy, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/opencode-helper/cli/"+path, nil)
	request.SetPathValue("path", path)
	p.ServeHTTP(recorder, request)
	return recorder
}

func TestCLIProxyUsesMetadataAndReleaseOrigins(t *testing.T) {
	var apiPath, releasePath, accept, apiVersion string
	p := testCLIProxy(t,
		func(w http.ResponseWriter, r *http.Request) {
			apiPath, accept, apiVersion = r.URL.Path, r.Header.Get("Accept"), r.Header.Get("X-GitHub-Api-Version")
			w.Header().Set("Set-Cookie", "must-not-leak")
			_, _ = io.WriteString(w, `{"tag_name":"v1.18.22"}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			releasePath = r.URL.Path
			_, _ = io.WriteString(w, "archive")
		},
	)
	metadata := cliRequest(t, p, http.MethodGet, "latest")
	if metadata.Code != http.StatusOK || metadata.Body.String() != `{"tag_name":"v1.18.22"}` ||
		apiPath != "/latest" || accept != "application/vnd.github+json" || apiVersion != "2022-11-28" ||
		metadata.Header().Get("Set-Cookie") != "" {
		t.Fatalf("metadata=%d %q path=%q accept=%q api=%q headers=%v", metadata.Code, metadata.Body.String(), apiPath, accept, apiVersion, metadata.Header())
	}
	archive := cliRequest(t, p, http.MethodGet, "releases/1.18.22/opencode-linux-x64.tar.gz")
	if archive.Code != http.StatusOK || archive.Body.String() != "archive" ||
		releasePath != "/v1.18.22/opencode-linux-x64.tar.gz" {
		t.Fatalf("archive=%d %q path=%q", archive.Code, archive.Body.String(), releasePath)
	}
}

func TestCLIProxyRejectsUnknownPathWithoutDial(t *testing.T) {
	dialed := false
	p := testCLIProxy(t, nil, func(http.ResponseWriter, *http.Request) { dialed = true })
	recorder := cliRequest(t, p, http.MethodGet, "releases/1.18.22/opencode-desktop-linux-amd64.deb")
	if recorder.Code != http.StatusNotFound || dialed {
		t.Fatalf("invalid path=%d dialed=%v", recorder.Code, dialed)
	}
}

func TestCLIAllowedRedirect(t *testing.T) {
	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	from := request("https://github.com/anomalyco/opencode/releases/download/v1.18.22/opencode-linux-x64.tar.gz")
	to := request("https://release-assets.githubusercontent.com/github-production-release-asset/123/file?signature=fake")
	if !cliAllowedRedirect(to, []*http.Request{from}) {
		t.Fatal("official GitHub release asset redirect should be allowed")
	}
	for _, target := range []string{
		"https://evil.invalid/github-production-release-asset/123/file",
		"http://release-assets.githubusercontent.com/github-production-release-asset/123/file",
		"https://release-assets.githubusercontent.com/not-a-release/file",
	} {
		if cliAllowedRedirect(request(target), []*http.Request{from}) {
			t.Errorf("redirect to %s should be rejected", target)
		}
	}
}

func TestCLIProxyRangeHeadOversizeAndBodyLogging(t *testing.T) {
	const marker = "SECRET-OPENCODE-BYTES-MUST-NOT-BE-LOGGED"
	var gotRange string
	var logs bytes.Buffer
	p := NewCLIProxy(slog.New(slog.NewTextHandler(&logs, nil)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(upstream.Close)
	p.SetBases(upstream.URL, upstream.URL)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/opencode-helper/cli/releases/1.18.22/opencode-linux-x64.tar.gz", nil)
	request.SetPathValue("path", "releases/1.18.22/opencode-linux-x64.tar.gz")
	request.Header.Set("Range", "bytes=0-3")
	p.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || gotRange != "bytes=0-3" || recorder.Body.String() != marker {
		t.Fatalf("range=%d %q %q", recorder.Code, gotRange, recorder.Body.String())
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("logs contain body: %s", logs.String())
	}
	if head := cliRequest(t, p, http.MethodHead, "latest"); head.Code != http.StatusPartialContent || head.Body.Len() != 0 {
		t.Fatalf("HEAD=%d %q", head.Code, head.Body.String())
	}

	p = testCLIProxy(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		_, _ = io.WriteString(w, "tiny")
	})
	if oversized := cliRequest(t, p, http.MethodGet, "releases/1.18.22/opencode-linux-x64.tar.gz"); oversized.Code != http.StatusBadGateway || strings.Contains(oversized.Body.String(), "tiny") {
		t.Fatalf("oversize=%d %q", oversized.Code, oversized.Body.String())
	}
}
