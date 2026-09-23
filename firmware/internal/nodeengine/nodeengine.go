// Package nodeengine 在**工作节点**上运行智能体 CLI，给创作工作空间当引擎：Codex CLI 的
// `codex app-server`（stdio JSON-RPC）与 Claude Code 的 stream-json 模式（`claude -p
// --input-format stream-json --output-format stream-json`），以及 Grok 的 `grok agent stdio`（ACP）。
// 引擎就跑在工作空间所在的那台节点上，用它自己的 shell / 读写文件工具直接处理目录里的素材；
// 设备只负责装配、转发与留痕。
//
// 一段会话 = 一条到节点的 SSH 连接（agenthost.Manager.Open，凭访问证书、主机公钥钉死）上的
// 三样东西：
//
//   - 一个**远程转发**（agenthost.Conn.Listen）：节点回环上的一个端口，连进来的连接经这条
//     SSH 连接回到设备，由本包的转发服务交给设备自己的回环网关（Options.Upstream）。只放行
//     开发工具接入面（/agents/codex/、/agents/claude/、/agents/grok/）与这段会话的 MCP 端点——节点不必按
//     局域网地址连得到设备，也碰不到管理面。模型调用照旧凭对话钉死的 API 密钥，订阅授权、
//     计量与预算和其它客户端同一道闸。
//   - 一段**准备脚本**（/bin/sh -s，脚本经 stdin 交过去）：在
//     `${XDG_CACHE_HOME:-$HOME/.cache}/llmgate/engine/s.XXXXXXXX`（0700）里落这段会话要用
//     的文件——Codex 的 config.toml、Claude 的 mcp.json / 开发者指令 / API 密钥文件。密钥与
//     MCP 令牌**只在这些 0600 文件里**，不进 argv、不进引擎的环境变量（引擎起的 shell 子进程
//     会继承环境，`env` 一下就进了时间线）；顺手清掉上一次异常退出留下的实例目录。
//     Grok 会保存会话，实例目录改用 /dev/shm/llmgate-<uid>/engine/，必须是 tmpfs。
//   - 一条**流式命令**（agenthost.Conn.Start）：包装脚本记下自己的 pid、`cd` 进工作空间、拉起
//     CLI，stdin / stdout 就是协议通道；CLI 退出（stdin 收到 EOF 即退）时 trap 删掉实例目录。
//
// 引擎在节点上以 SSH 登录用户的身份、不设沙箱运行（Codex `danger-full-access`，Claude 开放
// Bash / Read / Edit 等内置工具），与 Agent远控经 SSH 执行命令同一权限边界；每条命令与每次改
// 文件都经 hostagent.ExecSink 落时间线。Claude 一律 --bare 且 --setting-sources 给空串：不读
// 节点上用户或项目的 settings（工作空间里的 .claude/settings.json 能把 ANTHROPIC_BASE_URL
// 改道、把密钥带走）、不读 CLAUDE.md、不装插件与 hooks。
//
// §15.1：本包日志只记主机 id、引擎、时长与结局；协议正文、指令、回复、密钥与令牌不进日志。
package nodeengine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

// 引擎标识（studio_chats.engine / studio_runs.engine 与界面徽标用）。Codex 沿用板端引擎的
// 标识：存量对话行里记的就是它。
const (
	CodexID  = hostagent.CodexEngineID
	ClaudeID = "claude_code"
	GrokID   = "grok_acp"
)

// Engine 是一种在工作节点上运行的智能体 CLI。
type Engine interface {
	// ID 是引擎标识（CodexID / ClaudeID / GrokID）；Label 是界面上的名字。
	ID() string
	Label() string
	// Tool 是 gate 管的开发工具名（codex / claude / grok）：节点上的程序按它从工具配置的读数里找，
	// 密钥 / 模型按它在对应的开发工具接入面上解析。
	Tool() string
	// Efforts 是可选的推理档位（空 = 引擎缺省）。
	Efforts() []string
	// Validate 校验对话要钉死的密钥 / 模型 / 档位，并把留空的模型落成缺省；不行时返回
	// *hostagent.Error（CodeKeyInvalid / CodeInvalidInput / CodeEngineNotReady）。
	Validate(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error)
	// Start 在工作节点上起一段会话。
	Start(ctx context.Context, req Request) (hostagent.EngineSession, error)
}

// Request 是在工作节点上起一段会话的入参。
type Request struct {
	HostID int64
	// Binary 是节点上 CLI 的绝对路径（工具配置探测到的 gate 关联路径，或 PATH 上的同名程序）。
	Binary string
	// Workdir 是引擎的工作目录：工作空间在节点上的绝对路径。
	Workdir string
	Chat    hostagent.ChatConfig
	// Instructions 是开发者指令。
	Instructions string
	// Tools 是设备上的 MCP 工具端点：URL 是设备侧的路径（如 /studio-mcp），本包把它换成节点上
	// 经远程转发可达的地址；Token 只属于这一段会话。
	Tools hostagent.ToolEndpoint
}

// Resolver 按对话钉死的密钥 / 模型 / 档位，在 tool 对应的开发工具接入面上解析凭据（管理面实现，
// admin.AgentChatConfig）。只有就绪时才带明文。
type Resolver func(ctx context.Context, tool string, cfg hostagent.ChatConfig) (hostagent.KeyConfig, error)

// Opener 打开到工作节点的 SSH 连接（agenthost.Manager 满足）。
type Opener interface {
	Open(ctx context.Context, id int64) (*agenthost.Conn, error)
}

// Options 装配节点端引擎（同一份）。
type Options struct {
	Hosts   Opener
	Resolve Resolver
	// Upstream 是设备自己的回环基址（hostagent.LoopbackBaseURL）：节点上的引擎经远程转发回到这里。
	Upstream string
	// Version 是协议握手时自报的客户端版本（固件版本）。
	Version string
	Logger  *slog.Logger
}

func (o *Options) logger() *slog.Logger {
	if o.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.Logger
}

// Engines 装配全部节点端引擎（界面上的顺序）。
func Engines(o Options) []Engine {
	return []Engine{NewCodex(o), NewClaude(o), NewGrok(o)}
}

// validate 是各引擎共用的 Validate：档位在词汇表里、选了密钥、密钥在 tool 面上可用。
func validate(ctx context.Context, o *Options, tool string, efforts []string, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	if cfg.Effort != "" && !slices.Contains(efforts, cfg.Effort) {
		return cfg, &hostagent.Error{Code: hostagent.CodeInvalidInput, Msg: fmt.Sprintf("推理档位须是 %s 之一或留空", strings.Join(efforts, "、"))}
	}
	resolved, err := resolve(ctx, o, tool, cfg)
	if err != nil {
		return cfg, err
	}
	cfg.KeyDisplay = resolved.KeyDisplay
	cfg.Model = resolved.Model
	return cfg, nil
}

// resolve 解析一段对话的凭据，不就绪即 CodeKeyInvalid。
func resolve(ctx context.Context, o *Options, tool string, cfg hostagent.ChatConfig) (hostagent.KeyConfig, error) {
	if cfg.KeyID <= 0 {
		return hostagent.KeyConfig{}, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: "请为对话选择一把 API 密钥"}
	}
	if o.Resolve == nil {
		return hostagent.KeyConfig{}, &hostagent.Error{Code: hostagent.CodeEngineNotReady, Msg: "本进程未接入密钥解析"}
	}
	resolved, err := o.Resolve(ctx, tool, cfg)
	if err != nil {
		return resolved, err
	}
	if !resolved.Ready {
		return resolved, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: resolved.Reason}
	}
	return resolved, nil
}
