package hostagent

// 给引擎的开发者指令：主机身份、工作纪律、工具约定、当前主机档案与最近操作。模板在
// prompt.md（go:embed）：这是模型看的文本，不是界面文案，放 .md 让 i18n 提取器不把它
// 当成待翻译的键；工具描述同理在 tools.json。
//
// 主体在对话新建时渲染一次并存进对话行（agent_host_chats.instructions）——这就是「新建
// 对话时注入主机档案与最近的操作日志」；引擎会话在同一个对话里中途重启（空闲超时、
// 管理员结束会话、引擎退出）时，再把本对话先前的指令与回复渲染成一段附在后面，模型
// 才接得上前文。

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

//go:embed prompt.md
var promptTemplateText string

var promptTemplate = template.Must(template.New("prompt").Parse(promptTemplateText))

type promptData struct {
	Name, Username, Address string
	Port                    int
	System, Profile         string
	SudoNoPasswd            bool
	Recent                  []promptOp
}

// promptOp 是最近操作记录的一行（模板按 Kind 渲染）。
type promptOp struct {
	At, Kind, Title string
	Exit            bool
	ExitCode        int
}

func buildInstructions(host store.AgentHost, profile string, recent []store.AgentHostEvent) string {
	d := promptData{Name: host.Name, Username: host.Username, Address: host.Address, Port: host.Port,
		System: host.System, SudoNoPasswd: host.SudoNoPasswd, Profile: strings.TrimSpace(strings.TrimRight(profile, "\n"))}
	if d.Name == "" {
		d.Name = host.Address
	}
	for _, ev := range recent {
		op := promptOp{At: ev.At.Local().Format("01-02 15:04"), Kind: ev.Kind, Title: ev.Title}
		switch ev.Kind {
		case store.AgentEventCommand:
			op.Title = firstLine(ev.Title)
			if ev.ExitCode != nil {
				op.Exit, op.ExitCode = true, *ev.ExitCode
			}
		case store.AgentEventFile, store.AgentEventProfile:
		default:
			continue
		}
		d.Recent = append(d.Recent, op)
	}
	var b strings.Builder
	if err := promptTemplate.Execute(&b, d); err != nil {
		// 模板是随二进制内嵌的常量，执行失败只能是编程错误。
		panic(err)
	}
	return b.String()
}

// 本对话先前记录的上限：条数与每条的字符数（引擎重启时附在开发者指令后面）。
const (
	transcriptEvents    = 30
	transcriptBodyRunes = 1500
)

// transcriptTurn 是先前记录的一行（模板按 Kind 标出说话人，Truncated 标出被截断的正文）。
type transcriptTurn struct {
	At, Kind, Body string
	Truncated      bool
}

// buildTranscript 把本对话先前的指令与回复（旧→新）渲染成附加段；没有记录时为空串。
func buildTranscript(events []store.AgentHostEvent) string {
	turns := make([]transcriptTurn, 0, len(events))
	for _, ev := range events {
		if ev.Kind != store.AgentEventUser && ev.Kind != store.AgentEventAssistant {
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
