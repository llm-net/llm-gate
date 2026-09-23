package devhost_test

// 工作空间（workspace.go）验收：先校验后落地（仓库找不到 / 凭证不对 / 分支不存在都不建
// 目录）、凭证只经 stdin 交给脚本、克隆成功记下路径、同名目录拒绝、删除只认自己的路径、
// 受控纳管主机一律被拒。

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const wsToken = "ghp_TESTTOKEN0123456789abcdef"

func TestWorkspaceCreateAndRemove(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", Git: "git version 2.43.0", Repos: map[string]devhosttest.FakeRepo{
		"https://github.com/octocat/hello.git": {Branches: []string{"main", "dev"}},
		"https://github.com/acme/private.git":  {Branches: []string{"main"}, User: "octocat", Token: wsToken},
	}})
	host := f.enroll(t, "dev", false)
	ctx := context.Background()
	cred, err := f.st.CreateCredential(ctx, store.NewCredential{Kind: store.CredentialKindGit, Host: "github.com", Username: "octocat", Secret: wsToken})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := f.st.GetCredentialSecret(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 公开仓库、指定分支：建成。
	res, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "hello", RepoURL: "https://github.com/octocat/hello.git", Branch: "dev"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if !strings.HasSuffix(res.Path, "/workspaces/hello") || res.Host.ID != host.ID {
		t.Fatalf("结果 = %+v", res)
	}
	if ws := f.host.Workspaces()["hello"]; ws.URL != "https://github.com/octocat/hello.git" || ws.Branch != "dev" || ws.User != "" {
		t.Fatalf("主机上的目录 = %+v", ws)
	}
	// 同名再建：主机上已有目录。
	if _, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "hello"}); err == nil || code(t, err) != devhost.CodeWorkspaceDirExists {
		t.Fatalf("同名应答 workspace_dir_exists：%v", err)
	}
	// 分支不存在：不建目录。
	if _, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "nobranch", RepoURL: "https://github.com/octocat/hello.git", Branch: "nope"}); err == nil || code(t, err) != devhost.CodeBranchNotFound {
		t.Fatalf("分支不存在应答 branch_not_found：%v", err)
	}
	// 仓库不存在。
	if _, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "norepo", RepoURL: "https://github.com/octocat/missing.git"}); err == nil || code(t, err) != devhost.CodeRepoNotFound {
		t.Fatalf("仓库不存在应答 repo_not_found：%v", err)
	}
	// 私有仓库没带凭证 / 凭证不对：认证失败。
	if _, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "private", RepoURL: "https://github.com/acme/private.git"}); err == nil || code(t, err) != devhost.CodeRepoAuthFailed {
		t.Fatalf("没带凭证应答 repo_auth_failed：%v", err)
	}
	if got := f.host.Workspaces(); len(got) != 1 {
		t.Fatalf("失败的创建不该留下目录：%+v", got)
	}
	// 带凭证：建成，凭证只在 stdin、不在命令行。
	res, err = f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "private", RepoURL: "https://github.com/acme/private.git", Branch: "main", Credential: &secret})
	if err != nil {
		t.Fatalf("带凭证 CreateWorkspace: %v", err)
	}
	if ws := f.host.Workspaces()["private"]; ws.User != "octocat" || ws.Token != wsToken {
		t.Fatalf("主机收到的凭证 = %+v", ws)
	}
	for _, e := range f.host.Execs() {
		if strings.Contains(e.Command, wsToken) {
			t.Fatal("令牌拼进了命令行")
		}
	}
	// 空目录空间：不调 git 也行，但主机得有 git（脚本第一步就查）。
	res, err = f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "scratch"})
	if err != nil || f.host.Workspaces()["scratch"].URL != "" {
		t.Fatalf("空目录空间：%v / %+v", err, f.host.Workspaces())
	}
	// 入参：没仓库不能有分支 / 凭证；名称非法。
	for name, req := range map[string]devhost.WorkspaceRequest{
		"没仓库有分支": {HostID: host.ID, Name: "x1", Branch: "main"},
		"没仓库有凭证": {HostID: host.ID, Name: "x2", Credential: &secret},
		"名称非法":   {HostID: host.ID, Name: "bad name"},
		"地址非法":   {HostID: host.ID, Name: "x3", RepoURL: "https://x/$(id)"},
	} {
		if _, err := f.dev.CreateWorkspace(ctx, req); err == nil || code(t, err) != devhost.CodeInvalidHost {
			t.Fatalf("%s 应答 invalid_host：%v", name, err)
		}
	}

	// 删除：只认自己的路径形状；目录收掉。
	ws := &store.Workspace{HostID: host.ID, Name: "scratch", Path: res.Path}
	if _, err := f.dev.RemoveWorkspaceDir(ctx, ws); err != nil {
		t.Fatalf("RemoveWorkspaceDir: %v", err)
	}
	if _, still := f.host.Workspaces()["scratch"]; still {
		t.Fatal("目录没删掉")
	}
	for name, bad := range map[string]*store.Workspace{
		"根目录":  {HostID: host.ID, Name: "scratch", Path: "/"},
		"越界":   {HostID: host.ID, Name: "scratch", Path: "/home/dev/workspaces/../scratch"},
		"名字不符": {HostID: host.ID, Name: "scratch", Path: "/home/dev/workspaces/other"},
		"带引号":  {HostID: host.ID, Name: "scratch", Path: "/home/dev/workspaces/scratch'"},
	} {
		if _, err := f.dev.RemoveWorkspaceDir(ctx, bad); err == nil || code(t, err) != devhost.CodeInvalidHost {
			t.Fatalf("%s 应拒绝：%v", name, err)
		}
	}
}

func TestWorkspaceNeedsGitAndWorker(t *testing.T) {
	ctx := context.Background()
	// 没装 git：tool_missing。
	f := newFixture(t, devhosttest.Options{Username: "dev"})
	host := f.enroll(t, "dev", false)
	if _, err := f.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "x"}); err == nil || code(t, err) != devhost.CodeToolMissing {
		t.Fatalf("没 git 应答 tool_missing：%v", err)
	}
	// 受控纳管：不连主机。
	g := newFixture(t, devhosttest.Options{Username: "root", Git: "git version 2.43.0"})
	managed := g.enrollKind(t, store.AgentHostKindManaged, "root", false)
	before := len(g.host.Execs())
	if _, err := g.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: managed.ID, Name: "x"}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("受控纳管应答 host_kind_not_allowed：%v", err)
	}
	if _, err := g.dev.RemoveWorkspaceDir(ctx, &store.Workspace{HostID: managed.ID, Name: "x", Path: "/root/workspaces/x"}); err == nil || code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("受控纳管删除应答 host_kind_not_allowed：%v", err)
	}
	if len(g.host.Execs()) != before {
		t.Fatal("受控纳管的主机不该被连")
	}
}
