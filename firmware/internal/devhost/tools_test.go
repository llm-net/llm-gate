package devhost_test

// 工具配置（tools.go）验收：探测读数、装 git / tmux 的 root 路径、装 / 升级 / 卸载 gate 的前提与
// 结果，以及受控纳管主机一律被拒。假主机解释设备下发的命令，不出网。

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestToolsProbeAndPackageInstall(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", Pkg: "apt-get"})
	host := f.enroll(t, "dev", false)
	ctx := context.Background()

	tools, err := f.dev.Tools(ctx, host.ID)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if tools.Git.Installed || tools.Tmux.Installed || tools.Studio.FFmpeg.Installed || tools.PackageManager != "apt-get" || !tools.Curl || tools.Gate.Installed || tools.Home == "" {
		t.Fatalf("初始读数 = %+v", tools)
	}
	// 没免密 sudo 又没口令：先答 sudo_failed，不碰主机。
	if _, err := f.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: "git", ID: host.ID}); err == nil || code(t, err) != devhost.CodeSudoFailed {
		t.Fatalf("没口令应答 sudo_failed：%v", err)
	}
	if f.host.Git() != "" {
		t.Fatal("主机上不该装上 git")
	}
	tools, err = f.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: "git", ID: host.ID, Password: password})
	if err != nil {
		t.Fatalf("InstallPackage git: %v", err)
	}
	if !tools.Git.Installed || !strings.HasPrefix(tools.Git.Version, "git version") || f.host.Git() == "" {
		t.Fatalf("装完读数 = %+v", tools)
	}
	if tools.Tmux.Installed {
		t.Fatalf("tmux 不该被一起装上：%+v", tools.Tmux)
	}
	tools, err = f.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: "tmux", ID: host.ID, Password: password})
	if err != nil {
		t.Fatalf("InstallPackage tmux: %v", err)
	}
	if !tools.Tmux.Installed || !strings.HasPrefix(tools.Tmux.Version, "tmux ") || f.host.Tmux() == "" {
		t.Fatalf("装完 tmux 读数 = %+v", tools.Tmux)
	}
	if tools.Studio.FFmpeg.Installed {
		t.Fatal("安装 git / tmux 不该装上 FFmpeg")
	}
	// 不在词汇表里的程序名：invalid_host，不碰主机；创作工具只经动作队列装，不走这条同步路。
	for _, name := range []string{"vim", "ffmpeg", "fonts-cjk"} {
		if _, err := f.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: name, ID: host.ID, Password: password}); err == nil || code(t, err) != devhost.CodeInvalidHost {
			t.Fatalf("同步安装 %s 应答 invalid_host：%v", name, err)
		}
	}
	if f.host.FFmpeg() != "" {
		t.Fatal("同步路不该装上 FFmpeg")
	}
	// 主机没有认得的包管理器：tool_missing。
	g := newFixture(t, devhosttest.Options{Username: "root"})
	other := g.enroll(t, "root", false)
	if _, err := g.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: "git", ID: other.ID}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没有包管理器应答 tool_missing：%v", err)
	}
	// 口令不进日志。
	for _, e := range f.host.Execs() {
		if strings.Contains(e.Command, password) {
			t.Fatal("口令拼进了命令行")
		}
	}
}

func TestGateInstallUpgradeAndUninstall(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev"})
	host := f.enroll(t, "dev", true)
	ctx := context.Background()

	// 地址形态：只认站点根。
	for _, bad := range []string{"", "192.168.1.10", "ftp://x", "http://u:p@h", "http://h/path", "http://h?x=1", "http://h 'x"} {
		if _, err := f.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: host.ID, BaseURL: bad, Key: "sk_abc12345"}); err == nil || code(t, err) != devhost.CodeInvalidHost {
			t.Fatalf("%q 应被拒：%v", bad, err)
		}
	}
	// 首次安装没带 Key：不碰主机，先说明。
	if _, err := f.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: host.ID, BaseURL: "http://192.168.1.10/"}); err == nil || code(t, err) != devhost.CodeInvalidHost {
		t.Fatalf("首次安装没 Key 应被拒：%v", err)
	}
	if f.host.Gate().Version != "" {
		t.Fatal("主机上不该装上 gate")
	}
	res, err := f.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: host.ID, BaseURL: "http://192.168.1.10/", Key: "sk_abcdefghij1234567890"})
	if err != nil {
		t.Fatalf("InstallGate: %v", err)
	}
	if !res.Tools.Gate.Installed || !res.Tools.Gate.Configured || res.Tools.Gate.BaseURL != "http://192.168.1.10" || res.Tools.Gate.Version == "" || res.Output == "" {
		t.Fatalf("装完读数 = %+v / %q", res.Tools.Gate, res.Output)
	}
	if got := f.host.Gate(); got.LastKey != "sk_abcdefghij1234567890" || got.Base != "http://192.168.1.10" {
		t.Fatalf("主机上的 gate = %+v", got)
	}
	// 升级：不带 Key，沿用主机上保存的。
	res, err = f.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: host.ID, BaseURL: "https://box.example"})
	if err != nil {
		t.Fatalf("升级: %v", err)
	}
	if got := f.host.Gate(); got.LastKey != "" || got.Base != "https://box.example" || !got.HasKey || res.Tools.Gate.BaseURL != "https://box.example" {
		t.Fatalf("升级后主机上的 gate = %+v", got)
	}
	// 升级：跑 gate update，地址与 Key 都不动。
	res, err = f.dev.UpdateGate(ctx, host.ID)
	if err != nil {
		t.Fatalf("UpdateGate: %v", err)
	}
	if got := f.host.Gate(); got.Version != "fake-gate-updated" || got.Base != "https://box.example" || !got.HasKey || got.LastKey != "" {
		t.Fatalf("升级后主机上的 gate = %+v", got)
	}
	if res.Tools.Gate.BaseURL != "https://box.example" || !strings.Contains(res.Output, "updated") {
		t.Fatalf("升级后读数 = %+v / %q", res.Tools.Gate, res.Output)
	}
	// 设置 gate：跑 gate bootstrap 地址 [Key]；非法地址 / Key 不碰主机。
	if _, err := f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "https://box.example", Key: "bad key"}); err == nil || code(t, err) != devhost.CodeInvalidHost {
		t.Fatalf("非法 Key 应被拒：%v", err)
	}
	if _, err := f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "box.example"}); err == nil || code(t, err) != devhost.CodeInvalidHost {
		t.Fatalf("非法地址应被拒：%v", err)
	}
	res, err = f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "http://192.168.1.20/", Key: "sk_replacement0987654321"})
	if err != nil {
		t.Fatalf("ConfigureGate: %v", err)
	}
	if got := f.host.Gate(); got.LastKey != "sk_replacement0987654321" || got.Base != "http://192.168.1.20" || got.Version != "fake-gate-updated" {
		t.Fatalf("设置后主机上的 gate = %+v", got)
	}
	if !res.Tools.Gate.Configured || res.Tools.Gate.BaseURL != "http://192.168.1.20" || !strings.Contains(res.Output, "verified and saved") {
		t.Fatalf("设置后读数 = %+v / %q", res.Tools.Gate, res.Output)
	}
	// 只换地址：不带 Key，沿用已保存的。
	if _, err := f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "https://box.example"}); err != nil {
		t.Fatalf("只换地址: %v", err)
	}
	if got := f.host.Gate(); got.LastKey != "" || got.Base != "https://box.example" || !got.HasKey {
		t.Fatalf("只换地址后主机上的 gate = %+v", got)
	}
	// 卸载。
	tools, err := f.dev.UninstallGate(ctx, host.ID)
	if err != nil {
		t.Fatalf("UninstallGate: %v", err)
	}
	if tools.Gate.Installed || f.host.Gate().Version != "" {
		t.Fatalf("卸载后 = %+v", tools.Gate)
	}
	if _, err := f.dev.UninstallGate(ctx, host.ID); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("再卸应答 tool_missing：%v", err)
	}
	// 没装 gate 时设置或升级：tool_missing。
	if _, err := f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "https://box.example", Key: "sk_replacement0987654321"}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没装 gate 设置应答 tool_missing：%v", err)
	}
	if _, err := f.dev.UpdateGate(ctx, host.ID); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没装 gate 升级应答 tool_missing：%v", err)
	}
	// 主机取不到安装脚本：host_cannot_reach_device，带主机上 curl 的报错，不装。
	u := newFixture(t, devhosttest.Options{Username: "dev", GateUnreachable: true})
	uh := u.enroll(t, "dev", true)
	if _, err := u.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: uh.ID, BaseURL: "https://box.example", Key: "sk_abcdefghij"}); err == nil || code(t, err) != devhost.CodeHostCannotReach || !strings.Contains(err.Error(), "Could not resolve host") {
		t.Fatalf("取不到脚本应答 host_cannot_reach_device 并带 curl 报错：%v", err)
	}
	if u.host.Gate().Version != "" {
		t.Fatal("取不到脚本时不该装上 gate")
	}
	// 没有 curl：tool_missing。
	g := newFixture(t, devhosttest.Options{Username: "dev", NoCurl: true})
	other := g.enroll(t, "dev", true)
	if _, err := g.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: other.ID, BaseURL: "http://h", Key: "sk_abcdefghij"}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没 curl 应答 tool_missing：%v", err)
	}
}

func TestToolsRefusedForManagedHost(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "root", Pkg: "apt-get"})
	host := f.enrollKind(t, store.AgentHostKindManaged, "root", false)
	ctx := context.Background()
	if _, err := f.dev.Tools(ctx, host.ID); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("探测应被拒：%v", err)
	}
	if _, err := f.dev.InstallPackage(ctx, devhost.PackageInstallRequest{Name: "git", ID: host.ID}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("装 git 应被拒：%v", err)
	}
	if _, err := f.dev.InstallGate(ctx, devhost.GateInstallRequest{ID: host.ID, BaseURL: "http://h", Key: "sk_abcdefghij"}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("装 gate 应被拒：%v", err)
	}
	if _, err := f.dev.UninstallGate(ctx, host.ID); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("卸 gate 应被拒：%v", err)
	}
	if _, err := f.dev.ConfigureGate(ctx, devhost.GateConfigRequest{ID: host.ID, BaseURL: "http://h", Key: "sk_abcdefghij"}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("设置 gate 应被拒：%v", err)
	}
	if _, err := f.dev.UpdateGate(ctx, host.ID); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("升级 gate 应被拒：%v", err)
	}
	if len(f.host.Execs()) > 3 {
		// 纳管本身的两三条命令之外不该再碰主机。
		t.Fatalf("受控纳管主机被碰了：%d 条命令", len(f.host.Execs()))
	}
}

// gate 管的五个开发工具：探测读出关联表与 PATH 上的同名程序；install 幂等（PATH 有就只关联为
// external，没有才装受管副本）、update 只升级受管安装、disconnect / uninstall 撤关联；没装 gate
// 或 gate 没 Key 时不碰工具；工具名与动作只认词汇表。
func TestDevToolLifecycle(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", GateVersion: "fake-gate", GateBase: "http://192.168.1.10", GateKey: true,
		DevToolsFound: map[string]string{"claude": "/usr/local/bin/claude"}})
	host := f.enroll(t, "dev", false)
	ctx := context.Background()

	tools, err := f.dev.Tools(ctx, host.ID)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools.DevTools) != 6 || tools.DevTools[0].Name != "codex" || tools.DevTools[1].Name != "claude" {
		t.Fatalf("开发工具读数 = %+v", tools.DevTools)
	}
	if tools.DevTools[1].Linked || tools.DevTools[1].Found != "/usr/local/bin/claude" || tools.DevTools[0].Found != "" {
		t.Fatalf("初始读数 = %+v", tools.DevTools)
	}
	// 词汇表之外：不碰主机。
	for _, bad := range []devhost.DevToolRequest{{ID: host.ID, Tool: "vim", Action: devhost.DevToolInstall}, {ID: host.ID, Tool: "codex", Action: "purge"}} {
		if _, err := f.dev.DevTool(ctx, bad); err == nil || code(t, err) != devhost.CodeInvalidHost {
			t.Fatalf("%+v 应答 invalid_host：%v", bad, err)
		}
	}
	// 安装 codex：主机上没有，装受管副本。
	res, err := f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "codex", Action: devhost.DevToolInstall})
	if err != nil {
		t.Fatalf("install codex: %v", err)
	}
	codex := res.Tools.DevTools[0]
	if !codex.Linked || codex.Origin != "gate" || codex.Version == "" || !strings.HasSuffix(codex.Path, "/bin/codex") || !strings.Contains(res.Output, "installed") {
		t.Fatalf("装完 codex = %+v / %q", codex, res.Output)
	}
	// 安装 claude：PATH 上有，只关联为 external；再 update 被 gate 拒绝、原样带回。
	res, err = f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "claude", Action: devhost.DevToolInstall})
	if err != nil {
		t.Fatalf("install claude: %v", err)
	}
	if claude := res.Tools.DevTools[1]; !claude.Linked || claude.Origin != "external" || claude.Path != "/usr/local/bin/claude" {
		t.Fatalf("关联 claude = %+v", claude)
	}
	if _, err := f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "claude", Action: devhost.DevToolUpdate}); err == nil || code(t, err) != devhost.CodeCommandFailed || !strings.Contains(err.Error(), "--adopt") {
		t.Fatalf("外部安装 update 应带回 gate 的拒绝：%v", err)
	}
	// 升级受管 codex。
	res, err = f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "codex", Action: devhost.DevToolUpdate})
	if err != nil {
		t.Fatalf("update codex: %v", err)
	}
	if got := res.Tools.DevTools[0].Version; got == codex.Version {
		t.Fatalf("升级后版本没变：%q", got)
	}
	// 解除关联 claude、卸载 codex。
	if res, err = f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "claude", Action: devhost.DevToolDisconnect}); err != nil || res.Tools.DevTools[1].Linked {
		t.Fatalf("disconnect claude: %v / %+v", err, res)
	}
	if res, err = f.dev.DevTool(ctx, devhost.DevToolRequest{ID: host.ID, Tool: "codex", Action: devhost.DevToolUninstall}); err != nil || res.Tools.DevTools[0].Linked {
		t.Fatalf("uninstall codex: %v / %+v", err, res)
	}
	if len(f.host.DevTools()) != 0 {
		t.Fatalf("主机上还留着关联：%+v", f.host.DevTools())
	}
	// 动作都经主机上的 gate 执行，子命令是恒定字面量。
	seen := 0
	for _, e := range f.host.Execs() {
		if strings.HasSuffix(e.Command, "/.local/bin/gate' codex install") || strings.HasSuffix(e.Command, "/.local/bin/gate' codex update") ||
			strings.HasSuffix(e.Command, "/.local/bin/gate' claude install") || strings.HasSuffix(e.Command, "/.local/bin/gate' claude update") ||
			strings.HasSuffix(e.Command, "/.local/bin/gate' claude disconnect") || strings.HasSuffix(e.Command, "/.local/bin/gate' codex uninstall --yes") {
			seen++
		}
	}
	if seen != 6 {
		t.Fatalf("经 gate 执行的动作数 = %d，想要 6", seen)
	}

	// gate 没 Key：安装 / 升级先拒（tool_missing）、不碰主机；解除关联不需要设备，照做。
	g := newFixture(t, devhosttest.Options{Username: "dev", GateVersion: "fake-gate", GateBase: "http://192.168.1.10",
		DevToolsLinked: map[string]devhosttest.DevToolState{"grok": {Path: "/opt/grok/bin/grok", Version: "grok 0.1", Origin: "gate"}}})
	other := g.enroll(t, "dev", false)
	if _, err := g.dev.DevTool(ctx, devhost.DevToolRequest{ID: other.ID, Tool: "codex", Action: devhost.DevToolInstall}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("gate 没 Key 应答 tool_missing：%v", err)
	}
	if _, err := g.dev.DevTool(ctx, devhost.DevToolRequest{ID: other.ID, Tool: "grok", Action: devhost.DevToolUpdate}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("gate 没 Key 升级应答 tool_missing：%v", err)
	}
	res, err = g.dev.DevTool(ctx, devhost.DevToolRequest{ID: other.ID, Tool: "grok", Action: devhost.DevToolDisconnect})
	if err != nil || res.Tools.DevTools[2].Linked || len(g.host.DevTools()) != 0 {
		t.Fatalf("没 Key 也能解除关联：%v / %+v", err, res)
	}
	// 没装 gate：一律 tool_missing。
	n := newFixture(t, devhosttest.Options{Username: "dev"})
	bare := n.enroll(t, "dev", false)
	if _, err := n.dev.DevTool(ctx, devhost.DevToolRequest{ID: bare.ID, Tool: "codex", Action: devhost.DevToolInstall}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没装 gate 应答 tool_missing：%v", err)
	}
}
