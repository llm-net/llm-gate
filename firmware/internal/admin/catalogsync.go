package admin

// 数据升级把 LLM Gate官网的两份公开静态文件落到设备：
//
//	POST /admin/v1/system/data/update  管理员「立即更新」，无条件 GET
//	Server.AutoSyncData(ctx)           独立每小时定时器，条件 GET
//
// 两条入口共用 syncWebsiteData，官网不要求设备身份、凭据或开关。一次运行顺序
// 固定地取两份文件，各自有 20 秒下载上限：
//
//	official-pricing.json  官方目录价 → 存原文 + **只填充未定价的模型**（只搬 pricing 标准价；
//	                       条目可带的 schedule 分时段价原样留在已存文件里，不参与记账，形态
//	                       权威是 usage.ParseSchedule）。
//	platform-models.json   平台模型信息 → 存原文。两段两种分量：platforms 段是
//	                       「API密钥接入 → 添加模型」的选单（见 upstreammodels.go），
//	                       agents 段是 Agent 订阅模型的权威清单——落库之后本流程
//	                       顺手收敛一次（agentmodels.go），厂商上新/下架在这一刻
//	                       生效。
//
// 两份放在一次动作里，是因为它们同属官网的公开数据升级，不需要分别设动作或定时器。
// **失败语义却不同**：价目取不到 = 手动端点整个失败（那是这次动作的主产出）；
// 清单取不到只回一句原因（platform_error）——设备恒有固件内嵌的基线可用，顶多旧一点。
//
// 四条产品口径，改动前先读：
//
//  1. **绝不覆盖已定价的模型。** 管理员录过的价是他自己的口径（可能含渠道
//     折扣、可能是转售加价），官方价只是给"还没录"的那些一个合理默认值。
//     覆盖会让一次点击悄悄改掉全设备的账面单价——那是不可逆的产品事故。
//  2. **同步写进 models.pricing，不做运行期回退。** 换句话说：官方价文件不
//     参与计价，它只是一次性地把数字搬进目录。这样"这个模型现在按多少钱记账"
//     依旧只有一个权威（模型行本身），未定价徽章、priced 三态、审计流水、
//     PricedFor 的告警全部照常成立。做成运行期回退层的话，模型页显示未定价
//     而账上却在扣钱，那是最难排查的一类账目问题。
//  3. **改价只影响其后的请求**（视频任务按清算时点价），历史行永不重算——
//     与手工改价同一条口径，所以这里复用同一个审计事件 model.pricing。
//  4. **Agent 订阅流量按官方按量 API 价记名义金额**（2026-08-13 产品决定）：
//     订阅是包月的，账面金额不是现金支出——订阅补贴之下账面用量远大于月费是
//     **预期读数**，不折算、不打折。实现全靠既有旋钮：`applyAgentModelPricing`
//     按名（区分大小写）取目录价。**建行不由本流程做**（2026-08-15）：
//     哪份订阅有哪些模型是 platform-models.json 的 agents 段说了算，本流程只
//     负责把两份文件搬到设备上，然后叫一次收敛器（syncAgentModelsFor）——
//     那边按「目录 × 在场订阅」建/删行，价从**已存**的价目文件里取。
//
// 价格表的合法性走**手工录入那条**校验路径（parseModelPricing）：kind 圈定的
// 字段集、非负整数微元、上限、成对字段，一份定义两处消费。文件里某一条不合
// 形态时只跳过那一条并在结果里说明原因，不整批失败——一个厂商改了计费形态，
// 不该让其余七条也同步不了。
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

// settings 表里的两个设备级单值配置，值都是 storedCatalogFile 的 JSON。
// 键名保持 2026-08-14 改名前的字面量：改键要带
// 一次迁移，而这两个字符串没有对外语义，不值得。（2026-08-15 白天短暂有过第三个键
// recommended_apps，随「推荐应用」改判为透传删除，残留行由迁移 0017 清掉。）
const (
	settingOfficialPricing = "official_pricing"
	settingPlatformModels  = "platform_models"
)

// catalogFetchTimeout 是**下载那一段**的上限（两份文件各自受它约束）。
// 下载侧的 http.Client 自带 15s，这里是防远端把连接吊着的兜底；落库与写价
// 不受它约束。
const catalogFetchTimeout = 20 * time.Second

// catalogAutoInterval 是自动检查的最小间隔。**常量，刻意不加 YAML 旋钮**：
// 两份静态文件一天变不了几次，可调的间隔只会长出一堆各不相同的设备。
const catalogAutoInterval = time.Hour

// 跳过原因（对外是稳定的机读值，zh-CN 文案在管理台一侧）。
const (
	skipAlreadyPriced = "already_priced"   // 本地已录价：绝不覆盖
	skipNotInFile     = "not_in_file"      // 官方价文件里没有这个模型名
	skipKindMismatch  = "kind_mismatch"    // 名字对上了但种类不同（文本价不能套到视频模型上）
	skipNoPriceInFile = "no_price_in_file" // 文件里有这一条，但它自己就是未定价
	skipInvalidPrice  = "invalid_pricing"  // 文件里那张价目表不合本 kind 的形态
)

// CatalogSource 是两份官网数据文件的来源。生产实现是 *officialsite.Client，
// 测试用桩替换。
//
// 每个 Fetch 的 etag 参数是**上次存下的那个**（空串 = 无条件 GET）：自动更新带上
// 它做条件 GET，官网文件没变就吃 304；手动「立即更新」
// 恒传空串——人点那颗按钮就是要确认云上此刻是什么。
type CatalogSource interface {
	OfficialPricingURL() string
	FetchOfficialPricing(ctx context.Context, etag string) (*officialsite.OfficialPricingFile, error)
	PlatformModelsURL() string
	FetchPlatformModels(ctx context.Context, etag string) (*officialsite.PlatformModelsFile, error)
}

// SetCatalogSource 注入目录数据文件的来源。生产装配恒会注入官网客户端。
//
// 走注入而不是 New 的入参，避免测试装配把 nil 具体指针塞进非 nil 接口。
func (s *Server) SetCatalogSource(src CatalogSource) { s.catalog = src }

func (s *Server) catalogSyncable() bool { return s.catalog != nil }

// catalogActor 是一次同步的**来路**。审计不再记「谁」（设备只有一个管理员），
// 但仍要记得清「这次改价是谁点出来的还是设备自己跑的」——那是翻审计时真正要
// 分辨的一件事，所以留 Manual 这一位与手动路径的 IP。
type catalogActor struct {
	Manual bool   // true = 管理员点了「立即更新」；false = 每小时那次自动检查
	IP     string // 手动路径的对端 IP；自动路径为空
}

// manualCatalogActor 是「立即更新」那条路径的来路。
func manualCatalogActor(r *http.Request) catalogActor {
	return catalogActor{Manual: true, IP: remoteIP(r)}
}

// autoCatalogActor 是自动更新那条路径的来路：没有人，也没有 IP。
func autoCatalogActor() catalogActor { return catalogActor{} }

// trigger 把来路折成审计 detail 里的 by= 段（收敛器共用同一套词汇）。
func (a catalogActor) by() string {
	if a.Manual {
		return catalogByManual
	}
	return catalogByAuto
}

// 审计 detail 的 by= 词汇：一次数据升级到底是谁发起的。
const (
	catalogByManual = "手动"
	catalogByAuto   = "自动检查"
)

// catalogAutoState 是自动更新的进程内状态。零值可用；两个时刻**都是内存态**，
// 进程重启即忘——重启后的独立定时器会立即检查一次，代价是一次条件 GET（多半 304）。
type catalogAutoState struct {
	// single 是手动「立即更新」与自动更新**共用**的单飞锁。恒用 TryLock：
	// 拿不到就不做（自动那条等下一个节拍，手动那条答 409 catalog_sync_busy），
	// 绝不排队——排队会让按钮卡住几十秒，而静默降级成"成功"更糟：界面会显示
	// 一份不是这次取回来的读数。
	single sync.Mutex

	mu            sync.Mutex
	lastCheckedAt time.Time
	lastUpdatedAt time.Time
	// failed 是上一轮自动更新的成败，只用来把日志压成状态翻转两条
	// 日志只在失败状态翻转时写一次，避免官网长期不可达时刷屏。
	failed bool
}

// checked 记下「这一轮检查过了」。取没取到都算——失败不该让下一个定时节拍
// 立刻再打一发。
func (a *catalogAutoState) checked(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheckedAt = now
}

// updated 记下「这一轮真的有文件落库了」。
func (a *catalogAutoState) updated(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUpdatedAt = now
}

// reset 把「上次检查」清零；官网客户端未装配时调用，让以后完成装配后的第一轮
// 能立即检查。lastUpdatedAt 不动——它是既成事实。
func (a *catalogAutoState) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheckedAt = time.Time{}
}

// due 回答「距上次检查够不够一个间隔」。从没检查过（零值）恒为真。
func (a *catalogAutoState) due(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCheckedAt.IsZero() || now.Sub(a.lastCheckedAt) >= catalogAutoInterval
}

// times 读两个时刻（供 GET /admin/v1/system/data 的 automatic 读数）。
func (a *catalogAutoState) times() (checked, updated time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCheckedAt, a.lastUpdatedAt
}

// flip 记下这一轮的成败，并回答「该不该记日志」：只在连通性翻转时各记一条
// （成功→失败一条、失败→成功一条），不每小时刷屏。
func (a *catalogAutoState) flip(failed bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed == failed {
		return false
	}
	a.failed = failed
	return true
}

// storedCatalogFile 是两份目录数据文件共用的落库形态：下载到的文件原样放在
// File 里（"把文件下载到设备上"就是字面意思——不挑字段、不重排、不换算），
// 外面裹一层"什么时候、从哪儿、拿的是哪一版"。
//
// ETag 是取回时响应上的那个（2026-08-15 加）：下一轮自动更新拿它做条件 GET。
// 老固件存下的那些没有这一列，取到空串即退化成一次无条件 GET，下一轮就有了。
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
// 状态翻转两条（口径与 catalogAutoState.flip 相同）：第一次记一条，恢复正常
// 之后再坏才会再记一条。
//
// 为什么非压不可（2026-08-15）：这些读出路径挂在设备设置页的读数与「添加模型」
// 选单上，管理员每开一次页就走一遍。一份坏掉的已存文件（典型来路：固件降级后遇上
// 更新 schema 的那份）会变成"页面刷一次、journal 写一行"——而板上那是 SD 卡。
// 键是「settings 键 + 失败环节」，各环节互不干扰。
type storedCatalogWarn struct {
	mu   sync.Mutex
	seen map[string]bool
}

// fire 报告该不该记这一条（同一个键第一次为真，重复的为假）。
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

// clear 放开某个键的闸（那一环恢复正常了）。
func (w *storedCatalogWarn) clear(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, key)
}

// warnStoredCatalog 记一条已存文件的降级告警（同一状态只记第一条，见
// storedCatalogWarn）。stage 是失败环节：read（读库）/ decode（外层信封）/
// parse（里面那份目录）。
func (s *Server) warnStoredCatalog(key, stage, msg string, err error) {
	if s.storedWarn.fire(key + ":" + stage) {
		s.log.Warn(msg, "err", err)
	}
}

// clearStoredCatalogWarn 标记某个环节恢复正常，下次再坏还会再记一条。
func (s *Server) clearStoredCatalogWarn(key, stage string) {
	s.storedWarn.clear(key + ":" + stage)
}

// readStoredCatalogFile 读回一份已存的目录数据文件。读不到、读坏了都只回
// false（附一条 Warn）：这两份东西是页面上的附属读数，不该把宿主页面拖成
// 500（同"未定价徽章读不到目录时降级为不挂徽章"的处置）。
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
func (s *Server) storedCatalogETag(ctx context.Context, key, what string, conditional bool) string {
	if !conditional {
		return ""
	}
	stored, ok := s.readStoredCatalogFile(ctx, key, what)
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

// officialPricingJSON 是价目那份文件的同步状态读数：能不能点、上次同步了什么。
// 从未同步过时后四个字段整体缺席。
type officialPricingJSON struct {
	Supported bool   `json:"supported"`
	URL       string `json:"url,omitempty"`
	SyncedAt  string `json:"synced_at,omitempty"`
	Version   int64  `json:"version,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Entries   int    `json:"entries,omitempty"`
}

// officialPricingStatus 读同步状态。读库失败**不让宿主页面跟着 500**：它是页面
// 上的一个附属读数，读不到上次同步信息顶多少显示一行字（同"未定价徽章读不到
// 目录时降级为不挂徽章"的处置）。
//
// Supported 只回答「这一刻点得动『立即更新』吗」；**已存的读数与它解耦**
// （2026-08-15）：开关关着照样报 synced_at / version / entries，只是徽章变灰。
// 来源地址优先给活的那个，取不到就报上次是从哪儿取的——两者都是事实。
func (s *Server) officialPricingStatus(ctx context.Context) officialPricingJSON {
	out := officialPricingJSON{Supported: s.catalogSyncable()}
	if out.Supported {
		out.URL = s.catalog.OfficialPricingURL()
	}
	stored, ok := s.readStoredCatalogFile(ctx, settingOfficialPricing, "官方价格")
	if !ok {
		return out
	}
	if out.URL == "" {
		out.URL = stored.URL
	}
	var doc officialsite.OfficialPricing
	if err := json.Unmarshal(stored.File, &doc); err != nil {
		s.warnStoredCatalog(settingOfficialPricing, "parse", "已存的官方价格文件解析失败", err)
		return out
	}
	s.clearStoredCatalogWarn(settingOfficialPricing, "parse")
	out.SyncedAt = stored.FetchedAt
	out.Version = doc.Version
	out.UpdatedAt = doc.UpdatedAt
	out.Entries = len(doc.Models)
	return out
}

// platformModelsJSON 是「平台模型信息」这半边的读数。它与价格那半边有一处**根本
// 不同**：设备恒有一份固件内嵌的基线（platformcatalog.Builtin），所以
// BuiltinVersion 恒在场，Supported 只说「此刻点得动同步吗」，不说「有没有清单可用」。
// Origin 是当前实际生效的那份：builtin | synced。
type platformModelsJSON struct {
	Supported      bool   `json:"supported"`
	URL            string `json:"url,omitempty"`
	Origin         string `json:"origin"`
	Version        int64  `json:"version"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	BuiltinVersion int64  `json:"builtin_version"`
	Platforms      int    `json:"platforms"`
	Models         int    `json:"models"`
	SyncedAt       string `json:"synced_at,omitempty"`
}

// 生效来源的两个取值（对外是稳定的机读值，zh-CN 文案在管理台一侧）。
const (
	catalogOriginBuiltin = "builtin"
	catalogOriginSynced  = "synced"
)

// effectivePlatformModels 挑出当前生效的平台模型清单：同步下来的**版本号更大**
// 时用它，否则用固件内嵌的基线。
//
// 比版本号而不是比同步时间：云上文件只会往前走，而「刚同步过」不等于「更新」
// ——固件升级会带来更新的内嵌基线，那时上次同步的旧副本必须自动让位，否则
// 一次早年的同步会把设备永久钉在旧清单上。存的那份解析不了就当没有（外部文件，
// 降级到基线，附一条 Warn）。
func (s *Server) effectivePlatformModels(ctx context.Context) (platformcatalog.Doc, platformModelsJSON) {
	doc := platformcatalog.Builtin()
	out := platformModelsJSON{
		Origin:         catalogOriginBuiltin,
		Version:        doc.Version,
		UpdatedAt:      doc.UpdatedAt,
		BuiltinVersion: doc.Version,
	}
	if s.catalogSyncable() {
		out.Supported = true
		out.URL = s.catalog.PlatformModelsURL()
	}
	if stored, ok := s.readStoredCatalogFile(ctx, settingPlatformModels, "平台模型信息"); ok {
		if out.URL == "" {
			out.URL = stored.URL
		}
		synced, err := platformcatalog.Parse(stored.File)
		if err == nil {
			s.clearStoredCatalogWarn(settingPlatformModels, "parse")
		}
		switch {
		case err != nil:
			s.warnStoredCatalog(settingPlatformModels, "parse", "已存的平台模型文件解析失败", err)
		case synced.Version > doc.Version:
			doc = synced
			out.Origin = catalogOriginSynced
			out.Version = synced.Version
			out.UpdatedAt = synced.UpdatedAt
			out.SyncedAt = stored.FetchedAt
		default:
			// 存着但没生效（内嵌基线更新）：同步时间照旧报出来，否则界面会
			// 显示成"从没同步过"。
			out.SyncedAt = stored.FetchedAt
		}
	}
	out.Platforms = len(doc.Platforms)
	for _, p := range doc.Platforms {
		out.Models += len(p.Models)
	}
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
	OfficialPricing officialPricingJSON `json:"official_pricing"`
	PlatformModels  platformModelsJSON  `json:"platform_models"`
	Automatic       catalogAutoJSON     `json:"automatic"`
}

// handleDataStatus 返回数据升级读数；官网连接不需要开关、登记或设备凭据。
func (s *Server) handleDataStatus(w http.ResponseWriter, r *http.Request) {
	_, platform := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusOK, dataStatusResponse{
		OfficialPricing: s.officialPricingStatus(r.Context()),
		PlatformModels:  platform,
		Automatic:       s.catalogAutoStatus(),
	})
}

// appliedPricingJSON 是一条"填上了"的结果：填了哪张价目表、这条价目来自哪家、
// 有没有附注（如"厂商已预告涨价"）。Created 表示这一行是本次同步**新建**的
// （历史字段：2026-08-15 起同步本身不再建行，Agent 订阅模型改由收敛器按目录
// agents 段与在场订阅建删，所以它恒为 false；保留是为了不改对外形态）。
type appliedPricingJSON struct {
	Model   string        `json:"model"`
	Kind    string        `json:"kind"`
	Vendor  string        `json:"vendor,omitempty"`
	Pricing usage.Pricing `json:"pricing"`
	Note    string        `json:"note,omitempty" i18n:"text"`
	Created bool          `json:"created,omitempty"`
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
	Source  officialPricingJSON  `json:"source"`
	Applied []appliedPricingJSON `json:"applied"`
	Skipped []skippedPricingJSON `json:"skipped"`
	// PlatformModels 是清单那半边的结果。PlatformError 非空 = 这半边没取成
	// （文案给人看，价格那半边照常已经生效）——设备仍有内嵌基线可用，
	// 所以它不是整次同步的失败。
	PlatformModels platformModelsJSON `json:"platform_models"`
	PlatformError  string             `json:"platform_error,omitempty"`
	// AgentModels 是同步收尾那次 Agent 订阅模型收敛的账（2026-08-15）：
	// 建了/补了/回收了几行。三个数都是 0 就是"订阅模型没变化"。
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
	// 静默降级成"成功"（那会把一份不是这次取回来的读数当成本次结果）。
	// 文案不说"自动更新中"——锁也可能在另一个管理员（或同一个人的双击）手上。
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
	if miss := catalogMissReason(resp); miss != "" {
		s.log.Info("数据升级「立即更新」有文件没取到", "reason", miss)
	}
	writeJSON(w, http.StatusOK, resp)
}

// catalogMissReason 把"哪份没取到"折成一句话（两份都到手时为空串）。价目那份
// 不在其中——它取不到是整次失败，走 error 那条路。
func catalogMissReason(resp catalogSyncResponse) string {
	return resp.PlatformError
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
	// 整条路径（同步的闸门判定与后台那段活儿）都要兜住 panic：调用方是后台定时器，
	// 它没有 withRecovery，而**裸协程里的 panic 会带走整个进程**（数据面陪葬）。
	// 官网从来不是设备的运行依赖——一份写坏的远端文件绝不能停掉这台盒子。
	// 协程里那一道要单独下（defer 不跨协程）。
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
	// 有意的：闸门必须在定时器这一跳同步判完（否则每个节拍都会多起一个协程去
	// 抢锁），而真正的活儿要在后台跑完。
	go func() {
		defer s.catalogAuto.single.Unlock()
		defer s.recoverAutoSync()
		resp, err := s.syncWebsiteData(ctx, autoCatalogActor(), true)
		reason := ""
		if err != nil {
			reason = err.Error()
		} else {
			reason = catalogMissReason(resp)
		}
		// 日志压成状态翻转两条：每小时一轮，取不到就每小时刷一条的话，
		// 一份没发布的文件能把 journal 淹掉。只记原因，文件内容与被跳过条目的
		// 正文都不进日志（§15.1）。
		if !s.catalogAuto.flip(reason != "") {
			return
		}
		if reason != "" {
			s.log.Warn("数据升级自动检查没全取到（官网不是设备的运行依赖）", "reason", reason)
			return
		}
		s.log.Info("数据升级自动检查已恢复")
	}()
}

// recoverAutoSync 是自动更新路径上的 panic 兜底（HTTP 那条有 withRecovery，
// 这条自己兜）。记的与 withRecovery 同形：panic 值与栈，没有任何远端文件内容。
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
// 返回的 error 只有价目那份取不到时才非 nil（*catalogSyncFailure 或库错误）：
// 清单取不到只在响应里回一句原因。
//
// **失败一律不在这里记日志**，只把原因原样回给调用方：两条入口的噪声阈值不同
// ——手动那条每次都该在 journal 留一行（人正等着看结果），自动那条每小时一轮，
// 必须压成连通性翻转两条。核心只记"真的变了什么"（版本号与条目数）。
func (s *Server) syncWebsiteData(ctx context.Context, actor catalogActor, conditional bool) (catalogSyncResponse, error) {
	resp := catalogSyncResponse{
		Applied: []appliedPricingJSON{},
		Skipped: []skippedPricingJSON{},
	}
	if s.catalog == nil {
		return resp, websiteNotConfiguredFailure()
	}
	// 「上次检查」以**开跑**为准：这一轮取没取到都算检查过了，失败不该让下一个
	// 下一个定时节拍立刻再打一发。
	s.catalogAuto.checked(time.Now())

	changed, err := s.fetchOfficialPricing(ctx, conditional)
	if err != nil {
		return resp, err
	}
	platformChanged, platformErr := s.fetchPlatformModels(ctx, conditional)
	resp.PlatformError = platformErr
	if changed || platformChanged {
		s.catalogAuto.updated(time.Now())
	}

	// 本地那两步**每次都跑，304 的那一轮也跑**：它们的输入不只有文件，还有设备
	// 上此刻的模型行与订阅行——管理员昨天新建的模型今天该拿到价，今早连上的订阅
	// 今天该长出模型行，而云上文件一个字节都没改。两步都本地幂等（只填空、不覆盖），
	// 重复跑没有副作用。
	if err := s.applyStoredPricing(ctx, actor, &resp); err != nil {
		return resp, err
	}

	// 读数取**落库之后**的那一份：手动与 304 两条路径因此报的是同一件事实。
	resp.Source = s.officialPricingStatus(ctx)
	_, resp.PlatformModels = s.effectivePlatformModels(ctx)
	// 收尾收敛：厂商在 agents 段上新/下架、或刚取到的价目让某个计价行终于有了价，
	// 都在这一刻生效。放在写响应之前，界面刷新读到的就是收敛后的模型列表。
	resp.AgentModels = s.syncAgentModelsFor(ctx, actor, "catalog_sync")
	return resp, nil
}

// fetchOfficialPricing 取回并落库官方价目文件。**这是唯一一份取不到就算整次
// 失败的文件**——它是这次动作的主产出。返回值说的是"有没有新文件落库"（304 为 false）。
func (s *Server) fetchOfficialPricing(ctx context.Context, conditional bool) (bool, error) {
	// 超时只箍住**网络那一段**：落库与逐条写价随外层 ctx，绝不让一次慢下载把
	// 后面的写操作截在半途（那会留下一半填了价、一半没填的状态，虽然同步幂等
	// 可以重跑，但报给操作者的结果会与库里的实际不符）。
	fetchCtx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()
	etag := s.storedCatalogETag(ctx, settingOfficialPricing, "官方价格", conditional)
	file, err := s.catalog.FetchOfficialPricing(fetchCtx, etag)
	switch {
	case errors.Is(err, officialsite.ErrNotModified):
		// 官网那份没变：不落库。本地那两步照跑（见 syncWebsiteData 的注释）。
		return false, nil
	case err != nil:
		var ce *officialsite.HTTPError
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			return false, &catalogSyncFailure{http.StatusBadGateway, "official_pricing_unavailable",
				"LLM Gate官网还没有发布官方价格文件（404）"}
		}
		return false, &catalogSyncFailure{http.StatusBadGateway, "official_pricing_unavailable",
			"下载官方价格文件失败：" + err.Error()}
	}

	// 索引在落库之前建好：同名条目重复时整批拒绝，不"取第一条"。价目表的
	// 二义性一旦落到账上就是永久的，而这种文件问题只可能来自官网发布内容的
	// 发布错误——报出来让人去修，比静默选一条正确。
	if _, err := indexOfficialPricing(file.Doc.Models); err != nil {
		return false, &catalogSyncFailure{http.StatusBadGateway, "official_pricing_invalid",
			"官方价格文件有问题：" + err.Error()}
	}
	if err := s.storeCatalogFile(ctx, settingOfficialPricing, file.URL, file.Raw, file.ETag); err != nil {
		return false, err
	}
	s.log.Info("官方价格文件已更新", "version", file.Doc.Version, "entries", len(file.Doc.Models))
	return true, nil
}

// fetchPlatformModels 取回并落库平台模型信息文件。**它取不到不算这次同步失败**
// ——设备手上恒有一份能用的（内嵌基线，或上次同步下来的那份），取不到就把原因原样
// 回给界面（一行提示），别把一次成功的价格同步报成红色失败。落库失败与下载失败同等
// 对待（都只是这半边没成）。
//
// 提示只说"这次没更新成、继续用设备上已有的那份"，**不点名是哪一份**：同步过的设备
// 留着的是上次同步的副本，写死"继续用固件内嵌的那份"就是句假话。
func (s *Server) fetchPlatformModels(ctx context.Context, conditional bool) (bool, string) {
	fetchCtx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()
	etag := s.storedCatalogETag(ctx, settingPlatformModels, "平台模型信息", conditional)
	file, err := s.catalog.FetchPlatformModels(fetchCtx, etag)
	switch {
	case errors.Is(err, officialsite.ErrNotModified):
		return false, ""
	case err != nil:
		var ce *officialsite.HTTPError
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			// 官网还没发布这份文件：设备上
			// 已有的那份照常用，说清楚就好。
			return false, "平台模型文件还没发布（404），这次没更新成，「添加模型」的清单继续用设备上已有的那份"
		}
		return false, "平台模型文件没取到（" + err.Error() + "），这次没更新成，「添加模型」的清单继续用设备上已有的那份"
	}
	if stored, ok := s.readStoredCatalogFile(ctx, settingPlatformModels, "平台模型信息"); ok {
		if previous, parseErr := platformcatalog.Parse(stored.File); parseErr == nil {
			switch {
			case file.Doc.Version < previous.Version:
				return false, fmt.Sprintf("平台模型文件版本从 %d 回退到 %d，设备拒绝激活并继续使用已有数据",
					previous.Version, file.Doc.Version)
			case file.Doc.Version == previous.Version:
				oldDigest, oldErr := catalogSHA256(stored.File)
				newDigest, newErr := catalogSHA256(file.Raw)
				if oldErr == nil && newErr == nil && oldDigest != newDigest {
					return false, fmt.Sprintf("平台模型文件版本 %d 未递增但内容发生变化，设备拒绝激活并继续使用已有数据",
						file.Doc.Version)
				}
			}
		}
	}
	if err := s.storeCatalogFile(ctx, settingPlatformModels, file.URL, file.Raw, file.ETag); err != nil {
		s.log.Warn("平台模型文件落库失败", "err", err)
		return false, "平台模型文件存不下来，这次没更新成，「添加模型」的清单继续用设备上已有的那份"
	}
	s.log.Info("平台模型信息已更新", "version", file.Doc.Version, "platforms", len(file.Doc.Platforms))
	return true, ""
}

// applyStoredPricing 用**已存**的价目文件补填未定价的模型（口径 1：只填空、
// 绝不覆盖）。读的是库里那份而不是这一轮下载到的那份，好让 304 的那一轮也照跑。
//
// 已存文件读不到、解不开、或带着重名条目（老固件在校验之前落的库）一律降级为
// "这一轮不补价"并记一条 Warn：补价是同步的产出之一，不该把整次同步拖成 500。
func (s *Server) applyStoredPricing(ctx context.Context, actor catalogActor, resp *catalogSyncResponse) error {
	stored, ok := s.readStoredCatalogFile(ctx, settingOfficialPricing, "官方价格")
	if !ok {
		return nil
	}
	var doc officialsite.OfficialPricing
	if err := json.Unmarshal(stored.File, &doc); err != nil {
		s.log.Warn("已存的官方价格文件解析失败", "err", err)
		return nil
	}
	index, err := indexOfficialPricing(doc.Models)
	if err != nil {
		s.log.Warn("已存的官方价格文件不可用，本轮不补价", "err", err)
		return nil
	}
	// 只读模型自身字段（名/种类/现价），不必拉「模型 × 来源 × 上游」联查。
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return err
	}
	for _, m := range models {
		entry, ok := index[strings.ToLower(m.Name)]
		if !ok {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipNotInFile})
			continue
		}
		// 已定价的先判：管理员的口径优先于官方价，连"种类对不对"都不必问。
		if m.Pricing != "" {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipAlreadyPriced})
			continue
		}
		if entry.Kind != m.Kind {
			resp.Skipped = append(resp.Skipped, skippedPricingJSON{
				Model: m.Name, Kind: m.Kind, Reason: skipKindMismatch})
			continue
		}
		// 与手工录入同一条校验路径：字段集按本地模型的 kind 圈定，值必须是
		// 非负整数微元，成对字段必须齐全，通过后拿到的是规范化后的入库原文。
		pricing, _, perr := parseModelPricing(m.Kind, entry.Pricing)
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
			Detail: fmt.Sprintf("name=%s kind=%s pricing=%s source=official_pricing:v%d by=%s",
				m.Name, m.Kind, pricingAudit(pricing), doc.Version, actor.by()),
			RemoteIP: actor.IP,
		})
		resp.Applied = append(resp.Applied, appliedPricingJSON{
			Model:   m.Name,
			Kind:    m.Kind,
			Vendor:  clipDisplay(entry.Vendor),
			Pricing: modelPricingJSON(pricing),
			Note:    clipDisplay(entry.Note),
		})
	}
	if len(resp.Applied) > 0 || len(resp.Skipped) > 0 {
		s.log.Info("官方价格补填完成",
			"version", doc.Version, "entries", len(doc.Models),
			"applied", len(resp.Applied), "skipped", len(resp.Skipped))
	}
	return nil
}

// indexOfficialPricing 建「模型名（小写）→ 条目」索引。
//
// 匹配口径**只有一条**：模型名不分大小写精确相等。不按来源侧模型 ID 回退、
// 不做别名推断——一个模型可以挂多条来源、各带不同的来源侧 ID，任何"聪明"的
// 兜底匹配都会在某个配置下把甲的价填给乙，而这类错误在账面上看不出来。
//
// 大小写不敏感是有意的：厂商的原始名大小写各不相同（MiniMax-H3 与
// deepseek-v4-pro 同处一张表），而目录里同时存在只差大小写的两个模型属于
// 病态配置，真出现了两条都会被填上同一份价，这与"按名字匹配"的直觉一致。
func indexOfficialPricing(models []officialsite.OfficialPricingModel) (map[string]officialsite.OfficialPricingModel, error) {
	index := make(map[string]officialsite.OfficialPricingModel, len(models))
	for _, m := range models {
		// 名字不合目录规范的条目一律忽略：本地模型名都过了同一道校验，这种
		// 条目本就匹配不到任何东西，留在索引里只会干扰重名检测。
		if validateCatalogName(m.Name) != nil {
			continue
		}
		key := strings.ToLower(m.Name)
		if _, dup := index[key]; dup {
			return nil, fmt.Errorf("模型 %q 有重复的价目条目", clipPricingField(m.Name))
		}
		index[key] = m
	}
	if len(index) == 0 {
		return nil, errors.New("没有一条模型名可用")
	}
	return index, nil
}

// storedPricingByName 从**已存**的官方价格文件建「模型名（小写）→ 价目条目」
// 索引。两个消费方：Agent 订阅模型的收敛器（按名给计价行录官方名义价，
// agentmodels.go）与模型视图的 agent 注记（agentModelIndex 取其中带 agent
// 标记的 text 条目）。
//
// 同名重复取第一条：这种文件同步时会被整批拒绝（indexOfficialPricing），
// 但库里可能存着一份老固件落下的带重名的，取第一条与那侧的报错序一致。
// 读不到、解不开一律降级为空索引（readStoredCatalogFile 已 Warn）：
// 价目是附属读数，绝不把模型列表或一次收敛拖成 500——降级那一刻收敛器什么价
// 都不写（只填不洗，见 agentmodels.go 文件头第 3 条）。
func (s *Server) storedPricingByName(ctx context.Context) map[string]officialsite.OfficialPricingModel {
	stored, ok := s.readStoredCatalogFile(ctx, settingOfficialPricing, "官方价格")
	if !ok {
		return nil
	}
	var doc officialsite.OfficialPricing
	if err := json.Unmarshal(stored.File, &doc); err != nil {
		s.log.Warn("已存的官方价格文件解析失败", "err", err)
		return nil
	}
	idx := make(map[string]officialsite.OfficialPricingModel, len(doc.Models))
	for _, m := range doc.Models {
		key := strings.ToLower(m.Name)
		if _, dup := idx[key]; !dup {
			idx[key] = m
		}
	}
	return idx
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
