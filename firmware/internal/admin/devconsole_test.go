package admin_test

// 主机上的守护进程 devd端点验收：路由暴露档位、没接管理器时的 503，以及一条从纳管主机
// 到安装 / 检查 / devd 透传 / 轮换证书 / 卸载 / 解除纳管的完整路径（假主机在进程内跑真
// SSH 与真守护进程，不出网），外加终端 WebSocket 对接。

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/devd/wire"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/wsock"
)

const devPassword = "n0t-a-real-dev-password"

type devdBody struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Home      string `json:"home"`
	Tmux      bool   `json:"tmux"`
	LastError string `json:"last_error"`
}

type devdHostBody struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	SudoNoPasswd bool      `json:"sudo_nopasswd"`
	Devd         *devdBody `json:"devd"`
}

type devdHostsBody struct {
	Hosts         []devdHostBody `json:"hosts"`
	DaemonVersion string         `json:"daemon_version"`
}

// withDevHosts 给这套环境接上纳管管理器与守护进程管理器，两者都指向同一台假主机。
func withDevHosts(t *testing.T, e *env, opt devhosttest.Options) *devhosttest.Host {
	t.Helper()
	if opt.Password == "" {
		opt.Password = devPassword
	}
	host := devhosttest.New(t, opt)
	hosts := agenthost.New(agenthost.Options{Store: e.st, Dial: host.SSH.Dial})
	e.srv.SetAgentHosts(hosts)
	e.srv.SetDevHosts(devhost.New(devhost.Options{Store: e.st, Hosts: hosts, Binaries: devhosttest.FakeBinaries{}}))
	return host
}

// enrollDevHost 生成访问证书并按 kind 纳管假主机，回主机 id。
func enrollDevHost(t *testing.T, e *env, root string, host *devhosttest.Host, kind, username string, sudo bool) string {
	t.Helper()
	response := e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	addr, port := host.Addr()
	body := `{"name":"工作站","kind":"` + kind + `","address":"` + addr + `","port":` + strconv.Itoa(port) + `,"username":"` + username + `","password":"` + devPassword + `","configure_sudo":` + strconv.FormatBool(sudo) + `}`
	response = e.do("POST", "/admin/v1/agent-hosts", root, body)
	wantStatus(t, response, http.StatusOK)
	var one struct {
		Host devdHostBody `json:"host"`
	}
	decodeInto(t, response, &one)
	if one.Host.Status != "ready" || one.Host.Devd != nil {
		t.Fatalf("纳管后 = %+v", one.Host)
	}
	return strconv.FormatInt(one.Host.ID, 10)
}

func TestDevdUnavailableWithoutManager(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	response := e.do("POST", "/admin/v1/agent-hosts/1/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusServiceUnavailable)
	if code := errCode(t, response); code != "dev_hosts_unavailable" {
		t.Fatalf("错误码 = %q", code)
	}
	// 读数照常，只是没有守护进程那部分。
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	var snapshot devdHostsBody
	decodeInto(t, response, &snapshot)
	if snapshot.DaemonVersion != "" {
		t.Fatalf("没接管理器的读数 = %+v", snapshot)
	}
}

func TestDevdExposure(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 不暴露路由表")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for _, pattern := range []string{
		"POST /admin/v1/agent-hosts/{id}/devd/install", "POST /admin/v1/agent-hosts/{id}/devd/check",
		"DELETE /admin/v1/agent-hosts/{id}/devd",
		"GET /admin/v1/agent-hosts/{id}/terminal",
		"GET /admin/v1/agent-hosts/{id}/console/{path...}", "POST /admin/v1/agent-hosts/{id}/console/{path...}",
		"PUT /admin/v1/agent-hosts/{id}/console/{path...}", "DELETE /admin/v1/agent-hosts/{id}/console/{path...}",
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != tunnelctx.LANOnly {
			t.Fatalf("%s 应是 LANOnly：%v", pattern, tier)
		}
	}
	for _, gone := range []string{"GET /admin/v1/dev-hosts", "POST /admin/v1/dev-hosts", "GET /admin/v1/dev-daemon/{platform}",
		"POST /admin/v1/agent-hosts/dev-certificate/rotate", "POST /admin/v1/agent-hosts/{id}/devd/push"} {
		if _, found := tiers[gone]; found {
			t.Fatalf("旧路由 %s 不该再注册", gone)
		}
	}
}

func TestDevdLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev"})
	id := enrollDevHost(t, e, root, host, "worker", "dev", true)

	var snapshot devdHostsBody
	response := e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &snapshot)
	if snapshot.DaemonVersion != "test" || len(snapshot.Hosts) != 1 || snapshot.Hosts[0].Devd != nil || !snapshot.Hosts[0].SudoNoPasswd {
		t.Fatalf("安装前读数 = %+v", snapshot)
	}
	// 没装就检查 / 透传 / 卸载：409 devd_not_installed。
	for _, call := range [][2]string{{"POST", "/devd/check"}, {"GET", "/console/fs/list"}, {"DELETE", "/devd"}} {
		response = e.do(call[0], "/admin/v1/agent-hosts/"+id+call[1], root, "")
		wantStatus(t, response, http.StatusConflict)
		if code := errCode(t, response); code != devhost.CodeNotInstalled {
			t.Fatalf("%s %s 错误码 = %q", call[0], call[1], code)
		}
	}
	// 安装（免密 sudo，不要口令）。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var one struct {
		Host devdHostBody `json:"host"`
	}
	decodeInto(t, response, &one)
	d := one.Host.Devd
	if d == nil || d.Status != "ready" || d.Version != "fake" || d.Home == "" {
		t.Fatalf("安装后 = %+v", one.Host)
	}
	// 重装幂等、检查、devd 透传。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/check", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/console/fs/list", root, "")
	wantStatus(t, response, http.StatusOK)
	var listing struct {
		Path    string `json:"path"`
		Entries []any  `json:"entries"`
	}
	decodeInto(t, response, &listing)
	if listing.Path != one.Host.Devd.Home {
		t.Fatalf("listing = %+v", listing)
	}
	response = e.do("PUT", "/admin/v1/agent-hosts/"+id+"/console/fs/write", root, `{"path":"hello.txt","content":"hi\n"}`)
	wantStatus(t, response, http.StatusOK)
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/console/fs/read?path=hello.txt", root, "")
	wantStatus(t, response, http.StatusOK)
	var file struct {
		Content string `json:"content"`
	}
	decodeInto(t, response, &file)
	if file.Content != "hi\n" {
		t.Fatalf("content = %q", file.Content)
	}
	// 守护进程的错误体原样带回（状态与 code 保持）。
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/console/fs/read?path=missing", root, "")
	wantStatus(t, response, http.StatusNotFound)
	if code := errCode(t, response); code != "not_found" {
		t.Fatalf("透传错误码 = %q", code)
	}
	// 守护进程掉线：检查 502、行记原因、devd 不可用。
	host.MarkUninstalled()
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/check", root, `{}`)
	wantStatus(t, response, http.StatusBadGateway)
	if code := errCode(t, response); code != devhost.CodeUnreachable {
		t.Fatalf("掉线错误码 = %q", code)
	}
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	decodeInto(t, response, &snapshot)
	if snapshot.Hosts[0].Devd == nil || snapshot.Hosts[0].Devd.Status != "error" || snapshot.Hosts[0].Devd.LastError == "" {
		t.Fatalf("掉线后读数 = %+v", snapshot.Hosts[0])
	}
	host.MarkInstalled()

	// 卸载：主机上收走、行里没了守护进程、主机本身还在。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/devd", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var after struct {
		Host devdHostBody `json:"host"`
	}
	decodeInto(t, response, &after)
	if after.Host.Devd != nil || after.Host.Status != "ready" || host.Installed() {
		t.Fatalf("卸载后 = %+v installed=%v", after.Host, host.Installed())
	}
	// 再装上，然后整台解除纳管：守护进程与公钥一起收走。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id, root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var removed struct {
		KeyRemoved  bool `json:"key_removed"`
		DevdRemoved bool `json:"devd_removed"`
	}
	decodeInto(t, response, &removed)
	if !removed.KeyRemoved || !removed.DevdRemoved || host.Installed() {
		t.Fatalf("解除纳管 = %+v installed=%v", removed, host.Installed())
	}
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	decodeInto(t, response, &snapshot)
	if len(snapshot.Hosts) != 0 {
		t.Fatalf("删除后仍有主机 = %+v", snapshot.Hosts)
	}

	// §15.1：口令与私钥不进日志。
	if strings.Contains(e.buf.String(), devPassword) {
		t.Fatal("主机口令写进了日志")
	}
}

func TestDevdInstallNeedsSudoPassword(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev"})
	id := enrollDevHost(t, e, root, host, "worker", "dev", false)
	// 没免密 sudo 又没口令：sudo_failed，行不动。
	response := e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusBadGateway)
	if code := errCode(t, response); code != devhost.CodeSudoFailed {
		t.Fatalf("错误码 = %q", code)
	}
	var snapshot devdHostsBody
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	decodeInto(t, response, &snapshot)
	if snapshot.Hosts[0].Devd != nil {
		t.Fatalf("预检失败不该留下守护进程状态 = %+v", snapshot.Hosts[0])
	}
	// 带口令：经 sudo -S 装上。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{"password":"`+devPassword+`"}`)
	wantStatus(t, response, http.StatusOK)
	var one struct {
		Host devdHostBody `json:"host"`
	}
	decodeInto(t, response, &one)
	if one.Host.Devd == nil || one.Host.Devd.Status != "ready" {
		t.Fatalf("带口令安装 = %+v", one.Host)
	}
	// 卸载同样要口令；解除纳管时不给口令就只删本地、守护进程留在主机上。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/devd", root, `{}`)
	wantStatus(t, response, http.StatusBadGateway)
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id, root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var removed struct {
		KeyRemoved  bool `json:"key_removed"`
		DevdRemoved bool `json:"devd_removed"`
	}
	decodeInto(t, response, &removed)
	if !removed.KeyRemoved || removed.DevdRemoved || !host.Installed() {
		t.Fatalf("解除纳管 = %+v installed=%v", removed, host.Installed())
	}
	if strings.Contains(e.buf.String(), devPassword) {
		t.Fatal("主机口令写进了日志")
	}
}

// 受控纳管的主机：安装 devd 答 409 host_kind_not_allowed，不碰主机；重新纳管沿用类型、
// 请求体里的 kind 不能改它。
func TestDevdInstallRefusedForManagedHost(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev"})
	id := enrollDevHost(t, e, root, host, "managed", "dev", true)
	response := e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != devhost.CodeKindNotAllowed {
		t.Fatalf("错误码 = %q", code)
	}
	if host.Installed() {
		t.Fatal("主机上不该装上守护进程")
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/enroll", root, `{"kind":"worker","password":"`+devPassword+`","configure_sudo":true}`)
	wantStatus(t, response, http.StatusOK)
	var snapshot struct {
		Hosts []struct {
			Kind string    `json:"kind"`
			Devd *devdBody `json:"devd"`
		} `json:"hosts"`
	}
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	decodeInto(t, response, &snapshot)
	if len(snapshot.Hosts) != 1 || snapshot.Hosts[0].Kind != "managed" || snapshot.Hosts[0].Devd != nil {
		t.Fatalf("重新纳管后 = %+v", snapshot.Hosts)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusConflict)
}

func TestDevdTerminal(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("本机没有 tmux")
	}
	// tmux 服务器隔离到临时目录：attach 会 source 守护进程内嵌的 tmux.conf。
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev"})
	id := enrollDevHost(t, e, root, host, "worker", "dev", true)
	response := e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)

	ts := httptest.NewServer(e.h)
	defer ts.Close()
	name := "llmgate-admin-test-" + id
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	hdr := http.Header{"Cookie": {admin.SessionCookieName + "=" + root}}
	c, _, err := wsock.Dial("ws://"+strings.TrimPrefix(ts.URL, "http://")+"/admin/v1/agent-hosts/"+id+"/terminal?name="+name+"&cols=80&rows=24", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteMessage(wsock.OpBinary, wire.Encode(wire.Data, []byte("echo llmgate-ws-ok\n"))); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	for i := 0; i < 200; i++ {
		op, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("读终端帧：%v；已收 %q", err, seen.String())
		}
		if op != wsock.OpBinary {
			continue
		}
		f, err := wire.Decode(msg)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == wire.Exit {
			t.Fatalf("终端提前结束：%s；已收 %q", f.Payload, seen.String())
		}
		seen.Write(f.Payload)
		if strings.Count(seen.String(), "llmgate-ws-ok") >= 2 {
			break
		}
	}
	if strings.Count(seen.String(), "llmgate-ws-ok") < 2 {
		t.Fatalf("终端没有回显：%q", seen.String())
	}
	// 没登录的 WebSocket 被 401 挡在握手前。
	if _, resp, err := wsock.Dial("ws://"+strings.TrimPrefix(ts.URL, "http://")+"/admin/v1/agent-hosts/"+id+"/terminal", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录终端 = %v %v", resp, err)
	}
}
