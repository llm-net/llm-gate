package devd

// 守护进程验收：文件目录、Git 与 tmux 三样能力在临时家目录里走一遍（Git 需要本机
// git，tmux 部分没有 tmux 即 skip），外加 Unix socket 监听与安装布局。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devd/wire"
)

type testEnv struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	client *http.Client
	home   string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	home := t.TempDir()
	// tmux 服务器隔离到临时目录：attach 会 source 内嵌的 tmux.conf，不能碰开发者自己的 tmux。
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	srv := New(Options{Home: home, User: "tester", Shell: "/bin/sh", Version: "test", StateDir: t.TempDir()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, srv: srv, ts: ts, home: home, client: ts.Client()}
}

func (e *testEnv) do(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestInfo(t *testing.T) {
	e := newEnv(t)
	status, body := e.do("GET", "/v1/info", nil)
	if status != 200 || body["version"] != "test" || body["home"] != e.home || body["user"] != "tester" {
		t.Fatalf("info = %d %v", status, body)
	}
	if status, _ := e.do("GET", "/v1/nope", nil); status != 404 {
		t.Fatalf("未知路径 = %d", status)
	}
}

// TestUnixSocket 验 ListenAndServe：socket 文件 0600、残留文件被清、ctx 结束即停。
func TestUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "devd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "devd.sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Home: t.TempDir(), User: "tester", Shell: "/bin/sh", Version: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, sock) }()
	var st os.FileInfo
	for i := 0; i < 100; i++ {
		if st, err = os.Stat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket 没起来：%v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket 权限 = %o", st.Mode().Perm())
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := client.Get("http://devd/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("info over socket = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 结束后没有停机")
	}
}

func TestFilesystem(t *testing.T) {
	e := newEnv(t)
	status, body := e.do("POST", "/v1/fs/mkdir", map[string]string{"path": "proj/src"})
	if status != 200 || body["path"] != filepath.Join(e.home, "proj/src") {
		t.Fatalf("mkdir = %d %v", status, body)
	}
	status, body = e.do("PUT", "/v1/fs/write", map[string]string{"path": "proj/src/main.go", "content": "package main\n"})
	if status != 200 || body["content"] != "package main\n" || body["binary"] != false {
		t.Fatalf("write = %d %v", status, body)
	}
	status, body = e.do("GET", "/v1/fs/list?path="+url.QueryEscape("~/proj"), nil)
	if status != 200 {
		t.Fatalf("list = %d %v", status, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "src" || entries[0].(map[string]any)["dir"] != true {
		t.Fatalf("entries = %v", entries)
	}
	if body["parent"] != e.home || body["git_root"] != nil {
		t.Fatalf("list 元数据 = %v", body)
	}
	// 二进制文件不回内容。
	os.WriteFile(filepath.Join(e.home, "blob.bin"), []byte("abc\x00def"), 0o644)
	status, body = e.do("GET", "/v1/fs/read?path=blob.bin", nil)
	if status != 200 || body["binary"] != true || body["content"] != "" {
		t.Fatalf("read binary = %d %v", status, body)
	}
	status, body = e.do("POST", "/v1/fs/rename", map[string]string{"from": "blob.bin", "to": "blob2.bin"})
	if status != 200 {
		t.Fatalf("rename = %d %v", status, body)
	}
	status, body = e.do("POST", "/v1/fs/delete", map[string]any{"path": "proj"})
	if status != 400 {
		t.Fatalf("非空目录不带 recursive 应拒绝：%d %v", status, body)
	}
	status, _ = e.do("POST", "/v1/fs/delete", map[string]any{"path": "proj", "recursive": true})
	if status != 200 {
		t.Fatalf("recursive delete = %d", status)
	}
	if _, err := os.Stat(filepath.Join(e.home, "proj")); !os.IsNotExist(err) {
		t.Fatal("目录应已删除")
	}
	status, body = e.do("GET", "/v1/fs/read?path=missing.txt", nil)
	if status != 404 || body["error"].(map[string]any)["code"] != CodeNotFound {
		t.Fatalf("missing = %d %v", status, body)
	}
	status, body = e.do("POST", "/v1/fs/delete", map[string]any{"path": "", "recursive": true})
	if status != 400 {
		t.Fatalf("删家目录应拒绝：%d %v", status, body)
	}
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("本机没有 git")
	}
}

func TestGit(t *testing.T) {
	gitAvailable(t)
	e := newEnv(t)
	repo := filepath.Join(e.home, "repo")
	os.MkdirAll(repo, 0o755)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@x")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	run("add", "a.txt")
	run("commit", "-q", "-m", "first")

	status, body := e.do("GET", "/v1/fs/list?path=repo", nil)
	if status != 200 || body["git_root"] != repo {
		t.Fatalf("list git_root = %d %v", status, body)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("new\n"), 0o644)
	status, body = e.do("GET", "/v1/git/status?path=repo", nil)
	if status != 200 || body["branch"] != "main" {
		t.Fatalf("status = %d %v", status, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	status, body = e.do("GET", "/v1/git/diff?path=repo&file=a.txt", nil)
	if status != 200 || !strings.Contains(body["diff"].(string), "+two") {
		t.Fatalf("diff = %d %v", status, body)
	}
	status, body = e.do("GET", "/v1/git/diff?path=repo&file=b.txt", nil)
	if status != 200 || !strings.Contains(body["diff"].(string), "+new") {
		t.Fatalf("untracked diff = %d %v", status, body)
	}
	if status, body = e.do("POST", "/v1/git/stage", map[string]any{"path": "repo", "files": []string{"a.txt", "b.txt"}}); status != 200 {
		t.Fatalf("stage = %d %v", status, body)
	}
	if status, body = e.do("POST", "/v1/git/stage", map[string]any{"path": "repo", "files": []string{"b.txt"}, "unstage": true}); status != 200 {
		t.Fatalf("unstage = %d %v", status, body)
	}
	status, body = e.do("GET", "/v1/git/status?path=repo", nil)
	var staged, untracked int
	for _, raw := range body["entries"].([]any) {
		en := raw.(map[string]any)
		if en["index"] == "M" {
			staged++
		}
		if en["untracked"] == true {
			untracked++
		}
	}
	if staged != 1 || untracked != 1 {
		t.Fatalf("staged=%d untracked=%d body=%v", staged, untracked, body)
	}
	if status, body = e.do("POST", "/v1/git/commit", map[string]string{"path": "repo", "message": "second"}); status != 200 {
		t.Fatalf("commit = %d %v", status, body)
	}
	status, body = e.do("GET", "/v1/git/log?path=repo&n=10", nil)
	commits := body["commits"].([]any)
	if status != 200 || len(commits) != 2 || commits[0].(map[string]any)["subject"] != "second" {
		t.Fatalf("log = %d %v", status, body)
	}
	status, body = e.do("POST", "/v1/git/checkout", map[string]any{"path": "repo", "branch": "feature", "create": true})
	if status != 200 {
		t.Fatalf("checkout = %d %v", status, body)
	}
	status, body = e.do("GET", "/v1/git/branches?path=repo", nil)
	branches := body["branches"].([]any)
	if status != 200 || len(branches) != 2 {
		t.Fatalf("branches = %d %v", status, body)
	}
	if status, body = e.do("POST", "/v1/git/discard", map[string]any{"path": "repo", "files": []string{"b.txt"}}); status != 200 {
		t.Fatalf("discard = %d %v", status, body)
	}
	if _, err := os.Stat(filepath.Join(repo, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("未跟踪文件放弃后应被删除")
	}
	status, body = e.do("GET", "/v1/git/status?path="+url.QueryEscape(e.home), nil)
	if status != 404 || body["error"].(map[string]any)["code"] != CodeNotRepo {
		t.Fatalf("非仓库 = %d %v", status, body)
	}
	if status, _ = e.do("POST", "/v1/git/checkout", map[string]any{"path": "repo", "branch": "--evil"}); status != 400 {
		t.Fatalf("非法分支名 = %d", status)
	}
}

// attach 走一遍 Upgrade 握手，回 wire 帧流。
func (e *testEnv) attach(name string) net.Conn {
	e.t.Helper()
	u, _ := url.Parse(e.ts.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		e.t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /v1/tmux/attach?name=%s&cols=80&rows=24 HTTP/1.1\r\nHost: %s\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", name, u.Host, UpgradeProtocol)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("attach = %d %s", resp.StatusCode, raw)
	}
	return conn
}

func TestTerminalAttach(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("本机没有 tmux")
	}
	if _, err := openPTY(); err != nil {
		t.Skipf("本机没有伪终端：%v", err)
	}
	e := newEnv(t)
	name := fmt.Sprintf("llmgate-test-%d", os.Getpid())
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	conn := e.attach(name)
	defer conn.Close()
	marker := "llmgate-marker-ok"
	if err := wire.Write(conn, wire.Data, []byte("echo "+marker+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := wire.Write(conn, wire.Resize, wire.ResizePayload(100, 30)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	fr := wire.NewReader(conn)
	var seen bytes.Buffer
	for {
		f, err := fr.Next()
		if err != nil {
			t.Fatalf("终端输出里没等到标记：%v；已收：%q", err, seen.String())
		}
		if f.Type == wire.Data {
			seen.Write(f.Payload)
		}
		// 回显一次 + 输出一次：出现两次才说明命令真的跑了。
		if strings.Count(seen.String(), marker) >= 2 {
			break
		}
	}
	status, body := e.do("GET", "/v1/tmux/sessions", nil)
	if status != 200 || body["available"] != true {
		t.Fatalf("sessions = %d %v", status, body)
	}
	found := false
	for _, raw := range body["sessions"].([]any) {
		if raw.(map[string]any)["name"] == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("会话清单里没有 %s：%v", name, body)
	}
	// attach 顺手 source 了内嵌的 tmux.conf：文件落在 StateDir，鼠标模式与拖选绑定已生效。
	if raw, err := os.ReadFile(filepath.Join(e.srv.stateDir, tmuxConfName)); err != nil || !bytes.Equal(raw, tmuxConf) {
		t.Fatalf("StateDir 里的 tmux.conf 不对：%v", err)
	}
	for _, c := range [][]string{{"show", "-gv", "mouse"}, {"show", "-sv", "set-clipboard"}, {"show", "-gwv", "mode-keys"}} {
		cmd := exec.Command("tmux", c...)
		cmd.Env = e.srv.childEnv()
		out, err := cmd.Output()
		want := map[string]string{"mouse": "on", "set-clipboard": "on", "mode-keys": "vi"}[c[2]]
		if got := strings.TrimSpace(string(out)); err != nil || got != want {
			t.Fatalf("tmux %s = %q %v，想要 %q", c[2], got, err, want)
		}
	}
	keys := exec.Command("tmux", "list-keys", "-T", "copy-mode-vi", "MouseDragEnd1Pane")
	keys.Env = e.srv.childEnv()
	if out, err := keys.Output(); err != nil || !strings.Contains(string(out), "copy-selection-no-clear") {
		t.Fatalf("拖选绑定 = %q %v", out, err)
	}
	root := exec.Command("tmux", "list-keys", "-T", "root")
	root.Env = e.srv.childEnv()
	if out, _ := root.Output(); strings.Contains(string(out), "MouseDown3") {
		t.Fatalf("右键菜单仍绑定：%s", out)
	}
	conn.Close()
	if status, body = e.do("DELETE", "/v1/tmux/sessions/"+name, nil); status != 200 {
		t.Fatalf("kill = %d %v", status, body)
	}
	if status, body = e.do("DELETE", "/v1/tmux/sessions/"+name, nil); status != 404 {
		t.Fatalf("重复 kill = %d %v", status, body)
	}
	if status, _ = e.do("POST", "/v1/tmux/sessions", map[string]string{"name": "bad name"}); status != 400 {
		t.Fatalf("非法会话名 = %d", status)
	}
}

func TestInstallLayout(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Join(t.TempDir(), "etc")
	res, err := Install(InstallOptions{ConfigDir: dir, BinPath: "/opt/llmgate-devd", User: u.Username})
	if err != nil {
		t.Fatal(err)
	}
	if res.Socket != DefaultSocket {
		t.Fatalf("res = %+v", res)
	}
	lay := LayoutIn(dir)
	for _, p := range []string{lay.Config, res.UnitPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("缺文件 %s: %v", p, err)
		}
	}
	unit, _ := os.ReadFile(res.UnitPath)
	for _, want := range []string{"User=" + u.Username, "ExecStart=/opt/llmgate-devd serve --config-dir " + dir, "RuntimeDirectory=llmgate-devd", "RuntimeDirectoryMode=0700"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("unit 缺 %q：%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "network") {
		t.Fatalf("守护进程不依赖网络：%s", unit)
	}
	cfg, err := ReadConfig(lay.Config)
	if err != nil || cfg.Socket != DefaultSocket || cfg.User != u.Username {
		t.Fatalf("config = %+v %v", cfg, err)
	}
	// 重装换 socket 路径：幂等。
	res2, err := Install(InstallOptions{ConfigDir: dir, User: u.Username, Socket: "/tmp/x.sock"})
	if err != nil || res2.Socket != "/tmp/x.sock" {
		t.Fatalf("重装 = %+v %v", res2, err)
	}
	if cfg, _ = ReadConfig(lay.Config); cfg.Socket != "/tmp/x.sock" {
		t.Fatalf("重装后 config = %+v", cfg)
	}
	if _, err := Install(InstallOptions{ConfigDir: dir, User: "no-such-user-llmgate"}); err == nil {
		t.Fatal("不存在的用户应拒绝安装")
	}
	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("卸载后目录应删除")
	}
	// 没有任何证书或密钥文件落下。
	if _, err := os.Stat(filepath.Join(dir, "trusted.pem")); !os.IsNotExist(err) {
		t.Fatal("不该再有 trusted.pem")
	}
}

// 新终端直接启动开发工具：工具名封闭；主机没有 gate → gate_missing、gate 没配 Key →
// gate_unconfigured，都不建会话；齐全时会话建好并把 `gate <tool>` 敲进去（这里的假 gate
// 只把参数写到文件里，证明命令真的在会话里跑了）。
func TestTmuxNewWithTool(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("本机没有 tmux")
	}
	e := newEnv(t)
	name := fmt.Sprintf("llmgate-tool-%d", os.Getpid())
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(e.home, ".config"))

	listed := func() bool {
		_, body := e.do("GET", "/v1/tmux/sessions", nil)
		for _, raw := range body["sessions"].([]any) {
			if raw.(map[string]any)["name"] == name {
				return true
			}
		}
		return false
	}
	code := func(body map[string]any) string {
		return body["error"].(map[string]any)["code"].(string)
	}

	if status, _ := e.do("POST", "/v1/tmux/sessions", map[string]string{"name": name, "tool": "vim"}); status != 400 {
		t.Fatalf("不认识的工具 = %d", status)
	}
	status, body := e.do("POST", "/v1/tmux/sessions", map[string]string{"name": name, "tool": "codex"})
	if status != 409 || code(body) != CodeGateMissing {
		t.Fatalf("没有 gate = %d %v", status, body)
	}
	if listed() {
		t.Fatal("没有 gate 时不该建会话")
	}
	// 假 gate：把收到的参数写进家目录的 gate-args，然后像真 gate 一样留在前台等「工具」
	// （这里是 sleep）——会话清单要靠这棵进程树认出正在跑的工具。
	bin := filepath.Join(e.home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(e.home, "gate-args")
	if err := os.WriteFile(filepath.Join(bin, "gate"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > '"+argsFile+"'\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, body = e.do("POST", "/v1/tmux/sessions", map[string]string{"name": name, "tool": "codex"})
	if status != 409 || code(body) != CodeGateUnconfigured {
		t.Fatalf("gate 没配 Key = %d %v", status, body)
	}
	cfgDir := filepath.Join(e.home, ".config", "gate")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"base_url":"http://192.168.1.10","api_key":"sk_example_only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 会话里的登录 shell 要找得到假 gate：把 ~/.local/bin 放进守护进程的 PATH（真机上由
	// 安装脚本写进 shell 启动文件）。
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	status, body = e.do("POST", "/v1/tmux/sessions", map[string]string{"name": name, "dir": e.home, "tool": "codex"})
	if status != 200 {
		t.Fatalf("带工具建会话 = %d %v", status, body)
	}
	if !listed() {
		t.Fatal("会话清单里没有新会话")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if raw, err := os.ReadFile(argsFile); err == nil {
			if got := strings.TrimSpace(string(raw)); got != "codex" {
				t.Fatalf("gate 收到的参数 = %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("会话里没等到 gate codex 跑起来")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 清单认出会话里在跑 codex：从 pane 的 shell 沿子进程树找到 `sh …/gate codex`。
	for {
		_, body := e.do("GET", "/v1/tmux/sessions", nil)
		tool := ""
		for _, raw := range body["sessions"].([]any) {
			if m := raw.(map[string]any); m["name"] == name {
				tool, _ = m["tool"].(string)
			}
		}
		if tool == "codex" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("会话清单没标出正在跑的工具：tool=%q", tool)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, _ := e.do("DELETE", "/v1/tmux/sessions/"+name, nil); status != 200 {
		t.Fatalf("kill = %d", status)
	}
}

// 从 argv 认工具：`gate <tool>` 优先且不认表外的工具名；可执行名或解释器后的脚本名命中
// 表里的也认；子进程树里 gate 的读数盖过直接运行的工具名。
func TestDevToolDetect(t *testing.T) {
	cases := []struct {
		argv    []string
		tool    string
		viaGate bool
	}{
		{[]string{"gate", "codex"}, "codex", true},
		{[]string{"/home/dev/.local/bin/gate", "cursor", "--foo"}, "cursor", true},
		{[]string{"/bin/sh", "/home/dev/.local/bin/gate", "grok"}, "grok", true},
		{[]string{"gate", "vim"}, "", false},
		{[]string{"gate"}, "", false},
		{[]string{"/usr/local/bin/claude", "--settings", "x"}, "claude", false},
		{[]string{"node", "/usr/local/bin/claude"}, "claude", false},
		{[]string{"/home/dev/.local/bin/cursor-agent"}, "cursor", false},
		{[]string{"opencode"}, "opencode", false},
		{[]string{"bash", "-l"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		tool, viaGate := devToolOf(c.argv)
		if tool != c.tool || viaGate != c.viaGate {
			t.Errorf("devToolOf(%q) = %q %v，想要 %q %v", c.argv, tool, viaGate, c.tool, c.viaGate)
		}
	}
	tbl := &procTable{procs: map[int]procInfo{
		10: {ppid: 1, argv: []string{"bash", "-l"}},
		11: {ppid: 10, argv: []string{"gate", "claude"}},
		12: {ppid: 11, argv: []string{"/opt/claude/claude"}},
		20: {ppid: 1, argv: []string{"zsh"}},
		21: {ppid: 20, argv: []string{"vim"}},
		30: {ppid: 1, argv: []string{"bash"}},
		31: {ppid: 30, argv: []string{"opencode"}},
	}, children: map[int][]int{1: {10, 20, 30}, 10: {11}, 11: {12}, 20: {21}, 30: {31}}}
	for pid, want := range map[int]string{10: "claude", 20: "", 30: "opencode", 99: ""} {
		if got := tbl.toolOf(pid); got != want {
			t.Errorf("toolOf(%d) = %q，想要 %q", pid, got, want)
		}
	}
	if got := (*procTable)(nil).toolOf(10); got != "" {
		t.Errorf("nil 进程表 = %q", got)
	}
	// 真 /proc：本测试进程自己一定在表里，且父进程对得上。
	if runtime.GOOS == "linux" {
		snap := snapshotProcs()
		if snap == nil {
			t.Fatal("读不到 /proc")
		}
		if p, ok := snap.procs[os.Getpid()]; !ok || p.ppid != os.Getppid() {
			t.Fatalf("/proc 快照里的本进程 = %+v %v", p, ok)
		}
	}
}

// TestFSRaw：按原字节读写——PUT 流式落盘（临时文件 + rename）、GET 带 Content-Type 与 Range、
// 目录 / 缺失 / 超限的拒绝。
func TestFSRaw(t *testing.T) {
	e := newEnv(t)
	payload := bytes.Repeat([]byte("0123456789"), 1000)
	put := func(path string, body []byte) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/v1/fs/raw?path="+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		raw, _ := io.ReadAll(resp.Body)
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	if err := os.Mkdir(filepath.Join(e.home, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, body := put("media/clip.bin", payload)
	if status != 200 || body["size"] != float64(len(payload)) || body["path"] != filepath.Join(e.home, "media/clip.bin") {
		t.Fatalf("PUT raw = %d %v", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(e.home, "media/clip.bin")); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("落盘内容不符: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.home, "media"))
	if len(entries) != 1 {
		t.Fatalf("目录里留下了临时文件: %d 项", len(entries))
	}
	// 目录不能当文件写；父目录不存在报错。
	if status, _ := put("media", []byte("x")); status != 400 {
		t.Fatalf("PUT 到目录 = %d", status)
	}
	if status, _ := put("nope/x.bin", []byte("x")); status/100 == 2 {
		t.Fatalf("父目录不存在应失败，得到 %d", status)
	}
	// GET：整份、Range、缺失。
	resp, err := e.client.Get(e.ts.URL + "/v1/fs/raw?path=media/clip.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, payload) || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("GET raw = %d，%d 字节，Accept-Ranges=%q", resp.StatusCode, len(got), resp.Header.Get("Accept-Ranges"))
	}
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/v1/fs/raw?path=media/clip.bin", nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(got) != "0123456789" || resp.Header.Get("Content-Range") != "bytes 10-19/10000" {
		t.Fatalf("Range = %d %q %q", resp.StatusCode, got, resp.Header.Get("Content-Range"))
	}
	if status, body := e.do("GET", "/v1/fs/raw?path=media/missing.bin", nil); status != 404 || body["error"] == nil {
		t.Fatalf("缺失文件 = %d %v", status, body)
	}
	if status, _ := e.do("GET", "/v1/fs/raw?path=media", nil); status != 400 {
		t.Fatalf("读目录 = %d", status)
	}
	// 按扩展名给 Content-Type。
	put("media/a.png", []byte("not really png"))
	resp, _ = e.client.Get(e.ts.URL + "/v1/fs/raw?path=media/a.png")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
}
