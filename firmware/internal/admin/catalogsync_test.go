package admin_test

// 「数据升级」验收（手动「立即更新」与每小时自动更新共用一个核心）。
// 核心是那条不可退让的产品口径：**只填充未定价的模型**——已录价的行绝不能被
// 一次点击改掉。其余用例覆盖名字匹配、种类闸门、同步收尾那次 Agent 订阅模型
// 收敛（口径 4，2026-08-15 起建行由目录 agents 段与在场订阅决定）、坏条目只跳过
// 自己、官网客户端未装配的降级，以及设备设置页读得到两份文件和自动检查状态。
//
// 自动更新那一组在文件末尾（TestAutoSync*）：客户端未装配时零出站、
// 1h 内不重复检查、与手动单飞、304 不落库但本地两步照跑。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type syncSourceDTO struct {
	Supported bool   `json:"supported"`
	URL       string `json:"url"`
	SyncedAt  string `json:"synced_at"`
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
	Entries   int    `json:"entries"`
}

type syncAppliedDTO struct {
	Model   string           `json:"model"`
	Kind    string           `json:"kind"`
	Vendor  string           `json:"vendor"`
	Pricing map[string]int64 `json:"pricing"`
	Note    string           `json:"note"`
	Created bool             `json:"created"`
}

type syncSkippedDTO struct {
	Model  string `json:"model"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type syncPlatformDTO struct {
	Supported      bool   `json:"supported"`
	URL            string `json:"url"`
	Origin         string `json:"origin"`
	Version        int64  `json:"version"`
	UpdatedAt      string `json:"updated_at"`
	BuiltinVersion int64  `json:"builtin_version"`
	Platforms      int    `json:"platforms"`
	Models         int    `json:"models"`
	SyncedAt       string `json:"synced_at"`
}

// catalogAutoDTO 是自动更新那一行读数。
type catalogAutoDTO struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
	LastCheckedAt   string `json:"last_checked_at"`
	LastUpdatedAt   string `json:"last_updated_at"`
}

type dataStatusDTO struct {
	OfficialPricing syncSourceDTO   `json:"official_pricing"`
	PlatformModels  syncPlatformDTO `json:"platform_models"`
	CatalogAuto     catalogAutoDTO  `json:"automatic"`
}

type syncDTO struct {
	Source         syncSourceDTO    `json:"source"`
	Applied        []syncAppliedDTO `json:"applied"`
	Skipped        []syncSkippedDTO `json:"skipped"`
	PlatformModels syncPlatformDTO  `json:"platform_models"`
	PlatformError  string           `json:"platform_error"`
	AgentModels    struct {
		Created int `json:"created"`
		Updated int `json:"updated"`
		Removed int `json:"removed"`
	} `json:"agent_models"`
}

// bodyETag 给桩官网的一份文件算稳定 ETag（内容变化时 ETag 随之变化）。
func bodyETag(body string) string {
	return fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
}

// pricingFileWith 拼一份形态正确的官方价格文件（models 段由调用方给）。
func pricingFileWith(version int, models string) string {
	return fmt.Sprintf(`{"schema":%q,"version":%d,"updated_at":"2026-08-10",
		"currency":"CNY","unit":"micro_yuan","models":[%s]}`,
		officialsite.OfficialPricingSchema, version, models)
}

// catalogWebsite 是桩官网：伺服两份静态数据文件、认 If-None-Match（相符即 304），
// 并记下每一次来访。
type catalogWebsite struct {
	ts *httptest.Server

	mu     sync.Mutex
	bodies map[string]string // 路径 → 内容；空串 = 官网没有这份文件（404）
	hits   int               // 收到的请求总数（含 304）
	served int               // 真的回了正文的次数（304 不算）
	// gate 非 nil 时 handler 在回响应之前等它关闭：把一次同步按在"正在跑"的
	// 状态上，好压手动与自动的单飞。
	gate chan struct{}
	// arrived 每收到一次请求塞一个（有缓冲，没人收也不阻塞）。
	arrived chan struct{}
}

func (c *catalogWebsite) handle(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	body := c.bodies[r.URL.Path]
	c.hits++
	gate := c.gate
	c.mu.Unlock()
	select {
	case c.arrived <- struct{}{}:
	default:
	}
	if gate != nil {
		<-gate
	}
	if body == "" {
		http.NotFound(w, r)
		return
	}
	etag := bodyETag(body)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	c.mu.Lock()
	c.served++
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (c *catalogWebsite) hitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func (c *catalogWebsite) servedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.served
}

// set 换掉某一份文件的内容（ETag 随之改变，下一次条件 GET 就不再是 304）。
func (c *catalogWebsite) set(path, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies[path] = body
}

// hold 让此后的请求都卡在 handler 里，返回放行函数。
func (c *catalogWebsite) hold() func() {
	gate := make(chan struct{})
	c.mu.Lock()
	c.gate = gate
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.gate = nil
			c.mu.Unlock()
			close(gate)
		})
	}
}

// waitArrival 等下一个请求打到桩官网。
func (c *catalogWebsite) waitArrival(t *testing.T) {
	t.Helper()
	select {
	case <-c.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("等桩官网收到请求超时")
	}
}

// serveOfficialPricing 起一个只伺服价格文件的桩官网（另一份恒 404——它取不到
// 不该拖垮价格同步，本文件的其余用例都顺带压着这条）。
func (e *env) serveOfficialPricing(body string) *catalogWebsite {
	return e.serveCatalog(body, "")
}

// serveCatalog 起一个伺服两份官网数据文件的桩站点并把它注入管理面。
// 某一份的 body 为空 = 那份文件在官网不存在（404）。
//
// 每份按内容算一个 ETag 并认 If-None-Match（相符即 304 空体）：自动更新走的是
// 条件 GET，桩官网不认这个头就无法验证“没变不落库”。
func (e *env) serveCatalog(pricingBody, platformBody string) *catalogWebsite {
	e.t.Helper()
	c := &catalogWebsite{
		bodies: map[string]string{
			officialsite.OfficialPricingPath: pricingBody,
			officialsite.PlatformModelsPath:  platformBody,
		},
		arrived: make(chan struct{}, 64),
	}
	c.ts = httptest.NewServer(http.HandlerFunc(c.handle))
	e.t.Cleanup(c.ts.Close)
	client := officialsite.NewClient(c.ts.URL, nil)
	e.srv.SetCatalogSource(client)
	return c
}

// syncWebsiteData 点一次「立即更新」（手动路径，无条件 GET）。
func (e *env) syncWebsiteData(cookie string) syncDTO {
	e.t.Helper()
	resp := e.do("POST", catalogSyncPath, cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out syncDTO
	decodeInto(e.t, resp, &out)
	return out
}

// catalogSyncPath 是「立即更新」端点。
const catalogSyncPath = "/admin/v1/system/data/update"

// syncWebsiteDataWhenIdle 等自动更新把单飞锁还回来之后再点「立即更新」。
// 自动那轮**跑完才放锁**（它还要走本地补价与订阅模型收敛），而用例常常紧接着
// 就要点一下——不等就会撞上 409。那个 409 本身是 TestManualSyncBusyWhileAutoRuns
// 要压的行为，别的用例不该被它绊住。
func (e *env) syncWebsiteDataWhenIdle(cookie string) syncDTO {
	e.t.Helper()
	var out syncDTO
	waitFor(e.t, "自动更新放开单飞锁", func() bool {
		resp := e.do("POST", catalogSyncPath, cookie, "")
		if resp.StatusCode == http.StatusConflict {
			_ = readAll(e.t, resp)
			return false
		}
		wantStatus(e.t, resp, http.StatusOK)
		decodeInto(e.t, resp, &out)
		return true
	})
	return out
}

// dataStatus 读设备设置页「数据升级」分区的全部读数。
func (e *env) dataStatus(cookie string) dataStatusDTO {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/system/data", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out dataStatusDTO
	decodeInto(e.t, resp, &out)
	return out
}

// officialPricingStatus 读取数据升级状态中的官方目录价状态。
func (e *env) officialPricingStatus(cookie string) syncSourceDTO {
	e.t.Helper()
	return e.dataStatus(cookie).OfficialPricing
}

// waitFor 等一个条件成立——自动更新在自己的 goroutine 中执行，所以断言前
// 必须等它落地。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等 %s 超时", what)
}

// modelsByName 把模型列表折成名字索引。
func (e *env) modelsByName(cookie string) map[string]modelDTO {
	e.t.Helper()
	out := map[string]modelDTO{}
	for _, m := range e.listModels(cookie) {
		out[m.Name] = m
	}
	return out
}

func skipReasons(t *testing.T, got []syncSkippedDTO) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, s := range got {
		out[s.Model] = s.Reason
	}
	return out
}

// TestSyncOfficialPricingFillsOnlyUnpriced 是这个功能的主用例，也是它的守门线：
// 未定价的填上，已定价的原样不动。
func TestSyncOfficialPricingFillsOnlyUnpriced(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(7, `
		{"name":"demo-chat","kind":"text","vendor":"Demo 家",
		 "pricing":{"in":1000000,"cache_read":20000,"out":2000000},
		 "note":"厂商已预告调价"},
		{"name":"demo-priced","kind":"text",
		 "pricing":{"in":9000000,"out":9000000}},
		{"name":"demo-video","kind":"text",
		 "pricing":{"in":1,"out":1}}`))

	e.createModel(root, "demo-chat")
	// 管理员自己录过价的那条：官方价与它不同，同步后必须还是管理员的数。
	e.createModelBody(root, `{"name":"demo-priced","pricing":{"in":123456,"out":654321}}`)
	// 名字对上了但种类不同：文本价套不到视频模型上。
	e.createModelKind(root, "demo-video", "video")
	// 官方价文件里根本没有的模型。
	e.createModel(root, "demo-unknown")

	got := e.syncWebsiteData(root)

	if len(got.Applied) != 1 || got.Applied[0].Model != "demo-chat" {
		t.Fatalf("applied = %+v，期望只有 demo-chat", got.Applied)
	}
	applied := got.Applied[0]
	if applied.Pricing["in"] != 1000000 || applied.Pricing["cache_read"] != 20000 ||
		applied.Pricing["out"] != 2000000 {
		t.Errorf("填入的价目 = %v", applied.Pricing)
	}
	if applied.Vendor != "Demo 家" || applied.Note != "厂商已预告调价" {
		t.Errorf("厂商/附注没带回来：vendor=%q note=%q", applied.Vendor, applied.Note)
	}

	reasons := skipReasons(t, got.Skipped)
	for model, want := range map[string]string{
		"demo-priced":  "already_priced",
		"demo-video":   "kind_mismatch",
		"demo-unknown": "not_in_file",
	} {
		if reasons[model] != want {
			t.Errorf("%s 的跳过原因 = %q，期望 %q", model, reasons[model], want)
		}
	}

	// 库里的真实结果：这才是"钱按多少记"的权威。
	models := e.modelsByName(root)
	if models["demo-chat"].Pricing["out"] != 2000000 {
		t.Errorf("demo-chat 落库价 = %v", models["demo-chat"].Pricing)
	}
	if got := models["demo-priced"].Pricing; got["in"] != 123456 || got["out"] != 654321 {
		t.Fatalf("已定价的模型被官方价覆盖了：%v", got)
	}
	if len(models["demo-video"].Pricing) != 0 {
		t.Errorf("种类不符的模型被填了价：%v", models["demo-video"].Pricing)
	}
}

// TestSyncOfficialPricingIsIdempotent 再同步一次不该重复改价：第一次填完，
// 模型就"已定价"了，第二次一律走 already_priced。
func TestSyncOfficialPricingIsIdempotent(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(1,
		`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`))
	e.createModel(root, "demo-chat")

	if first := e.syncWebsiteData(root); len(first.Applied) != 1 {
		t.Fatalf("首次同步 applied = %+v", first.Applied)
	}
	second := e.syncWebsiteData(root)
	if len(second.Applied) != 0 {
		t.Errorf("二次同步又改了一遍价：%+v", second.Applied)
	}
	if r := skipReasons(t, second.Skipped)["demo-chat"]; r != "already_priced" {
		t.Errorf("二次同步的跳过原因 = %q，期望 already_priced", r)
	}
}

// TestSyncOfficialPricingMatchesNameCaseInsensitively：厂商原始名大小写各异
// （MiniMax-H3 与 deepseek-v4-pro 同处一张表），匹配不该被大小写卡住。
func TestSyncOfficialPricingMatchesNameCaseInsensitively(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(1,
		`{"name":"MiniMax-H3","kind":"video","pricing":{"minimax_video_sec_768p":500000,"minimax_video_sec_2k":800000}}`))
	e.createModelKind(root, "minimax-h3", "video")

	got := e.syncWebsiteData(root)
	if len(got.Applied) != 1 || got.Applied[0].Pricing["minimax_video_sec_2k"] != 800000 {
		t.Fatalf("applied = %+v", got.Applied)
	}
}

// TestSyncOfficialPricingBadEntryOnlySkipsItself：一个厂商改了计费形态（文件
// 里出现本 kind 不认的字段），不该连累其余条目同步不了。
func TestSyncOfficialPricingBadEntryOnlySkipsItself(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(3, `
		{"name":"demo-good","kind":"text","pricing":{"in":1000000,"out":2000000}},
		{"name":"demo-bad","kind":"text","pricing":{"per_request":100}},
		{"name":"demo-half","kind":"text","pricing":{"in":1000000}},
		{"name":"demo-empty","kind":"text","pricing":null}`))
	for _, n := range []string{"demo-good", "demo-bad", "demo-half", "demo-empty"} {
		e.createModel(root, n)
	}

	got := e.syncWebsiteData(root)
	if len(got.Applied) != 1 || got.Applied[0].Model != "demo-good" {
		t.Fatalf("applied = %+v，期望只有 demo-good", got.Applied)
	}
	reasons := skipReasons(t, got.Skipped)
	// 未知字段与"只填一半"都走手工录入那条校验路径，报的是同一类错。
	if reasons["demo-bad"] != "invalid_pricing" || reasons["demo-half"] != "invalid_pricing" {
		t.Errorf("坏条目的跳过原因 = %v", reasons)
	}
	if reasons["demo-empty"] != "no_price_in_file" {
		t.Errorf("空价目条目的跳过原因 = %q，期望 no_price_in_file", reasons["demo-empty"])
	}
	// 形态错误的细节要带回给管理员，便于维护方修正官网静态文件。
	for _, s := range got.Skipped {
		if s.Model == "demo-bad" && !strings.Contains(s.Detail, "per_request") {
			t.Errorf("invalid_pricing 没说明是哪个字段：%q", s.Detail)
		}
	}
}

// TestSyncOfficialPricingRejectsDuplicateEntries：同名条目重复 = 文件有二义性，
// 整批拒绝而不是"取第一条"——价目一旦落到账上就是永久的。
func TestSyncOfficialPricingRejectsDuplicateEntries(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(1, `
		{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}},
		{"name":"DEMO-CHAT","kind":"text","pricing":{"in":9000000,"out":9000000}}`))
	e.createModel(root, "demo-chat")

	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if got := errCode(t, resp); got != "official_pricing_invalid" {
		t.Errorf("error.code = %q，期望 official_pricing_invalid", got)
	}
	if p := e.modelsByName(root)["demo-chat"].Pricing; len(p) != 0 {
		t.Errorf("整批拒绝后仍然写了价：%v", p)
	}
}

// TestSyncCatalogConvergesAgentModels：同步收尾那次收敛（2026-08-15 口径 4）
// ——**建行不再由价目文件的 agent 标记决定**，而是 platform-models.json 的
// agents 段说了算：连着的订阅按目录长出模型行，文本行的名义价从刚同步下来的
// 价目文件里取；厂商在下一版目录里撤掉某个型号，下一次同步就把那行回收。
// 没连的订阅一行都不建（这是「只要连接了订阅就读出来显示」的字面实现）。
func TestSyncCatalogConvergesAgentModels(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderGrok)
	// 收敛过一轮了（内嵌基线）：这里换一份**版本号更大**的云上目录，只留一个
	// 文本模型，其余的应当被回收。
	pricing := pricingFileWith(7, `
		{"name":"grok-4.6","kind":"text","agent":"grok","vendor":"xAI",
		 "pricing":{"in":13500000,"out":40500000}},
		{"name":"gpt-5.6-sol","kind":"text","agent":"codex",
		 "pricing":{"in":33750000,"out":202500000}},
		{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)
	platform := platformFileWithAgents(99, `{"type":"deepseek","models":[{"name":"demo-chat","kind":"text"}]}`,
		`{"provider":"grok","models":[{"name":"grok-4.6","kind":"text"}]},
		 {"provider":"codex","models":[{"name":"gpt-5.6-sol","kind":"text"}]}`)
	e.serveCatalog(pricing, platform)

	got := e.syncWebsiteData(root)
	if got.AgentModels.Removed == 0 {
		t.Errorf("新目录撤掉的型号应当被回收：%+v", got.AgentModels)
	}
	models := e.modelsByName(root)
	// 留在目录里的那一个：文本行按刚同步的价录上名义价、注记归 grok。
	m, ok := models["grok-4.6"]
	if !ok || m.Agent != store.AgentProviderGrok || m.Pricing["in"] != 13500000 || len(m.Sources) != 0 {
		t.Errorf("grok-4.6 = %+v，期望带 grok 注记与官方名义价、无来源", m)
	}
	// 新目录里没有的型号：回收干净。
	for _, name := range []string{"grok-4.5"} {
		if _, ok := models[name]; ok {
			t.Errorf("目录里已撤掉的 %s 仍在", name)
		}
	}
	// 没连 Codex：它在目录里有条目也不建行。
	if _, ok := models["gpt-5.6-sol"]; ok {
		t.Error("没连 Codex 订阅却建了它的模型行")
	}

	// 幂等：同一份文件再同步一次，收敛的三个数都归零。
	again := e.syncWebsiteData(root)
	if again.AgentModels.Created != 0 || again.AgentModels.Updated != 0 || again.AgentModels.Removed != 0 {
		t.Errorf("重跑同步的收敛账 = %+v，期望全零", again.AgentModels)
	}
}

// TestSyncCatalogCodexOfficialPrices 用实际发布数据验证 Codex 模型与三价落库。
func TestSyncCatalogCodexOfficialPrices(t *testing.T) {
	path := filepath.Join("..", "..", "..", "website", "public", "updates", "data", "official-pricing.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("独立固件源码构建不含官网价目文件")
	}
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderCodex)
	e.serveCatalog(string(raw), string(platformcatalog.BuiltinRaw()))
	e.syncWebsiteData(root)

	models := e.modelsByName(root)
	// 标准档美元价按 1 USD = 6.75 CNY 折算；全程整数微元。
	for name, prices := range map[string][3]int64{
		"gpt-6-astra":   {67_500_000, 6_750_000, 337_500_000},
		"gpt-5.6-sol":   {27_000_000, 2_700_000, 135_000_000},
		"gpt-5.6-terra": {13_500_000, 1_350_000, 81_000_000},
		"gpt-5.6-luna":  {1_350_000, 135_000, 8_100_000},
	} {
		m, ok := models[name]
		if !ok || m.Agent != store.AgentProviderCodex || m.Kind != "text" || len(m.Sources) != 0 {
			t.Errorf("%s 未按 Codex 订阅模型落库：%+v", name, m)
			continue
		}
		for i, field := range []string{"in", "cache_read", "out"} {
			if got := m.Pricing[field]; got != prices[i] {
				t.Errorf("%s %s = %d，期望 %d", name, field, got, prices[i])
			}
		}
	}
	// 同一数据版本再次同步不重复改价或建行。
	again := e.syncWebsiteData(root)
	if again.AgentModels.Created != 0 || again.AgentModels.Updated != 0 || again.AgentModels.Removed != 0 {
		t.Errorf("重复同步仍修改订阅模型：%+v", again.AgentModels)
	}
}

// TestModelsCarryAgentAnnotation：agent 注记（modelJSON.Agent）逐次按**模型目录
// 数据的 agents 段 + 已存官方价文件**算出、不落库。三条边界：目录点过名的
// 名字开箱即注记（内嵌基线在场，不必先同步）、kind 不是 text 不注（文件里的
// 异种同名是发布错误）、两份文件都没提过的名字恒空。
func TestModelsCarryAgentAnnotation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 内嵌目录里就有 grok-4.6：连上订阅即长出带注记的行，一次同步都不必。
	seedAgentSubscription(t, e, store.AgentProviderGrok)
	if got := e.modelsByName(root)["grok-4.6"].Agent; got != store.AgentProviderGrok {
		t.Fatalf("未同步过时 grok-4.6 的注记 = %q，期望 grok（内嵌目录已收录）", got)
	}
	// 与文件里 text agent 条目同名的 video 行：注记不认（kind 闸门）。
	e.createModelKind(root, "seedance-agent", "video")

	// 价目文件里带 agent 标记、但目录 agents 段没收录的名字（claude-mythos-5
	// 是这样一个真例：Anthropic 有它的价，但它不在设备目录里）：注记照样认得出来。
	e.serveOfficialPricing(pricingFileWith(9, `
		{"name":"claude-mythos-5","kind":"text","agent":"claude",
		 "pricing":{"in":67500000,"out":337500000}},
		{"name":"seedance-agent","kind":"text","agent":"codex",
		 "pricing":{"in":1000000,"out":2000000}},
		{"name":"deepseek-v4-pro","kind":"text",
		 "pricing":{"in":3000000,"out":6000000}}`))
	e.syncWebsiteData(root)

	models := e.modelsByName(root)
	if got := models["seedance-agent"].Agent; got != "" {
		t.Errorf("video 行不该带注记：%q", got)
	}
	if _, ok := models["claude-mythos-5"]; ok {
		t.Error("同步不再按价目文件建行（建行由目录 agents 段与在场订阅决定）")
	}
	// 无 agent 标记、也不在目录里的名字：建得出来，且不带注记。
	if m := e.createModel(root, "deepseek-v4-pro"); m.Agent != "" {
		t.Errorf("无 agent 标记的名字不该带注记：%q", m.Agent)
	}
	// 只在价目文件里露过面、目录 agents 段没收录的名字**照旧建得出来**：那是
	// 「手工建同名 text 行给订阅流量借价」这条既有旋钮
	// （docs/firmware-agents-claude.md「计量与限额」），它不是 Agent 订阅模型，
	// 收敛器不认领、也不回收它。挡在通用建模面之外的只有目录点过名的那些
	// （claude 那批自 2026-08-19 起就在目录里了，见下面 grok-4.5 那条同款断言）。
	made := e.createModel(root, "claude-mythos-5")
	if made.Agent != "" {
		t.Errorf("目录 agents 段没有的名字不该带注记：%q", made.Agent)
	}
	resp := e.do("POST", "/admin/v1/models", root, `{"name":"grok-4.5","kind":"text"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "model_agent_managed" {
		t.Errorf("建目录点过名的 grok-4.5 错误码 = %q，期望 model_agent_managed", code)
	}
}

// TestSyncOfficialPricingStoresFileAndStatus：文件本身要落到设备上，设备设置
// 页面靠数据升级读数渲染面板与按钮副标题（不为它多发一次请求）。
func TestSyncOfficialPricingStoresFileAndStatus(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	file := pricingFileWith(42,
		`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)
	site := e.serveOfficialPricing(file)

	if before := e.officialPricingStatus(root); !before.Supported || before.SyncedAt != "" {
		t.Fatalf("同步前状态 = %+v，期望 supported 且未同步过", before)
	}
	got := e.syncWebsiteData(root)
	if got.Source.Version != 42 || got.Source.Entries != 1 || got.Source.SyncedAt == "" {
		t.Errorf("同步响应里的来源信息 = %+v", got.Source)
	}
	if got.Source.URL != site.ts.URL+officialsite.OfficialPricingPath {
		t.Errorf("来源地址 = %q", got.Source.URL)
	}

	status := e.officialPricingStatus(root)
	if status.Version != 42 || status.UpdatedAt != "2026-08-10" || status.SyncedAt == "" {
		t.Errorf("列表捎带的状态 = %+v", status)
	}

	// 落库的是**原文**：下次要按它渲染官方价、也便于人工核对。
	raw, err := e.st.GetSetting(context.Background(), "official_pricing")
	if err != nil {
		t.Fatalf("读 settings: %v", err)
	}
	var stored struct {
		FetchedAt string          `json:"fetched_at"`
		URL       string          `json:"url"`
		File      json.RawMessage `json:"file"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("落库形态不对: %v", err)
	}
	// 落库的是下载到的那份文件本身（仅 JSON 紧凑化，键序与数值不动）——
	// 不是挑几个字段重新拼的摘要。
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(file)); err != nil {
		t.Fatalf("json.Compact: %v", err)
	}
	if string(stored.File) != want.String() {
		t.Errorf("落库的不是下载到的那份文件：\n%s", stored.File)
	}
}

// TestSyncOfficialPricingAudited：改价即改钱，走与手工改价同一个审计事件，
// detail 要能看出这次是官方价同步来的。
func TestSyncOfficialPricingAudited(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing(pricingFileWith(9,
		`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`))
	e.createModel(root, "demo-chat")
	e.syncWebsiteData(root)

	details := e.auditDetails("model.pricing")
	if len(details) != 1 {
		t.Fatalf("model.pricing 审计条数 = %d，期望 1；%v", len(details), details)
	}
	for _, want := range []string{"name=demo-chat", "in:1000000", "source=official_pricing:v9"} {
		if !strings.Contains(details[0], want) {
			t.Errorf("审计 detail 缺 %q：%s", want, details[0])
		}
	}
}

// TestSyncOfficialPricingNotConfigured：官网客户端未装配时端点明确报错，数据升级读数
// 里的 supported 为 false（设备设置页据此把按钮置灰）。
func TestSyncOfficialPricingNotConfigured(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	if s := e.officialPricingStatus(root); s.Supported || s.URL != "" {
		t.Errorf("未接入云时的状态 = %+v", s)
	}
	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if got := errCode(t, resp); got != "website_not_configured" {
		t.Errorf("error.code = %q，期望 website_not_configured", got)
	}
}

// TestSyncOfficialPricingUnavailable：官网还没发布这份文件时
// SPA fallback 用 200 回 index.html）时报 502，且不动任何价。
func TestSyncOfficialPricingUnavailable(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveOfficialPricing("<!doctype html><html><body>SOC AGENT</body></html>")
	e.createModel(root, "demo-chat")

	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if got := errCode(t, resp); got != "official_pricing_unavailable" {
		t.Errorf("error.code = %q，期望 official_pricing_unavailable", got)
	}
	if p := e.modelsByName(root)["demo-chat"].Pricing; len(p) != 0 {
		t.Errorf("下载失败却写了价：%v", p)
	}
}

// TestSyncOfficialPricingRequiresSession：无会话打不动改价这条路
// （withSession 的默认拒绝覆盖，这里钉住它没被某条白名单漏掉）。
func TestSyncOfficialPricingRequiresSession(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	e.serveOfficialPricing(pricingFileWith(1,
		`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`))

	resp := e.do("POST", catalogSyncPath, "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
}

// platformFileWith 拼一份形态正确的平台模型信息文件（只有 platforms 段）。
func platformFileWith(version int, platforms string) string {
	return platformFileWithAgents(version, platforms, "")
}

// platformFileWithAgents 同上，另带 agents 段（v2 的「哪份订阅有哪些模型」）。
func platformFileWithAgents(version int, platforms, agents string) string {
	return fmt.Sprintf(`{"schema":"llmgate.platform-models/v2","version":%d,
		"updated_at":"2026-08-15","platforms":[%s],"agents":[%s]}`, version, platforms, agents)
}

// TestSyncCatalogTakesBothFiles：一次点击取两份文件——价格那份照旧填价，平台
// 模型那份落库后立刻成为「添加模型」的选单（版本号大于内嵌基线才生效）。
func TestSyncCatalogTakesBothFiles(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveCatalog(
		pricingFileWith(3, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`),
		platformFileWith(9999, `{"type":"deepseek","vendor":"DeepSeek","source":"https://example.test/models",
			"checked_at":"2026-08-15","models":[{"name":"site-only-model","kind":"text"}]}`))

	got := e.syncWebsiteData(root)
	if got.PlatformError != "" {
		t.Fatalf("平台模型同步报了错：%s", got.PlatformError)
	}
	if got.PlatformModels.Origin != "synced" || got.PlatformModels.Version != 9999 {
		t.Fatalf("平台模型状态 = %+v，期望同步来的那份生效", got.PlatformModels)
	}
	if got.PlatformModels.BuiltinVersion <= 0 || got.PlatformModels.SyncedAt == "" {
		t.Errorf("平台模型状态缺内嵌版本或同步时间：%+v", got.PlatformModels)
	}

	// 设备设置页只发一次请求就要拿到两份文件 + 自动更新的全部读数。
	status := e.dataStatus(root)
	if status.PlatformModels.Version != 9999 || status.PlatformModels.Origin != "synced" {
		t.Errorf("数据升级读数的平台模型状态 = %+v", status.PlatformModels)
	}
	if !status.CatalogAuto.Enabled || status.CatalogAuto.IntervalSeconds != 3600 {
		t.Errorf("catalog_auto = %+v，期望开着且间隔 1h", status.CatalogAuto)
	}
	if status.CatalogAuto.LastCheckedAt == "" || status.CatalogAuto.LastUpdatedAt == "" {
		t.Errorf("手动「立即更新」也该刷新两个时刻：%+v", status.CatalogAuto)
	}

	// 选单立刻换成同步来的那份。
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	list := e.upstreamModels(root, up.ID)
	if list.Catalog.Origin != "synced" || len(list.Models) != 1 || list.Models[0].Name != "site-only-model" {
		t.Fatalf("添加模型清单 = %+v / %+v", list.Catalog, list.Models)
	}
}

// TestDataUpgradeAddsPlatformAndSnapshotsEndpoint 钉住数据升级的完整产品路径：
// 新平台只复用固件已有兼容适配器，管理台立刻能列出并创建账号；账号保存平台
// 身份、计费方式和端点快照，后续目录换地址不会把已封存的 Key 静默改发过去。
func TestDataUpgradeAddsPlatformAndSnapshotsEndpoint(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	platformFile := func(version int, endpoint string) string {
		return fmt.Sprintf(`{"schema":"llmgate.platform-models/v2","version":%d,"updated_at":"2026-08-24","platforms":[{
			"id":"example_ai","type":"openai_compat","vendor":"Example AI","base_url":%q,
			"billing_mode":"usage","suggested_name":"example-ai","entries":"OpenAI 兼容单入口",
			"ability":"数据目录平台","models":[{"name":"example-reasoner","kind":"text"}]
		}],"agents":[]}`, version, endpoint)
	}
	price := `{"name":"example-reasoner","kind":"text","pricing":{"in":1000000,"out":2000000}}`
	e.serveCatalog(pricingFileWith(3, price), platformFile(9999, "https://api.example.invalid/v1"))
	if got := e.syncWebsiteData(root); got.PlatformError != "" || got.PlatformModels.Origin != "synced" {
		t.Fatalf("新平台数据未生效：%+v", got)
	}

	resp := e.do("GET", "/admin/v1/upstream-platforms", root, "")
	wantStatus(t, resp, http.StatusOK)
	var listed struct {
		Platforms []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Vendor      string `json:"vendor"`
			BillingMode string `json:"billing_mode"`
		} `json:"platforms"`
	}
	decodeInto(t, resp, &listed)
	if len(listed.Platforms) != 1 || listed.Platforms[0].ID != "example_ai" ||
		listed.Platforms[0].Type != "openai_compat" || listed.Platforms[0].BillingMode != "usage" {
		t.Fatalf("平台预设没有按升级数据生成：%+v", listed.Platforms)
	}

	up := e.createUpstream(root, fmt.Sprintf(
		`{"name":"example-account","type":"openai_compat","catalog_id":"example_ai","api_key":%q}`,
		upstreamKeyPlaintext))
	if up.CatalogID != "example_ai" || up.PlatformLabel != "Example AI" || up.BillingMode != "usage" ||
		up.BaseURL != "https://api.example.invalid/v1" {
		t.Fatalf("数据平台账号快照不完整：%+v", up)
	}
	models := e.upstreamModels(root, up.ID)
	if len(models.Models) != 1 || models.Models[0].Name != "example-reasoner" {
		t.Fatalf("数据平台没有自己的模型清单：%+v", models.Models)
	}
	e.addUpstreamModels(root, up.ID, `{"models":[{"name":"example-reasoner","kind":"text"}]}`)
	source := e.modelsByName(root)["example-reasoner"].Sources[0]
	if source.UpstreamCatalogID != "example_ai" || source.UpstreamPlatformLabel != "Example AI" ||
		source.UpstreamBillingMode != "usage" || source.Priority != 200 {
		t.Fatalf("动态平台来源没有沿用平台身份与计费优先级：%+v", source)
	}

	e.serveCatalog(pricingFileWith(4, price), platformFile(10000, "https://api.changed.invalid/v1"))
	e.syncWebsiteData(root)
	for _, saved := range e.listUpstreams(root) {
		if saved.ID == up.ID && saved.BaseURL != "https://api.example.invalid/v1" {
			t.Fatalf("后续数据升级改写了已有账号端点：%+v", saved)
		}
	}
	e.serveCatalog(pricingFileWith(5, price), platformFile(9998, "https://api.rollback.invalid/v1"))
	rollback := e.syncWebsiteData(root)
	if !strings.Contains(rollback.PlatformError, "拒绝激活") || rollback.PlatformModels.Version != 10000 {
		t.Fatalf("目录降级没有被拒绝并保留当前版本：%+v", rollback.PlatformModels)
	}
}

// TestSyncCatalogKeepsBuiltinWhenOlder：同步来的版本不比内嵌的新时，内嵌基线
// 继续生效——固件升级带来的新基线不该被一次早年的同步永久压住。
func TestSyncCatalogKeepsBuiltinWhenOlder(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveCatalog(
		pricingFileWith(3, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`),
		platformFileWith(1, `{"type":"deepseek","models":[{"name":"stale-model","kind":"text"}]}`))

	got := e.syncWebsiteData(root)
	if got.PlatformError != "" {
		t.Fatalf("平台模型同步报了错：%s", got.PlatformError)
	}
	if got.PlatformModels.Origin != "builtin" {
		t.Fatalf("平台模型状态 = %+v，期望内嵌基线继续生效", got.PlatformModels)
	}
	// 存过了就该报出同步时间，否则界面会显示成"从没同步过"。
	if got.PlatformModels.SyncedAt == "" {
		t.Errorf("同步时间缺席：%+v", got.PlatformModels)
	}
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	if _, ok := findUpModel(e.upstreamModels(root, up.ID).Models, "stale-model"); ok {
		t.Error("旧版本的清单不该生效")
	}
}

// TestSyncCatalogSurvivesMissingPlatformFile：平台模型文件缺席（云上还没发布）
// 不让整次同步失败——价格照常填好，界面收到一句原因。
func TestSyncCatalogSurvivesMissingPlatformFile(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	e.serveOfficialPricing(pricingFileWith(3, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`))

	got := e.syncWebsiteData(root)
	if len(got.Applied) != 1 || got.Applied[0].Model != "demo-chat" {
		t.Fatalf("价格没填上：%+v", got.Applied)
	}
	if got.PlatformError == "" {
		t.Error("平台模型文件 404 应当在响应里说明")
	}
	if got.PlatformModels.Origin != "builtin" || got.PlatformModels.SyncedAt != "" {
		t.Errorf("平台模型状态 = %+v，期望仍是没同步过的内嵌基线", got.PlatformModels)
	}
}

func TestCatalogReadoutsComeFromWebsite(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveCatalog(
		pricingFileWith(42, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`),
		platformFileWith(9999, `{"type":"deepseek","models":[{"name":"site-only-model","kind":"text"}]}`))
	e.syncWebsiteData(root)

	got := e.dataStatus(root)
	if !got.OfficialPricing.Supported || !got.PlatformModels.Supported {
		t.Errorf("接入官网时两份的 supported 都该是 true：%+v", got)
	}
	if !got.CatalogAuto.Enabled {
		t.Errorf("接入官网时 automatic.enabled 应为 true：%+v", got.CatalogAuto)
	}
	if got.OfficialPricing.SyncedAt == "" || got.PlatformModels.SyncedAt == "" {
		t.Fatalf("上次同步的时间被开关藏起来了：%+v", got)
	}
	if got.OfficialPricing.Version != 42 || got.PlatformModels.Version != 9999 {
		t.Errorf("上次同步的版本被开关藏起来了：%+v", got)
	}
	if got.OfficialPricing.URL == "" || got.PlatformModels.URL == "" {
		t.Errorf("上次是从哪儿取的也该照旧报出：%+v", got)
	}
}

// ---- 自动更新（每小时节拍，条件 GET）----

// waitAutoSync 等自动更新 goroutine 完成。
func (e *env) waitAutoSync(t *testing.T, site *catalogWebsite, wantHits int) {
	t.Helper()
	waitFor(t, "自动更新落地", func() bool { return site.hitCount() >= wantHits })
}

// autoPricingFile 是自动更新用例共用的价目文件（两个未定价模型 + 一个已定价的）。
func autoPricingFile(version int) string {
	return pricingFileWith(version, `
		{"name":"demo-chat","kind":"text","vendor":"Demo 家","pricing":{"in":1000000,"out":2000000}},
		{"name":"demo-two","kind":"text","pricing":{"in":3000000,"out":4000000}},
		{"name":"demo-priced","kind":"text","pricing":{"in":9000000,"out":9000000}}`)
}

// TestAutoSyncChecksAtMostOncePerInterval：距上次检查不足一个间隔（1h）的那些
// 节拍未到直接返回，避免频繁请求官网静态文件。
func TestAutoSyncChecksAtMostOncePerInterval(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	site := e.serveCatalog(autoPricingFile(5), "")
	e.createModel(root, "demo-chat")

	e.srv.AutoSyncData(context.Background())
	e.waitAutoSync(t, site, 2)
	waitFor(t, "第一轮落地", func() bool {
		return e.modelsByName(root)["demo-chat"].Pricing["in"] == 1000000
	})
	before := site.hitCount()

	// 紧接着的两个节拍：闸门在最前面，一个字节都不该再出去。
	e.srv.AutoSyncData(context.Background())
	e.srv.AutoSyncData(context.Background())
	if n := site.hitCount(); n != before {
		t.Errorf("一小时内又检查了：请求数 %d → %d", before, n)
	}
	// 手动「立即更新」不受这道闸约束（人点的就该真的去取）。
	e.syncWebsiteDataWhenIdle(root)
	if n := site.hitCount(); n <= before {
		t.Errorf("手动「立即更新」被自动更新那道闸挡住了：请求数 = %d", n)
	}
}

// TestAutoSyncFillsOnlyUnpricedMarkedAuto：自动路径与手动路径同一条口径
// （只填未定价的模型），但审计上要看得出来**这次改价不是谁点出来的**——
// 审计不再记 actor（设备只有一个操作者），来路记在 detail 的 by= 段里。
func TestAutoSyncFillsOnlyUnpricedMarkedAuto(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	site := e.serveCatalog(autoPricingFile(5), "")
	e.createModel(root, "demo-chat")
	// 管理员自己录过价的那条：官方价与它不同，自动更新之后必须还是管理员的数。
	e.createModelBody(root, `{"name":"demo-priced","pricing":{"in":123456,"out":654321}}`)

	e.srv.AutoSyncData(context.Background())
	e.waitAutoSync(t, site, 2)
	waitFor(t, "自动更新落地", func() bool {
		return e.modelsByName(root)["demo-chat"].Pricing["in"] == 1000000
	})
	// 「价改到了」还不等于「审计写完了」：applyStoredPricing 先 SetModelPricing
	// 再 audit，而自动那条整段跑在后台协程里——只等价格就会偶发抢在审计行前面
	// 读表（单跑不现形，整包跑、机器忙的时候才现形）。等审计行本身。
	waitFor(t, "自动路径的审计行落库", func() bool {
		return len(e.auditDetails("model.pricing")) == 1
	})

	if got := e.modelsByName(root)["demo-priced"].Pricing; got["in"] != 123456 || got["out"] != 654321 {
		t.Fatalf("已定价的模型被官方价覆盖了：%v", got)
	}
	details := e.auditDetails("model.pricing")
	if len(details) != 1 || !strings.Contains(details[0], "source=official_pricing:v5") {
		t.Errorf("审计 detail = %v", details)
	}
	if !strings.Contains(details[0], "by=自动检查") {
		t.Errorf("自动路径的审计 detail 应带 by=自动检查，实际 %q", details[0])
	}
}

// TestManualSyncBusyWhileAutoRuns：手动撞上正在跑的自动更新答 409
// catalog_sync_busy——不排队（按钮会卡住几十秒），也不静默降级成"成功"
// （那会把一份不是这次取回来的读数当成本次结果）。
func TestManualSyncBusyWhileAutoRuns(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	site := e.serveCatalog(autoPricingFile(5), "")
	release := site.hold()
	t.Cleanup(release)

	e.srv.AutoSyncData(context.Background())
	site.waitArrival(t) // 自动那轮已经在飞，锁在它手上

	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "catalog_sync_busy" {
		t.Errorf("error.code = %q，期望 catalog_sync_busy", code)
	}

	// 放行之后自动那轮还要把余下那份取完并跑本地两步，锁到那时才还回来——
	// 手动这条从"忙"恢复成可用，就是单飞锁确实被放开的证据。
	release()
	if got := e.syncWebsiteDataWhenIdle(root); got.Source.Version != 5 {
		t.Errorf("自动更新结束后手动同步 = %+v", got.Source)
	}
}

// stampStoredFetchedAt 把某份已存文件的 fetched_at 改成哨兵值（其余字段原样保留）。
func stampStoredFetchedAt(t *testing.T, e *env, key, stamp string) {
	t.Helper()
	raw, err := e.st.GetSetting(context.Background(), key)
	if err != nil || raw == "" {
		t.Fatalf("读 settings %s: %v", key, err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("解析已存文件: %v", err)
	}
	stored["fetched_at"] = json.RawMessage(`"` + stamp + `"`)
	out, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("序列化已存文件: %v", err)
	}
	if err := e.st.SetSetting(context.Background(), key, string(out)); err != nil {
		t.Fatalf("写 settings %s: %v", key, err)
	}
}

// storedFetchedAt 读某份已存文件的 fetched_at。
func storedFetchedAt(t *testing.T, e *env, key string) string {
	t.Helper()
	raw, err := e.st.GetSetting(context.Background(), key)
	if err != nil {
		t.Fatalf("读 settings %s: %v", key, err)
	}
	var stored struct {
		FetchedAt string `json:"fetched_at"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("解析已存文件: %v", err)
	}
	return stored.FetchedAt
}
