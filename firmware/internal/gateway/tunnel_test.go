package gateway_test

// Tunnel origin socket 的可执行验收（docs-dev/firmware-cloudflare-tunnel.md §4.2、§4.3、
// §11.1）：Host / X-Forwarded-Proto / CF-Connecting-IP 语义、代理头剥离、两档
// allowlist、缺省口令下的管理面收窄、LAN 请求伪造转发头无效，以及 socket 的
// 建立/权限/移除。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// adminStub 冒充管理面 handler：带路由注册表（实现 tunnelctx.RouteLister），
// /admin/v1/echo 把它看到的请求来源与头回显成 JSON。
type adminStub struct {
	mux *tunnelctx.Mux
	mu  sync.Mutex
	// last 是最近一次 echo 看到的事实。
	last echoSeen
}

type echoSeen struct {
	Trusted   bool              `json:"trusted"`
	ClientIP  string            `json:"client_ip"`
	Hostname  string            `json:"hostname"`
	Secure    bool              `json:"secure"`
	Headers   map[string]string `json:"headers"`
	RemoteAdr string            `json:"remote_addr"`
}

func newAdminStub() *adminStub {
	a := &adminStub{mux: tunnelctx.NewMux()}
	echo := func(w http.ResponseWriter, r *http.Request) {
		info, ok := tunnelctx.From(r.Context())
		seen := echoSeen{Trusted: ok, ClientIP: info.ClientIP, Hostname: info.Hostname, Secure: tunnelctx.Secure(r), Headers: map[string]string{}, RemoteAdr: r.RemoteAddr}
		for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "CF-Connecting-IP", "X-Forwarded-Proto", "CF-Ray", tunnelctx.ProbeHeader} {
			if v := r.Header.Get(h); v != "" {
				seen.Headers[h] = v
			}
		}
		a.mu.Lock()
		a.last = seen
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(seen)
	}
	a.mux.HandleFunc("GET /admin/v1/echo", tunnelctx.Admin, echo)
	a.mux.HandleFunc("GET /ui/", tunnelctx.Admin, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ui")) })
	a.mux.HandleFunc("GET /gate-helper/{name}", tunnelctx.API, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("gate")) })
	a.mux.HandleFunc("PUT /admin/v1/system/network", tunnelctx.LANOnly, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("net")) })
	a.mux.HandleFunc("/", tunnelctx.LANOnly, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "admin-404", http.StatusNotFound) })
	return a
}

func (a *adminStub) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }
func (a *adminStub) TunnelRoutes() []tunnelctx.Route                  { return a.mux.Routes() }

type tunnelEnv struct {
	t      *testing.T
	s      *gateway.Server
	admin  *adminStub
	socket string
	client *http.Client
}

func newTunnelEnv(t *testing.T, pol tunnelctx.Policy) *tunnelEnv {
	t.Helper()
	admin := newAdminStub()
	cfg := &config.Config{Listen: "127.0.0.1:1"}
	s := gateway.New(cfg, logging.New(io.Discard, slog.LevelDebug), nil, nil, admin, nil)
	socket := filepath.Join(t.TempDir(), "origin.sock")
	if err := s.StartTunnel(socket, "no-such-group-for-test", pol); err != nil {
		t.Fatalf("StartTunnel: %v", err)
	}
	t.Cleanup(func() { s.StopTunnel() })
	client := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &tunnelEnv{t: t, s: s, admin: admin, socket: socket, client: client}
}

// get 经 socket 发一个请求；hdr 里 nil 值表示删除缺省头。
func (e *tunnelEnv) get(path, host string, hdr map[string]string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://origin"+path, nil)
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &body)
	return body.Error.Code
}

func TestTunnelGateHostProtoAndIP(t *testing.T) {
	e := newTunnelEnv(t, tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIOnly})

	if resp := e.get("/healthz", "box.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("正确 Host + https 的 /healthz = %d", resp.StatusCode)
	}
	if resp := e.get("/healthz", "BOX.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("Host 大小写应不敏感: %d", resp.StatusCode)
	}
	for _, host := range []string{"other.example.com", "box.example.com:443", "", "box.example.com.", "203.0.113.5"} {
		if resp := e.get("/healthz", host, nil); resp.StatusCode != http.StatusMisdirectedRequest || errCode(t, resp) != "host_mismatch" {
			t.Errorf("Host %q 应 421 host_mismatch，得到 %d", host, resp.StatusCode)
		}
	}
	for _, proto := range []string{"", "http", "https, https", "HTTPS "} {
		resp := e.get("/healthz", "box.example.com", map[string]string{"X-Forwarded-Proto": proto})
		if proto == "HTTPS " {
			if resp.StatusCode != http.StatusOK {
				t.Errorf("大小写/空白容忍的 https 应通过: %d", resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "https_required" {
			t.Errorf("X-Forwarded-Proto %q 应 400 https_required，得到 %d", proto, resp.StatusCode)
		}
	}
	// 多值 X-Forwarded-Proto：两个头字段。
	req, _ := http.NewRequest(http.MethodGet, "http://origin/healthz", nil)
	req.Host = "box.example.com"
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "https")
	if resp, err := e.client.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("多值 X-Forwarded-Proto 应拒: %v %v", err, resp)
	}
}

func TestTunnelProfilesAndAdminGate(t *testing.T) {
	e := newTunnelEnv(t, tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIOnly})
	// api_only：管理面路由与 LAN-only 路由都 404，数据面/接入路由可达。
	for _, path := range []string{"/admin/v1/echo", "/ui/", "/ui/network", "/admin/v1/system/network", "/", "/nonexistent"} {
		if resp := e.get(path, "box.example.com", nil); resp.StatusCode != http.StatusNotFound || errCode(t, resp) != "not_found" {
			t.Errorf("api_only 下 %s 应 404 not_found，得到 %d", path, resp.StatusCode)
		}
	}
	if resp := e.get("/gate-helper/install.sh", "box.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("api_only 下 gate 接入路由应可达: %d", resp.StatusCode)
	}
	// 数据面兜底：/v1/anything 是显式登记的模式，落到数据面自己的先认证再 404。
	if resp := e.get("/v1/anything", "box.example.com", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/anything 应落到数据面的 401，得到 %d", resp.StatusCode)
	}

	// api_and_admin 但口令仍是缺省（未注入闸门）：Admin 档照旧 404。
	e.s.UpdateTunnelPolicy(tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIAndAdmin})
	if resp := e.get("/admin/v1/echo", "box.example.com", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺省口令下管理面应收窄为 404，得到 %d", resp.StatusCode)
	}
	gate := false
	e.s.SetTunnelAdminGate(func() bool { return gate })
	if resp := e.get("/ui/", "box.example.com", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("闸门为假时 /ui/ 应 404，得到 %d", resp.StatusCode)
	}
	gate = true
	if resp := e.get("/ui/", "box.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("闸门为真时 /ui/ 应可达，得到 %d", resp.StatusCode)
	}
	// LAN-only 永远不暴露。
	req, _ := http.NewRequest(http.MethodPut, "http://origin/admin/v1/system/network", nil)
	req.Host = "box.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if resp, _ := e.client.Do(req); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("LAN-only 路由经 Tunnel 应 404: %v", resp)
	}
	// 可信来源写进 context，代理头被剥离，X-Forwarded-Proto 保留。
	resp := e.get("/admin/v1/echo", "box.example.com", map[string]string{
		"X-Forwarded-For": "10.0.0.1, 203.0.113.9", "X-Real-IP": "10.0.0.1", "Forwarded": "for=10.0.0.1", "CF-Ray": "abc",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("echo = %d", resp.StatusCode)
	}
	var seen echoSeen
	json.NewDecoder(resp.Body).Decode(&seen)
	if !seen.Trusted || seen.ClientIP != "203.0.113.9" || seen.Hostname != "box.example.com" || !seen.Secure {
		t.Fatalf("可信来源不对: %+v", seen)
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "CF-Connecting-IP"} {
		if _, ok := seen.Headers[h]; ok {
			t.Errorf("%s 应在进入业务路由前被删掉", h)
		}
	}
	if seen.Headers["X-Forwarded-Proto"] != "https" || seen.Headers["CF-Ray"] != "abc" {
		t.Errorf("X-Forwarded-Proto/CF-Ray 应保留: %+v", seen.Headers)
	}
	// 非法/缺失/多值 CF-Connecting-IP → unknown，不回退读 X-Forwarded-For。
	for _, ip := range []string{"", "not-an-ip", "203.0.113.9, 10.0.0.1", "fe80::1%eth0"} {
		resp := e.get("/admin/v1/echo", "box.example.com", map[string]string{"CF-Connecting-IP": ip, "X-Forwarded-For": "10.0.0.1"})
		var seen echoSeen
		json.NewDecoder(resp.Body).Decode(&seen)
		if seen.ClientIP != tunnelctx.UnknownIP {
			t.Errorf("CF-Connecting-IP %q 应记 unknown，得到 %q", ip, seen.ClientIP)
		}
	}
	resp = e.get("/admin/v1/echo", "box.example.com", map[string]string{"CF-Connecting-IP": "2001:DB8::1"})
	json.NewDecoder(resp.Body).Decode(&seen)
	if seen.ClientIP != "2001:db8::1" {
		t.Errorf("IPv6 应规范化: %q", seen.ClientIP)
	}
}

// TestTunnelProbeReceipt：公网探测的回执——登记过的 nonce 经 socket 到达即被标记并
// 在进入业务路由前剥离；没登记的不记；没经 socket（LAN 路径）的登记 End 为假。
// 这是分辨 Cloudflare 回源「unix socket」与「http://localhost:80」的依据。
func TestTunnelProbeReceipt(t *testing.T) {
	e := newTunnelEnv(t, tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIOnly})
	nonce := e.s.BeginTunnelProbe()
	if len(nonce) != 32 {
		t.Fatalf("nonce 应是 16 字节 hex: %q", nonce)
	}
	if e.s.EndTunnelProbe(nonce) {
		t.Fatal("没经 socket 的探测不该算到达")
	}
	// 经 socket 到达：标记，且 End 取走登记。
	nonce = e.s.BeginTunnelProbe()
	if resp := e.get("/gate-helper/install.sh", "box.example.com", map[string]string{tunnelctx.ProbeHeader: nonce}); resp.StatusCode != http.StatusOK {
		t.Fatalf("探测请求本身应照常服务: %d", resp.StatusCode)
	}
	if !e.s.EndTunnelProbe(nonce) {
		t.Fatal("经 socket 到达的探测应被标记")
	}
	if e.s.EndTunnelProbe(nonce) {
		t.Fatal("End 应取走登记")
	}
	// 即使 Host 不对（被闸门 421 拒掉），到了 socket 也算到达——回执只回答「走没走 socket」。
	nonce = e.s.BeginTunnelProbe()
	if resp := e.get("/healthz", "other.example.com", map[string]string{tunnelctx.ProbeHeader: nonce}); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("错 Host 应 421: %d", resp.StatusCode)
	}
	if !e.s.EndTunnelProbe(nonce) {
		t.Fatal("到了 socket 就该标记，与 Host 判定无关")
	}
	// 没登记的 nonce：闸门不记（公网上塞这个头撑不大登记表），End 恒假。
	e.get("/gate-helper/install.sh", "box.example.com", map[string]string{tunnelctx.ProbeHeader: "deadbeef"})
	if e.s.EndTunnelProbe("deadbeef") {
		t.Fatal("没登记的 nonce 不该被记录")
	}
	// 头在进入业务路由前被剥离。
	e.s.UpdateTunnelPolicy(tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIAndAdmin})
	e.s.SetTunnelAdminGate(func() bool { return true })
	nonce = e.s.BeginTunnelProbe()
	resp := e.get("/admin/v1/echo", "box.example.com", map[string]string{tunnelctx.ProbeHeader: nonce})
	var seen echoSeen
	json.NewDecoder(resp.Body).Decode(&seen)
	if _, ok := seen.Headers[tunnelctx.ProbeHeader]; ok {
		t.Fatal("探测头应在进入业务路由前被剥离")
	}
	if !e.s.EndTunnelProbe(nonce) {
		t.Fatal("经 socket 到达的探测应被标记")
	}
	// 走根 handler（LAN listener 的路径）：带这个头也不算到达——这正是回源填成
	// http://localhost:80 时的形态。
	nonce = e.s.BeginTunnelProbe()
	req := httptest.NewRequest(http.MethodGet, "http://box.example.com/gate-helper/install.sh", nil)
	req.Header.Set(tunnelctx.ProbeHeader, nonce)
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LAN 请求应照常服务: %d", rec.Code)
	}
	if e.s.EndTunnelProbe(nonce) {
		t.Fatal("LAN listener 上的请求不该算经 socket 到达")
	}
}

// TestLANRequestsIgnoreForgedHeaders：走根 handler（LAN listener 的路径）时，伪造
// 的转发头构造不出可信来源，Cookie Secure 判定也不受影响。
func TestLANRequestsIgnoreForgedHeaders(t *testing.T) {
	admin := newAdminStub()
	s := gateway.New(&config.Config{Listen: "127.0.0.1:1"}, logging.New(io.Discard, slog.LevelDebug), nil, nil, admin, nil)
	req := httptest.NewRequest(http.MethodGet, "http://box.example.com/admin/v1/echo", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LAN 请求应直达管理面: %d", rec.Code)
	}
	var seen echoSeen
	json.NewDecoder(rec.Body).Decode(&seen)
	if seen.Trusted || seen.Secure || seen.ClientIP != "" {
		t.Fatalf("LAN 请求不该被判成可信 Tunnel: %+v", seen)
	}
	if seen.Headers["CF-Connecting-IP"] != "203.0.113.9" {
		t.Fatal("LAN 路径不动请求头（它们只是不被信任）")
	}
}

func TestTunnelSocketLifecycle(t *testing.T) {
	e := newTunnelEnv(t, tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIOnly})
	info, err := os.Stat(e.socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("socket 权限 = %o，期望 0660", info.Mode().Perm())
	}
	if !e.s.TunnelListening() {
		t.Fatal("应在监听")
	}
	// 重复 Start 同一 socket：幂等，策略更新。
	if err := e.s.StartTunnel(e.socket, "", tunnelctx.Policy{Hostname: "box2.example.com", Profile: tunnelctx.ProfileAPIOnly}); err != nil {
		t.Fatal(err)
	}
	if resp := e.get("/healthz", "box2.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("重复 Start 后新 hostname 应生效: %d", resp.StatusCode)
	}
	if err := e.s.StopTunnel(); err != nil {
		t.Fatal(err)
	}
	if e.s.TunnelListening() {
		t.Fatal("停止后不该在监听")
	}
	if _, err := os.Stat(e.socket); !os.IsNotExist(err) {
		t.Fatal("停止后 socket 文件应被移除")
	}
	if err := e.s.StopTunnel(); err != nil {
		t.Fatalf("重复 Stop 应无事发生: %v", err)
	}
	if err := e.s.StartTunnel("", "", tunnelctx.Policy{}); err == nil {
		t.Fatal("空 socket 路径应拒")
	}
	// 策略缺失时闸门失败关闭。
	if err := e.s.StartTunnel(e.socket, "", tunnelctx.Policy{}); err != nil {
		t.Fatal(err)
	}
	if resp := e.get("/healthz", "box.example.com", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无策略时应 503: %d", resp.StatusCode)
	}
}
