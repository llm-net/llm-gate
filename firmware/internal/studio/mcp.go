package studio

// Studio 会话与独立 MCP 工具层的装配：
//
//	POST /studio-mcp   Authorization: Bearer <会话令牌>
//
// 令牌只在一段引擎会话的生命周期里有效；远端地址必须是回环（节点上的引擎经 SSH 远程转发、
// 由设备自己的转发服务回到回环，internal/nodeengine）；路由由网关按 LANOnly 挂载。引擎在节点
// 上有自己的 shell，这里不再提供命令执行。

import (
	"context"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studiomcp"
)

// MCPHandler 是 /studio-mcp 的处理器。
func (m *Manager) MCPHandler() http.Handler {
	return studiomcp.NewHandler(func(token string) studiomcp.Session {
		s := m.sessionByToken(token)
		if s == nil {
			return nil
		}
		return s
	}, studiomcp.ModelDescriptions{
		Image: mediagen.DescribeKind(store.ModelKindImage),
		Video: mediagen.DescribeKind(store.ModelKindVideo),
	})
}

var _ studiomcp.Session = (*session)(nil)
var _ studiomcp.Workspace = (*mcpWorkspace)(nil)

// Workspace 为单次请求绑定当前工作空间，数据访问与会话资源留在业务层。
func (s *session) Workspace(ctx context.Context) (studiomcp.Workspace, error) {
	ws, err := s.m.st.GetWorkspace(ctx, s.wsID)
	if err != nil {
		return nil, err
	}
	return &mcpWorkspace{s: s, ws: ws}, nil
}

type mcpWorkspace struct {
	s  *session
	ws *store.Workspace
}

func (w *mcpWorkspace) ListFiles(ctx context.Context, a studiomcp.ListFilesArgs) mcpserve.Result {
	return w.s.listFiles(ctx, w.ws, a.Dir, a.Kind)
}

func (w *mcpWorkspace) ViewImage(ctx context.Context, a studiomcp.ViewImageArgs) mcpserve.Result {
	return w.s.viewImage(ctx, w.ws, a.Path, a.Detail)
}

func (w *mcpWorkspace) ReadText(ctx context.Context, a studiomcp.PathArgs) mcpserve.Result {
	return w.s.readText(ctx, w.ws, a.Path)
}

func (w *mcpWorkspace) WriteText(ctx context.Context, a studiomcp.WriteTextArgs) mcpserve.Result {
	return w.s.writeText(ctx, w.ws, a.Path, a.Content, a.Append)
}

func (w *mcpWorkspace) GenerateImage(ctx context.Context, a studiomcp.GenerateArgs) mcpserve.Result {
	return w.s.generate(ctx, w.ws, store.ModelKindImage, a)
}

func (w *mcpWorkspace) GenerateVideo(ctx context.Context, a studiomcp.GenerateArgs) mcpserve.Result {
	return w.s.generate(ctx, w.ws, store.ModelKindVideo, a)
}

func (w *mcpWorkspace) DeleteFile(ctx context.Context, a studiomcp.PathArgs) mcpserve.Result {
	return w.s.deleteFile(ctx, w.ws, a.Path)
}

func (w *mcpWorkspace) RenameFile(ctx context.Context, a studiomcp.RenameFileArgs) mcpserve.Result {
	return w.s.renameFile(ctx, w.ws, a.Path, a.NewPath)
}
func (m *Manager) sessionByToken(token string) *session {
	if token == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[token]
}
