package landomain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSettings 是内存版 Settings；密封值原样存（本包只关心「有没有」与来回一致）。
type fakeSettings struct {
	mu sync.Mutex
	kv map[string]string
}

func newFakeSettings() *fakeSettings { return &fakeSettings{kv: map[string]string{}} }

func (f *fakeSettings) GetSetting(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kv[key], nil
}

func (f *fakeSettings) SetSetting(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = value
	return nil
}

func (f *fakeSettings) GetSealedSetting(ctx context.Context, key string) (string, error) {
	return f.GetSetting(ctx, key)
}

func (f *fakeSettings) SetSealedSetting(ctx context.Context, key, value string) error {
	return f.SetSetting(ctx, key, value)
}

// fakeListener 记录 StartDomainTLS/StopDomainTLS 调用。
type fakeListener struct {
	mu      sync.Mutex
	addr    string
	started int
	stopped int
	fail    error
}

func (l *fakeListener) StartDomainTLS(addr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
	l.addr = addr
	l.started++
	return nil
}

func (l *fakeListener) StopDomainTLS() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addr = ""
	l.stopped++
	return nil
}

func (l *fakeListener) DomainTLSAddr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addr == "" {
		return ""
	}
	return "127.0.0.1:8443"
}

// fakeSite 模拟官网设备侧接口：一次关联在第 approveAfter 次轮询后批准；证书订单用测试 CA
// 现签 CSR，第 issueAfter 次状态查询后发出。
type fakeSite struct {
	t  *testing.T
	mu sync.Mutex

	approveAfter int
	polls        int
	deny         bool

	token    string
	domain   map[string]string // label -> targetIP
	custom   string            // 登记的自有域名
	customIP string
	dnsReady bool // dns-check 里委托 CNAME 是否就位
	checks   int
	orders   int
	issued   bool
	statuses int
	failWith string
	released int
	unlinked int

	caKey   *ecdsa.PrivateKey
	caCert  *x509.Certificate
	lastCSR *x509.CertificateRequest
}

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Fake Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &fakeSite{t: t, approveAfter: 1, token: "dvt_" + strings.Repeat("a", 43), domain: map[string]string{}, caKey: key, caCert: cert}
}

func (f *fakeSite) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, status int, code, msg string) {
		writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	authed := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+f.token
	}
	mux.HandleFunc("POST /api/device/link/start", func(w http.ResponseWriter, r *http.Request) {
		var req LinkStartRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "h618-x98h" {
			writeErr(w, 400, "bad_model", "模型不对")
			return
		}
		writeJSON(w, 201, map[string]any{
			"deviceCode": "dvc_" + strings.Repeat("b", 43), "userCode": "K7QM-3WPX",
			"verificationUrl": "https://llm.net/link/", "verificationUrlComplete": "https://llm.net/link/?code=K7QM-3WPX",
			"expiresIn": 900, "interval": 0,
		})
	})
	mux.HandleFunc("POST /api/device/link/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		if f.deny {
			writeJSON(w, 200, map[string]any{"status": "denied", "interval": 5})
			return
		}
		if f.polls < f.approveAfter {
			writeJSON(w, 200, map[string]any{"status": "pending", "interval": 5})
			return
		}
		writeJSON(w, 200, map[string]any{"status": "approved", "token": f.token, "linkId": "dvl_1", "interval": 5,
			"account": map[string]string{"displayName": "测试账号"}})
	})
	mux.HandleFunc("GET /api/device/link", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		writeJSON(w, 200, map[string]any{"link": map[string]any{"id": "dvl_1", "status": "approved", "account": map[string]string{"displayName": "测试账号"}},
			"domain": nil, "lanDomain": map[string]any{"enabled": true, "suffix": "llm.net", "dns": true}})
	})
	mux.HandleFunc("POST /api/device/link/unlink", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		f.mu.Lock()
		f.unlinked++
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"unlinked": true})
	})
	mux.HandleFunc("POST /api/device/domain/claim", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		var req struct {
			Label    string `json:"label"`
			TargetIP string `json:"targetIp"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Label == "taken" {
			writeErr(w, 409, "label_taken", "这个前缀已被其他设备使用，请换一个")
			return
		}
		f.mu.Lock()
		f.domain[req.Label] = req.TargetIP
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"domain": map[string]any{"id": "dom_1", "label": req.Label, "hostname": req.Label + ".llm.net", "targetIp": req.TargetIP}})
	})
	mux.HandleFunc("POST /api/device/domain/custom", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		var req struct {
			Hostname string `json:"hostname"`
			TargetIP string `json:"targetIp"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Hostname == "taken.example.com" {
			writeErr(w, 409, "hostname_taken", "这个域名已被其他设备登记")
			return
		}
		f.mu.Lock()
		f.custom, f.customIP = req.Hostname, req.TargetIP
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"domain": map[string]any{"id": "dom_2", "kind": "custom", "label": nil, "hostname": req.Hostname,
			"targetIp": req.TargetIP, "acmeDelegate": "0123456789abcdef0123.acme.llm.net"}})
	})
	mux.HandleFunc("POST /api/device/domain/dns-check", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		var req struct {
			TargetIP string `json:"targetIp"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.checks++
		if f.custom == "" {
			writeErr(w, 409, "domain_not_custom", "这台设备没有登记自有域名")
			return
		}
		challenge := map[string]any{"status": "missing", "target": nil}
		if f.dnsReady {
			challenge = map[string]any{"status": "ok", "target": "0123456789abcdef0123.acme.llm.net"}
		}
		writeJSON(w, 200, map[string]any{"check": map[string]any{
			"hostname": f.custom, "acmeDelegate": "0123456789abcdef0123.acme.llm.net", "targetIp": req.TargetIP,
			"a":         map[string]any{"status": "ok", "addresses": []string{req.TargetIP}},
			"challenge": challenge,
			"caa":       map[string]any{"status": "none", "foundAt": nil, "records": []string{}, "permitted": []string{"google"}},
			"ready":     f.dnsReady, "checkedAt": time.Now().Unix(),
		}})
	})
	mux.HandleFunc("PUT /api/device/domain/target", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TargetIP string `json:"targetIp"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, 200, map[string]any{"domain": map[string]any{"id": "dom_1", "label": "boxes", "hostname": "boxes.llm.net", "targetIp": req.TargetIP}})
	})
	mux.HandleFunc("POST /api/device/domain/release", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.released++
		f.custom = ""
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"released": true})
	})
	mux.HandleFunc("POST /api/device/domain/certificate", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeErr(w, 401, "device_token_invalid", "令牌无效")
			return
		}
		var req struct {
			CSRPEM string `json:"csrPem"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		block, _ := pem.Decode([]byte(req.CSRPEM))
		if block == nil {
			writeErr(w, 400, "invalid_csr", "CSR 格式不正确")
			return
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || len(csr.DNSNames) != 1 {
			writeErr(w, 400, "invalid_csr", "CSR 的 SAN 必须只包含这台设备申领的域名")
			return
		}
		f.mu.Lock()
		if f.custom != "" && !f.dnsReady {
			f.mu.Unlock()
			writeErr(w, 409, "dns_delegation_missing", "公共 DNS 里还没有 _acme-challenge."+f.custom+" 的 CNAME 记录")
			return
		}
		f.lastCSR = csr
		f.orders++
		f.statuses = 0
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"order": map[string]any{"id": "ord_1", "hostname": csr.DNSNames[0], "ca": "google", "status": "pending", "retryAfter": 0}})
	})
	mux.HandleFunc("GET /api/device/domain/certificate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.statuses++
		if f.failWith != "" {
			writeJSON(w, 200, map[string]any{"order": map[string]any{"id": "ord_1", "status": "failed", "ca": "google", "error": f.failWith}})
			return
		}
		if f.statuses < 2 || f.lastCSR == nil {
			writeJSON(w, 200, map[string]any{"order": map[string]any{"id": "ord_1", "status": "challenging", "ca": "google", "retryAfter": 0}})
			return
		}
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: f.lastCSR.DNSNames[0]},
			DNSNames:     f.lastCSR.DNSNames,
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(90 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, f.caCert, f.lastCSR.PublicKey, f.caKey)
		if err != nil {
			f.t.Fatal(err)
		}
		chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) +
			string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw}))
		f.issued = true
		writeJSON(w, 200, map[string]any{"order": map[string]any{"id": "ord_1", "status": "issued", "ca": "google", "certificatePem": chain, "notAfter": tpl.NotAfter.Unix()}})
	})
	return mux
}

type harness struct {
	t        *testing.T
	site     *fakeSite
	settings *fakeSettings
	listener *fakeListener
	mgr      *Manager
	dir      string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	site := newFakeSite(t)
	srv := httptest.NewServer(site.handler())
	t.Cleanup(srv.Close)
	settings := newFakeSettings()
	listener := &fakeListener{}
	dir := t.TempDir()
	mgr := New(Options{
		DataDir: dir, Settings: settings, Site: NewSiteClient(srv.URL), Listener: listener,
		Model: "h618-x98h", DeviceName: "测试板", Version: "2608291200-abcd", PollInterval: 10 * time.Millisecond,
	})
	return &harness{t: t, site: site, settings: settings, listener: listener, mgr: mgr, dir: dir}
}

func (h *harness) linkAndClaim(label string) State {
	h.t.Helper()
	ctx := context.Background()
	if err := h.mgr.SetProvider(ctx, ProviderOfficialSite); err != nil {
		h.t.Fatalf("SetProvider: %v", err)
	}
	if _, err := h.mgr.StartLink(ctx); err != nil {
		h.t.Fatalf("StartLink: %v", err)
	}
	sess, err := h.mgr.WaitLink(ctx, 2*time.Second)
	if err != nil || sess.Status != "approved" {
		h.t.Fatalf("WaitLink: %v / %+v", err, sess)
	}
	st, err := h.mgr.Claim(ctx, label, "192.168.1.20")
	if err != nil {
		h.t.Fatalf("Claim: %v", err)
	}
	return st
}

func TestValidateLabel(t *testing.T) {
	for _, ok := range []string{"boxes", "studio-01", "a1b2c3"} {
		if err := ValidateLabel(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"ab", "four", strings.Repeat("a", 33), "-lead", "trail-", "Upper", "a--b", "underscore_"} {
		if err := ValidateLabel(bad); err == nil {
			t.Errorf("%q 应非法", bad)
		}
	}
}

func TestProviderRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var verr *ValidationError
	if err := h.mgr.SetProvider(ctx, "cloudflare"); !errors.As(err, &verr) {
		t.Fatalf("未知提供方 cloudflare 应按输入错误拒绝: %v", err)
	}
	if err := h.mgr.SetProvider(ctx, "bogus"); err == nil {
		t.Fatal("未知提供方应被拒")
	}
	if _, err := h.mgr.StartLink(ctx); !errors.Is(err, ErrProviderNotSet) {
		t.Fatalf("未选提供方就关联应被拒: %v", err)
	}
	if err := h.mgr.SetProvider(ctx, ProviderOwnDomain); err != nil {
		t.Fatalf("自有域名应可选: %v", err)
	}
	if _, err := h.mgr.Claim(ctx, "boxes", "192.168.1.20"); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("自有域名方式下申领托管前缀应答 ErrWrongProvider: %v", err)
	}
	if err := h.mgr.SetProvider(ctx, ProviderOfficialSite); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Claim(ctx, "boxes", "192.168.1.20"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("未关联就申领应被拒: %v", err)
	}
	if _, err := h.mgr.Register(ctx, "box.example.com", "192.168.1.20"); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("官网方式下登记自有域名应答 ErrWrongProvider: %v", err)
	}
	st := h.mgr.Status(ctx)
	if st.State.Provider != ProviderOfficialSite || st.State.Linked || st.State.Claimed() {
		t.Fatalf("状态不对: %+v", st.State)
	}
}

func TestLinkClaimIssueAndRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.site.approveAfter = 2

	st := h.linkAndClaim("boxes")
	if st.Hostname != "boxes.llm.net" || st.TargetIP != "192.168.1.20" || !st.Linked || st.Account != "测试账号" {
		t.Fatalf("申领后状态不对: %+v", st)
	}
	if tok := h.settings.kv[settingLinkToken]; tok != h.site.token {
		t.Fatalf("令牌未密封入库")
	}
	if got := h.mgr.Status(ctx); got.Link != nil && got.Link.Status != "approved" {
		t.Fatalf("关联会话状态不对: %+v", got.Link)
	}

	ch, err := h.mgr.StartIssue()
	if err != nil {
		t.Fatalf("StartIssue: %v", err)
	}
	if _, err := h.mgr.StartIssue(); !errors.Is(err, ErrIssueBusy) {
		t.Fatalf("签发中再签发应 busy: %v", err)
	}
	// 陪等：阶段一变就返回；最后一轮 finished=true 且无错。假官网第一次状态查询答 challenging，
	// 所以至少见到 submitting 之外的一个阶段。
	seen := map[string]bool{}
	since := ""
	for rounds := 0; ; rounds++ {
		finished, werr := h.mgr.WaitIssue(ctx, 2*time.Second, since)
		if werr != nil {
			t.Fatalf("WaitIssue: %v", werr)
		}
		if finished {
			break
		}
		st := h.mgr.Status(ctx)
		if !st.Issuing || st.IssueStage == "" {
			t.Fatalf("未结束时读数应带阶段: %+v", st)
		}
		seen[st.IssueStage] = true
		since = st.IssueStage
		if rounds > 20 {
			t.Fatal("陪等轮数过多")
		}
	}
	if !seen[StageChallenging] && !seen[StagePending] {
		t.Fatalf("应观察到官网订单阶段: %v", seen)
	}
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("签发失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("签发超时")
	}
	if !h.mgr.HasCertificate() {
		t.Fatal("签发后应持有证书")
	}
	if st := h.mgr.Status(ctx); st.Issuing || st.IssueStage != "" {
		t.Fatalf("结束后不该再有阶段: %+v", st)
	}
	if finished, werr := h.mgr.WaitIssue(ctx, time.Second, ""); !finished || werr != nil {
		t.Fatalf("没有签发在进行时应立即返回: %v %v", finished, werr)
	}
	status := h.mgr.Status(ctx)
	if status.Cert == nil || !status.Cert.CoversHostname || status.Cert.ExpiringSoon {
		t.Fatalf("证书读数不对: %+v", status.Cert)
	}
	if status.State.LastCA != "google" || status.State.LastError != "" || status.State.LastIssuedAt.IsZero() {
		t.Fatalf("签发后状态不对: %+v", status.State)
	}
	if h.listener.addr != DefaultHTTPSListen || status.HTTPSAddr == "" {
		t.Fatalf("签发后应启动 HTTPS 监听: %+v", h.listener)
	}
	if h.site.lastCSR.Subject.CommonName != "boxes.llm.net" || h.site.lastCSR.DNSNames[0] != "boxes.llm.net" {
		t.Fatalf("CSR 名字不对: %+v", h.site.lastCSR.Subject)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "lan-domain", certPendingKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("签发后 pending 私钥应删除: %v", err)
	}
	info, err := os.Stat(filepath.Join(h.dir, "lan-domain", certKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("私钥文件权限不对: %v %v", err, info)
	}
	cert, err := h.mgr.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert.Leaf == nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	// 重新构造 Manager：证书从磁盘恢复，Run 开机即拉起监听。
	again := New(Options{DataDir: h.dir, Settings: h.settings, Site: h.mgr.opt.Site, Listener: h.listener})
	if !again.HasCertificate() {
		t.Fatal("重启后应恢复证书")
	}
	if again.needsIssue(ctx) {
		t.Fatal("有效证书不应需要续期")
	}

	if err := h.mgr.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.site.released != 1 || h.listener.stopped == 0 || h.mgr.HasCertificate() {
		t.Fatalf("释放后应删证书并停监听: released=%d stopped=%d", h.site.released, h.listener.stopped)
	}
	after := h.mgr.Status(ctx)
	if after.State.Claimed() || !after.State.Linked {
		t.Fatalf("释放后应保留关联: %+v", after.State)
	}
	if err := h.mgr.SetProvider(ctx, ProviderNone); err != nil {
		t.Fatalf("释放后应能切回不使用: %v", err)
	}
}

func TestIssueFailureIsRecorded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.linkAndClaim("boxes")
	h.site.failWith = "CA 拒绝：DNS problem: NXDOMAIN"
	ch, err := h.mgr.StartIssue()
	if err != nil {
		t.Fatal(err)
	}
	err = <-ch
	if err == nil || !strings.Contains(err.Error(), "NXDOMAIN") {
		t.Fatalf("失败原因应透出: %v", err)
	}
	if finished, werr := h.mgr.WaitIssue(ctx, time.Second, ""); !finished || werr == nil || !strings.Contains(werr.Error(), "NXDOMAIN") {
		t.Fatalf("陪等应拿到这次签发的失败: %v %v", finished, werr)
	}
	st := h.mgr.Status(ctx)
	if !strings.Contains(st.State.LastError, "NXDOMAIN") || st.State.LastErrorAt.IsZero() {
		t.Fatalf("失败应记进状态: %+v", st.State)
	}
	if h.mgr.HasCertificate() || h.listener.started != 0 {
		t.Fatal("失败不该装证书或起监听")
	}
	// pending 私钥保留：下一次订单沿用同一把钥匙。
	if _, err := os.Stat(filepath.Join(h.dir, "lan-domain", certPendingKeyFile)); err != nil {
		t.Fatalf("失败后 pending 私钥应保留: %v", err)
	}
	if err := h.mgr.SetProvider(ctx, ProviderNone); !errors.Is(err, ErrDomainClaimed) {
		t.Fatalf("已申领时切回不使用应被拒: %v", err)
	}
}

func TestLinkDeniedAndUnlink(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.site.deny = true
	if err := h.mgr.SetProvider(ctx, ProviderOfficialSite); err != nil {
		t.Fatal(err)
	}
	first, err := h.mgr.StartLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := h.mgr.StartLink(ctx); err != nil || again.UserCode != first.UserCode {
		t.Fatalf("进行中再发起应幂等返回同一会话: %v %+v", err, again)
	}
	sess, err := h.mgr.WaitLink(ctx, time.Second)
	if err != nil || sess.Status != "denied" {
		t.Fatalf("应收到拒绝: %v %+v", err, sess)
	}
	if h.mgr.Status(ctx).State.Linked {
		t.Fatal("拒绝后不应关联")
	}
	// 拒绝是终态，可重新发起。
	h.site.deny = false
	h.site.polls = 0
	if _, err := h.mgr.StartLink(ctx); err != nil {
		t.Fatalf("终态后重新发起: %v", err)
	}
	if sess, err := h.mgr.WaitLink(ctx, time.Second); err != nil || sess.Status != "approved" {
		t.Fatalf("第二次应批准: %v %+v", err, sess)
	}
	if err := h.mgr.Unlink(ctx); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if h.site.unlinked != 1 || h.settings.kv[settingLinkToken] != "" || h.mgr.Status(ctx).State.Linked {
		t.Fatal("解除后本地令牌应清空且官网被通知")
	}
}

func TestSetHTTPSListenValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []string{"443", "host:443", ":0", ":70000"} {
		if err := h.mgr.SetHTTPSListen(ctx, bad); !errors.Is(err, ErrInvalidListen) {
			t.Errorf("%q 应非法: %v", bad, err)
		}
	}
	if err := h.mgr.SetHTTPSListen(ctx, "0.0.0.0:8443"); err != nil {
		t.Fatal(err)
	}
	if got := h.mgr.Status(ctx).State.HTTPSListen; got != "0.0.0.0:8443" {
		t.Fatalf("监听地址未保存: %s", got)
	}
	if err := h.mgr.SetHTTPSListen(ctx, ""); err != nil || h.mgr.Status(ctx).State.HTTPSListen != DefaultHTTPSListen {
		t.Fatalf("空串应回缺省: %v", err)
	}
}

func TestSensitiveValuesDoNotFormat(t *testing.T) {
	sess := LinkSession{UserCode: "K7QM-3WPX", Status: "pending", deviceCode: "dvc_secret"}
	for _, s := range []string{fmt.Sprint(sess), fmt.Sprintf("%+v", sess), fmt.Sprintf("%#v", sess), fmt.Sprintf("%v", &sess)} {
		if strings.Contains(s, "dvc_secret") {
			t.Fatalf("device code 泄漏: %s", s)
		}
	}
	poll := LinkPollResponse{Status: "approved", Token: "dvt_secret"}
	for _, s := range []string{fmt.Sprint(poll), fmt.Sprintf("%+v", poll), fmt.Sprintf("%#v", poll), fmt.Sprintf("%v", &poll)} {
		if strings.Contains(s, "dvt_secret") {
			t.Fatalf("token 泄漏: %s", s)
		}
	}
}

func TestValidateHostname(t *testing.T) {
	for _, ok := range []string{"box.example.com", "ai-box.lab.example.co.uk", "xn--fsq.example", "a.b"} {
		if err := ValidateHostname(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	var verr *ValidationError
	for _, bad := range []string{"", "box", "*.example.com", "192.168.1.20", "-box.example.com", "box-.example.com", "bo_x.example.com",
		"Box.Example.com", strings.Repeat("a", 64) + ".example.com", "box.llm.net", "llm.net"} {
		if err := ValidateHostname(bad); !errors.As(err, &verr) {
			t.Errorf("%q 应非法（ValidationError）: %v", bad, err)
		}
	}
}

func TestOwnDomainRegisterCheckIssueRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.mgr.SetProvider(ctx, ProviderOwnDomain); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.StartLink(ctx); err != nil {
		t.Fatalf("自有域名方式也能发起关联: %v", err)
	}
	if sess, err := h.mgr.WaitLink(ctx, 2*time.Second); err != nil || sess.Status != "approved" {
		t.Fatalf("WaitLink: %v / %+v", err, sess)
	}
	if _, err := h.mgr.DNSCheck(ctx); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("未登记就检查 DNS 应答 ErrNotClaimed: %v", err)
	}
	if _, err := h.mgr.Register(ctx, "taken.example.com", "192.168.1.20"); err == nil || !strings.Contains(err.Error(), "已被其他设备登记") {
		t.Fatalf("官网拒绝应透出: %v", err)
	}
	if _, err := h.mgr.Register(ctx, "Bad Name", "192.168.1.20"); err == nil {
		t.Fatal("非法域名应被预检拒绝")
	}
	st, err := h.mgr.Register(ctx, " Box.Example.COM. ", "192.168.1.20")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if st.Hostname != "box.example.com" || st.TargetIP != "192.168.1.20" || st.AcmeDelegate != "0123456789abcdef0123.acme.llm.net" || st.Label() != "" || st.Kind() != KindCustom {
		t.Fatalf("登记后状态不对: %+v", st)
	}
	records := st.DNSRecords()
	if len(records) != 2 || records[0].Type != "A" || records[0].Name != "box.example.com" || records[0].Value != "192.168.1.20" ||
		records[1].Type != "CNAME" || records[1].Name != "_acme-challenge.box.example.com" || records[1].Value != st.AcmeDelegate {
		t.Fatalf("要设的 DNS 记录不对: %+v", records)
	}
	if err := h.mgr.SetProvider(ctx, ProviderOfficialSite); !errors.Is(err, ErrDomainClaimed) {
		t.Fatalf("已登记时换提供方式应被拒: %v", err)
	}

	// 委托 CNAME 未就位：DNS 检查报 missing，签发被官网 409 挡住并记进状态。
	check, err := h.mgr.DNSCheck(ctx)
	if err != nil || check.Challenge.Status != "missing" || check.Ready || check.A.Status != "ok" {
		t.Fatalf("DNS 检查读数不对: %v %+v", err, check)
	}
	ch, err := h.mgr.StartIssue()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err == nil || !strings.Contains(err.Error(), "_acme-challenge.box.example.com") {
		t.Fatalf("委托缺失应透出官网原因: %v", err)
	}
	if h.mgr.HasCertificate() {
		t.Fatal("委托缺失不该拿到证书")
	}

	// 管理员设好 CNAME 后：检查 ready，签发成功并拉起监听。
	h.site.mu.Lock()
	h.site.dnsReady = true
	h.site.mu.Unlock()
	if check, err := h.mgr.DNSCheck(ctx); err != nil || !check.Ready || check.Challenge.Target != st.AcmeDelegate {
		t.Fatalf("就位后 DNS 检查读数不对: %v %+v", err, check)
	}
	ch, err = h.mgr.StartIssue()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	status := h.mgr.Status(ctx)
	if status.Cert == nil || !status.Cert.CoversHostname || status.State.LastError != "" || h.listener.addr != DefaultHTTPSListen {
		t.Fatalf("签发后状态不对: %+v", status)
	}
	if h.site.lastCSR.DNSNames[0] != "box.example.com" {
		t.Fatalf("CSR 名字不对: %+v", h.site.lastCSR.DNSNames)
	}
	// 自有域名不同步 IP：官网侧 target 端点不该被调用。
	h.mgr.SyncIP(ctx)

	if err := h.mgr.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	after := h.mgr.Status(ctx)
	if after.State.Claimed() || after.State.AcmeDelegate != "" || h.mgr.HasCertificate() || h.site.released != 1 {
		t.Fatalf("释放后应清空登记与证书: %+v", after.State)
	}
	if err := h.mgr.SetProvider(ctx, ProviderOfficialSite); err != nil {
		t.Fatalf("释放后应能换提供方式: %v", err)
	}
}
