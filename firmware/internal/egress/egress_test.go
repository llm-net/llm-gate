package egress_test

// 出站代理的离线测试：假 SOCKS5 服务器（egresstest）+ 本地 httptest TLS 目标，
// 覆盖 profile 校验与自遮蔽、直连/代理/来源覆盖的选路、失败关闭、域名交给代理解析、
// 错误分类、热切换、显式测试的分层结果。不依赖公网或真实代理。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/egress/egresstest"
)

// target 是一台只认 example.com 的本地 HTTPS 目标；假代理把 example.com 解析到它。
type target struct {
	srv  *httptest.Server
	port string
	pool *x509.CertPool
	// direct 计数：绕过代理直接打到目标的连接数（验证代理失败时不回落直连）。
	mu     sync.Mutex
	direct int
}

func newTarget(t *testing.T) *target {
	t.Helper()
	tg := &target{}
	tg.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Host", r.Host)
		fmt.Fprint(w, "hello")
	}))
	tg.srv.EnableHTTP2 = true
	tg.srv.StartTLS()
	t.Cleanup(tg.srv.Close)
	u, _ := url.Parse(tg.srv.URL)
	tg.port = u.Port()
	tg.pool = x509.NewCertPool()
	tg.pool.AddCert(tg.srv.Certificate())
	return tg
}

func (tg *target) resolve(host string) string {
	if host == "example.com" {
		return "127.0.0.1"
	}
	return host
}

// baseTransport 是「客户端自己的直连 Transport」：信任 httptest 证书，并把直连计数记在 target 上。
func (tg *target) baseTransport() *http.Transport {
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 同一个 DialContext 也被代理侧拿去连代理端点（沿用客户端的建连超时），
			// 所以只把打到目标端口的拨号算作「直连」。
			host, port, _ := net.SplitHostPort(addr)
			if port == tg.port {
				tg.mu.Lock()
				tg.direct++
				tg.mu.Unlock()
			}
			// 直连时 example.com 解析不到，指到本地端口（模拟系统 DNS）。
			if host == "example.com" {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2: true,
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: tg.pool}
	return tr
}

func (tg *target) directCount() int {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.direct
}

func (tg *target) url() string { return "https://example.com:" + tg.port + "/" }

func newManager(t *testing.T, opt egress.Options) *egress.Manager {
	t.Helper()
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return egress.NewManager(opt)
}

func mustApply(t *testing.T, m *egress.Manager, pol egress.Policy) {
	t.Helper()
	if err := m.Apply(pol); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func get(t *testing.T, c *http.Client, u string, mode egress.Mode) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(egress.WithMode(context.Background(), mode), http.MethodGet, u, nil)
	return c.Do(req)
}

func TestProfileValidate(t *testing.T) {
	ok := []egress.Profile{
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:7891"},
		{Provider: egress.ProviderClash, Address: "[::1]:7890"},
		{Provider: egress.ProviderSOCKS5, Address: "proxy.lan:1080", Username: "u", Password: "p"},
		{Provider: egress.ProviderSOCKS5, Address: "10.0.0.5:1080"},
		{},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%v: 期望合法，得到 %v", p, err)
		}
	}
	bad := []egress.Profile{
		{Provider: egress.ProviderSOCKS5, Address: ""},
		{Provider: egress.ProviderSOCKS5, Address: "socks5://127.0.0.1:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:0"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:70000"},
		{Provider: egress.ProviderSOCKS5, Address: "0.0.0.0:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "[::]:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "224.0.0.1:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "255.255.255.255:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "user:pw@127.0.0.1:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080/path"},
		{Provider: egress.ProviderSOCKS5, Address: "bad_host!:1080"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080", Username: "u"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080", Password: "p"},
		{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080", Username: strings.Repeat("u", 256), Password: "p"},
		{Provider: "http", Address: "127.0.0.1:1080"},
		{Provider: egress.ProviderNone, Address: "127.0.0.1:1080"},
	}
	for _, p := range bad {
		err := p.Validate()
		if err == nil {
			t.Errorf("%v: 期望拒绝", p)
			continue
		}
		if strings.Contains(err.Error(), "secret-pw") {
			t.Errorf("错误文本回显口令: %v", err)
		}
	}
}

func TestPolicyValidateRequiresProfileForProxy(t *testing.T) {
	pol := egress.Policy{Routes: egress.Routes{egress.ScopeModelAPI: egress.RouteProxy}}
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "模型与订阅接口") {
		t.Fatalf("无代理时选 proxy 应被拒并点名分类，得到 %v", err)
	}
	pol.Profile = egress.Profile{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080"}
	if err := pol.Validate(); err != nil {
		t.Fatalf("配好代理后应合法: %v", err)
	}
	if err := (egress.Policy{Routes: egress.Routes{"bogus": egress.RouteProxy}}).Validate(); err == nil {
		t.Fatal("未知分类应被拒")
	}
}

func TestProfileMasking(t *testing.T) {
	p := egress.Profile{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1080", Username: "alice-user", Password: "secret-pw"}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("profile", "p", p, "pp", &p)
	outputs := []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p),
		fmt.Sprintf("%v", &p), fmt.Sprintf("%+v", &p), fmt.Sprintf("%#v", &p), fmt.Sprint(p), buf.String()}
	for _, out := range outputs {
		if strings.Contains(out, "secret-pw") || strings.Contains(out, "alice-user") {
			t.Errorf("格式化输出泄露凭据: %s", out)
		}
		if !strings.Contains(out, "127.0.0.1:1080") {
			t.Errorf("格式化输出应含地址: %s", out)
		}
	}
	type wrap struct{ P egress.Profile }
	if out := fmt.Sprintf("%+v", wrap{p}); strings.Contains(out, "secret-pw") {
		t.Errorf("按值内嵌泄露凭据: %s", out)
	}
}

func TestRouteFor(t *testing.T) {
	pol := egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: "127.0.0.1:1"},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	}
	if got := pol.RouteFor(egress.ScopeModelAPI, egress.ModeInherit); got != egress.RouteProxy {
		t.Errorf("inherit 应跟随分类 proxy，得到 %s", got)
	}
	if got := pol.RouteFor(egress.ScopeModelAPI, egress.ModeDirect); got != egress.RouteDirect {
		t.Errorf("direct 覆盖应直连，得到 %s", got)
	}
	if got := pol.RouteFor(egress.ScopeAgentAuth, egress.ModeProxy); got != egress.RouteProxy {
		t.Errorf("proxy 覆盖应经代理，得到 %s", got)
	}
	if got := pol.RouteFor(egress.ScopeAgentAuth, egress.ModeInherit); got != egress.RouteDirect {
		t.Errorf("未选的分类缺省直连，得到 %s", got)
	}
	if egress.ModeFrom(context.Background()) != egress.ModeInherit {
		t.Error("空 ctx 应为 inherit")
	}
	if egress.ModeFrom(egress.WithMode(context.Background(), egress.ModeProxy)) != egress.ModeProxy {
		t.Error("ctx 覆盖读不回")
	}
}

func TestDefaultIsDirect(t *testing.T) {
	tg := newTarget(t)
	m := newManager(t, egress.Options{})
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}
	resp, err := get(t, hc, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("缺省直连失败: %v", err)
	}
	resp.Body.Close()
	if tg.directCount() != 1 {
		t.Fatalf("直连计数 = %d，期望 1", tg.directCount())
	}
	var nilMgr *egress.Manager
	if rt := egress.TransportFor(nilMgr, egress.ScopeModelAPI, tg.baseTransport()); rt == nil {
		t.Fatal("nil Manager 也应给出直连 Transport")
	}
}

func TestProxyRouteUsesSOCKS5AndRemoteDNS(t *testing.T) {
	tg := newTarget(t)
	fake := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderClash, Address: fake.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}
	for i := 0; i < 2; i++ {
		resp, err := get(t, hc, tg.url(), egress.ModeInherit)
		if err != nil {
			t.Fatalf("经代理请求失败: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "hello" || resp.Header.Get("X-Host") != "example.com:"+tg.port {
			t.Fatalf("响应不对: %q host=%q", body, resp.Header.Get("X-Host"))
		}
		if resp.ProtoMajor != 2 {
			t.Errorf("经隧道应协商上 HTTP/2，得到 %s", resp.Proto)
		}
	}
	conns := fake.Connects()
	if len(conns) != 1 {
		t.Fatalf("代理应只收到 1 次 CONNECT（连接池复用），得到 %d", len(conns))
	}
	if conns[0].ATYP != 3 || conns[0].Host != "example.com" {
		t.Fatalf("目标应以域名交给代理解析，得到 ATYP=%d host=%q", conns[0].ATYP, conns[0].Host)
	}
	if tg.directCount() != 0 {
		t.Fatalf("经代理时不应有直连，计数 %d", tg.directCount())
	}
	// 其他分类仍直连。
	hc2 := &http.Client{Transport: m.Transport(egress.ScopeOfficialSite, tg.baseTransport())}
	resp, err := get(t, hc2, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("其他分类直连失败: %v", err)
	}
	resp.Body.Close()
	if tg.directCount() != 1 || len(fake.Connects()) != 1 {
		t.Fatalf("official_site 应直连: direct=%d proxied=%d", tg.directCount(), len(fake.Connects()))
	}
}

func TestSourceOverride(t *testing.T) {
	tg := newTarget(t)
	fake := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()}})
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}
	// 分类直连，来源强制 proxy。
	resp, err := get(t, hc, tg.url(), egress.ModeProxy)
	if err != nil {
		t.Fatalf("来源 proxy 覆盖失败: %v", err)
	}
	resp.Body.Close()
	if len(fake.Connects()) != 1 || tg.directCount() != 0 {
		t.Fatalf("proxy 覆盖应经代理: proxied=%d direct=%d", len(fake.Connects()), tg.directCount())
	}
	// 分类 proxy，来源强制 direct。
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	resp, err = get(t, hc, tg.url(), egress.ModeDirect)
	if err != nil {
		t.Fatalf("来源 direct 覆盖失败: %v", err)
	}
	resp.Body.Close()
	if len(fake.Connects()) != 1 || tg.directCount() != 1 {
		t.Fatalf("direct 覆盖应直连: proxied=%d direct=%d", len(fake.Connects()), tg.directCount())
	}
}

func TestFailClosed(t *testing.T) {
	tg := newTarget(t)
	m := newManager(t, egress.Options{})
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}

	// 代理未配置但来源要求 proxy：失败关闭，不直连。
	_, err := get(t, hc, tg.url(), egress.ModeProxy)
	assertCategory(t, err, egress.CategoryUnconfigured)
	if tg.directCount() != 0 {
		t.Fatalf("未配置代理时不得直连，计数 %d", tg.directCount())
	}

	// 代理端口无人监听：unreachable，仍不直连。
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: dead},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	_, err = get(t, hc, tg.url(), egress.ModeInherit)
	assertCategory(t, err, egress.CategoryUnreachable)
	if tg.directCount() != 0 {
		t.Fatalf("代理不可达时不得直连，计数 %d", tg.directCount())
	}
	st := m.Status()
	if pe, ok := st.LastErrors[egress.ScopeModelAPI]; !ok || pe.Category != egress.CategoryUnreachable {
		t.Fatalf("被动错误未记录: %+v", st.LastErrors)
	}

	// 明文目标经代理：拒绝。
	_, err = get(t, hc, "http://example.com:"+tg.port+"/", egress.ModeInherit)
	assertCategory(t, err, egress.CategoryPlaintextRefused)
	if tg.directCount() != 0 {
		t.Fatalf("明文拒绝也不得直连，计数 %d", tg.directCount())
	}
}

func TestErrorCategories(t *testing.T) {
	tg := newTarget(t)
	cases := []struct {
		name string
		opt  egresstest.Options
		auth bool
		want egress.Category
	}{
		{"auth required, none configured", egresstest.Options{Username: "u", Password: "p"}, false, egress.CategoryAuthFailed},
		{"auth rejected", egresstest.Options{Username: "u", Password: "other"}, true, egress.CategoryAuthFailed},
		{"connect refused by proxy", egresstest.Options{Reply: 0x05}, false, egress.CategoryTargetFailed},
		{"ruleset", egresstest.Options{Reply: 0x02}, false, egress.CategoryTargetFailed},
		{"bad version", egresstest.Options{BadVersion: true}, false, egress.CategoryProtocol},
		{"close on greeting", egresstest.Options{CloseOnGreeting: true}, false, egress.CategoryProtocol},
		{"handshake hang", egresstest.Options{Hang: true}, false, egress.CategoryTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opt.Resolve = tg.resolve
			fake := egresstest.New(t, tc.opt)
			m := newManager(t, egress.Options{HandshakeTimeout: 300 * time.Millisecond})
			prof := egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()}
			if tc.auth {
				prof.Username, prof.Password = "u", "p"
			}
			mustApply(t, m, egress.Policy{Profile: prof, Routes: egress.Routes{egress.ScopeModelAPI: egress.RouteProxy}})
			hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}
			_, err := get(t, hc, tg.url(), egress.ModeInherit)
			assertCategory(t, err, tc.want)
			// http.Client 外层的 url.Error 一如直连时带目标 URL；本包的错误值本身不含代理地址与目标。
			if e, _ := egress.Classify(err); strings.Contains(e.Error(), fake.Addr()) || strings.Contains(e.Error(), "example.com") {
				t.Errorf("错误文本不应含代理地址或目标: %v", e)
			}
			if tc.want == egress.CategoryTimeout {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					t.Errorf("超时错误应满足 net.Error.Timeout: %v", err)
				}
			}
			if tg.directCount() != 0 {
				t.Fatalf("代理失败不得直连，计数 %d", tg.directCount())
			}
		})
	}
}

func TestAuthSuccess(t *testing.T) {
	tg := newTarget(t)
	fake := egresstest.New(t, egresstest.Options{Username: "alice", Password: "s3cret", Resolve: tg.resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr(), Username: "alice", Password: "s3cret"},
		Routes:  egress.Routes{egress.ScopeCLIArtifacts: egress.RouteProxy},
	})
	hc := &http.Client{Transport: m.Transport(egress.ScopeCLIArtifacts, tg.baseTransport())}
	resp, err := get(t, hc, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("认证代理失败: %v", err)
	}
	resp.Body.Close()
	if c := fake.Connects(); len(c) != 1 || !c[0].Authenticated {
		t.Fatalf("应完成认证: %+v", c)
	}
}

func TestTLSFailureClassified(t *testing.T) {
	tg := newTarget(t)
	fake := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	// 不信任 httptest 证书的 base：经隧道后证书校验必须照常失败。
	base := tg.baseTransport()
	base.TLSClientConfig = nil
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, base)}
	_, err := get(t, hc, tg.url(), egress.ModeInherit)
	assertCategory(t, err, egress.CategoryTLSFailed)
}

func TestHotSwap(t *testing.T) {
	tg := newTarget(t)
	fakeA := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	fakeB := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fakeA.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, tg.baseTransport())}
	resp, err := get(t, hc, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("经 A 失败: %v", err)
	}
	resp.Body.Close()
	// 切到 B：旧连接池的空闲连接被关掉，新请求经 B。
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fakeB.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	resp, err = get(t, hc, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("经 B 失败: %v", err)
	}
	resp.Body.Close()
	if len(fakeA.Connects()) != 1 || len(fakeB.Connects()) != 1 {
		t.Fatalf("切换后新请求应经 B: A=%d B=%d", len(fakeA.Connects()), len(fakeB.Connects()))
	}
	// 切回直连：不再碰任何代理。
	mustApply(t, m, egress.Policy{Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fakeB.Addr()}})
	resp, err = get(t, hc, tg.url(), egress.ModeInherit)
	if err != nil {
		t.Fatalf("切回直连失败: %v", err)
	}
	resp.Body.Close()
	if tg.directCount() != 1 || len(fakeB.Connects()) != 1 {
		t.Fatalf("切回直连后应直连: direct=%d B=%d", tg.directCount(), len(fakeB.Connects()))
	}
	hc.CloseIdleConnections()
}

func TestInFlightRequestFinishesOnOldPath(t *testing.T) {
	// 慢目标：请求发出后再切换策略，响应仍从原路径完整回来。
	release := make(chan struct{})
	slow := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, "late")
	}))
	slow.StartTLS()
	defer slow.Close()
	u, _ := url.Parse(slow.URL)
	pool := x509.NewCertPool()
	pool.AddCert(slow.Certificate())
	resolve := func(h string) string {
		if h == "example.com" {
			return "127.0.0.1"
		}
		return h
	}
	fake := egresstest.New(t, egresstest.Options{Resolve: resolve})
	m := newManager(t, egress.Options{})
	mustApply(t, m, egress.Policy{
		Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()},
		Routes:  egress.Routes{egress.ScopeModelAPI: egress.RouteProxy},
	})
	base := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool}}
	hc := &http.Client{Transport: m.Transport(egress.ScopeModelAPI, base)}
	done := make(chan error, 1)
	go func() {
		resp, err := get(t, hc, "https://example.com:"+u.Port()+"/", egress.ModeInherit)
		if err != nil {
			done <- err
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "late" {
			done <- fmt.Errorf("body=%q", body)
			return
		}
		done <- nil
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(fake.Connects()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mustApply(t, m, egress.Policy{}) // 切回全直连
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("在飞请求应按旧路径完成: %v", err)
	}
}

func TestManagerTestLayers(t *testing.T) {
	tg := newTarget(t)
	fake := egresstest.New(t, egresstest.Options{Resolve: tg.resolve})
	m := newManager(t, egress.Options{TestTarget: tg.url(), TestRootCAs: tg.pool})
	if _, err := m.Test(context.Background()); !errors.Is(err, egress.ErrNotConfigured) {
		t.Fatalf("未配置时应答 ErrNotConfigured，得到 %v", err)
	}
	mustApply(t, m, egress.Policy{Profile: egress.Profile{Provider: egress.ProviderClash, Address: fake.Addr()}})
	res, err := m.Test(context.Background())
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK || len(res.Stages) != 4 || res.Target != "example.com" {
		t.Fatalf("分层结果不对: %+v", res)
	}
	for _, st := range res.Stages {
		if !st.OK || st.LatencyMS <= 0 {
			t.Errorf("阶段 %s 应通过: %+v", st.Name, st)
		}
	}
	if got := m.Status().LastTest; got == nil || !got.OK {
		t.Fatalf("Status 应带最近测试: %+v", got)
	}
	// 认证失败停在 socks5 层。
	bad := egresstest.New(t, egresstest.Options{Username: "u", Password: "p", Resolve: tg.resolve})
	mustApply(t, m, egress.Policy{Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: bad.Addr()}})
	res, _ = m.Test(context.Background())
	if res.OK || res.Category != egress.CategoryAuthFailed || len(res.Stages) != 2 || res.Stages[1].Name != "socks5" {
		t.Fatalf("认证失败应停在 socks5 层: %+v", res)
	}
	// 证书不受信任停在 tls 层。
	m2 := newManager(t, egress.Options{TestTarget: tg.url()})
	mustApply(t, m2, egress.Policy{Profile: egress.Profile{Provider: egress.ProviderSOCKS5, Address: fake.Addr()}})
	res, _ = m2.Test(context.Background())
	if res.OK || res.Category != egress.CategoryTLSFailed || len(res.Stages) != 3 {
		t.Fatalf("不受信证书应停在 tls 层: %+v", res)
	}
}

// memSettings 是内存版 Settings：验证 Update 的持久化形状（口令走 sealed 一侧）与 Load 回读。
type memSettings struct {
	mu     sync.Mutex
	plain  map[string]string
	sealed map[string]string
	fail   bool
}

func (m *memSettings) GetSetting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plain[key], nil
}

func (m *memSettings) GetSealedSetting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return "", errors.New("解封失败")
	}
	return m.sealed[key], nil
}

func (m *memSettings) SetSettingsAtomic(_ context.Context, plain, sealed map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plain == nil {
		m.plain, m.sealed = map[string]string{}, map[string]string{}
	}
	for k, v := range plain {
		m.plain[k] = v
	}
	for k, v := range sealed {
		m.sealed[k] = v
	}
	return nil
}

func str(s string) *string { return &s }

func TestUpdateAndLoad(t *testing.T) {
	st := &memSettings{}
	m := newManager(t, egress.Options{Settings: st})
	// 未配代理就选 proxy：拒绝，不落库。
	_, err := m.Update(context.Background(), egress.Change{Routes: map[egress.Scope]egress.Route{egress.ScopeModelAPI: egress.RouteProxy}})
	var ve *egress.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("应为校验错误，得到 %v", err)
	}
	if len(st.plain) != 0 {
		t.Fatal("校验失败不应落库")
	}
	sum, err := m.Update(context.Background(), egress.Change{
		Provider: str("clash"), Address: str(" 127.0.0.1:7891 "), Username: str("alice"), Password: str("secret-pw"),
		Routes: map[egress.Scope]egress.Route{egress.ScopeModelAPI: egress.RouteProxy, egress.ScopeAgentAuth: egress.RouteProxy},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !sum.ProviderChanged || !sum.AddressChanged || !sum.AuthReplaced || !sum.RoutesChanged {
		t.Fatalf("Summary 不对: %+v", sum)
	}
	for k, v := range st.plain {
		if strings.Contains(v, "secret-pw") || strings.Contains(v, "alice") {
			t.Fatalf("明文项 %s 含凭据", k)
		}
	}
	if st.sealed["egress.password"] != "secret-pw" || st.sealed["egress.username"] != "alice" {
		t.Fatalf("凭据应走密封一侧: %+v", st.sealed)
	}
	status := m.Status()
	if status.Provider != egress.ProviderClash || status.Address != "127.0.0.1:7891" || !status.UsernameSet || !status.PasswordSet ||
		status.Routes[egress.ScopeModelAPI] != egress.RouteProxy || status.Routes[egress.ScopeOfficialSite] != egress.RouteDirect {
		t.Fatalf("Status 不对: %+v", status)
	}
	// 省略即保留：只改路由，凭据不动。
	if _, err := m.Update(context.Background(), egress.Change{Routes: map[egress.Scope]egress.Route{egress.ScopeAgentAuth: egress.RouteDirect}}); err != nil {
		t.Fatalf("Update routes: %v", err)
	}
	if p := m.Policy(); p.Profile.Password != "secret-pw" || p.Routes[egress.ScopeAgentAuth] != egress.RouteDirect {
		t.Fatalf("省略字段应保留: %+v", p.Routes)
	}
	// 空串即清除。
	sum, err = m.Update(context.Background(), egress.Change{Username: str(""), Password: str("")})
	if err != nil || !sum.AuthCleared || m.Policy().Profile.HasAuth() {
		t.Fatalf("清除认证失败: %v %+v", err, sum)
	}
	// 清代理但仍有分类选 proxy：拒绝。
	if _, err := m.Update(context.Background(), egress.Change{Provider: str("")}); err == nil {
		t.Fatal("仍有分类经代理时清除 profile 应被拒")
	}
	// 同一提交把分类改回直连即可清除。
	if _, err := m.Update(context.Background(), egress.Change{Provider: str(""), Routes: map[egress.Scope]egress.Route{egress.ScopeModelAPI: egress.RouteDirect}}); err != nil {
		t.Fatalf("同提交改回直连应允许: %v", err)
	}
	if m.Status().Configured {
		t.Fatal("清除后应为未配置")
	}

	// Load 回读：解封失败 → 保留路由但不可用、失败关闭。
	st2 := &memSettings{plain: map[string]string{
		"egress.provider": "socks5", "egress.address": "127.0.0.1:1080",
		"egress.routes": `{"model_api":"proxy"}`,
	}, sealed: map[string]string{"egress.username": "u", "egress.password": "p"}, fail: true}
	m2 := newManager(t, egress.Options{Settings: st2})
	if err := m2.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	s2 := m2.Status()
	if !s2.Configured || s2.Available || s2.UnavailableReason == "" || s2.Routes[egress.ScopeModelAPI] != egress.RouteProxy {
		t.Fatalf("解封失败的读数不对: %+v", s2)
	}
	tg := newTarget(t)
	hc := &http.Client{Transport: m2.Transport(egress.ScopeModelAPI, tg.baseTransport())}
	_, err = get(t, hc, tg.url(), egress.ModeInherit)
	assertCategory(t, err, egress.CategoryUnconfigured)
	if tg.directCount() != 0 {
		t.Fatal("解封失败不得降级直连")
	}
	st2.fail = false
	if err := m2.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s := m2.Status(); !s.Available || !s.UsernameSet {
		t.Fatalf("解封成功后应可用: %+v", s)
	}
}

func assertCategory(t *testing.T, err error, want egress.Category) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望失败（%s），得到成功", want)
	}
	e, ok := egress.Classify(err)
	if !ok {
		t.Fatalf("期望 egress 分类错误（%s），得到 %T: %v", want, err, err)
	}
	if e.Category != want {
		t.Fatalf("分类 = %s，期望 %s（%v）", e.Category, want, err)
	}
}
