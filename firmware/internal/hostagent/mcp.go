package hostagent

// 给引擎用的 MCP 工具端点（骨架在 internal/mcpserve）：
//
//	POST /agent-mcp   Authorization: Bearer <会话令牌>
//
// 令牌只在一段引擎会话的生命周期里有效（起会话时生成、关会话时作废），拿不到令牌的
// 进程连 tools/list 都看不到；远端地址必须是回环。路由由网关按 LANOnly 挂载（Tunnel
// 不可达）。工具调用的正文（参数、输出）不进日志。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
)

// MCPHandler 是 /agent-mcp 的处理器。
func (m *Manager) MCPHandler() http.Handler {
	return &mcpserve.Server{Name: "llmgate-host", Catalog: toolCatalog, Lookup: func(token string) mcpserve.Session {
		s := m.sessionByToken(token)
		if s == nil {
			return nil
		}
		return s
	}}
}

// CallTool 实现 mcpserve.Session：主机工具只回文本。
func (s *session) CallTool(ctx context.Context, name string, args json.RawMessage) mcpserve.Result {
	text, isErr := s.callTool(ctx, name, args)
	return mcpserve.TextResult(text, isErr)
}

func (m *Manager) sessionByToken(token string) *session {
	if token == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[token]
}

// ErrMCPUnauthorized 只给测试断言用。
var ErrMCPUnauthorized = errors.New("hostagent: mcp unauthorized")
