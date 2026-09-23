package usage

import (
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestSystemOneMeterPersistenceAndBudget(t *testing.T) {
	at := time.Now()
	meter, st := newTestMeter(t, time.UTC, at)
	sample := textSample(at)
	sample.Entry = EntrySystemOne
	sample.Kind = store.ModelKindSystemOne
	sample.ModelName = "judge"
	sample.UpstreamType = "systemone"
	sample.Pricing = `{"in":1000000,"out":9000000}`
	sample.Tokens = Tokens{Prompt: 292, Completion: 0}
	meter.Record(sample)
	sample.UsageUnavailable = true
	sample.Tokens = Tokens{}
	meter.Record(sample)
	if got := meter.Spend(sample.KeyID); got.DayMicro != 292 {
		t.Fatalf("budget: %+v", got)
	}
	meter.flush(t.Context())
	rows, err := st.QueryUsageRange(t.Context(), at.Unix()/3600, at.Unix()/3600+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	row := rows[0]
	if row.Entry != EntrySystemOne || row.Kind != store.ModelKindSystemOne || row.PromptTokens != 292 || row.CompletionTokens != 0 || row.TotalTokens != 292 || row.CostMicro != 292 || row.Requests != 2 {
		t.Fatalf("persisted: %+v", row)
	}
}
