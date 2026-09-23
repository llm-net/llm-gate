package devhost

// 工作节点上的「工作空间」（store/workspaces.go）：主机上 ~/workspaces/<name> 那个目录，
// 可选从 git 仓库克隆而来。创建与删除目录都经这台主机已有的免密 SSH 连接、以 SSH 用户
// 身份跑一段 POSIX sh；读数在设备的 workspaces 表里，主机上不留任何记录。
//
//   - 创建先校验后落地：脚本先 `git ls-remote` 验仓库地址、分支与凭证，任一不通就退出、
//     不建目录；通过后 mkdir，再 `git clone`（克隆失败把目录收掉）。没给仓库只建空目录。
//   - git 凭证（托管站点的账号 + 令牌）经 stdin 交给脚本、只存在于脚本进程的环境变量里，
//     再由一次性的 credential.helper 交给 git：不进 argv、不进仓库配置、不落主机磁盘；
//     克隆完成后仓库里没有它（之后的 pull / push 用主机自己的凭据）。
//     设备侧不入库明文、不进日志、不进审计 detail（§15.1）。
//   - 删除只认设备自己当初落成的那个路径形状（$HOME/workspaces/<name>），别的路径一律拒绝。
//
// 受控纳管的主机没有工作空间：全部入口先按类型拒绝（CodeKindNotAllowed）。

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 错误码（补充 devhost.go 的那组）。
const (
	// CodeWorkspaceDirExists：主机上已有同名目录（不是设备建的，不敢动）。
	CodeWorkspaceDirExists = "workspace_dir_exists"
	// CodeRepoUnreachable：主机连不上仓库（解析不了、超时、被拒）。
	CodeRepoUnreachable = "repo_unreachable"
	// CodeRepoAuthFailed：仓库要求认证而凭证不对或没给。
	CodeRepoAuthFailed = "repo_auth_failed"
	// CodeRepoNotFound：仓库不存在（或凭证看不到它）。
	CodeRepoNotFound = "repo_not_found"
	// CodeBranchNotFound：仓库里没有这个分支。
	CodeBranchNotFound = "branch_not_found"
	// CodeCloneFailed：校验通过但克隆失败（目录已收掉）。
	CodeCloneFailed = "clone_failed"
)

// WorkspacesDir 是主机上放工作空间的目录（相对 SSH 用户的家目录）。
const WorkspacesDir = "workspaces"

const (
	workspaceCheckWait  = 3 * time.Minute
	workspaceCloneWait  = 10 * time.Minute
	workspaceRemoveWait = 2 * time.Minute
)

// workspaceMarker 是脚本的首行：假主机凭它认出这是工作空间脚本并读出参数。
const (
	workspaceMarker       = "# llmgate-workspace"
	workspaceRemoveMarker = "# llmgate-workspace-remove"
)

// 脚本的退出码：每一个对应一种可读的失败。
const (
	wsExitGitMissing  = 90
	wsExitDirExists   = 91
	wsExitRemoteError = 92
	wsExitNoBranch    = 93
	wsExitMkdirFailed = 94
	wsExitCloneFailed = 95
)

// WorkspaceRequest 是「创建工作空间」的入参。Name / RepoURL / Branch 须已经过 store 的
// Normalize* 收窄（这里再校验一遍：它们要拼进 shell 脚本）。
type WorkspaceRequest struct {
	HostID  int64
	Name    string
	RepoURL string
	Branch  string
	// Credential 是克隆用的 git 凭证；nil = 不带凭证。令牌只在本次请求内使用。
	Credential *store.CredentialSecret
}

// WorkspaceResult 是创建的结果。
type WorkspaceResult struct {
	Host *store.AgentHost
	// Path 是主机上落成的绝对路径。
	Path string
}

// workspaceScript 生成创建脚本：整段放进 sh -c '…'，里面没有单引号；名称、地址、分支
// 都已收窄到能安全地放进双引号。带凭证时前两行 stdin 是账号与令牌。
func workspaceScript(name, repo, branch string, withCred bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s name=%s url=%s branch=%s\n", workspaceMarker, name, repo, branch)
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")
	if withCred {
		b.WriteString("IFS= read -r GIT_USER; IFS= read -r GIT_TOKEN; export GIT_USER GIT_TOKEN\n")
		b.WriteString(`helper="!f() { printf \"username=%s\\npassword=%s\\n\" \"\$GIT_USER\" \"\$GIT_TOKEN\"; }; f"` + "\n")
		b.WriteString(`set -- -c credential.helper= -c credential.helper="$helper"` + "\n")
	}
	fmt.Fprintf(&b, "command -v git >/dev/null 2>&1 || exit %d\n", wsExitGitMissing)
	fmt.Fprintf(&b, "ws=\"$HOME/%s/%s\"\n", WorkspacesDir, name)
	fmt.Fprintf(&b, "[ -e \"$ws\" ] && exit %d\n", wsExitDirExists)
	if repo != "" {
		ref := ""
		if branch != "" {
			ref = "refs/heads/" + branch
		}
		fmt.Fprintf(&b, "out=$(git \"$@\" ls-remote --exit-code --heads -- \"%s\" %s 2>&1); rc=$?\n", repo, ref)
		fmt.Fprintf(&b, "if [ $rc -ne 0 ]; then printf \"%%s\\n\" \"$out\" >&2; [ $rc -eq 2 ] && exit %d; exit %d; fi\n", wsExitNoBranch, wsExitRemoteError)
	}
	fmt.Fprintf(&b, "mkdir -p \"$HOME/%s\" && mkdir \"$ws\" || exit %d\n", WorkspacesDir, wsExitMkdirFailed)
	if repo != "" {
		clone := "git \"$@\" clone"
		if branch != "" {
			clone += " --branch \"" + branch + "\""
		}
		fmt.Fprintf(&b, "%s -- \"%s\" \"$ws\" 2>&1 || { rm -rf \"$ws\"; exit %d; }\n", clone, repo, wsExitCloneFailed)
	}
	b.WriteString("printf \"path=%s\\n\" \"$ws\"\n")
	return b.String()
}

// wsPathRE 是设备自己落成的路径应有的形状；删除时只认它。
var wsPathRE = regexp.MustCompile(`^/[A-Za-z0-9._@/-]+$`)

// workspaceRemoveScript 生成删除脚本。
func workspaceRemoveScript(path string) string {
	return fmt.Sprintf("%s path=%s\nrm -rf -- \"%s\"\n", workspaceRemoveMarker, path, path)
}

// CreateWorkspace 在工作节点上校验并创建一个工作空间目录（可选克隆仓库）。
func (m *Manager) CreateWorkspace(ctx context.Context, req WorkspaceRequest) (*WorkspaceResult, error) {
	host, err := m.workerHost(ctx, req.HostID)
	if err != nil {
		return nil, err
	}
	name, err := store.NormalizeWorkspaceName(req.Name)
	if err != nil {
		return nil, &Error{Code: CodeInvalidHost, Msg: err.Error()}
	}
	repo, err := store.NormalizeRepoURL(req.RepoURL)
	if err != nil {
		return nil, &Error{Code: CodeInvalidHost, Msg: err.Error()}
	}
	branch, err := store.NormalizeBranch(req.Branch)
	if err != nil {
		return nil, &Error{Code: CodeInvalidHost, Msg: err.Error()}
	}
	if repo == "" && branch != "" {
		return nil, &Error{Code: CodeInvalidHost, Msg: "没有仓库地址时不能指定分支"}
	}
	if repo == "" && req.Credential != nil {
		return nil, &Error{Code: CodeInvalidHost, Msg: "没有仓库地址时不需要凭证"}
	}
	stdin := ""
	if req.Credential != nil {
		if strings.ContainsAny(req.Credential.Username, "\r\n") || strings.ContainsAny(req.Credential.Plaintext(), "\r\n") {
			return nil, &Error{Code: CodeInvalidHost, Msg: "凭证含换行，无法交给主机"}
		}
		stdin = req.Credential.Username + "\n" + req.Credential.Plaintext() + "\n"
	}
	wait := workspaceCheckWait
	if repo != "" {
		wait = workspaceCloneWait
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout+wait)
	defer cancel()
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	command := "/bin/sh -c '" + workspaceScript(name, repo, branch, req.Credential != nil) + "'"
	out, stderr, code, err := m.run(ctx, c, command, stdin, wait)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, workspaceFailure(code, stderr, repo, branch)
	}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "path=") {
			path = strings.TrimPrefix(strings.TrimSpace(line), "path=")
		}
	}
	if path == "" {
		return nil, &Error{Code: CodeCommandFailed, Msg: "主机上的脚本没有报出工作空间路径" + tail(stderr)}
	}
	return &WorkspaceResult{Host: host, Path: path}, nil
}

// workspaceFailure 把脚本的退出码与 stderr 翻成人话。stderr 里没有令牌（git 不回显它）。
func workspaceFailure(code int, stderr, repo, branch string) error {
	switch code {
	case wsExitGitMissing:
		return &Error{Code: CodeToolMissing, Msg: "主机上没有 git：请先在「工具配置」页安装。"}
	case wsExitDirExists:
		return &Error{Code: CodeWorkspaceDirExists, Msg: "主机上已有同名目录（可能是先前删除空间时保留下来的）：请换一个名称，或在主机上清理该目录后重试。"}
	case wsExitNoBranch:
		if branch == "" {
			return &Error{Code: CodeBranchNotFound, Msg: "仓库里没有任何分支" + tail(stderr)}
		}
		return &Error{Code: CodeBranchNotFound, Msg: fmt.Sprintf("仓库里没有分支 %s", branch)}
	case wsExitRemoteError:
		low := strings.ToLower(stderr)
		switch {
		case strings.Contains(low, "authentication failed"), strings.Contains(low, "could not read username"),
			strings.Contains(low, "invalid username or password"), strings.Contains(low, "permission denied"),
			strings.Contains(low, "403"), strings.Contains(low, "401"):
			return &Error{Code: CodeRepoAuthFailed, Msg: "仓库拒绝了访问：凭证不对、没有权限，或私有仓库没有选凭证" + tail(stderr)}
		case strings.Contains(low, "not found"), strings.Contains(low, "does not appear to be a git repository"),
			strings.Contains(low, "404"):
			return &Error{Code: CodeRepoNotFound, Msg: "找不到仓库 " + repo + tail(stderr)}
		}
		return &Error{Code: CodeRepoUnreachable, Msg: "主机连不上仓库 " + repo + tail(stderr)}
	case wsExitMkdirFailed:
		return &Error{Code: CodeCommandFailed, Msg: "在主机上创建目录失败" + tail(stderr)}
	case wsExitCloneFailed:
		return &Error{Code: CodeCloneFailed, Msg: "克隆仓库失败，已收回目录" + tail(stderr)}
	}
	return &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("在主机上创建工作空间失败（退出码 %d）%s", code, tail(stderr))}
}

// RemoveWorkspaceDir 在主机上删掉一个工作空间目录。只认设备自己落成的路径形状
// （…/workspaces/<name>），别的一律拒绝；目录本来就不在也算成功。
func (m *Manager) RemoveWorkspaceDir(ctx context.Context, ws *store.Workspace) (*store.AgentHost, error) {
	host, err := m.workerHost(ctx, ws.HostID)
	if err != nil {
		return nil, err
	}
	path := ws.Path
	if !wsPathRE.MatchString(path) || strings.Contains(path, "/../") || strings.Contains(path, "//") ||
		!strings.HasSuffix(path, "/"+WorkspacesDir+"/"+ws.Name) {
		return nil, &Error{Code: CodeInvalidHost, Msg: "工作空间路径形态异常，请手工删除主机上的目录：" + path}
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	_, stderr, code, err := m.run(ctx, c, "/bin/sh -c '"+workspaceRemoveScript(path)+"'", "", workspaceRemoveWait)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: "在主机上删除目录失败" + tail(stderr)}
	}
	return host, nil
}
