package gatehelper

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const installFixtureKey = "sk_bootstrap_example_only"

func TestInstallShRollsBackAndCommitsAsOneUnit(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("fixture archive matches the test host")
	}
	newProgram := fakeBootstrapProgram()
	archive := tarGZFixture(t, "gate", newProgram)
	srv := releaseFixtureServer(t, "gate-linux-amd64.tar.gz", archive, false)
	defer srv.Close()

	scriptBytes, err := files.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, scriptBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	configRoot := t.TempDir()
	target := filepath.Join(installDir, "gate")
	configFile := filepath.Join(configRoot, "config.json")
	oldProgram := []byte("#!/bin/sh\necho old-gate\n")
	oldConfig := []byte("old-config\n")
	writeFixture(t, target, oldProgram, 0o700)
	writeFixture(t, configFile, oldConfig, 0o600)

	home := t.TempDir()
	zshrc := filepath.Join(home, ".zshrc")
	pathLine := "export PATH=\"" + installDir + ":$PATH\""
	runInstallSh := func(fail bool) ([]byte, error) {
		cmd := exec.Command("sh", script, srv.URL, installFixtureKey)
		cmd.Env = []string{
			"HOME=" + home,
			"SHELL=/bin/zsh",
			"PATH=" + os.Getenv("PATH"),
			"GATE_INSTALL_DIR=" + installDir,
			"GATE_CONFIG_DIR=" + configRoot,
		}
		if fail {
			cmd.Env = append(cmd.Env, "FAKE_BOOTSTRAP_FAIL=1")
		}
		return cmd.CombinedOutput()
	}

	out, err := runInstallSh(true)
	if err == nil {
		t.Fatalf("failed bootstrap unexpectedly succeeded: %s", out)
	}
	assertFileBytes(t, target, oldProgram)
	assertFileBytes(t, configFile, oldConfig)
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("installer output leaked API key: %s", out)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configFile); err != nil {
		t.Fatal(err)
	}
	out, err = runInstallSh(true)
	if err == nil {
		t.Fatalf("failed fresh bootstrap unexpectedly succeeded: %s", out)
	}
	assertFileMissing(t, target)
	assertFileMissing(t, configFile)
	// PATH 只在程序与配置都提交后才写，失败路径不得碰启动文件。
	assertFileMissing(t, zshrc)

	out, err = runInstallSh(false)
	if err != nil {
		t.Fatalf("successful bootstrap failed: %v\n%s", err, out)
	}
	assertFileBytes(t, target, newProgram)
	assertFileBytes(t, configFile, []byte("new-config\n"))
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("installer output leaked API key: %s", out)
	}
	assertLineCount(t, zshrc, pathLine, 1)
	if !bytes.Contains(out, []byte("已把 "+installDir+" 写入 "+zshrc)) || !bytes.Contains(out, []byte("  "+pathLine+"\n")) {
		t.Fatalf("installer output should name the startup file and the export line:\n%s", out)
	}

	// 重复执行不重复追加同一行。
	out, err = runInstallSh(false)
	if err != nil {
		t.Fatalf("repeated bootstrap failed: %v\n%s", err, out)
	}
	assertLineCount(t, zshrc, pathLine, 1)
}

// TestInstallShWritesPathForLoginShell pins which startup file each login
// shell gets, the $HOME spelling for directories under HOME, and that
// directories already on PATH or unknown shells only produce the manual hint.
func TestInstallShWritesPathForLoginShell(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("fixture archive matches the test host")
	}
	archive := tarGZFixture(t, "gate", fakeBootstrapProgram())
	srv := releaseFixtureServer(t, "gate-linux-amd64.tar.gz", archive, false)
	defer srv.Close()
	scriptBytes, err := files.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, scriptBytes, 0o700); err != nil {
		t.Fatal(err)
	}

	type tc struct {
		name       string
		shell      string
		installDir string // 相对 HOME 的安装目录
		zdotdir    string // 相对 HOME 的 ZDOTDIR；空表示不设
		onPath     bool   // PATH 已含安装目录
		wantFile   string // 相对 HOME 的启动文件；空表示不得写任何文件
		wantLine   string
		wantHint   bool // 期望只打印手工提示
	}
	cases := []tc{
		{name: "zsh-under-home", shell: "/bin/zsh", installDir: ".local/bin", wantFile: ".zshrc", wantLine: `export PATH="$HOME/.local/bin:$PATH"`},
		{name: "zsh-zdotdir", shell: "/bin/zsh", installDir: ".local/bin", zdotdir: "zdot", wantFile: "zdot/.zshrc", wantLine: `export PATH="$HOME/.local/bin:$PATH"`},
		{name: "bash", shell: "/bin/bash", installDir: "bin", wantFile: ".bashrc", wantLine: `export PATH="$HOME/bin:$PATH"`},
		{name: "fish", shell: "/usr/bin/fish", installDir: ".local/bin", wantFile: ".config/fish/conf.d/gate.fish", wantLine: `if not contains -- "$HOME/.local/bin" $PATH; set -gx PATH "$HOME/.local/bin" $PATH; end`},
		{name: "sh-profile", shell: "/bin/sh", installDir: ".local/bin", wantFile: ".profile", wantLine: `export PATH="$HOME/.local/bin:$PATH"`},
		{name: "no-shell-defaults-bash", shell: "", installDir: ".local/bin", wantFile: ".bashrc", wantLine: `export PATH="$HOME/.local/bin:$PATH"`},
		{name: "unknown-shell-hint-only", shell: "/usr/bin/nu", installDir: ".local/bin", wantHint: true},
		{name: "already-on-path", shell: "/bin/zsh", installDir: ".local/bin", onPath: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			installDir := filepath.Join(home, c.installDir)
			configRoot := t.TempDir()
			path := os.Getenv("PATH")
			if c.onPath {
				path = installDir + ":" + path
			}
			cmd := exec.Command("sh", script, srv.URL, installFixtureKey)
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + path,
				"GATE_INSTALL_DIR=" + installDir,
				"GATE_CONFIG_DIR=" + configRoot,
			}
			if c.shell != "" {
				cmd.Env = append(cmd.Env, "SHELL="+c.shell)
			}
			if c.zdotdir != "" {
				cmd.Env = append(cmd.Env, "ZDOTDIR="+filepath.Join(home, c.zdotdir))
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if bytes.Contains(out, []byte(installFixtureKey)) {
				t.Fatalf("installer output leaked API key: %s", out)
			}
			// HOME 下除安装目录外的所有条目都视为启动文件写入。
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			topInstall := strings.SplitN(c.installDir, "/", 2)[0]
			var startup []string
			for _, e := range entries {
				if e.Name() != topInstall {
					startup = append(startup, e.Name())
				}
			}
			switch {
			case c.wantFile != "":
				file := filepath.Join(home, c.wantFile)
				assertLineCount(t, file, c.wantLine, 1)
				if len(startup) != 1 {
					t.Fatalf("exactly one startup entry expected under HOME, got %v", startup)
				}
				if !bytes.Contains(out, []byte("已把 "+installDir+" 写入 "+file)) {
					t.Fatalf("output should name the startup file:\n%s", out)
				}
				if bytes.Contains(out, []byte("请把 ")) {
					t.Fatalf("manual hint must not appear after a successful write:\n%s", out)
				}
			case c.wantHint:
				if len(startup) != 0 {
					t.Fatalf("unknown shell must not touch startup files, got %v", startup)
				}
				if !bytes.Contains(out, []byte("请把 "+installDir+" 加入 PATH")) || !bytes.Contains(out, []byte(`export PATH="$HOME/.local/bin:$PATH"`)) {
					t.Fatalf("manual hint with the export line expected:\n%s", out)
				}
			default:
				if len(startup) != 0 {
					t.Fatalf("directory already on PATH must not touch startup files, got %v", startup)
				}
				if bytes.Contains(out, []byte("PATH")) {
					t.Fatalf("no PATH message expected when already on PATH:\n%s", out)
				}
			}
		})
	}
}

// assertLineCount checks that exactly want whole lines of path equal line.
func assertLineCount(t *testing.T, path, line string, want int) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := 0
	for _, l := range strings.Split(string(body), "\n") {
		if l == line {
			got++
		}
	}
	if got != want {
		t.Fatalf("%s has %d line(s) %q, want %d:\n%s", path, got, line, want, body)
	}
}

func TestInstallPs1RollsBackAndCommitsAsOneUnit(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("PowerShell fixture runs on the Linux CI host")
	}
	pwsh := "/opt/pwsh/pwsh"
	if _, err := os.Stat(pwsh); err != nil {
		t.Fatalf("PowerShell 7 is required for bootstrap coverage: %v", err)
	}
	newProgram := fakeBootstrapProgram()
	archive := zipFixture(t, "gate.exe", newProgram)
	srv := releaseFixtureServer(t, "gate-windows-amd64.zip", archive, true)
	defer srv.Close()

	scriptBytes, err := files.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "install.ps1")
	if err := os.WriteFile(script, scriptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	configRoot := t.TempDir()
	target := filepath.Join(installDir, "gate.exe")
	configFile := filepath.Join(configRoot, "config.json")
	oldProgram := []byte("#!/bin/sh\necho old-gate\n")
	oldConfig := []byte("old-config\n")
	writeFixture(t, target, oldProgram, 0o700)
	writeFixture(t, configFile, oldConfig, 0o600)

	runInstallPS := func(fail bool) ([]byte, error) {
		cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", script, srv.URL, installFixtureKey)
		cmd.Env = []string{
			"HOME=" + t.TempDir(),
			"PATH=" + os.Getenv("PATH"),
			"PROCESSOR_ARCHITECTURE=AMD64",
			"LOCALAPPDATA=" + t.TempDir(),
			"APPDATA=" + t.TempDir(),
			"GATE_INSTALL_DIR=" + installDir,
			"GATE_CONFIG_DIR=" + configRoot,
		}
		if fail {
			cmd.Env = append(cmd.Env, "FAKE_BOOTSTRAP_FAIL=1")
		}
		return cmd.CombinedOutput()
	}

	out, err := runInstallPS(true)
	if err == nil {
		t.Fatalf("failed PowerShell bootstrap unexpectedly succeeded: %s", out)
	}
	assertFileBytes(t, target, oldProgram)
	assertFileBytes(t, configFile, oldConfig)
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("PowerShell installer output leaked API key: %s", out)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configFile); err != nil {
		t.Fatal(err)
	}
	out, err = runInstallPS(true)
	if err == nil {
		t.Fatalf("failed fresh PowerShell bootstrap unexpectedly succeeded: %s", out)
	}
	assertFileMissing(t, target)
	assertFileMissing(t, configFile)

	out, err = runInstallPS(false)
	if err != nil {
		t.Fatalf("successful PowerShell bootstrap failed: %v\n%s", err, out)
	}
	assertFileBytes(t, target, newProgram)
	assertFileBytes(t, configFile, []byte("new-config\n"))
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("PowerShell installer output leaked API key: %s", out)
	}

	const runtimeArch = "$runtimeArch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture"
	fallbackScriptBytes := bytes.Replace(scriptBytes, []byte(runtimeArch), []byte("$runtimeArch = $null"), 1)
	if bytes.Equal(fallbackScriptBytes, scriptBytes) {
		t.Fatal("PowerShell installer no longer contains the expected RuntimeInformation probe")
	}
	fallbackScript := filepath.Join(t.TempDir(), "install-fallback.ps1")
	if err := os.WriteFile(fallbackScript, fallbackScriptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	script = fallbackScript
	installDir = t.TempDir()
	configRoot = t.TempDir()
	target = filepath.Join(installDir, "gate.exe")
	configFile = filepath.Join(configRoot, "config.json")
	out, err = runInstallPS(false)
	if err != nil {
		t.Fatalf("PowerShell architecture fallback failed: %v\n%s", err, out)
	}
	assertFileBytes(t, target, newProgram)
	assertFileBytes(t, configFile, []byte("new-config\n"))
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("PowerShell fallback installer output leaked API key: %s", out)
	}
}

// 管理台给出的 Windows 命令把脚本当 scriptblock 在当前 PowerShell 会话里运行。用户 PATH 写进
// 注册表只对之后新开的终端生效，所以脚本还要把安装目录补进当前进程的 $env:Path，让同一窗口
// 紧接着就能执行 gate；重复安装不得重复追加，失败路径不得碰会话 PATH。
func TestInstallPs1AddsInstallDirToCurrentSessionPath(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("PowerShell fixture runs on the Linux CI host")
	}
	pwsh := "/opt/pwsh/pwsh"
	if _, err := os.Stat(pwsh); err != nil {
		t.Fatalf("PowerShell 7 is required for bootstrap coverage: %v", err)
	}
	archive := zipFixture(t, "gate.exe", fakeBootstrapProgram())
	srv := releaseFixtureServer(t, "gate-windows-amd64.zip", archive, true)
	defer srv.Close()

	scriptBytes, err := files.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "install.ps1")
	if err := os.WriteFile(script, scriptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	configRoot := t.TempDir()
	const existing = `C:\Existing\bin`

	runSession := func(fail bool) ([]byte, error) {
		// 一个 pwsh 进程内：先记下会话 PATH，再连续调用脚本两次，最后打印会话 PATH。
		// Linux 上 $env:Path 与 PATH 是两个变量，这里观察到的正是脚本改动的那一个。
		command := fmt.Sprintf(`$env:Path = '%s'
$block = [scriptblock]::Create((Get-Content -Raw -LiteralPath '%s'))
foreach ($i in 1, 2) {
  try { & $block '%s' '%s' } catch { Write-Output "INSTALL_FAILED: $($_.Exception.Message)" }
}
Write-Output "SESSION_PATH=$env:Path"`, existing, script, srv.URL, installFixtureKey)
		cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", command)
		cmd.Env = []string{
			"HOME=" + t.TempDir(),
			"PATH=" + os.Getenv("PATH"),
			"PROCESSOR_ARCHITECTURE=AMD64",
			"LOCALAPPDATA=" + t.TempDir(),
			"APPDATA=" + t.TempDir(),
			"GATE_INSTALL_DIR=" + installDir,
			"GATE_CONFIG_DIR=" + configRoot,
		}
		if fail {
			cmd.Env = append(cmd.Env, "FAKE_BOOTSTRAP_FAIL=1")
		}
		return cmd.CombinedOutput()
	}
	sessionPath := func(out []byte) string {
		t.Helper()
		for _, line := range strings.Split(string(out), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "SESSION_PATH="); ok {
				return rest
			}
		}
		t.Fatalf("PowerShell session did not report its PATH:\n%s", out)
		return ""
	}

	out, err := runSession(true)
	if err != nil {
		t.Fatalf("PowerShell session with failing bootstrap exited abnormally: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("INSTALL_FAILED:")) {
		t.Fatalf("failing bootstrap did not surface as an installer error:\n%s", out)
	}
	if got := sessionPath(out); got != existing {
		t.Fatalf("failed install changed the session PATH to %q, want %q untouched", got, existing)
	}
	assertFileMissing(t, filepath.Join(installDir, "gate.exe"))

	out, err = runSession(false)
	if err != nil {
		t.Fatalf("PowerShell session with successful bootstrap exited abnormally: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("INSTALL_FAILED:")) {
		t.Fatalf("successful bootstrap reported an installer error:\n%s", out)
	}
	if bytes.Contains(out, []byte(installFixtureKey)) {
		t.Fatalf("PowerShell installer output leaked API key: %s", out)
	}
	if !bytes.Contains(out, []byte("PATH")) {
		t.Fatalf("successful install printed no PATH hint:\n%s", out)
	}
	parts := strings.Split(sessionPath(out), ";")
	if parts[0] != existing {
		t.Fatalf("session PATH lost its original head: %q", parts)
	}
	count := 0
	for _, part := range parts {
		if part == installDir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("install dir appears %d time(s) in session PATH after two installs, want exactly 1: %q", count, parts)
	}
}

// Linux 的 .NET 不持久化 User 环境变量；用可拒绝读写的替身执行完整安装器，
// 覆盖 Windows 用户 PATH 权限不足、重复安装及 PATH 归属记录失败的回滚。
func TestInstallPs1UserPathFailuresAndOwnership(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("PowerShell fixture runs on the Linux CI host")
	}
	const pwsh = "/opt/pwsh/pwsh"
	if _, err := os.Stat(pwsh); err != nil {
		t.Fatalf("PowerShell 7 is required for bootstrap coverage: %v", err)
	}
	program := bytes.Replace(fakeBootstrapProgram(), []byte(`if [ "$1" = "bootstrap" ]; then`), []byte(`if [ "$1" = "__record-install" ]; then
  printf 'new-state\n' > "$GATE_CONFIG_DIR/install-state.json"
  printf 'new-locator\n' > "$0.install.json"
  if [ "${2:-}" = "--user-path" ]; then
    printf 'record\n' >> "$GATE_TEST_PATH_RECORDS"
    if [ "${GATE_TEST_PATH_MODE:-}" = "record-fails" ]; then exit 19; fi
  fi
  exit 0
fi
if [ "$1" = "bootstrap" ]; then`), 1)
	srv := releaseFixtureServer(t, "gate-windows-amd64.zip", zipFixture(t, "gate.exe", program), true)
	defer srv.Close()
	scriptBytes, err := files.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	const commitStart = "$commitScript = @'\n"
	before, commit, ok := strings.Cut(string(scriptBytes), commitStart)
	if !ok {
		t.Fatal("missing commit script")
	}
	commit, after, ok := strings.Cut(commit, "\n'@")
	if !ok {
		t.Fatal("missing commit script terminator")
	}
	const environmentFixture = `class GateTestEnvironment {
  static [string] GetEnvironmentVariable([string]$name, [string]$target) {
    if ($name -ne 'Path' -or $target -ne 'User') { throw 'Unexpected environment read' }
    if ($env:GATE_TEST_PATH_MODE -eq 'read-denied') {
      throw [System.Security.SecurityException]::new('Requested registry access is not allowed.')
    }
    return [IO.File]::ReadAllText($env:GATE_TEST_USER_PATH)
  }
  static [void] SetEnvironmentVariable([string]$name, [string]$value, [string]$target) {
    if ($name -ne 'Path' -or $target -ne 'User') { throw 'Unexpected environment write' }
    if ($env:GATE_TEST_PATH_MODE -eq 'write-denied') {
      throw [UnauthorizedAccessException]::new('Attempted to perform an unauthorized operation.')
    }
    [IO.File]::WriteAllText($env:GATE_TEST_USER_PATH, $value)
  }
}
`
	script := filepath.Join(t.TempDir(), "install.ps1")
	writeFixture(t, script, []byte(before+commitStart+environmentFixture+strings.ReplaceAll(commit, "[Environment]::", "[GateTestEnvironment]::")+"\n'@"+after), 0o600)
	for _, tc := range []struct {
		name           string
		mode           string
		upgrade        bool
		alreadyOnPath  bool
		wantWarning    bool
		wantFailure    bool
		wantPathRecord bool
	}{
		{name: "fresh-write-denied", mode: "write-denied", wantWarning: true},
		{name: "upgrade-write-denied", mode: "write-denied", upgrade: true, wantWarning: true},
		{name: "read-denied", mode: "read-denied", wantWarning: true},
		{name: "persistent-path-idempotent", wantPathRecord: true},
		{name: "existing-path-not-owned", alreadyOnPath: true},
		{name: "fresh-record-failure", mode: "record-fails", wantFailure: true, wantPathRecord: true},
		{name: "upgrade-record-failure", mode: "record-fails", upgrade: true, wantFailure: true, wantPathRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installDir, configRoot := t.TempDir(), t.TempDir()
			target := filepath.Join(installDir, "gate.exe")
			configFile := filepath.Join(configRoot, "config.json")
			stateFile := filepath.Join(configRoot, "install-state.json")
			locatorFile := target + ".install.json"
			oldFiles := map[string][]byte{
				target: []byte("old-program\n"), configFile: []byte("old-config\n"),
				stateFile: []byte("old-state\n"), locatorFile: []byte("old-locator\n"),
			}
			if tc.upgrade {
				for name, body := range oldFiles {
					writeFixture(t, name, body, 0o700)
				}
			}
			const existing = `C:\Existing\bin`
			previousPath := existing
			if tc.alreadyOnPath {
				previousPath += ";" + installDir
			}
			userPathFile := filepath.Join(t.TempDir(), "user-path")
			pathRecords := filepath.Join(t.TempDir(), "path-records")
			writeFixture(t, userPathFile, []byte(previousPath), 0o600)
			// 两次调用检验当前会话和持久 PATH 均不重复追加；真正提交失败则只调用一次。
			attempts := 2
			if tc.wantFailure {
				attempts = 1
			}
			command := fmt.Sprintf(`$env:Path = '%s'
$block = [scriptblock]::Create((Get-Content -Raw -LiteralPath '%s'))
foreach ($i in 1..%d) {
  try { & $block '%s' '%s' } catch { Write-Output "INSTALL_FAILED: $($_.Exception.Message)" }
}
Write-Output "SESSION_PATH=$env:Path"`, existing, script, attempts, srv.URL, installFixtureKey)
			cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", command)
			cmd.Env = []string{
				"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"),
				"LOCALAPPDATA=" + t.TempDir(), "APPDATA=" + t.TempDir(),
				"GATE_INSTALL_DIR=" + installDir, "GATE_CONFIG_DIR=" + configRoot,
				"GATE_TEST_USER_PATH=" + userPathFile, "GATE_TEST_PATH_MODE=" + tc.mode,
				"GATE_TEST_PATH_RECORDS=" + pathRecords,
			}
			out, err := cmd.CombinedOutput()
			if bytes.Contains(out, []byte(installFixtureKey)) {
				t.Fatal("PowerShell installer output leaked API key")
			}
			if err != nil {
				t.Fatalf("PowerShell session failed: %v\n%s", err, out)
			}
			if bytes.Contains(out, []byte("INSTALL_FAILED:")) != tc.wantFailure {
				t.Fatalf("unexpected installation result:\n%s", out)
			}
			if bytes.Contains(out, []byte("无法更新用户 PATH")) != tc.wantWarning {
				t.Fatalf("unexpected user PATH warning:\n%s", out)
			}
			wantSessionPath := existing
			wantUserPath := previousPath
			if tc.wantFailure {
				for name, body := range oldFiles {
					if tc.upgrade {
						assertFileBytes(t, name, body)
					} else {
						assertFileMissing(t, name)
					}
				}
			} else {
				assertFileBytes(t, target, program)
				assertFileBytes(t, configFile, []byte("new-config\n"))
				assertFileBytes(t, stateFile, []byte("new-state\n"))
				assertFileBytes(t, locatorFile, []byte("new-locator\n"))
				wantSessionPath += ";" + installDir
				if !tc.wantWarning && !tc.alreadyOnPath {
					wantUserPath += ";" + installDir
				}
			}
			assertFileBytes(t, userPathFile, []byte(wantUserPath))
			if !strings.Contains(strings.ReplaceAll(string(out), "\r\n", "\n"), "SESSION_PATH="+wantSessionPath+"\n") {
				t.Fatalf("unexpected session PATH, want %q:\n%s", wantSessionPath, out)
			}
			if tc.wantPathRecord {
				assertFileBytes(t, pathRecords, []byte("record\n"))
			} else {
				assertFileMissing(t, pathRecords)
			}
			if tc.wantWarning && bytes.Contains(out, []byte("其他终端需要重新打开")) {
				t.Fatalf("installer must not claim reopening terminals fixes a denied user PATH write:\n%s", out)
			}
		})
	}
}

func fakeBootstrapProgram() []byte {
	return []byte(`#!/bin/sh
if [ "$1" = "__installer-exec" ]; then shift; exec "$@"; fi
if [ "$1" = "bootstrap" ]; then
  mkdir -p "$GATE_CONFIG_DIR"
  if [ "${FAKE_BOOTSTRAP_FAIL:-}" = "1" ]; then
    printf 'partial-config\n' > "$GATE_CONFIG_DIR/config.json"
    exit 17
  fi
  printf 'new-config\n' > "$GATE_CONFIG_DIR/config.json"
  printf 'bootstrap ok\n'
  exit 0
fi
exit 0
`)
}

func releaseFixtureServer(t *testing.T, name string, body []byte, jsonManifest bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(body)
	mux := http.NewServeMux()
	mux.HandleFunc("/gate-helper/releases/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	mux.HandleFunc("/gate-helper/releases/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%x  %s\n", sum, name)
	})
	mux.HandleFunc("/gate-helper/releases/stable.json", func(w http.ResponseWriter, _ *http.Request) {
		if !jsonManifest {
			http.NotFound(w, nil)
			return
		}
		fmt.Fprintf(w, `{"schema_version":1,"version":"test","assets":[{"name":%q,"sha256":%q,"size":%d}]}`, name, fmt.Sprintf("%x", sum), len(body))
	})
	return httptest.NewServer(mux)
}

func tarGZFixture(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func zipFixture(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeFixture(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed:\n got %q\nwant %q", path, got, want)
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should not exist after rollback: %v", path, err)
	}
}

func TestInstallScriptsDoNotCarryLegacyToolInstallRoutes(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1"} {
		body, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, legacy := range []string{"/codex-helper/install.", "/grok-helper/install.", "/claude-helper/install."} {
			if strings.Contains(string(body), legacy) {
				t.Errorf("%s contains legacy route %q", name, legacy)
			}
		}
	}
}
