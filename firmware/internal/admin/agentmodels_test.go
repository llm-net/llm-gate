package admin_test

// agentmodels_test.go 是「Agent 订阅模型由模型目录数据定义」的可执行验收：
//
//	连上订阅 → 目录里自动长出这份订阅的文本计价行（不挂来源，上游就是订阅），
//	           没连的 provider 一行都不长；
//	撤掉订阅 → 行随之回收；
//	整组只读 → 改名/录价/启停/删除全拒；
//	不越界   → 管理员亲手挂了 API 上游的同名行一根手指都不碰。

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// grokAgentAuthJSON 是一份最小假订阅句柄（形态校验只在 import 路径；store
// 层照单全收——这里只需要 agent_accounts 里有一行 grok）。
const grokAgentAuthJSON = `{"https://auth.x.ai::fake-client":{"key":"fake-access-token","refresh_token":"fake-refresh-token"}}`

// seedAgentSubscription 直接经 store 连上一份假订阅（登录/导入流程另有专测），
// 再跑一次收敛——等价于"设备重启时对齐"那条触发路径。
func seedAgentSubscription(t *testing.T, e *env, provider string) *store.AgentAccount {
	t.Helper()
	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:  provider,
		Label:     "订阅一号",
		AccountID: "agent-user@example.invalid",
		AuthJSON:  grokAgentAuthJSON,
	})
	if err != nil {
		t.Fatalf("连接假订阅: %v", err)
	}
	e.srv.SyncAgentModels(t.Context())
	return acct
}

// TestAgentModelsMaterializeFromCatalog：连上 Grok Build 之后，内嵌目录数据
// agents 段里那份清单整份出现在模型目录里——文本行带 agent 注记且不挂来源
// （上游就是订阅），除此之外一行都不长；没连的 Codex 一行都不长。
func TestAgentModelsMaterializeFromCatalog(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	if got := e.listModels(root); len(got) != 0 {
		t.Fatalf("没连订阅时不该有模型行: %+v", got)
	}
	seedAgentSubscription(t, e, store.AgentProviderGrok)

	byName := map[string]modelDTO{}
	for _, m := range e.listModels(root) {
		byName[m.Name] = m
	}
	// 文本计价行：无来源、带 grok 注记（管理台据此归订阅接入标签页）。
	for _, name := range []string{"grok-4.6", "grok-4.5"} {
		m, ok := byName[name]
		if !ok || m.Kind != "text" || m.Agent != store.AgentProviderGrok || len(m.Sources) != 0 {
			t.Errorf("%s = %+v，期望 text / agent=grok / 无来源", name, m)
		}
	}
	// 订阅带的只有文本行：agents 段只收文本模型，不会长出别的种类。
	if len(byName) != 2 {
		t.Errorf("Grok 订阅下的模型 = %+v，期望只有两条文本行", byName)
	}
	// 没连 Codex：它的模型一行都不该长出来。
	for _, name := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if _, ok := byName[name]; ok {
			t.Errorf("没连 Codex 订阅却有 %s 的模型行", name)
		}
	}
	// 幂等：再收敛一次，行数不变。
	before := len(e.listModels(root))
	e.srv.SyncAgentModels(t.Context())
	if after := len(e.listModels(root)); after != before {
		t.Errorf("重复收敛后模型数 = %d，期望仍是 %d", after, before)
	}
}

// TestAgentModelsFollowSubscriptionLifecycle：撤掉订阅就撤掉它的模型
// （「只要连接了订阅就读出来显示」的另一半）；另一份订阅连上时只长自己那些行。
func TestAgentModelsFollowSubscriptionLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	acct := seedAgentSubscription(t, e, store.AgentProviderGrok)
	if len(e.listModels(root)) == 0 {
		t.Fatal("连上订阅后应当有模型行")
	}

	// 撤订阅走真实端点：收敛在写响应之前跑完，界面刷新读到的就是空列表。
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/agent-accounts/%d", acct.ID), root, ""), http.StatusNoContent)
	if got := e.listModels(root); len(got) != 0 {
		t.Fatalf("撤掉订阅后仍有模型行: %+v", got)
	}

	// 换 Codex：只长它自己的四条文本行，grok 的不该出现。
	seedAgentSubscription(t, e, store.AgentProviderCodex)
	byName := map[string]modelDTO{}
	for _, m := range e.listModels(root) {
		byName[m.Name] = m
	}
	if len(byName) != 4 {
		t.Fatalf("Codex 订阅下的模型 = %+v，期望四条文本行", byName)
	}
	for _, name := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		m, ok := byName[name]
		if !ok || m.Kind != "text" || m.Agent != store.AgentProviderCodex {
			t.Errorf("%s = %+v，期望 text / agent=codex", name, m)
		}
	}
}

// TestAgentModelsReadOnlyGuards：这批行整组只读——管理台不摆按钮，接口这一层
// 也一律拒。稳定机读值：model_agent_managed。
func TestAgentModelsReadOnlyGuards(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderGrok)

	var text modelDTO
	for _, m := range e.listModels(root) {
		if m.Name == "grok-4.6" {
			text = m
		}
	}
	if text.ID == 0 {
		t.Fatalf("收敛出的行不齐: text=%+v", text)
	}

	// 改名 / 录价 / 启停 / 删除全拒。
	for _, body := range []string{`{"name":"renamed"}`, `{"pricing":{"in":1}}`, `{"disabled":true}`} {
		resp := e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", text.ID), root, body)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "model_agent_managed" {
			t.Errorf("PATCH %s(%s) 错误码 = %q，期望 model_agent_managed", text.Name, body, got)
		}
	}
	resp := e.do("DELETE", fmt.Sprintf("/admin/v1/models/%d", text.ID), root, "")
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "model_agent_managed" {
		t.Errorf("DELETE %s 错误码 = %q，期望 model_agent_managed", text.Name, got)
	}
}

// TestAgentModelsLeaveAdminRowsAlone：管理员亲手挂了 API 上游的同名行不归
// 收敛器——不被改价、不被回收、照旧可编辑（那是他的 API 模型，只是恰好重名）。
// 名字被一个**种类不同**的行占着时同理：收敛器跳过，不抢名字。
func TestAgentModelsLeaveAdminRowsAlone(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 走「API密钥接入 → 添加模型」建行（建行连来源一起建，这是订阅同名模型进
	// API 侧的唯一入口；通用建模面对这些名字答 model_agent_managed，见下）。
	up := e.createUpstream(root, `{"name":"my-xai-account","type":"openai_compat","api_key":"sk-fake-key-0001","base_url":"https://api.example.invalid/v1"}`)
	e.addUpstreamModels(root, up.ID, `{"models":[{"name":"grok-4.6","kind":"text"}]}`)
	var mine modelDTO
	for _, m := range e.listModels(root) {
		if m.Name == "grok-4.6" {
			mine = m
		}
	}
	if mine.ID == 0 || len(mine.Sources) != 1 {
		t.Fatalf("管理员那条 grok-4.6 没建成: %+v", mine)
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", mine.ID), root,
		`{"pricing":{"in":1000000,"out":2000000}}`), http.StatusOK)
	// 名字被一个视频模型占着的订阅文本名（种类不符）：收敛器不抢名字。
	occupied := e.createModelKind(root, "grok-4.5", "video")

	seedAgentSubscription(t, e, store.AgentProviderGrok)

	byName := map[string]modelDTO{}
	for _, m := range e.listModels(root) {
		byName[m.Name] = m
	}
	got := byName["grok-4.6"]
	if got.ID != mine.ID || len(got.Sources) != 1 || got.Pricing["in"] != 1000000 {
		t.Fatalf("管理员的 grok-4.6 被动过: %+v", got)
	}
	// 它仍然可编辑（挂了上游 = 归 API密钥接入侧）。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", mine.ID), root, `{"pricing":{"in":3000000,"out":4000000}}`), http.StatusOK)

	stuck := byName["grok-4.5"]
	if stuck.ID != occupied.ID || stuck.Kind != "video" || stuck.Agent != "" || len(stuck.Sources) != 0 {
		t.Fatalf("被占名的行被动过: %+v", stuck)
	}

	// 通用建模面对目录点过名的文本模型答 model_agent_managed（不管那份订阅
	// 连没连）：在那里建一条空行只会被下一轮收敛回收，不如当场说清楚。
	resp := e.do("POST", "/admin/v1/models", root, `{"name":"gpt-5.6-luna","kind":"text"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "model_agent_managed" {
		t.Errorf("建同名空行的错误码 = %q，期望 model_agent_managed", code)
	}
}

// TestAgentSubscriptionModelsMatchAdminView 是「两处读数同源」的绊线：数据面
// `GET /agents/v1/models` 列出来的，必须与管理台模型视图里带 agent 注记的那批
// text 行**完全一致**——管理台把某一行画在订阅接入标签页上，订阅用户的 CLI
// 就该在模型清单里看见同一个名字。判据因此抽在 agentSubscriptionProvider 一处；
// 各写一遍必然分叉。
func TestAgentSubscriptionModelsMatchAdminView(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	seedAgentSubscription(t, e, store.AgentProviderGrok)
	// 一条管理员自己的图片模型：非 text 行不该进订阅模型读数。
	e.createModelKind(root, "my-free-image", "image")

	want := map[string]string{}
	media := []string{}
	for _, m := range e.listModels(root) {
		if m.Agent != "" {
			want[m.Name] = m.Agent
		}
		if m.Kind != "text" {
			media = append(media, m.Name)
		}
	}
	if len(want) == 0 || len(media) == 0 {
		t.Fatalf("夹具不成立（带注记的 text 行 %d 条、非 text 行 %d 条），用例失去意义", len(want), len(media))
	}

	got, err := e.srv.AgentSubscriptionModels(t.Context())
	if err != nil {
		t.Fatalf("AgentSubscriptionModels: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("条数 = %d，期望 %d\n数据面 %+v\n管理台 %+v", len(got), len(want), got, want)
	}
	for name, provider := range want {
		if got[name] != provider {
			t.Errorf("%s: 数据面 provider = %q，管理台 = %q", name, got[name], provider)
		}
	}
	// 非 text 行不进这条读数：/agents/v1/responses 是文本入口，把图片模型名
	// 列进 codex 的选择器只会让人选中一个调不通的名字。
	for _, name := range media {
		if _, ok := got[name]; ok {
			t.Errorf("非 text 行 %s 不该出现在订阅模型读数里", name)
		}
	}
}

// TestAgentSubscriptionModelsSkipAdminOwnedRow 是同一条安全边界在数据面读数
// 上的复验：管理员亲手把同名模型挂到 API 上游上之后，那一行归他（API密钥接入
// 标签页照常渲染、照常可编辑），订阅读数里就不该再有它——否则 CLI 列出来的
// 名字实际走的是按量上游，与「这是订阅带的模型」这句话对不上。
func TestAgentSubscriptionModelsSkipAdminOwnedRow(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 先建管理员那条同名行（建行连来源一起建，同 TestAgentModelsLeaveAdminRowsAlone
	// 的入口），再连订阅——收敛器会绕开它。
	up := e.createUpstream(root, `{"name":"my-xai-account","type":"openai_compat","api_key":"sk-fake-key-0001","base_url":"https://api.example.invalid/v1"}`)
	e.addUpstreamModels(root, up.ID, `{"models":[{"name":"grok-4.6","kind":"text"}]}`)
	seedAgentSubscription(t, e, store.AgentProviderGrok)

	got, err := e.srv.AgentSubscriptionModels(t.Context())
	if err != nil {
		t.Fatalf("AgentSubscriptionModels: %v", err)
	}
	if _, ok := got["grok-4.6"]; ok {
		t.Errorf("grok-4.6 挂着 API 上游，是管理员的模型，不该算订阅模型：%+v", got)
	}
	// 同一份订阅里没被占用的那个名字照常在——用例才证明得了「只少这一条」。
	if got["grok-4.5"] != store.AgentProviderGrok {
		t.Errorf("grok-4.5 应当仍是 grok 订阅模型：%+v", got)
	}
}
