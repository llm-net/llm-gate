package admin_test

// 「API密钥接入 → 添加模型」的验收：清单从固件内嵌的平台模型信息里取（不联网也
// 有得选）、协议面由账号定而不是让人挑、自定义名字照收（厂商上新时的出路）、
// 重复提交幂等、撞上别人的名字要 409 而不是偷走那一行。

import (
	"fmt"
	"net/http"
	"testing"
)

type upModelDTO struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Family          string `json:"family"`
	UpstreamModelID string `json:"upstream_model_id"`
	Note            string `json:"note"`
	Added           bool   `json:"added"`
	InCatalog       bool   `json:"in_catalog"`
	Blocked         string `json:"blocked"`
}

type upModelsDTO struct {
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	UpstreamType string `json:"upstream_type"`
	Priority     int64  `json:"priority"`
	Kinds        []struct {
		Kind   string `json:"kind"`
		Family string `json:"family"`
	} `json:"kinds"`
	Catalog struct {
		Origin    string `json:"origin"`
		Version   int64  `json:"version"`
		UpdatedAt string `json:"updated_at"`
		Listed    bool   `json:"listed"`
		Vendor    string `json:"vendor"`
		Note      string `json:"note"`
		Source    string `json:"source"`
		CheckedAt string `json:"checked_at"`
	} `json:"catalog"`
	Models  []upModelDTO `json:"models"`
	Created int          `json:"created"`
	Added   int          `json:"added"`
}

func (e *env) upstreamModels(cookie string, upstreamID int64) upModelsDTO {
	e.t.Helper()
	resp := e.do("GET", fmt.Sprintf("/admin/v1/upstreams/%d/models", upstreamID), cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out upModelsDTO
	decodeInto(e.t, resp, &out)
	return out
}

func (e *env) addUpstreamModels(cookie string, upstreamID int64, body string) upModelsDTO {
	e.t.Helper()
	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", upstreamID), cookie, body)
	wantStatus(e.t, resp, http.StatusOK)
	var out upModelsDTO
	decodeInto(e.t, resp, &out)
	return out
}

func findUpModel(rows []upModelDTO, name string) (upModelDTO, bool) {
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return upModelDTO{}, false
}

// TestUpstreamModelsListsBuiltinCatalog：从未执行官网数据升级的设备照样有选单，
// 因为固件内嵌一份基线。
func TestUpstreamModelsListsBuiltinCatalog(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.upstreamModels(root, up.ID)
	if got.Catalog.Origin != "builtin" || !got.Catalog.Listed || got.Catalog.Version <= 0 {
		t.Fatalf("清单出处 = %+v，期望内嵌基线且已收录 deepseek", got.Catalog)
	}
	if got.Catalog.Source == "" || got.Catalog.CheckedAt == "" {
		t.Errorf("清单缺出处/核对日期：%+v", got.Catalog)
	}
	// deepseek 只服务文本双入口：种类清单里不该出现视频/图片，自定义表单也
	// 因此不必让人选族。
	if len(got.Kinds) != 1 || got.Kinds[0].Kind != "text" || got.Kinds[0].Family != "" {
		t.Fatalf("可承载种类 = %+v，期望仅 text", got.Kinds)
	}
	// 按量平台的缺省优先级是兜底档 200（订阅才是 100）。
	if got.Priority != 200 {
		t.Errorf("缺省优先级 = %d，期望 200", got.Priority)
	}
	m, ok := findUpModel(got.Models, "deepseek-v4-pro")
	if !ok {
		t.Fatalf("清单里没有 deepseek-v4-pro：%+v", got.Models)
	}
	if m.Kind != "text" || m.Family != "" || m.Added || m.InCatalog {
		t.Errorf("清单项 = %+v，期望未添加的文本模型", m)
	}
}

// TestUpstreamModelsAddCreatesModelAndSource：选单项与自定义项一起提交，一次
// 把模型行与来源行都建出来；再点一次是幂等空转。
func TestUpstreamModelsAddCreatesModelAndSource(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.addUpstreamModels(root, up.ID, `{"models":[
		{"name":"deepseek-v4-pro","kind":"text"},
		{"name":"deepseek-v9-brandnew","kind":"text","upstream_model_id":"ds-v9-internal"}]}`)
	if got.Created != 2 || got.Added != 2 {
		t.Fatalf("created=%d added=%d，期望 2/2", got.Created, got.Added)
	}
	if m, ok := findUpModel(got.Models, "deepseek-v4-pro"); !ok || !m.Added {
		t.Errorf("添加后清单项 = %+v, %v，期望标记已添加", m, ok)
	}

	models := e.modelsByName(root)
	custom, ok := models["deepseek-v9-brandnew"]
	if !ok {
		t.Fatalf("自定义模型没建出来：%v", models)
	}
	if custom.Kind != "text" || !custom.EntryOpenAI || !custom.EntryAnthropic {
		t.Errorf("自定义模型 = %+v，期望文本双入口", custom)
	}
	if len(custom.Sources) != 1 {
		t.Fatalf("自定义模型的来源数 = %d，期望 1", len(custom.Sources))
	}
	src := custom.Sources[0]
	if src.UpstreamID != up.ID || src.UpstreamModelID != "ds-v9-internal" || src.Priority != 200 {
		t.Errorf("来源 = %+v，期望挂在本账号上、带来源侧 ID、优先级 200", src)
	}

	// 幂等：同样的提交不再建行，也不再挂来源。
	again := e.addUpstreamModels(root, up.ID, `{"models":[{"name":"deepseek-v4-pro","kind":"text"}]}`)
	if again.Created != 0 || again.Added != 0 {
		t.Errorf("重复提交 created=%d added=%d，期望 0/0", again.Created, again.Added)
	}
}

// TestUpstreamModelsFamilyComesFromAccount：AIGC 的协议面由**账号**定——请求
// 不带 family 也能建对，带错了要被拒；账号不服务的种类一律不收。
func TestUpstreamModelsFamilyComesFromAccount(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	mm := e.createUpstream(root, fmt.Sprintf(`{"name":"mm","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.upstreamModels(root, mm.ID)
	if len(got.Kinds) != 1 || got.Kinds[0].Kind != "video" || got.Kinds[0].Family != "minimax_video" {
		t.Fatalf("可承载种类 = %+v，期望仅 video/minimax_video", got.Kinds)
	}
	if m, ok := findUpModel(got.Models, "MiniMax-H3"); !ok || m.Family != "minimax_video" {
		t.Fatalf("清单项 = %+v, %v", m, ok)
	}

	// 不带 family：由账号推出 minimax_video。
	e.addUpstreamModels(root, mm.ID, `{"models":[{"name":"MiniMax-H3","kind":"video"}]}`)
	if m := e.modelsByName(root)["MiniMax-H3"]; m.Family != "minimax_video" {
		t.Errorf("模型协议面 = %q，期望 minimax_video", m.Family)
	}

	// 这个账号没有文本入口：文本模型不该收下（否则建出一行永远打不通的死行）。
	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", mm.ID), root,
		`{"models":[{"name":"some-chat","kind":"text"}]}`)
	wantStatus(t, resp, http.StatusBadRequest)
	// 族写错同样拒绝（挡住清单文件发布错误与手写请求）。
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", mm.ID), root,
		`{"models":[{"name":"MiniMax-H4","kind":"video","family":"ark_video"}]}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if _, ok := e.modelsByName(root)["some-chat"]; ok {
		t.Error("被拒的请求不该建出模型行")
	}
}

// TestUpstreamModelsNameConflict：同名行已被另一种模型占着时 409，绝不认领
// ——名字就是客户端调用名，覆盖等于偷走别人的入口。
func TestUpstreamModelsNameConflict(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	e.createModelKind(root, "shared-name", "text")
	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.upstreamModels(root, ark.ID)
	// ark 三种入口都服务：种类清单三条，视频/图片各带自己的族。
	if len(got.Kinds) != 3 {
		t.Fatalf("可承载种类 = %+v，期望三种", got.Kinds)
	}
	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", ark.ID), root,
		`{"models":[{"name":"shared-name","kind":"video"}]}`)
	wantStatus(t, resp, http.StatusConflict)

	// 同名同种类则是"给既有模型多挂一条来源"，不是冲突。
	after := e.addUpstreamModels(root, ark.ID, `{"models":[{"name":"shared-name","kind":"text"}]}`)
	if after.Created != 0 || after.Added != 1 {
		t.Fatalf("created=%d added=%d，期望 0/1（只挂来源）", after.Created, after.Added)
	}
}

// TestUpstreamModelsQwenPlanCatalog：Token Plan 的官方文本型号形成选单；选单
// 不是白名单，厂商上新但数据目录尚未收录时，自定义仍然照走。
func TestUpstreamModelsQwenPlanCatalog(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"qp","type":"qwen_plan","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.upstreamModels(root, up.ID)
	want := map[string]bool{
		"qwen3.8-max": true, "qwen3.8-flash": true, "qwen3.7-max": true,
		"qwen3.7-plus": true, "qwen3.6-flash": true, "deepseek-v4-pro": true,
		"deepseek-v4-pro-0813": true, "deepseek-v4-flash-0731": true, "glm-5.2": true,
	}
	if len(got.Models) != len(want) {
		t.Fatalf("qwen_plan 选单条目数 = %d，期望 %d：%+v", len(got.Models), len(want), got.Models)
	}
	for _, m := range got.Models {
		if !want[m.Name] || m.Kind != "text" || m.Family != "" {
			t.Errorf("qwen_plan 选单含意外条目：%+v", m)
		}
	}
	// 订阅型的缺省优先级是 100（先用满已付费额度）。
	if got.Priority != 100 {
		t.Errorf("缺省优先级 = %d，期望 100", got.Priority)
	}
	added := e.addUpstreamModels(root, up.ID, `{"models":[{"name":"qwen-custom-max","kind":"text"}]}`)
	if added.Created != 1 || added.Added != 1 {
		t.Fatalf("created=%d added=%d，期望 1/1", added.Created, added.Added)
	}
}

// TestUpstreamModelsRequireSession：无会话碰不到这条入口。
func TestUpstreamModelsRequireSession(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))

	wantStatus(t, e.do("GET", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), "", ""), http.StatusUnauthorized)
	wantStatus(t, e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), "",
		`{"models":[{"name":"x-model","kind":"text"}]}`), http.StatusUnauthorized)
}
