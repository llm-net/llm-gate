package studio

// 创作工作空间的文件工具业务：路径、文件保护、图像读取与时间线。
// 工具清单、入参和分发在 internal/studiomcp；本包的装配在 mcp.go。
// 工具参数与输出不进日志（§15.1）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studiomcp"
)

const (
	// viewShortEdgeLow / High 是 view_image 交给模型的图像短边。
	viewShortEdgeLow  = 768
	viewShortEdgeHigh = 1536
	// viewJPEGQuality 是交给模型的图像（view_image 与生成结果预览）的 JPEG 质量。引擎
	// 每次请求都重发整段会话历史、图片也在其中，所以比目录缩略图的 85 低：768 短边的
	// 写实画面约小三分之一。
	viewJPEGQuality = 70
	// storeTextLimit 是写文件事件里留的内容头部上限。
	storeTextLimit = 16 << 10
)

// toolErr 把本包错误翻成给模型看的一句话。
func toolErr(err error) mcpserve.Result {
	var se *Error
	if errors.As(err, &se) {
		return mcpserve.TextResult(se.Msg, true)
	}
	return mcpserve.TextResult("the operation failed: "+err.Error(), true)
}

func (s *session) listFiles(ctx context.Context, ws *store.Workspace, dir, kind string) mcpserve.Result {
	if dir = strings.Trim(strings.TrimSpace(dir), "/"); dir != "" && !ValidDir(dir) {
		return mcpserve.TextResult("dir must be one of agent, docs, media", true)
	}
	files, err := s.m.ListFiles(ctx, ws)
	if err != nil {
		return toolErr(err)
	}
	var b strings.Builder
	n := 0
	for _, f := range files {
		if kind != "" && f.Kind != kind {
			continue
		}
		if d, _ := SplitPath(f.Name); dir != "" && d != dir {
			continue
		}
		n++
		fmt.Fprintf(&b, "- %s (%s, %s", f.Name, f.Kind, humanSize(f.Bytes))
		if f.Width > 0 {
			fmt.Fprintf(&b, ", %dx%d", f.Width, f.Height)
		}
		fmt.Fprintf(&b, ", %s", f.Origin)
		if f.Provider != "" {
			fmt.Fprintf(&b, ", %s/%s", f.Provider, f.Model)
		}
		if p := strings.TrimSpace(f.Prompt); p != "" {
			r := []rune(strings.ReplaceAll(p, "\n", " "))
			if len(r) > promptPromptRunes {
				r = append(r[:promptPromptRunes], '…')
			}
			fmt.Fprintf(&b, ", prompt: %s", string(r))
		}
		b.WriteString(")\n")
	}
	if n == 0 {
		return mcpserve.TextResult("(no files)", false)
	}
	return mcpserve.TextResult(fmt.Sprintf("%d file(s):\n%s", n, b.String()), false)
}

func (s *session) viewImage(ctx context.Context, ws *store.Workspace, name, detail string) mcpserve.Result {
	if FileKind(name) != store.StudioFileImage {
		return mcpserve.TextResult("not an image file (expected media/<name>.png|jpg|webp|gif): "+name, true)
	}
	// 设备解得开的图像整份读进来缩放（上限同缩略图源 thumbSourceLimit）；解不开的格式才受 8 MiB 原字节上限约束。
	raw, err := s.m.ReadFile(ctx, ws, name, thumbSourceLimit)
	if err != nil {
		return toolErr(err)
	}
	edge := viewShortEdgeLow
	if detail == "high" {
		edge = viewShortEdgeHigh
	}
	data, err := mediagen.MakeJPEG(bytes.NewReader(raw), edge, viewJPEGQuality)
	if err != nil {
		// 设备解不开的格式（WebP）：原字节直接交给模型（上限 8 MiB）。
		if len(raw) > MaxImageBytes {
			return mcpserve.TextResult(fmt.Sprintf("%s cannot be decoded on the device and exceeds the %d MiB limit for handing over raw bytes", name, MaxImageBytes>>20), true)
		}
		s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventTool, Title: studiomcp.ToolViewImage, Body: name})
		return mcpserve.Result{Content: []mcpserve.Content{mcpserve.Text(imageCaption(ctx, s, ws, name)), mcpserve.Image(raw, MimeFor(name))}}
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventTool, Title: studiomcp.ToolViewImage, Body: name})
	return mcpserve.Result{Content: []mcpserve.Content{mcpserve.Text(imageCaption(ctx, s, ws, name)), mcpserve.Image(data, "image/jpeg")}}
}

// imageCaption 是随图像一起交给模型的一句元数据。
func imageCaption(ctx context.Context, s *session, ws *store.Workspace, name string) string {
	row, err := s.m.st.GetStudioFile(ctx, ws.ID, name)
	if err != nil {
		return name
	}
	cap := fmt.Sprintf("%s (%s", name, humanSize(row.Bytes))
	if row.Width > 0 {
		cap += fmt.Sprintf(", %dx%d", row.Width, row.Height)
	}
	cap += ", " + row.Origin
	if row.Prompt != "" {
		cap += ", prompt: " + strings.ReplaceAll(strings.TrimSpace(row.Prompt), "\n", " ")
	}
	return cap + ")"
}

func (s *session) readText(ctx context.Context, ws *store.Workspace, name string) mcpserve.Result {
	if FileKind(name) != store.StudioFileText {
		return mcpserve.TextResult("not a text file: "+name, true)
	}
	data, err := s.m.ReadFile(ctx, ws, name, MaxTextBytes)
	if err != nil {
		return toolErr(err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return mcpserve.TextResult("the file is binary", true)
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventTool, Title: studiomcp.ToolReadText, Body: name})
	if len(data) == 0 {
		return mcpserve.TextResult("(the file is empty)", false)
	}
	return mcpserve.TextResult(string(data), false)
}

func (s *session) writeText(ctx context.Context, ws *store.Workspace, name, content string, appendTo bool) mcpserve.Result {
	dir, _, err := ValidatePath(name)
	if err != nil {
		return toolErr(err)
	}
	if FileKind(name) != store.StudioFileText {
		return mcpserve.TextResult("write_text only writes text files (.md, .txt, .json, .csv, .srt …): "+name, true)
	}
	if !DirAllows(dir, store.StudioFileText) {
		return mcpserve.TextResult("text files go under agent/ (your own notes) or docs/ (deliverables), not "+dir+"/: "+name, true)
	}
	if len(content) > MaxTextBytes {
		return mcpserve.TextResult(fmt.Sprintf("content exceeds the %d KiB limit", MaxTextBytes>>10), true)
	}
	full := content
	if appendTo {
		prev, err := s.m.ReadFile(ctx, ws, name, MaxTextBytes)
		if err != nil {
			var se *Error
			if !errors.As(err, &se) || se.Code != CodeFileNotFound {
				return toolErr(err)
			}
		}
		if len(prev) > 0 && !bytes.HasSuffix(prev, []byte("\n")) {
			prev = append(prev, '\n')
		}
		full = string(prev) + content
		if len(full) > MaxTextBytes {
			return mcpserve.TextResult(fmt.Sprintf("the file would exceed the %d KiB limit", MaxTextBytes>>10), true)
		}
	}
	s.setActivity(nil, "写文件："+name)
	defer s.setActivity(nil, "")
	// 管理员上传的素材不由智能体覆盖：只允许写它自己建的 / 生成的文本文件与还不存在的文件。
	if row, err := s.m.st.GetStudioFile(ctx, ws.ID, name); err == nil && row.Origin == store.StudioFileOriginUpload && !appendTo {
		return mcpserve.TextResult("refusing to overwrite a file uploaded by the administrator: "+name+" (write a new file instead)", true)
	}
	f, err := s.m.SaveFile(ctx, ws, name, strings.NewReader(full), MaxTextBytes, FileMeta{Origin: store.StudioFileOriginAgent, ChatID: s.chatID, RunID: s.currentRunID()})
	if err != nil {
		return toolErr(err)
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventFile, Title: name, Body: head(content, storeTextLimit),
		Meta: metaJSON(map[string]any{"action": "write", "bytes": f.Bytes, "append": appendTo})})
	return mcpserve.TextResult(fmt.Sprintf("wrote %s (%d bytes)", name, f.Bytes), false)
}

func (s *session) deleteFile(ctx context.Context, ws *store.Workspace, name string) mcpserve.Result {
	if _, _, err := ValidatePath(name); err != nil {
		return toolErr(err)
	}
	if row, err := s.m.st.GetStudioFile(ctx, ws.ID, name); err == nil && row.Origin == store.StudioFileOriginUpload {
		return mcpserve.TextResult("refusing to delete a file uploaded by the administrator: "+name, true)
	}
	if err := s.m.DeleteFile(ctx, ws, name); err != nil {
		return toolErr(err)
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventFile, Title: name, Meta: metaJSON(map[string]any{"action": "delete"})})
	return mcpserve.TextResult("deleted "+name, false)
}

func (s *session) renameFile(ctx context.Context, ws *store.Workspace, from, to string) mcpserve.Result {
	if err := s.m.RenameFile(ctx, ws, from, to); err != nil {
		return toolErr(err)
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: s.currentRunID(), Kind: store.StudioEventFile, Title: to, Meta: metaJSON(map[string]any{"action": "rename", "from": from})})
	return mcpserve.TextResult(fmt.Sprintf("renamed %s to %s", from, to), false)
}

// head 保留开头 n 字节（按 UTF-8 边界）。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for i := len(cut) - 1; i >= 0 && i > len(cut)-4; i-- {
		if cut[i]&0xC0 != 0x80 {
			cut = cut[:i]
			break
		}
	}
	return cut + "\n…(rest truncated)"
}
