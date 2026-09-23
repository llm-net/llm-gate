package admin

// 数据升级把 LLM Gate官网的那份公开静态文件——模型目录数据 model-catalog.json
// （平台、模型、价格合一，形态见 internal/platformcatalog）——落到设备：
//
//	POST /admin/v1/system/data/update  管理员「立即更新」，无条件 GET
//	Server.AutoSyncData(ctx)           独立每小时定时器，条件 GET
//
// 两条入口共用 syncWebsiteData，官网不要求设备身份、凭据或开关。一次运行取一份
// 文件（20 秒下载上限）→ 存原文 → 用**生效**的那份（内嵌基线或版本号更大的同步
// 副本）做两件本地事：
//
//	补价    只给「未定价」的模型填目录价（applyCatalogPricing）：先按模型挂的来源
//	        找那条平台上的同名条目，没有来源再按名在全部平台里找。
//	收敛    按 agents 段建/补/删 Agent 订阅模型行（agentmodels.go），名义价取
//	        agents 段各型号的 pricing。
//
// 四条产品口径，改动前先读：
//
//  1. **绝不覆盖已定价的模型。** 管理员录过的价是他自己的口径（可能含渠道
//     折扣、可能是转售加价），目录价只是给"还没录"的那些一个合理默认值。
//     覆盖会让一次点击悄悄改掉全设备的账面单价——那是不可逆的产品事故。
//  2. **同步写进 models.pricing，不做运行期回退。** 换句话说：目录文件不
//     参与计价，它只是一次性地把数字搬进目录。这样"这个模型现在按多少钱记账"
//     依旧只有一个权威（模型行本身），未定价徽章、priced 三态、审计流水、
//     PricedFor 的告警全部照常成立。做成运行期回退层的话，模型页显示未定价
//     而账上却在扣钱，那是最难排查的一类账目问题。
//  3. **改价只影响其后的请求**（视频任务按清算时点价），历史行永不重算——
//     与手工改价同一条口径，所以这里复用同一个审计事件 model.pricing。
//  4. **Agent 订阅流量按官方按量 API 价记名义金额**：订阅是包月的，账面金额不是
//     现金支出——订阅补贴之下账面用量远大于月费是**预期读数**，不折算、不打折。
//     建行不由本流程做：哪份订阅有哪些模型是目录 agents 段说了算，本流程只负责
//     把文件搬到设备上，然后叫一次收敛器（syncAgentModelsFor）。
//
// 建模时的填价（从平台「添加模型」、手工建模不带价）走同一份查找
// （catalogPricingFor）：新模型建出来就有价，不必等下一轮同步。
//
// 价格表的合法性走**手工录入那条**校验路径（parseModelPricing）：kind 圈定的
// 字段集、非负整数微元、上限、成对字段，一份定义两处消费。文件里某一条不合
// 形态时只跳过那一条并在结果里说明原因，不整批失败。
//
// 自动更新不阻塞任何请求；官网不可达只影响本次升级，不影响设备数据面。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// settingModelCatalog 是 settings 表里的设备级单值配置，值是 storedCatalogFile 的
// JSON。合并前的两个键（official_pricing / platform_models）由迁移 0056 清掉。
const settingModelCatalog = "model_catalog"

// catalogFetchTimeout 是**下载那一段**的上限。下载侧的 http.Client 自带 15s，
// 这里是防远端把连接吊着的兜底；落库与写价不受它约束。
const catalogFetchTimeout = 20 * time.Second

// catalogAutoInterval 是自动检查的最小间隔。**常量，刻意不加 YAML 旋钮**：
// 一份静态文件一天变不了几次，可调的间隔只会长出一堆各不相同的设备。
const catalogAutoInterval = time.Hour

// 跳过原因（对外是稳定的机读值，zh-CN 文案在管理台一侧）。
const (
	skipAlreadyPriced = "already_priced"   // 本地已录价：绝不覆盖
	skipNotInFile     = "not_in_file"      // 目录里没有这个模型名
	skipKindMismatch  = "kind_mismatch"    // 名字对上了但种类不同（文本价不能套到视频模型上）
	skipNoPriceInFile = "no_price_in_file" // 目录里有这一条，但它自己就是未定价
	skipInvalidPrice  = "invalid_pricing"  // 目录里那张价目表不合本 kind 的形态
	skipAgentManaged  = "agent_managed"    // 订阅计价行：价由收敛器按 agents 段写
)

// CatalogSource 是官网模型目录文件的来源。生产实现是 *officialsite.Client，
// 测试用桩替换。
//
// etag 参数是**上次存下的那个**（空串 = 无条件 GET）：自动更新带上它做条件 GET，
// 官网文件没变就吃 304；手动「立即更新」恒传空串——人点那颗按钮就是要确认云上
// 此刻是什么。
type CatalogSource interface {
	ModelCatalogURL() string
	FetchModelCatalog(ctx context.Context, etag string) (*officialsite.ModelCatalogFile, error)
}

// SetCatalogSource 注入目录数据文件的来源。生产装配恒会注入官网客户端。
//
// 走注入而不是 New 的入参，避免测试装配把 nil 具体指针塞进非 nil 接口。
func (s *Server) SetCatalogSource(src CatalogSource) { s.catalog = src }

func (s *Server) catalogSyncable() bool { return s.catalog != nil }

// catalogActor 是一次同步的**来路**。审计不再记「谁」（设备只有一个管理员），
// 但仍要记得清「这次改价是谁点出来的还是设备自己跑的」。
type catalogActor struct {
	Manual bool   // true = 管理员点了「立即更新」；false = 每小时那次自动检查
	IP     string // 手动路径的对端 IP；自动路径为空
}

func manualCatalogActor(r *http.Request) catalogActor {
	return catalogActor{Manual: true, IP: remoteIP(r)}
}

func autoCatalogActor() catalogActor { return catalogActor{} }

// by 把来路折成审计 detail 里的 by= 段（收敛器共用同一套词汇）。
func (a catalogActor) by() string {
	if a.Manual {
		return catalogByManual
	}
	return catalogByAuto
}

const (
	catalogByManual = "手动"
	catalogByAuto   = "自动检查"
)

// catalogAutoState 是自动更新的进程内状态。零值可用；两个时刻**都是内存态**，
// 进程重启即忘——重启后的独立定时器会立即检查一次，代价是一次条件 GET（多半 304）。
type catalogAutoState struct {
	// single 是手动「立即更新」与自动更新**共用**的单飞锁。恒用 TryLock：
	// 拿不到就不做（自动那条等下一个节拍，手动那条答 409 catalog_sync_busy），
	// 绝不排队。
	single sync.Mutex

	mu            sync.Mutex
	lastCheckedAt time.Time
	lastUpdatedAt time.Time
	// failed 是上一轮自动更新的成败，只用来把日志压成状态翻转两条。
	failed bool
}

func (a *catalogAutoState) checked(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheckedAt = now
}

func (a *catalogAutoState) updated(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUpdatedAt = now
}

func (a *catalogAutoState) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheckedAt = time.Time{}
}

func (a *catalogAutoState) due(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCheckedAt.IsZero() || now.Sub(a.lastCheckedAt) >= catalogAutoInterval
}

func (a *catalogAutoState) times() (checked, updated time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCheckedAt, a.lastUpdatedAt
}

func (a *catalogAutoState) flip(failed bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed == failed {
		return false
	}
	a.failed = failed
	return true
}

// storedCatalogFile 是目录数据文件的落库形态：下载到的文件原样放在 File 里，
// 外面裹一层"什么时候、从哪儿、拿的是哪一版"。ETag 供下一轮条件 GET；老固件
// 存下的没有这一列，取到空串即退化成一次无条件 GET。
//
// 唯一的字节级差异是 encoding/json 对 RawMessage 的紧凑化（去掉无意义空白、
// 对 < > & 做 \u 转义）：JSON 语义完全等价，键序与数值都不动。
type storedCatalogFile struct {
	FetchedAt string          `json:"fetched_at"`
	URL       string          `json:"url"`
	ETag      string          `json:"etag,omitempty"`
	SHA256    string          `json:"sha256,omitempty"`
	File      json.RawMessage `json:"file"`
}

// storedCatalogWarn 把「已存文件读不出来 / 解不开」这类**持续状态**的告警压成
// 状态翻转两条：这些读出路径挂在设备设置页的读数与「添加模型」选单上，管理员
// 每开一次页就走一遍，一份坏掉的已存文件不该变成"页面刷一次、journal 写一行"。
type storedCatalogWarn struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (w *storedCatalogWarn) fire(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[key] {
		return false
	}
	if w.seen == nil {
		w.seen = make(map[string]bool, 4)
	}
	w.seen[key] = true
	return true
}

func (w *storedCatalogWarn) clear(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, key)
}

func (s *Server) warnStoredCatalog(key, stage, msg string, err error) {
	if s.storedWarn.fire(key + ":" + stage) {
		s.log.Warn(msg, "err", err)
	}
}

func (s *Server) clearStoredCatalogWarn(key, stage string) {
	s.storedWarn.clear(key + ":" + stage)
}

// readStoredCatalogFile 读回已存的目录数据文件。读不到、读坏了都只回 false
// （附一条 Warn）：它是页面上的附属读数，不该把宿主页面拖成 500。
func (s *Server) readStoredCatalogFile(ctx context.Context, key, what string) (storedCatalogFile, bool) {
	raw, err := s.st.GetSetting(ctx, key)
	if err != nil {
		s.warnStoredCatalog(key, "read", "读取"+what+"同步状态失败", err)
		return storedCatalogFile{}, false
	}
	s.clearStoredCatalogWarn(key, "read")
	if raw == "" {
		return storedCatalogFile{}, false
	}
	var stored storedCatalogFile
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		s.warnStoredCatalog(key, "decode", what+"同步状态解析失败", err)
		return storedCatalogFile{}, false
	}
	s.clearStoredCatalogWarn(key, "decode")
	if stored.SHA256 != "" {
		got, err := catalogSHA256(stored.File)
		if err != nil || got != stored.SHA256 {
			s.warnStoredCatalog(key, "integrity", what+"同步状态完整性校验失败", errors.New("sha256 mismatch"))
			return storedCatalogFile{}, false
		}
	}
	s.clearStoredCatalogWarn(key, "integrity")
	return stored, true
}

// storedCatalogETag 取上次存下的 ETag 供条件 GET 用。conditional 为 false
// （手动「立即更新」）或从没存过时回空串 = 无条件 GET。
func (s *Server) storedCatalogETag(ctx context.Context, conditional bool) string {
	if !conditional {
		return ""
	}
	stored, ok := s.readStoredCatalogFile(ctx, settingModelCatalog, "模型目录")
	if !ok {
		return ""
	}
	return stored.ETag
}

// storeCatalogFile 落一份文件的原文。
func (s *Server) storeCatalogFile(ctx context.Context, key, url string, raw []byte, etag string) error {
	digest, err := catalogSHA256(raw)
	if err != nil {
		return err
	}
	stored, err := json.Marshal(storedCatalogFile{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		URL:       url,
		ETag:      etag,
		SHA256:    digest,
		File:      json.RawMessage(raw),
	})
	if err != nil { // json.RawMessage 已校验过是合法 JSON；防御分支
		return err
	}
	return s.st.SetSetting(ctx, key, string(stored))
}

// catalogSHA256 对 JSON 语义的稳定编码取摘要。外层 storedCatalogFile 会压缩
// RawMessage 并转义 HTML 字符，不能直接对下载字节取摘要后再与读回字节比较。
func catalogSHA256(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

// modelCatalogJSON 是模型目录数据的读数。设备恒有一份固件内嵌的基线
// （platformcatalog.Builtin），所以 BuiltinVersion 恒在场；Supported 只说
// 「此刻点得动同步吗」。Origin 是当前实际生效的那份：builtin | synced。
// SyncedAt 是上次落库时刻（存着但没生效时也报，否则界面会显示成"从没同步过"）。
type modelCatalogJSON struct {
	Supported      bool   `json:"supported"`
	URL            string `json:"url,omitempty"`
	Origin         string `json:"origin"`
	Version        int64  `json:"version"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	DataTag        string `json:"data_tag,omitempty"`
	BuiltinVersion int64  `json:"builtin_version"`
	Platforms      int    `json:"platforms"`
	Models         int    `json:"models"`
	Priced         int    `json:"priced"`
	Agents         int    `json:"agents"`
	SyncedAt       string `json:"synced_at,omitempty"`
}

const (
	catalogOriginBuiltin = "builtin"
	catalogOriginSynced  = "synced"
)

// effectivePlatformModels 挑出当前生效的模型目录：同步下来的**版本号更大**
// 时用它，否则用固件内嵌的基线。
//
// 比版本号而不是比同步时间：云上文件只会往前走，而「刚同步过」不等于「更新」
// ——固件升级会带来更新的内嵌基线，那时上次同步的旧副本必须自动让位，否则
// 一次早年的同步会把设备永久钉在旧清单上。存的那份解析不了就当没有（外部文件，
// 降级到基线，附一条 Warn）。
func (s *Server) effectivePlatformModels(ctx context.Context) (platformcatalog.Doc, modelCatalogJSON) {
	doc := platformcatalog.Builtin()
	out := modelCatalogJSON{
		Origin:         catalogOriginBuiltin,
		Version:        doc.Version,
		UpdatedAt:      doc.UpdatedAt,
		DataTag:        doc.Source.Tag,
		BuiltinVersion: doc.Version,
	}
	if s.catalogSyncable() {
		out.Supported = true
		out.URL = s.catalog.ModelCatalogURL()
	}
	if stored, ok := s.readStoredCatalogFile(ctx, settingModelCatalog, "模型目录"); ok {
		if out.URL == "" {
			out.URL = stored.URL
		}
		synced, err := platformcatalog.Parse(stored.File)
		if err == nil {
			s.clearStoredCatalogWarn(settingModelCatalog, "parse")
		}
		switch {
		case err != nil:
			s.warnStoredCatalog(settingModelCatalog, "parse", "已存的模型目录文件解析失败", err)
		case synced.Version > doc.Version:
			doc = synced
			out.Origin = catalogOriginSynced
			out.Version = synced.Version
			out.UpdatedAt = synced.UpdatedAt
			out.DataTag = synced.Source.Tag
			out.SyncedAt = stored.FetchedAt
		default:
			out.SyncedAt = stored.FetchedAt
		}
	}
	out.Platforms = len(doc.Platforms)
	for _, p := range doc.Platforms {
		out.Models += len(p.Models)
		for _, m := range p.Models {
			if len(m.Pricing) > 0 {
				out.Priced++
			}
		}
	}
	out.Agents = len(doc.Agents)
	return doc, out
}

// EffectivePlatformModels 给同进程的数据面提供与管理台完全相同的生效目录。
// 返回值只含公开数据文件里的模型元数据，不含设备凭据或账号事实。
func (s *Server) EffectivePlatformModels(ctx context.Context) platformcatalog.Doc {
	doc, _ := s.effectivePlatformModels(ctx)
	return doc
}

// catalogAutoJSON 是数据升级自动检查的读数。Enabled = 官网客户端已装配；
// 两个时刻是内存态，进程重启即空。
type catalogAutoJSON struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
	LastCheckedAt   string `json:"last_checked_at,omitempty"`
	LastUpdatedAt   string `json:"last_updated_at,omitempty"`
}

func (s *Server) catalogAutoStatus() catalogAutoJSON {
	checked, updated := s.catalogAuto.times()
	out := catalogAutoJSON{
		Enabled:         s.catalogSyncable(),
		IntervalSeconds: int64(catalogAutoInterval / time.Second),
	}
	if !checked.IsZero() {
		out.LastCheckedAt = checked.UTC().Format(time.RFC3339)
	}
	if !updated.IsZero() {
		out.LastUpdatedAt = updated.UTC().Format(time.RFC3339)
	}
	return out
}

type dataStatusResponse struct {
	ModelCatalog modelCatalogJSON `json:"model_catalog"`
	Automatic    catalogAutoJSON  `json:"automatic"`
}

// handleDataStatus 返回数据升级读数；官网连接不需要开关、登记或设备凭据。
func (s *Server) handleDataStatus(w http.ResponseWriter, r *http.Request) {
	_, catalog := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusOK, dataStatusResponse{
		ModelCatalog: catalog,
		Automatic:    s.catalogAutoStatus(),
	})
}

// appliedPricingJSON 是一条"填上了"的结果：填了哪张价目表、来自哪条平台、
// 有没有附注。
type appliedPricingJSON struct {
	Model    string        `json:"model"`
	Kind     string        `json:"kind"`
	Platform string        `json:"platform,omitempty"`
	Vendor   string        `json:"vendor,omitempty"`
	Pricing  usage.Pricing `json:"pricing"`
	Note     string        `json:"note,omitempty" i18n:"text"`
}

// skippedPricingJSON 是一条"没填"的结果。Reason 是机读值，Detail 只在
// invalid_pricing 时带上形态错误的原文（那是文件内容的问题，管理员需要知道
// 具体是哪个字段）。
type skippedPricingJSON struct {
	Model  string `json:"model"`
	Kind   string `json:"kind"`
	Reason string `json:"reason" i18n:"text"`
	Detail string `json:"detail,omitempty" i18n:"text"`
}

type catalogSyncResponse struct {
	Source  modelCatalogJSON     `json:"source"`
	Applied []appliedPricingJSON `json:"applied"`
	Skipped []skippedPricingJSON `json:"skipped"`
	// AgentModels 是同步收尾那次 Agent 订阅模型收敛的账：建了/补了/回收了几行。
	AgentModels agentModelSyncResult `json:"agent_models"`
}

// catalogSyncFailure 是一次同步的**对外**失败：机读 code + 可直接展示的文案 +
// HTTP 状态。核心把它回给两条入口——手动那条照原样答给界面，自动那条只记日志。
// 不是这个类型的错误一律当内部错误处置（handler 走 internalError，500）。
type catalogSyncFailure struct {
	status  int
	code    string
	message string
}

func (e *catalogSyncFailure) Error() string { return e.code + "：" + e.message }

func websiteNotConfiguredFailure() *catalogSyncFailure {
	return &catalogSyncFailure{http.StatusServiceUnavailable, "website_not_configured",
		"本进程未接入 LLM Gate官网数据源"}
}

// handleSyncCatalog 处理 POST /admin/v1/system/data/update（「立即更新」，仅
// admin）。手动路径恒**无条件 GET**：人点按钮就是要确认官网此刻发布的内容，
// 让它可能什么都不做会让人怀疑按钮坏了。
func (s *Server) handleSyncCatalog(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		f := websiteNotConfiguredFailure()
		writeError(w, f.status, f.code, f.message)
		return
	}
	// 单飞：与自动更新共用同一把锁。撞上正在跑的那一轮就明说，不排队、也不
	// 静默降级成"成功"。文案不说"自动更新中"——锁也可能在另一个管理员（或同一个人的双击）手上。
	if !s.catalogAuto.single.TryLock() {
		writeError(w, http.StatusConflict, "catalog_sync_busy",
			"数据正在更新中，请稍后再点一次「立即更新」")
		return
	}
	defer s.catalogAuto.single.Unlock()

	resp, err := s.syncWebsiteData(r.Context(), manualCatalogActor(r), false)
	if err != nil {
		var f *catalogSyncFailure
		if errors.As(err, &f) {
			// 失败的日志由两条入口各自负责（核心保持沉默，见 syncWebsiteData）：
			// 手动这条留一行 journal——"我点了没反应"来问时有据可查。
			s.log.Warn("数据升级「立即更新」失败", "code", f.code, "reason", f.message)
			writeError(w, f.status, f.code, f.message)
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// AutoSyncData 是每小时数据升级的入口，由 gatewayd 的独立定时器调用。
//
// 三道闸门，顺序即契约：
//
//  1. 官网客户端未装配时返回并把 lastCheckedAt 清零。
//  2. 距上次检查不足 catalogAutoInterval 直接返回（手动「立即更新」也刷新那个
//     时刻——人刚点过，一分钟后自动再取一遍是白跑）。
//  3. TryLock 单飞：与手动端点共用同一把锁，撞上就等下一个节拍；真正的同步
//     在自己的 goroutine 中执行，不阻塞调用方。
//
// 失败只按连通性翻转记日志，绝不重试、不退避、不影响任何请求路径。
func (s *Server) AutoSyncData(ctx context.Context) {
	// 整条路径都要兜住 panic：调用方是后台定时器，它没有 withRecovery，而
	// **裸协程里的 panic 会带走整个进程**。官网从来不是设备的运行依赖——一份写坏
	// 的远端文件绝不能停掉这台盒子。协程里那一道要单独下（defer 不跨协程）。
	defer s.recoverAutoSync()
	if !s.catalogSyncable() {
		s.catalogAuto.reset()
		return
	}
	if !s.catalogAuto.due(time.Now()) {
		return
	}
	if !s.catalogAuto.single.TryLock() {
		return
	}
	// 锁在这个协程里拿、在下面那个协程里还——Go 的 Mutex 不归属协程，这是
	// 有意的：闸门必须在定时器这一跳同步判完，而真正的活儿要在后台跑完。
	go func() {
		defer s.catalogAuto.single.Unlock()
		defer s.recoverAutoSync()
		_, err := s.syncWebsiteData(ctx, autoCatalogActor(), true)
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		// 日志压成状态翻转两条：每小时一轮，取不到就每小时刷一条的话，一份没
		// 发布的文件能把 journal 淹掉。只记原因，文件内容与被跳过条目的正文都不
		// 进日志（§15.1）。
		if !s.catalogAuto.flip(reason != "") {
			return
		}
		if reason != "" {
			s.log.Warn("数据升级自动检查没取到（官网不是设备的运行依赖）", "reason", reason)
			return
		}
		s.log.Info("数据升级自动检查已恢复")
	}()
}

// recoverAutoSync 是自动更新路径上的 panic 兜底。记的与 withRecovery 同形：
// panic 值与栈，没有任何远端文件内容。
func (s *Server) recoverAutoSync() {
	rec := recover()
	if rec == nil {
		return
	}
	s.log.Error("数据升级自动检查 panic recovered",
		"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
}

// syncWebsiteData 是手动与自动**共用**的同步核心，与请求无关。conditional 为真时
// 走条件 GET（带上次存下的 ETag，云上没变吃 304 就不落库）。
//
// 返回的 error 只有文件取不到 / 存不下时才非 nil（*catalogSyncFailure 或库错误）。
//
// **失败一律不在这里记日志**，只把原因原样回给调用方：两条入口的噪声阈值不同
// ——手动那条每次都该在 journal 留一行，自动那条每小时一轮，必须压成连通性翻转
// 两条。核心只记"真的变了什么"（版本号与条目数）。
func (s *Server) syncWebsiteData(ctx context.Context, actor catalogActor, conditional bool) (catalogSyncResponse, error) {
	resp := catalogSyncResponse{
		Applied: []appliedPricingJSON{},
		Skipped: []skippedPricingJSON{},
	}
	if s.catalog == nil {
		return resp, websiteNotConfiguredFailure()
	}
	// 「上次检查」以**开跑**为准：这一轮取没取到都算检查过了，失败不该让下一个
	// 定时节拍立刻再打一发。
	s.catalogAuto.checked(time.Now())

	changed, err := s.fetchModelCatalog(ctx, conditional)
	if err != nil {
		return resp, err
	}
	if changed {
		s.catalogAuto.updated(time.Now())
	}

	// 本地那两步**每次都跑，304 的那一轮也跑**：它们的输入不只有文件，还有设备
	// 上此刻的模型行与订阅行——管理员昨天新建的模型今天该拿到价，今早连上的订阅
	// 今天该长出模型行，而云上文件一个字节都没改。两步都本地幂等（只填空、不覆盖），
	// 重复跑没有副作用。
	doc, status := s.effectivePlatformModels(ctx)
	if err := s.applyCatalogPricing(ctx, doc, status.Version, actor, &resp); err != nil {
		return resp, err
	}
	resp.Source = status
	// 收尾收敛：厂商在 agents 段上新/下架、或刚取到的目录让某个计价行终于有了价，
	// 都在这一刻生效。放在写响应之前，界面刷新读到的就是收敛后的模型列表。
	resp.AgentModels = s.syncAgentModelsFor(ctx, actor, "catalog_sync")
	return resp, nil
}

// fetchModelCatalog 取回并落库模型目录文件。返回值说的是"有没有新文件落库"
// （304 为 false）。版本回退或同版本改内容的文件拒绝落库并报失败——目录的
// 二义性一旦落到账上就是永久的，而这种问题只可能来自官网发布错误，报出来让人去修。
func (s *Server) fetchModelCatalog(ctx context.Context, conditional bool) (bool, error) {
	// 超时只箍住**网络那一段**：落库与逐条写价随外层 ctx，绝不让一次慢下载把
	// 后面的写操作截在半途。
	fetchCtx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()
	etag := s.storedCatalogETag(ctx, conditional)
	file, err := s.catalog.FetchModelCatalog(fetchCtx, etag)
	switch {
	case errors.Is(err, officialsite.ErrNotModified):
		return false, nil
	case err != nil:
		var ce *officialsite.HTTPError
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			return false, &catalogSyncFailure{http.StatusBadGateway, "model_catalog_unavailable",
				"LLM Gate官网还没有发布模型目录文件（404）"}
		}
		return false, &catalogSyncFailure{http.StatusBadGateway, "model_catalog_unavailable",
			"下载模型目录文件失败：" + err.Error()}
	}
	if stored, ok := s.readStoredCatalogFile(ctx, settingModelCatalog, "模型目录"); ok {
		if previous, parseErr := platformcatalog.Parse(stored.File); parseErr == nil {
			switch {
			case file.Doc.Version < previous.Version:
				return false, &catalogSyncFailure{http.StatusBadGateway, "model_catalog_invalid",
					fmt.Sprintf("模型目录文件版本从 %d 回退到 %d，设备拒绝激活并继续使用已有数据", previous.Version, file.Doc.Version)}
			case file.Doc.Version == previous.Version:
				oldDigest, oldErr := catalogSHA256(stored.File)
				newDigest, newErr := catalogSHA256(file.Raw)
				if oldErr == nil && newErr == nil && oldDigest != newDigest {
					return false, &catalogSyncFailure{http.StatusBadGateway, "model_catalog_invalid",
						fmt.Sprintf("模型目录文件版本 %d 未递增但内容发生变化，设备拒绝激活并继续使用已有数据", file.Doc.Version)}
				}
			}
		}
	}
	if err := s.storeCatalogFile(ctx, settingModelCatalog, file.URL, file.Raw, file.ETag); err != nil {
		return false, err
	}
	s.log.Info("模型目录文件已更新", "version", file.Doc.Version, "tag", file.Doc.Source.Tag,
		"platforms", len(file.Doc.Platforms), "agents", len(file.Doc.Agents))
	return true, nil
}

// catalogPriceHit 是「这个模型该填哪条价」的查找结果。
type catalogPriceHit struct {
	Platform platformcatalog.Platform
	Model    platformcatalog.Model
}

// catalogPricingFor 在生效目录里给一个模型找它的估算目录价：
//
//  1. 先按模型挂的来源（来源序 = 优先级序）找那条平台上的同名条目——从哪条平台
//     接的模型就按那条平台的价，聚合平台与厂商直连的同名型号价可以不同。
//  2. 一条来源都没挂（手工建模）时按名在全部平台里找，取文件序第一条有价的。
//
// 返回：命中条目、跳过原因（skip* 之一；命中时为空）。带价条目优先于同名无价
// 条目；名字对上但种类不同的只在没有同名同种类条目时才算 kind_mismatch。
func catalogPricingFor(doc platformcatalog.Doc, name, kind string, sources []store.ModelSourceDetail) (catalogPriceHit, string) {
	var unpriced *catalogPriceHit
	for _, src := range sources {
		p, ok := doc.PlatformByID(src.UpstreamCatalogID, src.UpstreamType)
		if !ok {
			continue
		}
		m, ok := doc.PlatformModel(p.ID, p.Type, name, kind)
		if !ok {
			continue
		}
		if len(m.Pricing) > 0 {
			return catalogPriceHit{Platform: p, Model: m}, ""
		}
		if unpriced == nil {
			unpriced = &catalogPriceHit{Platform: p, Model: m}
		}
	}
	if len(sources) == 0 {
		for _, p := range doc.Platforms {
			m, ok := doc.PlatformModel(p.ID, p.Type, name, kind)
			if !ok {
				continue
			}
			if len(m.Pricing) > 0 {
				return catalogPriceHit{Platform: p, Model: m}, ""
			}
			if unpriced == nil {
				unpriced = &catalogPriceHit{Platform: p, Model: m}
			}
		}
	}
	if unpriced != nil {
		return *unpriced, skipNoPriceInFile
	}
	if len(doc.ModelsNamed(name, "")) > 0 {
		return catalogPriceHit{}, skipKindMismatch
	}
	return catalogPriceHit{}, skipNotInFile
}

// applyCatalogPricing 用生效目录补填未定价的模型（口径 1：只填空、绝不覆盖）。
// version 只进审计 detail。
func (s *Server) applyCatalogPricing(ctx context.Context, doc platformcatalog.Doc, version int64, actor catalogActor, resp *catalogSyncResponse) error {
	models, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return err
	}
	agents := s.agentCatalogIndex(ctx)
	for i := range models {
		m := &models[i]
		// 已定价的先判：管理员的口径优先于目录价，连"种类对不对"都不必问。
		if m.Pricing != "" {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipAlreadyPriced})
			continue
		}
		// 订阅计价行的价由收敛器按 agents 段写（同一轮的收尾），这里不抢。
		if agentSubscriptionProvider(agents, m.Name, m.Kind, len(m.Sources)) != "" {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipAgentManaged})
			continue
		}
		hit, reason := catalogPricingFor(doc, m.Name, m.Kind, m.Sources)
		if reason != "" {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: reason})
			continue
		}
		// 与手工录入同一条校验路径：字段集按本地模型的 kind 圈定，值必须是
		// 非负整数微元，成对字段必须齐全，通过后拿到的是规范化后的入库原文。
		pricing, _, perr := parseModelPricing(m.Kind, hit.Model.Pricing)
		if perr != nil {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipInvalidPrice, Detail: perr.Error()})
			continue
		}
		if pricing == "" {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipNoPriceInFile})
			continue
		}
		if err := s.st.SetModelPricing(ctx, m.ID, pricing); err != nil {
			// 写库失败就地中止：已经写进去的那些是好的（同步幂等，重跑一次
			// 会接着填剩下的），继续往下写只会把同一个故障重复 N 遍。
			return err
		}
		s.audit(ctx, store.AuditEvent{
			Event:  EventModelPricing,
			Entity: entityModel(m.ID),
			Detail: fmt.Sprintf("name=%s kind=%s pricing=%s source=model_catalog:v%d platform=%s by=%s",
				m.Name, m.Kind, pricingAudit(pricing), version, hit.Platform.ID, actor.by()),
			RemoteIP: actor.IP,
		})
		resp.Applied = append(resp.Applied, appliedPricingJSON{
			Model:    m.Name,
			Kind:     m.Kind,
			Platform: hit.Platform.ID,
			Vendor:   clipDisplay(hit.Platform.Vendor),
			Pricing:  modelPricingJSON(pricing),
			Note:     clipDisplay(hit.Model.Note),
		})
	}
	if len(resp.Applied) > 0 {
		s.log.Info("目录价补填完成", "version", version,
			"applied", len(resp.Applied), "skipped", len(resp.Skipped))
	}
	return nil
}

// initialCatalogPricing 给一条**正在新建**的模型行算初始目录价：从平台「添加模型」
// 时按那条平台的条目，手工建模时按名兜底。找不到、或目录里那条不合本 kind 的
// 形态，都回空串 = 未定价（照常转发、金额记 0、管理台挂警示徽章），下一轮同步
// 还会再试一次。第二个返回值是命中的平台 ID（只进审计）。
func (s *Server) initialCatalogPricing(ctx context.Context, name, kind, catalogID, adapter string) (string, string) {
	doc, _ := s.effectivePlatformModels(ctx)
	var sources []store.ModelSourceDetail
	if adapter != "" {
		sources = []store.ModelSourceDetail{{UpstreamCatalogID: catalogID, UpstreamType: adapter}}
	}
	hit, reason := catalogPricingFor(doc, name, kind, sources)
	if reason != "" {
		return "", ""
	}
	pricing, _, err := parseModelPricing(kind, hit.Model.Pricing)
	if err != nil {
		s.log.Warn("模型目录里的价目不合本模型种类的形态，新模型按未定价建行",
			"model", name, "kind", kind, "platform", hit.Platform.ID, "err", err.Error())
		return "", ""
	}
	return pricing, hit.Platform.ID
}

// recognizedAgentProvider 判断 agent 取值是不是本固件认识的订阅 provider
// （认识 = 数据面有它的入口、管理台渲染得出它的调用路径）。
func recognizedAgentProvider(agent string) bool {
	return agent == store.AgentProviderCodex || agent == store.AgentProviderGrok ||
		agent == store.AgentProviderClaude || agent == store.AgentProviderCursor
}

// maxDisplayRunes 是回显给管理台的展示字段（厂商名、附注）的长度上限。
// 这些字段是**远端文件的内容**，直接进管理台界面，截断是最低限度的约束。
const maxDisplayRunes = 200

// clipDisplay 把远端来的展示字段裁到上限，并去掉控制字符。
func clipDisplay(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > maxDisplayRunes {
		return string(runes[:maxDisplayRunes]) + "…"
	}
	return s
}
