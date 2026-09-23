package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestSystemOneManagementAndAccess(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, `{"name":"semif","type":"systemone","api_key":"fake-systemone-key","base_url":"http://192.168.1.30:18080"}`)
	if up.BillingMode != "none" || up.CatalogID != "systemone" || up.BalanceSupported {
		t.Fatalf("upstream: %+v", up)
	}
	m := e.createModelBody(root, `{"name":"jev-latest","kind":"systemone","pricing":{"in":0,"out":0}}`)
	if m.Kind != "systemone" || m.Family != "systemone" {
		t.Fatalf("model: %+v", m)
	}
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	if len(src.Protocols) != 1 || src.Protocols[0] != "systemone" {
		t.Fatalf("protocols: %+v", src.Protocols)
	}
	resp := e.do("GET", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), root, "")
	wantStatus(t, resp, 200)
	var choices struct {
		Kinds []struct{ Kind, Family string }
	}
	decodeInto(t, resp, &choices)
	if len(choices.Kinds) != 1 || choices.Kinds[0].Kind != "systemone" || choices.Kinds[0].Family != "systemone" {
		t.Fatalf("choices: %+v", choices)
	}
	// 自定义模型可从账号的添加模型动作建立，与独立建模走同一约束。
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), root, `{"models":[{"name":"judge-two","kind":"systemone","upstream_model_id":"jev-preview"}]}`)
	wantStatus(t, resp, 200)
	text := e.createModel(root, "text-only")
	resp = e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", text.ID), root, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	wantStatus(t, resp, http.StatusBadRequest)
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"base_url":"http://127.0.0.1:18080"}`)
	wantStatus(t, resp, 400)
	if got := errCode(t, resp); got != "base_url_requires_key" {
		t.Fatal(got)
	}
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"base_url":"http://127.0.0.1:18080","api_key":"fake-replacement-key"}`)
	wantStatus(t, resp, 200)
	resp.Body.Close()
	resp = e.do("GET", "/admin/v1/endpoints", root, "")
	wantStatus(t, resp, 200)
	var snap struct {
		SystemOneModels []struct{ Name string } `json:"systemone_models"`
		Counts          map[string]struct {
			SystemOne int `json:"systemone"`
		} `json:"api_model_counts"`
	}
	decodeInto(t, resp, &snap)
	if len(snap.SystemOneModels) != 2 || snap.Counts["usage"].SystemOne != 2 {
		t.Fatalf("snapshot: %+v", snap)
	}
	// 同一快照实现用 Key 授权裁剪，不把其他语义判断模型交给持有人。

	key, _ := e.createKey(root, "semantic-client")
	if _, _, err := e.st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{KeyID: key.ID, Restricted: true, ModelIDs: []int64{m.ID}}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.srv.KeyAccessSnapshot(e.req("GET", "/gate-helper/v1/endpoints", "", ""), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var holder struct {
		Models []struct{ Name string } `json:"systemone_models"`
	}
	if err := json.Unmarshal(body, &holder); err != nil {
		t.Fatal(err)
	}
	if len(holder.Models) != 1 || holder.Models[0].Name != m.Name {
		t.Fatalf("key scope: %+v", holder)
	}
}
