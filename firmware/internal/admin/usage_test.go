// usage_test.go 钉住用量读数端点的契约：区间关键词、五个维度分解、计量未装配
// 时的 503，以及照 endpoints_test.go 先例的那条红线——响应里没有密钥明文，
// 也没有任何内容字段。
package admin_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	usageUpstream = "ark-plan-vendor-acct"
	usageModel    = "deepseek-v4-pro"
)

// usageDTO 是端点的响应形（只解本用例要断言的字段）。
type usageDTO struct {
	Range string `json:"range"`
	KeyID int64  `json:"key_id"`
	Total struct {
		CostMicro         int64 `json:"cost_micro"`
		Requests          int64 `json:"requests"`
		RejectedRequests  int64 `json:"rejected_requests"`
		EstimatedRequests int64 `json:"estimated_requests"`
		PromptTokens      int64 `json:"prompt_tokens"`
	} `json:"total"`
	ByKey      []dimDTO `json:"by_key"`
	ByModel    []dimDTO `json:"by_model"`
	ByUpstream []dimDTO `json:"by_upstream"`
	ByEntry    []dimDTO `json:"by_entry"`
	ByKind     []dimDTO `json:"by_kind"`
	Keys       []struct {
		ID      int64  `json:"id"`
		Display string `json:"display"`
		Label   string `json:"label"`
	} `json:"keys"`
	Days []struct {
		Day       string `json:"day"`
		CostMicro int64  `json:"cost_micro"`
	} `json:"days"`
	Recent []struct {
		ModelName string `json:"model_name"`
		Upstream  string `json:"upstream_name"`
		CostMicro int64  `json:"cost_micro"`
		TaskID    string `json:"task_id"`
	} `json:"recent"`
}

type dimDTO struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Label     string `json:"label"`
	CostMicro int64  `json:"cost_micro"`
}

// spend 记一笔文本消费到计量器上（与数据面请求收尾时那一笔同形）。
// 目录价：输入 2 元 / 输出 8 元每百万 token。
func (e *env) spend(keyID int64, keyDisplay string, prompt, completion int64) {
	e.t.Helper()
	e.meter.Record(usage.Sample{
		At:    time.Now(),
		KeyID: keyID, KeyDisplay: keyDisplay,
		ModelName: usageModel, ModelKnown: true, UpstreamName: usageUpstream,
		Entry: usage.EntryChat, Kind: store.ModelKindText,
		Pricing: `{"in":2000000,"out":8000000}`,
		Tokens:  usage.Tokens{Prompt: prompt, Completion: completion},
		Status:  200, Attempts: 1, DurationMs: 120,
	})
}

// usageReport 取一次读数（期望 200），同时把原始 JSON 交出来供 grep 红线。
func (e *env) usageReport(path, cookie string) (usageDTO, string) {
	e.t.Helper()
	resp := e.do("GET", path, cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	raw := readAll(e.t, resp)
	var out usageDTO
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		e.t.Fatalf("解析用量响应 %q: %v", raw, err)
	}
	return out, raw
}

// TestUsageReport：设备只有一本账——五个维度分解 + 按日序列 + 明细环一次取齐；
// 无会话 401。
func TestUsageReport(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	key, _ := e.createKey(root, "audit-key")
	display := key.DisplayPrefix + "…" + key.DisplayLast4

	// 1 元输入 + 4 元输出 = 500000 + 4000000 = 4500000 微元。
	e.spend(key.ID, display, 250_000, 500_000)

	got, raw := e.usageReport("/admin/v1/usage", root)
	const want = 4_500_000
	if got.Range != "today" {
		t.Errorf("range = %q，期望缺省 today", got.Range)
	}
	if got.Total.CostMicro != want || got.Total.Requests != 1 {
		t.Errorf("合计 = %+v，期望 cost=%d requests=1", got.Total, want)
	}
	if len(got.ByModel) != 1 || got.ByModel[0].Key != usageModel {
		t.Errorf("按模型分解 = %+v，期望只有 %s", got.ByModel, usageModel)
	}
	if len(got.ByKey) != 1 || got.ByKey[0].ID != key.ID || got.ByKey[0].Key != display {
		t.Errorf("按密钥分解 = %+v，期望那一把", got.ByKey)
	}
	if got.ByKey[0].Label != "audit-key" {
		t.Errorf("按密钥分解标签 = %q，期望 audit-key", got.ByKey[0].Label)
	}
	if len(got.Keys) != 1 || got.Keys[0].ID != key.ID || got.Keys[0].Display != display || got.Keys[0].Label != "audit-key" {
		t.Errorf("密钥选择项 = %+v，期望带标签与脱敏展示串", got.Keys)
	}
	if len(got.ByUpstream) != 1 || got.ByUpstream[0].Key != usageUpstream {
		t.Errorf("按上游分解 = %+v，期望 %s 一行", got.ByUpstream, usageUpstream)
	}
	if len(got.ByEntry) != 1 || len(got.ByKind) != 1 {
		t.Errorf("按入口/种类分解 = %+v / %+v，各期望一行", got.ByEntry, got.ByKind)
	}
	if len(got.Days) != 1 || got.Days[0].CostMicro != want {
		t.Errorf("按日序列 = %+v，期望今天一格 %d 微元", got.Days, want)
	}
	if len(got.Recent) != 1 || got.Recent[0].ModelName != usageModel ||
		got.Recent[0].Upstream != usageUpstream {
		t.Errorf("最近请求 = %+v", got.Recent)
	}
	if !strings.Contains(raw, usageUpstream) { // 反向锚：红线用例的 grep 才有意义
		t.Error("响应里居然没有上游账户名，红线断言失去意义")
	}

	wantStatus(t, e.do("GET", "/admin/v1/usage", "", ""), http.StatusUnauthorized)
}

// TestUsageReportForKey：key_id 把总计、各维度、趋势与最近请求同时收窄到
// 单把密钥，但 keys 仍列出全部当前密钥，页面可以直接切换统计对象。
func TestUsageReportForKey(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	// 闲置 Key 故意先创建：合并选择项时它会先 append，使切片扩容；后两把
	// 有用量 Key 的标签必须仍写回最终切片，而不是写进扩容前的旧 backing array。
	idle, _ := e.createKey(root, "暂未使用")
	first, _ := e.createKey(root, "研发组")
	second, _ := e.createKey(root, "自动化")

	e.spend(first.ID, first.DisplayPrefix+"…"+first.DisplayLast4, 250_000, 500_000)
	e.spend(second.ID, second.DisplayPrefix+"…"+second.DisplayLast4, 100_000, 200_000)

	got, _ := e.usageReport("/admin/v1/usage?range=today&key_id="+strconv.FormatInt(first.ID, 10), root)
	if got.KeyID != first.ID {
		t.Errorf("key_id 回显 = %d，期望 %d", got.KeyID, first.ID)
	}
	if got.Total.Requests != 1 || got.Total.CostMicro != 4_500_000 {
		t.Errorf("单 Key 合计 = %+v", got.Total)
	}
	if len(got.ByKey) != 1 || got.ByKey[0].ID != first.ID || got.ByKey[0].Label != "研发组" {
		t.Errorf("单 Key 分解 = %+v", got.ByKey)
	}
	if len(got.Recent) != 1 || got.Recent[0].CostMicro != 4_500_000 {
		t.Errorf("单 Key 最近请求 = %+v", got.Recent)
	}
	if len(got.Keys) != 3 {
		t.Fatalf("统计对象选择项 = %+v，期望包含本区间没有用量的当前密钥", got.Keys)
	}
	labels := map[int64]string{}
	for _, key := range got.Keys {
		labels[key.ID] = key.Label
	}
	if labels[first.ID] != "研发组" || labels[second.ID] != "自动化" || labels[idle.ID] != "暂未使用" {
		t.Errorf("统计对象标签 = %+v", labels)
	}
}

// TestUsageRedLines：响应里不能有密钥明文，也不能有任何内容字段。
// 照 endpoints_test.go 的先例：直接 grep 整个响应体，别只查自己记得的字段。
func TestUsageRedLines(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	key, plaintext := e.createKey(root, "red-line-key")
	e.spend(key.ID, key.DisplayPrefix+"…"+key.DisplayLast4, 250_000, 500_000)

	_, raw := e.usageReport("/admin/v1/usage", root)
	if strings.Contains(raw, plaintext) {
		t.Error("用量响应里出现了密钥明文")
	}
	// 内容字段（提示词/模型响应）压根没有进过 internal/usage 的结构体，
	// 这里连它们的字段名都不该出现。
	for _, field := range []string{"\"messages\"", "\"content\"", "\"prompt\":", "\"api_key"} {
		if strings.Contains(raw, field) {
			t.Errorf("用量响应里出现了内容/凭据字段 %s:\n%s", field, raw)
		}
	}
}

// TestUsageRangeParam：认识的关键词各自换一个窗口，不认识的一律 400——
// 静默退回缺省区间会让人以为屏幕上的数字是自己要的那一段。
func TestUsageRangeParam(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	for _, name := range []string{"today", "month", "7d", "30d"} {
		got, _ := e.usageReport("/admin/v1/usage?range="+name, root)
		if got.Range != name {
			t.Errorf("range=%s 回显 %q", name, got.Range)
		}
	}
	for _, bad := range []string{"yesterday", "1h", "90d", "TODAY"} {
		resp := e.do("GET", "/admin/v1/usage?range="+bad, root, "")
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "invalid_range" {
			t.Errorf("range=%s 的 error.code = %q，期望 invalid_range", bad, got)
		}
	}
}

// TestUsageKeyParam：非正整数与非数字都拒绝，不能把错误参数静默当设备总览。
func TestUsageKeyParam(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	for _, bad := range []string{"0", "-1", "abc", "1.5"} {
		resp := e.do("GET", "/admin/v1/usage?key_id="+bad, root, "")
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "invalid_key_id" {
			t.Errorf("key_id=%s 的 error.code = %q，期望 invalid_key_id", bad, got)
		}
	}
}

// TestUsageWithoutMeter：没接计量器时用量端点答 503——**宁可说实话，也不编一份
// 空报表**（密钥列表对同一件事的说法是整块 spend 缺席，见 TestKeysListWithoutMeter）。
func TestUsageWithoutMeter(t *testing.T) {
	e := newEnvNoMeter(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/usage", root, "")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if got := errCode(t, resp); got != "usage_unavailable" {
		t.Errorf("error.code = %q，期望 usage_unavailable", got)
	}
}

// TestKeysListSpendWindows：密钥列表把「已消费额」与预算摆在同一行——那是
// 「API密钥」页每行那格仪表的数据来源（预算是刻度，消费是指针，缺一格读不出
// 松紧）。读数恒为**当前**自然日/周/月（列表没有区间参数，也不该有）；
// 没花过钱的密钥是实实在在的 0。
func TestKeysListSpendWindows(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	spent, _ := e.createKey(root, "spent-key")
	idle, _ := e.createKey(root, "idle-key")

	const want = 4_500_000
	e.spend(spent.ID, spent.DisplayPrefix+"…"+spent.DisplayLast4, 250_000, 500_000)

	resp := e.do("GET", "/admin/v1/keys", root, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	decodeInto(t, resp, &list)
	byID := map[int64]keyDTO{}
	for _, k := range list.Keys {
		byID[k.ID] = k
	}
	got, ok := byID[spent.ID]
	if !ok || got.Spend == nil {
		t.Fatalf("花过钱的密钥没有 spend: %+v", got)
	}
	// 三个窗口都含「现在」，因此都等于这一笔。
	if got.Spend.DayMicro != want || got.Spend.WeekMicro != want || got.Spend.MonthMicro != want {
		t.Errorf("已消费额 = %+v，期望三个窗口都是 %d", *got.Spend, want)
	}
	// 一分钱没花的密钥：字段在、值是 0（「没花钱」与「没计量」是两回事）。
	if k := byID[idle.ID]; k.Spend == nil || k.Spend.DayMicro != 0 {
		t.Errorf("闲置密钥的已消费额 = %+v，期望字段在且为 0", k.Spend)
	}
}
