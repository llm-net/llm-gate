// endpoints_test.go 钉住接入读数端点的契约：作用域（/me 之外唯一对 member
// 开放的端点，未登录仍 401）、地址形状（只报可直连的 IPv4，回环与 IPv6 一律
// 不出现）、primary 标记（「本次连接落在哪个地址上」必须命中且排第一，示例
// 代码就用它）、明文端口如实回显（界面靠它拼内网直连地址，不再猜浏览器的），
// 以及可用模型清单与数据面 /v1/models 同口径。
package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type endpointsDTO struct {
	Addresses []struct {
		Host      string `json:"host"`
		Interface string `json:"interface"`
		Primary   bool   `json:"primary"`
	} `json:"addresses"`
	HTTPPort     int    `json:"http_port"`
	ExternalURL  string `json:"external_url"`
	LanDomainURL string `json:"lan_domain_url"`
}

type servableModelDTO struct {
	Name      string   `json:"name"`
	Protocols []string `json:"protocols"`
}

type aigcModelDTO struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	API       string `json:"api"`
	Available bool   `json:"available"`
}

type agentsAccessDTO struct {
	Available    bool   `json:"available"`
	Provider     string `json:"provider"`
	DefaultModel string `json:"default_model"`
}

type apiModelCountDTO struct {
	Text int `json:"text"`
	AIGC int `json:"aigc"`
}

type accessDTO struct {
	Endpoints       endpointsDTO                `json:"endpoints"`
	Models          []servableModelDTO          `json:"models"`
	AIGCModels      []aigcModelDTO              `json:"aigc_models"`
	HardwareModel   string                      `json:"hardware_model"`
	FirmwareVersion string                      `json:"firmware_version"`
	APIModelCounts  map[string]apiModelCountDTO `json:"api_model_counts"`
	// Agents 是每 provider 一项的数组（codex、grok、claude、cursor 恒在场）。
	Agents []agentsAccessDTO `json:"agents"`
}

func TestEndpointsAPIModelCounts(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	check := func(usageText, subscriptionText, usageAIGC, subscriptionAIGC int) {
		t.Helper()
		got := e.access(root).APIModelCounts
		if len(got) != 2 || got["usage"] != (apiModelCountDTO{Text: usageText, AIGC: usageAIGC}) ||
			got["subscription"] != (apiModelCountDTO{Text: subscriptionText, AIGC: subscriptionAIGC}) {
			t.Fatalf("api_model_counts = %+v, want usage=%d/%d subscription=%d/%d", got, usageText, usageAIGC, subscriptionText, subscriptionAIGC)
		}
	}
	check(0, 0, 0, 0)
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"usage","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	mock := e.createUpstream(root, mockUpstreamBody("none", "http://127.0.0.1:18080/v1"))
	// 类型仍是 deepseek，按账号快照的 subscription 分组，不能按适配器猜计费模式。
	plan, err := e.st.CreateCatalogUpstream(t.Context(), "plan", "deepseek", "test-plan", "subscription", upstreamKeyPlaintext, "")
	if err != nil {
		t.Fatal(err)
	}
	usage := e.createModel(root, "usage-text")
	e.createSource(root, usage.ID, fmt.Sprintf(`{"upstream_id":%d}`, mock.ID))
	shared := e.createModel(root, "shared-text")
	for _, id := range []int64{ds.ID, mock.ID} {
		e.createSource(root, shared.ID, fmt.Sprintf(`{"upstream_id":%d}`, id))
	}
	planSource := e.createSource(root, shared.ID, fmt.Sprintf(`{"upstream_id":%d}`, plan.ID))
	onlyPlan := e.createModel(root, "plan-text")
	e.createSource(root, onlyPlan.ID, fmt.Sprintf(`{"upstream_id":%d}`, plan.ID))
	e.createModel(root, "no-source")
	check(2, 2, 0, 0)

	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))
	arkPlan := e.createUpstream(root, fmt.Sprintf(`{"name":"ark-plan","type":"ark_plan","api_key":"%s"}`, upstreamKeyPlaintext))
	video := e.createModelKind(root, "shared-video", "video")
	for _, id := range []int64{ark.ID, arkPlan.ID} {
		e.createSource(root, video.ID, fmt.Sprintf(`{"upstream_id":%d}`, id))
	}
	image := e.createModelKind(root, "plan-image", "image")
	e.createSource(root, image.ID, fmt.Sprintf(`{"upstream_id":%d}`, arkPlan.ID))
	// 存量不服务该协议面的来源不得把模型计入按量组。
	dead := e.createModelKind(root, "dead-video", "video")
	if _, err := e.st.CreateModelSource(t.Context(), dead.ID, ds.ID, "", 100); err != nil {
		t.Fatal(err)
	}
	check(2, 2, 1, 2)

	// 关闭某来源仅有的协议面时，不影响另一组仍可用的同名模型。
	chatPlan, err := e.st.CreateCatalogUpstream(t.Context(), "chat-plan", "ark", "test-chat-plan", "subscription", upstreamKeyPlaintext, "")
	if err != nil {
		t.Fatal(err)
	}
	blocked := e.createModel(root, "blocked-plan")
	e.createSource(root, blocked.ID, fmt.Sprintf(`{"upstream_id":%d}`, chatPlan.ID))
	e.createSource(root, blocked.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", blocked.ID), root,
		`{"entry_openai":false,"entry_responses":false,"entry_anthropic":true}`), http.StatusOK)
	check(3, 2, 1, 2)

	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", usage.ID), root, `{"disabled":true}`), http.StatusOK)
	check(2, 2, 1, 2)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", planSource.ID), root, `{"disabled":true}`), http.StatusOK)
	check(2, 1, 1, 2)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", arkPlan.ID), root, `{"disabled":true}`), http.StatusOK)
	check(2, 1, 1, 0)
}

// agentEntry 从读数里取指定 provider 的那一项（没有则 fail）。
func agentEntry(t *testing.T, list []agentsAccessDTO, provider string) agentsAccessDTO {
	t.Helper()
	for _, a := range list {
		if a.Provider == provider {
			return a
		}
	}
	t.Fatalf("agents 读数里没有 provider=%s 的项: %+v", provider, list)
	return agentsAccessDTO{}
}

func (e *env) access(cookie string) accessDTO {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/endpoints", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out accessDTO
	decodeInto(e.t, resp, &out)
	return out
}

func (e *env) endpoints(cookie string) endpointsDTO {
	e.t.Helper()
	return e.access(cookie).Endpoints
}

// TestEndpointsScope：接入地址要会话，未登录 401。
func TestEndpointsScope(t *testing.T) {
	e := newEnv(t)
	e.endpoints(e.rootSession())

	resp := e.do("GET", "/admin/v1/endpoints", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if got := errCode(t, resp); got != "unauthorized" {
		t.Errorf("无会话 error.code = %q，期望 unauthorized", got)
	}
}

// TestEndpointsShape：读数只含可直连的 IPv4，且与本机网卡对得上。回环地址
// 出现在这里等于教客户端连它自己，是必须挡住的错。
func TestEndpointsShape(t *testing.T) {
	e := newEnv(t)
	got := e.endpoints(e.rootSession())

	seen := map[string]bool{}
	for _, a := range got.Addresses {
		ip := net.ParseIP(a.Host)
		switch {
		case ip == nil:
			t.Errorf("地址 %q 不是合法 IP", a.Host)
		case ip.To4() == nil:
			t.Errorf("地址 %q 是 IPv6：接入指引只给 IPv4，理由见 localIPv4s", a.Host)
		case !ip.IsGlobalUnicast():
			t.Errorf("地址 %q 不是全局单播（回环/链路本地/组播都不该出现）", a.Host)
		}
		if a.Interface == "" {
			t.Errorf("地址 %q 缺网卡名", a.Host)
		}
		if seen[a.Host] {
			t.Errorf("地址 %q 重复", a.Host)
		}
		seen[a.Host] = true
	}
}

// TestEndpointsPrimaryFirst：本次连接落在哪个本机地址上，那个地址就带
// primary 且排第一——它对当前这个浏览器一定可达，示例代码优先用它。
func TestEndpointsPrimaryFirst(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	all := e.endpoints(cookie)
	if len(all.Addresses) < 2 {
		t.Skipf("本机可直连 IPv4 少于 2 个（%d 个），排序无从验证", len(all.Addresses))
	}
	// 取一个非首位地址当「本次连接的落地地址」，它必须被顶到第一。
	want := all.Addresses[len(all.Addresses)-1].Host
	r := e.req("GET", "/admin/v1/endpoints", cookie, "")
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP(want), Port: 80}))
	resp := e.send(r)
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Endpoints endpointsDTO `json:"endpoints"`
	}
	decodeInto(t, resp, &out)

	if len(out.Endpoints.Addresses) != len(all.Addresses) {
		t.Fatalf("地址条数 = %d，期望 %d（primary 只改排序与标记，不增删）",
			len(out.Endpoints.Addresses), len(all.Addresses))
	}
	first := out.Endpoints.Addresses[0]
	if first.Host != want || !first.Primary {
		t.Errorf("首位 = %q（primary=%v），期望 %q 且 primary=true", first.Host, first.Primary, want)
	}
	for _, a := range out.Endpoints.Addresses[1:] {
		if a.Primary {
			t.Errorf("地址 %q 也被标成 primary，本次连接只落在一个地址上", a.Host)
		}
	}
}

// TestEndpointsPorts：读数如实回显设备自己的监听端口。界面靠它拼「内网 IP
// 直连该填哪个地址」，不能猜浏览器地址栏的端口。
func TestEndpointsPorts(t *testing.T) {
	e := newEnv(t)
	all := e.access(e.rootSession())
	got := all.Endpoints
	if got.HTTPPort != testListenPort {
		t.Errorf("http_port = %d，期望 %d（cfg.listen 的端口，不是浏览器的）", got.HTTPPort, testListenPort)
	}
	if all.HardwareModel != testHardwareModel {
		t.Errorf("hardware_model = %q，期望 %q", all.HardwareModel, testHardwareModel)
	}
}

// TestEndpointsModels：可用模型清单与数据面 /v1/models 同口径（模型、来源、
// 上游三者都启用才算数），并标出每个模型能服务的入口协议。
func TestEndpointsModels(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	if models := e.access(root).Models; len(models) != 0 {
		t.Fatalf("空目录下 models = %v，期望空", models)
	}

	// deepseek 双入口、ark 仅 chat：协议标注要能区分这两种上游。
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))
	both := e.createModel(root, "both-entries")
	e.createSource(root, both.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	chatOnly := e.createModel(root, "chat-only")
	e.createSource(root, chatOnly.ID, fmt.Sprintf(`{"upstream_id":%d}`, ark.ID))
	// 无来源的模型进不了清单（/v1/models 也不列它）。
	e.createModel(root, "no-source")

	models := e.access(root).Models
	if len(models) != 2 {
		t.Fatalf("可用模型 = %+v，期望 2 个（无来源的不算）", models)
	}
	if models[0].Name != "both-entries" || models[1].Name != "chat-only" {
		t.Errorf("清单顺序 = %q/%q，期望按模型名字典序", models[0].Name, models[1].Name)
	}
	if len(models[0].Protocols) != 3 || models[0].Protocols[0] != "openai_chat" ||
		models[0].Protocols[1] != "openai_responses" || models[0].Protocols[2] != "anthropic_messages" {
		t.Errorf("deepseek 模型的入口 = %v，期望 [openai_chat openai_responses anthropic_messages]（顺序恒定）", models[0].Protocols)
	}
	if len(models[1].Protocols) != 2 || models[1].Protocols[0] != "openai_chat" || models[1].Protocols[1] != "openai_responses" {
		t.Errorf("ark 模型的入口 = %v，期望 [openai_chat openai_responses]", models[1].Protocols)
	}

	// 停用模型即退出清单（与数据面同一个请求即生效的口径）。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", chatOnly.ID), root, `{"disabled":true}`), http.StatusOK)
	models = e.access(root).Models
	if len(models) != 1 || models[0].Name != "both-entries" {
		t.Errorf("停用后清单 = %+v，期望只剩 both-entries", models)
	}

	// 调用入口开关（0013）：关掉的入口不进这份成员可读的清单——来源能力
	// 与开关求交集；双关模型整个消失（等同停用）。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", both.ID), root, `{"entry_anthropic":false}`), http.StatusOK)
	models = e.access(root).Models
	if len(models) != 1 || len(models[0].Protocols) != 2 || models[0].Protocols[0] != "openai_chat" || models[0].Protocols[1] != "openai_responses" {
		t.Errorf("关 anthropic 后入口 = %+v，期望 [openai_chat openai_responses]", models)
	}
}

// TestEndpointsAIGCModels：视频/图片模型的最小可见性。2026-08-09 厂商官方
// 接口改版起，member 可读的清单带 api（协议面协议名，如 minimax_video）——
// 模型固定源头厂商官方接口后，协议面就是客户端契约，使用API页要靠它教官方
// 路径；仍然不得泄露的是上游**账户**（账户名、来源侧模型 ID、凭据）。
// available 按「任一来源的上游类型服务该 kind 的协议面」判定；文本清单集合
// 不受影响。
func TestEndpointsAIGCModels(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	if got := e.access(root).AIGCModels; len(got) != 0 {
		t.Fatalf("空目录下 aigc_models = %v，期望空", got)
	}

	mm := e.createUpstream(root, fmt.Sprintf(`{"name":"secret-video-account","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"up-text","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))

	text := e.createModel(root, "text-model")
	e.createSource(root, text.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	video := e.createModelKind(root, "video-model", "video")
	e.createSource(root, video.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"vendor-video-id"}`, mm.ID))
	// 挂在不服务视频协议面的上游类型上的行如今在写入侧就被拒
	// （source_kind_unservable），available=false 只可能来自守卫之前的存量
	// 配置——store 直建等价那条路（管理 API 拒绝不拦库层写入）。
	dead := e.createModelKind(root, "dead-video", "video")
	if _, err := e.st.CreateModelSource(t.Context(), dead.ID, ds.ID, "", 100); err != nil {
		t.Fatalf("store 直建存量死来源: %v", err)
	}
	// 无来源的视频模型不出现（与 /v1/models 排除无来源模型同一口径）。
	e.createModelKind(root, "bare-video", "video")

	snap := e.access(root)
	if len(snap.Models) != 1 || snap.Models[0].Name != "text-model" {
		t.Errorf("文本清单 = %+v，期望不受视频/图片模型影响", snap.Models)
	}
	want := []aigcModelDTO{
		{Name: "dead-video", Kind: "video", API: "", Available: false},
		{Name: "video-model", Kind: "video", API: "minimax_video", Available: true},
	}
	if len(snap.AIGCModels) != len(want) {
		t.Fatalf("aigc_models = %+v，期望 %+v", snap.AIGCModels, want)
	}
	for i, w := range want {
		if snap.AIGCModels[i] != w {
			t.Errorf("aigc_models[%d] = %+v，期望 %+v", i, snap.AIGCModels[i], w)
		}
	}

	// 泄露检查：上游账户名、来源侧模型 ID、凭据不得出现（api 字段的协议面
	// 协议名是刻意公开的客户端契约，不在此列）。
	resp := e.do("GET", "/admin/v1/endpoints", root, "")
	wantStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	for _, leak := range []string{"secret-video-account", "up-text", "vendor-video-id", upstreamKeyPlaintext} {
		if strings.Contains(body, leak) {
			t.Errorf("接入读数泄露 %q：\n%s", leak, body)
		}
	}

	// 停用视频模型即从清单消失（与文本清单同一即时生效口径）。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", video.ID), root, `{"disabled":true}`), http.StatusOK)
	got := e.access(root).AIGCModels
	if len(got) != 1 || got[0].Name != "dead-video" {
		t.Errorf("停用后 aigc_models = %+v，期望只剩 dead-video", got)
	}
}

// TestKeyAccessSnapshotNarrowsToKeyScope：凭 Key 自证的读数（数据面
// GET /gate-helper/v1/endpoints 的执行体 KeyAccessSnapshot）与管理员视角同一份地址、
// 端口与铭牌；模型清单（文本与视频/图片）按这把 Key 的 API模型范围裁剪——范围外
// 的名字整个不出现，与 /v1/models 同一口径；不带订阅读数（那归 /gate-helper/v1/config）。
// 管理员视角的 /admin/v1/endpoints 不受任何 Key 范围影响。
func TestKeyAccessSnapshotNarrowsToKeyScope(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	key, _ := e.createKey(root, "friend")

	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"up-text","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	mm := e.createUpstream(root, fmt.Sprintf(`{"name":"up-video","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))
	allowed := e.createModel(root, "allowed-model")
	e.createSource(root, allowed.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	denied := e.createModel(root, "denied-model")
	e.createSource(root, denied.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	video := e.createModelKind(root, "video-model", "video")
	e.createSource(root, video.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"vendor-video-id"}`, mm.ID))

	keyAccess := func() (accessDTO, map[string]json.RawMessage) {
		t.Helper()
		raw, err := e.srv.KeyAccessSnapshot(e.req("GET", "/gate-helper/v1/endpoints", "", ""), key.ID)
		if err != nil {
			t.Fatalf("KeyAccessSnapshot: %v", err)
		}
		body, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		for _, leak := range []string{"up-text", "up-video", "vendor-video-id", upstreamKeyPlaintext} {
			if strings.Contains(string(body), leak) {
				t.Errorf("凭 Key 读数泄露 %q：\n%s", leak, body)
			}
		}
		var out accessDTO
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(body, &keys); err != nil {
			t.Fatalf("Unmarshal keys: %v", err)
		}
		return out, keys
	}

	// 不限制的 Key：与管理员视角同一份清单。
	got, keys := keyAccess()
	if _, has := keys["agents"]; has {
		t.Errorf("凭 Key 读数不该带 agents（订阅读数归 /gate-helper/v1/config）：%s", keys["agents"])
	}
	if _, has := keys["api_model_counts"]; has {
		t.Error("凭 Key 读数不该带管理员顶栏的全设备模型统计")
	}
	if _, has := keys["device_name"]; has {
		t.Error("凭 Key 读数不该带管理员自定义设备名")
	}
	if got.Endpoints.HTTPPort != testListenPort {
		t.Errorf("http_port = %d，期望 %d（与管理员视角同一份）", got.Endpoints.HTTPPort, testListenPort)
	}
	if got.FirmwareVersion == "" {
		t.Errorf("凭 Key 读数缺 firmware_version")
	}
	if len(got.Models) != 2 || got.Models[0].Name != "allowed-model" || got.Models[1].Name != "denied-model" {
		t.Fatalf("不限制时 models = %+v，期望两个都在", got.Models)
	}
	if len(got.AIGCModels) != 1 || got.AIGCModels[0].Name != "video-model" || got.AIGCModels[0].API != "minimax_video" {
		t.Fatalf("不限制时 aigc_models = %+v，期望 video-model", got.AIGCModels)
	}

	// 收窄到 allowed-model：denied-model 与视频模型对这把 Key 整个不出现。
	if _, _, err := e.st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{
		KeyID: key.ID, Restricted: true, ModelIDs: []int64{allowed.ID},
	}); err != nil {
		t.Fatalf("ReplaceKeyAPIModelConfig: %v", err)
	}
	got, _ = keyAccess()
	if len(got.Models) != 1 || got.Models[0].Name != "allowed-model" {
		t.Errorf("收窄后 models = %+v，期望只剩 allowed-model", got.Models)
	}
	if len(got.AIGCModels) != 0 {
		t.Errorf("收窄后 aigc_models = %+v，期望为空（视频模型不在范围内）", got.AIGCModels)
	}
	// 管理员视角不套 Key 范围。
	admin := e.access(root)
	if len(admin.Models) != 2 || len(admin.AIGCModels) != 1 {
		t.Errorf("管理员视角 models=%+v aigc=%+v，期望不受 Key 范围影响", admin.Models, admin.AIGCModels)
	}
}

// TestEndpointsAgents：Agents 块（迭代 11）——「这台设备现在能不能当 Codex
// 后端用」。member 读得到（同 aigc_models 的准入线：这是设备能力，不是用户
// 数据），块内**只有三个键**，订阅账号自身的信息（名称、账号标识、状态、
// 最近刷新）与任何凭据都不得出现。
func TestEndpointsAgents(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 未连接订阅：四项都不可用，但 provider 照给——它说的是「这台设备**能**
	// 代理哪几种 agent」，与连没连上无关，界面据此渲染而不是把厂商名写死。
	// 顺序固定 codex → grok → claude → cursor。
	list := e.access(root).Agents
	if len(list) != 4 || list[0].Provider != "codex" || list[1].Provider != "grok" ||
		list[2].Provider != "claude" || list[3].Provider != "cursor" {
		t.Fatalf("agents 读数 = %+v，期望恒含 codex、grok、claude、cursor 四项且顺序固定", list)
	}
	for _, a := range list {
		if a.Available || a.DefaultModel != "" {
			t.Errorf("未连接时 agents[%s] = %+v，期望不可用且无默认模型", a.Provider, a)
		}
	}

	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:     store.AgentProviderCodex,
		Label:        "订阅一号",
		AccountID:    agentAcctID,
		DefaultModel: "gpt-5-codex",
		AuthJSON:     agentAuthJSON(agentAccess, agentRefresh, agentAcctID),
	})
	if err != nil {
		t.Fatalf("连接订阅: %v", err)
	}
	for _, cookie := range []string{root} {
		got := agentEntry(t, e.access(cookie).Agents, "codex")
		if !got.Available || got.DefaultModel != "gpt-5-codex" {
			t.Errorf("已连接时 agents[codex] = %+v，期望 {codex true gpt-5-codex}", got)
		}
		// 只连了 codex：grok 那一项照常在场且不可用。
		if g := agentEntry(t, e.access(cookie).Agents, "grok"); g.Available || g.DefaultModel != "" {
			t.Errorf("未连接 grok 时 agents[grok] = %+v，期望不可用", g)
		}
		if c := agentEntry(t, e.access(cookie).Agents, "claude"); c.Available || c.DefaultModel != "" {
			t.Errorf("未连接 claude 时 agents[claude] = %+v，期望不可用", c)
		}
		if c := agentEntry(t, e.access(cookie).Agents, "cursor"); c.Available || c.DefaultModel != "" {
			t.Errorf("未连接 cursor 时 agents[cursor] = %+v，期望不可用", c)
		}
	}
	claudeCred, err := claudeauth.FromSetupToken("fake-claude-setup-credential-never-real")
	if err != nil {
		t.Fatal(err)
	}
	claudeBlob, err := claudeCred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderClaude, AuthJSON: claudeBlob,
	}); err != nil {
		t.Fatal(err)
	}
	if c := agentEntry(t, e.access(root).Agents, "claude"); !c.Available {
		t.Errorf("已连接 claude 时 agents[claude] = %+v，期望可用", c)
	}
	cursorCred, err := cursorauth.FromAPIKey("fake-cursor-dashboard-key-never-real")
	if err != nil {
		t.Fatal(err)
	}
	cursorBlob, err := cursorCred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCursor, AuthJSON: cursorBlob,
	}); err != nil {
		t.Fatal(err)
	}
	if c := agentEntry(t, e.access(root).Agents, "cursor"); !c.Available {
		t.Errorf("已连接 cursor 时 agents[cursor] = %+v，期望可用", c)
	}

	// 键集：每项只有 provider/available/default_model 三键。多一个字段就是把
	// 管理员视角的账号信息讲给了全体用户。
	resp := e.do("GET", "/admin/v1/endpoints", root, "")
	wantStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	var raw struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("解析读数: %v", err)
	}
	if len(raw.Agents) != 4 {
		t.Fatalf("agents 项数 = %d，期望 4", len(raw.Agents))
	}
	for _, entry := range raw.Agents {
		if len(entry) != 3 {
			t.Errorf("agents 项的键 = %v，期望恰好 provider/available/default_model 三个", keysOf(entry))
		}
		for _, k := range []string{"available", "provider", "default_model"} {
			if _, ok := entry[k]; !ok {
				t.Errorf("agents 项缺键 %q（恒在场，缺省会让前端把「读不到」当成「没连」）", k)
			}
		}
	}
	// 泄露检查：账号信息与凭据一个字节都不该出现。
	for _, leak := range []string{"订阅一号", agentAcctID, agentAccess, agentRefresh, agentIDToken,
		"fake-cursor-dashboard-key-never-real"} {
		if strings.Contains(body, leak) {
			t.Errorf("接入读数泄露 %q：\n%s", leak, body)
		}
	}

	// 停用与登录失效都算不可用，且默认模型一并收回——页面此时根本不渲染命令，
	// 给了只会被当成「可以照抄」的假信号。判据与数据面 /v1/responses 逐字一致。
	for _, status := range []string{store.AgentStatusDisabled, store.AgentStatusAuthExpired} {
		if err := e.st.SetAgentStatus(t.Context(), acct.ID, status); err != nil {
			t.Fatalf("置 %s: %v", status, err)
		}
		got := agentEntry(t, e.access(root).Agents, "codex")
		if got.Available || got.DefaultModel != "" {
			t.Errorf("status=%s 时 agents[codex] = %+v，期望不可用且不带默认模型", status, got)
		}
	}
}

// keysOf 取 map 的键（只为断言失败时那句话能指名道姓）。
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEndpointsNoUpstreamLeak：清单只给客户端可见的模型名与入口协议。上游
// 账户名、来源侧模型 ID、凭据都是对客户端隐藏的（架构 §11.1），而这个端点
// 对 member 开放——漏一个字段就是把「上游是谁」讲给了全体用户。
func TestEndpointsNoUpstreamLeak(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"secret-account","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	m := e.createModel(root, "public-name")
	e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"vendor-internal-id"}`, up.ID))

	resp := e.do("GET", "/admin/v1/endpoints", root, "")
	wantStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	for _, leak := range []string{"secret-account", "vendor-internal-id", upstreamKeyPlaintext, "deepseek"} {
		if strings.Contains(body, leak) {
			t.Errorf("接入读数泄露 %q：\n%s", leak, body)
		}
	}
}
