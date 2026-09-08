package gateway_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// Exercise the subscription route through the real meter and the report used by
// the UI. Cache components are input subsets; repeated terminal events must not
// inflate totals or charges, and absent cache-write usage must not be inferred.
func TestCodexUsageDetailsReport(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			writeJSON string
			wantWrite int64
		}{
			{name: "cache write reported", writeJSON: `,"cache_write_tokens":100`, wantWrite: 100},
			{name: "cache write omitted"},
			{name: "cache write zero", writeJSON: `,"cache_write_tokens":0`},
			{name: "invalid cache write", writeJSON: `,"cache_write_tokens":-1`},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":%q,"usage":{"input_tokens":1200,"output_tokens":42,"input_tokens_details":{"cached_tokens":900%s},"output_tokens_details":{"reasoning_tokens":10},"total_tokens":1242}}`, codexBackendModel, tc.writeJSON)
				reply := jsonReply(http.StatusOK, body)
				if stream {
					event := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
					reply = sseReply(event + event)
				}
				e := newAgentEnv(t, reply)
				if err := e.st.SetModelPricing(t.Context(), dbModel(t, e.st, codexModel), meterPricing); err != nil {
					t.Fatal(err)
				}
				m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
				e.srv.EnableMetering(m)
				w := do(e.h, "POST", "/agents/codex/v1/responses", codexAuth, codexReq(stream))
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", w.Code)
				}
				now := time.Now()
				rep, err := m.Report(t.Context(), now.Add(-time.Hour), now.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if len(rep.Recent) != 1 {
					t.Fatalf("recent events = %d, want 1", len(rep.Recent))
				}
				ev := rep.Recent[0]
				if ev.Entry != usage.EntryResponsesAgents || ev.PromptTokens != 1200 || ev.CompletionTokens != 42 || ev.CacheReadTokens != 900 || ev.CacheWriteTokens != tc.wantWrite || ev.TotalTokens != 1242 || ev.Estimated || ev.UsageUnavailable {
					t.Errorf("usage details = %+v, want input=1200 output=42 read=900 write=%d total=1242", ev, tc.wantWrite)
				}
				if rep.Total.Requests != 1 || rep.Total.PromptTokens != 1200 || rep.Total.CompletionTokens != 42 || rep.Total.CacheReadTokens != 900 || rep.Total.CacheWriteTokens != tc.wantWrite || rep.Total.TotalTokens != 1242 {
					t.Errorf("summary = %+v", rep.Total)
				}
				// (1200 - 900) * 3 + 900 * 0.3 + 42 * 12 = 1674 micro-yuan.
				if ev.CostMicro != 1674 || rep.Total.CostMicro != 1674 || m.Spend(testKeyID(t, e.st)).DayMicro != 1674 {
					t.Error("cache-write details changed the subscription's input pricing")
				}
			})
		}
	}
}
