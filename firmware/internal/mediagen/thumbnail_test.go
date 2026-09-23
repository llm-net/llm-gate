package mediagen

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// 缩略图短边恒为 ThumbShortEdge，长边按比例；源图短边不足时不放大；输出是 JPEG。
func TestMakeThumbShortEdge(t *testing.T) {
	cases := []struct{ w, h, wantW, wantH int }{
		{1600, 960, 800, 480},
		{960, 1600, 480, 800},
		{2048, 2048, 480, 480},
		{300, 200, 300, 200},
		{480, 720, 480, 720},
	}
	for _, c := range cases {
		out, err := makeThumb(bytes.NewReader(encodePNG(t, c.w, c.h)))
		if err != nil {
			t.Fatalf("%d×%d makeThumb: %v", c.w, c.h, err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("%d×%d 输出不是 JPEG: %v", c.w, c.h, err)
		}
		if cfg.Width != c.wantW || cfg.Height != c.wantH {
			t.Fatalf("%d×%d 缩略图 = %d×%d，期望 %d×%d", c.w, c.h, cfg.Width, cfg.Height, c.wantW, c.wantH)
		}
	}
}

// MakeJPEG 与 MakeThumb 同一套缩放规则，只换 JPEG 质量；越界的质量按缩略图质量。
func TestMakeJPEGQuality(t *testing.T) {
	src := encodePNG(t, 1024, 1536)
	thumb, err := MakeThumb(bytes.NewReader(src), 768)
	if err != nil {
		t.Fatal(err)
	}
	low, err := MakeJPEG(bytes.NewReader(src), 768, 60)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(low))
	if err != nil || cfg.Width != 768 || cfg.Height != 1152 {
		t.Fatalf("MakeJPEG 输出 = %+v / %v，期望 768×1152 的 JPEG", cfg, err)
	}
	if len(low) >= len(thumb) {
		t.Fatalf("质量 60 的输出 %d 字节不小于缩略图质量的 %d 字节", len(low), len(thumb))
	}
	for _, q := range []int{0, 101} {
		out, err := MakeJPEG(bytes.NewReader(src), 768, q)
		if err != nil || !bytes.Equal(out, thumb) {
			t.Fatalf("质量 %d 应按缩略图质量编码", q)
		}
	}
}

// 解不开的字节（视频、WebP、损坏文件）报 errThumbUndecodable，不 panic。
func TestMakeThumbRejectsUndecodable(t *testing.T) {
	if _, err := makeThumb(strings.NewReader("ABC")); err == nil {
		t.Fatal("非图像字节应报错")
	}
}

// 透明像素合成到白底：JPEG 没有 alpha，不能变成黑底。
func TestMakeThumbFlattensToWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, err := makeThumb(&buf)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := dec.At(4, 4).RGBA()
	if r < 0xf000 || g < 0xf000 || b < 0xf000 {
		t.Fatalf("透明像素应合成到白底，得到 %#x %#x %#x", r, g, b)
	}
}

// dataURIBackend 是立刻回内联图像结果的假 Codex 后端。
func dataURIBackend(uri string) *fakeBackend {
	b := &fakeBackend{name: BackendCodex}
	b.generate = func(context.Context, Request) (Result, error) {
		return Result{Status: store.MediaStatusSucceeded, MediaURL: uri, MediaType: "image"}, nil
	}
	return b
}

func thumbSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("缩略图文件: %v", err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("缩略图不是 JPEG: %v", err)
	}
	return cfg.Width, cfg.Height
}

// 图像结果落盘时同时生成短边 480 的 JPEG 缩略图；行里 data URI 去掉、只剩两个文件名。
func TestPersistImageGeneratesThumb(t *testing.T) {
	s, _, dir := newDiskService(t, nil, dataURIBackend("data:image/png;base64,"+base64.StdEncoding.EncodeToString(encodePNG(t, 1200, 800))))
	task := submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p"})[0]
	got := waitTerminal(t, s, task.ID)
	if got.Status != "succeeded" || got.MediaFile != task.ID+".png" || got.ThumbFile != task.ID+ThumbExt || got.MediaURL != "" {
		t.Fatalf("任务 = %+v", got)
	}
	if w, h := thumbSize(t, filepath.Join(dir, got.ThumbFile)); w != 720 || h != 480 {
		t.Fatalf("缩略图 = %d×%d，期望 720×480", w, h)
	}
	m, err := s.OpenThumb(got)
	if err != nil {
		t.Fatalf("OpenThumb: %v", err)
	}
	m.Close()
	if m.ContentType != "image/jpeg" || m.Filename != got.ThumbFile {
		t.Fatalf("OpenThumb = %+v", m)
	}
	// 删除任务连带删掉缩略图。
	if err := s.Delete(context.Background(), task.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("删除后目录仍有 %d 个文件", len(entries))
	}
}

// 启动时把仍以 data URI 留在行里的旧图像结果补成本地文件与缩略图，已落盘但没有
// 缩略图的图像补生成缩略图；视频不在此列。
func TestMaterializeUpgradesLegacyRows(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mediaDir := filepath.Join(dir, MediaDirName)
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	legacy := store.MediaJob{ID: "LEGACY0000000000000000000A", Backend: BackendCodex, Kind: "image", Model: "m", Prompt: "p", Status: "succeeded",
		MediaURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(encodePNG(t, 600, 1200)), MediaType: "image", CreatedAt: now, UpdatedAt: now}
	onDisk := store.MediaJob{ID: "LEGACY0000000000000000000B", Backend: BackendGrok, Kind: "image", Model: "m", Prompt: "p", Status: "succeeded",
		MediaFile: "LEGACY0000000000000000000B.png", MediaType: "image", CreatedAt: now, UpdatedAt: now}
	video := store.MediaJob{ID: "LEGACY0000000000000000000C", Backend: BackendGrok, Kind: "video", Model: "m", Prompt: "p", Status: "succeeded",
		MediaFile: "LEGACY0000000000000000000C.mp4", MediaType: "video", CreatedAt: now, UpdatedAt: now}
	for _, row := range []store.MediaJob{legacy, onDisk, video} {
		if err := st.CreateMediaJob(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mediaDir, onDisk.MediaFile), encodePNG(t, 1000, 500), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, video.MediaFile), []byte("notavideo"), 0o600); err != nil {
		t.Fatal(err)
	}
	New(st, slog.New(slog.DiscardHandler), nil, dir)
	waitUntil(t, func() bool {
		a, err1 := st.GetMediaJob(context.Background(), legacy.ID)
		b, err2 := st.GetMediaJob(context.Background(), onDisk.ID)
		return err1 == nil && err2 == nil && a.ThumbFile != "" && b.ThumbFile != ""
	})
	a, _ := st.GetMediaJob(context.Background(), legacy.ID)
	if a.MediaURL != "" || a.MediaFile != legacy.ID+".png" || a.ThumbFile != legacy.ID+ThumbExt {
		t.Fatalf("data URI 行补齐后 = %+v", a)
	}
	if w, h := thumbSize(t, filepath.Join(mediaDir, a.ThumbFile)); w != 480 || h != 960 {
		t.Fatalf("竖图缩略图 = %d×%d，期望 480×960", w, h)
	}
	b, _ := st.GetMediaJob(context.Background(), onDisk.ID)
	if w, h := thumbSize(t, filepath.Join(mediaDir, b.ThumbFile)); w != 960 || h != 480 {
		t.Fatalf("横图缩略图 = %d×%d，期望 960×480", w, h)
	}
	c, _ := st.GetMediaJob(context.Background(), video.ID)
	if c.ThumbFile != "" {
		t.Fatalf("视频不该在启动时生成缩略图：%+v", c)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, video.MediaFile)); err != nil {
		t.Fatalf("视频文件不该被当孤儿清掉: %v", err)
	}
}

// 页面回传封面帧：只在成功且没有缩略图时接收，字节重新解码缩放后保存；再次回传
// 不覆盖；不是图像的字节报 *InvalidError；未成功的任务报 ErrMediaMissing。
func TestSaveThumb(t *testing.T) {
	s, st, dir := newDiskService(t, nil)
	now := time.Now()
	video := store.MediaJob{ID: "VIDEO00000000000000000000A", Backend: BackendGrok, Kind: "video", Model: "m", Prompt: "p", Status: "succeeded",
		MediaFile: "VIDEO00000000000000000000A.mp4", MediaType: "video", CreatedAt: now, UpdatedAt: now}
	running := store.MediaJob{ID: "VIDEO00000000000000000000B", Backend: BackendGrok, Kind: "video", Model: "m", Prompt: "p", Status: "running", CreatedAt: now, UpdatedAt: now}
	for _, row := range []store.MediaJob{video, running} {
		if err := st.CreateMediaJob(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, video.MediaFile), []byte("notavideo"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.SaveThumb(context.Background(), video.ID, encodePNG(t, 1920, 1080))
	if err != nil {
		t.Fatalf("SaveThumb: %v", err)
	}
	if got.ThumbFile != video.ID+ThumbExt {
		t.Fatalf("SaveThumb 行 = %+v", got)
	}
	if w, h := thumbSize(t, filepath.Join(dir, got.ThumbFile)); w != 853 || h != 480 {
		t.Fatalf("封面缩略图 = %d×%d，期望 853×480", w, h)
	}
	first, _ := os.Stat(filepath.Join(dir, got.ThumbFile))
	again, err := s.SaveThumb(context.Background(), video.ID, encodePNG(t, 100, 100))
	if err != nil || again.ThumbFile != got.ThumbFile {
		t.Fatalf("重复回传 = %+v / %v", again, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, got.ThumbFile)); info.Size() != first.Size() {
		t.Fatal("重复回传不该覆盖已有缩略图")
	}
	var invalid *InvalidError
	if _, err := s.SaveThumb(context.Background(), running.ID, encodePNG(t, 10, 10)); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("未成功任务 = %v，期望 ErrMediaMissing", err)
	}
	if _, err := s.SaveThumb(context.Background(), "NOPE", encodePNG(t, 10, 10)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在任务 = %v，期望 ErrNotFound", err)
	}
	if _, err := s.SaveThumb(context.Background(), video.ID, []byte("junk")); err != nil && !errors.As(err, &invalid) {
		t.Fatalf("已有缩略图时 junk = %v", err)
	}
	// 没有缩略图的任务收到非图像字节：InvalidError。
	if err := os.Remove(filepath.Join(dir, got.ThumbFile)); err != nil {
		t.Fatal(err)
	}
	got.ThumbFile = ""
	if err := st.UpdateMediaJob(context.Background(), *got); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveThumb(context.Background(), video.ID, []byte("junk")); !errors.As(err, &invalid) {
		t.Fatalf("非图像字节 = %v，期望 *InvalidError", err)
	}
}
