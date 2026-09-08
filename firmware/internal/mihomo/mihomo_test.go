package mihomo

// 板上内核组件的离线验收：签名清单（官方 URL 策略、gzip 声明、防回退）、订阅解析
//（只取 proxies、剔除不支持的协议与危险键、重名编号）、受限配置生成（只绑回环、选定节点
// 排首位、只有 MATCH 规则）、管理器全流程（导入清单 → 上传 gzip 制品 → 安装 → 订阅 → 启用
// → 选节点 → 停用），假引擎 / 假官网 / 假出站策略，全部离线。

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
	"gopkg.in/yaml.v3"
)

// ---- 夹具 ----

// elfHeader 是 elfcheck 认的本机架构 ELF 头（与 cloudflared 测试同一造法）。
func fakeELF(version string) []byte {
	b := make([]byte, 64+len(version))
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b[16], b[17] = 2, 0 // e_type EXEC
	// e_machine 按本机：x86-64 0x3e / aarch64 0xb7
	switch HostPlatform() {
	case "linux-arm64":
		b[18], b[19] = 0xb7, 0
	default:
		b[18], b[19] = 0x3e, 0
	}
	b[20] = 1 // e_version
	copy(b[64:], version)
	return b
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	keys map[string]ed25519.PublicKey
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{pub: pub, priv: priv, keys: map[string]ed25519.PublicKey{"test-key": pub}}
}

func (s *signer) sign(t *testing.T, idx Index) (raw, sig []byte) {
	t.Helper()
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	sf := signatureFile{Schema: SignatureSchema, KeyID: "test-key", Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, raw))}
	sig, _ = json.Marshal(sf)
	return raw, sig
}

func release(version string, gz, elf []byte) Release {
	return Release{
		Version: version, Platform: HostPlatform(),
		ArtifactURL:    "https://github.com/MetaCubeX/mihomo/releases/download/v" + version + "/mihomo-" + HostPlatform() + "-v" + version + ".gz",
		ArtifactSHA256: sum(gz), SizeBytes: int64(len(gz)), Packaging: "gzip",
		UnpackedSHA256: sum(elf), UnpackedSizeBytes: int64(len(elf)),
		License: "GPL-3.0", SourceURL: "https://github.com/MetaCubeX/mihomo/tree/v" + version,
		MinComponentManager: 1, AllowInstall: true, AllowUpdate: true,
	}
}

func index(rev int64, rels ...Release) Index {
	return Index{Schema: IndexSchema, Component: ComponentName, Channel: "stable", Revision: rev, UpdatedAt: "2026-08-30T00:00:00Z", Releases: rels}
}

type fakeSettings struct {
	mu     sync.Mutex
	plain  map[string]string
	sealed map[string]string
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{plain: map[string]string{}, sealed: map[string]string{}}
}

func (f *fakeSettings) GetSetting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plain[k], nil
}

func (f *fakeSettings) SetSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[k] = v
	return nil
}

func (f *fakeSettings) GetSealedSetting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sealed[k], nil
}

func (f *fakeSettings) SetSealedSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed[k] = v
	return nil
}

type fakeEngine struct {
	mu        sync.Mutex
	installed *updated.ComponentSlot
	previous  *updated.ComponentSlot
	running   bool
	config    string
	starts    int
	stops     int
	failStart bool
}

func (f *fakeEngine) status() *updated.ComponentStatus {
	st := &updated.ComponentStatus{Name: ComponentName}
	if f.installed != nil {
		st.Installed, st.CurrentSlot, st.Current = true, "a", f.installed
	}
	if f.previous != nil {
		st.PreviousSlot, st.Previous = "b", f.previous
	}
	return st
}

func (f *fakeEngine) ComponentStatus(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status(), nil
}

func (f *fakeEngine) ComponentInstall(_ context.Context, _ string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, err
	}
	if sum(raw) != req.SHA256 {
		return nil, errors.New("摘要不符")
	}
	f.previous = f.installed
	f.installed = &updated.ComponentSlot{Version: req.Version, SHA256: req.SHA256, SizeBytes: int64(len(raw))}
	return f.status(), nil
}

func (f *fakeEngine) ComponentRollback(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.previous == nil {
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrNoPreviousSlot.Error()}
	}
	f.installed, f.previous = f.previous, f.installed
	return f.status(), nil
}

func (f *fakeEngine) ComponentRemove(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed, f.previous, f.running, f.config = nil, nil, false, ""
	return f.status(), nil
}

func (f *fakeEngine) proxyStatus() *updated.ProxyStatus {
	st := &updated.ProxyStatus{UnitState: "inactive", Component: f.status(), ConfigPresent: f.config != ""}
	if f.running {
		st.UnitState, st.Ready = "active", true
	}
	return st
}

func (f *fakeEngine) ProxyStatus(context.Context) (*updated.ProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proxyStatus(), nil
}

func (f *fakeEngine) ProxyStart(_ context.Context, config string) (*updated.ProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed == nil {
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrComponentMissing.Error()}
	}
	if f.failStart {
		return f.proxyStatus(), errors.New("未就绪")
	}
	if !f.running || f.config != config {
		f.starts++
	}
	f.running, f.config = true, config
	return f.proxyStatus(), nil
}

func (f *fakeEngine) ProxyStop(context.Context) (*updated.ProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running, f.config = false, ""
	f.stops++
	return f.proxyStatus(), nil
}

type fakeEgress struct {
	mu     sync.Mutex
	core   bool
	addr   string
	forced bool
}

func (f *fakeEgress) SetCoreAddress(_ context.Context, addr string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addr, f.forced = addr, force
	return nil
}

func (f *fakeEgress) ProviderIsCore() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.core
}

type fakeWebsite struct {
	index, sig []byte
	artifact   []byte
}

func (w *fakeWebsite) FetchComponentIndex(context.Context, string) ([]byte, []byte, error) {
	if w.index == nil {
		return nil, nil, errors.New("无法连接 LLM Gate官网")
	}
	return w.index, w.sig, nil
}

func (w *fakeWebsite) FetchComponentArtifact(_ context.Context, _ string, pol officialsite.ArtifactPolicy, out io.Writer) (string, int64, error) {
	if int64(len(w.artifact)) > pol.MaxBytes {
		return "", 0, errors.New("超过上限")
	}
	n, err := out.Write(w.artifact)
	return sum(w.artifact), int64(n), err
}

const subscriptionFixture = `port: 7890
socks-port: 7891
allow-lan: true
mode: rule
external-controller: 0.0.0.0:9090
dns: {enable: true, listen: 0.0.0.0:53}
proxies:
  - {name: "香港 01", type: ss, server: hk1.example.net, port: 443, cipher: aes-128-gcm, password: fixture-pw-1, udp: true, interface-name: eth0}
  - {name: "香港 01", type: ss, server: hk2.example.net, port: 443, cipher: aes-128-gcm, password: fixture-pw-2, routing-mark: 255}
  - {name: "日本 AnyTLS", type: anytls, server: 203.0.113.9, port: 8443, password: fixture-pw-3}
  - {name: "坏端口", type: ss, server: hk3.example.net, port: 70000, cipher: aes-128-gcm, password: x}
  - {name: "不支持", type: warp, server: 1.1.1.1, port: 1}
  - {name: "LLM Gate", type: trojan, server: sg.example.net, port: 443, password: fixture-pw-4}
proxy-groups:
  - {name: 代理, type: select, proxies: ["香港 01"]}
rules:
  - MATCH,代理
`

// ---- 清单 ----

func TestManifestPolicy(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("1.19.30")
	gz := gzipBytes(t, elf)
	good := release("1.19.30", gz, elf)
	raw, sig := s.sign(t, index(1, good))
	v, err := ParseIndex(raw, sig, s.keys)
	if err != nil || v.Index.Revision != 1 {
		t.Fatalf("合法清单被拒: %v", err)
	}
	if _, err := ParseIndex(raw, sig, map[string]ed25519.PublicKey{}); err == nil {
		t.Fatal("未知密钥应拒")
	}
	raw2 := bytes.Replace(raw, []byte(`"revision":1`), []byte(`"revision":2`), 1)
	if _, err := ParseIndex(raw2, sig, s.keys); err == nil {
		t.Fatal("篡改正文应拒")
	}
	bad := []func(r *Release){
		func(r *Release) {
			r.ArtifactURL = "https://github.com/MetaCubeX/mihomo/releases/latest/download/mihomo.gz"
		},
		func(r *Release) { r.ArtifactURL = "https://example.com/mihomo.gz" },
		func(r *Release) { r.ArtifactURL = strings.Replace(r.ArtifactURL, "v1.19.30/", "v1.19.31/", 1) },
		func(r *Release) { r.Packaging = "zip" },
		func(r *Release) { r.License = "MIT" },
		func(r *Release) { r.UnpackedSHA256 = "ABC" },
		func(r *Release) { r.UnpackedSizeBytes = 0 },
		func(r *Release) { r.Version = "v1.19.30" },
		func(r *Release) { r.Platform = "linux-386" },
	}
	for i, mutate := range bad {
		r := good
		mutate(&r)
		raw, sig := s.sign(t, index(1, r))
		if _, err := ParseIndex(raw, sig, s.keys); err == nil {
			t.Errorf("案例 %d 应被拒", i)
		}
	}
	// 防回退。
	if err := checkRevision(v, 2, ""); !errors.Is(err, ErrManifestStale) {
		t.Fatal("revision 回退应拒")
	}
	if err := checkRevision(v, 1, "other"); !errors.Is(err, ErrManifestStale) {
		t.Fatal("同 revision 换内容应拒")
	}
	adv := v.Index.selectRelease(HostPlatform(), "")
	if adv.Latest == nil || adv.Latest.Version != "1.19.30" {
		t.Fatalf("应可安装: %+v", adv)
	}
	if adv := v.Index.selectRelease(HostPlatform(), "1.19.30"); adv.Latest != nil {
		t.Fatal("已装同版本不应再建议")
	}
}

// ---- 订阅 ----

func TestParseSubscription(t *testing.T) {
	p, err := ParseSubscription([]byte(subscriptionFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Nodes) != 4 || p.Dropped != 2 || p.DroppedTypes["warp"] != 1 || p.DroppedTypes["ss"] != 1 {
		t.Fatalf("解析结果不对: nodes=%d dropped=%d types=%v", len(p.Nodes), p.Dropped, p.DroppedTypes)
	}
	if p.Nodes[0].Name != "香港 01" || p.Nodes[1].Name != "香港 01 #2" {
		t.Fatalf("重名应编号: %q %q", p.Nodes[0].Name, p.Nodes[1].Name)
	}
	for _, n := range p.Nodes {
		if _, ok := n.Raw["interface-name"]; ok {
			t.Error("interface-name 应被剔除")
		}
		if _, ok := n.Raw["routing-mark"]; ok {
			t.Error("routing-mark 应被剔除")
		}
	}
	if _, err := ParseSubscription([]byte("dmxlc3M6Ly9hYmM=")); !errors.Is(err, ErrNotClashConfig) {
		t.Fatalf("非 Clash 正文应报 ErrNotClashConfig: %v", err)
	}
	if _, err := ParseSubscription([]byte("proxies: []\n")); !errors.Is(err, ErrNotClashConfig) {
		t.Fatal("空 proxies 应报 ErrNotClashConfig")
	}
	if _, err := ParseSubscription([]byte("proxies:\n  - {name: x, type: warp, server: a, port: 1}\n")); err == nil || errors.Is(err, ErrNotClashConfig) {
		t.Fatalf("全被剔除应报没有可用节点: %v", err)
	}
	for _, u := range []string{"http://mysub.example/clash", "https://user:pw@mysub.example/x", "ftp://x", "", "https://"} {
		if _, err := ValidateSubscriptionURL(u); err == nil {
			t.Errorf("%q 应被拒", u)
		}
	}
	if u, err := ValidateSubscriptionURL(" https://mysub.example/subscribe/1/abc/clash/?flag=1#frag "); err != nil || u != "https://mysub.example/subscribe/1/abc/clash/?flag=1" {
		t.Fatalf("规范化不对: %q %v", u, err)
	}
	ui := parseUserInfo("upload=1; download=2000; total=3000; expire=1700000000")
	if ui == nil || ui.Download != 2000 || ui.Expire != 1700000000 {
		t.Fatalf("userinfo 解析不对: %+v", ui)
	}
}

// ---- 配置 ----

func TestBuildConfig(t *testing.T) {
	p, _ := ParseSubscription([]byte(subscriptionFixture))
	raw, err := BuildConfig(ConfigOptions{Port: 7891, Nodes: p.Nodes, Selected: "日本 AnyTLS"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["allow-lan"] != false || doc["bind-address"] != "127.0.0.1" || doc["socks-port"] != 7891 || doc["mode"] != "rule" {
		t.Fatalf("入站约束不对: %v", doc)
	}
	for _, k := range []string{"port", "mixed-port", "redir-port", "tproxy-port", "tun", "dns", "external-controller", "external-ui", "hosts", "script", "geodata-mode"} {
		if _, ok := doc[k]; ok {
			t.Errorf("受限配置不该有 %s", k)
		}
	}
	rules, _ := doc["rules"].([]any)
	if len(rules) != 1 || rules[0] != "MATCH,"+GroupSelect {
		t.Fatalf("规则应只有 MATCH: %v", rules)
	}
	groups, _ := doc["proxy-groups"].([]any)
	sel, _ := groups[0].(map[string]any)
	list, _ := sel["proxies"].([]any)
	if sel["name"] != GroupSelect || list[0] != "日本 AnyTLS" || list[1] != GroupAuto {
		t.Fatalf("选定节点应排首位: %v", list)
	}
	// 与策略组同名的节点被改名，且不再出现在配置里原名。
	found := false
	for _, x := range list {
		if x == "LLM Gate ·" {
			found = true
		}
		if x == GroupSelect {
			t.Fatal("节点不该与策略组重名")
		}
	}
	if !found {
		t.Fatalf("重名节点应被改名: %v", list)
	}
	raw2, _ := BuildConfig(ConfigOptions{Port: 7891, Nodes: p.Nodes})
	yaml.Unmarshal(raw2, &doc)
	groups, _ = doc["proxy-groups"].([]any)
	sel, _ = groups[0].(map[string]any)
	list, _ = sel["proxies"].([]any)
	if list[0] != GroupAuto {
		t.Fatalf("未选定时首位应是自动选择: %v", list)
	}
	if _, err := BuildConfig(ConfigOptions{Port: 7891}); err == nil {
		t.Fatal("无节点应报错")
	}
}

// ---- 管理器 ----

func TestManagerFlow(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("1.19.30")
	gz := gzipBytes(t, elf)
	raw, sig := s.sign(t, index(1, release("1.19.30", gz, elf)))
	st := newFakeSettings()
	eng := &fakeEngine{}
	eg := &fakeEgress{core: true}
	sub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "clash") {
			http.Error(w, "need clash ua", http.StatusBadRequest)
			return
		}
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=3; expire=4")
		io.WriteString(w, subscriptionFixture)
	}))
	defer sub.Close()
	m := NewManager(Options{
		DataDir: t.TempDir(), Settings: st, Engine: eng, Egress: eg, Keys: s.keys,
		Website: &fakeWebsite{index: raw, sig: sig, artifact: gz},
		Fetch:   sub.Client(),
	})
	ctx := context.Background()

	// 清单：官网检查 → 建议安装。
	adv, err := m.Check(ctx)
	if err != nil || adv.Latest == nil {
		t.Fatalf("Check: %v %+v", err, adv)
	}
	status := m.Status(ctx)
	if status.Component.State != "not_installed" || status.Enabled {
		t.Fatalf("初始状态: %+v", status.Component)
	}
	// 上传 gzip 制品 → 解压校验 → staged。
	if _, err := m.Upload(bytes.NewReader([]byte("garbage"))); !errors.Is(err, ErrUploadNotListed) {
		t.Fatalf("不在清单的上传应拒: %v", err)
	}
	staged, err := m.Upload(bytes.NewReader(gz))
	if err != nil || staged.Version != "1.19.30" || staged.SHA256 != sum(elf) || staged.SizeBytes != int64(len(elf)) {
		t.Fatalf("Upload: %v %+v", err, staged)
	}
	if m.Status(ctx).Component.State != "staged" {
		t.Fatal("应为 staged")
	}
	// 启用前置：未装、未订阅、未确认许可证。
	if err := m.Enable(ctx, true); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("未安装应拒: %v", err)
	}
	if _, err := m.Install(ctx); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if st := m.Status(ctx); st.Component.State != "installed" || st.Component.Version != "1.19.30" {
		t.Fatalf("安装后状态: %+v", st.Component)
	}
	if err := m.Enable(ctx, true); !errors.Is(err, ErrNoNodes) {
		t.Fatalf("无节点应拒: %v", err)
	}
	// 订阅：拉取、解析、落盘（不含订阅地址）。
	if _, err := m.SetSubscription(ctx, "http://insecure.example/x"); err == nil {
		t.Fatal("http 订阅应拒")
	}
	parsed, err := m.SetSubscription(ctx, sub.URL+"/subscribe/1/token-secret/clash/")
	if err != nil || len(parsed.Nodes) != 4 {
		t.Fatalf("SetSubscription: %v", err)
	}
	if st.sealed[settingSubscriptionURL] == "" || strings.Contains(st.plain[settingFetchedAt], "token-secret") {
		t.Fatal("订阅地址应密封")
	}
	status = m.Status(ctx)
	if !status.Subscription.Set || status.Subscription.NodeCount != 4 || status.Subscription.Dropped != 2 || status.Subscription.UserInfo == nil || status.Subscription.UserInfo.Download != 2 {
		t.Fatalf("订阅读数: %+v", status.Subscription)
	}
	if strings.Contains(status.Subscription.Host, "token-secret") || len(status.Nodes) != 4 {
		t.Fatalf("读数不应含令牌: %+v", status.Subscription)
	}
	// 许可证门 → 启用 → 内核配置只绑回环、egress 地址写入。
	if err := m.Enable(ctx, false); !errors.Is(err, ErrLicenseConsent) {
		t.Fatalf("未确认许可证应拒: %v", err)
	}
	eg.core = false
	if err := m.Enable(ctx, true); !errors.Is(err, ErrProviderNotMihomo) {
		t.Fatalf("方式不是内核应拒: %v", err)
	}
	eg.core = true
	if err := m.Enable(ctx, true); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if eg.addr != "127.0.0.1:7891" || eng.starts != 1 || !strings.Contains(eng.config, "bind-address: 127.0.0.1") || strings.Contains(eng.config, "allow-lan: true") {
		t.Fatalf("启用后: addr=%q starts=%d", eg.addr, eng.starts)
	}
	if !strings.Contains(eng.config, "fixture-pw-1") {
		t.Fatal("内核配置应含节点凭据（只进引擎）")
	}
	status = m.Status(ctx)
	if !status.Enabled || status.Core.State != "running" || !status.LicenseAccepted {
		t.Fatalf("启用读数: %+v", status.Core)
	}
	// 选节点：重新生成并重启；未知节点拒。
	if err := m.SelectNode(ctx, "不存在"); !errors.Is(err, ErrNodeUnknown) {
		t.Fatal("未知节点应拒")
	}
	if err := m.SelectNode(ctx, "日本 AnyTLS"); err != nil || eng.starts != 2 {
		t.Fatalf("选节点: %v starts=%d", err, eng.starts)
	}
	if first := firstSelectEntry(t, eng.config); first != "日本 AnyTLS" {
		t.Fatalf("选定节点应排首位，得到 %q", first)
	}
	// 同内容刷新不重启；启用中不能清订阅。
	if _, err := m.Refresh(ctx); err != nil || eng.starts != 2 {
		t.Fatalf("同内容刷新不应重启: %v %d", err, eng.starts)
	}
	if err := m.ClearSubscription(ctx); !errors.Is(err, ErrSubscriptionInUse) {
		t.Fatal("启用中清订阅应拒")
	}
	// 开机恢复：同配置在跑 → 不重启。
	m.Resume(ctx)
	if eng.starts != 2 {
		t.Fatalf("Resume 不应重启: %d", eng.starts)
	}
	// 停用：egress 地址清空（强制直连）、内核停止。
	if err := m.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if eg.addr != "" || !eg.forced || eng.stops != 1 || eng.running {
		t.Fatalf("停用后: addr=%q forced=%v stops=%d", eg.addr, eg.forced, eng.stops)
	}
	if err := m.ClearSubscription(ctx); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(ctx); st.Subscription.Set || len(st.Nodes) != 0 {
		t.Fatalf("清除后: %+v", st.Subscription)
	}
	// 日志与设置里搜不到节点凭据。
	for k, v := range st.plain {
		if strings.Contains(v, "fixture-pw") || strings.Contains(v, "token-secret") {
			t.Fatalf("明文设置 %s 泄露凭据", k)
		}
	}
	_ = fmt.Sprint(status)
}

func TestTestLatency(t *testing.T) {
	// 可达节点：本机监听器；不可达节点：先占一个端口再关掉（连接被拒，快速失败）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	m := NewManager(Options{DataDir: t.TempDir(), Settings: newFakeSettings(), Engine: &fakeEngine{}})
	ctx := context.Background()
	if _, err := m.TestLatency(ctx); !errors.Is(err, ErrNoNodes) {
		t.Fatalf("无节点应拒: %v", err)
	}
	nodes := []Node{
		{Name: "本机", Type: "socks5", Raw: map[string]any{"name": "本机", "type": "socks5", "server": "127.0.0.1", "port": ln.Addr().(*net.TCPAddr).Port}},
		{Name: "关着", Type: "ss", Raw: map[string]any{"name": "关着", "type": "ss", "server": "127.0.0.1", "port": closedPort, "cipher": "aes-128-gcm", "password": "x"}},
	}
	encoded, err := marshalNodes(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.nodesPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := m.TestLatency(ctx)
	if err != nil || sum.Total != 2 || sum.Reachable != 1 {
		t.Fatalf("TestLatency: %v %+v", err, sum)
	}
	status := m.Status(ctx)
	if status.LatencyTestedAt == "" {
		t.Fatal("读数应有测试时刻")
	}
	byName := map[string]NodeInfo{}
	for _, n := range status.Nodes {
		byName[n.Name] = n
	}
	if n := byName["本机"]; n.LatencyMS < 1 || n.LatencyFailed {
		t.Fatalf("可达节点读数: %+v", n)
	}
	if n := byName["关着"]; n.LatencyMS != 0 || !n.LatencyFailed {
		t.Fatalf("不可达节点读数: %+v", n)
	}
	// 撞上进行中的测试即答忙。
	m.latencyMu.Lock()
	if _, err := m.TestLatency(ctx); !errors.Is(err, ErrLatencyBusy) {
		t.Fatalf("并发测试应答忙: %v", err)
	}
	m.latencyMu.Unlock()
	// 清除订阅后延迟读数一并清空。
	if err := m.ClearSubscription(ctx); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(ctx); st.LatencyTestedAt != "" || len(st.Nodes) != 0 {
		t.Fatalf("清除后仍有延迟读数: %+v", st)
	}
}

// firstSelectEntry 读内核配置里 select 组的第一个候选。
func firstSelectEntry(t *testing.T, cfg string) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatal(err)
	}
	groups, _ := doc["proxy-groups"].([]any)
	sel, _ := groups[0].(map[string]any)
	list, _ := sel["proxies"].([]any)
	first, _ := list[0].(string)
	return first
}

func TestManagerDownloadUnpackCap(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("1.19.30")
	gz := gzipBytes(t, elf)
	rel := release("1.19.30", gz, elf)
	rel.UnpackedSizeBytes = int64(len(elf)) - 1 // 清单少报一字节：解压超限必须拒
	raw, sig := s.sign(t, index(1, rel))
	st := newFakeSettings()
	m := NewManager(Options{DataDir: t.TempDir(), Settings: st, Engine: &fakeEngine{}, Keys: s.keys,
		Website: &fakeWebsite{index: raw, sig: sig, artifact: gz}})
	ctx := context.Background()
	if _, err := m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Download(ctx); err == nil || !strings.Contains(err.Error(), "解压后长度") {
		t.Fatalf("解压超限应拒: %v", err)
	}
	if _, ok := m.StagedManifest(); ok {
		t.Fatal("失败不应留下 staged")
	}
}

// TestOfficialURLAcceptsCompatibleAMD64Asset 钉住 linux-amd64 的两种官方 asset 都放行
// （compatible 是 v1 基线，云主机缺省发它），其余平台仍只认精确同名 asset。
func TestOfficialURLAcceptsCompatibleAMD64Asset(t *testing.T) {
	base := "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.30/"
	cases := []struct {
		platform, asset string
		ok              bool
	}{
		{"linux-amd64", "mihomo-linux-amd64-compatible-v1.19.30.gz", true},
		{"linux-amd64", "mihomo-linux-amd64-v1.19.30.gz", true},
		{"linux-amd64", "mihomo-linux-amd64-v3-v1.19.30.gz", false},
		{"linux-arm64", "mihomo-linux-arm64-v1.19.30.gz", true},
		{"linux-arm64", "mihomo-linux-arm64-compatible-v1.19.30.gz", false},
		{"linux-arm64", "mihomo-linux-amd64-v1.19.30.gz", false},
	}
	for _, c := range cases {
		err := checkOfficialURL(base+c.asset, "1.19.30", c.platform)
		if (err == nil) != c.ok {
			t.Errorf("%s / %s: err=%v want ok=%v", c.platform, c.asset, err, c.ok)
		}
	}
}
