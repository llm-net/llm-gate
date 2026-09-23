package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 内嵌的界面产物不入库、由 make web 生成：没有产物时（裸 go test）跳过，
// 有产物时 /ui/ 必须端出 index.html，未知子路径回退同一页且不可缓存。
func TestEmbeddedUIServesIndex(t *testing.T) {
	if _, err := fs.Stat(uiFiles, "uidist/index.html"); err != nil {
		t.Skip("uidist 里没有 index.html：先 make web 生成产物（make build / make test 会自动做）")
	}
	h := UIHandler()
	for _, path := range []string{"/ui/", "/ui/media", "/ui/no/such/file.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d，期望 200", path, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s Cache-Control = %q，期望 no-store", path, rec.Header().Get("Cache-Control"))
		}
	}
}
