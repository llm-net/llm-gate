// egress_test.go 钉住出站代理的管理端点：读数不含凭据；PATCH 整组校验、原子落库、
// 省略即保留 / 空串即清除；未配代理不能选 proxy；账号覆盖与清除冲突；写入与测试端点
// 只限 LAN；审计与日志里搜不到用户名、口令。
package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

type auditEvent struct{ Event, Entity, Detail string }

// audits 直接读 audit_events 表（与 server_test 的审计用例同一取法）。
func (e *env) audits() []auditEvent {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		e.t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		e.t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var out []auditEvent
	for rows.Next() {
		var a auditEvent
		if err := rows.Scan(&a.Event, &a.Entity, &a.Detail); err != nil {
			e.t.Fatalf("scan audit: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

type egressReadJSON struct {
	Provider    string            `json:"provider"`
	Address     string            `json:"address"`
	UsernameSet bool              `json:"username_set"`
	PasswordSet bool              `json:"password_set"`
	Configured  bool              `json:"configured"`
	Available   bool              `json:"available"`
	Routes      map[string]string `json:"routes"`
	Scopes      []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Route string `json:"route"`
	} `json:"scopes"`
	Overrides []struct {
		UpstreamID int64  `json:"upstream_id"`
		Name       string `json:"name"`
		EgressMode string `json:"egress_mode"`
	} `json:"overrides"`
	LastTest *struct {
		OK       bool   `json:"ok"`
		Category string `json:"category"`
		Stages   []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"stages"`
	} `json:"last_test"`
}

func (e *env) egressRead(cookie string) egressReadJSON {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/system/egress", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var body struct {
		Egress egressReadJSON `json:"egress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		e.t.Fatalf("解码 egress: %v", err)
	}
	return body.Egress
}

func (e *env) egressPatch(cookie string, body map[string]any) *http.Response {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	return e.do("PATCH", "/admin/v1/system/egress", cookie, string(raw))
}

func TestEgressReadDefaultsAndAuth(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/system/egress", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
	resp = e.egressPatch("", map[string]any{"provider": "socks5"})
	wantStatus(t, resp, http.StatusUnauthorized)

	got := e.egressRead(root)
	if got.Provider != "" || got.Configured || got.Available || got.UsernameSet || got.PasswordSet {
		t.Fatalf("出厂读数不对: %+v", got)
	}
	if len(got.Scopes) != 6 || len(got.Routes) != 6 {
		t.Fatalf("应有六类流量: %+v", got)
	}
	for _, sc := range got.Scopes {
		if sc.Route != "direct" || sc.Label == "" {
			t.Fatalf("缺省应全部直连且带界面名称: %+v", sc)
		}
	}
	if got.Overrides == nil || len(got.Overrides) != 0 {
		t.Fatalf("overrides 应为空数组: %+v", got.Overrides)
	}
}

func TestEgressUpdateAndSecrets(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 未配代理就选 proxy：400，不落库。
	resp := e.egressPatch(root, map[string]any{"routes": map[string]string{"model_api": "proxy"}})
	wantStatus(t, resp, http.StatusBadRequest)
	if got := e.egressRead(root); got.Routes["model_api"] != "direct" {
		t.Fatalf("校验失败不应生效: %+v", got.Routes)
	}
	// 非法地址。
	resp = e.egressPatch(root, map[string]any{"provider": "clash", "address": "socks5://127.0.0.1:7891"})
	wantStatus(t, resp, http.StatusBadRequest)
	// 只填用户名。
	resp = e.egressPatch(root, map[string]any{"provider": "clash", "address": "127.0.0.1:7891", "username": "alice"})
	wantStatus(t, resp, http.StatusBadRequest)

	// 一次提交：方式 + 地址 + 认证 + 两类经代理。
	resp = e.egressPatch(root, map[string]any{
		"provider": "clash", "address": "127.0.0.1:7891", "username": "alice-user", "password": "secret-pw",
		"routes": map[string]string{"model_api": "proxy", "agent_auth": "proxy"},
	})
	wantStatus(t, resp, http.StatusOK)
	got := e.egressRead(root)
	if got.Provider != "clash" || got.Address != "127.0.0.1:7891" || !got.UsernameSet || !got.PasswordSet || !got.Configured || !got.Available {
		t.Fatalf("读数不对: %+v", got)
	}
	if got.Routes["model_api"] != "proxy" || got.Routes["agent_auth"] != "proxy" || got.Routes["official_site"] != "direct" {
		t.Fatalf("路由不对: %+v", got.Routes)
	}
	// 生效快照里有凭据；持久化后重新 Load 也读得回（密封走设备密钥）。
	if p := e.egress.Policy(); p.Profile.Password != "secret-pw" || p.Profile.Username != "alice-user" {
		t.Fatalf("快照凭据不对: %v", p.Profile)
	}
	fresh := egress.NewManager(egress.Options{Settings: e.st})
	if err := fresh.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := fresh.Policy(); p.Profile.Password != "secret-pw" || p.Routes[egress.ScopeModelAPI] != egress.RouteProxy {
		t.Fatalf("回读不对: %v %v", p.Profile, p.Routes)
	}

	// 省略即保留：只改一类路由。
	resp = e.egressPatch(root, map[string]any{"routes": map[string]string{"agent_auth": "direct"}})
	wantStatus(t, resp, http.StatusOK)
	got = e.egressRead(root)
	if !got.PasswordSet || got.Routes["model_api"] != "proxy" || got.Routes["agent_auth"] != "direct" {
		t.Fatalf("省略字段应保留: %+v", got)
	}
	// 空串即清除认证。
	resp = e.egressPatch(root, map[string]any{"username": "", "password": ""})
	wantStatus(t, resp, http.StatusOK)
	if got = e.egressRead(root); got.UsernameSet || got.PasswordSet || !got.Configured {
		t.Fatalf("清除认证后读数不对: %+v", got)
	}
	// 仍有分类经代理时不能清除代理。
	resp = e.egressPatch(root, map[string]any{"provider": ""})
	wantStatus(t, resp, http.StatusBadRequest)
	// 同一提交改回直连即可。
	resp = e.egressPatch(root, map[string]any{"provider": "", "routes": map[string]string{"model_api": "direct"}})
	wantStatus(t, resp, http.StatusOK)
	if got = e.egressRead(root); got.Configured || got.Address != "" {
		t.Fatalf("清除后应未配置: %+v", got)
	}

	// 响应、审计与日志里搜不到凭据。
	for _, needle := range []string{"secret-pw", "alice-user"} {
		if strings.Contains(e.buf.String(), needle) {
			t.Fatalf("日志泄露 %q", needle)
		}
		for _, ev := range e.audits() {
			if strings.Contains(ev.Detail, needle) {
				t.Fatalf("审计泄露 %q: %s", needle, ev.Detail)
			}
		}
	}
	var sawUpdate bool
	for _, ev := range e.audits() {
		if ev.Event == "system.egress_update" {
			sawUpdate = true
			if strings.Contains(ev.Detail, "127.0.0.1:7891") {
				t.Fatalf("审计不应记代理地址: %s", ev.Detail)
			}
			if !strings.Contains(ev.Detail, "provider=") || !strings.Contains(ev.Detail, "routes=") {
				t.Fatalf("审计应记方式与路由: %s", ev.Detail)
			}
		}
	}
	if !sawUpdate {
		t.Fatal("缺少 system.egress_update 审计")
	}
}

func TestEgressUpstreamOverride(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, `{"name":"deepseek-main","type":"deepseek","api_key":"sk-deepseek-test-key-0001"}`)
	if up.EgressMode != "inherit" {
		t.Fatalf("新账号缺省 inherit，得到 %q", up.EgressMode)
	}
	// 未配代理时账号不能设为 proxy。
	resp := e.do("PATCH", "/admin/v1/upstreams/"+itoa(up.ID), root, `{"egress_mode":"proxy"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	resp = e.do("PATCH", "/admin/v1/upstreams/"+itoa(up.ID), root, `{"egress_mode":"bogus"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	// direct 覆盖随时可设。
	resp = e.do("PATCH", "/admin/v1/upstreams/"+itoa(up.ID), root, `{"egress_mode":"direct"}`)
	wantStatus(t, resp, http.StatusOK)

	resp = e.egressPatch(root, map[string]any{"provider": "socks5", "address": "10.0.0.9:1080"})
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("PATCH", "/admin/v1/upstreams/"+itoa(up.ID), root, `{"egress_mode":"proxy"}`)
	wantStatus(t, resp, http.StatusOK)
	var body struct {
		Upstream struct {
			EgressMode string `json:"egress_mode"`
		} `json:"upstream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Upstream.EgressMode != "proxy" {
		t.Fatalf("PATCH 应回 egress_mode=proxy: %v %+v", err, body)
	}
	got := e.egressRead(root)
	if len(got.Overrides) != 1 || got.Overrides[0].UpstreamID != up.ID || got.Overrides[0].EgressMode != "proxy" {
		t.Fatalf("overrides 应列出该账号: %+v", got.Overrides)
	}
	// 账号仍经代理：清除代理端点 409。
	resp = e.egressPatch(root, map[string]any{"provider": ""})
	wantStatus(t, resp, http.StatusConflict)
	resp = e.do("PATCH", "/admin/v1/upstreams/"+itoa(up.ID), root, `{"egress_mode":"inherit"}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.egressPatch(root, map[string]any{"provider": ""})
	wantStatus(t, resp, http.StatusOK)
	if got = e.egressRead(root); len(got.Overrides) != 0 {
		t.Fatalf("改回 inherit 后不应有 overrides: %+v", got.Overrides)
	}
}

func TestEgressTestEndpoint(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	resp := e.do("POST", "/admin/v1/system/egress/test", root, "{}")
	wantStatus(t, resp, http.StatusConflict)
	// 指向无人监听的本地端口：测试如实报 proxy_unreachable，不出网。
	resp = e.egressPatch(root, map[string]any{"provider": "socks5", "address": "127.0.0.1:9"})
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/egress/test", root, "{}")
	wantStatus(t, resp, http.StatusOK)
	got := e.egressRead(root)
	if got.LastTest == nil || got.LastTest.OK || got.LastTest.Category != "proxy_unreachable" || len(got.LastTest.Stages) != 1 || got.LastTest.Stages[0].Name != "proxy_tcp" {
		t.Fatalf("测试结果不对: %+v", got.LastTest)
	}
	var saw bool
	for _, ev := range e.audits() {
		if ev.Event == "system.egress_test" {
			saw = true
			if ev.Detail != "result=proxy_unreachable" {
				t.Fatalf("测试审计 detail = %q", ev.Detail)
			}
		}
	}
	if !saw {
		t.Fatal("缺少 system.egress_test 审计")
	}
}

func TestEgressRoutesAreLANOnlyForWrites(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	routes := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		routes[r.Pattern] = r.Exposure
	}
	want := map[string]tunnelctx.Exposure{
		"GET /admin/v1/system/egress":       tunnelctx.Admin,
		"PATCH /admin/v1/system/egress":     tunnelctx.LANOnly,
		"POST /admin/v1/system/egress/test": tunnelctx.LANOnly,
	}
	for pat, exp := range want {
		if got, ok := routes[pat]; !ok || got != exp {
			t.Errorf("%s 档位 = %v（登记 %v），期望 %v", pat, got, ok, exp)
		}
	}
}
