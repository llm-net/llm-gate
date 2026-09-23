package store

// agent_hosts 仓储：「智能体 → 主机/SoC」纳管的主机清单。
// 约定同 repo.go：context 化、走 prepared statement、未命中 → ErrNotFound、
// 唯一性冲突 → ErrConflict、时间入库经 fmtTime。
//
// 一行是一台可被智能体远程管理的主机或 SoC 开发板，纳管时选定类型（kind）之后不改：
// 受控纳管只能用 Agent远控，工作节点还可安装守护进程 devd。纳管口令**不入库**：它只在
// 「装证书」那一次请求里经内存传给 SSH 会话（§15.1）。设备访问证书的私钥封存在
// settings（见 internal/agenthost），本表只记该主机上装着的公钥指纹——它与当前
// 证书指纹不一致，就是「证书待更新」。
//
// 主机公钥（HostKey）是首次纳管时钉死的值，之后每次连接逐字比对：换了就是换了
// 一台机器或被劫持，调用方据此拒绝连接，而不是静默信任。
//
// 同一行还记着这台主机有没有装守护进程 devd llmgate-devd（devd_* 列，见
// internal/devhost）：Devd.Status 为空 = 没装。守护进程经这条 SSH 连接的转发通道
// 访问，没有自己的端口、证书或身份，行里只有它的自述与最近一次检查结果。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// agent_hosts.kind 的封闭词汇表（与 0057 迁移的 CHECK 同步维护）。类型在纳管时选定、
// 之后不改；它决定这台主机能用哪些能力，不影响 SSH 连接与证书。
const (
	// AgentHostKindManaged 受控纳管：只能用 Agent远控，不能安装守护进程。
	AgentHostKindManaged = "managed"
	// AgentHostKindWorker 工作节点：Agent远控之外还可安装 devd、git 与 gate，开工作空间。
	AgentHostKindWorker = "worker"
	// AgentHostKindModelService 模型服务节点：装守护进程 modeld（llmgate-modeld），管理多台
	// 算力服务器、调度模型推理任务、对外提供任务 API、缓存模型文件、直接运行 Codex App Server /
	// Claude Code；工具配置页照常可用（gate 与开发工具），没有工作空间。
	AgentHostKindModelService = "model_service"
)

// ValidAgentHostKind 报告 kind 是否在词汇表里。
func ValidAgentHostKind(kind string) bool {
	return kind == AgentHostKindManaged || kind == AgentHostKindWorker || kind == AgentHostKindModelService
}

// agent_hosts.status 的封闭词汇表（与 0042 迁移的 CHECK 同步维护）。
const (
	// AgentHostStatusReady：最近一次操作确认了「凭证书免密登录可用」。
	AgentHostStatusReady = "ready"
	// AgentHostStatusError：最近一次操作失败（连不上、认证被拒、装证书没成）。
	// 失败原因记 LastError，界面照原文展示。
	AgentHostStatusError = "error"
)

// agent_hosts.devd_status 的封闭词汇表（与 0045 迁移的 CHECK 同步维护）。devd_* 列记的是
// 「这台主机上的守护进程」：工作节点是 devd（llmgate-devd），模型服务节点是 modeld
// （llmgate-modeld）——一台主机只有一种类型、只装一种守护进程，列名沿用。
const (
	// DevdStatusNone：这台主机上没装守护进程（其余 devd 字段全是零值）。
	DevdStatusNone = ""
	// DevdStatusReady：最近一次操作确认了「经 SSH 连得上守护进程」。
	DevdStatusReady = "ready"
	// DevdStatusError：最近一次安装 / 检查失败，原因记 DevdLastError。
	DevdStatusError = "error"
)

// agentHostColumns 是 SELECT 列表（与 scanAgentHost 的扫描顺序一一对应，
// 改任一侧必须同步另一侧）。
const agentHostColumns = `id, name, kind, address, port, username, host_key, key_fingerprint,
	sudo_nopasswd, system, status, last_error, last_checked_at, created_at, updated_at,
	devd_version, devd_home, devd_tmux, devd_status, devd_last_error, devd_checked_at`

// AgentHost 是 agent_hosts 表的一行。
type AgentHost struct {
	ID   int64
	Name string
	// Kind 是纳管时选定的类型（AgentHostKindManaged | AgentHostKindWorker |
	// AgentHostKindModelService），之后不改。
	Kind     string
	Address  string
	Port     int
	Username string
	// HostKey 是该主机的公钥（authorized_keys 单行形状），首次纳管时钉死。
	HostKey string
	// KeyFingerprint 是这台主机上已安装的设备访问证书公钥指纹（SHA256:…）。
	// 空 = 还没装上；与当前证书不同 = 证书待更新。
	KeyFingerprint string
	// SudoNoPasswd 报告最近一次检查时该用户能否免密 sudo（root 恒为 true）。
	SudoNoPasswd bool
	// System 是主机自述的系统与架构（uname -srm 的输出），只作展示。
	System string
	Status string // AgentHostStatusReady | AgentHostStatusError
	// LastError 是最近一次操作的失败原因（成功即清空）。不含口令与私钥。
	LastError string
	// LastCheckedAt 是最近一次连接检查的时刻；零值 = 从未检查过。
	LastCheckedAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// Devd 是这台主机上守护进程（工作节点 devd / 模型服务节点 modeld）的观测值；
	// Devd.Status 为空 = 没装。
	Devd AgentHostDevd
}

// AgentHostDevd 是一台主机上守护进程（llmgate-devd 或 llmgate-modeld）的观测值。
type AgentHostDevd struct {
	// Version / Home / Tmux 是守护进程自述，只作展示。
	Version string
	Home    string
	Tmux    bool
	Status  string // DevdStatusNone | DevdStatusReady | DevdStatusError
	// LastError 是最近一次安装 / 检查的失败原因（成功即清空）。不含口令与私钥。
	LastError string
	// CheckedAt 是最近一次连守护进程的时刻；零值 = 从未。
	CheckedAt time.Time
}

// Installed 报告这台主机上装着守护进程（不论最近一次检查成败）。
func (d AgentHostDevd) Installed() bool { return d.Status != DevdStatusNone }

// AllowsDevd 报告这种类型的主机能不能安装守护进程 devd、开工作空间：只有工作节点可以。
func (h AgentHost) AllowsDevd() bool { return h.Kind == AgentHostKindWorker }

// IsModelService 报告这是一台模型服务节点（守护进程是 modeld）。
func (h AgentHost) IsModelService() bool { return h.Kind == AgentHostKindModelService }

// AllowsDaemon 报告这种类型的主机能不能安装守护进程（工作节点装 devd、模型服务节点装
// modeld）；受控纳管不能。
func (h AgentHost) AllowsDaemon() bool { return h.AllowsDevd() || h.IsModelService() }

// AllowsTools 报告这种类型的主机有没有「工具配置」页（git / tmux / gate / 开发工具）：
// 工作节点与模型服务节点都有——模型服务节点要靠 gate 装 Codex / Claude Code 才能运行引擎。
func (h AgentHost) AllowsTools() bool { return h.AllowsDaemon() }

// DaemonName 是这种类型的主机上守护进程的短名（devd / modeld），受控纳管为空。
func (h AgentHost) DaemonName() string {
	switch h.Kind {
	case AgentHostKindWorker:
		return "devd"
	case AgentHostKindModelService:
		return "modeld"
	}
	return ""
}

// NewAgentHost 是 [Store.CreateAgentHost] 的入参（同型 string 字段多，用具名
// 结构体而非位置参数）。
type NewAgentHost struct {
	Name string
	// Kind 必填，须在词汇表里（AgentHostKindManaged | AgentHostKindWorker |
	// AgentHostKindModelService）。
	Kind     string
	Address  string
	Port     int
	Username string
	// CreatedAt 零值取当前时间（测试与固定时间入库用）。
	CreatedAt time.Time
}

// AgentHostState 是一次纳管 / 下发 / 检查之后要写回的整组观测值。
// 分组写回而不是逐列 setter：这些值出自同一次会话，分开写会留下「证书装上了、
// 状态还是失败」这类中间态。
type AgentHostState struct {
	ID             int64
	HostKey        string
	KeyFingerprint string
	SudoNoPasswd   bool
	System         string
	Status         string
	LastError      string
	// CheckedAt 零值取当前时间。
	CheckedAt time.Time
}

// AgentHostDevdState 是一次安装 / 检查守护进程之后要整组写回的观测值。
type AgentHostDevdState struct {
	ID        int64
	Version   string
	Home      string
	Tmux      bool
	Status    string // DevdStatusReady | DevdStatusError
	LastError string
	// CheckedAt 零值取当前时间。
	CheckedAt time.Time
}

// CreateAgentHost 建一行主机。连接三元组（用户名、地址、端口）重复即 ErrConflict。
// 新行恒以 AgentHostStatusError 起步：没装上证书之前，它不是「可用」。
func (s *Store) CreateAgentHost(ctx context.Context, nh NewAgentHost) (*AgentHost, error) {
	if nh.Address == "" || nh.Username == "" {
		return nil, fmt.Errorf("添加主机: 地址与用户名不能为空")
	}
	if nh.Port <= 0 || nh.Port > 65535 {
		return nil, fmt.Errorf("添加主机: 端口 %d 不在 1–65535", nh.Port)
	}
	if !ValidAgentHostKind(nh.Kind) {
		return nil, fmt.Errorf("添加主机: 非法类型 %q", nh.Kind)
	}
	at := nh.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	ts := fmtTime(at)
	res, err := s.stmtCreateAgentHost.ExecContext(ctx,
		nh.Name, nh.Kind, nh.Address, nh.Port, nh.Username, AgentHostStatusError, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("添加主机: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("添加主机: %w", err)
	}
	return s.GetAgentHost(ctx, id)
}

// ListAgentHosts 按 id 升序列出全部主机（添加顺序即列表顺序）。
func (s *Store) ListAgentHosts(ctx context.Context) ([]AgentHost, error) {
	rows, err := s.stmtListAgentHosts.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出主机: %w", err)
	}
	defer rows.Close()
	hosts := []AgentHost{}
	for rows.Next() {
		h, err := scanAgentHost(rows)
		if err != nil {
			return nil, fmt.Errorf("列出主机: %w", err)
		}
		hosts = append(hosts, *h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出主机: %w", err)
	}
	return hosts, nil
}

// GetAgentHost 按 id 取一行；不存在返回 ErrNotFound。
func (s *Store) GetAgentHost(ctx context.Context, id int64) (*AgentHost, error) {
	h, err := scanAgentHost(s.stmtGetAgentHost.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取主机 %d: %w", id, err)
	}
	return h, nil
}

// RenameAgentHost 只改展示名称（空串合法，表示清空）。
func (s *Store) RenameAgentHost(ctx context.Context, id int64, name string) (*AgentHost, error) {
	res, err := s.stmtRenameAgentHost.ExecContext(ctx, name, fmtTime(time.Now()), id)
	if err != nil {
		return nil, fmt.Errorf("重命名主机 %d: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("重命名主机 %d: %w", id, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAgentHost(ctx, id)
}

// SetAgentHostState 写回一次会话的整组观测值；行不存在返回 ErrNotFound。
func (s *Store) SetAgentHostState(ctx context.Context, st AgentHostState) (*AgentHost, error) {
	if st.Status != AgentHostStatusReady && st.Status != AgentHostStatusError {
		return nil, fmt.Errorf("更新主机 %d: 非法状态 %q", st.ID, st.Status)
	}
	at := st.CheckedAt
	if at.IsZero() {
		at = time.Now()
	}
	ts := fmtTime(at)
	res, err := s.stmtSetAgentHostState.ExecContext(ctx,
		st.HostKey, st.KeyFingerprint, boolToInt(st.SudoNoPasswd), st.System,
		st.Status, st.LastError, ts, ts, st.ID)
	if err != nil {
		return nil, fmt.Errorf("更新主机 %d: %w", st.ID, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("更新主机 %d: %w", st.ID, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAgentHost(ctx, st.ID)
}

// SetAgentHostDevd 写回一次安装 / 检查守护进程的整组观测值；行不存在返回 ErrNotFound。
// 状态只能是 ready / error：「没装」由 ClearAgentHostDevd 表达。
func (s *Store) SetAgentHostDevd(ctx context.Context, st AgentHostDevdState) (*AgentHost, error) {
	if st.Status != DevdStatusReady && st.Status != DevdStatusError {
		return nil, fmt.Errorf("更新主机 %d 守护进程: 非法状态 %q", st.ID, st.Status)
	}
	at := st.CheckedAt
	if at.IsZero() {
		at = time.Now()
	}
	ts := fmtTime(at)
	res, err := s.stmtSetAgentHostDevd.ExecContext(ctx,
		st.Version, st.Home, boolToInt(st.Tmux), st.Status, st.LastError, ts, ts, st.ID)
	if err != nil {
		return nil, fmt.Errorf("更新主机 %d 守护进程: %w", st.ID, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("更新主机 %d 守护进程: %w", st.ID, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAgentHost(ctx, st.ID)
}

// ClearAgentHostDevd 把守护进程的观测值全部归零（卸载之后）；行不存在返回 ErrNotFound。
func (s *Store) ClearAgentHostDevd(ctx context.Context, id int64) (*AgentHost, error) {
	res, err := s.stmtClearAgentHostDevd.ExecContext(ctx, fmtTime(time.Now()), id)
	if err != nil {
		return nil, fmt.Errorf("清除主机 %d 守护进程: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("清除主机 %d 守护进程: %w", id, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAgentHost(ctx, id)
}

// DeleteAgentHost 删一行；不存在返回 ErrNotFound。设备上不留该主机的任何痕迹，
// 主机上那把公钥由调用方在删行前尽力移除（失败不拦删除，见 internal/agenthost）。
func (s *Store) DeleteAgentHost(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteAgentHost.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("删除主机 %d: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删除主机 %d: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanAgentHost(row interface{ Scan(...any) error }) (*AgentHost, error) {
	var (
		h             AgentHost
		sudo          int64
		tmux          int64
		checkedAt     sql.NullString
		devdCheckedAt sql.NullString
		createdAt     string
		updatedAt     string
	)
	if err := row.Scan(&h.ID, &h.Name, &h.Kind, &h.Address, &h.Port, &h.Username, &h.HostKey,
		&h.KeyFingerprint, &sudo, &h.System, &h.Status, &h.LastError,
		&checkedAt, &createdAt, &updatedAt,
		&h.Devd.Version, &h.Devd.Home, &tmux, &h.Devd.Status, &h.Devd.LastError, &devdCheckedAt); err != nil {
		return nil, err
	}
	h.SudoNoPasswd = sudo != 0
	h.Devd.Tmux = tmux != 0
	if checkedAt.Valid && checkedAt.String != "" {
		t, err := parseTime(checkedAt.String)
		if err != nil {
			return nil, fmt.Errorf("解析检查时刻: %w", err)
		}
		h.LastCheckedAt = t
	}
	if devdCheckedAt.Valid && devdCheckedAt.String != "" {
		t, err := parseTime(devdCheckedAt.String)
		if err != nil {
			return nil, fmt.Errorf("解析守护进程检查时刻: %w", err)
		}
		h.Devd.CheckedAt = t
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("解析创建时刻: %w", err)
	}
	h.CreatedAt = t
	if t, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("解析更新时刻: %w", err)
	}
	h.UpdatedAt = t
	return &h, nil
}
