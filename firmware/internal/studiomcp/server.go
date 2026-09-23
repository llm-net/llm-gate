package studiomcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
)

// NewHandler 构造只接受回环请求与有效会话令牌的 MCP 端点。
// lookup 由业务会话管理器提供，令牌失效时必须返回 nil；端点不缓存认证结果。
// HTTP / JSON-RPC 与请求大小限制由公共 mcpserve 骨架负责。
func NewHandler(lookup func(string) Session, descriptions ModelDescriptions) http.Handler {
	catalog := newCatalog(descriptions)
	return &mcpserve.Server{Name: "llmgate-studio", Catalog: catalog, Lookup: func(token string) mcpserve.Session {
		if lookup == nil {
			return nil
		}
		backend := lookup(token)
		if backend == nil {
			return nil
		}
		return &session{backend: backend, catalog: catalog}
	}}
}

type session struct {
	backend Session
	catalog mcpserve.Catalog
}

var _ mcpserve.Session = (*session)(nil)

func (s *session) CallTool(ctx context.Context, name string, args json.RawMessage) mcpserve.Result {
	ws, err := s.backend.Workspace(ctx)
	if err != nil {
		return mcpserve.TextResult("the workspace no longer exists", true)
	}
	switch name {
	case ToolListFiles:
		return call(ctx, args, ws.ListFiles)
	case ToolViewImage:
		return call(ctx, args, ws.ViewImage)
	case ToolReadText:
		return call(ctx, args, ws.ReadText)
	case ToolWriteText:
		return call(ctx, args, ws.WriteText)
	case ToolGenerateImage:
		return call(ctx, args, ws.GenerateImage)
	case ToolGenerateVideo:
		return call(ctx, args, ws.GenerateVideo)
	case ToolDeleteFile:
		return call(ctx, args, ws.DeleteFile)
	case ToolRenameFile:
		return call(ctx, args, ws.RenameFile)
	default:
		return mcpserve.TextResult("unknown tool: "+name, true)
	}
}

func call[A any](ctx context.Context, raw json.RawMessage, fn func(context.Context, A) mcpserve.Result) mcpserve.Result {
	var args A
	if err := mcpserve.DecodeArgs(raw, &args); err != nil {
		return mcpserve.TextResult(err.Error(), true)
	}
	return fn(ctx, args)
}
