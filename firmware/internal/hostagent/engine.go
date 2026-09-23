package hostagent

// 引擎抽象：Agent远控本体（对话、队列、工具、时间线）不关心背后是哪家智能体运行时，
// 只要它能「校验一段对话钉死的密钥 / 模型、起一段会话、逐条执行指令、把回复流回来、
// 能被中止」。当前唯一实现是 Codex App Server（codex.go）；再接别的引擎只需实现这两个
// 接口并在装配处换掉。
//
// 对主机的操作**不经引擎**：引擎只拿到一个 MCP 工具端点（mcp.go），每次要动主机都得
// 经过设备这一侧执行并留痕，引擎自己没有主机凭据。

import (
	"context"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Engine 是一种智能体运行时。
type Engine interface {
	// ID 是引擎标识（agent_host_chats.engine / agent_host_runs.engine 与界面徽标用），如 codex_app_server。
	ID() string
	// Status 报告引擎此刻能否用：组件是否装好。密钥 / 模型是每个对话自己的事（Validate）。
	Status(ctx context.Context) EngineStatus
	// Validate 校验一段对话要钉死的密钥 / 模型 / 推理档位此刻能不能起会话：新建对话与每次
	// 提交都调。返回落实后的配置（密钥展示串、留空时落成的缺省模型）；不行时返回
	// *Error（CodeKeyInvalid / CodeInvalidInput / CodeEngineNotReady）带可读原因。
	Validate(ctx context.Context, cfg ChatConfig) (ChatConfig, error)
	// Start 为一个对话起一段会话。会话之内的多条指令共享上下文；会话结束即丢弃。
	Start(ctx context.Context, req StartRequest) (EngineSession, error)
}

// EngineStatus 是引擎的可用性读数（界面照原样展示）。
type EngineStatus struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Ready bool   `json:"ready"`
	// Reason 是不可用的原因（可用时为空）。
	Reason string `json:"reason,omitempty" i18n:"text"`
}

// ChatConfig 是一段对话钉死的模型调用配置：新建对话时选定，之后不改。
type ChatConfig struct {
	// KeyID 是模型调用挂靠的 API 密钥；KeyDisplay 是它的展示串（Validate 填）。
	KeyID      int64  `json:"key_id"`
	KeyDisplay string `json:"key_display,omitempty"`
	// Model 留空时 Validate 落成该密钥的缺省模型；Effort 留空即引擎缺省。
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// StartRequest 是起会话的入参。
type StartRequest struct {
	Host store.AgentHost
	// Chat 是这段会话所属的对话（密钥 / 模型 / 档位从它取）。
	Chat store.AgentHostChat
	// Instructions 是给智能体的开发者指令（prompt.go）：对话新建时注入的主机档案与最近
	// 操作，引擎中途重启时再附上本对话先前的记录。
	Instructions string
	// Tools 是设备暴露给这段会话的 MCP 工具端点（mcp.go）；令牌只属于这一段会话。
	Tools ToolEndpoint
}

// ToolEndpoint 是 MCP 工具服务的地址与承载令牌。Server 是写进引擎配置的 MCP 服务器名
// （模型看到的工具命名空间，如 mcp__host__exec 里的 host）；空取 host。
type ToolEndpoint struct {
	URL    string
	Token  string
	Server string
}

// ServerName 是落成的 MCP 服务器名（空取 host）。
func (t ToolEndpoint) ServerName() string {
	if t.Server == "" {
		return "host"
	}
	return t.Server
}

// Input 是一条指令：文本加零到几张图片（data URI）。图片只在内存里传给引擎。
type Input struct {
	Text   string
	Images []string
}

// Sink 接收一条指令执行期间的流式事实。实现由会话提供（session.go），负责落库与刷新读数。
type Sink interface {
	// Delta 是回复正文的增量（流式显示用，不落库）。
	Delta(text string)
	// Message 是一段完整的回复正文（落时间线）。
	Message(text string)
	// Reasoning 是一段推理摘要（落时间线，界面折叠显示）。
	Reasoning(text string)
	// Activity 是当前正在做的事的一句话（工具调用中…），空串即清除。
	Activity(label string)
	// Usage 是本条指令到目前为止消耗的 token（引擎能报告时）。
	Usage(input, output int64)
}

// ExecSink 是 Sink 的可选扩展：引擎在它自己所在的机器上直接做的事（节点端引擎的 shell 命令、
// 改文件、读文件）。板端引擎本地什么也不执行，用不到它；实现方负责截断与落时间线。
type ExecSink interface {
	// Command 是一条执行完的命令：命令行、合并输出（未截断）、退出码（引擎没报告时为 nil）、耗时。
	Command(command, output string, exitCode *int, dur time.Duration)
	// FileChange 是引擎改动的一个文件：路径（引擎给的原样，通常是绝对路径）与动作（write / delete）。
	FileChange(path, action string)
	// ToolUse 是引擎用的一个只读工具（读文件之类）：工具名与对象。
	ToolUse(name, target string)
}

// Outcome 是一条指令执行完的结局。
type Outcome struct {
	// Status ∈ OutcomeCompleted / OutcomeInterrupted / OutcomeFailed。
	Status string
	// Error 是失败原因（可展示）。
	Error string
}

const (
	OutcomeCompleted   = "completed"
	OutcomeInterrupted = "interrupted"
	OutcomeFailed      = "failed"
)

// EngineSession 是一段已起的会话。方法可从不同 goroutine 调用（Run 串行，Interrupt / Close 随时）。
type EngineSession interface {
	// Run 执行一条指令，直到这一轮结束才返回；ctx 取消视同中止。
	Run(ctx context.Context, in Input, sink Sink) (Outcome, error)
	// Interrupt 请求中止正在执行的那条指令（Run 随后以 OutcomeInterrupted 返回）。
	Interrupt(ctx context.Context) error
	// Done 在会话不可再用（引擎进程退出）时关闭。
	Done() <-chan struct{}
	// Close 结束会话并释放引擎资源；可重复调用。
	Close() error
}

// RunTimeout 是一条指令的整体上限：远程排障、装环境动辄十几分钟，但不能没有上限。
const RunTimeout = 60 * time.Minute
