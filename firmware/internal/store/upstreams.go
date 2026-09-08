package store

// 上游账户与模型目录三表（upstreams / models / model_sources）的仓储方法。
// 约定同 repo.go：context 化、走 prepared statement、未命中 → ErrNotFound、
// 唯一性/引用冲突 → ErrConflict、时间入库经 fmtTime。
//
// 两类视图刻意分开（决策 2 的 §15.1 边界）：
//
//   - 管理视图 [Upstream] / [ModelWithSources]：只带凭证末 4 位，永不携带明文；
//   - 路由视图 [ModelRoute]：带解密后的凭证明文，只允许流向出站请求头，
//     绝不进日志、审计 detail 或管理 API 响应。
//
// 路由每请求点查（决策 3，延续 iteration-4 的无缓存先例）：启停、优先级、
// 换 Key 都在下一个请求即时生效，代价是每请求一次三表联查。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Upstream 是 upstreams 表的一行——管理视图，不含凭证明文。
type Upstream struct {
	ID   int64
	Name string
	Type string // 与 config.Upstream* 常量同集；兼容适配器含 openai_compat / anthropic_compat
	// CatalogID 是平台模型数据里的具体平台身份；迁移前账号按 Type 补齐。
	// BillingMode 是创建账号时从目录快照的调度语义，不随后续数据升级改写。
	CatalogID   string
	BillingMode string
	// APIKeyLast4 由密文解出后取末 4 位（见 last4）。空串有三种含义：该上游
	// 无凭证（mock）、凭证过短不足以安全展示、或设备密钥与密文不匹配——
	// 最后一种是"需在管理台重新录入 Key"的信号，不阻断列表可读性。
	APIKeyLast4 string
	// BaseURL：mock 与两种 compat 的必填端点根（无内置端点表条目）、minimax
	// 的站点选择；其余产品上游为空串（dev 覆盖除外）。
	BaseURL  string
	Disabled bool
	// EgressMode 是该账号的出站方式覆盖（internal/egress）：inherit 跟随设备级
	// 「模型与订阅接口」出口，direct / proxy 强制。表 DEFAULT 'inherit'。
	EgressMode string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// 模型种类（models.kind 的 CHECK 约束同步维护）。契约：数据面入口按 kind
// 选路——文本入口只收 text、视频/图片入口同理，kind 不匹配一律
// model_not_found（gateway.resolveRoute 的 kind 闸门）。本包保证 kind 事实
// 随各视图透出，且两条 servable 读数（/v1/models 与使用API页）恒为文本口径
// （谓词钉死 kind=text）。
const (
	ModelKindText  = "text"
	ModelKindVideo = "video"
	ModelKindImage = "image"
)

// modelColumns 是 models 全列的 SELECT 列表（与 scanModel 及各联查里的内联
// 扫描顺序一一对应，改任一处必须同步全部）。modelColumnsPrefixed 是它带
// `m.` 别名的联查版本。
const modelColumns = `id, name, kind, family, pricing, disabled, entry_openai, entry_responses, entry_anthropic, created_at, updated_at`
const modelColumnsPrefixed = `m.id, m.name, m.kind, m.family, m.pricing, m.disabled, m.entry_openai, m.entry_responses, m.entry_anthropic, m.created_at, m.updated_at`

// Model 是 models 表的一行。Name 即客户端可见的原始模型名。
type Model struct {
	ID   int64
	Name string
	// Kind 建后不可改（改 kind 等于换模型，照上游 type 先例）：本包不提供
	// 任何改写 kind 的方法，管理 API 对改动回 kind_immutable。
	Kind string // ModelKindText | ModelKindVideo | ModelKindImage
	// Family 是 AIGC 模型声明的协议面（0014，值即厂商协议面的协议标识
	// minimax_video|ark_video|ark_image；text 恒空串）。建模时声明、建后不可改
	// ——本包只有带 family='' 守卫的写方法（SetModelFamilyIfUnset），管理 API
	// 对改动回 family_immutable。空串 = 存量未声明（迁移回填不到的无来源 video
	// 行），由第一条来源懒钉，行为与旧「首源钉族」一致。
	Family string
	// Pricing 是该模型的目录价 JSON（**形态定字段**：文本三价 / Seedance 两档 /
	// H3 秒价档+附加 / 图片张价），值一律整数微元。空串 = 未定价——照常转发、
	// 金额记 0、管理台挂警示徽章；与显式 0 价（定价为免费）是两个状态。
	// 入库前经 validatePricing 保证是一张扁平的非负整数表；形态字段集按 kind
	// 校验是管理层的事。计价函数（internal/usage）是它唯一的读取方。
	Pricing  string
	Disabled bool
	// EntryOpenAI / EntryResponses / EntryAnthropic 是协议面开关：仅 kind=text 有
	// 语义（kind 闸门先于入口开关，视频/图片入口不读它们），默认全开。
	// 管理层保证 text 模型至少开一个；选路对关掉的入口不组候选
	// （gateway.resolveRoute），两条 servable 读数的谓词同步排除全关的行。
	EntryOpenAI    bool
	EntryResponses bool
	EntryAnthropic bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ModelSource 是 model_sources 表的一行：模型挂到某个上游的一条来源。
// UpstreamModelID 为空表示"与模型名相同"——管理视图保留这个原始空值（UI 用
// 模型名做占位符），路由视图由 ResolveModelRoute 解析成实际值。
// Priority 小者优先，同值按 ID（创建序）。预留列 quota 本迭代无读写语义
// （决策 7），故不出现在本结构体里。
type ModelSource struct {
	ID              int64
	ModelID         int64
	UpstreamID      int64
	UpstreamModelID string
	Priority        int64
	Disabled        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ModelSourceDetail 是来源行加上其上游的展示字段（管理 API 的嵌套视图）。
// UpstreamBaseURL 随行带回（算 protocols 徽标要它——mock 靠 base_url 才有
// 端点），管理面不必为它再拉一次上游列表；凭证仍然不在内。
type ModelSourceDetail struct {
	ModelSource
	UpstreamName        string
	UpstreamType        string
	UpstreamCatalogID   string
	UpstreamBillingMode string
	UpstreamBaseURL     string
	UpstreamDisabled    bool
	// UpstreamEgressMode 随行带回，让管理台在同一模型混合直连/代理来源时给出提示。
	UpstreamEgressMode string
}

// ModelWithSources 是模型行加上它的全部来源（含已停用的），
// 来源按 (priority, 订阅先行, id) 排序（见 billingRankSQL）。
type ModelWithSources struct {
	Model
	Sources []ModelSourceDetail
}

// RouteUpstream 是路由候选携带的上游事实集，APIKey 是解密后的明文——
// 只允许注入出站请求头，绝不进日志、审计或管理响应（§15.1）。
type RouteUpstream struct {
	ID          int64
	Name        string
	Type        string
	CatalogID   string
	BillingMode string
	APIKey      string
	BaseURL     string
	Disabled    bool
	// EgressMode 是该账号的出站方式覆盖（inherit|direct|proxy），数据面装配请求时带进 ctx。
	EgressMode string
}

// RouteCandidate 是一条候选来源。UpstreamModelID 已解析：库中为空时取模型名。
type RouteCandidate struct {
	SourceID        int64
	Priority        int64
	UpstreamModelID string
	SourceDisabled  bool
	Upstream        RouteUpstream
}

// ModelRoute 是 ResolveModelRoute 的结果：模型行加上全部候选来源（含停用的），
// 候选按 (priority, 订阅先行, id) 排序（见 billingRankSQL）。
// 三层 disabled 位（模型 / 来源 / 上游）都在结果里，
// 由调用方按入口口径裁决——模型停用或无任何启用来源 → model_not_found；
// 有启用来源但都不支持本入口协议 → protocol_mismatch（决策 4）。
type ModelRoute struct {
	Model      Model
	Candidates []RouteCandidate
}

// SourceRoute 是单条来源的探测视图（管理面「测试来源」用）：模型行 + 该来源
// 的路由事实（含解密后的上游凭证）。与 ResolveModelRoute 同属路由视图，
// APIKey 只允许流向出站请求头，绝不进日志、审计 detail 或管理 API 响应。
type SourceRoute struct {
	Model     Model
	Candidate RouteCandidate
}

// ---- upstreams ----

// CreateUpstream 建上游账户并返回管理视图的完整行。apiKey 明文经设备密钥
// 封存后入库，调用方拿不回明文（只有 APIKeyLast4）；重名返回 ErrConflict，
// 非法 type 由表 CHECK 约束拒绝。
func (s *Store) CreateUpstream(ctx context.Context, name, typ, apiKey, baseURL string) (*Upstream, error) {
	return s.CreateCatalogUpstream(ctx, name, typ, typ, defaultBillingMode(typ), apiKey, baseURL)
}

// CreateCatalogUpstream 建一条由平台目录选择的账号。catalogID/billingMode 与
// baseURL 都在管理员录入 Key 的同一次创建里快照，之后目录更新不改这三项。
func (s *Store) CreateCatalogUpstream(ctx context.Context, name, typ, catalogID, billingMode, apiKey, baseURL string) (*Upstream, error) {
	sealed, err := s.sealKey(apiKey)
	if err != nil {
		return nil, fmt.Errorf("创建上游: %w", err)
	}
	ts := fmtTime(time.Now())
	res, err := s.stmtCreateUpstream.ExecContext(ctx, name, typ, catalogID, billingMode, sealed, baseURL, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("创建上游: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("创建上游: %w", err)
	}
	t, _ := parseTime(ts)
	return &Upstream{
		ID: id, Name: name, Type: typ, CatalogID: catalogID, BillingMode: billingMode,
		APIKeyLast4: last4(apiKey), BaseURL: baseURL, EgressMode: "inherit",
		CreatedAt: t, UpdatedAt: t,
	}, nil
}

func defaultBillingMode(typ string) string {
	switch typ {
	case "ark_plan", "qwen_plan", "opencode_go":
		return "subscription"
	case "deepseek", "ark", "minimax":
		return "usage"
	default:
		return "none"
	}
}

// ListUpstreams 按 id 升序返回全部上游（含已停用的）。
func (s *Store) ListUpstreams(ctx context.Context) ([]Upstream, error) {
	rows, err := s.stmtListUpstreams.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出上游: %w", err)
	}
	defer rows.Close()
	var ups []Upstream
	for rows.Next() {
		u, err := s.scanUpstream(rows)
		if err != nil {
			return nil, fmt.Errorf("列出上游: %w", err)
		}
		ups = append(ups, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出上游: %w", err)
	}
	return ups, nil
}

// GetUpstreamByID 按主键取上游；未命中返回 ErrNotFound。
func (s *Store) GetUpstreamByID(ctx context.Context, id int64) (*Upstream, error) {
	u, err := s.scanUpstream(s.stmtGetUpstreamByID.QueryRowContext(ctx, id))
	if err != nil {
		return nil, fmt.Errorf("查询上游: %w", err)
	}
	return u, nil
}

// GetRouteUpstreamByID 按主键取路由视图的上游（解密后的凭证明文，只允许流向
// 出站请求头，绝不进日志、审计 detail 或管理 API 响应）。任务面（aigc_tasks，
// 迭代 8）的查询/取消/下载按任务行钉死的 upstream_id 走这里——**disabled 位
// 有意放行、原样带回**：停用只挡新提交的选路，已受理的异步任务必须始终可查
// 可取消，冻结查询侧只会把已付费的任务困成孤儿（与 GetSourceRoute 无视启停
// 位同一理由）。凭证解不开返回 ErrKeyUnreadable（拿空凭证去上游只会换回误导
// 性的 401）；未命中返回 ErrNotFound。
func (s *Store) GetRouteUpstreamByID(ctx context.Context, id int64) (*RouteUpstream, error) {
	var (
		u        RouteUpstream
		sealed   string
		disabled int
	)
	err := s.stmtGetRouteUpstreamByID.QueryRowContext(ctx, id).
		Scan(&u.ID, &u.Name, &u.Type, &u.CatalogID, &u.BillingMode, &sealed, &u.BaseURL, &disabled, &u.EgressMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询上游路由: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查询上游路由: %w", err)
	}
	u.Disabled = disabled != 0
	if u.APIKey, err = s.openKey(sealed); err != nil {
		// openKey 的错误文本已含补救指引且不含密钥物料，包上哨兵供调用方分流。
		return nil, fmt.Errorf("查询上游路由: %w: %v", ErrKeyUnreadable, err)
	}
	return &u, nil
}

// UpdateUpstream 改上游的 name 与 base_url（type 不可改——建错重建，
// 决策 8）；重名返回 ErrConflict，目标不存在返回 ErrNotFound。
func (s *Store) UpdateUpstream(ctx context.Context, id int64, name, baseURL string) error {
	res, err := s.stmtUpdateUpstream.ExecContext(ctx, name, baseURL, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新上游")
}

// UpdateUpstreamAddressAndKey 一条 UPDATE 同时改 name、base_url 与凭证
// （明文经设备密钥封存）。openai_compat 的改址路径专用：改址被允许的前提是
// 「封存的旧 Key 绝不发往管理员没为它录过 Key 的主机」，所以新地址与新 Key
// 必须原子落库——拆成两步，中间任何失败都会留下「旧 Key 配新地址」的组合，
// 数据面下一个请求就把旧 Key 发去新主机了。重名返回 ErrConflict，
// 目标不存在返回 ErrNotFound。
func (s *Store) UpdateUpstreamAddressAndKey(ctx context.Context, id int64, name, baseURL, apiKey string) error {
	sealed, err := s.sealKey(apiKey)
	if err != nil {
		return fmt.Errorf("更新上游地址与凭证: %w", err)
	}
	res, err := s.stmtUpdateUpstreamWithKey.ExecContext(ctx, name, baseURL, sealed, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新上游地址与凭证")
}

// SetUpstreamDisabled 置上游禁用位；目标不存在返回 ErrNotFound。
// 无缓存的每请求点查保证对数据面即时生效。
func (s *Store) SetUpstreamDisabled(ctx context.Context, id int64, disabled bool) error {
	res, err := s.stmtSetUpstreamDisabled.ExecContext(ctx, boolToInt(disabled), fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新上游禁用位")
}

// SetUpstreamKey 换上游凭证（明文经设备密钥封存后覆盖旧密文）；
// 目标不存在返回 ErrNotFound。空明文即清空凭证，由调用方决定是否允许。
func (s *Store) SetUpstreamKey(ctx context.Context, id int64, apiKey string) error {
	sealed, err := s.sealKey(apiKey)
	if err != nil {
		return fmt.Errorf("更新上游凭证: %w", err)
	}
	res, err := s.stmtSetUpstreamKey.ExecContext(ctx, sealed, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新上游凭证")
}

// SetUpstreamEgressMode 写该账号的出站方式覆盖（inherit|direct|proxy，合法值由表
// CHECK 约束兜底、管理层先校验）；目标不存在返回 ErrNotFound。每请求点查即时生效。
func (s *Store) SetUpstreamEgressMode(ctx context.Context, id int64, mode string) error {
	res, err := s.stmtSetUpstreamEgressMode.ExecContext(ctx, mode, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新上游出站方式")
}

// DeleteUpstream 删上游；仍被模型来源引用时（外键 RESTRICT）返回 ErrConflict
// 并给出可读原因，目标不存在返回 ErrNotFound。
func (s *Store) DeleteUpstream(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteUpstream.ExecContext(ctx, id)
	if err != nil {
		if errors.Is(mapErr(err), ErrConflict) {
			return fmt.Errorf("删除上游: 仍被模型来源引用，请先删除或改挂这些来源: %w", ErrConflict)
		}
		return fmt.Errorf("删除上游: %w", err)
	}
	return execOneRow(res, nil, "删除上游")
}

// ---- models ----

// CreateModel 建模型（name 即客户端可见的原始名；kind 必须显式给出且建后
// 不可改；pricing 空串 = 未定价）；重名返回 ErrConflict，非法 kind 由表 CHECK
// 约束拒绝，pricing 非法（非 JSON 对象 / 非整数 / 负数）在落库前即报错。
func (s *Store) CreateModel(ctx context.Context, name, kind, pricing string) (*Model, error) {
	if err := validatePricing(pricing); err != nil {
		return nil, fmt.Errorf("创建模型: %w", err)
	}
	ts := fmtTime(time.Now())
	res, err := s.stmtCreateModel.ExecContext(ctx, name, kind, pricing, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("创建模型: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("创建模型: %w", err)
	}
	t, _ := parseTime(ts)
	// 入口开关走表默认（全开），协议面走表默认（空串 = 未声明）；建时想关某个
	// 入口 / 声明协议面由管理层随后 SetModelEntries / SetModelFamilyIfUnset
	// ——新模型没有来源、进不了任何候选，这个两步窗口对数据面不可观测。
	return &Model{ID: id, Name: name, Kind: kind, Pricing: pricing, EntryOpenAI: true, EntryResponses: true, EntryAnthropic: true, CreatedAt: t, UpdatedAt: t}, nil
}

// SetModelFamilyIfUnset 声明模型的协议面（0014）：UPDATE 带 family = ” 守卫，
// 只写还未声明的行——「建后不可改」由此保证（本包没有无守卫的 family 写方法，
// 照 kind 无写方法的先例）。已声明为同值视为幂等成功（懒钉路径并发到达的
// 常态）；已声明为异值返回 ErrConflict；目标不存在返回 ErrNotFound。
// 合法值由管理层按 kind 圈定（kindProtocols），本层照单全收。
func (s *Store) SetModelFamilyIfUnset(ctx context.Context, id int64, family string) error {
	res, err := s.stmtSetModelFamily.ExecContext(ctx, family, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("声明模型协议面: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("声明模型协议面: %w", err)
	}
	if n == 1 {
		return nil
	}
	m, err := s.GetModelByID(ctx, id)
	if err != nil {
		return err // 含 ErrNotFound
	}
	if m.Family == family {
		return nil
	}
	return fmt.Errorf("声明模型协议面: 已声明为 %s: %w", m.Family, ErrConflict)
}

// GetModelByID 按主键取模型；未命中返回 ErrNotFound。
func (s *Store) GetModelByID(ctx context.Context, id int64) (*Model, error) {
	m, err := scanModel(s.stmtGetModelByID.QueryRowContext(ctx, id))
	if err != nil {
		return nil, fmt.Errorf("查询模型: %w", err)
	}
	return m, nil
}

// GetModelByName 按客户端可见名取模型（name 有 UNIQUE，至多一行）；未命中
// 返回 ErrNotFound。管理面「订阅接入 → 添加模型」的幂等路径用它认领同名行。
func (s *Store) GetModelByName(ctx context.Context, name string) (*Model, error) {
	m, err := scanModel(s.stmtGetModelByName.QueryRowContext(ctx, name))
	if err != nil {
		return nil, fmt.Errorf("查询模型: %w", err)
	}
	return m, nil
}

// RenameModel 改模型名（改的就是客户端可见名，调用方需知会客户端）；
// 重名返回 ErrConflict，目标不存在返回 ErrNotFound。
func (s *Store) RenameModel(ctx context.Context, id int64, name string) error {
	res, err := s.stmtRenameModel.ExecContext(ctx, name, fmtTime(time.Now()), id)
	return execOneRow(res, err, "重命名模型")
}

// SetModelDisabled 置模型禁用位；目标不存在返回 ErrNotFound。
func (s *Store) SetModelDisabled(ctx context.Context, id int64, disabled bool) error {
	res, err := s.stmtSetModelDisabled.ExecContext(ctx, boolToInt(disabled), fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新模型禁用位")
}

// SetModelPricing 改模型目录价（空串 = 清为未定价）；非法 JSON 在落库前报错，
// 目标不存在返回 ErrNotFound。**记账时点价**：改价只影响其后的请求与其后的
// 任务清算，历史行的金额不重算——预算播种、图表与审计因此全部自洽。
func (s *Store) SetModelPricing(ctx context.Context, id int64, pricing string) error {
	if err := validatePricing(pricing); err != nil {
		return fmt.Errorf("更新模型目录价: %w", err)
	}
	res, err := s.stmtSetModelPricing.ExecContext(ctx, pricing, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新模型目录价")
}

// SetModelEntries 写协议面开关（仅 text 有语义，管理层保证至少开
// 一个——本层照单全收，全关的行为等同三个协议面均返回 404，与停用一致且可恢复）。
func (s *Store) SetModelEntries(ctx context.Context, id int64, entryOpenAI, entryResponses, entryAnthropic bool) error {
	res, err := s.stmtSetModelEntries.ExecContext(ctx, boolToInt(entryOpenAI), boolToInt(entryResponses), boolToInt(entryAnthropic), fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新模型入口开关")
}

// DeleteModel 删模型，其全部来源随外键 ON DELETE CASCADE 一并删除；
// 目标不存在返回 ErrNotFound。
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteModel.ExecContext(ctx, id)
	return execOneRow(res, err, "删除模型")
}

// ---- model_sources ----

// CreateModelSource 给模型挂一条来源。upstreamModelID 空串表示与模型名相同；
// priority 小者优先。(model_id, upstream_id) 重复返回 ErrConflict；
// 模型或上游不存在由外键拒绝，同样映射为 ErrConflict。
func (s *Store) CreateModelSource(ctx context.Context, modelID, upstreamID int64, upstreamModelID string, priority int64) (*ModelSource, error) {
	ts := fmtTime(time.Now())
	res, err := s.stmtCreateModelSource.ExecContext(ctx, modelID, upstreamID, upstreamModelID, priority, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("创建模型来源: %w", mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("创建模型来源: %w", err)
	}
	t, _ := parseTime(ts)
	return &ModelSource{
		ID: id, ModelID: modelID, UpstreamID: upstreamID,
		UpstreamModelID: upstreamModelID, Priority: priority,
		CreatedAt: t, UpdatedAt: t,
	}, nil
}

// GetModelSourceByID 按主键取来源；未命中返回 ErrNotFound。
func (s *Store) GetModelSourceByID(ctx context.Context, id int64) (*ModelSource, error) {
	src, err := scanModelSource(s.stmtGetModelSourceByID.QueryRowContext(ctx, id))
	if err != nil {
		return nil, fmt.Errorf("查询模型来源: %w", err)
	}
	return src, nil
}

// UpdateModelSource 改来源的来源侧模型 ID 与优先级（上游归属不可改——
// 换上游即删了重挂）；目标不存在返回 ErrNotFound。
func (s *Store) UpdateModelSource(ctx context.Context, id int64, upstreamModelID string, priority int64) error {
	res, err := s.stmtUpdateModelSource.ExecContext(ctx, upstreamModelID, priority, fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新模型来源")
}

// SetModelSourceDisabled 置来源禁用位；目标不存在返回 ErrNotFound。
func (s *Store) SetModelSourceDisabled(ctx context.Context, id int64, disabled bool) error {
	res, err := s.stmtSetModelSourceDisabled.ExecContext(ctx, boolToInt(disabled), fmtTime(time.Now()), id)
	return execOneRow(res, err, "更新模型来源禁用位")
}

// DeleteModelSource 删来源；目标不存在返回 ErrNotFound。
func (s *Store) DeleteModelSource(ctx context.Context, id int64) error {
	res, err := s.stmtDeleteModelSource.ExecContext(ctx, id)
	return execOneRow(res, err, "删除模型来源")
}

// ---- 联查视图 ----

// ListModelsWithSources 返回全部模型（名字典序）及其全部来源（含停用的，
// 按 priority、id 排序），供管理 API 的嵌套列表使用。一次左联查取完，
// 无来源的模型也会出现（Sources 为 nil）。不含任何凭证明文。
func (s *Store) ListModelsWithSources(ctx context.Context) ([]ModelWithSources, error) {
	rows, err := s.stmtListModelsWithSources.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出模型: %w", err)
	}
	defer rows.Close()
	out, err := collectModelsWithSources(rows)
	if err != nil {
		return nil, fmt.Errorf("列出模型: %w", err)
	}
	return out, nil
}

// GetModelWithSources 按主键取单个模型的嵌套视图（同 ListModelsWithSources
// 的行形态与来源排序）；未命中返回 ErrNotFound。单模型的管理读写（PATCH/
// DELETE 的回显与审计）走这里，不必物化整个目录。
func (s *Store) GetModelWithSources(ctx context.Context, id int64) (*ModelWithSources, error) {
	rows, err := s.stmtGetModelWithSources.QueryContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("查询模型: %w", err)
	}
	defer rows.Close()
	out, err := collectModelsWithSources(rows)
	if err != nil {
		return nil, fmt.Errorf("查询模型: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("查询模型: %w", ErrNotFound)
	}
	return &out[0], nil
}

// collectModelsWithSources 把「模型 × 来源 × 上游」左联查的行折成嵌套视图，
// 行序按模型分组（同一模型的来源行相邻）。错误不带操作前缀，由调用方包装。
func collectModelsWithSources(rows *sql.Rows) ([]ModelWithSources, error) {
	var out []ModelWithSources
	for rows.Next() {
		var (
			m                                              Model
			modelDisabled                                  int
			mEntryOpenAI, mEntryResponses, mEntryAnthropic int
			mCreated, mUpdated                             string

			srcID, srcUpstreamID, srcPriority, srcDisabled sql.NullInt64
			srcUpstreamModelID, srcCreated, srcUpdated     sql.NullString

			upName, upType, upCatalogID, upBillingMode, upBaseURL, upEgress sql.NullString
			upDisabled                                                      sql.NullInt64
		)
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &m.Family, &m.Pricing, &modelDisabled, &mEntryOpenAI, &mEntryResponses, &mEntryAnthropic, &mCreated, &mUpdated,
			&srcID, &srcUpstreamID, &srcUpstreamModelID, &srcPriority, &srcDisabled, &srcCreated, &srcUpdated,
			&upName, &upType, &upCatalogID, &upBillingMode, &upBaseURL, &upDisabled, &upEgress); err != nil {
			return nil, err
		}
		var err error
		m.Disabled = modelDisabled != 0
		m.EntryOpenAI, m.EntryResponses, m.EntryAnthropic = mEntryOpenAI != 0, mEntryResponses != 0, mEntryAnthropic != 0
		if m.CreatedAt, err = parseTime(mCreated); err != nil {
			return nil, fmt.Errorf("created_at 非法: %w", err)
		}
		if m.UpdatedAt, err = parseTime(mUpdated); err != nil {
			return nil, fmt.Errorf("updated_at 非法: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].ID != m.ID {
			out = append(out, ModelWithSources{Model: m})
		}
		if !srcID.Valid { // 左联查的空来源行：模型尚未挂任何来源
			continue
		}
		src := ModelSourceDetail{
			ModelSource: ModelSource{
				ID:              srcID.Int64,
				ModelID:         m.ID,
				UpstreamID:      srcUpstreamID.Int64,
				UpstreamModelID: srcUpstreamModelID.String,
				Priority:        srcPriority.Int64,
				Disabled:        srcDisabled.Int64 != 0,
			},
			UpstreamName:        upName.String,
			UpstreamType:        upType.String,
			UpstreamCatalogID:   upCatalogID.String,
			UpstreamBillingMode: upBillingMode.String,
			UpstreamBaseURL:     upBaseURL.String,
			UpstreamEgressMode:  upEgress.String,
			UpstreamDisabled:    upDisabled.Int64 != 0,
		}
		if src.CreatedAt, err = parseTime(srcCreated.String); err != nil {
			return nil, fmt.Errorf("来源 created_at 非法: %w", err)
		}
		if src.UpdatedAt, err = parseTime(srcUpdated.String); err != nil {
			return nil, fmt.Errorf("来源 updated_at 非法: %w", err)
		}
		cur := &out[len(out)-1]
		cur.Sources = append(cur.Sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListModels 按名字典序返回全部模型行（不带来源）。只要模型自身字段的读方
// （官方价格同步）走这里，省下左联查与逐来源行的解析。
func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.stmtListModels.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出模型行: %w", err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("列出模型行: %w", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出模型行: %w", err)
	}
	return out, nil
}

// ResolveModelRoute 是数据面选路的热路径：按客户端请求的模型名一次联查三表，
// 返回模型行与全部候选来源（含停用的，按 priority、id 排序）。
// 模型不存在返回 ErrNotFound；模型存在但没挂来源时 Candidates 为空。
//
// 凭证只对**可能被选中**的候选解密（来源与上游都启用）：这类候选解不开即整体
// 报错而不是静默跳过——设备密钥被替换意味着凭证不可用，静默跳过只会把配置事故
// 伪装成 model_not_found。停用的候选反正进不了选路，其 APIKey 留空，
// 一行停用的历史上游不会因密文解不开而拖垮整条路由。
func (s *Store) ResolveModelRoute(ctx context.Context, name string) (*ModelRoute, error) {
	rows, err := s.stmtResolveModelRoute.QueryContext(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("解析模型路由: %w", err)
	}
	defer rows.Close()

	var route *ModelRoute
	for rows.Next() {
		var (
			m                                              Model
			modelDisabled                                  int
			mEntryOpenAI, mEntryResponses, mEntryAnthropic int
			mCreated, mUpdated                             string

			srcID, srcPriority, srcDisabled sql.NullInt64
			srcUpstreamModelID              sql.NullString

			upID, upDisabled                                                          sql.NullInt64
			upName, upType, upCatalogID, upBillingMode, upSealed, upBaseURL, upEgress sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &m.Family, &m.Pricing, &modelDisabled, &mEntryOpenAI, &mEntryResponses, &mEntryAnthropic, &mCreated, &mUpdated,
			&srcID, &srcUpstreamModelID, &srcPriority, &srcDisabled,
			&upID, &upName, &upType, &upCatalogID, &upBillingMode, &upSealed, &upBaseURL, &upDisabled, &upEgress); err != nil {
			return nil, fmt.Errorf("解析模型路由: %w", err)
		}
		if route == nil {
			m.Disabled = modelDisabled != 0
			m.EntryOpenAI, m.EntryResponses, m.EntryAnthropic = mEntryOpenAI != 0, mEntryResponses != 0, mEntryAnthropic != 0
			if m.CreatedAt, err = parseTime(mCreated); err != nil {
				return nil, fmt.Errorf("解析模型路由: created_at 非法: %w", err)
			}
			if m.UpdatedAt, err = parseTime(mUpdated); err != nil {
				return nil, fmt.Errorf("解析模型路由: updated_at 非法: %w", err)
			}
			route = &ModelRoute{Model: m}
		}
		if !srcID.Valid || !upID.Valid { // 左联查的空来源行：模型尚未挂任何来源
			continue
		}
		sourceDisabled, upstreamDisabled := srcDisabled.Int64 != 0, upDisabled.Int64 != 0
		// 只解密可能被选中的候选：停用的来源/上游一律进不了选路，它们的凭证
		// 解不开也不该拖垮整条路由——设备密钥换过之后逐个上游重录 Key 的恢复
		// 过程里，"停用旧行、启用新行"是常规操作，此时旧行的密文必然解不开。
		apiKey := ""
		if !sourceDisabled && !upstreamDisabled {
			if apiKey, err = s.openKey(upSealed.String); err != nil {
				return nil, fmt.Errorf("解析模型路由: 上游 %s: %w", upName.String, err)
			}
		}
		upstreamModelID := srcUpstreamModelID.String
		if upstreamModelID == "" {
			upstreamModelID = route.Model.Name
		}
		route.Candidates = append(route.Candidates, RouteCandidate{
			SourceID:        srcID.Int64,
			Priority:        srcPriority.Int64,
			UpstreamModelID: upstreamModelID,
			SourceDisabled:  sourceDisabled,
			Upstream: RouteUpstream{
				ID:          upID.Int64,
				Name:        upName.String,
				Type:        upType.String,
				CatalogID:   upCatalogID.String,
				BillingMode: upBillingMode.String,
				APIKey:      apiKey,
				BaseURL:     upBaseURL.String,
				Disabled:    upstreamDisabled,
				EgressMode:  upEgress.String,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("解析模型路由: %w", err)
	}
	if route == nil {
		return nil, fmt.Errorf("解析模型路由: %w", ErrNotFound)
	}
	return route, nil
}

// GetSourceRoute 按来源主键取探测视图（管理面「测试来源」）。与
// ResolveModelRoute 的两点刻意差异：
//
//   - 无视三层启停位——测试的对象就包括已停用的行（先测通再启用是常规操作），
//     启停事实原样带回，由调用方决定是否提示；
//   - 凭证解不开返回 ErrKeyUnreadable 而不是留空——调用方要给出「重新录入
//     Key」的指引，拿空凭证去上游只会换回一个误导性的 401。
//
// UpstreamModelID 已解析：库中为空时取模型名。未命中返回 ErrNotFound。
func (s *Store) GetSourceRoute(ctx context.Context, sourceID int64) (*SourceRoute, error) {
	var (
		m                                              Model
		modelDisabled                                  int
		mEntryOpenAI, mEntryResponses, mEntryAnthropic int
		mCreated, mUpdated                             string

		c                        RouteCandidate
		srcDisabled, upsDisabled int
		sealed                   string
	)
	err := s.stmtGetSourceRoute.QueryRowContext(ctx, sourceID).Scan(
		&m.ID, &m.Name, &m.Kind, &m.Family, &m.Pricing, &modelDisabled, &mEntryOpenAI, &mEntryResponses, &mEntryAnthropic, &mCreated, &mUpdated,
		&c.SourceID, &c.UpstreamModelID, &c.Priority, &srcDisabled,
		&c.Upstream.ID, &c.Upstream.Name, &c.Upstream.Type, &c.Upstream.CatalogID, &c.Upstream.BillingMode, &sealed, &c.Upstream.BaseURL, &upsDisabled, &c.Upstream.EgressMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询来源路由: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查询来源路由: %w", err)
	}
	m.Disabled = modelDisabled != 0
	m.EntryOpenAI, m.EntryResponses, m.EntryAnthropic = mEntryOpenAI != 0, mEntryResponses != 0, mEntryAnthropic != 0
	if m.CreatedAt, err = parseTime(mCreated); err != nil {
		return nil, fmt.Errorf("查询来源路由: created_at 非法: %w", err)
	}
	if m.UpdatedAt, err = parseTime(mUpdated); err != nil {
		return nil, fmt.Errorf("查询来源路由: updated_at 非法: %w", err)
	}
	c.SourceDisabled = srcDisabled != 0
	c.Upstream.Disabled = upsDisabled != 0
	if c.Upstream.APIKey, err = s.openKey(sealed); err != nil {
		// openKey 的错误文本已含补救指引且不含密钥物料，包上哨兵供调用方分流。
		return nil, fmt.Errorf("查询来源路由: %w: %v", ErrKeyUnreadable, err)
	}
	if c.UpstreamModelID == "" {
		c.UpstreamModelID = m.Name
	}
	return &SourceRoute{Model: m, Candidate: c}, nil
}

// ListServableModels 返回 /v1/models 的口径：**文本**（kind=text，迭代 8 起
// 谓词钉死——/v1/models 是文本对话面的契约，视频/图片模型不进）、启用、且
// 至少有一条启用来源（其上游也启用）的模型，按原始名字典序（SQLite 默认
// BINARY 排序即字节序）。
func (s *Store) ListServableModels(ctx context.Context) ([]Model, error) {
	rows, err := s.stmtListServableModels.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出可服务模型: %w", err)
	}
	defer rows.Close()
	var models []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("列出可服务模型: %w", err)
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出可服务模型: %w", err)
	}
	return models, nil
}

// ServableModelSource 是「一个可服务模型 × 它的一条可用来源」的扁平读数：
// 模型名 + 该来源所在上游的身份（type 与 base_url），调用方据这两个字段算出
// 这条来源能服务哪些入口协议（upstream.Account.Endpoint）。
//
// 刻意不带上游名、来源侧模型 ID 与优先级：这份读数是给客户端使用者看的
// （使用API页的可用模型清单），而「上游账户是谁、有几条来源、哪条服务了这次
// 请求」对客户端恒隐藏（架构 §11.1）。凭据字段更不在其中。
//
// ModelID 是目录模型行的 id：读数层拿它套单把 Key 的 API模型范围
// （KeyAPIModelScope.Allows），按 Key 裁剪清单；它是设备内部标识，不出客户端可见的响应。
type ServableModelSource struct {
	ModelID         int64
	ModelName       string
	UpstreamModelID string
	// EntryOpenAI / EntryResponses / EntryAnthropic 是模型行的调用入口开关：调用方把「来源
	// 能服务的协议」与它求交集才是对客户端真实开放的入口——开关是模型的
	// 服务承诺，能力是来源的事实，两者都带回、由读数层合成。
	EntryOpenAI       bool
	EntryResponses    bool
	EntryAnthropic    bool
	UpstreamType      string
	UpstreamCatalogID string
	UpstreamBaseURL   string
	// UpstreamBillingMode 供管理顶栏按账号快照分组计数，不出客户端模型清单。
	UpstreamBillingMode string
}

// ListServableModelSources 把 ListServableModels 的「有没有可用来源」摊开成
// 「有哪些可用来源」：两条查询的 WHERE 谓词逐字相同（模型为文本 kind + 模型
// 启用 + 来源启用 + 上游启用），所以两者的模型集合恒等——/v1/models 列得出的
// 模型，这里必然有至少一行。一个模型有几条可用来源就回几行，按模型名、
// 优先级、id 排序。
func (s *Store) ListServableModelSources(ctx context.Context) ([]ServableModelSource, error) {
	rows, err := s.stmtListServableModelSource.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出可服务模型来源: %w", err)
	}
	defer rows.Close()
	var out []ServableModelSource
	for rows.Next() {
		var (
			row                                      ServableModelSource
			entryOpenAI, entryResponses, entryAnthro int
		)
		if err := rows.Scan(&row.ModelID, &row.ModelName, &row.UpstreamModelID, &entryOpenAI, &entryResponses, &entryAnthro, &row.UpstreamType, &row.UpstreamCatalogID, &row.UpstreamBaseURL, &row.UpstreamBillingMode); err != nil {
			return nil, fmt.Errorf("列出可服务模型来源: %w", err)
		}
		row.EntryOpenAI, row.EntryResponses, row.EntryAnthropic = entryOpenAI != 0, entryResponses != 0, entryAnthro != 0
		if row.UpstreamModelID == "" {
			row.UpstreamModelID = row.ModelName
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出可服务模型来源: %w", err)
	}
	return out, nil
}

// ServableAIGCSource 是「一个启用的视频/图片模型 × 它的一条启用来源」的
// 扁平读数（迭代 8 Phase 6，使用API页的最小可见性）。字段纪律同
// ServableModelSource：只带模型名、kind 与该来源所在上游的 type/base_url——
// 调用方拿后两者判定「这条来源能不能服务该 kind 的入口」；上游名、来源侧
// 模型 ID 与凭据对客户端恒隐藏，不出这条查询。ModelID 同 ServableModelSource：
// 只给读数层套 Key 的 API模型范围，不出响应。
type ServableAIGCSource struct {
	ModelID         int64
	ModelName       string
	Kind            string // ModelKindVideo | ModelKindImage
	UpstreamType    string
	UpstreamBaseURL string
	// UpstreamBillingMode 与文本来源同口径，只用于管理顶栏分组计数。
	UpstreamBillingMode string
}

// ListServableAIGCSources 列出视频/图片模型的可用来源：启用谓词与文本读数
// 逐字相同（模型/来源/上游三层都启用），kind 显式圈定 video|image。零启用
// 来源的模型不出现（与 /v1/models 排除无来源模型同一口径）；来源挂在不服务
// 该 kind 的上游类型上时行照出——「列得出但入口进不去」由调用方按协议表
// 算出来如实标注，不在 SQL 里吞掉。
func (s *Store) ListServableAIGCSources(ctx context.Context) ([]ServableAIGCSource, error) {
	rows, err := s.stmtListServableAIGCSources.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出可服务视频/图片来源: %w", err)
	}
	defer rows.Close()
	var out []ServableAIGCSource
	for rows.Next() {
		var row ServableAIGCSource
		if err := rows.Scan(&row.ModelID, &row.ModelName, &row.Kind, &row.UpstreamType, &row.UpstreamBaseURL, &row.UpstreamBillingMode); err != nil {
			return nil, fmt.Errorf("列出可服务视频/图片来源: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出可服务视频/图片来源: %w", err)
	}
	return out, nil
}

// ---- 内部工具 ----

// scanUpstream 从行扫描管理视图的 Upstream；未命中返回 ErrNotFound。
// 解密只为取末 4 位，失败时留空而不是让整张列表不可读（见 Upstream.APIKeyLast4）。
func (s *Store) scanUpstream(r rowScanner) (*Upstream, error) {
	var (
		u                Upstream
		sealed           string
		disabled         int
		created, updated string
	)
	err := r.Scan(&u.ID, &u.Name, &u.Type, &u.CatalogID, &u.BillingMode, &sealed, &u.BaseURL, &disabled, &u.EgressMode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if u.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("updated_at 非法: %w", err)
	}
	if plaintext, err := s.openKey(sealed); err == nil {
		u.APIKeyLast4 = last4(plaintext)
	}
	return &u, nil
}

// scanModel 从行扫描 Model；未命中返回 ErrNotFound。
func scanModel(r rowScanner) (*Model, error) {
	var (
		m                                        Model
		disabled                                 int
		entryOpenAI, entryResponses, entryAnthro int
		created, updated                         string
	)
	err := r.Scan(&m.ID, &m.Name, &m.Kind, &m.Family, &m.Pricing, &disabled, &entryOpenAI, &entryResponses, &entryAnthro, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Disabled = disabled != 0
	m.EntryOpenAI = entryOpenAI != 0
	m.EntryResponses = entryResponses != 0
	m.EntryAnthropic = entryAnthro != 0
	if m.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("updated_at 非法: %w", err)
	}
	return &m, nil
}

// scanModelSource 从行扫描 ModelSource；未命中返回 ErrNotFound。
func scanModelSource(r rowScanner) (*ModelSource, error) {
	var (
		src              ModelSource
		disabled         int
		created, updated string
	)
	err := r.Scan(&src.ID, &src.ModelID, &src.UpstreamID, &src.UpstreamModelID,
		&src.Priority, &disabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.Disabled = disabled != 0
	if src.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if src.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("updated_at 非法: %w", err)
	}
	return &src, nil
}
