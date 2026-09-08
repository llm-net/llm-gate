// Package gatehelper serves the embedded gate bootstrap scripts and release set.
package gatehelper

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

//go:embed install.sh install.ps1 assets/*
var files embed.FS

var releaseNames = map[string]bool{
	"stable.json": true, "SHA256SUMS": true,
	"gate-linux-amd64.tar.gz": true, "gate-linux-arm64.tar.gz": true,
	"gate-darwin-amd64.tar.gz": true, "gate-darwin-arm64.tar.gz": true,
	"gate-windows-amd64.zip": true, "gate-windows-arm64.zip": true,
}

var etags = func() map[string]string {
	result := make(map[string]string, len(releaseNames)+2)
	for _, name := range []string{
		"install.sh", "install.ps1", "assets/stable.json", "assets/SHA256SUMS",
		"assets/gate-linux-amd64.tar.gz", "assets/gate-linux-arm64.tar.gz",
		"assets/gate-darwin-amd64.tar.gz", "assets/gate-darwin-arm64.tar.gz",
		"assets/gate-windows-amd64.zip", "assets/gate-windows-arm64.zip",
	} {
		if b, err := files.ReadFile(name); err == nil {
			sum := sha256.Sum256(b)
			result[name] = fmt.Sprintf("\"gate-%x\"", sum[:12])
		}
	}
	return result
}()

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.PathValue("name")
		file := ""
		switch name {
		case "install.sh", "install.ps1":
			file = name
		default:
			if strings.Contains(name, "/") || path.Base(name) != name || !releaseNames[name] {
				http.NotFound(w, r)
				return
			}
			file = "assets/" + name
		}
		f, err := files.Open(file)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "embedded release unavailable", http.StatusInternalServerError)
			return
		}
		reader, ok := f.(io.ReadSeeker)
		if !ok {
			http.Error(w, "embedded release unavailable", http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".ps1") || strings.HasSuffix(name, ".json") || name == "SHA256SUMS" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		if strings.HasSuffix(name, ".json") {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("ETag", etags[file])
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, stat.ModTime(), reader)
	})
}
