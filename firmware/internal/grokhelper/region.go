// region.go 是 ~/.grok/config.toml 受管区的**唯一** TOML 渲染器（契约见
// docs/firmware-agents-grok.md「订阅模型目录与受管区」）。
//
// 受管区由 GET /grok-helper/managed-config（internal/gateway/grok_managed.go）
// 服务端渲染；gate 只按原字节写进自己的 Grok 配置根，不在客户端复刻条目。
//
// §15.1：api_key 由调用方从请求 Authorization 头取来回填——密钥从属主的请求里
// 来、回到属主的响应里去（与「属主自助复制」同一条产品决定）；本包不落任何
// 日志，渲染结果也绝不进日志。
package grokhelper

import (
	"fmt"
	"strings"
)

// RegionBegin / RegionEnd 是受管区的首尾标记行。多表受管区不能再用「表头到
// 下一个表头」定界（区内有多个表与数组表），这两行 ASCII 标记是唯一稳定边界；
// gate 把整段响应作为唯一受管配置写入独立配置根。
// 保留 SOC AGENT 边界供已安装的 gate 校验；它们只在 TOML 注释中，不作模型显示名。
const (
	RegionBegin = "# >>> SOC AGENT grok managed block >>>"
	RegionEnd   = "# <<< SOC AGENT grok managed block <<<"
)

// FallbackDefaultModel 是默认条目在「查询参数与管理员 default_model 都缺席」
// 时的模型名（docs.x.ai 官方示例的旗舰名）。
const FallbackDefaultModel = "grok-4.5"

// EffortOption 是效力菜单的一个档位（wire 与 CLI config 同形：
// {id,value,label,description,default}，value 必填其余可缺）。
type EffortOption struct {
	ID          string
	Value       string
	Label       string
	Description string
	Default     bool
}

// CatalogModel 是订阅目录里一个模型在受管区里要用到的元数据子集。零值字段
// 不渲染（wire 缺席与显式零对 CLI 是一回事：宽松解析下缺字段走 CLI 缺省）。
type CatalogModel struct {
	Slug                        string // wire 的 model/id：请求里发的槽位名
	Name                        string // 目录显示名；空退 Slug
	Description                 string
	ContextWindow               int64
	AutoCompactThresholdPercent int64
	SystemPromptLabel           string
	SupportsBackendSearch       bool
	// Efforts 是效力菜单的唯一事实源；非空时 CLI 自行派生下面两个 legacy
	// 标量，所以列表在场就不写标量（docs/firmware-agents-grok.md）。
	Efforts                 []EffortOption
	ReasoningEffort         string
	SupportsReasoningEffort bool
}

// RenderManagedRegion 渲染完整受管区文本（含首尾标记行与结尾换行）。
//
// 每个模型一个条目，表键恒为 TOML 引号键 `"llmgate-<slug>"`——slug 含 `.`
// 时裸键的 `.` 是表分隔符；`llmgate-` 前缀是防劫持约束：裸 slug 键会作为
// override 叠进用户个人登录的同名原生目录条目（契约文档记有论证与真机验证）。
//
// defaultModel 那一条**恒排在最前**，Grok 据此选出缺省条目；它不在目录里
// （目录空、拉取降级、管理员设了个
// 目录外的名字）时补一条只有五行核心的同名条目，所以受管区恒非空、
// 恒有一条可直接用的默认条目。
//
// 这里**不再渲染 `[model.llmgate]` 这种不带模型名的别名条目**：它与默认
// 模型那条逐字段重复，在 grok 的 /model 里多出一行看不出是哪个模型的
// 「LLM Gate」，只会让人选错。
func RenderManagedRegion(base, apiKey, defaultModel string, models []CatalogModel) string {
	if defaultModel == "" {
		defaultModel = FallbackDefaultModel
	}

	// 默认条目提到最前；不在目录里就补一条无元数据的。
	entries := make([]CatalogModel, 0, len(models)+1)
	rest := make([]CatalogModel, 0, len(models))
	found := false
	for i := range models {
		if !found && models[i].Slug == defaultModel {
			entries = append(entries, models[i])
			found = true
			continue
		}
		rest = append(rest, models[i])
	}
	if !found {
		entries = append(entries, CatalogModel{Slug: defaultModel})
	}
	entries = append(entries, rest...)

	var b strings.Builder
	b.WriteString(RegionBegin + "\n")
	b.WriteString("# 本区由 LLM Gate 管理；gate grok 启动时会整区更新。\n")
	for i := range entries {
		m := &entries[i]
		display := m.Name
		if display == "" {
			display = m.Slug
		}
		if i > 0 {
			b.WriteString("\n")
		}
		// 补出来的默认条目字段全零，writeModelEntry 只写五行核心。
		header := fmt.Sprintf("[model.%s]", tomlQuote(EntryKey(m.Slug)))
		writeModelEntry(&b, header, "LLM Gate · "+display, m.Slug, base, apiKey, m)
	}
	b.WriteString("\n" + RegionEnd + "\n")
	return b.String()
}

// EntryKey 是模型 slug 在受管区里的表键（不含引号）。
func EntryKey(slug string) string { return "llmgate-" + slug }

// writeModelEntry 写出一个模型条目：五行核心 + 可选目录元数据（meta 为 nil
// 时只有核心）。字段顺序固定——受管区按字节比对幂等，顺序即契约。
func writeModelEntry(b *strings.Builder, header, name, model, base, apiKey string, meta *CatalogModel) {
	b.WriteString(header + "\n")
	b.WriteString("name = " + tomlQuote(name) + "\n")
	b.WriteString("model = " + tomlQuote(model) + "\n")
	b.WriteString("base_url = " + tomlQuote(base+"/agents/grok/v1") + "\n")
	b.WriteString("api_backend = \"responses\"\n")
	b.WriteString("api_key = " + tomlQuote(apiKey) + "\n")
	if meta == nil {
		return
	}
	if meta.Description != "" {
		b.WriteString("description = " + tomlQuote(meta.Description) + "\n")
	}
	if meta.ContextWindow > 0 {
		fmt.Fprintf(b, "context_window = %d\n", meta.ContextWindow)
	}
	if meta.AutoCompactThresholdPercent > 0 {
		fmt.Fprintf(b, "auto_compact_threshold_percent = %d\n", meta.AutoCompactThresholdPercent)
	}
	if meta.SystemPromptLabel != "" {
		b.WriteString("system_prompt_label = " + tomlQuote(meta.SystemPromptLabel) + "\n")
	}
	if meta.SupportsBackendSearch {
		b.WriteString("supports_backend_search = true\n")
	}
	switch {
	case len(meta.Efforts) > 0:
		b.WriteString("reasoning_efforts = [\n")
		for _, e := range meta.Efforts {
			b.WriteString("  { ")
			if e.ID != "" {
				b.WriteString("id = " + tomlQuote(e.ID) + ", ")
			}
			b.WriteString("value = " + tomlQuote(e.Value))
			if e.Label != "" {
				b.WriteString(", label = " + tomlQuote(e.Label))
			}
			if e.Description != "" {
				b.WriteString(", description = " + tomlQuote(e.Description))
			}
			fmt.Fprintf(b, ", default = %t },\n", e.Default)
		}
		b.WriteString("]\n")
	case meta.ReasoningEffort != "" || meta.SupportsReasoningEffort:
		if meta.SupportsReasoningEffort {
			b.WriteString("supports_reasoning_effort = true\n")
		}
		if meta.ReasoningEffort != "" {
			b.WriteString("reasoning_effort = " + tomlQuote(meta.ReasoningEffort) + "\n")
		}
	}
}

// tomlQuote 把 s 编码成 TOML basic string（含首尾双引号）。TOML 不认 Go
// strconv.Quote 的 \xNN 转义，控制字符必须走 \u00XX，其余合法 Unicode 原样。
func tomlQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
