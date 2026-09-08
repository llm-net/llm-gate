package usage

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestSubscriptionReportGroupsStoredAndPendingTraffic(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)
	bucket := at.Unix() / 3600
	entries := []string{EntryResponsesAgents, EntryClaudeCode, EntryCursorAgent, EntryImagineImage, EntryImagineVideo, EntryChat, EntryResponses, EntryMessages, EntryImage, EntryVideo}
	var stored []store.UsageDelta
	for _, entry := range entries {
		model := "test-model"
		if entry == EntryCursorAgent {
			model = CursorAgentRunSSERPC
		}
		stored = append(stored, store.UsageDelta{
			BucketHour: bucket, KeyID: 7, ModelName: model,
			Entry: entry, Kind: EntryKind(entry), Requests: 2, DurationMsSum: 40,
		})
	}
	stored[0].CostMicro, stored[0].PromptTokens, stored[0].CompletionTokens, stored[0].TotalTokens = 123, 10, 2, 12
	stored[1].CostMicro, stored[1].CacheReadTokens, stored[1].CacheWriteTokens, stored[1].TotalTokens = 456, 3, 4, 7
	stored[1].Errors, stored[1].RejectedRequests, stored[1].EstimatedRequests = 1, 1, 1
	stored[3].ImageCount = 2
	stored[4].VideoSeconds = 6
	// 辅助 RPC 不算订阅模型消费；另一把 Key 只进入全量视图。
	stored = append(stored,
		store.UsageDelta{BucketHour: bucket, KeyID: 7, Entry: EntryCursorAgent, ModelName: "aiserver.v1.AiService/AvailableModels", Requests: 99},
		store.UsageDelta{BucketHour: bucket, KeyID: 9, Entry: EntryClaudeCode, ModelName: "test-model", Requests: 3, CostMicro: 789},
	)
	if err := st.AddUsageDeltas(ctx, stored); err != nil {
		t.Fatal(err)
	}
	before, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatal(err)
	}
	// 新请求与历史行经过同一汇总口径；Claude 的缓存仍按 Anthropic 语义计量。
	m.Record(Sample{
		At: at, KeyID: 7, Entry: EntryClaudeCode, ModelName: "test-model",
		Tokens: Tokens{Prompt: 10, Completion: 2, CacheRead: 3, CacheWrite: 4},
		Status: 200, Attempts: 1, DurationMs: 25,
	})
	m.Record(Sample{
		At: at, KeyID: 7, Entry: EntryCursorAgent, ModelName: CursorAgentModelDimension,
		Status: 200, Attempts: 1, DurationMs: 30,
	})
	want := Summary{
		Requests: 12, CostMicro: 579, Errors: 1, RejectedRequests: 1, EstimatedRequests: 1,
		PromptTokens: 20, CompletionTokens: 4, CacheReadTokens: 6, CacheWriteTokens: 8, TotalTokens: 38,
		ImageCount: 2, VideoSeconds: 6, DurationMsSum: 255,
	}
	for _, phase := range []string{"pending", "flushed"} {
		if phase == "flushed" {
			m.flush(ctx)
		}
		rep, err := m.ReportForKey(ctx, at.Add(-time.Hour), at.Add(time.Hour), 7)
		if err != nil {
			t.Fatal(err)
		}
		byEntry := make(map[string]Summary)
		for _, row := range rep.ByEntry {
			if _, duplicate := byEntry[row.Key]; duplicate {
				t.Fatalf("%s: duplicate entry %q", phase, row.Key)
			}
			byEntry[row.Key] = row.Summary
		}
		if len(byEntry) != 6 || byEntry[SubscriptionTrafficDimension] != want {
			t.Fatalf("%s: by_entry = %+v, want one subscription row %+v and five API rows", phase, byEntry, want)
		}
		for _, entry := range []string{EntryChat, EntryResponses, EntryMessages, EntryImage, EntryVideo} {
			if got := byEntry[entry]; got.Requests != 2 || got.DurationMsSum != 40 {
				t.Errorf("%s: API entry %q = %+v", phase, entry, got)
			}
		}
		if rep.Total.Requests != 22 || rep.Total.CostMicro != 579 || rep.Total.TotalTokens != 38 {
			t.Errorf("%s: total = %+v", phase, rep.Total)
		}
		if len(rep.Recent) != 2 || rep.Recent[0].Entry != EntryCursorAgent || rep.Recent[1].Entry != EntryClaudeCode || rep.Recent[1].TotalTokens != 19 {
			t.Errorf("%s: raw recent entries or Claude token semantics changed: %+v", phase, rep.Recent)
		}
		if phase == "pending" {
			after, err := st.QueryUsageRange(ctx, bucket, bucket+1)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("report changed stored ledger: err=%v", err)
			}
		}
	}
	all, err := m.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want.Requests += 3
	want.CostMicro += 789
	if len(all.ByEntry) != 6 || all.ByEntry[0].Key != SubscriptionTrafficDimension || all.ByEntry[0].Summary != want || all.Total.Requests != 25 {
		t.Errorf("all keys: by_entry=%+v total=%+v", all.ByEntry, all.Total)
	}
	rows, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		seen[row.Entry] = true
	}
	if seen[SubscriptionTrafficDimension] {
		t.Error("report dimension must not be stored as a raw entry")
	}
	for _, entry := range entries {
		if !seen[entry] {
			t.Errorf("raw entry %q missing from ledger", entry)
		}
	}
}
