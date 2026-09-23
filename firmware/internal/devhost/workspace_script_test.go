package devhost

// 工作空间脚本交本机 /bin/sh 跑一遍（假 git 记 argv 与 credential.helper 的输出）：
// 凭证只经 helper 到达 git、不在 argv；已有目录退出码 91；没给仓库只建目录。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fakeGit = `#!/bin/sh
printf 'ARGV:%s\n' "$*" >> "$HOME/git.log"
helper=""
while [ $# -gt 0 ]; do
  case "$1" in -c) shift; case "$1" in credential.helper=!*) helper="${1#credential.helper=!}";; esac;; esac
  shift
done
if [ -n "$helper" ]; then sh -c "$helper" >> "$HOME/cred.log"; fi
exit 0
`

func TestWorkspaceScriptLocalShell(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("本机没有 /bin/sh")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	run := func(script, stdin string) (string, int) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}

	out, code := run(workspaceScript("demo", "https://example.com/o/r.git", "main", true), "alice\nsecret-token\n")
	if code != 0 || !strings.Contains(out, "path="+home+"/workspaces/demo") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	gitLog, _ := os.ReadFile(filepath.Join(home, "git.log"))
	credLog, _ := os.ReadFile(filepath.Join(home, "cred.log"))
	if strings.Contains(string(gitLog), "secret-token") || strings.Contains(string(gitLog), "alice") {
		t.Fatalf("凭证出现在 git 的 argv 里：%s", gitLog)
	}
	if !strings.Contains(string(credLog), "username=alice\npassword=secret-token\n") {
		t.Fatalf("helper 输出 = %q", credLog)
	}
	if !strings.Contains(string(gitLog), "ls-remote --exit-code --heads -- https://example.com/o/r.git refs/heads/main\n") ||
		!strings.Contains(string(gitLog), "clone --branch main -- https://example.com/o/r.git "+home+"/workspaces/demo\n") {
		t.Fatalf("git argv = %s", gitLog)
	}
	if _, err := os.Stat(filepath.Join(home, "workspaces", "demo")); err != nil {
		t.Fatal(err)
	}
	// 同名目录已在：91，不动它。
	if _, code := run(workspaceScript("demo", "", "", false), ""); code != wsExitDirExists {
		t.Fatalf("已存在时退出码 = %d", code)
	}
	// 没给仓库：只建目录，不调 git。
	before, _ := os.ReadFile(filepath.Join(home, "git.log"))
	if out, code := run(workspaceScript("scratch", "", "", false), ""); code != 0 || !strings.Contains(out, "path="+home+"/workspaces/scratch") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	after, _ := os.ReadFile(filepath.Join(home, "git.log"))
	if string(before) != string(after) {
		t.Fatal("空目录空间不该调 git")
	}
	// 删除：只认自己的路径形状。
	if _, code := run(workspaceRemoveScript(home+"/workspaces/scratch"), ""); code != 0 {
		t.Fatalf("删除退出码 = %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "workspaces", "scratch")); !os.IsNotExist(err) {
		t.Fatalf("目录没删掉：%v", err)
	}
}
