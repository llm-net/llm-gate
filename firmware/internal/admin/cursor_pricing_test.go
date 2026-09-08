package admin_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func TestCursorPricingReadOnlyCatalog(t *testing.T) {
	e := newEnv(t)
	const path = "/admin/v1/cursor-pricing"
	resp := e.do("GET", path, "", "")
	wantStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
	cookie := e.rootSession()
	read := func() []struct {
		Name    string
		Pricing usage.Pricing
	} {
		var out struct {
			Models []struct {
				Name    string
				Pricing usage.Pricing
			}
		}
		resp := e.do("GET", path, cookie, "")
		wantStatus(t, resp, http.StatusOK)
		decodeInto(t, resp, &out)
		return out.Models
	}
	if got := read(); len(got) != 0 {
		t.Fatal("unconnected subscription exposed models")
	}
	account := seedAgentSubscription(t, e, store.AgentProviderCursor)
	if got := e.listModels(cookie); len(got) != 0 {
		t.Fatal("Cursor materialized shared API models")
	}
	agent, ok := platformcatalog.Builtin().Agent(store.AgentProviderCursor)
	if !ok || len(agent.Models) == 0 {
		t.Fatal("missing builtin Cursor catalog")
	}
	got := read()
	if len(got) != len(agent.Models) {
		t.Fatalf("models=%d want=%d", len(got), len(agent.Models))
	}
	for i, entry := range agent.Models {
		price, err := usage.ParseCursorPrice(string(entry.Pricing))
		if err != nil || got[i].Name != entry.Name || got[i].Pricing[usage.FieldIn] != price[usage.FieldIn] {
			t.Fatalf("catalog mismatch: %+v", got[i])
		}
	}
	// Local API prices, legacy overrides and observed names cannot add or edit rows.
	e.createModelBody(cookie, `{"name":"composer-2.5","pricing":{"in":1,"out":1}}`)
	if err := e.st.SetSetting(t.Context(), "cursor_model_pricing", `{"composer-2.5":{"in":0,"out":0},"local-only":{"in":1,"out":1}}`); err != nil {
		t.Fatal(err)
	}
	e.meter.Record(usage.Sample{At: time.Now(), Entry: usage.EntryCursorAgent, ModelName: "observed-only", Status: 200, UsageUnavailable: true})
	if current := read(); !reflect.DeepEqual(current, got) {
		t.Fatal("local data changed catalog")
	}
	for _, method := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		resp := e.do(method, path, cookie, `{"prices":{"local-only":{"in":1,"out":1}}}`)
		wantStatus(t, resp, http.StatusNotFound)
		resp.Body.Close()
	}
	// New effective data updates prices and membership without any usage first.
	doc, err := platformcatalog.Parse(platformcatalog.BuiltinRaw())
	if err != nil {
		t.Fatal(err)
	}
	doc.Version++
	for i := range doc.Agents {
		if doc.Agents[i].Provider == store.AgentProviderCursor {
			doc.Agents[i].Models = []platformcatalog.AgentModel{{Name: "catalog-only", Kind: "text", Pricing: json.RawMessage(`{"in":0,"out":0,"cache_read":0}`)}}
		}
	}
	raw, _ := json.Marshal(struct {
		File platformcatalog.Doc `json:"file"`
	}{doc})
	if err := e.st.SetSetting(t.Context(), "platform_models", string(raw)); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 1 || got[0].Name != "catalog-only" || got[0].Pricing[usage.FieldIn] != 0 {
		t.Fatalf("synced catalog not effective: %+v", got)
	}
	e.meter.Record(usage.Sample{At: time.Now(), Entry: usage.EntryCursorAgent, ModelName: "catalog-only", Status: 200, UsageUnavailable: true})
	report, err := e.meter.Report(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range report.ByModel {
		want := row.Key == "catalog-only"
		if row.Priced == nil || *row.Priced != want {
			t.Fatalf("wrong catalog pricing annotation: %+v", row)
		}
	}
	if err := e.st.SetSetting(t.Context(), "platform_models", `{"file":{"schema":"invalid"}}`); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != len(agent.Models) {
		t.Fatal("invalid data did not fall back to builtin")
	}
	if err := e.st.DeleteAgentAccount(t.Context(), account.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 0 {
		t.Fatal("disconnected subscription still shows prices")
	}
}
