package admin_test

// 「智能体 → 工作空间」端点验收：路由暴露档位、读数形状、先校验后落地（仓库 / 分支 / 凭证
// 不通就不建）、凭证明文只经设备解封交给假主机（不回响应、不进日志）、删除可选连同目录、
// 主机解除纳管级联。

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

type workspaceDTO struct {
	ID              string `json:"id"`
	HostID          int64  `json:"host_id"`
	HostName        string `json:"host_name"`
	HostReady       bool   `json:"host_ready"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	RepoURL         string `json:"repo_url"`
	Branch          string `json:"branch"`
	CredentialID    string `json:"credential_id"`
	CredentialLabel string `json:"credential_label"`
	Template        string `json:"template"`
	TemplateName    string `json:"template_name"`
	CreatedAt       string `json:"created_at"`
}

type workspacesBody struct {
	Workspaces []workspaceDTO `json:"workspaces"`
	Hosts      []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		DevdReady bool   `json:"devd_ready"`
	} `json:"hosts"`
	Credentials []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"credentials"`
}

func TestWorkspacesExposure(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 不暴露路由表")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for pattern, want := range map[string]tunnelctx.Exposure{
		"GET /admin/v1/workspaces":         tunnelctx.Admin,
		"POST /admin/v1/workspaces":        tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}":    tunnelctx.Admin,
		"DELETE /admin/v1/workspaces/{id}": tunnelctx.LANOnly,
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != want {
			t.Fatalf("%s 档位 = %v，期望 %v", pattern, tier, want)
		}
	}
	for _, ep := range []struct{ method, path string }{
		{"GET", "/admin/v1/workspaces"}, {"POST", "/admin/v1/workspaces"},
		{"GET", "/admin/v1/workspaces/x"}, {"DELETE", "/admin/v1/workspaces/x"},
	} {
		wantStatus(t, e.do(ep.method, ep.path, "", `{}`), http.StatusUnauthorized)
	}
}

func TestWorkspacesLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev", Git: "git version 2.43.0", Repos: map[string]devhosttest.FakeRepo{
		"https://github.com/octocat/hello.git": {Branches: []string{"main"}},
		"https://github.com/acme/private.git":  {Branches: []string{"main", "dev"}, User: "octocat", Token: testCredentialToken},
	}})
	id := enrollDevHost(t, e, root, host, "worker", "dev", true)
	hostID, _ := strconv.ParseInt(id, 10, 64)

	// 空读数：工作节点在可选列表里（没装 devd → devd_ready=false），凭证列表空。
	var list workspacesBody
	response := e.do("GET", "/admin/v1/workspaces", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &list)
	if len(list.Workspaces) != 0 || len(list.Hosts) != 1 || list.Hosts[0].ID != hostID || list.Hosts[0].DevdReady || len(list.Credentials) != 0 {
		t.Fatalf("初始读数 = %+v", list)
	}

	// 一份 git 凭证。
	var one struct {
		Credential credentialDTO `json:"credential"`
	}
	response = e.do("POST", "/admin/v1/credentials", root,
		`{"kind":"git","name":"工作账号","host":"github.com","username":"octocat","secret":"`+testCredentialToken+`"}`)
	wantStatus(t, response, http.StatusCreated)
	decodeInto(t, response, &one)
	credID := one.Credential.ID

	// 入参校验。
	for name, body := range map[string]string{
		"缺名称":    `{"host_id":` + id + `}`,
		"名称非法":   `{"host_id":` + id + `,"name":"a b"}`,
		"地址非法":   `{"host_id":` + id + `,"name":"x","repo_url":"ftp://x/y"}`,
		"没仓库有分支": `{"host_id":` + id + `,"name":"x","branch":"main"}`,
		"没仓库有凭证": `{"host_id":` + id + `,"name":"x","credential_id":"` + credID + `"}`,
	} {
		response = e.do("POST", "/admin/v1/workspaces", root, body)
		wantStatus(t, response, http.StatusBadRequest)
		if code := errCode(t, response); code != "invalid_workspace" {
			t.Fatalf("%s：错误码 = %q", name, code)
		}
	}
	// 主机不存在 / 凭证不存在。
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":999,"name":"x"}`)
	wantStatus(t, response, http.StatusNotFound)
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"x","repo_url":"https://github.com/octocat/hello.git","credential_id":"nope"}`)
	wantStatus(t, response, http.StatusNotFound)
	if code := errCode(t, response); code != "credential_not_found" {
		t.Fatalf("错误码 = %q", code)
	}

	// 先校验后落地：分支不存在 → 400，主机上没建目录，设备上没落行。
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"hello","repo_url":"https://github.com/octocat/hello.git","branch":"nope"}`)
	wantStatus(t, response, http.StatusBadRequest)
	if code := errCode(t, response); code != "branch_not_found" {
		t.Fatalf("错误码 = %q", code)
	}
	// 私有仓库没带凭证 → 400 repo_auth_failed。
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"private","repo_url":"https://github.com/acme/private.git"}`)
	wantStatus(t, response, http.StatusBadRequest)
	if code := errCode(t, response); code != "repo_auth_failed" {
		t.Fatalf("错误码 = %q", code)
	}
	if len(host.Workspaces()) != 0 {
		t.Fatalf("失败的创建留下了目录：%+v", host.Workspaces())
	}
	response = e.do("GET", "/admin/v1/workspaces", root, "")
	decodeInto(t, response, &list)
	if len(list.Workspaces) != 0 {
		t.Fatalf("失败的创建落了行：%+v", list.Workspaces)
	}

	// 带凭证克隆私有仓库：201，凭证明文到了主机、不在响应与日志里。
	var created struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"private","repo_url":"https://github.com/acme/private.git","branch":"dev","credential_id":"`+credID+`"}`)
	wantStatus(t, response, http.StatusCreated)
	decodeInto(t, response, &created)
	ws := created.Workspace
	if len(ws.ID) != 26 || ws.HostID != hostID || ws.HostName != "工作站" || ws.HostReady || ws.Name != "private" ||
		!strings.HasSuffix(ws.Path, "/workspaces/private") || ws.RepoURL != "https://github.com/acme/private.git" || ws.Branch != "dev" ||
		ws.CredentialID != credID || ws.CredentialLabel != "工作账号（octocat@github.com）" || ws.CreatedAt == "" {
		t.Fatalf("新行 = %+v", ws)
	}
	if got := host.Workspaces()["private"]; got.User != "octocat" || got.Token != testCredentialToken || got.Branch != "dev" {
		t.Fatalf("主机上的目录 = %+v", got)
	}
	if strings.Contains(e.buf.String(), testCredentialToken) {
		t.Fatal("令牌写进了日志")
	}
	// 同名 → 409（不连主机）。
	before := len(host.Execs())
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"private"}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "workspace_exists" || len(host.Execs()) != before {
		t.Fatalf("同名：错误码 = %q，连了主机 %v", code, len(host.Execs()) != before)
	}
	// 空目录空间（缺席的字段不会被 JSON 解码重置：用新变量接）。
	var created2 struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"scratch"}`)
	wantStatus(t, response, http.StatusCreated)
	decodeInto(t, response, &created2)
	scratch := created2.Workspace
	if scratch.RepoURL != "" || scratch.CredentialID != "" {
		t.Fatalf("空目录空间 = %+v", scratch)
	}

	// 单个读数与列表。
	response = e.do("GET", "/admin/v1/workspaces/"+ws.ID, root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &created)
	if created.Workspace.ID != ws.ID || created.Workspace.CredentialLabel == "" {
		t.Fatalf("单个读数 = %+v", created.Workspace)
	}
	response = e.do("GET", "/admin/v1/workspaces", root, "")
	decodeInto(t, response, &list)
	if len(list.Workspaces) != 2 || len(list.Credentials) != 1 || list.Credentials[0].ID != credID {
		t.Fatalf("列表 = %+v", list)
	}
	// 凭证删掉：空间还在，凭证字段缺席。
	wantStatus(t, e.do("DELETE", "/admin/v1/credentials/"+credID, root, ""), http.StatusNoContent)
	var after struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	response = e.do("GET", "/admin/v1/workspaces/"+ws.ID, root, "")
	decodeInto(t, response, &after)
	if after.Workspace.CredentialID != "" || after.Workspace.CredentialLabel != "" {
		t.Fatalf("删凭证后 = %+v", after.Workspace)
	}

	// 删除：不删目录 → 主机上目录还在；连同目录 → 收掉。
	wantStatus(t, e.do("DELETE", "/admin/v1/workspaces/"+scratch.ID, root, `{}`), http.StatusNoContent)
	if _, still := host.Workspaces()["scratch"]; !still {
		t.Fatal("不删目录的删除动了主机上的目录")
	}
	wantStatus(t, e.do("DELETE", "/admin/v1/workspaces/"+ws.ID, root, `{"remove_dir":true}`), http.StatusNoContent)
	if _, still := host.Workspaces()["private"]; still {
		t.Fatal("连同目录的删除没删主机上的目录")
	}
	wantStatus(t, e.do("DELETE", "/admin/v1/workspaces/"+ws.ID, root, ""), http.StatusNotFound)

	// 主机解除纳管：空间级联删除。
	response = e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"gone"}`)
	wantStatus(t, response, http.StatusCreated)
	wantStatus(t, e.do("DELETE", "/admin/v1/agent-hosts/"+id, root, `{"remove_key":false}`), http.StatusOK)
	response = e.do("GET", "/admin/v1/workspaces", root, "")
	decodeInto(t, response, &list)
	if len(list.Workspaces) != 0 || len(list.Hosts) != 0 {
		t.Fatalf("主机解除纳管后 = %+v", list)
	}
}

func TestWorkspacesRefusedForManagedHost(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "root", Git: "git version 2.43.0"})
	id := enrollDevHost(t, e, root, host, "managed", "root", false)
	response := e.do("POST", "/admin/v1/workspaces", root, `{"host_id":`+id+`,"name":"x"}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "host_kind_not_allowed" {
		t.Fatalf("错误码 = %q", code)
	}
	// 受控纳管的主机不在可选列表里。
	var list workspacesBody
	response = e.do("GET", "/admin/v1/workspaces", root, "")
	decodeInto(t, response, &list)
	if len(list.Hosts) != 0 {
		t.Fatalf("可选主机 = %+v", list.Hosts)
	}
}
