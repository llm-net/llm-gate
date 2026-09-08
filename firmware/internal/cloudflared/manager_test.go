package cloudflared

// 管理器流程的离线验收：清单接受与防回退、官方直下/手动上传同一条校验、安装、
// 启用前置条件与两道确认、停用/删除的 token 语义、开机恢复、自检分层，以及
// token 不出现在任何日志。全部依赖（settings/引擎/官网/listener）都是假的。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// ---- 假依赖 ----

type fakeSettings struct {
	mu sync.Mutex
	kv map[string]string
}

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

// 密封用可辨识的前缀模拟；"corrupt" 模拟解不开。
func (f *fakeSettings) SetSealedSetting(ctx context.Context, key, value string) error {
	if value == "" {
		return f.SetSetting(ctx, key, "")
	}
	return f.SetSetting(ctx, key, "sealed:"+value)
}

func (f *fakeSettings) GetSealedSetting(ctx context.Context, key string) (string, error) {
	v, _ := f.GetSetting(ctx, key)
	if v == "" {
		return "", nil
	}
	if v == "corrupt" {
		return "", errors.New("解封失败")
	}
	return strings.TrimPrefix(v, "sealed:"), nil
}

type fakeEngine struct {
	mu          sync.Mutex
	unavailable bool
	slots       []updated.ComponentSlot // [0]=current
	unit        string
	token       string
	restarts    int
	installFail string
	calls       []string
}

func (f *fakeEngine) ComponentStatus(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return nil, updated.ErrUnavailable
	}
	st := &updated.ComponentStatus{Name: ComponentName}
	if len(f.slots) > 0 {
		st.Installed = true
		st.CurrentSlot = "a"
		cur := f.slots[0]
		st.Current = &cur
	}
	if len(f.slots) > 1 {
		prev := f.slots[1]
		st.PreviousSlot = "b"
		st.Previous = &prev
	}
	return st, nil
}

func (f *fakeEngine) ComponentInstall(_ context.Context, _ string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "install "+req.Version)
	if f.unavailable {
		f.mu.Unlock()
		return nil, updated.ErrUnavailable
	}
	if f.installFail != "" {
		f.mu.Unlock()
		return nil, &updated.EngineError{Status: 409, Message: f.installFail}
	}
	// 引擎自己会重算摘要：这里也核一遍，保证管理器传的是真文件。
	raw, err := os.ReadFile(req.Path)
	if err != nil {
		f.mu.Unlock()
		return nil, &updated.EngineError{Status: 400, Message: "读取失败"}
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != req.SHA256 {
		f.mu.Unlock()
		return nil, &updated.EngineError{Status: 400, Message: "摘要不符"}
	}
	f.slots = append([]updated.ComponentSlot{{Version: req.Version, SHA256: req.SHA256, SizeBytes: int64(len(raw))}}, f.slots...)
	if len(f.slots) > 2 {
		f.slots = f.slots[:2]
	}
	f.mu.Unlock()
	return f.ComponentStatus(context.Background(), ComponentName)
}

func (f *fakeEngine) ComponentRollback(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "rollback")
	if len(f.slots) < 2 {
		f.mu.Unlock()
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrNoPreviousSlot.Error()}
	}
	f.slots[0], f.slots[1] = f.slots[1], f.slots[0]
	f.mu.Unlock()
	return f.ComponentStatus(context.Background(), ComponentName)
}

func (f *fakeEngine) ComponentRemove(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "remove")
	f.slots = nil
	f.unit = "inactive"
	f.token = ""
	f.mu.Unlock()
	return f.ComponentStatus(context.Background(), ComponentName)
}

func (f *fakeEngine) TunnelStatus(context.Context) (*updated.TunnelStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return nil, updated.ErrUnavailable
	}
	state := f.unit
	if state == "" {
		state = "inactive"
	}
	return &updated.TunnelStatus{UnitState: state, Restarts: f.restarts, TokenPresent: f.token != ""}, nil
}

func (f *fakeEngine) TunnelStart(ctx context.Context, token string) (*updated.TunnelStatus, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "tunnel start")
	if f.unavailable {
		f.mu.Unlock()
		return nil, updated.ErrUnavailable
	}
	if len(f.slots) == 0 {
		f.mu.Unlock()
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrComponentMissing.Error()}
	}
	if f.unit == "active" && f.token != token {
		f.restarts++
	}
	f.token = token
	f.unit = "active"
	f.mu.Unlock()
	return f.TunnelStatus(ctx)
}

func (f *fakeEngine) TunnelStop(ctx context.Context) (*updated.TunnelStatus, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "tunnel stop")
	if f.unavailable {
		f.mu.Unlock()
		return nil, updated.ErrUnavailable
	}
	f.unit = "inactive"
	f.token = ""
	f.mu.Unlock()
	return f.TunnelStatus(ctx)
}

// fakeListener 真的在 socket 上监听一个最小 origin：Host 不对回 421，否则 200，
// 让 probeOrigin 的两次自连有东西可测。
type fakeListener struct {
	mu        sync.Mutex
	listening bool
	policy    tunnelctx.Policy
	ln        net.Listener
	srv       *http.Server
	fail      bool
	// probes 是登记过的探测 nonce → 有没有「经 socket 到达」（stubRT 模拟到达时标记）。
	probes map[string]bool
	seq    int
}

func (f *fakeListener) BeginTunnelProbe() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probes == nil {
		f.probes = map[string]bool{}
	}
	f.seq++
	nonce := fmt.Sprintf("probe-%d", f.seq)
	f.probes[nonce] = false
	return nonce
}

func (f *fakeListener) EndTunnelProbe(nonce string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen, ok := f.probes[nonce]
	delete(f.probes, nonce)
	return ok && seen
}

// markProbe 模拟闸门在 socket 上见到 nonce；没登记的不记。
func (f *fakeListener) markProbe(nonce string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.probes[nonce]; ok {
		f.probes[nonce] = true
	}
}

func (f *fakeListener) StartTunnel(socket, _ string, pol tunnelctx.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("socket 建不出来")
	}
	f.policy = pol
	if f.listening {
		return nil
	}
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	f.ln = ln
	f.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		host := f.policy.Hostname
		f.mu.Unlock()
		if r.Host != host || r.Header.Get("X-Forwarded-Proto") != "https" {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})}
	go f.srv.Serve(ln)
	f.listening = true
	return nil
}

func (f *fakeListener) StopTunnel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.listening {
		return nil
	}
	f.srv.Close()
	f.listening = false
	return nil
}

func (f *fakeListener) UpdateTunnelPolicy(pol tunnelctx.Policy) {
	f.mu.Lock()
	f.policy = pol
	f.mu.Unlock()
}

func (f *fakeListener) TunnelListening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listening
}

type fakeWebsite struct {
	index, sig []byte
	artifact   []byte
	indexErr   error
	fetches    int
}

func (f *fakeWebsite) FetchComponentIndex(context.Context, string) ([]byte, []byte, error) {
	if f.indexErr != nil {
		return nil, nil, f.indexErr
	}
	return f.index, f.sig, nil
}

func (f *fakeWebsite) FetchComponentArtifact(_ context.Context, _ string, pol officialsite.ArtifactPolicy, w io.Writer) (string, int64, error) {
	f.fetches++
	body := f.artifact
	if int64(len(body)) > pol.MaxBytes {
		return "", 0, fmt.Errorf("组件制品超过 %d 字节上限", pol.MaxBytes)
	}
	w.Write(body)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), int64(len(body)), nil
}

// ---- 装配 ----

type env struct {
	t        *testing.T
	m        *Manager
	set      *fakeSettings
	eng      *fakeEngine
	ln       *fakeListener
	web      *fakeWebsite
	s        *signer
	logs     *bytes.Buffer
	artifact []byte
	release  Release
	gate     bool
}

// minimalELF 造本机架构的最小 ELF（elfcheck 只解析头部）。
func minimalELF(tag string) []byte {
	machine, ok := elfcheck.HostMachine()
	if !ok {
		panic("unsupported arch")
	}
	b := make([]byte, 64)
	copy(b[:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4], b[5], b[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[58:], 64)
	return append(b, []byte(tag)...)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, set: &fakeSettings{kv: map[string]string{}}, eng: &fakeEngine{}, ln: &fakeListener{}, s: newSigner(t), logs: &bytes.Buffer{}, gate: true}
	e.artifact = minimalELF("cloudflared-2026.8.2")
	sum := sha256.Sum256(e.artifact)
	e.release = rel("2026.8.2", HostPlatform(), hex.EncodeToString(sum[:]), int64(len(e.artifact)))
	raw := sampleIndex(1, e.release)
	e.web = &fakeWebsite{index: raw, sig: e.s.sign(raw), artifact: e.artifact}
	e.m = NewManager(Options{
		DataDir: dir, Settings: e.set, Website: e.web, Engine: e.eng, Listener: e.ln,
		Logger: logging.New(e.logs, slog.LevelDebug), Socket: filepath.Join(dir, "origin.sock"),
		AdminGate: func(context.Context) bool { return e.gate }, Keys: e.s.keys,
		MetricsReadyURL: "http://127.0.0.1:1/ready", // 不可达：connector 恒 connecting/degraded
		Now:             func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) },
	})
	t.Cleanup(func() { e.ln.StopTunnel() })
	return e
}

func (e *env) installed() *env {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.m.Check(ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.m.Download(ctx); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.m.Install(ctx); err != nil {
		e.t.Fatal(err)
	}
	return e
}

func (e *env) configured() *env {
	e.t.Helper()
	host, tok := "box.example.com", sampleToken()
	if _, err := e.m.UpdateConfig(context.Background(), ConfigPatch{Hostname: &host, Token: &tok}); err != nil {
		e.t.Fatal(err)
	}
	return e
}

// ---- 用例 ----

func TestCheckDownloadInstallPipeline(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st := e.m.Status(ctx)
	if st.Component.State != "not_installed" || st.Component.Advisory != nil || st.Credential.State != "unset" || st.Connector.State != "stopped" || st.Origin.State != "not_listening" {
		t.Fatalf("初始状态不对: %+v", st)
	}
	if err := e.m.Download(ctx); !errors.Is(err, ErrNoAdvisory) {
		t.Fatalf("没有清单就下载应拒: %v", err)
	}
	adv, err := e.m.Check(ctx)
	if err != nil || adv.Latest == nil || adv.Latest.Version != "2026.8.2" || adv.Source != "website" {
		t.Fatalf("Check: %+v %v", adv, err)
	}
	// 落盘的清单能被新实例读回（重启后不必再联网）。
	m2 := NewManager(Options{DataDir: e.m.opt.DataDir, Settings: e.set, Engine: e.eng, Listener: e.ln, Keys: e.s.keys})
	if a := m2.Advisory(ctx); a == nil || a.Latest == nil || a.Source != "stored" {
		t.Fatalf("落盘清单没读回: %+v", a)
	}
	if err := e.m.Download(ctx); err != nil {
		t.Fatal(err)
	}
	staged, ok := e.m.StagedManifest()
	if !ok || staged.Version != "2026.8.2" || staged.Source != "website" {
		t.Fatalf("staged 不对: %+v", staged)
	}
	if info, _ := os.Stat(e.m.stagedBinPath()); info.Mode().Perm()&0o111 != 0 {
		t.Fatal("staging 文件不该有执行权限")
	}
	if st := e.m.Status(ctx); st.Component.State != "staged" {
		t.Fatalf("有 staged 时状态应为 staged: %+v", st.Component)
	}
	comp, err := e.m.Install(ctx)
	if err != nil || !comp.Installed || comp.Current.Version != "2026.8.2" {
		t.Fatalf("Install: %+v %v", comp, err)
	}
	if _, ok := e.m.StagedManifest(); ok {
		t.Fatal("安装成功后 staging 应被丢弃")
	}
	st = e.m.Status(ctx)
	if st.Component.State != "installed" || st.Component.Version != "2026.8.2" || st.Component.Advisory.Latest != nil {
		t.Fatalf("安装后状态不对: %+v", st.Component)
	}
	if _, err := e.m.Install(ctx); !errors.Is(err, ErrNoStaged) {
		t.Fatalf("没有 staged 再装应拒: %v", err)
	}
}

func TestDownloadRejectsWrongBytes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	e.web.artifact = minimalELF("tampered")
	if err := e.m.Download(ctx); err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("字节不符应拒: %v", err)
	}
	if _, ok := e.m.StagedManifest(); ok {
		t.Fatal("被拒的下载不该留下 staged")
	}
	if _, downloadErr := e.m.Downloading(); downloadErr == "" {
		t.Fatal("下载失败原因应留在状态里")
	}
	// 非 ELF 但摘要/长度对得上——清单被人改了摘要也过不了形状闸。
	text := bytes.Repeat([]byte("x"), len(e.artifact))
	sum := sha256.Sum256(text)
	raw := sampleIndex(2, rel("2026.8.2", HostPlatform(), hex.EncodeToString(sum[:]), int64(len(text))))
	e.web.index, e.web.sig, e.web.artifact = raw, e.s.sign(raw), text
	if _, err := e.m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Download(ctx); !errors.Is(err, elfcheck.ErrNotExecutable) {
		t.Fatalf("非 ELF 应被形状闸拒绝: %v", err)
	}
}

func TestManifestAntiRollbackAndImport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	stale := sampleIndex(0)
	stale = sampleIndex(1, e.release, rel("2026.9.9", HostPlatform(), testSHA, 100))
	if _, err := e.m.ImportManifest(ctx, stale, e.s.sign(stale)); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("同 revision 换内容应拒: %v", err)
	}
	newer := sampleIndex(2, e.release, rel("2026.9.9", HostPlatform(), testSHA, 100))
	adv, err := e.m.ImportManifest(ctx, newer, e.s.sign(newer))
	if err != nil || adv.Latest == nil || adv.Latest.Version != "2026.9.9" || adv.Source != "upload" {
		t.Fatalf("导入更新的清单: %+v %v", adv, err)
	}
	older := sampleIndex(1, e.release)
	e.web.index, e.web.sig = older, e.s.sign(older)
	if _, err := e.m.Check(ctx); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("官网回退到旧 revision 应拒: %v", err)
	}
	if adv := e.m.Advisory(ctx); adv.Revision != 2 {
		t.Fatalf("被拒的清单不该覆盖已接受的: %+v", adv)
	}
	if _, err := e.m.ImportManifest(ctx, newer, []byte("garbage")); err == nil {
		t.Fatal("坏签名应拒")
	}
}

func TestUploadMustMatchManifest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.Upload(bytes.NewReader(e.artifact)); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("没有清单时上传应拒: %v", err)
	}
	if _, err := e.m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Upload(bytes.NewReader(minimalELF("other"))); !errors.Is(err, ErrUploadNotListed) {
		t.Fatalf("不在清单里的文件应拒: %v", err)
	}
	st, err := e.m.Upload(bytes.NewReader(e.artifact))
	if err != nil || st.Version != "2026.8.2" || st.Source != "upload" {
		t.Fatalf("Upload: %+v %v", st, err)
	}
	if err := e.m.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.StagedManifest(); ok {
		t.Fatal("丢弃后不该有 staged")
	}
	if _, err := e.m.Upload(bytes.NewReader(nil)); err == nil {
		t.Fatal("空文件应拒")
	}
}

func TestEnablePreconditionsAndLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	all := EnableRequest{AcceptThirdParty: true, ConfirmAdminExposure: true}
	if err := e.m.Enable(ctx, all); !errors.Is(err, ErrHostnameRequired) {
		t.Fatalf("无 hostname: %v", err)
	}
	host := "box.example.com"
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{Hostname: &host}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Enable(ctx, all); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("无 token: %v", err)
	}
	tok := sampleToken()
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{Token: &tok}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Enable(ctx, EnableRequest{}); !errors.Is(err, ErrThirdPartyConsent) {
		t.Fatalf("未确认第三方: %v", err)
	}
	if err := e.m.Enable(ctx, all); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("组件未装: %v", err)
	}
	e.installed()
	// api_and_admin：缺省口令挡、二次确认挡。
	exp := string(tunnelctx.ProfileAPIAndAdmin)
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{Exposure: &exp}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true}); !errors.Is(err, ErrAdminConsent) {
		t.Fatalf("管理面暴露未二次确认: %v", err)
	}
	e.gate = false
	if err := e.m.Enable(ctx, all); !errors.Is(err, ErrAdminGate) {
		t.Fatalf("缺省口令应挡: %v", err)
	}
	e.gate = true
	if err := e.m.Enable(ctx, all); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !e.ln.TunnelListening() || e.ln.policy.Hostname != "box.example.com" || e.ln.policy.Profile != tunnelctx.ProfileAPIAndAdmin {
		t.Fatalf("listener 策略不对: %+v", e.ln.policy)
	}
	if e.eng.unit != "active" || e.eng.token != tok {
		t.Fatal("引擎应收到 token 并启动 connector")
	}
	st := e.m.Status(ctx)
	if !st.Enabled || st.ExternalURL != "https://box.example.com" || st.Origin.State != "listening" || st.Credential.State != "sealed" || st.Connector.State != "connecting" {
		t.Fatalf("启用后状态不对: %+v", st)
	}
	// 启用中：清 token 拒、换 hostname 即时更新策略、换 token 重启 connector。
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{ClearToken: true}); !errors.Is(err, ErrTokenInUse) {
		t.Fatalf("启用中清 token: %v", err)
	}
	host2 := "box2.example.com"
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{Hostname: &host2}); err != nil || e.ln.policy.Hostname != "box2.example.com" {
		t.Fatalf("改 hostname 应更新策略: %v %+v", err, e.ln.policy)
	}
	tok2 := sampleToken2()
	ch, err := e.m.UpdateConfig(ctx, ConfigPatch{Token: &tok2})
	if err != nil || !ch.TokenReplaced || !ch.Restarted || e.eng.token != tok2 || e.eng.restarts != 1 {
		t.Fatalf("换 token 应重启 connector: %+v %v restarts=%d", ch, err, e.eng.restarts)
	}
	if st := e.m.Status(ctx); st.Connector.State != "degraded" {
		t.Fatalf("有重启且未就绪应判 degraded: %+v", st.Connector)
	}
	// 自检（本地）：组件 ok、connector 未就绪、origin 自连 ok。
	rep := e.m.Test(ctx, false)
	want := map[string]bool{"component": true, "connector": false, "origin": true}
	for _, c := range rep.Checks {
		if ok, listed := want[c.Layer]; !listed || ok != c.OK {
			t.Errorf("自检 %s = %v（%s），期望 %v", c.Layer, c.OK, c.Detail, want[c.Layer])
		}
	}
	if rep.OK || len(rep.Checks) != 3 {
		t.Fatalf("自检整体应失败且三项: %+v", rep)
	}
	// 停用保留 token → 再启用不必重贴。
	if err := e.m.Disable(ctx, true); err != nil {
		t.Fatal(err)
	}
	st = e.m.Status(ctx)
	if st.Enabled || st.ExternalURL != "" || st.Credential.State != "sealed" || e.ln.TunnelListening() || e.eng.unit != "inactive" || e.eng.token != "" {
		t.Fatalf("停用后状态不对: %+v", st)
	}
	if err := e.m.Enable(ctx, all); err != nil {
		t.Fatalf("重新启用: %v", err)
	}
	// 停用并销毁 token。
	if err := e.m.Disable(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(ctx); st.Credential.State != "unset" {
		t.Fatalf("销毁 token 后应为 unset: %+v", st.Credential)
	}
	if err := e.m.Enable(ctx, all); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("销毁后再启用应要求 token: %v", err)
	}
	// 删除本机配置连组件一起卸。
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{Token: &tok}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Delete(ctx, true); err != nil {
		t.Fatal(err)
	}
	st = e.m.Status(ctx)
	if st.Config.Hostname != "" || st.Credential.State != "unset" || st.Component.Installed {
		t.Fatalf("删除后状态不对: %+v", st)
	}
	// §15.1：整个流程的日志里搜不到 token。
	for _, secret := range []string{tok, tok2, "secret-bytes", "rotated-secret"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatal("日志里出现了 token")
		}
	}
}

func TestEnableRollsBackListenerWhenEngineFails(t *testing.T) {
	e := newEnv(t).installed().configured()
	ctx := context.Background()
	e.eng.unavailable = true
	err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true})
	if !errors.Is(err, updated.ErrUnavailable) {
		t.Fatalf("引擎不可达应报 ErrUnavailable: %v", err)
	}
	if e.ln.TunnelListening() {
		t.Fatal("引擎失败后 listener 应被关闭，不留半开状态")
	}
	if en, _ := e.m.Enabled(ctx); en {
		t.Fatal("失败的启用不该落库")
	}
	e.eng.unavailable = false
	e.ln.fail = true
	if err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true}); err == nil || e.eng.unit == "active" {
		t.Fatalf("socket 建不出来时不该启动 connector: %v", err)
	}
}

func TestResumeOnBoot(t *testing.T) {
	e := newEnv(t).installed().configured()
	ctx := context.Background()
	if err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true}); err != nil {
		t.Fatal(err)
	}
	// 模拟重启：新的 Manager，listener 已停，引擎里 unit 仍在跑（systemd 管的）。
	e.ln.StopTunnel()
	m2 := NewManager(Options{DataDir: e.m.opt.DataDir, Settings: e.set, Engine: e.eng, Listener: e.ln, Keys: e.s.keys, Socket: e.m.opt.Socket})
	m2.Resume(ctx)
	if !e.ln.TunnelListening() {
		t.Fatal("Resume 应重建 origin socket")
	}
	if e.eng.restarts != 0 {
		t.Fatal("同 token 恢复不该重启 connector")
	}
	if st := m2.Status(ctx); st.LastError != "" || !st.Enabled {
		t.Fatalf("恢复后状态不对: %+v", st)
	}
	// token 解不开：不恢复公网入口，状态标出。
	e.ln.StopTunnel()
	e.set.kv[settingTokenSealed] = "corrupt"
	m3 := NewManager(Options{DataDir: e.m.opt.DataDir, Settings: e.set, Engine: e.eng, Listener: e.ln, Keys: e.s.keys, Socket: e.m.opt.Socket})
	m3.Resume(ctx)
	if e.ln.TunnelListening() {
		t.Fatal("token 不可用时不该起 listener")
	}
	if st := m3.Status(ctx); st.LastError != "credential_unreadable" || st.Credential.State != "unreadable" {
		t.Fatalf("解封失败应标出: %+v", st)
	}
	// 未启用：什么都不做。
	e.set.kv[settingEnabled] = "0"
	m4 := NewManager(Options{DataDir: e.m.opt.DataDir, Settings: e.set, Engine: e.eng, Listener: e.ln, Keys: e.s.keys, Socket: e.m.opt.Socket})
	m4.Resume(ctx)
	if e.ln.TunnelListening() {
		t.Fatal("未启用不该起 listener")
	}
}

func TestAutoUpdateOnlyWhenEnabledAndOptedIn(t *testing.T) {
	e := newEnv(t).installed().configured()
	ctx := context.Background()
	newer := minimalELF("cloudflared-2026.9.1")
	sum := sha256.Sum256(newer)
	raw := sampleIndex(2, e.release, rel("2026.9.1", HostPlatform(), hex.EncodeToString(sum[:]), int64(len(newer))))
	e.web.index, e.web.sig, e.web.artifact = raw, e.s.sign(raw), newer

	e.m.AutoUpdate(ctx) // 未启用：不动
	if e.web.fetches != 1 {
		t.Fatalf("未启用不该下载: fetches=%d", e.web.fetches)
	}
	if err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true}); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{AutoUpdate: &off}); err != nil {
		t.Fatal(err)
	}
	e.m.AutoUpdate(ctx) // 关了自动更新：不动
	if e.web.fetches != 1 {
		t.Fatal("关闭自动更新不该下载")
	}
	on := true
	if _, err := e.m.UpdateConfig(ctx, ConfigPatch{AutoUpdate: &on}); err != nil {
		t.Fatal(err)
	}
	e.m.AutoUpdate(ctx)
	if e.web.fetches != 2 {
		t.Fatalf("应下载一次新版本: fetches=%d", e.web.fetches)
	}
	if st := e.m.Status(ctx); st.Component.Version != "2026.9.1" || st.Component.PreviousVersion != "2026.8.2" {
		t.Fatalf("自动更新后版本不对: %+v", st.Component)
	}
	// 安装失败（引擎切回旧版）：保留 staged 之外不改任何状态，日志有翻转记录。
	blocked := minimalELF("cloudflared-2026.10.1")
	bsum := sha256.Sum256(blocked)
	raw = sampleIndex(3, e.release, rel("2026.10.1", HostPlatform(), hex.EncodeToString(bsum[:]), int64(len(blocked))))
	e.web.index, e.web.sig, e.web.artifact = raw, e.s.sign(raw), blocked
	e.eng.installFail = "新版本 2026.10.1 未能在健康窗口内就绪，已回到旧版本继续运行"
	e.m.AutoUpdate(ctx)
	if st := e.m.Status(ctx); st.Component.Version != "2026.9.1" {
		t.Fatalf("安装失败不该改版本: %+v", st.Component)
	}
	if !strings.Contains(e.logs.String(), "自动更新未完成") {
		t.Fatal("自动更新失败应记一条翻转日志")
	}
}

func TestPublicProbeCategories(t *testing.T) {
	e := newEnv(t).installed().configured()
	ctx := context.Background()
	if err := e.m.Enable(ctx, EnableRequest{AcceptThirdParty: true}); err != nil {
		t.Fatal(err)
	}
	// 用可控的 RoundTripper 替代真公网：缺省模拟「Cloudflare 回源到 unix socket」，
	// 即请求带的探测 nonce 会被闸门见到。
	rt := &stubRT{ln: e.ln}
	e.m.opt.PublicClient = &http.Client{Transport: rt}
	cases := []struct {
		status int
		body   string
		want   bool
		detail string
	}{
		{200, `{"status":"ok"}`, true, "经 origin socket 到达"},
		{530, "error 1033", false, "HTTP 530"},
		{200, "<html>access</html>", false, "不是设备的 /healthz"},
		{403, "blocked", false, "HTTP 403"},
	}
	for _, c := range cases {
		rt.status, rt.body = c.status, c.body
		rep := e.m.Test(ctx, true)
		var pub *TestCheck
		for i := range rep.Checks {
			if rep.Checks[i].Layer == "public" {
				pub = &rep.Checks[i]
			}
		}
		if pub == nil || pub.OK != c.want || !strings.Contains(pub.Detail, c.detail) {
			t.Errorf("status %d: %+v", c.status, pub)
		}
		if rt.lastHost != "box.example.com" || rt.lastPath != "/healthz" {
			t.Errorf("公网探测应打 https://box.example.com/healthz，得到 %s%s", rt.lastHost, rt.lastPath)
		}
	}
	// 回源填成 http://localhost:80：/healthz 照样 200，但 nonce 没经过 socket——必须报未通过，
	// 并把原因（Service URL 不是 unix socket、全部路由暴露）说清楚。
	rt.status, rt.body, rt.bypass = 200, `{"status":"ok"}`, true
	rep := e.m.Test(ctx, true)
	pub := rep.Checks[len(rep.Checks)-1]
	if pub.Layer != "public" || pub.OK || !strings.Contains(pub.Detail, "没有经过 origin socket") || !strings.Contains(pub.Detail, "unix:"+e.m.opt.Socket) {
		t.Fatalf("绕过闸门的 200 应报未通过并点名 Service URL: %+v", pub)
	}
	if rep.OK {
		t.Fatal("绕过闸门时整份自检不该通过")
	}
	if !strings.Contains(e.logs.String(), "绕过了闸门") {
		t.Fatal("绕过应记一条告警")
	}
	if len(e.ln.probes) != 0 {
		t.Fatalf("探测登记应被取走: %v", e.ln.probes)
	}
	rt.bypass = false
	rt.err = &net.DNSError{Err: "no such host", Name: "box.example.com"}
	rep = e.m.Test(ctx, true)
	if last := rep.Checks[len(rep.Checks)-1]; last.OK || !strings.Contains(last.Detail, "dns_failed") {
		t.Errorf("DNS 失败应归类 dns_failed: %+v", last)
	}
	if len(e.ln.probes) != 0 {
		t.Fatalf("探测失败也要取走登记: %v", e.ln.probes)
	}
	if st := e.m.Status(ctx); st.LastTest == nil || !st.LastTest.Public {
		t.Fatal("最近一次自检应留在状态里")
	}
}

type stubRT struct {
	status   int
	body     string
	err      error
	lastHost string
	lastPath string
	// ln 非空且 bypass 为假时，模拟请求经 origin socket 回到本机（闸门标记探测 nonce）；
	// bypass 为真模拟 Cloudflare 回源填成了 http://localhost:80——请求到了 LAN listener，
	// nonce 无人标记。
	ln     *fakeListener
	bypass bool
}

func (s *stubRT) RoundTrip(r *http.Request) (*http.Response, error) {
	s.lastHost, s.lastPath = r.URL.Host, r.URL.Path
	if s.err != nil {
		return nil, s.err
	}
	if s.ln != nil && !s.bypass {
		s.ln.markProbe(r.Header.Get(tunnelctx.ProbeHeader))
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader(s.body)), Header: http.Header{}, Request: r}, nil
}
