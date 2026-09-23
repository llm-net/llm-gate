package modeld

// 本机 Codex：`codex app-server`（stdio JSON-RPC），协议形态与 internal/hostagent/codex.go 相同
// （initialize → thread/start（ephemeral）→ turn/start → 通知直到 turn/completed）。实例的
// config.toml 在实例目录里（CODEX_HOME），provider 指向设备的 /agents/codex/v1、密钥写成
// experimental_bearer_token——只在这个 0600 文件里，不进环境变量（codex 的 shell 子进程会继承
// 环境）。线程 sandbox 为 danger-full-access：引擎就跑在模型服务节点上、以守护进程用户的身份，
// 本地命令与改文件的审批一律放行、结果落事件流。history 与 analytics 关掉。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	codexStartTimeout   = 60 * time.Second
	codexInterruptGrace = 10 * time.Second
	codexIncomingBuffer = 256
)

type codexDriver struct {
	client   *rpcClient
	threadID string
	effort   string

	mu      sync.Mutex
	turn    *codexTurn
	usageIn int64
	usageOu int64
}

type codexTurn struct {
	id     string
	msgs   chan rpcMessage
	baseIn int64
	baseOu int64
}

func codexConfigTOML(apiBase, apiKey, model, effort string) string {
	var b strings.Builder
	b.WriteString("model_provider = \"llmgate\"\n")
	if model != "" {
		fmt.Fprintf(&b, "model = %s\n", jsonString(model))
	}
	if effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", jsonString(effort))
	}
	b.WriteString("web_search = \"disabled\"\n\n")
	b.WriteString("[model_providers.llmgate]\nname = \"LLM Gate\"\n")
	fmt.Fprintf(&b, "base_url = %s\n", jsonString(apiBase+"/agents/codex/v1"))
	b.WriteString("wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "experimental_bearer_token = %s\n\n", jsonString(apiKey))
	b.WriteString("[features]\nmulti_agent = false\nmulti_agent_v2 = false\n\n")
	b.WriteString("[history]\npersistence = \"none\"\n\n[analytics]\nenabled = false\n")
	return b.String()
}

func (m *engineManager) startCodex(ctx context.Context, sess *session, in sessionInput) (engineDriver, error) {
	if err := writeInstanceFile(sess.dir, "config.toml", codexConfigTOML(in.APIBase, in.APIKey, in.Model, in.Effort)); err != nil {
		return nil, err
	}
	env := append(m.baseEnv(), "CODEX_HOME="+sess.dir, "RUST_LOG=error")
	stdin, stdout, err := m.spawn(sess, []string{"app-server"}, env)
	if err != nil {
		return nil, err
	}
	client := newRPCClient(stdout, stdin, codexIncomingBuffer)
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	params := map[string]any{
		"clientInfo":   map[string]any{"name": "llmgate-modeld", "title": "LLM Gate", "version": m.s.version},
		"capabilities": map[string]any{"experimentalApi": true},
	}
	if err := client.Call(ctx, "initialize", params, &init); err != nil {
		client.Close()
		return nil, errors.New("Codex app-server 握手失败（节点上的 Codex CLI 需支持 app-server）：" + err.Error())
	}
	if err := client.Notify("initialized", nil); err != nil {
		client.Close()
		return nil, err
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	tparams := map[string]any{
		"ephemeral": true, "cwd": sess.Workdir, "approvalPolicy": "on-request", "sandbox": "danger-full-access",
		"developerInstructions": in.Instructions,
	}
	if in.Model != "" {
		tparams["model"] = in.Model
	}
	sctx, cancel := context.WithTimeout(ctx, codexStartTimeout)
	defer cancel()
	if err := client.Call(sctx, "thread/start", tparams, &started); err != nil {
		client.Close()
		return nil, errors.New("Codex App Server 开线程失败：" + err.Error())
	}
	d := &codexDriver{client: client, threadID: started.Thread.ID, effort: in.Effort}
	go d.readLoop()
	return d, nil
}

func (d *codexDriver) Done() <-chan struct{} { return d.client.Done() }

func (d *codexDriver) readLoop() {
	for {
		select {
		case msg := <-d.client.Incoming():
			if len(msg.ID) > 0 {
				d.answer(msg)
				continue
			}
			d.mu.Lock()
			t := d.turn
			d.mu.Unlock()
			if t == nil {
				continue
			}
			select {
			case t.msgs <- msg:
			case <-d.client.Done():
				return
			}
		case <-d.client.Done():
			return
		}
	}
}

// answer 应答反向请求：本地命令 / 改文件审批放行（引擎就在本机、权限即守护进程用户的），权限升级
// 拒绝，提问答空，其它答空对象。
func (d *codexDriver) answer(msg rpcMessage) {
	var result any
	switch msg.Method {
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": "accept", "content": map[string]any{}}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]any{"decision": "accept"}
	case "item/permissions/requestApproval":
		result = map[string]any{"decision": "decline"}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	default:
		result = map[string]any{}
	}
	_ = d.client.Reply(msg.ID, result)
}

func (d *codexDriver) Run(ctx context.Context, text string, sink *sessionSink) (string, string) {
	d.mu.Lock()
	t := &codexTurn{msgs: make(chan rpcMessage, 1024), baseIn: d.usageIn, baseOu: d.usageOu}
	d.turn = t
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.turn = nil
		d.mu.Unlock()
	}()
	params := map[string]any{"threadId": d.threadID, "input": []map[string]any{{"type": "text", "text": text}}}
	if d.effort != "" {
		params["effort"] = d.effort
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	cctx, cancel := context.WithTimeout(ctx, codexStartTimeout)
	err := d.client.Call(cctx, "turn/start", params, &started)
	cancel()
	if err != nil {
		if errors.Is(err, errRPCClosed) {
			return "failed", "引擎进程已退出"
		}
		return "failed", "引擎未能开始执行：" + err.Error()
	}
	d.mu.Lock()
	t.id = started.Turn.ID
	d.mu.Unlock()
	lastError := ""
	for {
		select {
		case msg := <-t.msgs:
			if out, errMsg, done := d.handle(t, msg, &lastError, sink); done {
				return out, errMsg
			}
		case <-d.client.Done():
			return "failed", firstNonEmpty(lastError, "引擎进程已退出")
		case <-ctx.Done():
			_ = d.Interrupt(context.Background())
			grace := time.NewTimer(codexInterruptGrace)
			for {
				select {
				case msg := <-t.msgs:
					if out, errMsg, done := d.handle(t, msg, &lastError, sink); done {
						grace.Stop()
						return out, errMsg
					}
				case <-grace.C:
					return "interrupted", ""
				case <-d.client.Done():
					grace.Stop()
					return "interrupted", ""
				}
			}
		}
	}
}

func (d *codexDriver) handle(t *codexTurn, msg rpcMessage, lastError *string, sink *sessionSink) (string, string, bool) {
	switch msg.Method {
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.Delta != "" {
			sink.Delta(p.Delta)
		}
	case "item/started", "item/completed":
		var p struct {
			Item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Tool    string `json:"tool"`
				Command string `json:"command"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
				AggregatedOutput string `json:"aggregatedOutput"`
				ExitCode         *int   `json:"exitCode"`
				Changes          []struct {
					Path string          `json:"path"`
					Kind json.RawMessage `json:"kind"`
				} `json:"changes"`
			} `json:"item"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return "", "", false
		}
		completed := msg.Method == "item/completed"
		switch p.Item.Type {
		case "agentMessage":
			if completed {
				sink.Message(p.Item.Text)
			}
		case "reasoning":
			if completed {
				parts := make([]string, 0, len(p.Item.Summary))
				for _, sm := range p.Item.Summary {
					if strings.TrimSpace(sm.Text) != "" {
						parts = append(parts, sm.Text)
					}
				}
				if len(parts) > 0 {
					sink.Reasoning(strings.Join(parts, "\n"))
				}
			} else {
				sink.Activity("思考中…")
			}
		case "mcpToolCall":
			if !completed {
				sink.Activity("调用工具：" + p.Item.Tool)
			}
		case "commandExecution":
			if !completed {
				sink.Activity("本地命令：" + firstLine(unwrapShellCommand(p.Item.Command)))
				break
			}
			sink.Command(unwrapShellCommand(p.Item.Command), p.Item.AggregatedOutput, p.Item.ExitCode)
		case "fileChange":
			if completed {
				for _, ch := range p.Item.Changes {
					sink.File(ch.Path, fileChangeAction(ch.Kind))
				}
			}
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Total struct {
					InputTokens  int64 `json:"inputTokens"`
					OutputTokens int64 `json:"outputTokens"`
				} `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			d.mu.Lock()
			in, out := p.TokenUsage.Total.InputTokens-d.usageIn, p.TokenUsage.Total.OutputTokens-d.usageOu
			d.usageIn, d.usageOu = p.TokenUsage.Total.InputTokens, p.TokenUsage.Total.OutputTokens
			d.mu.Unlock()
			if in > 0 || out > 0 {
				sink.Usage(max(in, 0), max(out, 0))
			}
		}
	case "error":
		var p struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && !p.WillRetry && p.Error.Message != "" {
			*lastError = p.Error.Message
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return "failed", "引擎回报了无法解析的结局", true
		}
		switch p.Turn.Status {
		case "completed":
			return "completed", "", true
		case "interrupted":
			return "interrupted", "", true
		default:
			reason := *lastError
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				reason = p.Turn.Error.Message
			}
			return "failed", firstNonEmpty(reason, "引擎报告执行失败"), true
		}
	}
	return "", "", false
}

func (d *codexDriver) Interrupt(ctx context.Context) error {
	d.mu.Lock()
	t := d.turn
	d.mu.Unlock()
	if t == nil || t.id == "" {
		return errors.New("没有正在执行的指令")
	}
	cctx, cancel := context.WithTimeout(ctx, codexInterruptGrace)
	defer cancel()
	return d.client.Call(cctx, "turn/interrupt", map[string]any{"threadId": d.threadID, "turnId": t.id}, nil)
}

var shellWrapRE = regexp.MustCompile(`^\S*/?(?:ba|z|da)?sh -l?c '(.*)'$`)

func unwrapShellCommand(cmd string) string {
	m := shellWrapRE.FindStringSubmatch(strings.TrimSpace(cmd))
	if m == nil {
		return cmd
	}
	return strings.ReplaceAll(m[1], `'\''`, `'`)
}

func fileChangeAction(kind json.RawMessage) string {
	var k struct {
		Type string `json:"type"`
	}
	var name string
	if json.Unmarshal(kind, &k) == nil && k.Type != "" {
		name = k.Type
	} else {
		_ = json.Unmarshal(kind, &name)
	}
	if strings.EqualFold(name, "delete") {
		return "delete"
	}
	return "write"
}
