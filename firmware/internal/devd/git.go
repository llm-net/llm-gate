package devd

// Git 集成：全部经主机上的 git 命令完成，守护进程不实现任何 Git 内部格式。
// 每条命令带超时、GIT_TERMINAL_PROMPT=0（要口令就直接失败，不会挂住）与
// LC_ALL=C.UTF-8（输出形状稳定，可解析）。推送/拉取用主机上已有的凭据与
// SSH 代理，守护进程不保存、不转交任何 Git 凭据。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	gitTimeout    = 60 * time.Second
	gitNetTimeout = 5 * time.Minute
	gitOutputMax  = 4 << 20
	gitLogMax     = 200
)

// GitStatus 是 `git status --porcelain=v2 --branch` 的结构化结果。
type GitStatus struct {
	Root     string           `json:"root"`
	Branch   string           `json:"branch"`
	Upstream string           `json:"upstream,omitempty"`
	Ahead    int              `json:"ahead"`
	Behind   int              `json:"behind"`
	Detached bool             `json:"detached"`
	Entries  []GitStatusEntry `json:"entries"`
}

// GitStatusEntry 是一条变更。Index / Worktree 是 porcelain 的两个状态字母
// （"." = 无变化），Untracked 是 ? 行。
type GitStatusEntry struct {
	Path      string `json:"path"`
	OrigPath  string `json:"orig_path,omitempty"`
	Index     string `json:"index"`
	Worktree  string `json:"worktree"`
	Untracked bool   `json:"untracked"`
	Conflict  bool   `json:"conflict"`
}

// GitCommit 是一条提交摘要。
type GitCommit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// GitBranch 是一个本地分支。
type GitBranch struct {
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// GitResult 是 pull / push / commit / checkout 这类动作的原始输出。
type GitResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

// gitRoot 找 dir 所在仓库的顶层；不在仓库里回空串。
func gitRoot(dir string) string {
	out, _, code, err := runGit(context.Background(), dir, gitTimeout, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C.UTF-8",
		"LANG=C.UTF-8",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, max: gitOutputMax}
	cmd.Stderr = &limitedBuffer{buf: &errBuf, max: 64 << 10}
	err = cmd.Run()
	stdout, stderr = out.String(), errBuf.String()
	if err == nil {
		return stdout, stderr, 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout, stderr, exit.ExitCode(), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, 0, &Error{Code: CodeGitFailed, Msg: "git 命令超时未返回"}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return stdout, stderr, 0, &Error{Code: CodeGitMissing, Msg: "主机上没有 git"}
	}
	return stdout, stderr, 0, &Error{Code: CodeGitFailed, Msg: fmt.Sprintf("git 未能执行：%s", err)}
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// gitRepo 把请求路径折成仓库顶层；不在仓库里答 CodeNotRepo。
func (s *Server) gitRepo(p string) (string, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err != nil {
		return "", fsError("读取目录", abs, err)
	} else if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root := gitRoot(abs)
	if root == "" {
		return "", &Error{Code: CodeNotRepo, Msg: fmt.Sprintf("%s 不在 Git 仓库里", abs)}
	}
	return root, nil
}

func gitFailure(stderr string, code int) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = fmt.Sprintf("git 退出码 %d", code)
	}
	return &Error{Code: CodeGitFailed, Msg: msg}
}

var aheadBehindRE = regexp.MustCompile(`\+(\d+) -(\d+)`)

func (s *Server) gitStatus(ctx context.Context, p string) (*GitStatus, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	out, stderr, code, err := runGit(ctx, root, gitTimeout, "status", "--porcelain=v2", "--branch", "--untracked-files=all", "-z")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitFailure(stderr, code)
	}
	st := &GitStatus{Root: root, Entries: []GitStatusEntry{}}
	parseStatusV2(out, st)
	return st, nil
}

// parseStatusV2 解 -z 分隔的 porcelain v2 输出（改名行的原路径在下一个 NUL 段）。
func parseStatusV2(out string, st *GitStatus) {
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		line := fields[i]
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			st.Branch = strings.TrimPrefix(line, "# branch.head ")
			if st.Branch == "(detached)" {
				st.Detached = true
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			if m := aheadBehindRE.FindStringSubmatch(line); m != nil {
				st.Ahead, _ = strconv.Atoi(m[1])
				st.Behind, _ = strconv.Atoi(m[2])
			}
		case strings.HasPrefix(line, "1 "):
			parts := strings.SplitN(line, " ", 9)
			if len(parts) == 9 {
				st.Entries = append(st.Entries, GitStatusEntry{Path: parts[8], Index: parts[1][:1], Worktree: parts[1][1:]})
			}
		case strings.HasPrefix(line, "2 "):
			parts := strings.SplitN(line, " ", 10)
			if len(parts) == 10 && i+1 < len(fields) {
				e := GitStatusEntry{Path: parts[9], Index: parts[1][:1], Worktree: parts[1][1:], OrigPath: fields[i+1]}
				i++
				st.Entries = append(st.Entries, e)
			}
		case strings.HasPrefix(line, "u "):
			parts := strings.SplitN(line, " ", 11)
			if len(parts) == 11 {
				st.Entries = append(st.Entries, GitStatusEntry{Path: parts[10], Index: parts[1][:1], Worktree: parts[1][1:], Conflict: true})
			}
		case strings.HasPrefix(line, "? "):
			st.Entries = append(st.Entries, GitStatusEntry{Path: line[2:], Index: ".", Worktree: "?", Untracked: true})
		}
	}
}

func (s *Server) gitLog(ctx context.Context, p string, n int) ([]GitCommit, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > gitLogMax {
		n = 50
	}
	out, stderr, code, err := runGit(ctx, root, gitTimeout, "log", "-z", "--format=%H%x1f%h%x1f%an%x1f%aI%x1f%s", "-n", strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		if strings.Contains(stderr, "does not have any commits") || strings.Contains(stderr, "unknown revision") {
			return []GitCommit{}, nil
		}
		return nil, gitFailure(stderr, code)
	}
	commits := []GitCommit{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 5)
		if len(f) != 5 {
			continue
		}
		commits = append(commits, GitCommit{Hash: f[0], Short: f[1], Author: f[2], Date: f[3], Subject: f[4]})
	}
	return commits, nil
}

func (s *Server) gitDiff(ctx context.Context, p, file string, staged bool) (string, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
	if file != "" {
		args = append(args, "--", file)
	}
	out, stderr, code, err := runGit(ctx, root, gitTimeout, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", gitFailure(stderr, code)
	}
	if out == "" && file != "" && !staged {
		// 未跟踪的新文件没有 diff：把整个文件当新增展示。
		out, _, _, _ = runGit(ctx, root, gitTimeout, "diff", "--no-color", "--no-index", "--", "/dev/null", file)
	}
	return out, nil
}

func (s *Server) gitStage(ctx context.Context, p string, files []string, unstage bool) error {
	root, err := s.gitRepo(p)
	if err != nil {
		return err
	}
	for _, f := range files {
		if strings.HasPrefix(f, "-") {
			return &Error{Code: CodeInvalidPath, Msg: "文件名不能以 - 开头"}
		}
	}
	var args []string
	switch {
	case unstage && len(files) == 0:
		args = []string{"reset", "-q"}
	case unstage:
		args = append([]string{"reset", "-q", "--"}, files...)
	case len(files) == 0:
		args = []string{"add", "-A"}
	default:
		args = append([]string{"add", "-A", "--"}, files...)
	}
	_, stderr, code, err := runGit(ctx, root, gitTimeout, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitFailure(stderr, code)
	}
	return nil
}

func (s *Server) gitDiscard(ctx context.Context, p string, files []string) error {
	root, err := s.gitRepo(p)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return &Error{Code: CodeInvalidPath, Msg: "请指明要放弃改动的文件"}
	}
	for _, f := range files {
		if strings.HasPrefix(f, "-") {
			return &Error{Code: CodeInvalidPath, Msg: "文件名不能以 - 开头"}
		}
	}
	// 已跟踪文件恢复到 HEAD；未跟踪的新文件直接删。
	_, stderr, code, err := runGit(ctx, root, gitTimeout, append([]string{"checkout", "HEAD", "--"}, files...)...)
	if err != nil {
		return err
	}
	if code != 0 {
		st, err := s.gitStatus(ctx, root)
		if err != nil {
			return err
		}
		untracked := map[string]bool{}
		for _, e := range st.Entries {
			if e.Untracked {
				untracked[e.Path] = true
			}
		}
		for _, f := range files {
			if !untracked[f] {
				return gitFailure(stderr, code)
			}
			os.Remove(filepath.Join(root, f))
		}
	}
	return nil
}

func (s *Server) gitCommit(ctx context.Context, p, message string) (*GitResult, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, &Error{Code: CodeInvalidPath, Msg: "提交说明不能为空"}
	}
	// 说明经 -m 传入：它不是秘密，进 argv 没有问题。
	out, stderr, code, err := runGit(ctx, root, gitTimeout, "commit", "-q", "-m", message)
	if err != nil {
		return nil, err
	}
	res := &GitResult{OK: code == 0, Output: strings.TrimSpace(out + stderr)}
	if code != 0 {
		return res, gitFailure(stderr+out, code)
	}
	return res, nil
}

func (s *Server) gitBranches(ctx context.Context, p string) ([]GitBranch, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	out, stderr, code, err := runGit(ctx, root, gitTimeout, "branch", "--format=%(refname:short)%09%(HEAD)")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitFailure(stderr, code)
	}
	branches := []GitBranch{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		name, head, _ := strings.Cut(line, "\t")
		branches = append(branches, GitBranch{Name: name, Current: head == "*"})
	}
	return branches, nil
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func (s *Server) gitCheckout(ctx context.Context, p, branch string, create bool) (*GitResult, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") {
		return nil, &Error{Code: CodeInvalidPath, Msg: "分支名不合法"}
	}
	args := []string{"checkout", "-q"}
	if create {
		args = append(args, "-b")
	}
	args = append(args, branch)
	out, stderr, code, err := runGit(ctx, root, gitTimeout, args...)
	if err != nil {
		return nil, err
	}
	res := &GitResult{OK: code == 0, Output: strings.TrimSpace(out + stderr)}
	if code != 0 {
		return res, gitFailure(stderr, code)
	}
	return res, nil
}

// gitRemote 跑 pull / push：走主机自己的凭据与 SSH 代理，要交互就直接失败。
func (s *Server) gitRemote(ctx context.Context, p, action string) (*GitResult, error) {
	root, err := s.gitRepo(p)
	if err != nil {
		return nil, err
	}
	var args []string
	switch action {
	case "pull":
		args = []string{"pull", "--ff-only", "--no-rebase"}
	case "push":
		args = []string{"push"}
	case "fetch":
		args = []string{"fetch", "--prune"}
	default:
		return nil, &Error{Code: CodeInvalidPath, Msg: "未知的 Git 动作"}
	}
	out, stderr, code, err := runGit(ctx, root, gitNetTimeout, args...)
	if err != nil {
		return nil, err
	}
	res := &GitResult{OK: code == 0, Output: strings.TrimSpace(out + "\n" + stderr)}
	if code != 0 {
		return res, gitFailure(stderr, code)
	}
	return res, nil
}
