package studio

import (
	"context"
	"fmt"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// CodeFileChanged 表示抓取封面期间原件已被替换。
const CodeFileChanged = "studio_file_changed"

// SourceRevision 与目录对账使用相同的字节数、毫秒修改时刻；补尺寸和预览图不改变它。
func SourceRevision(f store.StudioFile) string {
	return fmt.Sprintf("%x-%x", f.Bytes, fileTime(f.ModTime).UnixMilli())
}

func thumbSourceEntry(ctx context.Context, st Storage, p string) (Entry, error) {
	dir, name := SplitPath(p)
	entries, err := st.List(ctx, dir)
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if entry.Name == name {
			return entry, nil
		}
	}
	return Entry{}, &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
}

// EnsureThumb 在读取预览图时按需补齐设备能解码的图像，不受列目录的分析张数限制。
// 与上传/生成共用 MakeThumb：480 短边、等比不裁切、不放大小图、透明合成白底、JPEG 85。
// 视频或无法解码的图像回没有缩略图，由浏览器补封面后交 SaveThumb 再校验编码。
func (m *Manager) EnsureThumb(ctx context.Context, ws *store.Workspace, p string) error {
	if _, _, err := ValidatePath(p); err != nil {
		return err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if m.HasThumb(ws, p) {
		return nil
	}
	if FileKind(p) == store.StudioFileImage {
		st, err := m.storage(ws)
		if err != nil {
			return err
		}
		entry, err := thumbSourceEntry(ctx, st, p)
		if err != nil {
			return err
		}
		m.analyzeImage(ctx, ws, st, p, entry.Size)
		if m.HasThumb(ws, p) {
			return nil
		}
	}
	return &Error{Code: CodeFileNotFound, Msg: "没有缩略图"}
}
