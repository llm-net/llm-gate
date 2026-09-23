// Package mcpserve 是设备暴露给智能体引擎的 MCP 工具端点的公共骨架（Streamable HTTP、
// JSON-RPC over POST）：
//
//	POST <path>   Authorization: Bearer <会话令牌>
//
// 只服务本机回环上的引擎进程：远端地址必须是回环，令牌由调用方按一段引擎会话的生命周期
// 签发 / 作废（Lookup 找不到即 401），拿不到令牌的进程连 tools/list 都看不到。路由由网关
// 按 LANOnly 挂载（Tunnel 不可达），这里再钉一层回环。
//
// 支持 initialize / ping / tools/list / tools/call；通知（无 id）答 202；不开 GET 事件流
// （405），引擎的 MCP 客户端接受纯 JSON 应答。工具调用的正文（参数、输出）不进日志（§15.1）。
// Agent远控（internal/hostagent）与创作工作空间（internal/studiomcp）各自只提供工具清单与
// 按令牌找会话的函数。
package mcpserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
)

const (
	protocolVersion = "2025-06-18"
	maxBody         = 8 << 20
)

// ToolDef 是 tools/list 里的一条工具描述。
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Catalog 是内嵌 tools.json 的形状：给引擎的一句总说明与工具清单。
type Catalog struct {
	Instructions string    `json:"instructions"`
	Tools        []ToolDef `json:"tools"`
}

// MustCatalog 解析内嵌的 tools.json；它是随二进制内嵌的常量，解析失败只能是编程错误。
func MustCatalog(raw []byte) Catalog {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		panic(err)
	}
	return c
}

// Content 是工具结果里的一块内容（text / image）。
type Content map[string]any

// Text 造一块文本内容。
func Text(s string) Content { return Content{"type": "text", "text": s} }

// Image 造一块图像内容（base64 正文 + MIME）。
func Image(data []byte, mimeType string) Content {
	return Content{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": mimeType}
}

// Result 是一次工具调用的结果。
type Result struct {
	Content []Content
	IsError bool
}

// TextResult 是只有一段文本的结果。
func TextResult(text string, isErr bool) Result {
	return Result{Content: []Content{Text(text)}, IsError: isErr}
}

// Session 是按令牌找到的一段引擎会话：只需要能执行工具调用。
type Session interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) Result
}

// Server 是一个 MCP 工具端点。
type Server struct {
	// Name 是 initialize 应答里的 serverInfo.name。
	Name string
	// Catalog 是工具清单与总说明。
	Catalog Catalog
	// Lookup 按承载令牌（不含 "Bearer "）找会话；nil 即未授权。
	Lookup func(token string) Session
}

type request struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var sess Session
	if ok && token != "" && s.Lookup != nil {
		sess = s.Lookup(token)
	}
	if sess == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		write(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// 通知：initialized、cancelled 之类，收下即可。
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		write(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": "1"},
			"instructions":    s.Catalog.Instructions,
		}, nil)
	case "ping":
		write(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := s.Catalog.Tools
		if tools == nil {
			tools = []ToolDef{}
		}
		write(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			write(w, req.ID, nil, &rpcError{Code: -32602, Message: "invalid params"})
			return
		}
		res := sess.CallTool(r.Context(), p.Name, p.Arguments)
		content := res.Content
		if content == nil {
			content = []Content{}
		}
		write(w, req.ID, map[string]any{"content": content, "isError": res.IsError}, nil)
	default:
		write(w, req.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
	}
}

func write(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	msg := map[string]any{"jsonrpc": "2.0"}
	if len(id) > 0 {
		msg["id"] = id
	} else {
		msg["id"] = nil
	}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	_ = json.NewEncoder(w).Encode(msg)
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DecodeArgs 把工具入参解成目标结构；空入参视为空对象。
func DecodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errArgs
	}
	return nil
}

type argsError struct{}

func (argsError) Error() string { return "arguments must be a JSON object" }

var errArgs error = argsError{}
