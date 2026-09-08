// external_test.go 钉住公网接入方式的管理端点：只有已登录管理员能写；三态
// mode 二选一（外网映射 / Cloudflare Tunnel），两套设置分别保留；地址经过
// 规范化后随接入读数返回；查询串、片段和 userinfo 不得进入设置或审计。
package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func (e *env) externalURL(cookie string) string {
	e.t.Helper()
	return e.endpoints(cookie).ExternalURL
}

type externalJSON struct {
	Mode              string `json:"mode"`
	ManualURL         string `json:"manual_url"`
	ExternalURL       string `json:"external_url"`
	CloudflareEnabled bool   `json:"cloudflare_enabled"`
}

func (e *env) external(cookie string) externalJSON {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/system/external", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var body struct {
		External externalJSON `json:"external"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		e.t.Fatalf("解码 external: %v", err)
	}
	return body.External
}

func setExternalBody(mode, url string) string {
	b, _ := json.Marshal(struct {
		Mode      string `json:"mode"`
		ManualURL string `json:"manual_url"`
	}{Mode: mode, ManualURL: url})
	return string(b)
}

func TestExternalMappingScope(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	resp := e.do("PUT", "/admin/v1/system/external", "", setExternalBody("manual", "https://device.example.com"))
	wantStatus(t, resp, http.StatusUnauthorized)
	resp = e.do("GET", "/admin/v1/system/external", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if got := e.externalURL(root); got != "" {
		t.Errorf("未配置时读到 %q，期望空串", got)
	}
	if ext := e.external(root); ext.Mode != "none" || ext.ManualURL != "" || ext.CloudflareEnabled {
		t.Errorf("出厂读数不对: %+v", ext)
	}

	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("manual", "https://device.example.com/"))
	wantStatus(t, resp, http.StatusOK)
	if got := e.externalURL(root); got != "https://device.example.com" {
		t.Errorf("读到的外网映射 = %q，期望 https://device.example.com", got)
	}
	ext := e.external(root)
	if ext.Mode != "manual" || ext.ManualURL != "https://device.example.com" || ext.ExternalURL != "https://device.example.com" {
		t.Errorf("manual 读数不对: %+v", ext)
	}
}

func TestExternalMappingNormalize(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	ok := []struct{ in, want string }{
		{"https://device.example.com/", "https://device.example.com"},
		{"HTTPS://DEVICE.Example.COM", "https://device.example.com"},
		{"http://host:8443/llmgate/", "http://host:8443/llmgate"},
	}
	for _, c := range ok {
		resp := e.do("PUT", "/admin/v1/system/external", root, setExternalBody("manual", c.in))
		wantStatus(t, resp, http.StatusOK)
		if got := e.externalURL(root); got != c.want {
			t.Errorf("PUT %q → 读回 %q，期望 %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"ftp://host",
		"device.example.com",
		"https://host/?x=1",
		"https://host/#part",
		"https://user:secret@host",
		"https://",
		"", // manual 模式必须有地址
	}
	for _, in := range bad {
		resp := e.do("PUT", "/admin/v1/system/external", root, setExternalBody("manual", in))
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "invalid_external_url" {
			t.Errorf("PUT %q error.code = %q，期望 invalid_external_url", in, got)
		}
	}
	resp := e.do("PUT", "/admin/v1/system/external", root, setExternalBody("vpn", "https://host"))
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "invalid_external_mode" {
		t.Errorf("未知 mode error.code = %q", got)
	}

	// 关闭公网接入但保留地址：切换时不要求重填。
	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("none", "https://host:8443/llmgate"))
	wantStatus(t, resp, http.StatusOK)
	if got := e.externalURL(root); got != "" {
		t.Errorf("none 模式不该公布地址，读到 %q", got)
	}
	if ext := e.external(root); ext.Mode != "none" || ext.ManualURL != "http://host:8443/llmgate" && ext.ManualURL != "https://host:8443/llmgate" {
		t.Errorf("none 模式应保留 manual_url: %+v", ext)
	}
	// 选 cloudflare 而 Tunnel 未启用：方式选中了，但没有生效地址。
	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("cloudflare", ""))
	wantStatus(t, resp, http.StatusOK)
	if got := e.externalURL(root); got != "" {
		t.Errorf("Tunnel 未启用时 cloudflare 模式不该公布地址，读到 %q", got)
	}
	if ext := e.external(root); ext.Mode != "cloudflare" || ext.ManualURL != "" {
		t.Errorf("cloudflare 模式读数不对: %+v", ext)
	}
}
