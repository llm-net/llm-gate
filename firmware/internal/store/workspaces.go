package store

// workspaces 仓储：「智能体 → 工作空间」。约定同 repo.go：context 化、走 prepared
// statement、未命中 → ErrNotFound、唯一性冲突 → ErrConflict、时间入库经 fmtTime。
//
// 一行是一个工作空间，分两种类型（Kind）：
//   - dev    开发工作空间：一台工作节点上 ~/workspaces/<name> 那个目录，外加创建时克隆的
//     仓库地址、分支与所用的 git 凭证。目录本身在主机上（由 internal/devhost 创建 / 删除），
//     本表只记它在哪、从哪来；行随主机解除纳管级联删除，凭证删掉只把 CredentialID 置空
//     （空间还在，只是之后没有凭证可用）。
//   - studio 创作工作空间：智能体在里面生成与整理图像 / 视频的目录，没有仓库。落在设备自己
//     的数据目录下（HostID 为 0、列为 NULL，internal/studio 创建 / 删除；设备上的创作工作
//     空间之间空间名唯一），或落在一台工作节点上（HostID 非零，目录同开发工作空间由
//     internal/devhost 创建 / 删除，设备经守护进程读写文件；同一台主机上空间名唯一）。
//     创作工作空间还带一个创作类型（Template，internal/studio 的内嵌模板名），建空间时选定
//     一次、之后不改，本表只存名字。
//
// 入参校验失败返回包着 [ErrInvalidWorkspace] 的错误，管理面据此答 400。名称、仓库地址与
// 分支名都要拼进主机上跑的 shell 脚本（双引号里），因此这里的白名单同时是脚本安全边界：
// 三者都不含空白、控制字符、引号、`$`、反引号与反斜杠。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrInvalidWorkspace 是工作空间入参不合法的哨兵；具体原因在包着它的错误文本里。
var ErrInvalidWorkspace = errors.New("工作空间无效")

// WorkspaceNameMaxRunes 是空间名上限：主机上的 tmux 会话名（ws-<name>-main / ws-<name>-t<n>）要放得进
// 32 个字符。
const WorkspaceNameMaxRunes = 24

var (
	workspaceNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,23}$`)
	// workspaceBranchRE 与 devd 的分支名白名单同形。
	workspaceBranchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	// workspaceURLRE 是仓库地址允许的字符：https 地址与 scp 形状的 ssh 地址都在其中；
	// 不含引号、空白、`$`、反引号、反斜杠，拼进双引号里不会被 shell 再解释。
	workspaceURLRE = regexp.MustCompile(`^[A-Za-z0-9._~:/@%+-]+$`)
	// workspaceTemplateRE 是创作类型名的形状（internal/studio 的模板 ID）：小写字母 / 数字与连字符。
	workspaceTemplateRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// workspaces.kind 的封闭词汇表（与 0052 迁移的 CHECK 同步维护）。
const (
	// WorkspaceKindDev 开发工作空间：工作节点上的目录。
	WorkspaceKindDev = "dev"
	// WorkspaceKindStudio 创作工作空间：设备数据目录下的目录，由智能体生成 / 整理素材。
	WorkspaceKindStudio = "studio"
)

// ValidWorkspaceKind 报告 kind 是否在词汇表里。
func ValidWorkspaceKind(kind string) bool {
	return kind == WorkspaceKindDev || kind == WorkspaceKindStudio
}

// workspaceColumns 是 SELECT 列表（与 scanWorkspace 的扫描顺序一一对应）。
const workspaceColumns = `id, kind, COALESCE(host_id, 0), name, path, repo_url, branch, COALESCE(credential_id, ''), template, created_at, updated_at`

// Workspace 是 workspaces 表的一行。
type Workspace struct {
	ID string
	// Kind 是类型（WorkspaceKindDev | WorkspaceKindStudio），创建时选定、之后不改。
	Kind string
	// HostID 是所在工作节点；创作工作空间没有主机，为 0。
	HostID int64
	// Name 是空间名，也是主机上的目录名（同一台主机上唯一）。
	Name string
	// Path 是创建时在主机上落成的绝对路径。
	Path string
	// RepoURL / Branch 是创建时克隆的仓库与分支；空 = 空目录 / 缺省分支。
	RepoURL string
	Branch  string
	// CredentialID 是克隆时用的 git 凭证；空 = 没用凭证或凭证已删除。
	CredentialID string
	// Template 是创作工作空间的创作类型（internal/studio 的模板 ID），创建时选定、之后不改；
	// 开发工作空间为空。
	Template  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsStudio 报告是不是创作工作空间。
func (w Workspace) IsStudio() bool { return w.Kind == WorkspaceKindStudio }

// OnHost 报告目录在一台工作节点上（HostID 非零）；否则在设备自己的数据目录下。
func (w Workspace) OnHost() bool { return w.HostID != 0 }

// NewWorkspace 是 [Store.CreateWorkspace] 的入参。字段须已经过 Normalize* 收窄。
type NewWorkspace struct {
	// ID 空取 NewULID(CreatedAt)；创作工作空间的目录名就是 ID，调用方先生成再建目录。
	ID string
	// Kind 空取 WorkspaceKindDev。创作工作空间不能带仓库 / 分支 / 凭证（HostID 为 0 在设备上、
	// 非零在那台主机上）；开发工作空间 HostID 必填。
	Kind         string
	HostID       int64
	Name         string
	Path         string
	RepoURL      string
	Branch       string
	CredentialID string
	// Template 是创作类型：创作工作空间必填（调用方先按 internal/studio 的模板表校验），开发
	// 工作空间必须为空。
	Template string
	// CreatedAt 零值取当前时间。
	CreatedAt time.Time
}

// NormalizeWorkspaceTemplate 收窄创作类型名：1–32 个小写字母 / 数字 / 连字符，字母或数字开头。
// 只管形状，名字是否存在由 internal/studio 判。
func NormalizeWorkspaceTemplate(template string) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", fmt.Errorf("%w: 创作类型不能为空", ErrInvalidWorkspace)
	}
	if !workspaceTemplateRE.MatchString(template) {
		return "", fmt.Errorf("%w: 创作类型名只能是小写字母、数字与连字符", ErrInvalidWorkspace)
	}
	return template, nil
}

// NormalizeWorkspaceName 收窄空间名：1–24 个字符，字母 / 数字开头，只含字母、数字、
// `.`、`_`、`-`。
func NormalizeWorkspaceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: 名称不能为空", ErrInvalidWorkspace)
	}
	if !workspaceNameRE.MatchString(name) {
		return "", fmt.Errorf("%w: 名称须以字母或数字开头，只含字母、数字、点、下划线与连字符，且不超过 %d 个字符", ErrInvalidWorkspace, WorkspaceNameMaxRunes)
	}
	return name, nil
}

// NormalizeRepoURL 收窄仓库地址：空串合法（不克隆）；否则只认 http(s):// 地址、
// ssh:// 地址与 `user@host:path` 的 scp 形状，且只含白名单字符、不以 `-` 开头。
func NormalizeRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 512 || !workspaceURLRE.MatchString(raw) || strings.HasPrefix(raw, "-") {
		return "", fmt.Errorf("%w: 仓库地址含不允许的字符", ErrInvalidWorkspace)
	}
	switch {
	case strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "ssh://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: 仓库地址不是合法的 URL", ErrInvalidWorkspace)
		}
		// ssh 地址里的用户名（git@）是协议的一部分；http(s) 里的账号 / 口令走凭证，不进地址。
		if u.User != nil && (u.Scheme != "ssh" || u.User.Username() == "" || func() bool { _, has := u.User.Password(); return has }()) {
			return "", fmt.Errorf("%w: 仓库地址里不能带账号或口令，请改用凭证", ErrInvalidWorkspace)
		}
		return raw, nil
	case strings.Contains(raw, "@") && strings.Contains(raw, ":") && !strings.Contains(raw, "://"):
		// git@github.com:org/repo.git
		return raw, nil
	}
	return "", fmt.Errorf("%w: 仓库地址须以 https://、http://、ssh:// 开头，或是 git@主机:路径 的形式", ErrInvalidWorkspace)
}

// NormalizeBranch 收窄分支名：空串合法（缺省分支）；否则按 git 引用名的常见约束。
func NormalizeBranch(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", nil
	}
	if !workspaceBranchRE.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".lock") || strings.Contains(branch, "//") || strings.Contains(branch, "/.") {
		return "", fmt.Errorf("%w: 分支名不合法", ErrInvalidWorkspace)
	}
	return branch, nil
}

// CreateWorkspace 建一行。同一台主机上（或设备上的创作工作空间之间）重名即 ErrConflict；
// 主机或凭证不存在（外键）也按 ErrConflict 报出，调用方应先点查它们。
func (s *Store) CreateWorkspace(ctx context.Context, nw NewWorkspace) (*Workspace, error) {
	kind := nw.Kind
	if kind == "" {
		kind = WorkspaceKindDev
	}
	if !ValidWorkspaceKind(kind) {
		return nil, fmt.Errorf("%w: 非法类型 %q", ErrInvalidWorkspace, kind)
	}
	name, err := NormalizeWorkspaceName(nw.Name)
	if err != nil {
		return nil, err
	}
	switch kind {
	case WorkspaceKindStudio:
		if nw.HostID < 0 {
			return nil, fmt.Errorf("%w: 非法的主机", ErrInvalidWorkspace)
		}
		if nw.RepoURL != "" || nw.Branch != "" || nw.CredentialID != "" {
			return nil, fmt.Errorf("%w: 创作工作空间不能带仓库、分支或凭证", ErrInvalidWorkspace)
		}
		if _, err := NormalizeWorkspaceTemplate(nw.Template); err != nil {
			return nil, err
		}
	default:
		if nw.HostID <= 0 {
			return nil, fmt.Errorf("%w: 开发工作空间必须指定工作节点", ErrInvalidWorkspace)
		}
		if nw.Template != "" {
			return nil, fmt.Errorf("%w: 开发工作空间没有创作类型", ErrInvalidWorkspace)
		}
	}
	repo, err := NormalizeRepoURL(nw.RepoURL)
	if err != nil {
		return nil, err
	}
	branch, err := NormalizeBranch(nw.Branch)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(nw.Path) == "" {
		return nil, fmt.Errorf("%w: 路径不能为空", ErrInvalidWorkspace)
	}
	at := nw.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := nw.ID
	if id == "" {
		id = NewULID(at)
	}
	ts := fmtTime(at)
	var cred, host any
	if nw.CredentialID != "" {
		cred = nw.CredentialID
	}
	if nw.HostID != 0 {
		host = nw.HostID
	}
	if _, err := s.stmtCreateWorkspace.ExecContext(ctx, id, kind, host, name, nw.Path, repo, branch, cred, strings.TrimSpace(nw.Template), ts, ts); err != nil {
		return nil, fmt.Errorf("创建工作空间: %w", mapErr(err))
	}
	return s.GetWorkspace(ctx, id)
}

// ListWorkspaces 按创建顺序列出全部工作空间（ULID 字典序即时间序）。
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.stmtListWorkspaces.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出工作空间: %w", err)
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("列出工作空间: %w", err)
		}
		out = append(out, *w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出工作空间: %w", err)
	}
	return out, nil
}

// GetWorkspace 按 id 取一行；不存在返回 ErrNotFound。
func (s *Store) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	w, err := scanWorkspace(s.stmtGetWorkspace.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取工作空间 %s: %w", id, err)
	}
	return w, nil
}

// GetWorkspaceByName 按主机与空间名取一行（hostID 为 0 即设备上的创作工作空间）；不存在
// 返回 ErrNotFound。
func (s *Store) GetWorkspaceByName(ctx context.Context, hostID int64, name string) (*Workspace, error) {
	var row *sql.Row
	if hostID == 0 {
		row = s.stmtGetStudioWorkspaceByName.QueryRowContext(ctx, name)
	} else {
		row = s.stmtGetWorkspaceByName.QueryRowContext(ctx, hostID, name)
	}
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取工作空间 %d/%s: %w", hostID, name, err)
	}
	return w, nil
}

// DeleteWorkspace 删一行；不存在返回 ErrNotFound。主机上的目录不归本方法管。
func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	res, err := s.stmtDeleteWorkspace.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("删除工作空间 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删除工作空间 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanWorkspace(row interface{ Scan(...any) error }) (*Workspace, error) {
	var (
		w         Workspace
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&w.ID, &w.Kind, &w.HostID, &w.Name, &w.Path, &w.RepoURL, &w.Branch, &w.CredentialID, &w.Template, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("解析创建时刻: %w", err)
	}
	w.CreatedAt = t
	if t, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("解析更新时刻: %w", err)
	}
	w.UpdatedAt = t
	return &w, nil
}
