package agenthost

// 远端脚本的语义验收：把设备真正下发的那几段 POSIX sh 拿到本机 /bin/sh 上跑一遍
// （HOME 指向临时目录），验的是「装公钥幂等、只动自己那几行、权限收得住」。
// 假主机（agenthosttest）验的是流程，这里验的是脚本本身——两者缺一不可。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func runSh(t *testing.T, script, home, stdin string) (string, int) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("本机没有 /bin/sh")
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exitErr):
		return out.String(), exitErr.ExitCode()
	default:
		t.Fatalf("跑脚本失败: %v（stderr=%s）", err, errBuf.String())
		return "", 0
	}
}

func authorizedKeys(t *testing.T, home string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatalf("读 authorized_keys: %v", err)
	}
	lines := []string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

const (
	keyA     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA llmgate-agent"
	keyB     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB llmgate-agent"
	otherKey = "ssh-rsa AAAAB3NzaC1yc2E-someone-else vendor@laptop"
)

func TestInstallKeyScriptIsIdempotentAndLeavesOtherKeys(t *testing.T) {
	home := t.TempDir()
	// 主机上本来就有别人的公钥：脚本不许动它。
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(otherKey+"\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	for range 2 { // 跑两遍：幂等
		if _, code := runSh(t, installKeyScript, home, keyA+"\n\n"); code != 0 {
			t.Fatalf("装公钥退出码 = %d", code)
		}
	}
	if got := authorizedKeys(t, home); len(got) != 2 || got[0] != otherKey || got[1] != keyA {
		t.Fatalf("authorized_keys = %q", got)
	}
	if info, err := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限 = %o", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(home, ".ssh")); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("目录权限 = %o", info.Mode().Perm())
	}

	// 换证书：装新的、摘旧的，别人的照旧。
	if _, code := runSh(t, installKeyScript, home, keyB+"\n"+keyA+"\n"); code != 0 {
		t.Fatalf("换公钥退出码 = %d", code)
	}
	if got := authorizedKeys(t, home); len(got) != 2 || got[0] != otherKey || got[1] != keyB {
		t.Fatalf("换证书后 = %q", got)
	}
	// 临时文件不留在主机上。
	entries, err := os.ReadDir(filepath.Join(home, ".ssh"))
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("残留文件 = %v", entries)
	}
}

func TestInstallKeyScriptCreatesMissingDirectory(t *testing.T) {
	home := t.TempDir()
	if _, code := runSh(t, installKeyScript, home, keyA+"\n\n"); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if got := authorizedKeys(t, home); len(got) != 1 || got[0] != keyA {
		t.Fatalf("authorized_keys = %q", got)
	}
}

func TestInstallKeyScriptRejectsEmptyStdin(t *testing.T) {
	home := t.TempDir()
	if _, code := runSh(t, installKeyScript, home, "\n\n"); code != 3 {
		t.Fatalf("退出码 = %d，期望 3", code)
	}
}

func TestProbeScriptReportsInstalledAndSystem(t *testing.T) {
	home := t.TempDir()
	out, code := runSh(t, probeScript, home, keyA+"\n")
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	installed, system := parseProbe(out)
	if installed {
		t.Fatal("还没装就报「已安装」")
	}
	if system == "" {
		t.Fatal("没读到系统自述")
	}
	if _, code := runSh(t, installKeyScript, home, keyA+"\n\n"); code != 0 {
		t.Fatalf("装公钥退出码 = %d", code)
	}
	out, code = runSh(t, probeScript, home, keyA+"\n")
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if installed, _ = parseProbe(out); !installed {
		t.Fatalf("装好之后仍报未安装：%q", out)
	}
}

func TestRemoveKeyScriptTakesOnlyOurKeys(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{keyA, keyB} {
		if _, code := runSh(t, installKeyScript, home, k+"\n\n"); code != 0 {
			t.Fatalf("装公钥退出码 = %d", code)
		}
	}
	f := filepath.Join(home, ".ssh", "authorized_keys")
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if err := os.WriteFile(f, append([]byte(otherKey+"\n"), raw...), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	if _, code := runSh(t, removeKeyScript, home, keyA+"\n"+keyB+"\n"); code != 0 {
		t.Fatalf("摘公钥退出码 = %d", code)
	}
	if got := authorizedKeys(t, home); len(got) != 1 || got[0] != otherKey {
		t.Fatalf("摘干净之后 = %q", got)
	}
}

func TestRemoveKeyScriptOnHostWithoutFile(t *testing.T) {
	if _, code := runSh(t, removeKeyScript, t.TempDir(), keyA+"\n"); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
}

// sudoers 那段要经 `sudo -S -p ” /bin/sh -c '…'` 传过去：里面一旦出现单引号，
// 整条命令就会在对端被截断。
func TestSudoersScriptQuotingAndContent(t *testing.T) {
	script := sudoersScript("admin")
	if strings.Contains(script, "'") {
		t.Fatal("内层脚本不能含单引号")
	}
	for _, want := range []string{
		"f=/etc/sudoers.d/90-llmgate-admin",
		`printf "%s ALL=(ALL) NOPASSWD:ALL\n" "$u"`,
		"visudo -cf $t",
		"chmod 0440 $t",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("内层脚本缺 %q：\n%s", want, script)
		}
	}
	if got := sudoersPath("admin"); strings.Contains(filepath.Base(got), ".") {
		t.Fatalf("sudoers 文件名不能带点号：%q", got)
	}
}

// 证书私钥不能被任何一种打印方式印出来（§15.1：值与指针都要遮蔽）。
func TestCertKeyMasksPrivateKey(t *testing.T) {
	k, err := newCertKey(time.Now())
	if err != nil {
		t.Fatalf("生成: %v", err)
	}
	if !strings.Contains(k.privatePEM, "PRIVATE KEY") {
		t.Fatal("测试前提不成立：privatePEM 不像私钥")
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("cert", "key", *k)
	log.Info("cert", "key", k)
	raw, err := json.Marshal(k)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	buf.Write(raw)
	if raw, err = json.Marshal(*k); err != nil {
		t.Fatalf("json: %v", err)
	}
	buf.Write(raw)
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		buf.WriteString(fmt.Sprintf(format, *k))
		buf.WriteString(fmt.Sprintf(format, k))
	}
	if strings.Contains(buf.String(), "PRIVATE KEY") || strings.Contains(buf.String(), k.privatePEM) {
		t.Fatalf("私钥被印了出来：%s", buf.String())
	}
	if !strings.Contains(buf.String(), k.fingerprint()) {
		t.Fatalf("遮蔽之后连指纹都看不见：%s", buf.String())
	}
}

// 生成的公钥行必须是 sshd 认得的 authorized_keys 形状。
func TestCertKeyPublicLineParses(t *testing.T) {
	k, err := newCertKey(time.Now())
	if err != nil {
		t.Fatalf("生成: %v", err)
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(k.public))
	if err != nil {
		t.Fatalf("解析公钥行: %v", err)
	}
	if comment != keyComment || len(rest) != 0 {
		t.Fatalf("注释 = %q，剩余 = %q", comment, rest)
	}
	if ssh.FingerprintSHA256(pub) != k.fingerprint() {
		t.Fatal("指纹对不上")
	}
	signer, err := ssh.ParsePrivateKey([]byte(k.privatePEM))
	if err != nil {
		t.Fatalf("解析私钥: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("密钥类型 = %q", signer.PublicKey().Type())
	}
	// 每次生成都是新的一把，不是某个常量。
	again, err := newCertKey(time.Now())
	if err != nil {
		t.Fatalf("再生成: %v", err)
	}
	if again.fingerprint() == k.fingerprint() {
		t.Fatal("两次生成得到同一把密钥")
	}
}
