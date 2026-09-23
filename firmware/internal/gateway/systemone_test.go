package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const systemOnePath = "/typesafe/v1/systemone"
const systemOneRequest = ` {"state":{"message":"private-state-marker", "model":"nested"}, "model" : "judge", "questions":{"z":{"type":"choice","criteria":{"z":null,"a":{"n":1.00}}},"n":{"type":"noul","instructions":null},"s":{"type":"score","criteria":[{"rank":0},["one"]]}}} `
const systemOneResponse = ` {"model":"semif-qwen3.5-4b-bf16","answers":{"z":{"type":"choice","choice":"z","probabilities":{"z":0.5,"a":0.5},"confidence":0},"n":{"type":"noul","noul":0.95},"s":{"type":"score","score":0.7,"legend":{"0":{"rank":0},"1":["one"]},"probabilities":{"0":0.3,"1":0.7},"confidence":0.4}},"usage":{"input_tokens":292,"output_tokens":0}} `

func newSystemOneEnv(t *testing.T, respond http.HandlerFunc) (*routeEnv, *fakeMeter, int64) {
	t.Helper()
	e := newRouteEnv(t)
	server := httptest.NewServer(respond)
	t.Cleanup(server.Close)
	up := dbUpstream(t, e.st, "semif", config.UpstreamSystemOne, "fake-systemone-upstream-key", server.URL)
	model := dbKindModel(t, e.st, "judge", store.ModelKindSystemOne)
	dbSource(t, e.st, model, up, "jev-latest", 100)
	if err := e.st.SetModelPricing(t.Context(), model, `{"in":1000000,"out":2000000}`); err != nil {
		t.Fatal(err)
	}
	meter := &fakeMeter{}
	e.srv.EnableMetering(meter)
	return e, meter, model
}

func TestSystemOneMixedBytePreservationAndUsage(t *testing.T) {
	e, meter, _ := newSystemOneEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" {
			t.Errorf("upstream path: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fake-systemone-upstream-key" {
			t.Error("upstream authorization not replaced")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("client cookie forwarded")
		}
		body, _ := io.ReadAll(r.Body)
		want := strings.Replace(systemOneRequest, `"judge"`, `"jev-latest"`, 1)
		if string(body) != want {
			t.Errorf("request bytes changed beyond model: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-typesafe-request-id", "upstream-request-id")
		w.Header().Set("x-semif-backend", "semif-qwen3.5-4b-bf16")
		w.Header().Set("x-semif-confidence-method", "normalized-peak-v1")
		io.WriteString(w, systemOneResponse)
	})
	w := do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
	if w.Code != 200 || w.Body.String() != systemOneResponse {
		t.Fatalf("response changed: %d %s", w.Code, w.Body.String())
	}
	for key, want := range map[string]string{"x-typesafe-request-id": "upstream-request-id", "x-semif-backend": "semif-qwen3.5-4b-bf16", "x-semif-confidence-method": "normalized-peak-v1"} {
		if w.Header().Get(key) != want {
			t.Errorf("lost header %s", key)
		}
	}
	sample := meter.only(t)
	if sample.Entry != usage.EntrySystemOne || sample.Kind != store.ModelKindSystemOne || sample.ModelName != "judge" || sample.Tokens.Prompt != 292 || sample.Tokens.Completion != 0 || sample.Estimated || sample.UsageUnavailable {
		t.Fatalf("wrong sample: %+v", sample)
	}
	p, err := usage.ParsePricing(sample.Pricing)
	if err != nil {
		t.Fatal(err)
	}
	if got := usage.Cost(p, usage.Measure{Kind: sample.Kind, Entry: sample.Entry, Tokens: sample.Tokens}); got != 292 {
		t.Errorf("cost = %d", got)
	}
	if strings.Contains(e.logBuf.String(), "private-state-marker") || strings.Contains(e.logBuf.String(), "fake-systemone-upstream-key") || strings.Contains(e.logBuf.String(), `"answers"`) {
		t.Fatal("sensitive body or credential logged")
	}
}

func TestSystemOneErrorsAndInputLimit(t *testing.T) {
	for _, status := range []int{401, 404, 413, 422, 429, 500, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := `{"detail":[{"loc":["body","questions"],"msg":"invalid","input":"private-error-marker","ctx":{"limit":10}}]}`
			e, meter, _ := newSystemOneEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				io.WriteString(w, body)
			})
			w := do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
			if w.Code != status || w.Body.String() != body || w.Header().Get("Retry-After") != "7" {
				t.Fatalf("error not preserved: %d %s", w.Code, w.Body.String())
			}
			sample := meter.only(t)
			if sample.Tokens.Prompt != 0 || sample.Tokens.Completion != 0 {
				t.Fatal("error billed")
			}
			if strings.Contains(e.logBuf.String(), "private-error-marker") {
				t.Fatal("error body logged")
			}
		})
	}
	e, _, _ := newSystemOneEnv(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached upstream") })
	for _, body := range []string{`{}`, `[]`, `null`, `{"model":1}`, `{"model":"judge","model":"other"}`, `{"model":"judge"} {}`, `{"model":"judge","state":NaN}`} {
		w := do(e.h, "POST", systemOnePath, chatAuth, body)
		if w.Code != 422 || !strings.Contains(w.Body.String(), `"detail"`) {
			t.Errorf("invalid input: %d", w.Code)
		}
	}
	w := do(e.h, "POST", systemOnePath, chatAuth, `{"model":"judge","state":"`+strings.Repeat("x", 2<<20)+`"}`)
	if w.Code != 413 {
		t.Errorf("limit: %d", w.Code)
	}
	w = do(e.h, "POST", systemOnePath, nil, systemOneRequest)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"detail"`) {
		t.Errorf("auth: %d", w.Code)
	}
}

func TestSystemOneDiscoveryAndIsolation(t *testing.T) {
	e, _, model := newSystemOneEnv(t, func(w http.ResponseWriter, r *http.Request) { t.Error("discovery or wrong protocol reached upstream") })
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/ark/api/v3/images/generations"} {
		w := do(e.h, "POST", path, chatAuth, `{"model":"judge","messages":[],"input":"ping","max_tokens":1}`)
		if w.Code != 404 {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	up := dbUpstream(t, e.st, "text", config.UpstreamMock, "fake-text-key", deadURL)
	dbSource(t, e.st, dbModel(t, e.st, "text-only"), up, "", 100)
	w := do(e.h, "POST", systemOnePath, chatAuth, `{"model":"text-only"}`)
	if w.Code != 404 {
		t.Errorf("text routed to System One: %d", w.Code)
	}
	list := func() []map[string]string {
		t.Helper()
		w := do(e.h, "GET", "/typesafe/v1/models", chatAuth, "")
		var got struct {
			Models []map[string]string `json:"models"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
		}
		return got.Models
	}
	if got := list(); len(got) != 1 || got[0]["name"] != "judge" {
		t.Fatalf("models: %+v", got)
	}
	w = do(e.h, "GET", "/v1/models", chatAuth, "")
	if strings.Contains(w.Body.String(), "judge") {
		t.Fatal("System One leaked into OpenAI list")
	}
	restrictAPIModels(t, e.st)
	if got := list(); len(got) != 0 {
		t.Fatalf("restricted discovery: %+v", got)
	}
	w = do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
	if w.Code != 404 {
		t.Errorf("scope bypass: %d", w.Code)
	}
	restrictAPIModels(t, e.st, model)
	if got := list(); len(got) != 1 {
		t.Fatal("allowed model missing")
	}
	if err := e.st.SetModelDisabled(t.Context(), model, true); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatal("disabled model visible")
	}
}

func TestSystemOneMissingAndZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		usage   string
		missing bool
	}{
		{`{"input_tokens":0,"output_tokens":0}`, false},
		{`{"input_tokens":292}`, true},
		{`{"input_tokens":-1,"output_tokens":0}`, true},
		{`{"input_tokens":1.5,"output_tokens":0}`, true},
		{`{"input_tokens":9223372036854775808,"output_tokens":0}`, true},
		{`null`, true},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			e, meter, _ := newSystemOneEnv(t, jsonReply(200, `{"model":"backend","answers":{},"usage":`+tc.usage+`}`))
			w := do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			sample := meter.only(t)
			if sample.UsageUnavailable != tc.missing || sample.Estimated || sample.Tokens.Prompt != 0 {
				t.Fatalf("usage: %+v", sample)
			}
		})
	}
}

func TestSystemOneFailoverAndAdmission(t *testing.T) {
	for _, status := range []int{429, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			e, meter, model := newSystemOneEnv(t, jsonReply(status, `{"detail":"busy"}`))
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != strings.Replace(systemOneRequest, `"judge"`, `"jev-preview"`, 1) {
					t.Error("fallback changed request bytes")
				}
				jsonReply(200, systemOneResponse)(w, r)
			}))
			defer fallback.Close()
			up := dbUpstream(t, e.st, "fallback", config.UpstreamSystemOne, "fake-fallback-key", fallback.URL)
			dbSource(t, e.st, model, up, "jev-preview", 200)
			w := do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
			if w.Code != 200 || w.Body.String() != systemOneResponse {
				t.Fatalf("failover: %d", w.Code)
			}
			if got := meter.only(t); got.Attempts != 2 || got.Tokens.Prompt != 292 {
				t.Fatalf("failover billed incorrectly: %+v", got)
			}
		})
	}
	e, meter, _ := newSystemOneEnv(t, func(w http.ResponseWriter, r *http.Request) { t.Error("rejected request reached upstream") })
	meter.deny = &usage.Decision{Allowed: false, Reason: "rate_limited"}
	w := do(e.h, "POST", systemOnePath, chatAuth, systemOneRequest)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"detail"`) {
		t.Fatalf("admission: %d %s", w.Code, w.Body.String())
	}
	if got := meter.only(t); !got.Rejected || got.Tokens.Prompt != 0 {
		t.Fatalf("rejected billing: %+v", got)
	}
}

func TestSystemOneTunnelAPIProfile(t *testing.T) {
	e := newTunnelEnv(t, tunnelctx.Policy{Hostname: "box.example.com", Profile: tunnelctx.ProfileAPIOnly})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/typesafe/v1/models"
		if method == http.MethodPost {
			path = systemOnePath
		}
		req, _ := http.NewRequest(method, "http://origin"+path, strings.NewReader(systemOneRequest))
		req.Host = "box.example.com"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("CF-Connecting-IP", "203.0.113.9")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.Contains(string(body), `"detail"`) || resp.Header.Get("X-Typesafe-Request-Id") == "" {
			t.Fatalf("API profile must reach Bearer authentication: %d %s", resp.StatusCode, body)
		}
	}
}
