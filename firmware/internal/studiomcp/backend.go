// Package studiomcp 定义 Studio MCP 的工具契约与分发。
// 它通过 Session / Workspace 接口调用业务，不持有数据库、引擎、SSH 连接或媒体生成服务。
package studiomcp

import (
	"context"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
)

// Session 是令牌所绑定的创作会话。每次调用工具都重新取得工作空间，避免把已删除的工作空间
// 缓存进 MCP 端点。
type Session interface {
	Workspace(context.Context) (Workspace, error)
}

// Workspace 是一次工具请求使用的业务上下文。实现方负责路径校验、权限、
// 文件保护、生成准入、事件记录以及执行资源的生命周期。
type Workspace interface {
	ListFiles(context.Context, ListFilesArgs) mcpserve.Result
	ViewImage(context.Context, ViewImageArgs) mcpserve.Result
	ReadText(context.Context, PathArgs) mcpserve.Result
	WriteText(context.Context, WriteTextArgs) mcpserve.Result
	GenerateImage(context.Context, GenerateArgs) mcpserve.Result
	GenerateVideo(context.Context, GenerateArgs) mcpserve.Result
	DeleteFile(context.Context, PathArgs) mcpserve.Result
	RenameFile(context.Context, RenameFileArgs) mcpserve.Result
}

type ListFilesArgs struct {
	Dir  string `json:"dir"`
	Kind string `json:"kind"`
}

type PathArgs struct {
	Path string `json:"path"`
}

type ViewImageArgs struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

type WriteTextArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

// GenerateArgs 是两个生成工具共用的模型、文件输入、命名与参数形状。
type GenerateArgs struct {
	Prompt          string         `json:"prompt"`
	Model           string         `json:"model"`
	Name            string         `json:"name"`
	Operation       string         `json:"operation"`
	FirstFrame      string         `json:"first_frame"`
	LastFrame       string         `json:"last_frame"`
	ReferenceImages []string       `json:"reference_images"`
	SourceVideo     string         `json:"source_video"`
	Params          map[string]any `json:"params"`
	Count           int            `json:"count"`
}

type RenameFileArgs struct {
	Path    string `json:"path"`
	NewPath string `json:"new_path"`
}
