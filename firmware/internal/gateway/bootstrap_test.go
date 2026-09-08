// bootstrap_test.go 是 iteration-5 Phase 2/3 的可执行验收：YAML 上游/模型两段
// 的 seed 闸门（决策 6b：只在两表皆空时导入）、底层导入原语的幂等与转换口径，
// 以及 §15.1 的导入日志纪律（只记条数与名称，绝不记 Key 物料）。风格同
// server_test.go 的 TestImportConfigKeysIdempotent：直接对临时 SQLite 跑导入器。
package gateway_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 导入用例里的上游凭证：断言它绝不出现在日志与库文件明文里。
const (
	importKeyDS  = "sk-import-ds-secret-0123456789"
	importKeyArk = "sk-import-ark-secret-abcdefghij"
)

// newImportEnv 打开临时库并返回 store、日志缓冲、数据目录。
func newImportEnv(t *testing.T) (*store.Store, *bytes.Buffer, *slog.Logger, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	var logBuf bytes.Buffer
	return st, &logBuf, logging.New(&logBuf, slog.LevelDebug), dir
}

func yamlUpstreams() []config.Upstream {
	return []config.Upstream{
		{Name: "ds", Type: config.UpstreamDeepseek, APIKey: importKeyDS},
		{Name: "ark-plan", Type: config.UpstreamArkPlan, APIKey: importKeyArk},
	}
}

// TestImportConfigUpstreamsIdempotent：按 name 幂等——重复导入不重建行、
// 已存在的条目整条跳过（YAML 改 Key 不覆盖库内凭证）、停用状态不被复活。
func TestImportConfigUpstreamsIdempotent(t *testing.T) {
	st, _, logger, _ := newImportEnv(t)
	ups := yamlUpstreams()

	imported, skipped, err := gateway.ImportConfigUpstreams(t.Context(), st, ups, logger)
	if err != nil {
		t.Fatalf("首次导入: %v", err)
	}
	if imported != 2 || skipped != 0 {
		t.Errorf("首次导入 imported/skipped = %d/%d，期望 2/0", imported, skipped)
	}
	rows, err := st.ListUpstreams(t.Context())
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("导入后上游行数 = %d，期望 2", len(rows))
	}
	if rows[0].Name != "ds" || rows[0].Type != config.UpstreamDeepseek {
		t.Errorf("首行 = %+v，期望 name=ds type=deepseek", rows[0])
	}
	// 凭证入库即密文：管理视图只给末 4 位。
	if rows[0].APIKeyLast4 != importKeyDS[len(importKeyDS)-4:] {
		t.Errorf("api_key_last4 = %q，期望 %q", rows[0].APIKeyLast4, importKeyDS[len(importKeyDS)-4:])
	}

	// 重复导入（模拟重复启动）：整条跳过，不重复建行。
	imported, skipped, err = gateway.ImportConfigUpstreams(t.Context(), st, ups, logger)
	if err != nil {
		t.Fatalf("重复导入: %v", err)
	}
	if imported != 0 || skipped != 2 {
		t.Errorf("重复导入 imported/skipped = %d/%d，期望 0/2", imported, skipped)
	}

	// YAML 改 Key / 改 base_url / 改 disabled 都不覆盖库内行——只经管理台。
	if err := st.SetUpstreamDisabled(t.Context(), rows[0].ID, true); err != nil {
		t.Fatalf("SetUpstreamDisabled: %v", err)
	}
	mutated := yamlUpstreams()
	mutated[0].APIKey = "sk-import-ds-rotated-in-yaml"
	mutated[0].BaseURL = "http://127.0.0.1:9/v1"
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st, mutated, logger); err != nil {
		t.Fatalf("改动后导入: %v", err)
	}
	after, err := st.ListUpstreams(t.Context())
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("改动后上游行数 = %d，期望仍为 2", len(after))
	}
	if !after[0].Disabled {
		t.Error("已停用的上游被导入复活")
	}
	if after[0].APIKeyLast4 != importKeyDS[len(importKeyDS)-4:] {
		t.Errorf("库内凭证被 YAML 覆盖：last4 = %q", after[0].APIKeyLast4)
	}
	if after[0].BaseURL != "" {
		t.Errorf("库内 base_url 被 YAML 覆盖：%q", after[0].BaseURL)
	}
}

// TestImportConfigUpstreamsOptional：整段缺省（nil / 空切片）可启动，不建行、
// 不记日志。
func TestImportConfigUpstreamsOptional(t *testing.T) {
	st, logBuf, logger, _ := newImportEnv(t)
	for _, ups := range [][]config.Upstream{nil, {}} {
		imported, skipped, err := gateway.ImportConfigUpstreams(t.Context(), st, ups, logger)
		if err != nil || imported != 0 || skipped != 0 {
			t.Fatalf("空 upstreams 导入 = (%d, %d, %v)，期望 (0, 0, nil)", imported, skipped, err)
		}
	}
	rows, err := st.ListUpstreams(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("空导入后上游行数 = %d（err=%v），期望 0", len(rows), err)
	}
	if logBuf.Len() != 0 {
		t.Errorf("空导入不应写日志:\n%s", logBuf.String())
	}
}

// TestImportConfigModelsConversion：logical_models 自动转换——模型名取
// upstream_model_id（逻辑名丢弃）、同名跨上游合并为多来源、优先级按段内
// 出现序 100/200/300、protocol 忽略、来源侧 ID 留空（= 与模型名相同）。
func TestImportConfigModelsConversion(t *testing.T) {
	st, _, logger, _ := newImportEnv(t)
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st, yamlUpstreams(), logger); err != nil {
		t.Fatalf("导入上游: %v", err)
	}

	// 逻辑名字典序：a-ds < b-ark < c-ds-anthropic < d-only-ark。
	// deepseek-chat 由 3 条 binding 合并：ds（首次出现，priority 100）、
	// ark-plan（第二个上游，priority 200）、以及与 ds 重复的 anthropic 条目（合并掉）。
	bindings := map[string]config.Binding{
		"a-ds":           {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"b-ark":          {Upstream: "ark-plan", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"c-ds-anthropic": {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolAnthropicMessages},
		"d-only-ark":     {Upstream: "ark-plan", UpstreamModelID: "deepseek-v4-flash", Protocol: config.ProtocolAnthropicMessages},
	}
	models, sources, skipped, err := gateway.ImportConfigModels(t.Context(), st, bindings, logger)
	if err != nil {
		t.Fatalf("导入模型: %v", err)
	}
	if models != 2 || sources != 3 || skipped != 1 {
		t.Errorf("导入计数 models/sources/skipped = %d/%d/%d，期望 2/3/1", models, sources, skipped)
	}
	if sources+skipped != len(bindings) {
		t.Errorf("不变式 sources+skipped(%d) != len(bindings)(%d)", sources+skipped, len(bindings))
	}

	rows, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("模型行数 = %d，期望 2（原始名，逻辑名不建行）", len(rows))
	}
	// 字典序：deepseek-chat < deepseek-v4-flash。
	if rows[0].Name != "deepseek-chat" || rows[1].Name != "deepseek-v4-flash" {
		t.Fatalf("模型名 = %q / %q，期望 deepseek-chat / deepseek-v4-flash", rows[0].Name, rows[1].Name)
	}
	if len(rows[0].Sources) != 2 {
		t.Fatalf("deepseek-chat 来源数 = %d，期望 2（同名跨上游合并）", len(rows[0].Sources))
	}
	want := []struct {
		upstream string
		priority int64
	}{{"ds", 100}, {"ark-plan", 200}}
	for i, w := range want {
		got := rows[0].Sources[i]
		if got.UpstreamName != w.upstream || got.Priority != w.priority {
			t.Errorf("来源[%d] = (%s, %d)，期望 (%s, %d)", i, got.UpstreamName, got.Priority, w.upstream, w.priority)
		}
		if got.UpstreamModelID != "" {
			t.Errorf("来源[%d] upstream_model_id = %q，期望空（= 与模型名相同）", i, got.UpstreamModelID)
		}
		if got.Disabled {
			t.Errorf("来源[%d] 不应停用", i)
		}
	}
	if len(rows[1].Sources) != 1 || rows[1].Sources[0].Priority != 100 {
		t.Errorf("deepseek-v4-flash 来源 = %+v，期望单来源 priority 100", rows[1].Sources)
	}

	// 转换结果对数据面即时可用：路由联查取得同样的有序候选。
	route, err := st.ResolveModelRoute(t.Context(), "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(route.Candidates))
	}
	// 来源侧 ID 空值由路由视图解析为模型名；凭证解出的是导入时的明文。
	if route.Candidates[0].UpstreamModelID != "deepseek-chat" {
		t.Errorf("候选[0] upstream_model_id = %q，期望解析为模型名", route.Candidates[0].UpstreamModelID)
	}
	if route.Candidates[0].Upstream.APIKey != importKeyDS {
		t.Error("候选[0] 的上游凭证与导入的明文不一致")
	}
}

// TestImportConfigModelsIdempotent：重复导入不重建行；已存在的 (模型, 上游)
// 整条跳过——停用的来源不被复活、手工改过的优先级与来源侧 ID 不被覆盖。
func TestImportConfigModelsIdempotent(t *testing.T) {
	st, _, logger, _ := newImportEnv(t)
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st, yamlUpstreams(), logger); err != nil {
		t.Fatalf("导入上游: %v", err)
	}
	bindings := map[string]config.Binding{
		"ds-chat": {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"cc-ds":   {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolAnthropicMessages},
		"ark-fla": {Upstream: "ark-plan", UpstreamModelID: "deepseek-v4-flash", Protocol: config.ProtocolOpenAIChat},
	}
	models, sources, skipped, err := gateway.ImportConfigModels(t.Context(), st, bindings, logger)
	if err != nil {
		t.Fatalf("首次导入: %v", err)
	}
	// 板上现状：ds-chat + cc-ds 合并为 deepseek-chat 单来源，ark 段单来源。
	if models != 2 || sources != 2 || skipped != 1 {
		t.Errorf("首次导入 models/sources/skipped = %d/%d/%d，期望 2/2/1", models, sources, skipped)
	}

	// 管理台侧的手工调整：改优先级 + 改来源侧 ID + 停用。
	rows, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	src := rows[0].Sources[0]
	if err := st.UpdateModelSource(t.Context(), src.ID, "deepseek-chat-20260801", 42); err != nil {
		t.Fatalf("UpdateModelSource: %v", err)
	}
	if err := st.SetModelSourceDisabled(t.Context(), src.ID, true); err != nil {
		t.Fatalf("SetModelSourceDisabled: %v", err)
	}
	if err := st.SetModelDisabled(t.Context(), rows[0].ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}

	models, sources, skipped, err = gateway.ImportConfigModels(t.Context(), st, bindings, logger)
	if err != nil {
		t.Fatalf("重复导入: %v", err)
	}
	if models != 0 || sources != 0 || skipped != 3 {
		t.Errorf("重复导入 models/sources/skipped = %d/%d/%d，期望 0/0/3", models, sources, skipped)
	}
	after, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("重复导入后模型行数 = %d，期望仍为 2", len(after))
	}
	if !after[0].Disabled {
		t.Error("已停用的模型被导入复活")
	}
	if len(after[0].Sources) != 1 {
		t.Fatalf("重复导入后来源数 = %d，期望仍为 1", len(after[0].Sources))
	}
	got := after[0].Sources[0]
	if !got.Disabled {
		t.Error("已停用的来源被导入复活")
	}
	if got.Priority != 42 || got.UpstreamModelID != "deepseek-chat-20260801" {
		t.Errorf("手工调整被 YAML 覆盖：priority=%d upstream_model_id=%q", got.Priority, got.UpstreamModelID)
	}
}

// TestImportConfigModelsPriorityAppends：往已导入的模型追加来源时，优先级接在
// 现有来源之后，绝不与既有值相同、也绝不插到管理台排好的顺序前面——哪怕新条目
// 的逻辑名排在旧条目前面（导入序取逻辑名字典序）。
func TestImportConfigModelsPriorityAppends(t *testing.T) {
	st, _, logger, _ := newImportEnv(t)
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st, yamlUpstreams(), logger); err != nil {
		t.Fatalf("导入上游: %v", err)
	}
	// 第一代 YAML：只有 z-logical（挂 ds），得 priority 100。
	first := map[string]config.Binding{
		"z-logical": {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
	}
	if _, sources, _, err := gateway.ImportConfigModels(t.Context(), st, first, logger); err != nil || sources != 1 {
		t.Fatalf("首次导入 sources = %d, err = %v，期望 1/nil", sources, err)
	}

	// 第二代 YAML：加一条 a-logical（挂 ark-plan，同一模型），逻辑名排在 z 之前。
	second := map[string]config.Binding{
		"a-logical": {Upstream: "ark-plan", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"z-logical": first["z-logical"],
	}
	models, sources, skipped, err := gateway.ImportConfigModels(t.Context(), st, second, logger)
	if err != nil {
		t.Fatalf("追加导入: %v", err)
	}
	if models != 0 || sources != 1 || skipped != 1 {
		t.Errorf("追加导入 models/sources/skipped = %d/%d/%d，期望 0/1/1", models, sources, skipped)
	}
	rows, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Sources) != 2 {
		t.Fatalf("模型/来源数 = %d/%v，期望 1 个模型 2 条来源", len(rows), rows)
	}
	got := rows[0].Sources
	if got[0].UpstreamName != "ds" || got[0].Priority != 100 {
		t.Errorf("既有来源被改动：%s @%d，期望 ds @100", got[0].UpstreamName, got[0].Priority)
	}
	if got[1].UpstreamName != "ark-plan" || got[1].Priority != 200 {
		t.Errorf("追加来源 = %s @%d，期望 ark-plan @200（接在现有来源之后、不与之相同）",
			got[1].UpstreamName, got[1].Priority)
	}

	// 管理台把既有来源调到 300 之后再追加：新来源仍排在它之后（400），
	// 不会因为 YAML 的段内序抢到前面。
	if err := st.UpdateModelSource(t.Context(), got[0].ID, "", 300); err != nil {
		t.Fatalf("UpdateModelSource: %v", err)
	}
	if err := st.DeleteModelSource(t.Context(), got[1].ID); err != nil {
		t.Fatalf("DeleteModelSource: %v", err)
	}
	if _, sources, _, err := gateway.ImportConfigModels(t.Context(), st, second, logger); err != nil || sources != 1 {
		t.Fatalf("再次追加 sources = %d, err = %v，期望 1/nil", sources, err)
	}
	rows, err = st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(rows[0].Sources) != 2 || rows[0].Sources[1].Priority != 400 {
		t.Errorf("再次追加后来源 = %+v，期望第二条 priority 400", rows[0].Sources)
	}
}

// TestImportConfigModelsOptional：整段缺省/为空可启动，不建行、不记日志；
// 引用不上库内上游的条目跳过该条而不是让启动失败（防御分支：经 config.Load
// 的配置走不到——校验保证引用存在、上游又先导入，这里直接构造该状态）。
func TestImportConfigModelsOptional(t *testing.T) {
	st, logBuf, logger, _ := newImportEnv(t)
	for _, bindings := range []map[string]config.Binding{nil, {}} {
		models, sources, skipped, err := gateway.ImportConfigModels(t.Context(), st, bindings, logger)
		if err != nil || models != 0 || sources != 0 || skipped != 0 {
			t.Fatalf("空 logical_models 导入 = (%d, %d, %d, %v)，期望全 0/nil", models, sources, skipped, err)
		}
	}
	if logBuf.Len() != 0 {
		t.Errorf("空导入不应写日志:\n%s", logBuf.String())
	}

	// 上游不在库：跳过该条，其余条目照常导入。
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st,
		[]config.Upstream{{Name: "ds", Type: config.UpstreamDeepseek, APIKey: importKeyDS}}, logger); err != nil {
		t.Fatalf("导入上游: %v", err)
	}
	models, sources, skipped, err := gateway.ImportConfigModels(t.Context(), st, map[string]config.Binding{
		"live":  {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"stale": {Upstream: "deleted-in-admin", UpstreamModelID: "ghost-model", Protocol: config.ProtocolOpenAIChat},
	}, logger)
	if err != nil {
		t.Fatalf("含失效引用的导入不应失败: %v", err)
	}
	if models != 1 || sources != 1 || skipped != 1 {
		t.Errorf("models/sources/skipped = %d/%d/%d，期望 1/1/1", models, sources, skipped)
	}
	rows, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "deepseek-chat" {
		t.Errorf("失效引用不应建模型行: %+v", rows)
	}
}

// smokeBindings 是 seed 用例的模型段：两条 binding 合并出两个原始名模型。
func smokeBindings() map[string]config.Binding {
	return map[string]config.Binding{
		"ds-chat": {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
		"ark-fla": {Upstream: "ark-plan", UpstreamModelID: "deepseek-v4-flash", Protocol: config.ProtocolOpenAIChat},
	}
}

// TestImportConfigCatalogSeedsEmptyDB（决策 6b）：两表皆空的首启照常导入，
// 且重跑同一份 YAML 不会重复建行（第二次整体跳过）。
func TestImportConfigCatalogSeedsEmptyDB(t *testing.T) {
	st, logBuf, logger, _ := newImportEnv(t)
	if err := gateway.ImportConfigCatalog(t.Context(), st, yamlUpstreams(), smokeBindings(), logger); err != nil {
		t.Fatalf("空库 seed: %v", err)
	}
	ups, err := st.ListUpstreams(t.Context())
	if err != nil || len(ups) != 2 {
		t.Fatalf("seed 后上游行数 = %d（err=%v），期望 2", len(ups), err)
	}
	models, err := st.ListModelsWithSources(t.Context())
	if err != nil || len(models) != 2 {
		t.Fatalf("seed 后模型行数 = %d（err=%v），期望 2", len(models), err)
	}
	if strings.Contains(logBuf.String(), "bootstrap_skipped") {
		t.Errorf("空库 seed 不应记跳过日志:\n%s", logBuf.String())
	}

	// 第二次启动：库已非空 → 整体跳过（不再逐条查重）。
	logBuf.Reset()
	if err := gateway.ImportConfigCatalog(t.Context(), st, yamlUpstreams(), smokeBindings(), logger); err != nil {
		t.Fatalf("二次 seed: %v", err)
	}
	if !strings.Contains(logBuf.String(), `"reason":"bootstrap_skipped"`) {
		t.Errorf("二次启动应记一条 bootstrap_skipped:\n%s", logBuf.String())
	}
	after, err := st.ListUpstreams(t.Context())
	if err != nil || len(after) != 2 {
		t.Errorf("二次启动后上游行数 = %d，期望仍为 2", len(after))
	}
}

// TestImportConfigCatalogSkipsNonEmptyDB（决策 6b）：库内已有上游或模型时整段
// 跳过——管理台删掉的行不被 YAML 复活，YAML 新增的条目也不再生效。
func TestImportConfigCatalogSkipsNonEmptyDB(t *testing.T) {
	st, logBuf, logger, _ := newImportEnv(t)
	if err := gateway.ImportConfigCatalog(t.Context(), st, yamlUpstreams(), smokeBindings(), logger); err != nil {
		t.Fatalf("首启 seed: %v", err)
	}
	// 管理台侧：删掉一个模型（连带来源）与一个上游。
	models, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	var arkModelID, arkUpstreamID int64
	for _, m := range models {
		if m.Name == "deepseek-v4-flash" {
			arkModelID, arkUpstreamID = m.ID, m.Sources[0].UpstreamID
		}
	}
	if arkModelID == 0 {
		t.Fatalf("seed 未建出 deepseek-v4-flash: %+v", models)
	}
	if err := st.DeleteModel(t.Context(), arkModelID); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if err := st.DeleteUpstream(t.Context(), arkUpstreamID); err != nil {
		t.Fatalf("DeleteUpstream: %v", err)
	}

	// 重启：YAML 条目还在，但库非空 → 整体跳过，被删的行不复活。
	logBuf.Reset()
	if err := gateway.ImportConfigCatalog(t.Context(), st, yamlUpstreams(), smokeBindings(), logger); err != nil {
		t.Fatalf("重启导入: %v", err)
	}
	if !strings.Contains(logBuf.String(), `"reason":"bootstrap_skipped"`) {
		t.Errorf("非空库应记一条 bootstrap_skipped:\n%s", logBuf.String())
	}
	ups, err := st.ListUpstreams(t.Context())
	if err != nil || len(ups) != 1 || ups[0].Name != "ds" {
		t.Errorf("被删的上游被 YAML 复活: %+v（err=%v）", ups, err)
	}
	after, err := st.ListModelsWithSources(t.Context())
	if err != nil || len(after) != 1 || after[0].Name != "deepseek-chat" {
		t.Errorf("被删的模型被 YAML 复活: %+v（err=%v）", after, err)
	}

	// 只有模型表非空（上游被删光）时同样跳过：闸门看的是两表的并集。
	if err := st.DeleteUpstream(t.Context(), ups[0].ID); err == nil {
		t.Fatal("被来源引用的上游应删不掉（FK 限制）")
	}
}

// TestImportConfigCatalogOptional：两段都缺省时无事可做，也不记日志。
func TestImportConfigCatalogOptional(t *testing.T) {
	st, logBuf, logger, _ := newImportEnv(t)
	if err := gateway.ImportConfigCatalog(t.Context(), st, nil, nil, logger); err != nil {
		t.Fatalf("空段导入: %v", err)
	}
	if logBuf.Len() != 0 {
		t.Errorf("空段导入不应写日志:\n%s", logBuf.String())
	}
}

// TestImportLogsNoKeyMaterial（§15.1）：两张表的导入日志只含条数与名称，
// 凭证明文既不进日志、也不以明文躺在库文件里（设备密钥封存，决策 2）。
func TestImportLogsNoKeyMaterial(t *testing.T) {
	st, logBuf, logger, dir := newImportEnv(t)
	if _, _, err := gateway.ImportConfigUpstreams(t.Context(), st, yamlUpstreams(), logger); err != nil {
		t.Fatalf("导入上游: %v", err)
	}
	if _, _, _, err := gateway.ImportConfigModels(t.Context(), st, map[string]config.Binding{
		"ds-chat": {Upstream: "ds", UpstreamModelID: "deepseek-chat", Protocol: config.ProtocolOpenAIChat},
	}, logger); err != nil {
		t.Fatalf("导入模型: %v", err)
	}

	out := logBuf.String()
	for _, secret := range []string{importKeyDS, importKeyArk} {
		if strings.Contains(out, secret) {
			t.Errorf("导入日志泄露上游 Key 明文 %s:\n%s", logging.RedactKey(secret), out)
		}
		// 超过 4 位的明文前缀同样不允许（对齐 config 包的脱敏尺度）。
		if strings.Contains(out, secret[:12]) {
			t.Errorf("导入日志泄露 Key 前缀 %s:\n%s", logging.RedactKey(secret), out)
		}
	}
	// 导入日志只记条数：条数字段在，物料不在（logging.New 输出 slog JSON 行）。
	for _, want := range []string{`"imported":2`, `"models_created":1`, `"sources_created":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("导入日志缺少条数字段 %s:\n%s", want, out)
		}
	}

	// 库文件原始字节不含凭证明文（WAL 尚未 checkpoint 时明文可能只在 -wal 里，
	// 故三个文件一并扫描）。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(dir, store.DBFileName+suffix)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // -wal/-shm 可能尚未生成
		}
		for _, secret := range []string{importKeyDS, importKeyArk} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s 含上游 Key 明文 %s", filepath.Base(path), logging.RedactKey(secret))
			}
		}
	}
}
