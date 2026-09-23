package devhost_test

// 守护进程 devd流程验收：经已纳管主机的免密 SSH 安装（root、免密 sudo、sudo 口令三条路）、
// 检查、devd 透传与终端帧流（都经 SSH 转发通道到守护进程 socket）、掉线与卸载。
// 假主机在进程内跑真 SSH 与真守护进程，不出网。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/devd/wire"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const password = "n0t-a-real-password"

type fixture struct {
	dev   *devhost.Manager
	hosts *agenthost.Manager
	host  *devhosttest.Host
	st    *store.Store
}

func newFixture(t *testing.T, opt devhosttest.Options) *fixture {
	t.Helper()
	if opt.Password == "" {
		opt.Password = password
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	host := devhosttest.New(t, opt)
	hosts := agenthost.New(agenthost.Options{Store: st, Dial: host.SSH.Dial})
	dev := devhost.New(devhost.Options{Store: st, Hosts: hosts, Binaries: devhosttest.FakeBinaries{}})
	return &fixture{dev: dev, hosts: hosts, host: host, st: st}
}

// enroll 先把假主机按 kind 纳管成「免密可达」（可选顺手配免密 sudo），回主机行。
func (f *fixture) enrollKind(t *testing.T, kind, username string, sudo bool) *store.AgentHost {
	t.Helper()
	ctx := context.Background()
	if _, err := f.hosts.GenerateCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	addr, port := f.host.Addr()
	h, err := f.hosts.Enroll(ctx, agenthost.EnrollRequest{Name: "板子", Kind: kind, Address: addr, Port: port, Username: username, Password: password, ConfigureSudo: sudo})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if h.Status != store.AgentHostStatusReady {
		t.Fatalf("纳管后 = %+v", h)
	}
	return h
}

// enroll 纳管成工作节点：安装 / 透传用例都以它为前提。
func (f *fixture) enroll(t *testing.T, username string, sudo bool) *store.AgentHost {
	t.Helper()
	return f.enrollKind(t, store.AgentHostKindWorker, username, sudo)
}

func code(t *testing.T, err error) string {
	t.Helper()
	var de *devhost.Error
	if !errors.As(err, &de) {
		t.Fatalf("不是 devhost.Error: %v", err)
	}
	return de.Code
}

// 受控纳管的主机不能装守护进程：不连主机、不动行；其余守护进程动作照常答「没装」。
func TestInstallRefusedForManagedHost(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "root", Arch: "x86_64"})
	host := f.enrollKind(t, store.AgentHostKindManaged, "root", false)
	ctx := context.Background()
	if _, err := f.dev.Install(ctx, devhost.InstallRequest{ID: host.ID}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("受控纳管安装应被拒：%v", err)
	}
	if f.host.Installed() {
		t.Fatal("主机上不该装上守护进程")
	}
	row, err := f.st.GetAgentHost(ctx, host.ID)
	if err != nil || row.Devd.Installed() || row.Devd.LastError != "" {
		t.Fatalf("行不该被动过 = %+v（err=%v）", row, err)
	}
	if _, err := f.dev.Check(ctx, host.ID); err == nil || code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("检查应答没装：%v", err)
	}
	if _, st, ok := devhost.StatusFromError(&devhost.Error{Code: devhost.CodeKindNotAllowed}); !ok || st != 409 {
		t.Fatalf("host_kind_not_allowed 应映射 409，得到 %d", st)
	}
}

func TestInstallOverEnrolledHost(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", Arch: "aarch64"})
	m, host := f.dev, f.host
	ctx := context.Background()
	row := f.enroll(t, "dev", true)
	if !row.SudoNoPasswd || row.Devd.Installed() {
		t.Fatalf("纳管后行 = %+v", row)
	}
	// 没装就检查 / 透传 / 卸载：一律 devd_not_installed。
	if _, err := m.Check(ctx, row.ID); code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("未装检查 = %v", err)
	}
	if _, err := m.Proxy(ctx, row.ID, http.MethodGet, "info", nil, nil, ""); code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("未装透传 = %v", err)
	}
	if _, err := m.Uninstall(ctx, devhost.UninstallRequest{ID: row.ID}); code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("未装卸载 = %v", err)
	}
	// 安装：不要口令（免密 sudo）。
	h, err := m.Install(ctx, devhost.InstallRequest{ID: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	d := h.Devd
	if !d.Installed() || d.Status != store.DevdStatusReady || d.Home == "" || d.Version != "fake" || d.CheckedAt.IsZero() {
		t.Fatalf("守护进程 = %+v", d)
	}
	if h.Status != store.AgentHostStatusReady || h.KeyFingerprint == "" {
		t.Fatalf("主机自身的列不该被动过 = %+v", h)
	}
	if user := host.InstalledAs(); user != "dev" || !strings.Contains(string(host.InstalledBinary()), "arm64") {
		t.Fatalf("安装参数 = %s %q", user, host.InstalledBinary())
	}
	// 安装那几条命令全经证书登录、全经 sudo -n；口令不在任何 argv 或 stdin 里；
	// 验收连的是守护进程的 socket。
	sawInstall := false
	for _, ex := range host.Execs() {
		if strings.Contains(ex.Command, password) {
			t.Fatalf("口令进了命令行：%q", ex.Command)
		}
		if strings.Contains(ex.Command, devd.DefaultBinPath+" install") {
			sawInstall = true
			if !ex.PublicKeyAuth || !strings.HasPrefix(ex.Command, "sudo -n ") || ex.Stdin != "" || strings.Contains(ex.Command, "--cert") {
				t.Fatalf("安装命令 = %+v", ex)
			}
		}
	}
	if !sawInstall {
		t.Fatal("没看到安装命令")
	}
	if socks := host.Sockets(); len(socks) == 0 || socks[0] != devd.DefaultSocket {
		t.Fatalf("转发目标 = %v", socks)
	}
	// 重装幂等。
	if h, err = m.Install(ctx, devhost.InstallRequest{ID: row.ID}); err != nil || h.Devd.Status != store.DevdStatusReady {
		t.Fatalf("重装 = %+v %v", h, err)
	}
	// 检查、透传、终端。
	if h, err = m.Check(ctx, row.ID); err != nil || h.Devd.Status != store.DevdStatusReady {
		t.Fatalf("check = %+v %v", h, err)
	}
	resp, err := m.Proxy(ctx, row.ID, http.MethodGet, "fs/list", nil, nil, "")
	if err != nil || resp.Status != 200 {
		t.Fatalf("proxy = %+v %v", resp, err)
	}
	var listing struct {
		Path string `json:"path"`
	}
	json.Unmarshal(resp.Body, &listing)
	if listing.Path != h.Devd.Home {
		t.Fatalf("listing = %+v", listing)
	}
	resp, err = m.Proxy(ctx, row.ID, http.MethodGet, "fs/read", map[string][]string{"path": {"nope"}}, nil, "")
	if err != nil || resp.Status != 404 || !strings.Contains(string(resp.Body), "not_found") {
		t.Fatalf("proxy 404 = %+v %v", resp, err)
	}
	// tmux 服务器隔离到临时目录：attach 会 source 守护进程内嵌的 tmux.conf。
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	if _, err := exec.LookPath("tmux"); err == nil {
		conn, err := m.Attach(ctx, row.ID, "llmgate-devhost-test", "", 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "=llmgate-devhost-test").Run() })
		wire.Write(conn, wire.Data, []byte("exit\n"))
		fr := wire.NewReader(conn)
		if f, err := fr.Next(); err != nil || f.Type != wire.Data {
			t.Fatalf("终端首帧 = %+v %v", f, err)
		}
		conn.Close()
	}
	// 卸载：主机上收走、行里归零、主机自身仍是免密可达。
	h, err = m.Uninstall(ctx, devhost.UninstallRequest{ID: row.ID})
	if err != nil || h.Devd.Installed() || host.Installed() || h.Status != store.AgentHostStatusReady {
		t.Fatalf("uninstall = %+v %v installed=%v", h, err, host.Installed())
	}
	if _, err := m.Proxy(ctx, row.ID, http.MethodGet, "info", nil, nil, ""); code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("卸载后透传 = %v", err)
	}
	if _, err := m.Install(ctx, devhost.InstallRequest{ID: 999}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的主机 = %v", err)
	}
}

func TestInstallSudoPaths(t *testing.T) {
	ctx := context.Background()
	// root：直接跑，不经 sudo。
	f := newFixture(t, devhosttest.Options{Username: "root"})
	row := f.enroll(t, "root", false)
	h, err := f.dev.Install(ctx, devhost.InstallRequest{ID: row.ID})
	if err != nil || h.Devd.Status != store.DevdStatusReady {
		t.Fatalf("root 安装 = %+v %v", h, err)
	}
	for _, ex := range f.host.Execs() {
		if strings.HasPrefix(ex.Command, "sudo") {
			t.Fatalf("root 不该走 sudo：%q", ex.Command)
		}
	}

	// 没配免密 sudo：不给口令连都不连（行不动）；给口令走 sudo -S，口令只在 stdin 首行。
	f2 := newFixture(t, devhosttest.Options{Username: "dev"})
	row2 := f2.enroll(t, "dev", false)
	before := len(f2.host.Execs())
	if _, err := f2.dev.Install(ctx, devhost.InstallRequest{ID: row2.ID}); code(t, err) != devhost.CodeSudoFailed {
		t.Fatalf("没免密 sudo 且没口令 = %v", err)
	}
	if got, _ := f2.hosts.Get(ctx, row2.ID); got.Devd.Installed() || len(f2.host.Execs()) != before {
		t.Fatalf("预检失败不该碰主机或行：%+v", got.Devd)
	}
	if _, err := f2.dev.Install(ctx, devhost.InstallRequest{ID: row2.ID, Password: "wrong"}); code(t, err) != devhost.CodeSudoFailed {
		t.Fatalf("错口令 = %v", err)
	}
	if got, _ := f2.hosts.Get(ctx, row2.ID); got.Devd.Status != store.DevdStatusError || got.Devd.LastError == "" {
		t.Fatalf("错口令后行 = %+v", got.Devd)
	}
	h2, err := f2.dev.Install(ctx, devhost.InstallRequest{ID: row2.ID, Password: password})
	if err != nil || h2.Devd.Status != store.DevdStatusReady {
		t.Fatalf("带口令安装 = %+v %v", h2, err)
	}
	for _, ex := range f2.host.Execs() {
		if strings.Contains(ex.Command, password) {
			t.Fatalf("口令进了命令行：%q", ex.Command)
		}
		if strings.Contains(ex.Command, devd.DefaultBinPath+" install") && !strings.HasPrefix(ex.Command, "sudo -S -p '' ") {
			t.Fatalf("应经 sudo -S：%q", ex.Command)
		}
	}
	// 卸载同样要口令。
	if _, err := f2.dev.Uninstall(ctx, devhost.UninstallRequest{ID: row2.ID}); code(t, err) != devhost.CodeSudoFailed {
		t.Fatalf("没口令卸载 = %v", err)
	}
	if h2, err = f2.dev.Uninstall(ctx, devhost.UninstallRequest{ID: row2.ID, Password: password}); err != nil || h2.Devd.Installed() {
		t.Fatalf("带口令卸载 = %+v %v", h2, err)
	}

	// 主机上根本没有 sudo。
	f3 := newFixture(t, devhosttest.Options{Username: "dev", NoSudo: true})
	row3 := f3.enroll(t, "dev", false)
	if _, err := f3.dev.Install(ctx, devhost.InstallRequest{ID: row3.ID, Password: password}); code(t, err) != devhost.CodeSudoFailed {
		t.Fatalf("没 sudo = %v", err)
	}

	// 不支持的架构。
	f4 := newFixture(t, devhosttest.Options{Username: "dev", Arch: "mips"})
	row4 := f4.enroll(t, "dev", true)
	if _, err := f4.dev.Install(ctx, devhost.InstallRequest{ID: row4.ID}); code(t, err) != devhost.CodeUnsupportedArch {
		t.Fatalf("不支持的架构 = %v", err)
	}
	if got, _ := f4.hosts.Get(ctx, row4.ID); got.Devd.Status != store.DevdStatusError {
		t.Fatalf("失败行 = %+v", got.Devd)
	}
}

func TestDaemonDownAndForwardingDisabled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, devhosttest.Options{Username: "dev"})
	m, host := f.dev, f.host
	row := f.enroll(t, "dev", true)
	if _, err := m.Install(ctx, devhost.InstallRequest{ID: row.ID}); err != nil {
		t.Fatal(err)
	}
	// 守护进程掉线：检查失败、行记原因；重装（免密）后恢复。
	host.MarkUninstalled()
	if _, err := m.Check(ctx, row.ID); code(t, err) != devhost.CodeUnreachable {
		t.Fatalf("掉线检查 = %v", err)
	}
	if got, _ := f.hosts.Get(ctx, row.ID); got.Devd.Status != store.DevdStatusError || got.Devd.LastError == "" {
		t.Fatalf("掉线后行 = %+v", got.Devd)
	}
	if _, err := m.Proxy(ctx, row.ID, http.MethodGet, "info", nil, nil, ""); code(t, err) != devhost.CodeUnreachable {
		t.Fatalf("掉线透传 = %v", err)
	}
	if h, err := m.Install(ctx, devhost.InstallRequest{ID: row.ID}); err != nil || h.Devd.Status != store.DevdStatusReady {
		t.Fatalf("重装 = %+v %v", h, err)
	}

	// sshd 禁止转发，且像 OpenSSH 那样只回一句 open failed：客户端分不出是哪一种，
	// 错误里必须把可能的原因一次列全（服务没起来、装的是旧版、sshd 关了转发、权限）。
	f2 := newFixture(t, devhosttest.Options{Username: "dev", NoForward: true})
	row2 := f2.enroll(t, "dev", true)
	_, err := f2.dev.Install(ctx, devhost.InstallRequest{ID: row2.ID})
	if code(t, err) != devhost.CodeUnreachable {
		t.Fatalf("禁止转发 = %v", err)
	}
	for _, want := range []string{"systemctl status llmgate-devd.service", "旧版守护进程", "AllowStreamLocalForwarding", devd.DefaultSocket} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("禁止转发的说明里缺 %q：%v", want, err)
		}
	}
	if got, _ := f2.hosts.Get(ctx, row2.ID); got.Devd.Status != store.DevdStatusError {
		t.Fatalf("禁止转发后行 = %+v", got.Devd)
	}

	// 对端如实回 administratively prohibited（非 OpenSSH）：这时才折成 forward_denied。
	f3 := newFixture(t, devhosttest.Options{Username: "dev", NoForward: true, ForwardProhibited: true})
	row3 := f3.enroll(t, "dev", true)
	_, err = f3.dev.Install(ctx, devhost.InstallRequest{ID: row3.ID})
	if code(t, err) != devhost.CodeForwardDenied || !strings.Contains(err.Error(), "AllowStreamLocalForwarding") {
		t.Fatalf("明说禁止转发 = %v", err)
	}
}

// TestProxyRecordsReachability：devd 透传自己也要把可达性记回行里——守护进程掉线后
// 主机表不能还写着「已安装、正常」，恢复后也不用管理员手动点一次「检查 devd」才转好。
func TestProxyRecordsReachability(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, devhosttest.Options{Username: "dev"})
	row := f.enroll(t, "dev", true)
	if _, err := f.dev.Install(ctx, devhost.InstallRequest{ID: row.ID}); err != nil {
		t.Fatal(err)
	}

	f.host.MarkUninstalled()
	if _, err := f.dev.Proxy(ctx, row.ID, http.MethodGet, "info", nil, nil, ""); code(t, err) != devhost.CodeUnreachable {
		t.Fatalf("掉线透传 = %v", err)
	}
	got, _ := f.hosts.Get(ctx, row.ID)
	if got.Devd.Status != store.DevdStatusError || got.Devd.LastError == "" {
		t.Fatalf("掉线透传后行 = %+v", got.Devd)
	}
	if got.Devd.Version == "" {
		t.Fatalf("掉线不该抹掉自述：%+v", got.Devd)
	}

	// 守护进程回来：下一次透传就把行转回正常，不必再点「检查 devd」。
	f.host.MarkInstalled()
	if _, err := f.dev.Proxy(ctx, row.ID, http.MethodGet, "info", nil, nil, ""); err != nil {
		t.Fatalf("恢复透传 = %v", err)
	}
	if got, _ := f.hosts.Get(ctx, row.ID); got.Devd.Status != store.DevdStatusReady || got.Devd.LastError != "" {
		t.Fatalf("恢复后行 = %+v", got.Devd)
	}

	// 守护进程自己答出来的错（这里是不存在的路径）说明它活着：行保持正常。
	resp, err := f.dev.Proxy(ctx, row.ID, http.MethodGet, "fs/list", url.Values{"path": {"../../etc"}}, nil, "")
	if err != nil {
		t.Fatalf("越界读取应由守护进程答错：%v", err)
	}
	if resp.Status/100 != 4 {
		t.Fatalf("越界读取 = %d", resp.Status)
	}
	if got, _ := f.hosts.Get(ctx, row.ID); got.Devd.Status != store.DevdStatusReady {
		t.Fatalf("守护进程答错后行 = %+v", got.Devd)
	}
}
