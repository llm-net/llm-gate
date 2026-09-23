package mcodehelper

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallerWhitelistAndAnonymousProxy(t *testing.T) {
	for _, path := range []string{"install.sh", "install.ps1"} {
		if !ValidCLIPath(path) {
			t.Fatalf("rejected %s", path)
		}
	}
	for _, path := range []string{"", "../install.sh", "install.sh?url=x", "install.sh/extra", "https://evil.invalid/install.sh", "npm/package", "node.exe"} {
		if ValidCLIPath(path) {
			t.Fatalf("accepted %s", path)
		}
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/install.sh" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("request escaped whitelist or forwarded credentials")
		}
		w.Header().Set("Set-Cookie", "must-not-forward")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "#!/bin/sh\n# fake official installer\n")
	}))
	defer up.Close()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	p.SetBase(up.URL)
	r := httptest.NewRequest("GET", "/mcode-helper/cli/install.sh", nil)
	r.SetPathValue("path", "install.sh")
	r.Header.Set("Authorization", "Bearer fake-only")
	r.Header.Set("Cookie", "sid=fake-only")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "#!/bin/sh\n# fake official installer\n" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("proxy changed bytes or forwarded cookies")
	}
	r.SetPathValue("path", "../install.sh")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("unknown artifact allowed")
	}
	r.Method = "POST"
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal("write method allowed")
	}
}

func TestNodeRuntimeWhitelistAndMapping(t *testing.T) {
	prefix := "node/v" + NodeVersion + "/"
	paths := []string{prefix + "SHASUMS256.txt"}
	for _, platform := range []string{"linux-x64.tar.gz", "linux-arm64.tar.gz", "darwin-x64.tar.gz", "darwin-arm64.tar.gz", "win-x64.zip", "win-arm64.zip"} {
		paths = append(paths, prefix+"node-v"+NodeVersion+"-"+platform)
	}
	var expected string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != expected || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected Node.js origin path or credentials")
		}
		io.WriteString(w, "fake-node-artifact")
	}))
	defer up.Close()
	p := NewCLIProxy(slog.New(slog.DiscardHandler))
	p.SetBase(up.URL)
	for _, path := range paths {
		if !ValidCLIPath(path) {
			t.Fatalf("rejected %s", path)
		}
		expected = "/" + strings.TrimPrefix(path, "node/")
		r := httptest.NewRequest("GET", "/mcode-helper/cli/"+path, nil)
		r.SetPathValue("path", path)
		r.Header.Set("Authorization", "Bearer fake-only")
		r.Header.Set("Cookie", "sid=fake-only")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "fake-node-artifact" {
			t.Fatalf("Node.js artifact mapping failed: %s", path)
		}
	}
	for _, path := range []string{prefix + "../SHASUMS256.txt", prefix + "node-v" + NodeVersion + "-linux-riscv64.tar.gz", "node/v0.0.0/SHASUMS256.txt", prefix + "SHASUMS256.txt?url=x"} {
		if ValidCLIPath(path) {
			t.Fatalf("accepted %s", path)
		}
	}
}
