// landomain_test.go 钉住内网域名管理端点：未接管理器时答 503；提供方式只认 LLM Gate官网
// 与自有域名（别的值按输入错误 400）；未关联账号不能申领/登记；自有域名与托管域名的端点
// 互不串用；写入端点只限 LAN；接入读数在证书就绪前不带域名地址。
package admin_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/landomain"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

type lanDomainJSON struct {
	Provider  string `json:"provider"`
	Providers []struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		Available bool   `json:"available"`
	} `json:"providers"`
	SiteConfigured bool   `json:"site_configured"`
	Suffix         string `json:"suffix"`
	Link           struct {
		Linked bool `json:"linked"`
	} `json:"link"`
	Claimed     bool   `json:"claimed"`
	Kind        string `json:"kind"`
	HTTPSListen string `json:"https_listen"`
	HTTPSActive bool   `json:"https_active"`
	Issuing     bool   `json:"issuing"`
	IssueStage  string `json:"issue_stage"`
}

func (e *env) lanDomain(cookie string) lanDomainJSON {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/system/lan-domain", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var body struct {
		LanDomain lanDomainJSON `json:"lan_domain"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		e.t.Fatalf("解码 lan_domain: %v", err)
	}
	return body.LanDomain
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解码错误体: %v", err)
	}
	return body.Error.Code
}

func TestLanDomainUnavailableWithoutManager(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/system/lan-domain", root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if code := errorCode(t, resp); code != "lan_domain_unavailable" {
		t.Fatalf("code = %s", code)
	}
	if got := e.endpoints(root).LanDomainURL; got != "" {
		t.Fatalf("未接管理器时接入读数不该带域名地址: %q", got)
	}
}

func TestLanDomainProviderAndGuards(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	mgr := landomain.New(landomain.Options{DataDir: e.dir, Settings: e.st, Site: landomain.NewSiteClient("http://127.0.0.1:9"), Model: "test"})
	e.srv.SetLanDomain(mgr)

	resp := e.do("GET", "/admin/v1/system/lan-domain", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)

	ld := e.lanDomain(root)
	if ld.Provider != "" || ld.Suffix != "llm.net" || ld.Link.Linked || ld.Claimed || ld.HTTPSListen != ":443" || !ld.SiteConfigured {
		t.Fatalf("出厂读数不对: %+v", ld)
	}
	var official, own bool
	for _, p := range ld.Providers {
		switch p.ID {
		case "official_site":
			official = p.Available
		case "own_domain":
			own = p.Available
		}
	}
	if !official || !own || len(ld.Providers) != 2 {
		t.Fatalf("提供方可用性不对: %+v", ld.Providers)
	}

	resp = e.do("PUT", "/admin/v1/system/lan-domain", root, `{"provider":"cloudflare"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errorCode(t, resp); code != "invalid_lan_domain" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/claim", root, `{"label":"box","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "provider_not_set" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/register", root, `{"hostname":"box.example.com","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "provider_not_set" {
		t.Fatalf("code = %s", code)
	}

	// 自有域名：未关联不能登记；域名形状不对答 400；托管域名的 claim 在这个方式下 409。
	resp = e.do("PUT", "/admin/v1/system/lan-domain", root, `{"provider":"own_domain"}`)
	wantStatus(t, resp, http.StatusOK)
	if ld = e.lanDomain(root); ld.Provider != "own_domain" || ld.Kind != "custom" {
		t.Fatalf("自有域名读数不对: %+v", ld)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/register", root, `{"hostname":"box.example.com","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "not_linked" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/dns-check", root, `{}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "domain_not_claimed" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/claim", root, `{"label":"box","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "provider_mismatch" {
		t.Fatalf("code = %s", code)
	}
	// 没有签发在进行：陪等立即回快照，不阻塞。
	resp = e.do("POST", "/admin/v1/system/lan-domain/certificate/wait", root, `{"since":"submitting"}`)
	wantStatus(t, resp, http.StatusOK)
	if ld = e.lanDomain(root); ld.Issuing || ld.IssueStage != "" {
		t.Fatalf("空闲时陪等读数不对: %+v", ld)
	}

	resp = e.do("PUT", "/admin/v1/system/lan-domain", root, `{"provider":"official_site","https_listen":"0.0.0.0:8443"}`)
	wantStatus(t, resp, http.StatusOK)
	ld = e.lanDomain(root)
	if ld.Provider != "official_site" || ld.HTTPSListen != "0.0.0.0:8443" {
		t.Fatalf("保存后读数不对: %+v", ld)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/claim", root, `{"label":"box","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "not_linked" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("POST", "/admin/v1/system/lan-domain/link/wait", root, `{}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "no_link_session" {
		t.Fatalf("code = %s", code)
	}
	// 官网不可达：发起关联答 502，message 是中文。
	resp = e.do("POST", "/admin/v1/system/lan-domain/link/start", root, `{}`)
	wantStatus(t, resp, http.StatusBadGateway)
	if code := errorCode(t, resp); code != "site_unreachable" {
		t.Fatalf("code = %s", code)
	}
	resp = e.do("PUT", "/admin/v1/system/lan-domain", root, `{"https_listen":"nope"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errorCode(t, resp); code != "invalid_listen" {
		t.Fatalf("code = %s", code)
	}
	if got := e.endpoints(root).LanDomainURL; got != "" {
		t.Fatalf("无证书时接入读数不该带域名地址: %q", got)
	}
}

func TestLanDomainWriteRoutesAreLANOnly(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	if tiers["GET /admin/v1/system/lan-domain"] != tunnelctx.Admin {
		t.Fatalf("读数应是 Admin 档: %v", tiers["GET /admin/v1/system/lan-domain"])
	}
	for pattern, tier := range tiers {
		if !strings.Contains(pattern, "/admin/v1/system/lan-domain") || strings.HasPrefix(pattern, "GET ") {
			continue
		}
		if tier != tunnelctx.LANOnly {
			t.Errorf("%s 应只限 LAN，实际 %v", pattern, tier)
		}
	}
}

// TestLanDomainSiteErrorsPassThrough 钉住官网业务拒绝的透传：申领/登记/改地址/释放被官网
// 拒绝时，管理面按官网的状态码与 code 应答；令牌被撤销（401）映射为 409 link_revoked。
func TestLanDomainSiteErrorsPassThrough(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	var mu sync.Mutex
	reject := struct {
		status int
		code   string
	}{}
	setReject := func(status int, code string) {
		mu.Lock()
		defer mu.Unlock()
		reject.status, reject.code = status, code
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/device/link/start":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"deviceCode":"dvc_`+strings.Repeat("b", 43)+`","userCode":"K7QM-3WPX","verificationUrl":"https://llm.net/link/","verificationUrlComplete":"https://llm.net/link/?code=K7QM-3WPX","expiresIn":900,"interval":1}`)
		case "/api/device/link/poll":
			io.WriteString(w, `{"status":"approved","token":"dvt_`+strings.Repeat("a", 43)+`","linkId":"dvl_1","interval":5,"account":{"displayName":"Owner"}}`)
		case "/api/device/link":
			io.WriteString(w, `{"link":{"id":"dvl_1"},"domain":null,"lanDomain":{"enabled":true,"suffix":"llm.net"}}`)
		case "/api/device/domain/claim", "/api/device/domain/custom", "/api/device/domain/target", "/api/device/domain/release":
			mu.Lock()
			status, code := reject.status, reject.code
			mu.Unlock()
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%q,"message":"官网拒绝：%s"}}`, code, code)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	defer site.Close()

	mgr := landomain.New(landomain.Options{
		DataDir: e.dir, Settings: e.st, Model: "test",
		Site:         landomain.NewSiteClient(site.URL),
		PollInterval: 10 * time.Millisecond,
	})
	e.srv.SetLanDomain(mgr)

	resp := e.do("PUT", "/admin/v1/system/lan-domain", root, `{"provider":"official_site"}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/lan-domain/link/start", root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/lan-domain/link/wait", root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	if ld := e.lanDomain(root); !ld.Link.Linked {
		t.Fatalf("关联后读数应为已关联: %+v", ld)
	}

	cases := []struct {
		siteStatus int
		siteCode   string
		wantStatus int
		wantCode   string
	}{
		{http.StatusServiceUnavailable, "lan_domain_not_configured", http.StatusServiceUnavailable, "lan_domain_not_configured"},
		{http.StatusConflict, "label_taken", http.StatusConflict, "label_taken"},
		{http.StatusTooManyRequests, "rate_limited", http.StatusTooManyRequests, "rate_limited"},
		{http.StatusUnauthorized, "device_token_invalid", http.StatusConflict, "link_revoked"},
	}
	for _, tc := range cases {
		setReject(tc.siteStatus, tc.siteCode)
		resp = e.do("POST", "/admin/v1/system/lan-domain/claim", root, `{"label":"studio","target_ip":"192.168.1.20"}`)
		wantStatus(t, resp, tc.wantStatus)
		if code := errorCode(t, resp); code != tc.wantCode {
			t.Fatalf("官网 %d %s → 管理面 code = %s，期望 %s", tc.siteStatus, tc.siteCode, code, tc.wantCode)
		}
	}
	// 官网方式换成自有域名后登记同样透传（先解除已关联但未申领的状态不必：登记只要求已关联）。
	resp = e.do("PUT", "/admin/v1/system/lan-domain", root, `{"provider":"own_domain"}`)
	wantStatus(t, resp, http.StatusOK)
	setReject(http.StatusConflict, "hostname_taken")
	resp = e.do("POST", "/admin/v1/system/lan-domain/register", root, `{"hostname":"box.example.com","target_ip":"192.168.1.20"}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errorCode(t, resp); code != "hostname_taken" {
		t.Fatalf("登记被官网拒绝应透出 hostname_taken: %s", code)
	}
}
