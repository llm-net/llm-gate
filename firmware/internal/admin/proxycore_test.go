// proxycore_test.go 钉住内置代理内核端点的暴露档位（写入恒 LANOnly）、未注入管理器时的
// 503、上传路径的 octet-stream 放行，以及「内核启用中不能切走代理方式」的守卫入口形状。
package admin_test

import (
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

func TestProxyCoreRouteTiers(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	want := map[string]tunnelctx.Exposure{
		"GET /admin/v1/system/proxy-core":                       tunnelctx.Admin,
		"PUT /admin/v1/system/proxy-core/subscription":          tunnelctx.LANOnly,
		"POST /admin/v1/system/proxy-core/subscription/refresh": tunnelctx.LANOnly,
		"DELETE /admin/v1/system/proxy-core/subscription":       tunnelctx.LANOnly,
		"PUT /admin/v1/system/proxy-core/node":                  tunnelctx.LANOnly,
		"POST /admin/v1/system/proxy-core/latency":              tunnelctx.LANOnly,
		"POST /admin/v1/system/proxy-core/enable":               tunnelctx.LANOnly,
		"POST /admin/v1/system/proxy-core/disable":              tunnelctx.LANOnly,
		"GET /admin/v1/system/components/mihomo":                tunnelctx.Admin,
		"POST /admin/v1/system/components/mihomo/check":         tunnelctx.LANOnly,
		"POST /admin/v1/system/components/mihomo/manifest":      tunnelctx.LANOnly,
		"POST /admin/v1/system/components/mihomo/download":      tunnelctx.LANOnly,
		"POST " + admin.MihomoUploadPath:                        tunnelctx.LANOnly,
		"POST /admin/v1/system/components/mihomo/install":       tunnelctx.LANOnly,
		"POST /admin/v1/system/components/mihomo/rollback":      tunnelctx.LANOnly,
		"DELETE /admin/v1/system/components/mihomo/staged":      tunnelctx.LANOnly,
		"DELETE /admin/v1/system/components/mihomo":             tunnelctx.LANOnly,
	}
	for pat, exp := range want {
		if got, ok := tiers[pat]; !ok || got != exp {
			t.Errorf("%s 档位 = %v（登记 %v），期望 %v", pat, got, ok, exp)
		}
	}
}

func TestProxyCoreUnavailableAndAuth(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/system/proxy-core", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
	// 未注入管理器：读数与写入都如实答 503，不假装有内核。
	resp = e.do("GET", "/admin/v1/system/proxy-core", root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	resp = e.do("PUT", "/admin/v1/system/proxy-core/subscription", root, `{"url":"https://mysub.example/clash"}`)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	resp = e.do("POST", "/admin/v1/system/proxy-core/enable", root, `{"accept_license":true}`)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	// 上传路径接受 octet-stream（withCSRF 放行），仍要求会话与 CSRF 头。
	r := e.req("POST", admin.MihomoUploadPath, root, "xx")
	r.Header.Set("Content-Type", "application/octet-stream")
	resp = e.send(r)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	// 代理方式可以选内置内核（地址由固件管理，API 不能填地址）。
	resp = e.egressPatch(root, map[string]any{"provider": "mihomo"})
	wantStatus(t, resp, http.StatusOK)
	got := e.egressRead(root)
	if got.Provider != "mihomo" || got.Configured {
		t.Fatalf("选内核后读数: %+v", got)
	}
	resp = e.egressPatch(root, map[string]any{"address": "127.0.0.1:7891"})
	wantStatus(t, resp, http.StatusBadRequest)
	resp = e.egressPatch(root, map[string]any{"routes": map[string]string{"model_api": "proxy"}})
	wantStatus(t, resp, http.StatusBadRequest)
}
