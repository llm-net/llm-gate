package mediagen

import (
	"fmt"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// describe.go 把能力表渲染成给模型看的文本：创作工作空间生成工具的参数描述与创作智能体开发者
// 指令里的模型说明都出自这里，与受理校验、页面表单同一份真值。

// Describe 把一组模型渲染成 Markdown 列表：每个模型一项，每个操作一行（输入角色、提示词
// 是否可省、参数与取值）。
func Describe(models []Model) string {
	var b strings.Builder
	for _, m := range models {
		fmt.Fprintf(&b, "- `%s`（%s · %s · %s）", m.ID, kindLabel(m.Kind), m.Backend, billingLabel(m.Billing))
		if m.Note != "" {
			b.WriteString("：" + m.Note)
		}
		b.WriteByte('\n')
		for _, op := range m.Operations {
			fmt.Fprintf(&b, "  - operation `%s`：%s\n", op.Name, describeOperation(op))
		}
	}
	return b.String()
}

// DescribeKind 渲染某个种类（image / video）的全部能力：订阅后端的固定预设，加各厂商面后端的
// 基线（模型名由管理员配置，用 <后端名> 占位）。工具描述用它——工具清单不随 Key 变化，
// 这把 Key 此刻能用哪些模型写在开发者指令里。
func DescribeKind(kind string) string {
	var models []Model
	for _, m := range presets {
		if m.Kind == kind {
			models = append(models, m)
		}
	}
	for _, name := range Families() {
		if m, _ := FamilyModel(name, "<"+name+" 模型>"); m.Kind == kind {
			models = append(models, m)
		}
	}
	return Describe(models)
}

func describeOperation(op Operation) string {
	var parts []string
	if len(op.Inputs) == 0 {
		parts = append(parts, "不接受媒体输入")
	} else {
		var ins []string
		for _, in := range op.Inputs {
			s := fmt.Sprintf("%s（%s，至多 %d 个，%s", in.Role, in.Label, in.Max, strings.ToUpper(strings.Join(in.Formats, "/")))
			if in.Required {
				s += "，必填"
			}
			ins = append(ins, s+"）")
		}
		parts = append(parts, "输入 "+strings.Join(ins, "、"))
	}
	if len(op.PromptOptionalWith) > 0 {
		parts = append(parts, "带 "+strings.Join(op.PromptOptionalWith, " 或 ")+" 时提示词可省")
	}
	if len(op.Params) == 0 {
		parts = append(parts, "没有参数")
	} else {
		var ps []string
		for _, p := range op.Params {
			ps = append(ps, describeParam(p))
		}
		parts = append(parts, "params "+strings.Join(ps, "、"))
	}
	return strings.Join(parts, "；")
}

func describeParam(p Param) string {
	var detail string
	switch p.Type {
	case ParamEnum:
		detail = strings.Join(p.Values, " | ")
	case ParamInteger:
		detail = fmt.Sprintf("整数 %d–%d", p.Min, p.Max)
		if p.Unit != "" {
			detail += " " + p.Unit
		}
	case ParamBoolean:
		detail = "true | false"
	case ParamStrings:
		detail = fmt.Sprintf("字符串数组，至多 %d 项", p.MaxItems)
	case ParamString:
		detail = "字符串"
	}
	if p.Required {
		detail += "，必填"
	}
	if p.Description != "" {
		detail += "；" + p.Description
	}
	return fmt.Sprintf("%s（%s：%s）", p.Name, p.Label, detail)
}

func kindLabel(kind string) string {
	if kind == store.ModelKindVideo {
		return "视频"
	}
	return "图像"
}

func billingLabel(billing string) string {
	if billing == BillingMetered {
		return "按量计费"
	}
	return "订阅流量"
}
