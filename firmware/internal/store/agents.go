package store

// agent_accounts 仓储：Agents（Codex / Grok Build / Claude Code / Cursor 订阅
// 代理）的账号与凭据（迭代 11 Phase 1，Claude 于 2026-08-14 接入）。
// 约定同 repo.go：context 化、走 prepared statement、未命中 → ErrNotFound、
// 唯一性冲突 → ErrConflict、时间入库经 fmtTime。
//
// 两个视图，与 upstreams 同一分法（管理视图 Upstream / 路由视图 ModelRoute）：
//
//   - **管理视图** [AgentAccount]（[Store.ListAgentAccounts] / [Store.GetAgentAccount]）：
//     provider/label/account_id/default_model/status/last_refresh_at。密文列
//     根本不进 SELECT——不是"取了再抹掉"，是压根没读上来。
//   - **取凭据视图** [Store.GetAgentCredential]：解封后的规范凭据明文（Codex/Grok
//     是 auth.json，Claude 是 setup-token 包装，Cursor 是 Dashboard API Key
//     包装）。只在刷新或代理注入的进程内路径上出现。
//
// 单账户语义：一个 provider 一行（UNIQUE(provider)），重复连接是覆盖而非增行。
// 代价（决策 2 已接受）：订阅代理没有多来源故障切换。
//
// §15.1：Agent 凭据（auth.json 或 Claude setup-token）与它的密文绝不进 API 响应、日志、
// 审计 detail。本文件的错误只含 id/provider/状态与补救动作，两侧都不回显。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Agent provider 的封闭词汇表（`provider` 列刻意无 CHECK——0011 迁移的设计
// 预付，加一个 provider 不该是一次重建；词汇表由管理面校验把守）。
const (
	// AgentProviderCodex：OpenAI Codex 订阅（docs/firmware-agents-codex.md）。
	AgentProviderCodex = "codex"
	// AgentProviderGrok：xAI Grok Build 订阅（docs/firmware-agents-grok.md）。
	AgentProviderGrok = "grok"
	// AgentProviderClaude：Anthropic Claude Code setup-token 订阅代理
	//（docs/firmware-agents-claude.md）。
	AgentProviderClaude = "claude"
	// AgentProviderCursor：Cursor 订阅（管理员粘贴 cursor.com/dashboard 签发的
	// API Key，设备逐请求换发上游 token 后按 aiserver.v1 命名空间透明转发；
	// 凭据形态见 internal/cursorauth）。
	AgentProviderCursor = "cursor"
)

// agent_accounts.status 的封闭词汇表（与 0011 迁移的 CHECK 同步维护）。
const (
	// AgentStatusActive：凭据在手且可用，/v1/responses 走这一条。
	AgentStatusActive = "active"
	// AgentStatusDisabled：管理员停用。凭据还在，只是不许用。
	AgentStatusDisabled = "disabled"
	// AgentStatusAuthExpired：上游确定性拒绝凭据（Codex/Grok 的刷新
	// invalid_grant，或 Claude 请求的 401）。自动重试到此为止，要管理员
	// 重新连接才能回 active（决策 5）。
	AgentStatusAuthExpired = "auth_expired"
)

// ErrAgentAuthUnreadable 表示 Agents 凭据密文解不开（设备密钥被替换或密文
// 损坏）。补救动作是在管理台重新连接一次订阅——同 ErrKeyUnreadable 之于上游
// Key。只有明确要拿明文的 [Store.GetAgentCredential] 返回它；管理视图对同一
// 情况照常列出（那一行的状态字段仍然可读，没有理由整页报错）。
var ErrAgentAuthUnreadable = errors.New("store: Agents 凭据不可解密")

// agentAccountColumns 是**管理视图**的 SELECT 列表（与 scanAgentAccount 的扫描
// 顺序一一对应，改任一侧必须同步另一侧）。密文列不在其中，是有意的。
const agentAccountColumns = `id, provider, label, account_id, default_model, status,
	last_refresh_at, created_at, updated_at`

// AgentAccount 是 agent_accounts 表的一行（管理视图）。
type AgentAccount struct {
	ID       int64
	Provider string // AgentProviderCodex | AgentProviderGrok | AgentProviderClaude | AgentProviderCursor
	Label    string
	// AccountID 是 OpenAI 侧账号标识（id_token 的 claim）。明文字段，不是密钥
	// 物料：代理注入 ChatGPT-Account-ID 用它，管理台靠它认账号。
	AccountID string
	// DefaultModel 按 provider 有两种语义（都由界面文案分辨，共用这一列）：
	// codex/grok 是启动器命令写进 CLI 配置的默认模型名；claude 是「对成员
	// 可见的模型」，模型发现只透出同名一项。Cursor 模型选择由客户端负责，
	// 这一列对 cursor 恒为空；模型由 cursor-agent 经订阅面自行发现和选择。
	// 空 = 未设/不收窄。
	DefaultModel string
	Status       string // AgentStatusActive | AgentStatusDisabled | AgentStatusAuthExpired
	// AuthJSONSealed 是该 provider 规范凭据的密文。**管理读回恒留空**——只有
	// [Store.GetAgentCredential] 这条取令牌路径会填它。json:"-" 是最后一道闸：
	// 即便有人把本结构直接塞进 API 响应，密文也不会出去。同理，本结构不得以
	// %+v/%#v 打印（§15.1）。
	AuthJSONSealed string `json:"-"`
	// LastRefreshAt 是最近一次令牌刷新成功的时刻；零值 = 从未刷新过。
	LastRefreshAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// LogValue 让本结构进 slog 时只吐出 id/provider/状态（§15.1 自遮蔽，照
// boardinfo.SIDValue 的先例：「keep it that way rather than passing raw strings
// around」）。
//
// 要挡的是**唯一一条填了密文的路径**——[Store.GetAgentCredential] 的返回值。
// 它的调用方（网关代理、管理面自检）各自都持有 logger，一次顺手的
// slog.Any("account", acct) 就会把整份 auth.json 密文写进日志。管理视图那条路
// 密文列压根不进 SELECT，所以遮蔽对它是空操作，白拿。
//
// **只加 LogValue，不加 String()/MarshalJSON()**：json:"-" 已经把序列化那条路
// 封死了，而后两者会改掉本结构在管理面与测试里的既有格式化行为——那条路本就
// 没有密文可漏，为它改行为是净损失。
func (a AgentAccount) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", a.ID),
		slog.String("provider", a.Provider),
		slog.String("status", a.Status),
	)
}

// NewAgentAccount 是 [Store.UpsertAgentAccount] 的入参。同型 string 字段多，
// 用具名结构体而非位置参数（同 NewUser 的理由）。
type NewAgentAccount struct {
	Provider string
	// Label/DefaultModel 为空 = **保持既有值**（见 UpsertAgentAccount 的
	// "空即保持"语义）。
	Label string
	// AccountID 跟着凭据走，**无条件覆盖**（空就是空）——它是 id_token 的 claim，
	// 与 AuthJSON 必须同进同出，理由见 UpsertAgentAccount。
	AccountID    string
	DefaultModel string
	// AuthJSON 是该 provider 规范凭据明文，由本方法密封后入库；**不得为空**
	// （一行的存在即"句柄在手"，见 UpsertAgentAccount）。
	AuthJSON string
	// Status 为空取 AgentStatusActive——连接成功是这条路唯一的调用场景。
	Status string
	// CreatedAt 零值取当前时间（照 NewAIGCTask 先例；测试与固定时间入库用）。
	CreatedAt time.Time
}

// UpsertAgentAccount 落一份订阅凭据并返回该行（管理视图）。同一 provider 重复
// 连接是**覆盖而非增行**（UNIQUE(provider)，决策 3 的单账户语义）。
//
// 覆盖语义分两组，分界线是**这个值是不是从凭据里来的**：
//
//   - 凭据、AccountID 与状态**无条件覆盖**——这就是重新登录要达到的效果。
//     AccountID 是 id_token 的 claim，必须与凭据同进同出：换一个 ChatGPT 账号
//     重连、而 claim 又恰好解不出来（粘贴的 auth.json 没有可用 id_token）时，
//     若"空即保持"就会留下甲账号的 AccountID 配乙账号的 token——代理注入
//     ChatGPT-Account-ID 的那一刻必 401，管理台却还显示着甲。宁可空着。
//   - Label/DefaultModel **空即保持**：重新登录（或粘贴 auth.json 兜底）通常
//     不带这两项，若照空值覆盖，管理员设过的默认模型会被每一次重登悄悄抹掉。
//     要显式改这两项走管理面的更新方法（迭代 11 Phase 4 的 PATCH），不走本方法。
//   - LastRefreshAt 不动：重新登录不是一次刷新。
func (s *Store) UpsertAgentAccount(ctx context.Context, na NewAgentAccount) (*AgentAccount, error) {
	if na.Provider == "" {
		return nil, fmt.Errorf("连接 Agents 账号: provider 不能为空")
	}
	// 空凭据不许建行：一行的存在就意味着"这个 provider 的句柄在盒子手里"。
	// 允许空会造出一台状态 active、代理面据此宣称"可用"、到取令牌那一步才失败
	// 的账号——形态校验（access+refresh 在不在）归 codexauth，但"有没有"归这里。
	if na.AuthJSON == "" {
		return nil, fmt.Errorf("连接 Agents 账号: 凭据不能为空")
	}
	// Cursor 只观察请求模型用于记账，目录、连接与运行时配置都没有可执行的
	// 默认模型语义。入口层也拒绝设置；仓储再归零一次，避免内部调用重新造出
	// 一个看似可配置、实际上不会生效的字段。
	if na.Provider == AgentProviderCursor {
		na.DefaultModel = ""
	}
	status := na.Status
	if status == "" {
		status = AgentStatusActive
	}
	if !validAgentStatus(status) {
		return nil, fmt.Errorf("连接 Agents 账号: 非法状态 %q", status)
	}
	sealed, err := s.sealAgent(na.AuthJSON, na.Provider)
	if err != nil {
		return nil, fmt.Errorf("连接 Agents 账号: %w", err)
	}
	at := na.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	ts := fmtTime(at)
	if _, err := s.stmtUpsertAgentAccount.ExecContext(ctx,
		na.Provider, na.Label, na.AccountID, na.DefaultModel, sealed, status, ts, ts); err != nil {
		return nil, fmt.Errorf("连接 Agents 账号: %w", mapErr(err))
	}
	// 覆盖分支的"空即保持"发生在 SQL 里，返回值必须回读而不是就地拼装。
	acct, err := scanAgentAccount(s.stmtGetAgentAccountByProvider.QueryRowContext(ctx, na.Provider), false)
	if err != nil {
		return nil, fmt.Errorf("连接 Agents 账号: %w", err)
	}
	return acct, nil
}

// ListAgentAccounts 列出全部订阅账号（管理视图，无密文、无 auth.json）。
func (s *Store) ListAgentAccounts(ctx context.Context) ([]AgentAccount, error) {
	rows, err := s.stmtListAgentAccounts.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出 Agents 账号: %w", err)
	}
	defer rows.Close()

	var out []AgentAccount
	for rows.Next() {
		acct, err := scanAgentAccount(rows, false)
		if err != nil {
			return nil, fmt.Errorf("列出 Agents 账号: %w", err)
		}
		out = append(out, *acct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出 Agents 账号: %w", err)
	}
	return out, nil
}

// GetAgentAccount 按 id 取一行（管理视图，无密文）。
func (s *Store) GetAgentAccount(ctx context.Context, id int64) (*AgentAccount, error) {
	acct, err := scanAgentAccount(s.stmtGetAgentAccountByID.QueryRowContext(ctx, id), false)
	if err != nil {
		return nil, fmt.Errorf("查询 Agents 账号: %w", err)
	}
	return acct, nil
}

// GetAgentCredential 按 provider 取一行**并解开 auth.json**，供刷新令牌与代理
// 注入请求头的进程内路径使用。返回的明文不得进日志/审计/API 响应（§15.1）。
//
// 状态不过滤是有意的：调用方要按状态分岔出不同的机读原因（无行 →
// agent_not_configured，auth_expired → agent_auth_expired），在 SQL 里滤掉行
// 就把两种情况压成了同一种。密文解不开返回 ErrAgentAuthUnreadable——那与"没
// 连过"是两件事，补救动作也不同。
func (s *Store) GetAgentCredential(ctx context.Context, provider string) (*AgentAccount, string, error) {
	acct, err := scanAgentAccount(s.stmtGetAgentCredential.QueryRowContext(ctx, provider), true)
	if err != nil {
		return nil, "", fmt.Errorf("读取 Agents 凭据: %w", err)
	}
	authJSON, err := s.openAgent(acct.AuthJSONSealed, provider)
	if err != nil {
		return nil, "", err
	}
	return acct, authJSON, nil
}

// SetAgentAuthJSON 把刷新得到的**新一代** auth.json 重新密封落库，并盖上
// last_refresh_at。轮换型 refresh token 单次消费，不落库则重启后拿的是已作废
// 的那一份，必 401（决策 5）。
//
// provider 进 WHERE 而不只是拿来算 AAD：这样"用 A 的 AAD 封的密文写进 B 的行"
// 在 SQL 层就落不下去——AAD 防的正是这件事，别让写入路径自己绕过它。
// 状态不动：刷新成功不该把管理员停用的行改回 active（回 active 是重新登录或
// 粘贴导入的语义，见 UpsertAgentAccount）。
func (s *Store) SetAgentAuthJSON(ctx context.Context, id int64, provider, authJSON string) error {
	// 同 UpsertAgentAccount：空凭据不是"清空"而是把句柄弄丢了，拒绝落库。
	// 要撤掉一份订阅走 DeleteAgentAccount，要停用走 SetAgentStatus。
	if authJSON == "" {
		return fmt.Errorf("更新 Agents 凭据: 凭据不能为空")
	}
	sealed, err := s.sealAgent(authJSON, provider)
	if err != nil {
		return fmt.Errorf("更新 Agents 凭据: %w", err)
	}
	now := fmtTime(time.Now())
	res, err := s.stmtSetAgentAuthJSON.ExecContext(ctx, sealed, now, now, id, provider)
	return execOneRow(res, err, "更新 Agents 凭据")
}

// UpdateAgentAccount 改管理员设的那两项（label、default_model）。
//
// 与 [Store.UpsertAgentAccount] 的「空即保持」**语义相反**，这条路上空串就是
// 清空——两条路都得存在：重新登录不带这两项时不该抹掉管理员的配置（那是
// Upsert 的口径），而管理员在管理台把名字删干净时也必须真的删得掉（这一条）。
// 少了它，"清空 label" 在整个系统里无从表达。
//
// 凭据与状态一概不动：改名不是重新登录，也不是启停。
func (s *Store) UpdateAgentAccount(ctx context.Context, id int64, label, defaultModel string) error {
	res, err := s.stmtUpdateAgentAccount.ExecContext(ctx, label, defaultModel, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新 Agents 账号")
}

// SetAgentStatus 改行状态（三值之一）。刷新被确定性拒绝时标 auth_expired、
// 管理员停用标 disabled、重新登录成功回 active（后者由 UpsertAgentAccount 顺带
// 完成）。非法状态在这里就拒，不留给 CHECK 报一句读不懂的约束错。
func (s *Store) SetAgentStatus(ctx context.Context, id int64, status string) error {
	if !validAgentStatus(status) {
		return fmt.Errorf("更新 Agents 状态: 非法状态 %q", status)
	}
	res, err := s.stmtSetAgentStatus.ExecContext(ctx, status, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新 Agents 状态")
}

// DeleteAgentAccount 删除一行（连同密文）。删除后 /v1/responses 立刻回
// agent_not_configured——取账号是每请求点查，没有缓存要失效。
func (s *Store) DeleteAgentAccount(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteAgentAccount.ExecContext(ctx, id)
	return execOneRow(res, err, "删除 Agents 账号")
}

// validAgentStatus 判定状态是否在封闭词汇表内。
func validAgentStatus(status string) bool {
	switch status {
	case AgentStatusActive, AgentStatusDisabled, AgentStatusAuthExpired:
		return true
	default:
		return false
	}
}

// sealAgent 把整份 auth.json 封存为入库文本，AAD 钉 "agent:<provider>"：
// codex 的密文抄进 claude 的行解不开，而不是被悄悄当成 claude 的凭据用。
func (s *Store) sealAgent(authJSON, provider string) (string, error) {
	sealed, err := s.seal(authJSON, agentAAD(provider))
	if err != nil {
		return "", fmt.Errorf("封存 Agents 凭据: %w", err)
	}
	return sealed, nil
}

// openAgent 解出 auth.json 明文。解不开返回 ErrAgentAuthUnreadable + 补救动作，
// 错误文本不含密文也不含明文。
//
// **空密文与解不开同一处置**（这里不走 open 的"空串解出空串"）：写入两侧都拒了
// 空凭据，所以一行存在就意味着句柄在手；真出现空密文（手改过的库、将来某个忘了
// 封存那一步的写入方）时，返回一份"成功的空凭据"会把故障推迟到 ParseAuthJSON
// 或者一次没有 Authorization 的上游请求上，离病因十万八千里。
func (s *Store) openAgent(sealed, provider string) (string, error) {
	if sealed == "" {
		return "", fmt.Errorf("%w：该行没有凭据（密文列为空），请在管理台重新连接该订阅",
			ErrAgentAuthUnreadable)
	}
	authJSON, err := s.open(sealed, agentAAD(provider))
	if err != nil {
		return "", fmt.Errorf("%w：%s 与密文不匹配或密文损坏（%s 被替换、或凭据被写到了别的 provider 行上），"+
			"请在管理台重新连接该订阅", ErrAgentAuthUnreadable, DeviceKeyFileName, DeviceKeyFileName)
	}
	return authJSON, nil
}

// agentAAD 是 agent_accounts 密文的附加认证数据。
func agentAAD(provider string) []byte { return []byte("agent:" + provider) }

// scanAgentAccount 从行扫描 AgentAccount（列序与 agentAccountColumns 一一对应）；
// withSealed 为真时额外扫描末尾的密文列（取令牌路径专用）。未命中 → ErrNotFound。
func scanAgentAccount(r rowScanner, withSealed bool) (*AgentAccount, error) {
	var (
		acct             AgentAccount
		lastRefresh      sql.NullString
		created, updated string
	)
	dest := []any{&acct.ID, &acct.Provider, &acct.Label, &acct.AccountID, &acct.DefaultModel,
		&acct.Status, &lastRefresh, &created, &updated}
	if withSealed {
		dest = append(dest, &acct.AuthJSONSealed)
	}
	err := r.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastRefresh.Valid {
		if acct.LastRefreshAt, err = parseTime(lastRefresh.String); err != nil {
			return nil, fmt.Errorf("last_refresh_at 非法: %w", err)
		}
	}
	if acct.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if acct.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("updated_at 非法: %w", err)
	}
	return &acct, nil
}
