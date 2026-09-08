package grokhelper

import (
	"strings"
	"testing"
)

const testKey = "sk_testkey0123456789"

var catalogFixture = []CatalogModel{
	{
		Slug: "grok-4.6", Name: "Grok 4.6", Description: "fixture frontier model",
		ContextWindow: 500000, AutoCompactThresholdPercent: 80,
		SystemPromptLabel: "Grok 4.6", SupportsBackendSearch: true,
		Efforts: []EffortOption{
			{ID: "xhigh", Value: "xhigh", Label: "Extra High Effort", Default: true},
			{ID: "low", Value: "low", Label: "Low Effort"},
		},
	},
	{Slug: "grok-4.5", Name: "Grok 4.5", ContextWindow: 500000},
}

func TestRenderManagedRegionShape(t *testing.T) {
	got := RenderManagedRegion("http://dev.example:8080", testKey, "grok-4.6", catalogFixture)
	for _, want := range []string{
		"# >>> SOC AGENT grok managed block >>>\n", // 已安装的 gate 依赖此边界
		"\n# <<< SOC AGENT grok managed block <<<\n",
		"description = \"fixture frontier model\"\n",
		"[model.\"llmgate-grok-4.6\"]\nname = \"LLM Gate · Grok 4.6\"\nmodel = \"grok-4.6\"\n",
		"[model.\"llmgate-grok-4.5\"]\nname = \"LLM Gate · Grok 4.5\"\n",
		"base_url = \"http://dev.example:8080/agents/grok/v1\"\n",
		"api_key = \"" + testKey + "\"\n",
		"context_window = 500000\n",
		"auto_compact_threshold_percent = 80\n",
		"supports_backend_search = true\n",
		"reasoning_efforts = [\n  { id = \"xhigh\", value = \"xhigh\", label = \"Extra High Effort\", default = true },\n  { id = \"low\", value = \"low\", label = \"Low Effort\", default = false },\n]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("受管区缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "[model.llmgate]") {
		t.Errorf("受管区不该有无模型名别名：\n%s", got)
	}
	first := "[model." + tomlQuote(EntryKey("grok-4.6")) + "]"
	if idx, other := strings.Index(got, first), strings.Index(got, "[model.\"llmgate-grok-4.5\"]"); idx < 0 || idx > other {
		t.Errorf("默认条目未排在最前：\n%s", got)
	}

	minimal := RenderManagedRegion("http://dev.example:8080", testKey, "", nil)
	if strings.Count(minimal, "[model.") != 1 ||
		!strings.Contains(minimal, "[model.\""+EntryKey(FallbackDefaultModel)+"\"]\n") ||
		!strings.Contains(minimal, "name = \"LLM Gate · "+FallbackDefaultModel+"\"\n") ||
		!strings.Contains(minimal, "model = \""+FallbackDefaultModel+"\"\n") {
		t.Errorf("空目录降级条目不正确：\n%s", minimal)
	}
}

func TestRenderLegacyEffortScalars(t *testing.T) {
	got := RenderManagedRegion("http://d", testKey, "x", []CatalogModel{
		{Slug: "grok-x", ReasoningEffort: "high", SupportsReasoningEffort: true},
	})
	for _, want := range []string{"supports_reasoning_effort = true\n", "reasoning_effort = \"high\"\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
}

func TestTomlQuote(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:          `"plain"`,
		`with "quotes"`:  `"with \"quotes\""`,
		`back\slash`:     `"back\\slash"`,
		"ctrl\x01\ntail": "\"ctrl\\u0001\\u000Atail\"",
		`中文·点`:           `"中文·点"`,
	} {
		if got := tomlQuote(in); got != want {
			t.Errorf("tomlQuote(%q) = %s，期望 %s", in, got, want)
		}
	}
}
