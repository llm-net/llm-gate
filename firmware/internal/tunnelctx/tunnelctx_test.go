package tunnelctx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInfoRoundTripAndHTTPS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if Trusted(r) || HTTPS(r) || Secure(r) {
		t.Fatal("普通请求不该被判成可信 Tunnel / HTTPS")
	}
	// 伪造转发头在普通请求上无效：判据只看 context。
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if Trusted(r) || HTTPS(r) {
		t.Fatal("转发头不能构造可信标记")
	}
	ctx := With(context.Background(), Info{ClientIP: "203.0.113.9", Hostname: "box.example.com"})
	r2 := r.WithContext(ctx)
	info, ok := From(r2.Context())
	if !ok || info.ClientIP != "203.0.113.9" || info.Hostname != "box.example.com" {
		t.Fatalf("From = %+v, %v", info, ok)
	}
	if !Trusted(r2) || !HTTPS(r2) || !Secure(r2) {
		t.Fatal("可信 Tunnel 请求应判成 HTTPS/Secure")
	}
}

func TestMatcherProfiles(t *testing.T) {
	m := NewMux()
	m.HandleFunc("GET /healthz", API, func(http.ResponseWriter, *http.Request) {})
	m.HandleFunc("POST /v1/chat/completions", API, func(http.ResponseWriter, *http.Request) {})
	m.Handle("/v1/", API, http.NotFoundHandler())
	m.HandleFunc("GET /ui/", Admin, func(http.ResponseWriter, *http.Request) {})
	m.HandleFunc("PUT /admin/v1/system/network", LANOnly, func(http.ResponseWriter, *http.Request) {})
	m.HandleFunc("/", LANOnly, func(http.ResponseWriter, *http.Request) {})
	if got := len(m.Routes()); got != 6 {
		t.Fatalf("Routes = %d 条，期望 6", got)
	}
	mt := NewMatcher(m.Routes())

	cases := []struct {
		method, path string
		api, admin   bool
		adminOnly    bool
	}{
		{"GET", "/healthz", true, true, false},
		{"POST", "/v1/chat/completions", true, true, false},
		{"GET", "/v1/anything", true, true, false}, // 显式登记的兜底模式也算列出
		{"GET", "/ui/", false, true, true},
		{"GET", "/ui/network", false, true, true},
		{"PUT", "/admin/v1/system/network", false, false, false},
		{"GET", "/admin/v1/keys", false, false, false},
		{"GET", "/", false, false, false},
		{"DELETE", "/healthz", false, false, false}, // 方法不匹配 = 未列出
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := mt.Allows(r, ProfileAPIOnly); got != c.api {
			t.Errorf("%s %s api_only = %v，期望 %v", c.method, c.path, got, c.api)
		}
		if got := mt.Allows(r, ProfileAPIAndAdmin); got != c.admin {
			t.Errorf("%s %s api_and_admin = %v，期望 %v", c.method, c.path, got, c.admin)
		}
		if got := mt.AdminOnly(r); got != c.adminOnly {
			t.Errorf("%s %s AdminOnly = %v，期望 %v", c.method, c.path, got, c.adminOnly)
		}
		if mt.Allows(r, Profile("bogus")) {
			t.Errorf("未知 profile 必须一概不放行（%s %s）", c.method, c.path)
		}
	}
}

func TestProfileValid(t *testing.T) {
	if !ProfileAPIOnly.Valid() || !ProfileAPIAndAdmin.Valid() || Profile("").Valid() || Profile("all").Valid() {
		t.Fatal("Profile.Valid 判定不对")
	}
	if LANOnly.String() != "lan_only" || API.String() != "api" || Admin.String() != "admin" {
		t.Fatal("Exposure.String 不对")
	}
}
