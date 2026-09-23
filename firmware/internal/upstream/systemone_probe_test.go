package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

const mixedProbeResponse = `{"model":"backend","answers":{"choice":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"technical":0.1},"confidence":0.8},"noul":{"type":"noul","noul":0.95},"score":{"type":"score","score":1.6,"probabilities":{"0":0.1,"1":0.2,"2":0.7},"legend":{"0":"可以等待","1":"本周处理","2":"今天处理"},"confidence":0.55}},"usage":{"input_tokens":300,"output_tokens":0}}`

func TestSystemOneProbeMixedSingleRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/systemone" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fake-probe-key" {
			t.Error("incorrect probe request")
		}
		var payload struct {
			Model     string
			State     string
			Questions map[string]struct {
				Type         string
				Instructions string
				Criteria     json.RawMessage
			}
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "wire-model" || payload.State == "" || len(payload.Questions) != 3 {
			t.Error("probe must carry one shared state and three questions")
		}
		for _, kind := range []string{"choice", "noul", "score"} {
			q, ok := payload.Questions[kind]
			if !ok || q.Type != kind || q.Instructions == "" || len(q.Criteria) == 0 {
				t.Errorf("missing explicit %s question", kind)
			}
		}
		io.WriteString(w, mixedProbeResponse)
	}))
	defer server.Close()
	for _, typ := range []string{config.UpstreamSystemOne, config.UpstreamGeneric} {
		urls, _ := json.Marshal(map[string]string{config.ProtocolSystemOne: server.URL})
		account := Account{Type: typ, BaseURL: server.URL, ProtocolURLs: string(urls), APIKey: "fake-probe-key"}
		before := calls.Load()
		result := Probe(t.Context(), testClient(), account, config.ProtocolSystemOne, "wire-model")
		if calls.Load() != before+1 || !result.OK || result.Status != 200 || len(result.Questions) != 3 {
			t.Fatalf("mixed probe: %+v", result)
		}
		for i, kind := range []string{"choice", "noul", "score"} {
			if !result.Questions[i].OK || result.Questions[i].Type != kind {
				t.Fatalf("question results: %+v", result.Questions)
			}
		}
		if result.Questions[0].Choice != "billing" || *result.Questions[1].Noul != 0.95 || *result.Questions[2].Score != 1.6 {
			t.Fatal("validated answers not returned")
		}
	}
}

func TestSystemOneProbeValidatesEveryAnswer(t *testing.T) {
	tests := []struct {
		name, old, replacement string
		badQuestion            int
	}{
		{"choice_missing", `"choice":{"type":"choice"`, `"unknown":{"type":"choice"`, 0},
		{"choice_type", `"type":"choice"`, `"type":"noul"`, 0},
		{"choice_secret", `"choice":"billing"`, `"choice":"fake-secret-value"`, 0},
		{"choice_not_peak", `"choice":"billing"`, `"choice":"technical"`, 0},
		{"probability_sum", `"billing":0.9`, `"billing":0.5`, 0},
		{"probability_null", `"technical":0.1`, `"technical":null`, 0},
		{"confidence_missing", `,"confidence":0.8`, ``, 0},
		{"confidence_range", `"confidence":0.8`, `"confidence":1.2`, 0},
		{"noul_range", `"noul":0.95`, `"noul":1.5`, 1},
		{"noul_confidence", `"noul":0.95`, `"noul":0.95,"confidence":null`, 1},
		{"score_weight", `"score":1.6`, `"score":1.5`, 2},
		{"score_legend", `"2":"今天处理"`, `"2":"fake-secret-value"`, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(mixedProbeResponse, tc.old, tc.replacement, 1)
			var p map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatal(err)
			}
			results, ok := systemOneProbeResults(p["answers"])
			if ok || len(results) != 3 || results[tc.badQuestion].OK {
				t.Fatalf("invalid answer accepted: %+v", results)
			}
			for i, result := range results {
				if i != tc.badQuestion && !result.OK {
					t.Fatalf("other question lost: %+v", results)
				}
			}
			safe, _ := json.Marshal(results)
			if strings.Contains(string(safe), "fake-secret-value") {
				t.Fatal("arbitrary upstream value reflected")
			}
		})
	}
}

func TestSystemOneProbeZeroValuesAndErrors(t *testing.T) {
	body := strings.Replace(mixedProbeResponse, `"noul":0.95`, `"noul":0`, 1)
	body = strings.Replace(body, `"score":1.6`, `"score":0`, 1)
	body = strings.Replace(body, `"0":0.1,"1":0.2,"2":0.7`, `"0":1,"1":0,"2":0`, 1)
	var p map[string]json.RawMessage
	json.Unmarshal([]byte(body), &p)
	results, ok := systemOneProbeResults(p["answers"])
	if !ok || results[1].Noul == nil || *results[1].Noul != 0 || results[2].Score == nil || *results[2].Score != 0 {
		t.Fatal("valid zero answers were lost")
	}
	for _, status := range []int{422, 529} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"detail":"fake-secret-value"}`)
		}))
		result := Probe(t.Context(), testClient(), Account{Type: config.UpstreamSystemOne, BaseURL: server.URL, APIKey: "fake-probe-key"}, config.ProtocolSystemOne, "wire-model")
		server.Close()
		if result.OK || result.Status != status || len(result.Questions) != 0 || strings.Contains(result.Message, "fake-secret-value") {
			t.Fatalf("bad error result: %+v", result)
		}
	}
}
