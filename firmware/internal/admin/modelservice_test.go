package admin_test

// 模型服务节点端点验收：暴露档位、类型闸、一条从纳管 → 安装 modeld → 透传（算力服务器 / 令牌）→
// 起引擎会话（密钥解封只经 SSH 交给守护进程、不回响应）的路径。假主机在进程内跑真 SSH 与真 modeld。

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return string(raw)
}

func TestModelServiceExposure(t *testing.T) {
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
		"GET /admin/v1/agent-hosts/{id}/model",
		"POST /admin/v1/agent-hosts/{id}/model/engines/sessions",
		"GET /admin/v1/agent-hosts/{id}/model/{path...}", "POST /admin/v1/agent-hosts/{id}/model/{path...}",
		"PUT /admin/v1/agent-hosts/{id}/model/{path...}", "DELETE /admin/v1/agent-hosts/{id}/model/{path...}",
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != tunnelctx.LANOnly {
			t.Fatalf("%s 应是 LANOnly：%v", pattern, tier)
		}
	}
}

func TestModelServiceLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "root", Arch: "x86_64"})
	id := enrollDevHost(t, e, root, host, "model_service", "root", false)

	// 没装之前：透传答没装。
	response := e.do("GET", "/admin/v1/agent-hosts/"+id+"/model", root, "")
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "devd_not_installed" {
		t.Fatalf("未装错误码 = %q", code)
	}

	// 主机清单带两种守护进程的内嵌版本与类型。
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	if body := bodyOf(t, response); !strings.Contains(body, `"model_daemon_version":"test"`) || !strings.Contains(body, `"kind":"model_service"`) {
		t.Fatalf("清单 = %s", body)
	}

	// 安装（沿用 /devd/install，按类型装 modeld）。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var one struct {
		Host devdHostBody `json:"host"`
	}
	decodeInto(t, response, &one)
	if one.Host.Devd == nil || one.Host.Devd.Status != "ready" {
		t.Fatalf("安装后 = %+v", one.Host)
	}
	if !host.Installed() {
		t.Fatal("主机上应装着守护进程")
	}

	// 一屏读数与透传。
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/model", root, "")
	wantStatus(t, response, http.StatusOK)
	if body := bodyOf(t, response); !strings.Contains(body, `"daemon":"modeld"`) {
		t.Fatalf("summary = %s", body)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/backends", root, `{"name":"gpu-1","base_url":"http://127.0.0.1:1","models":["h3-video"],"slots":2}`)
	wantStatus(t, response, http.StatusCreated)
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/tokens", root, `{"name":"client"}`)
	wantStatus(t, response, http.StatusCreated)
	if body := bodyOf(t, response); !strings.Contains(body, `"plaintext":"msk_`) {
		t.Fatalf("签发令牌 = %s", body)
	}
	// 守护进程自己答出来的错原样带回。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/tasks", root, `{"model":"nope"}`)
	wantStatus(t, response, http.StatusServiceUnavailable)
	if code := errCode(t, response); code != "no_backend" {
		t.Fatalf("no_backend 错误码 = %q", code)
	}

	// 起引擎会话：入参校验先于解封密钥。
	for _, body := range []string{
		`{"engine":"x","key_id":1,"base_url":"http://10.0.0.1"}`,
		`{"engine":"codex","key_id":1,"base_url":"http://10.0.0.1/path"}`,
		`{"engine":"codex","base_url":"http://10.0.0.1"}`,
	} {
		response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/engines/sessions", root, body)
		wantStatus(t, response, http.StatusBadRequest)
	}
	// 一把有明文的密钥 + 假 codex：会话起来，明文不回响应、不进任何 SSH 命令行。
	key, plaintext := e.createKey(root, "模型服务")
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(fakeCodexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/engines/sessions", root,
		`{"engine":"codex","key_id":`+strconv.FormatInt(key.ID, 10)+`,"base_url":"http://10.0.0.1/","binary":"`+bin+`","workdir":"`+t.TempDir()+`"}`)
	wantStatus(t, response, http.StatusCreated)
	body := bodyOf(t, response)
	if strings.Contains(body, plaintext) || !strings.Contains(body, `"status":"idle"`) {
		t.Fatalf("起会话响应 = %s", body)
	}
	for _, ex := range host.Execs() {
		if strings.Contains(ex.Command, plaintext) || strings.Contains(ex.Stdin, plaintext) {
			t.Fatalf("密钥进了 SSH 命令：%q", ex.Command)
		}
	}
	// 密钥确实到了守护进程：实例目录的 config.toml 里有它（0600）。
	entries, _ := os.ReadDir(filepath.Join(host.Modeld().StateDir(), "engine"))
	if len(entries) != 1 {
		t.Fatalf("实例目录 = %v", entries)
	}
	cfg, err := os.ReadFile(filepath.Join(host.Modeld().StateDir(), "engine", entries[0].Name(), "config.toml"))
	if err != nil || !strings.Contains(string(cfg), plaintext) || !strings.Contains(string(cfg), "http://10.0.0.1/agents/codex/v1") {
		t.Fatalf("config.toml = %s %v", cfg, err)
	}
	// 关会话。
	sid := body[strings.Index(body, `"id":"`)+6:]
	sid = sid[:strings.Index(sid, `"`)]
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/model/engines/sessions/"+sid, root, "")
	wantStatus(t, response, http.StatusOK)

	// 审计只记方法与路径、密钥标签，不记请求体与明文。
	response = e.do("GET", "/admin/v1/audit?limit=50", root, "")
	if response.StatusCode == http.StatusOK {
		audit := bodyOf(t, response)
		if strings.Contains(audit, plaintext) || strings.Contains(audit, "gpu-1") {
			t.Fatalf("审计泄露了请求体或明文：%s", audit)
		}
	}
}

func TestModelServiceRefusedForWorker(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "root", Arch: "x86_64"})
	id := enrollDevHost(t, e, root, host, "worker", "root", false)
	response := e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/model", root, "")
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "host_kind_not_allowed" {
		t.Fatalf("工作节点错误码 = %q", code)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/model/engines/sessions", root, `{"engine":"codex","key_id":1,"base_url":"http://10.0.0.1"}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "host_kind_not_allowed" {
		t.Fatalf("工作节点起会话错误码 = %q", code)
	}
}

// fakeCodexScript 是最小的假 codex app-server（sh）：握手、开线程即可，指令不在本测试范围。
const fakeCodexScript = `#!/bin/sh
[ "$1" = "app-server" ] || exit 9
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{"userAgent":"fake"}}\n' "$id" ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"th1"}}}\n' "$id" ;;
  esac
done
`
