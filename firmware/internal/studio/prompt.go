package studio

// 给引擎的开发者指令：工作空间身份与三个子目录的分工、工作纪律、创作类型的工作规程、当前项目说明与目录清单
// （每项是「子目录/文件名」的路径）；节点上的创作工具一节与可用的生成模型一节在引擎会话启动时
// 按那一刻的节点探测与工作空间配置另渲染（nodeToolsSection、media.go）。
// 模板在 prompt.md（go:embed）：模型看的文本，不是界面文案（i18n 提取器不把它当键）；
// 工具描述同理在 internal/studiomcp/tools.json。主体在对话新建时渲染一次并存进对话行；引擎会话在同一对话里
// 中途重启时再把先前的指令与回复渲染成一段附在后面。

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

//go:embed prompt.md
var promptTemplateText string

var promptTemplate = template.Must(template.New("prompt").Parse(promptTemplateText))

type promptFile struct {
	Name, Kind, Size, Origin, Prompt string
	Width, Height                    int
}

// promptData 是模板入参；文案在 prompt.md 里。可用的生成模型不在这里：它随工作空间的配置变，
// 引擎会话启动时另渲染一节（media.go mediaSection）。
type promptData struct {
	Name, Brief   string
	Files         []promptFile
	MoreFiles     int
	BriefFileName string
	// TemplateName / TemplateGuide 是工作空间的创作类型（template.go）：名称与写进指令的工作规程。
	TemplateName, TemplateGuide string
	// HostName / HostLogin / HostPath 是引擎所在的工作节点与工作空间目录。
	HostName, HostLogin, HostPath string
}

// promptListFiles 是写进开发者指令的目录清单上限（三个子目录合计）；promptPromptRunes 是每条
// 提示词摘录的长度。
const (
	promptListFiles   = 80
	promptPromptRunes = 120
)

func buildInstructions(ws store.Workspace, host *store.AgentHost, brief string, files []store.StudioFile) string {
	tpl := templateFor(ws.Template)
	d := promptData{Name: ws.Name, Brief: strings.TrimSpace(strings.TrimRight(brief, "\n")), BriefFileName: BriefFileName,
		TemplateName: tpl.Name, TemplateGuide: tpl.Guide}
	if host != nil {
		d.HostName, d.HostPath = host.Name, ws.Path
		if d.HostName == "" {
			d.HostName = host.Address
		}
		d.HostLogin = fmt.Sprintf("%s@%s:%d", host.Username, host.Address, host.Port)
	}
	for i, f := range files {
		if i >= promptListFiles {
			d.MoreFiles = len(files) - promptListFiles
			break
		}
		p := promptFile{Name: f.Name, Kind: f.Kind, Size: humanSize(f.Bytes), Origin: f.Origin, Width: f.Width, Height: f.Height}
		if r := []rune(strings.TrimSpace(f.Prompt)); len(r) > 0 {
			if len(r) > promptPromptRunes {
				r = append(r[:promptPromptRunes], '…')
			}
			p.Prompt = strings.ReplaceAll(string(r), "\n", " ")
		}
		d.Files = append(d.Files, p)
	}
	var b strings.Builder
	if err := promptTemplate.Execute(&b, d); err != nil {
		panic(err)
	}
	return b.String()
}

// nodeToolsSection 渲染「节点上的创作工具」一节（prompt.md 的 node_tools 模板）：引擎会话启动时
// 按那一次探测的读数写明节点上可用的程序与版本，Agent 不必自己试探；读数缺失（nil）时说明未探到。
func nodeToolsSection(t *devhost.StudioTools) string {
	var b strings.Builder
	if err := promptTemplate.ExecuteTemplate(&b, "node_tools", t); err != nil {
		panic(err)
	}
	return b.String()
}

// 本对话先前记录的上限：条数与每条的字符数。
const (
	transcriptEvents    = 30
	transcriptBodyRunes = 1500
)

type transcriptTurn struct {
	At, Kind, Body string
	Truncated      bool
}

// buildTranscript 把本对话先前的指令与回复（旧→新）渲染成附加段；没有记录时为空串。
func buildTranscript(events []store.StudioEvent) string {
	turns := make([]transcriptTurn, 0, len(events))
	for _, ev := range events {
		if ev.Kind != store.StudioEventUser && ev.Kind != store.StudioEventAssistant {
			continue
		}
		turn := transcriptTurn{At: ev.At.Local().Format("01-02 15:04"), Kind: ev.Kind, Body: strings.TrimSpace(ev.Body)}
		if r := []rune(turn.Body); len(r) > transcriptBodyRunes {
			turn.Body, turn.Truncated = string(r[:transcriptBodyRunes]), true
		}
		turns = append(turns, turn)
	}
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	if err := promptTemplate.ExecuteTemplate(&b, "transcript", turns); err != nil {
		panic(err)
	}
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return trimFloat(float64(n)/(1<<30)) + " GiB"
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + " MiB"
	case n >= 1<<10:
		return trimFloat(float64(n)/(1<<10)) + " KiB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}
