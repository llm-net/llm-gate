package gateway_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func cursorPB(n int, b []byte) []byte {
	p := binary.AppendUvarint(nil, uint64(n<<3|2))
	p = binary.AppendUvarint(p, uint64(len(b)))
	return append(p, b...)
}
func cursorNum(n int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(n<<3)), v)
}
func cursorFrame(flags byte, b []byte) []byte {
	p := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(p[1:], uint32(len(b)))
	return append(p, b...)
}

func TestCursorRunModelUsageBudgetEndToEnd(t *testing.T) {
	for _, model := range []struct{ requested, billed string }{
		{"cursor-test-model", "cursor-test-model"},
		{"default", "cursor-auto"},
		{"auto", "cursor-auto"},
	} {
		for _, bidi := range []bool{false, true} {
			t.Run(model.requested+"/"+map[bool]string{false: "Run", true: "RunSSE"}[bidi], func(t *testing.T) {
				usageBody := cursorPB(1, cursorPB(14, bytes.Join([][]byte{cursorNum(1, 1000), cursorNum(2, 20), cursorNum(3, 700), cursorNum(4, 100), cursorNum(5, 5)}, nil)))
				response := append(cursorFrame(0, usageBody), cursorFrame(2, []byte(`{}`))...)
				run := cursorPB(1, append(cursorPB(3, cursorPB(1, []byte("ignored-fallback"))), cursorPB(9, cursorPB(1, []byte(model.requested)))...))
				var forwarded [][]byte
				srv := cursorUsageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					forwarded = append(forwarded, body)
					if strings.HasSuffix(r.URL.Path, "BidiAppend") {
						w.Header().Set("Content-Type", "application/proto")
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write(response)
				})
				e := newRouteEnv(t)
				e.srv.SetCursorEndpoints(srv.URL)
				connectCursorCredential(t, e, cursorAPIKeyFake)
				m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
				e.srv.EnableMetering(m)
				doc := platformcatalog.Doc{Agents: []platformcatalog.Agent{{Provider: store.AgentProviderCursor, Models: []platformcatalog.AgentModel{{Name: model.billed, Kind: "text", Pricing: json.RawMessage(`{"in":2000000,"out":8000000,"cache_read":200000,"cache_write":2500000}`)}}}}}
				e.srv.SetPlatformModels(fixedPlatformModels{doc: doc})
				// 本地覆盖价与同名 API 模型价不得改变 Cursor 的目录计费。
				if err := e.st.SetSetting(t.Context(), "cursor_model_pricing", `{"`+model.billed+`":{"in":0,"out":0}}`); err != nil {
					t.Fatal(err)
				}
				if _, err := e.st.CreateModel(t.Context(), model.billed, store.ModelKindText, `{"in":1,"out":1}`); err != nil {
					t.Fatal(err)
				}
				h := cursorHeaders()
				h.Set("Authorization", "Bearer "+exchangeCursorClientToken(t, e))
				body := cursorFrame(0, run)
				rpc := usage.CursorAgentRunRPC
				if bidi {
					appendBody := append(cursorPB(2, cursorPB(1, []byte("fake-sensitive-handle"))), cursorPB(4, run)...)
					w := serveClaude(e, "POST", "/agents/cursor/aiserver.v1.BidiService/BidiAppend", h, string(appendBody), false)
					if w.Code != 200 {
						t.Fatal(w.Code)
					}
					if len(forwarded) != 1 || !bytes.Equal(forwarded[0], appendBody) {
						t.Fatal("append bytes changed")
					}
					body = cursorPB(1, []byte("fake-sensitive-handle"))
					rpc = usage.CursorAgentRunSSERPC
				} else {
					h.Set("Content-Type", "application/connect+proto")
				}
				w := serveCursorUsage(e, "/agents/cursor/"+rpc, h, body)
				if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), response) || !bytes.Equal(forwarded[len(forwarded)-1], body) {
					t.Fatal("wire bytes changed")
				}
				r, err := m.Report(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if r.Total.Requests != 1 || r.Total.CostMicro != 950 || r.Total.TotalTokens != 1020 || r.Total.CacheReadTokens != 700 || r.Total.CacheWriteTokens != 100 || r.Total.UnavailableRequests != 0 || r.Total.EstimatedRequests != 0 {
					t.Fatalf("report %+v", r.Total)
				}
				if len(r.ByModel) != 1 || r.ByModel[0].Key != model.billed || r.ByEntry[0].Key != usage.SubscriptionTrafficDimension {
					t.Fatal("dimensions wrong")
				}
				if m.Spend(testKeyID(t, e.st)).DayMicro != 950 {
					t.Fatal("budget not charged")
				}

				if !bidi {
					doc.Agents[0].Models[0].Pricing = json.RawMessage(`{"in":2000000,"out":8000000}`)
					e.srv.SetPlatformModels(fixedPlatformModels{doc: doc})
					if w := serveCursorUsage(e, "/agents/cursor/"+rpc, h, body); w.Code != 200 {
						t.Fatal(w.Code)
					}
					if got := m.Spend(testKeyID(t, e.st)).DayMicro; got != 3110 {
						t.Fatalf("updated catalog spend=%d", got)
					}
					// 撤下型号后，已有本地模型和覆盖设置不能让它继续扣费。
					e.srv.SetPlatformModels(fixedPlatformModels{})
					if w := serveCursorUsage(e, "/agents/cursor/"+rpc, h, body); w.Code != 200 {
						t.Fatal(w.Code)
					}
					if got := m.Spend(testKeyID(t, e.st)).DayMicro; got != 3110 {
						t.Fatalf("unlisted model charged: %d", got)
					}
				}
				for _, secret := range []string{"fake-sensitive-handle", cursorAPIKeyFake, cursorUpstreamTok, testKey} {
					if strings.Contains(e.logBuf.String(), secret) {
						t.Fatal("sensitive value logged")
					}
				}
			})
		}
	}
}

func TestCursorIncompleteUsageAndConnectError(t *testing.T) {
	response := cursorFrame(2, []byte(`{"error":{"code":"internal","message":"fake-private-response"}}`))
	srv := cursorUsageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(response)
	})
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(srv.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	h := cursorHeaders()
	h.Set("Authorization", "Bearer "+exchangeCursorClientToken(t, e))
	w := serveCursorUsage(e, "/agents/cursor/"+usage.CursorAgentRunRPC, h, cursorPB(1, cursorPB(3, cursorPB(1, []byte("cursor-test-model")))))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), response) {
		t.Fatal("error response changed")
	}
	s := fm.only(t)
	if s.Status != 502 || !s.UsageUnavailable || s.Tokens.Total(s.Entry) != 0 {
		t.Fatalf("failed sample %+v", s)
	}
	if strings.Contains(e.logBuf.String(), "fake-private-response") {
		t.Fatal("error text logged")
	}
}

func cursorUsageUpstream(t *testing.T, rpc http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/exchange_user_api_key" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"accessToken":"`+fakeCursorUpstreamToken(1)+`"}`)
			return
		}
		rpc(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type cursorDuplexRecorder struct{ *httptest.ResponseRecorder }

func (*cursorDuplexRecorder) EnableFullDuplex() error { return nil }
func serveCursorUsage(e *routeEnv, path string, h http.Header, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Header = h.Clone()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(&cursorDuplexRecorder{w}, r)
	return w
}
