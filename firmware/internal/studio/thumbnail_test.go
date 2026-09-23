package studio_test

import (
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

func studioThumbDimensions(t *testing.T, e *env, name string) (int, int) {
	t.Helper()
	thumb, err := e.m.OpenThumb(e.ws, name)
	if err != nil {
		t.Fatalf("OpenThumb(%s): %v", name, err)
	}
	defer thumb.Close()
	if thumb.ContentType != "image/jpeg" {
		t.Fatalf("缩略图类型 = %q", thumb.ContentType)
	}
	cfg, err := jpeg.DecodeConfig(thumb.Content)
	if err != nil {
		t.Fatalf("缩略图不是 JPEG: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestThumbnailListRepairsMissingCache(t *testing.T) {
	for _, where := range []string{"device", "host"} {
		t.Run(where, func(t *testing.T) {
			var e *env
			if where == "host" {
				e = newHostEnv(t).env
			} else {
				e = newDeviceEnv(t)
			}
			f := e.upload("media/cover.png", pngBytes(t, 1600, 960))
			if f.Width != 1600 || f.Height != 960 {
				t.Fatalf("原图尺寸 = %d × %d", f.Width, f.Height)
			}
			cache := filepath.Join(e.m.Dir(e.ws), ".thumbs", "media", "cover.png.jpg")
			if err := os.Remove(cache); err != nil {
				t.Fatal(err)
			}
			if _, err := e.m.ListFiles(context.Background(), e.ws); err != nil {
				t.Fatal(err)
			}
			if w, h := studioThumbDimensions(t, e, f.Name); w != 800 || h != 480 {
				t.Fatalf("补回的缩略图 = %d × %d，期望 800 × 480", w, h)
			}
		})
	}
}

func TestThumbnailOnDemandBeyondListBudget(t *testing.T) {
	h := newHostEnv(t)
	e := h.env
	ctx := context.Background()
	if _, err := e.m.ListFiles(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	// 主机直接产出的坏图排在前面，清单的八张处理预算不应阻止后面的预览请求。
	for i := range 8 {
		p := filepath.Join(h.hostDir, "media", fmt.Sprintf("a-%02d.png", i))
		if err := os.WriteFile(p, []byte("invalid image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name := "media/z-cover.png"
	if err := os.WriteFile(filepath.Join(h.hostDir, filepath.FromSlash(name)), pngBytes(t, 960, 1600), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ListFiles(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	if err := e.m.EnsureThumb(ctx, e.ws, name); err != nil {
		t.Fatalf("按需生成预算之外的缩略图: %v", err)
	}
	if w, h := studioThumbDimensions(t, e, name); w != 480 || h != 800 {
		t.Fatalf("按需缩略图 = %d × %d，期望 480 × 800", w, h)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, ".thumbs")); !os.IsNotExist(err) {
		t.Fatalf("缩略图不得缓存在工作节点，stat: %v", err)
	}
	// 已有缓存的请求不需要重新读取原图。
	if err := e.m.EnsureThumb(ctx, e.ws, name); err != nil {
		t.Fatalf("复用已有缩略图: %v", err)
	}
}

func TestThumbnailVideoFrameRevision(t *testing.T) {
	e := newDeviceEnv(t)
	ctx := context.Background()
	name := "media/clip.mp4"
	old := e.upload(name, []byte("old fake video"))
	oldRevision := studio.SourceRevision(*old)
	frame := pngBytes(t, 1920, 1080)
	if err := e.m.SaveThumb(ctx, e.ws, name, frame, oldRevision); err != nil {
		t.Fatalf("回传封面: %v", err)
	}
	if w, h := studioThumbDimensions(t, e, name); w != 853 || h != 480 {
		t.Fatalf("视频封面 = %d × %d，期望 853 × 480", w, h)
	}
	current := e.upload(name, []byte("replacement fake video"))
	if e.m.HasThumb(e.ws, name) {
		t.Fatal("覆盖视频后不能保留旧封面")
	}
	err := e.m.SaveThumb(ctx, e.ws, name, frame, oldRevision)
	var se *studio.Error
	if !errors.As(err, &se) || se.Code != studio.CodeFileChanged {
		t.Fatalf("旧版本封面应被拒绝: %v", err)
	}
	if e.m.HasThumb(e.ws, name) {
		t.Fatal("被拒绝的旧封面不能写入缓存")
	}
	if err := e.m.SaveThumb(ctx, e.ws, name, pngBytes(t, 300, 200), studio.SourceRevision(*current)); err != nil {
		t.Fatalf("新版本封面: %v", err)
	}
	if w, h := studioThumbDimensions(t, e, name); w != 300 || h != 200 {
		t.Fatalf("小尺寸封面不应放大: %d × %d", w, h)
	}
}

func TestThumbnailWriteFailure(t *testing.T) {
	e := newDeviceEnv(t)
	ctx := context.Background()
	name := "media/clip.mp4"
	f := e.upload(name, []byte("fake video"))
	// 用普通文件占住缓存目录，在 root 下也能稳定制造写失败。
	cache := filepath.Join(e.m.Dir(e.ws), ".thumbs")
	if err := os.WriteFile(cache, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	frame := pngBytes(t, 640, 480)
	if err := e.m.SaveThumb(ctx, e.ws, name, frame, studio.SourceRevision(*f)); err == nil {
		t.Fatal("缩略图写入失败不能报告成功")
	}
	if e.m.HasThumb(e.ws, name) {
		t.Fatal("写入失败不应有缩略图")
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	if err := e.m.SaveThumb(ctx, e.ws, name, frame); err != nil {
		t.Fatalf("缓存恢复后应能重试，无 revision 的调用仍可用: %v", err)
	}
	if w, h := studioThumbDimensions(t, e, name); w != 640 || h != 480 {
		t.Fatalf("重试后的缩略图 = %d × %d", w, h)
	}
}

func TestThumbnailRejectsUnsupportedFiles(t *testing.T) {
	e := newDeviceEnv(t)
	ctx := context.Background()
	for _, name := range []string{"docs/notes.txt", "media/voice.mp3", "media/broken.png"} {
		f := e.upload(name, []byte("not an image"))
		err := e.m.EnsureThumb(ctx, e.ws, name)
		var se *studio.Error
		if !errors.As(err, &se) || se.Code != studio.CodeFileNotFound {
			t.Fatalf("EnsureThumb(%s) 应没有预览: %v", name, err)
		}
		if name != "media/broken.png" {
			if err := e.m.SaveThumb(ctx, e.ws, name, e.png, studio.SourceRevision(*f)); err == nil {
				t.Fatalf("%s 不应接受图像封面", name)
			}
		}
	}
}
