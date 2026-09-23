package nodeengine

// 节点端 Codex：工作节点上 gate 关联的 Codex CLI 以 `codex app-server` 跑 stdio JSON-RPC，
// 协议与板端 Codex App Server 相同（会话逻辑共用 hostagent.StartCodexThread）。区别只在
// 执行位置与边界：线程的 cwd 是工作空间目录、sandbox 为 danger-full-access，本地命令与改
// 文件的审批一律放行、结果经 hostagent.ExecSink 落时间线。
//
// 实例的 config.toml 在实例目录里（CODEX_HOME），provider 指向远程转发回设备的
// /agents/codex/v1。密钥写成 provider 的 experimental_bearer_token、MCP 令牌写成
// http_headers——都只在这个 0600 文件里，不进环境变量：模型起的 shell 继承实例的环境，而
// codex 的 shell_environment_policy 缺省保留含 KEY / TOKEN 的变量（ignore_default_excludes
// 缺省为真），实测 v0.149 / v0.155 下密钥经环境变量会被 `env` 一下带进时间线。history 与
// analytics 都关掉：会话记录只在设备上，节点上不留 history.jsonl，也不向 OpenAI 报遥测。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

const (
	codexHandshakeTimeout = 60 * time.Second
	// codexToolTimeoutSec 是 MCP 工具调用的上限：生成视频要陪等到终态，与板端同值。
	codexToolTimeoutSec = 960
	codexIncomingBuffer = 256
)

type codexEngine struct{ o Options }

// NewCodex 装配节点端 Codex 引擎。
func NewCodex(o Options) Engine { return &codexEngine{o: o} }

func (e *codexEngine) ID() string        { return CodexID }
func (e *codexEngine) Label() string     { return "Codex" }
func (e *codexEngine) Tool() string      { return "codex" }
func (e *codexEngine) Efforts() []string { return hostagent.CodexEfforts }

func (e *codexEngine) Validate(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	return validate(ctx, &e.o, e.Tool(), e.Efforts(), cfg)
}

// Start 在节点上拉起 `codex app-server`、握手、开 ephemeral 线程。
func (e *codexEngine) Start(ctx context.Context, req Request) (hostagent.EngineSession, error) {
	cfg, err := resolve(ctx, &e.o, e.Tool(), req.Chat)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	r, err := e.o.start(ctx, req, launchSpec{
		files: func(base string) map[string]string {
			return map[string]string{"config.toml": codexConfigTOML(base, req.Tools, cfg)}
		},
		command: func(string) string {
			return "CODEX_HOME=\"$d\" RUST_LOG=error " + shellQuote(req.Binary) + " app-server"
		},
	})
	if err != nil {
		return nil, err
	}
	log := e.o.logger().With("srv", "nodeengine", "engine", CodexID, "host_id", req.HostID)
	client := codexappserver.NewClient(r.stream.Stdout, r.stream.Stdin, codexIncomingBuffer)
	closeFn := func() error {
		client.Close()
		err := r.Close()
		log.Info("节点端引擎会话已关闭")
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, codexHandshakeTimeout)
	defer cancel()
	if _, err := codexappserver.Handshake(hctx, client, e.o.Version); err != nil {
		derr := r.diagnose("Codex app-server 握手失败（节点上的 Codex CLI 需支持 app-server）", err)
		closeFn()
		return nil, derr
	}
	sess, err := hostagent.StartCodexThread(ctx, client, closeFn, hostagent.CodexThreadOptions{
		Cwd: req.Workdir, Sandbox: "danger-full-access", Instructions: req.Instructions,
		Model: cfg.Model, Effort: cfg.Effort, LocalExec: true, Log: log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("节点端引擎会话已启动", "duration_ms", time.Since(started).Milliseconds())
	return sess, nil
}

// codexConfigTOML 渲染节点上实例的 config.toml（含密钥与令牌，只落在 0600 的实例目录里）。
func codexConfigTOML(base string, tools hostagent.ToolEndpoint, cfg hostagent.KeyConfig) string {
	var b strings.Builder
	b.WriteString("model_provider = \"llmgate\"\n")
	if cfg.Model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlQuote(cfg.Model))
	}
	if cfg.Effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", tomlQuote(cfg.Effort))
	}
	b.WriteString("web_search = \"disabled\"\n\n")
	b.WriteString("[model_providers.llmgate]\nname = \"LLM Gate\"\n")
	fmt.Fprintf(&b, "base_url = %s\n", tomlQuote(base+"/agents/codex/v1"))
	b.WriteString("wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "experimental_bearer_token = %s\n\n", tomlQuote(cfg.KeyPlaintext))
	b.WriteString("[features]\nmulti_agent = false\nmulti_agent_v2 = false\n\n")
	b.WriteString("[history]\npersistence = \"none\"\n\n[analytics]\nenabled = false\n\n")
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", tools.ServerName())
	fmt.Fprintf(&b, "url = %s\n", tomlQuote(base+tools.URL))
	fmt.Fprintf(&b, "http_headers = { Authorization = %s }\n", tomlQuote("Bearer "+tools.Token))
	b.WriteString("startup_timeout_sec = 30\n")
	fmt.Fprintf(&b, "tool_timeout_sec = %d\n", codexToolTimeoutSec)
	return b.String()
}

// tomlQuote 用 TOML 基本字符串（与 JSON 字符串同形）引一个值。
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
