package admin_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const systemOneMixedProbeResponse = `{"model":"wire-judge","answers":{"choice":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"technical":0.1},"confidence":0.8},"noul":{"type":"noul","noul":0.95},"score":{"type":"score","score":1.6,"probabilities":{"0":0.1,"1":0.2,"2":0.7},"legend":{"0":"可以等待","1":"本周处理","2":"今天处理"},"confidence":0.55}}}`

func TestSystemOneSourceMixedProbe(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload struct {
			Model     string
			State     string
			Questions map[string]struct{ Type, Instructions string }
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "wire-judge" || len(payload.Questions) != 3 {
			t.Error("expected a single mixed request using source model ID")
		}
		for _, kind := range []string{"choice", "noul", "score"} {
			if payload.Questions[kind].Type != kind || payload.Questions[kind].Instructions == "" {
				t.Errorf("missing %s", kind)
			}
		}
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer fake-mixed-key" {
			t.Error("incorrect path/auth")
		}
		io.WriteString(w, systemOneMixedProbeResponse)
	}))
	defer server.Close()
	for _, typ := range []string{"systemone", "generic"} {
		t.Run(typ, func(t *testing.T) {
			extra := fmt.Sprintf(`"base_url":%q`, server.URL)
			if typ == "generic" {
				extra = fmt.Sprintf(`"protocol_urls":{"systemone":%q}`, server.URL)
			}
			up := e.createUpstream(root, fmt.Sprintf(`{"name":%q,"type":%q,"api_key":"fake-mixed-key",%s}`, typ, typ, extra))
			m := e.createModelBody(root, fmt.Sprintf(`{"name":%q,"kind":"systemone"}`, typ+"-judge"))
			source := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"wire-judge"}`, up.ID))
			before := calls.Load()
			response := e.do("POST", fmt.Sprintf("/admin/v1/sources/%d/test", source.ID), root, "")
			wantStatus(t, response, 200)
			var report struct {
				Results []struct {
					Protocol  string
					OK        bool
					Status    int
					Questions []struct {
						Type        string
						OK          bool
						Choice      string
						Noul, Score *float64
					}
				}
			}
			decodeInto(t, response, &report)
			if calls.Load() != before+1 || len(report.Results) != 1 || !report.Results[0].OK || report.Results[0].Status != 200 || report.Results[0].Protocol != "systemone" {
				t.Fatalf("report: %+v", report)
			}
			qs := report.Results[0].Questions
			if len(qs) != 3 || qs[0].Type != "choice" || !qs[0].OK || qs[0].Choice != "billing" || qs[1].Noul == nil || qs[2].Score == nil {
				t.Fatalf("missing question results: %+v", qs)
			}
		})
	}
	for _, secret := range []string{"fake-mixed-key", "客户要求退款", "\"probabilities\"", "\"answers\""} {
		if strings.Contains(e.buf.String(), secret) || strings.Contains(strings.Join(e.auditDetails("source.test"), "\n"), secret) {
			t.Fatal("test content leaked to logs/audit")
		}
	}
}
