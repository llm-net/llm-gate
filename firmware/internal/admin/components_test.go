// components_test.go 钉住「第三方组件」页依赖的卸载契约：`DELETE /admin/v1/system/components/<name>`
// 只限 LAN；功能启用中（Tunnel 在跑）答 409 component_in_use、什么都不动；停用后卸载成功、
// 读数回到 not_installed 并留一条 system.component_remove 审计；未注入管理器时如实 503。
package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

func TestComponentRemoveRouteTiers(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for _, pat := range []string{
		"DELETE /admin/v1/system/components/cloudflared",
		"DELETE /admin/v1/system/components/mihomo",
	} {
		if got, ok := tiers[pat]; !ok || got != tunnelctx.LANOnly {
			t.Errorf("%s 档位 = %v（登记 %v），期望 LANOnly", pat, got, ok)
		}
	}
	// 未注入管理器：如实 503，不假装卸载成功。
	root := e.rootSession()
	for _, path := range []string{"/admin/v1/system/components/cloudflared", "/admin/v1/system/components/mihomo"} {
		resp := e.do("DELETE", path, root, "")
		wantStatus(t, resp, http.StatusServiceUnavailable)
	}
}

func TestComponentRemoveRefusedWhileTunnelEnabled(t *testing.T) {
	e := newCFEnv(t)
	root := e.rootSession()
	resp := e.do("DELETE", "/admin/v1/system/components/cloudflared", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)

	// 装上组件、配好 hostname 与 token，启用 Tunnel。
	for _, step := range []string{"check", "download", "install"} {
		resp = e.do("POST", "/admin/v1/system/components/cloudflared/"+step, root, "{}")
		wantStatus(t, resp, http.StatusOK)
	}
	resp = e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, `{"hostname":"box.example.com","exposure":"api_only","token":"`+fakeToken+`"}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/enable", root, `{"accept_third_party":true}`)
	wantStatus(t, resp, http.StatusOK)

	// 启用中卸载：409，组件与 connector 原地不动。
	resp = e.do("DELETE", "/admin/v1/system/components/cloudflared", root, "")
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "component_in_use" {
		t.Fatalf("error.code = %q，期望 component_in_use", got)
	}
	if len(e.eng.slots) != 1 || e.eng.unit != "active" {
		t.Fatalf("启用中卸载不应动组件或 connector: slots=%d unit=%q", len(e.eng.slots), e.eng.unit)
	}
	for _, row := range e.auditRows() {
		if row.Event == "system.component_remove" {
			t.Fatal("被拒的卸载不该留审计")
		}
	}

	// 停用后卸载：200，读数回到 not_installed，审计只记组件名与版本。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/disable", root, `{"keep_token":true}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("DELETE", "/admin/v1/system/components/cloudflared", root, "")
	wantStatus(t, resp, http.StatusOK)
	var body struct {
		Component struct {
			State     string `json:"state"`
			Installed bool   `json:"installed"`
		} `json:"component"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Component.Installed || body.Component.State != "not_installed" {
		t.Fatalf("卸载后读数: %+v", body.Component)
	}
	if len(e.eng.slots) != 0 {
		t.Fatalf("引擎应已删掉组件目录: slots=%d", len(e.eng.slots))
	}
	var saw bool
	for _, row := range e.auditRows() {
		if row.Event == "system.component_remove" {
			saw = true
			if row.Entity != "component:cloudflared" || row.Detail != "卸载 cloudflared 组件（2026.8.2）" {
				t.Fatalf("审计行不对: %+v", row)
			}
		}
	}
	if !saw {
		t.Fatal("卸载成功应留 system.component_remove 审计")
	}
	// Tunnel 配置与密封 token 不随组件卸载消失：再启用只差组件。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/enable", root, `{"accept_third_party":true}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "component_missing" {
		t.Fatalf("卸载后再启用应差组件: %q", got)
	}
}
