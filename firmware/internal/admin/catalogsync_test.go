package admin_test

// 「数据升级」验收（手动「立即更新」与每小时自动更新共用一个核心）。
// 核心是那条不可退让的产品口径：**只填充未定价的模型**——已录价的行绝不能被
// 一次点击改掉。其余用例覆盖名字匹配、种类闸门、按来源平台取价、建模即填价、
// 同步收尾那次 Agent 订阅模型收敛（建行由目录 agents 段与在场订阅决定）、坏条目
// 只跳过自己、官网客户端未装配的降级，以及设备设置页读得到文件与自动检查状态。
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

type syncCatalogDTO struct {
	Supported      bool   `json:"supported"`
	URL            string `json:"url"`
	Origin         string `json:"origin"`
	Version        int64  `json:"version"`
	UpdatedAt      string `json:"updated_at"`
	DataTag        string `json:"data_tag"`
	BuiltinVersion int64  `json:"builtin_version"`
	Platforms      int    `json:"platforms"`
	Models         int    `json:"models"`
	Priced         int    `json:"priced"`
	Agents         int    `json:"agents"`
	SyncedAt       string `json:"synced_at"`
}

type syncAppliedDTO struct {
	Model    string           `json:"model"`
	Kind     string           `json:"kind"`
	Platform string           `json:"platform"`
	Vendor   string           `json:"vendor"`
	Pricing  map[string]int64 `json:"pricing"`
	Note     string           `json:"note"`
}

type syncSkippedDTO struct {
	Model  string `json:"model"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// catalogAutoDTO 是自动更新那一行读数。
type catalogAutoDTO struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
	LastCheckedAt   string `json:"last_checked_at"`
	LastUpdatedAt   string `json:"last_updated_at"`
}

type dataStatusDTO struct {
	ModelCatalog syncCatalogDTO `json:"model_catalog"`
	CatalogAuto  catalogAutoDTO `json:"automatic"`
}

type syncDTO struct {
	Source      syncCatalogDTO   `json:"source"`
	Applied     []syncAppliedDTO `json:"applied"`
	Skipped     []syncSkippedDTO `json:"skipped"`
	AgentModels struct {
		Created int `json:"created"`
		Updated int `json:"updated"`
		Removed int `json:"removed"`
	} `json:"agent_models"`
}

// newerVersion 比内嵌基线（tag 编码 YYYYMMDD×1000+N）大的版本号；olderVersion 比它小。
const (
	newerVersion = 99999999999
	olderVersion = 1
)

// bodyETag 给桩官网的一份文件算稳定 ETag（内容变化时 ETag 随之变化）。
func bodyETag(body string) string {
	return fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
}

// catalogFileWith 拼一份形态正确的模型目录文件（platforms / agents 段由调用方给）。
func catalogFileWith(version int64, platforms, agents string) string {
	return fmt.Sprintf(`{"schema":%q,"version":%d,"updated_at":"2026-08-10",
		"currency":"CNY","unit":"micro_yuan",
		"source":{"repository":"https://example.test/data","tag":"data-2026.08.10.1","commit":"abc","usd_cny":"6.75"},
		"platforms":[%s],"agents":[%s]}`, platformcatalog.Schema, version, platforms, agents)
}

// deepseekPlatformWith 拼一条 deepseek 平台（内置适配器），models 段由调用方给。
func deepseekPlatformWith(models string) string {
	return fmt.Sprintf(`{"type":"deepseek","vendor":"DeepSeek","source":"https://example.test/models",
		"checked_at":"2026-08-10","models":[%s]}`, models)
}

// catalogWebsite 是桩官网：伺服静态数据文件、认 If-None-Match（相符即 304），
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

// set 换掉文件的内容（ETag 随之改变，下一次条件 GET 就不再是 304）。
func (c *catalogWebsite) set(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies[officialsite.ModelCatalogPath] = body
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

// serveCatalog 起一个伺服官网数据文件的桩站点并把它注入管理面。body 为空 = 文件
// 在官网不存在（404）。按内容算 ETag 并认 If-None-Match（相符即 304 空体）：自动
// 更新走的是条件 GET，桩官网不认这个头就无法验证"没变不落库"。
func (e *env) serveCatalog(body string) *catalogWebsite {
	e.t.Helper()
	c := &catalogWebsite{
		bodies:  map[string]string{officialsite.ModelCatalogPath: body},
		arrived: make(chan struct{}, 64),
	}
	c.ts = httptest.NewServer(http.HandlerFunc(c.handle))
	e.t.Cleanup(c.ts.Close)
	client := officialsite.NewClient(c.ts.URL, nil)
	e.srv.SetCatalogSource(client)
	return c
}

// serveDeepseekCatalog 起一个只有一条 deepseek 平台的桩官网。
func (e *env) serveDeepseekCatalog(version int64, models string) *catalogWebsite {
	return e.serveCatalog(catalogFileWith(version, deepseekPlatformWith(models), ""))
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

// catalogStatus 读数据升级状态中的模型目录状态。
func (e *env) catalogStatus(cookie string) syncCatalogDTO {
	e.t.Helper()
	return e.dataStatus(cookie).ModelCatalog
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

// TestSyncCatalogFillsOnlyUnpriced 是这个功能的主用例，也是它的守门线：
// 未定价的填上，已定价的原样不动。
func TestSyncCatalogFillsOnlyUnpriced(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	// 先建模型再接官网：这时生效目录是内嵌基线，里面没有这些名字，建出来都是未定价。
	e.createModel(root, "demo-chat")
	// 管理员自己录过价的那条：目录价与它不同，同步后必须还是管理员的数。
	e.createModelBody(root, `{"name":"demo-priced","pricing":{"in":123456,"out":654321}}`)
	// 名字对上了但种类不同：文本价套不到视频模型上。
	e.createModelKind(root, "demo-video", "video")
	// 目录里根本没有的模型。
	e.createModel(root, "demo-unknown")
	e.serveDeepseekCatalog(newerVersion, `
		{"name":"demo-chat","kind":"text","note":"厂商已预告调价",
		 "pricing":{"in":1000000,"cache_read":20000,"out":2000000}},
		{"name":"demo-priced","kind":"text","pricing":{"in":9000000,"out":9000000}},
		{"name":"demo-video","kind":"text","pricing":{"in":1,"out":1}}`)

	got := e.syncWebsiteData(root)

	if len(got.Applied) != 1 || got.Applied[0].Model != "demo-chat" {
		t.Fatalf("applied = %+v，期望只有 demo-chat", got.Applied)
	}
	applied := got.Applied[0]
	if applied.Pricing["in"] != 1000000 || applied.Pricing["cache_read"] != 20000 ||
		applied.Pricing["out"] != 2000000 {
		t.Errorf("填入的价目 = %v", applied.Pricing)
	}
	if applied.Platform != "deepseek" || applied.Vendor != "DeepSeek" || applied.Note != "厂商已预告调价" {
		t.Errorf("平台/厂商/附注没带回来：%+v", applied)
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
		t.Fatalf("已定价的模型被目录价覆盖了：%v", got)
	}
	if len(models["demo-video"].Pricing) != 0 {
		t.Errorf("种类不符的模型被填了价：%v", models["demo-video"].Pricing)
	}
}

// TestSyncCatalogIsIdempotent 再同步一次不该重复改价：第一次填完，模型就"已定价"
// 了，第二次一律走 already_priced。
func TestSyncCatalogIsIdempotent(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	e.serveDeepseekCatalog(newerVersion, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)

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

// TestSyncCatalogMatchesNameCaseInsensitively：厂商原始名大小写各异，匹配不该被
// 大小写卡住。
func TestSyncCatalogMatchesNameCaseInsensitively(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModelKind(root, "demo-video-x", "video")
	e.serveCatalog(catalogFileWith(newerVersion, `{"type":"minimax","vendor":"MiniMax","source":"https://example.test/m","checked_at":"2026-08-10",
		"models":[{"name":"Demo-Video-X","kind":"video","family":"minimax_video","pricing":{"minimax_video_sec_768p":500000,"minimax_video_sec_2k":800000}}]}`, ""))

	got := e.syncWebsiteData(root)
	if len(got.Applied) != 1 || got.Applied[0].Pricing["minimax_video_sec_2k"] != 800000 {
		t.Fatalf("applied = %+v", got.Applied)
	}
}

// TestSyncCatalogBadEntryOnlySkipsItself：一个厂商改了计费形态（目录里出现本
// kind 不认的字段），不该连累其余条目同步不了。
func TestSyncCatalogBadEntryOnlySkipsItself(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	for _, n := range []string{"demo-good", "demo-bad", "demo-half", "demo-empty"} {
		e.createModel(root, n)
	}
	e.serveDeepseekCatalog(newerVersion, `
		{"name":"demo-good","kind":"text","pricing":{"in":1000000,"out":2000000}},
		{"name":"demo-bad","kind":"text","pricing":{"per_request":100}},
		{"name":"demo-half","kind":"text","pricing":{"in":1000000}},
		{"name":"demo-empty","kind":"text"}`)

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
		t.Errorf("无价条目的跳过原因 = %q，期望 no_price_in_file", reasons["demo-empty"])
	}
	// 形态错误的细节要带回给管理员，便于维护方修正数据。
	for _, s := range got.Skipped {
		if s.Model == "demo-bad" && !strings.Contains(s.Detail, "per_request") {
			t.Errorf("invalid_pricing 没说明是哪个字段：%q", s.Detail)
		}
	}
}

// TestSyncCatalogRejectsDuplicateEntries：同一平台同名条目重复 = 文件有二义性，
// 整份拒绝而不是"取第一条"——价目一旦落到账上就是永久的。
func TestSyncCatalogRejectsDuplicateEntries(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	e.serveDeepseekCatalog(newerVersion, `
		{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}},
		{"name":"demo-chat","kind":"text","pricing":{"in":9000000,"out":9000000}}`)

	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if got := errCode(t, resp); got != "model_catalog_unavailable" {
		t.Errorf("error.code = %q，期望 model_catalog_unavailable", got)
	}
	if p := e.modelsByName(root)["demo-chat"].Pricing; len(p) != 0 {
		t.Errorf("整份拒绝后仍然写了价：%v", p)
	}
}

// TestSyncCatalogPricesBySourcePlatform：同名型号在不同平台各有各的价——从哪条
// 平台接的模型就按那条平台的价补；没挂来源的按名兜底取文件序第一条有价的。
func TestSyncCatalogPricesBySourcePlatform(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat") // 未定价（内嵌基线里没有）
	e.createModel(root, "demo-only-example")
	e.serveCatalog(catalogFileWith(newerVersion,
		deepseekPlatformWith(`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)+`,
		{"id":"example_ai","type":"openai_compat","vendor":"Example AI","base_url":"https://api.example.invalid/v1",
		 "billing_mode":"usage","source":"https://example.test/e","checked_at":"2026-08-10","models":[
			{"name":"demo-chat","kind":"text","pricing":{"in":5000000,"out":6000000}},
			{"name":"demo-only-example","kind":"text","pricing":{"in":7000000,"out":8000000}}]}`, ""))
	// 先把目录同步下来（这一轮 demo-chat 无来源，按名兜底拿到 deepseek 那条），
	// 再用另一条同名模型验证来源优先。
	first := e.syncWebsiteData(root)
	reasons := map[string]map[string]int64{}
	for _, a := range first.Applied {
		reasons[a.Model] = a.Pricing
	}
	if reasons["demo-chat"]["in"] != 1000000 || reasons["demo-only-example"]["in"] != 7000000 {
		t.Fatalf("按名兜底的价不对：%+v", first.Applied)
	}

	// 接到 example_ai 的新模型：建行即按 example_ai 的价，不必等同步。
	up := e.createUpstream(root, fmt.Sprintf(
		`{"name":"example","type":"openai_compat","catalog_id":"example_ai","api_key":%q}`, upstreamKeyPlaintext))
	e.addUpstreamModels(root, up.ID, `{"models":[{"name":"demo-chat-2","kind":"text"}]}`)
	// demo-chat-2 不在目录：未定价。再加一个目录里 example_ai 有价的名字。
	if p := e.modelsByName(root)["demo-chat-2"].Pricing; len(p) != 0 {
		t.Errorf("目录里没有的模型被填了价：%v", p)
	}
	e.createModelBody(root, `{"name":"demo-priced-by-hand","pricing":{"in":1,"out":2}}`)
	e.createModelKind(root, "demo-late", "text")
	// 手工先建了一条未定价的 demo-late，之后挂到 example_ai：下一轮同步按来源平台取价。
	e.serveCatalog(catalogFileWith(newerVersion+1,
		deepseekPlatformWith(`{"name":"demo-late","kind":"text","pricing":{"in":1000000,"out":2000000}}`)+`,
		{"id":"example_ai","type":"openai_compat","vendor":"Example AI","base_url":"https://api.example.invalid/v1",
		 "billing_mode":"usage","source":"https://example.test/e","checked_at":"2026-08-10","models":[
			{"name":"demo-late","kind":"text","pricing":{"in":5000000,"out":6000000}}]}`, ""))
	e.addUpstreamModels(root, up.ID, `{"models":[{"name":"demo-late","kind":"text"}]}`)
	got := e.syncWebsiteData(root)
	var late *syncAppliedDTO
	for i := range got.Applied {
		if got.Applied[i].Model == "demo-late" {
			late = &got.Applied[i]
		}
	}
	if late == nil || late.Platform != "example_ai" || late.Pricing["in"] != 5000000 {
		t.Fatalf("挂了 example_ai 来源的模型应按 example_ai 的价补：%+v", got.Applied)
	}
}

// TestAddUpstreamModelPricesImmediately 是「新增模型不显示价格、要再同步一次才有」
// 那条缺陷的回归：从平台「添加模型」建出来的行**建行即带价**，且取的是那条
// 平台的价；手工建模不带 pricing 字段时按名取价，显式 null 保持未定价。
func TestAddUpstreamModelPricesImmediately(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveCatalog(catalogFileWith(newerVersion,
		deepseekPlatformWith(`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"cache_read":20000,"out":2000000}},
			{"name":"demo-bad","kind":"text","pricing":{"per_request":100}}`)+`,
		{"id":"example_ai","type":"openai_compat","vendor":"Example AI","base_url":"https://api.example.invalid/v1",
		 "billing_mode":"usage","source":"https://example.test/e","checked_at":"2026-08-10","models":[
			{"name":"demo-chat","kind":"text","pricing":{"in":5000000,"out":6000000}},
			{"name":"demo-example-only","kind":"text","pricing":{"in":7000000,"out":8000000}},
			{"name":"demo-manual","kind":"text","pricing":{"in":7000000,"out":8000000}},
			{"name":"demo-manual-null","kind":"text","pricing":{"in":7000000,"out":8000000}}]}`, ""))
	e.syncWebsiteData(root)

	// 从 deepseek 账号添加：建行即 deepseek 的价，管理台列表立刻看得到。
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	e.addUpstreamModels(root, ds.ID, `{"models":[{"name":"demo-chat","kind":"text"}]}`)
	models := e.modelsByName(root)
	if p := models["demo-chat"].Pricing; p["in"] != 1000000 || p["cache_read"] != 20000 || p["out"] != 2000000 {
		t.Fatalf("从 deepseek 添加的模型建行时没带上 deepseek 的价：%v", p)
	}
	// 同一个名字再从 example_ai 添加：行已在，价是管理员/已有的口径，不动。
	ex := e.createUpstream(root, fmt.Sprintf(
		`{"name":"example","type":"openai_compat","catalog_id":"example_ai","api_key":%q}`, upstreamKeyPlaintext))
	e.addUpstreamModels(root, ex.ID, `{"models":[{"name":"demo-chat","kind":"text"},{"name":"demo-example-only","kind":"text"}]}`)
	models = e.modelsByName(root)
	if p := models["demo-chat"].Pricing; p["in"] != 1000000 {
		t.Errorf("已有行的价被第二条来源改了：%v", p)
	}
	if p := models["demo-example-only"].Pricing; p["in"] != 7000000 || p["out"] != 8000000 {
		t.Errorf("从 example_ai 添加的模型建行时没带上 example_ai 的价：%v", p)
	}
	// 目录里那条价目不合本 kind 形态（文本模型配了不认的字段）：建行照建，按未定价。
	e.addUpstreamModels(root, ds.ID, `{"models":[{"name":"demo-bad","kind":"text"}]}`)
	if p := e.modelsByName(root)["demo-bad"].Pricing; len(p) != 0 {
		t.Errorf("形态不合的目录价不该写进模型：%v", p)
	}

	// 手工建模：不带 pricing 字段 → 按名取价；显式 null → 就是未定价。
	if m := e.createModel(root, "demo-example-only-2"); len(m.Pricing) != 0 {
		t.Errorf("目录里没有的名字不该有价：%v", m.Pricing)
	}
	if m := e.createModel(root, "demo-manual"); m.Pricing["in"] != 7000000 {
		t.Errorf("手工建模应按名取到目录价：%v", m.Pricing)
	}
	if m := e.createModelBody(root, `{"name":"demo-manual-null","pricing":null}`); len(m.Pricing) != 0 {
		t.Errorf("显式 null 应保持未定价：%v", m.Pricing)
	}
	// 审计要看得出这条价来自目录。
	found := false
	for _, d := range e.auditDetails("model.create") {
		if strings.Contains(d, "name=demo-chat") && strings.Contains(d, "pricing_source=model_catalog:deepseek") {
			found = true
		}
	}
	if !found {
		t.Errorf("建模审计缺 pricing_source：%v", e.auditDetails("model.create"))
	}
}

// TestSyncCatalogConvergesAgentModels：同步收尾那次收敛——建行由目录 agents 段
// 说了算：连着的订阅按目录长出模型行，文本行的名义价从 agents 段取；厂商在下一版
// 目录里撤掉某个型号，下一次同步就把那行回收。没连的订阅一行都不建。
func TestSyncCatalogConvergesAgentModels(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderGrok)
	// 收敛过一轮了（内嵌基线）：这里换一份**版本号更大**的云上目录，只留一个
	// 文本模型，其余的应当被回收。
	e.serveCatalog(catalogFileWith(newerVersion, deepseekPlatformWith(`{"name":"demo-chat","kind":"text"}`), `
		{"provider":"grok","vendor":"xAI","source":"https://example.test/g","checked_at":"2026-08-10",
		 "models":[{"name":"grok-4.6","kind":"text","pricing":{"in":13500000,"out":40500000}}]},
		{"provider":"codex","vendor":"OpenAI","source":"https://example.test/c","checked_at":"2026-08-10",
		 "models":[{"name":"gpt-5.6-sol","kind":"text","pricing":{"in":33750000,"out":202500000}}]}`))

	got := e.syncWebsiteData(root)
	if got.AgentModels.Removed == 0 {
		t.Errorf("新目录撤掉的型号应当被回收：%+v", got.AgentModels)
	}
	models := e.modelsByName(root)
	m, ok := models["grok-4.6"]
	if !ok || m.Agent != store.AgentProviderGrok || m.Pricing["in"] != 13500000 || len(m.Sources) != 0 {
		t.Errorf("grok-4.6 = %+v，期望带 grok 注记与官方名义价、无来源", m)
	}
	if _, ok := models["grok-4.5"]; ok {
		t.Error("目录里已撤掉的 grok-4.5 仍在")
	}
	if _, ok := models["gpt-5.6-sol"]; ok {
		t.Error("没连 Codex 订阅却建了它的模型行")
	}
	// 订阅计价行不归补价那一步管（它由收敛器按 agents 段写）。
	if r := skipReasons(t, got.Skipped)["grok-4.6"]; r != "already_priced" && r != "agent_managed" {
		t.Errorf("订阅计价行的跳过原因 = %q", r)
	}

	// 幂等：同一份文件再同步一次，收敛的三个数都归零。
	again := e.syncWebsiteData(root)
	if again.AgentModels.Created != 0 || again.AgentModels.Updated != 0 || again.AgentModels.Removed != 0 {
		t.Errorf("重跑同步的收敛账 = %+v，期望全零", again.AgentModels)
	}
}

// TestSyncCatalogCodexOfficialPrices 用实际发布数据验证 Codex 模型与三价落库。
func TestSyncCatalogCodexOfficialPrices(t *testing.T) {
	path := filepath.Join("..", "..", "..", "website", "public", "updates", "data", "model-catalog.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("独立固件源码构建不含官网目录文件")
	}
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderCodex)
	e.serveCatalog(string(raw))
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

// TestModelsCarryAgentAnnotation：agent 注记（modelJSON.Agent）逐次按**生效目录的
// agents 段**算出、不落库。三条边界：目录点过名的名字开箱即注记（内嵌基线在场，
// 不必先同步）、kind 不是 text 不注、目录没提过的名字恒空。
func TestModelsCarryAgentAnnotation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 内嵌目录里就有 grok-4.6：连上订阅即长出带注记的行，一次同步都不必。
	seedAgentSubscription(t, e, store.AgentProviderGrok)
	if got := e.modelsByName(root)["grok-4.6"].Agent; got != store.AgentProviderGrok {
		t.Fatalf("未同步过时 grok-4.6 的注记 = %q，期望 grok（内嵌目录已收录）", got)
	}
	// 与目录里 text 订阅条目同名的 video 行：注记不认（kind 闸门）。
	e.createModelKind(root, "seedance-agent", "video")
	e.serveCatalog(catalogFileWith(newerVersion, deepseekPlatformWith(`{"name":"deepseek-v4-pro","kind":"text","pricing":{"in":3000000,"out":6000000}}`), `
		{"provider":"grok","vendor":"xAI","source":"https://example.test/g","checked_at":"2026-08-10",
		 "models":[{"name":"grok-4.6","kind":"text","pricing":{"in":1,"out":1}},{"name":"grok-4.5","kind":"text","pricing":{"in":1,"out":1}}]},
		{"provider":"codex","vendor":"OpenAI","source":"https://example.test/c","checked_at":"2026-08-10",
		 "models":[{"name":"seedance-agent","kind":"text","pricing":{"in":1000000,"out":2000000}}]}`))
	e.syncWebsiteData(root)

	models := e.modelsByName(root)
	if got := models["seedance-agent"].Agent; got != "" {
		t.Errorf("video 行不该带注记：%q", got)
	}
	// 目录 agents 段没有的名字：建得出来，且不带注记。
	if m := e.createModel(root, "claude-mythos-5"); m.Agent != "" {
		t.Errorf("目录 agents 段没有的名字不该带注记：%q", m.Agent)
	}
	// 目录点过名的名字挡在通用建模面之外。
	resp := e.do("POST", "/admin/v1/models", root, `{"name":"grok-4.5","kind":"text"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "model_agent_managed" {
		t.Errorf("建目录点过名的 grok-4.5 错误码 = %q，期望 model_agent_managed", code)
	}
}

// TestSyncCatalogStoresFileAndStatus：文件本身要落到设备上，设备设置页面靠数据
// 升级读数渲染面板与按钮副标题（不为它多发一次请求）。
func TestSyncCatalogStoresFileAndStatus(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	file := catalogFileWith(newerVersion, deepseekPlatformWith(`{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`), "")
	site := e.serveCatalog(file)

	if before := e.catalogStatus(root); !before.Supported || before.SyncedAt != "" || before.Origin != "builtin" {
		t.Fatalf("同步前状态 = %+v，期望 supported、内嵌基线生效且未同步过", before)
	}
	got := e.syncWebsiteData(root)
	if got.Source.Version != newerVersion || got.Source.Platforms != 1 || got.Source.Models != 1 || got.Source.Priced != 1 || got.Source.SyncedAt == "" {
		t.Errorf("同步响应里的来源信息 = %+v", got.Source)
	}
	if got.Source.URL != site.ts.URL+officialsite.ModelCatalogPath || got.Source.DataTag != "data-2026.08.10.1" {
		t.Errorf("来源地址 / 数据版本 = %+v", got.Source)
	}

	status := e.catalogStatus(root)
	if status.Version != newerVersion || status.Origin != "synced" || status.UpdatedAt != "2026-08-10" || status.SyncedAt == "" || status.BuiltinVersion <= 0 {
		t.Errorf("状态 = %+v", status)
	}

	// 落库的是**原文**：下次要按它渲染、也便于人工核对。
	raw, err := e.st.GetSetting(context.Background(), "model_catalog")
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
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(file)); err != nil {
		t.Fatalf("json.Compact: %v", err)
	}
	if string(stored.File) != want.String() {
		t.Errorf("落库的不是下载到的那份文件：\n%s", stored.File)
	}
}

// TestSyncCatalogAudited：改价即改钱，走与手工改价同一个审计事件，detail 要能看出
// 这次是目录同步来的、来自哪条平台。
func TestSyncCatalogAudited(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	e.serveDeepseekCatalog(newerVersion, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)
	e.syncWebsiteData(root)

	details := e.auditDetails("model.pricing")
	if len(details) != 1 {
		t.Fatalf("model.pricing 审计条数 = %d，期望 1；%v", len(details), details)
	}
	for _, want := range []string{"name=demo-chat", "in:1000000", fmt.Sprintf("source=model_catalog:v%d", newerVersion), "platform=deepseek"} {
		if !strings.Contains(details[0], want) {
			t.Errorf("审计 detail 缺 %q：%s", want, details[0])
		}
	}
}

// TestSyncCatalogNotConfigured：官网客户端未装配时端点明确报错，数据升级读数里的
// supported 为 false（设备设置页据此把按钮置灰），内嵌基线照常生效。
func TestSyncCatalogNotConfigured(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	if s := e.catalogStatus(root); s.Supported || s.URL != "" || s.Origin != "builtin" || s.Platforms == 0 {
		t.Errorf("未接入云时的状态 = %+v", s)
	}
	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if got := errCode(t, resp); got != "website_not_configured" {
		t.Errorf("error.code = %q，期望 website_not_configured", got)
	}
}

// TestSyncCatalogUnavailable：官网还没发布这份文件（404，或 SPA fallback 用 200 回
// index.html）时报 502，且不动任何价。
func TestSyncCatalogUnavailable(t *testing.T) {
	for name, body := range map[string]string{"404": "", "html": "<!doctype html><html><body>LLM Gate</body></html>"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			root := e.rootSession()
			e.serveCatalog(body)
			e.createModel(root, "demo-chat")

			resp := e.do("POST", catalogSyncPath, root, "")
			wantStatus(t, resp, http.StatusBadGateway)
			if got := errCode(t, resp); got != "model_catalog_unavailable" {
				t.Errorf("error.code = %q，期望 model_catalog_unavailable", got)
			}
			if p := e.modelsByName(root)["demo-chat"].Pricing; len(p) != 0 {
				t.Errorf("下载失败却写了价：%v", p)
			}
			if s := e.catalogStatus(root); s.Origin != "builtin" || s.SyncedAt != "" {
				t.Errorf("状态 = %+v，期望仍是没同步过的内嵌基线", s)
			}
		})
	}
}

// TestSyncCatalogRequiresSession：无会话打不动改价这条路。
func TestSyncCatalogRequiresSession(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	e.serveDeepseekCatalog(newerVersion, `{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}}`)

	resp := e.do("POST", catalogSyncPath, "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
}

// TestSyncCatalogActivatesNewerVersion：同步来的版本号大于内嵌基线才生效，生效后
// 立刻成为「添加模型」的选单。
func TestSyncCatalogActivatesNewerVersion(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveDeepseekCatalog(newerVersion, `{"name":"site-only-model","kind":"text"}`)

	got := e.syncWebsiteData(root)
	if got.Source.Origin != "synced" || got.Source.Version != newerVersion {
		t.Fatalf("目录状态 = %+v，期望同步来的那份生效", got.Source)
	}
	status := e.dataStatus(root)
	if !status.CatalogAuto.Enabled || status.CatalogAuto.IntervalSeconds != 3600 {
		t.Errorf("automatic = %+v，期望开着且间隔 1h", status.CatalogAuto)
	}
	if status.CatalogAuto.LastCheckedAt == "" || status.CatalogAuto.LastUpdatedAt == "" {
		t.Errorf("手动「立即更新」也该刷新两个时刻：%+v", status.CatalogAuto)
	}
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	list := e.upstreamModels(root, up.ID)
	if list.Catalog.Origin != "synced" || len(list.Models) != 1 || list.Models[0].Name != "site-only-model" {
		t.Fatalf("添加模型清单 = %+v / %+v", list.Catalog, list.Models)
	}
}

// TestDataUpgradeAddsPlatformAndSnapshotsEndpoint 钉住数据升级的完整产品路径：
// 新平台只复用固件已有兼容适配器，管理台立刻能列出并创建账号；账号保存平台
// 身份、计费方式和端点快照，后续目录换地址不会把已封存的 Key 静默改发过去；
// 版本回退的文件整份拒收。
func TestDataUpgradeAddsPlatformAndSnapshotsEndpoint(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	platformFile := func(version int64, endpoint string) string {
		return catalogFileWith(version, fmt.Sprintf(`{
			"id":"example_ai","type":"openai_compat","vendor":"Example AI","base_url":%q,
			"billing_mode":"usage","suggested_name":"example-ai","entries":"OpenAI 兼容单入口",
			"ability":"数据目录平台","source":"https://example.test/e","checked_at":"2026-08-10",
			"models":[{"name":"example-reasoner","kind":"text","pricing":{"in":1000000,"out":2000000}}]}`, endpoint), "")
	}
	e.serveCatalog(platformFile(newerVersion, "https://api.example.invalid/v1"))
	if got := e.syncWebsiteData(root); got.Source.Origin != "synced" {
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
	if len(listed.Platforms) != 2 || listed.Platforms[1].ID != "generic" || listed.Platforms[0].ID != "example_ai" ||
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
	row := e.modelsByName(root)["example-reasoner"]
	source := row.Sources[0]
	if source.UpstreamCatalogID != "example_ai" || source.UpstreamPlatformLabel != "Example AI" ||
		source.UpstreamBillingMode != "usage" || source.Priority != 200 {
		t.Fatalf("动态平台来源没有沿用平台身份与计费优先级：%+v", source)
	}
	if row.Pricing["in"] != 1000000 {
		t.Fatalf("从数据平台添加的模型建行时没带价：%+v", row.Pricing)
	}

	e.serveCatalog(platformFile(newerVersion+1, "https://api.changed.invalid/v1"))
	e.syncWebsiteData(root)
	for _, saved := range e.listUpstreams(root) {
		if saved.ID == up.ID && saved.BaseURL != "https://api.example.invalid/v1" {
			t.Fatalf("后续数据升级改写了已有账号端点：%+v", saved)
		}
	}
	e.serveCatalog(platformFile(newerVersion, "https://api.rollback.invalid/v1"))
	rollback := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, rollback, http.StatusBadGateway)
	if code := errCode(t, rollback); code != "model_catalog_invalid" {
		t.Errorf("版本回退的错误码 = %q，期望 model_catalog_invalid", code)
	}
	if s := e.catalogStatus(root); s.Version != newerVersion+1 {
		t.Fatalf("目录降级没有被拒绝并保留当前版本：%+v", s)
	}
}

// TestSyncCatalogKeepsBuiltinWhenOlder：同步来的版本不比内嵌的新时，内嵌基线
// 继续生效——固件升级带来的新基线不该被一次早年的同步永久压住。
func TestSyncCatalogKeepsBuiltinWhenOlder(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveDeepseekCatalog(olderVersion, `{"name":"stale-model","kind":"text"}`)

	got := e.syncWebsiteData(root)
	if got.Source.Origin != "builtin" {
		t.Fatalf("目录状态 = %+v，期望内嵌基线继续生效", got.Source)
	}
	// 存过了就该报出同步时间，否则界面会显示成"从没同步过"。
	if got.Source.SyncedAt == "" {
		t.Errorf("同步时间缺席：%+v", got.Source)
	}
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	if _, ok := findUpModel(e.upstreamModels(root, up.ID).Models, "stale-model"); ok {
		t.Error("旧版本的清单不该生效")
	}
}

func TestCatalogReadoutsComeFromWebsite(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.serveDeepseekCatalog(newerVersion, `{"name":"site-only-model","kind":"text"}`)
	e.syncWebsiteData(root)

	got := e.dataStatus(root)
	if !got.ModelCatalog.Supported || !got.CatalogAuto.Enabled {
		t.Errorf("接入官网时 supported / automatic.enabled 都该是 true：%+v", got)
	}
	if got.ModelCatalog.SyncedAt == "" || got.ModelCatalog.Version != newerVersion || got.ModelCatalog.URL == "" {
		t.Errorf("上次同步的时间 / 版本 / 地址都该报出：%+v", got.ModelCatalog)
	}
}

// ---- 自动更新（每小时节拍，条件 GET）----

// waitAutoSync 等自动更新 goroutine 打到桩官网。
func (e *env) waitAutoSync(t *testing.T, site *catalogWebsite, wantHits int) {
	t.Helper()
	waitFor(t, "自动更新落地", func() bool { return site.hitCount() >= wantHits })
}

// autoCatalogFile 是自动更新用例共用的目录文件（两个未定价模型 + 一个已定价的）。
func autoCatalogFile(version int64) string {
	return catalogFileWith(version, deepseekPlatformWith(`
		{"name":"demo-chat","kind":"text","pricing":{"in":1000000,"out":2000000}},
		{"name":"demo-two","kind":"text","pricing":{"in":3000000,"out":4000000}},
		{"name":"demo-priced","kind":"text","pricing":{"in":9000000,"out":9000000}}`), "")
}

// TestAutoSyncChecksAtMostOncePerInterval：距上次检查不足一个间隔（1h）的那些
// 节拍未到直接返回，避免频繁请求官网静态文件。
func TestAutoSyncChecksAtMostOncePerInterval(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	site := e.serveCatalog(autoCatalogFile(newerVersion))

	e.srv.AutoSyncData(context.Background())
	e.waitAutoSync(t, site, 1)
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
// 来路记在 detail 的 by= 段里。
func TestAutoSyncFillsOnlyUnpricedMarkedAuto(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModel(root, "demo-chat")
	// 管理员自己录过价的那条：目录价与它不同，自动更新之后必须还是管理员的数。
	e.createModelBody(root, `{"name":"demo-priced","pricing":{"in":123456,"out":654321}}`)
	site := e.serveCatalog(autoCatalogFile(newerVersion))

	e.srv.AutoSyncData(context.Background())
	e.waitAutoSync(t, site, 1)
	waitFor(t, "自动更新落地", func() bool {
		return e.modelsByName(root)["demo-chat"].Pricing["in"] == 1000000
	})
	// 「价改到了」还不等于「审计写完了」：先 SetModelPricing 再 audit，而自动那条
	// 整段跑在后台协程里。等审计行本身。
	waitFor(t, "自动路径的审计行落库", func() bool {
		return len(e.auditDetails("model.pricing")) == 1
	})

	if got := e.modelsByName(root)["demo-priced"].Pricing; got["in"] != 123456 || got["out"] != 654321 {
		t.Fatalf("已定价的模型被目录价覆盖了：%v", got)
	}
	details := e.auditDetails("model.pricing")
	if len(details) != 1 || !strings.Contains(details[0], fmt.Sprintf("source=model_catalog:v%d", newerVersion)) {
		t.Errorf("审计 detail = %v", details)
	}
	if !strings.Contains(details[0], "by=自动检查") {
		t.Errorf("自动路径的审计 detail 应带 by=自动检查，实际 %q", details[0])
	}
}

// TestManualSyncBusyWhileAutoRuns：手动撞上正在跑的自动更新答 409
// catalog_sync_busy——不排队（按钮会卡住几十秒），也不静默降级成"成功"。
func TestManualSyncBusyWhileAutoRuns(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	site := e.serveCatalog(autoCatalogFile(newerVersion))
	release := site.hold()
	t.Cleanup(release)

	e.srv.AutoSyncData(context.Background())
	site.waitArrival(t) // 自动那轮已经在飞，锁在它手上

	resp := e.do("POST", catalogSyncPath, root, "")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "catalog_sync_busy" {
		t.Errorf("error.code = %q，期望 catalog_sync_busy", code)
	}

	// 放行之后自动那轮还要跑本地两步，锁到那时才还回来——手动这条从"忙"恢复成
	// 可用，就是单飞锁确实被放开的证据。
	release()
	if got := e.syncWebsiteDataWhenIdle(root); got.Source.Version != newerVersion {
		t.Errorf("自动更新结束后手动同步 = %+v", got.Source)
	}
}
