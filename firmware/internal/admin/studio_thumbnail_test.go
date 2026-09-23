package admin_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

func TestStudioThumbnailHTTP(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	sh := withStudio(t, e, root)
	resp := e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"preview","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	decodeInto(t, resp, &created)
	ws := created.Workspace
	base := "/admin/v1/workspaces/" + ws.ID + "/files"
	encodePNG := func(w, h int) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	type fileWithRevision struct {
		Name           string `json:"name"`
		HasThumb       bool   `json:"has_thumb"`
		SourceRevision string `json:"source_revision"`
	}
	upload := func(name string, data []byte) fileWithRevision {
		t.Helper()
		resp := e.uploadStudio(root, ws.ID, name, data)
		wantStatus(t, resp, http.StatusCreated)
		var result struct {
			File fileWithRevision `json:"file"`
		}
		decodeInto(t, resp, &result)
		if result.File.SourceRevision == "" {
			t.Fatal("上传响应缺少原件版本")
		}
		return result.File
	}
	checkJPEG := func(name string, wantW, wantH int) {
		t.Helper()
		resp := e.do("GET", base+"/"+name+"/thumb", root, "")
		wantStatus(t, resp, http.StatusOK)
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("缩略图响应类型 / 缓存策略 = %q / %q", resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
		}
		cfg, err := jpeg.DecodeConfig(resp.Body)
		if err != nil || cfg.Width != wantW || cfg.Height != wantH {
			t.Fatalf("%s 缩略图 = %d × %d / %v，期望 %d × %d", name, cfg.Width, cfg.Height, err, wantW, wantH)
		}
	}

	cover := upload("media/cover.png", encodePNG(640, 960))
	if !cover.HasThumb {
		t.Fatal("上传图像后应有缩略图")
	}
	if err := os.Remove(filepath.Join(sh.mgr.Root(), ws.ID, ".thumbs", "media", "cover.png.jpg")); err != nil {
		t.Fatal(err)
	}
	// 不重新列目录：GET 本身应从节点补回缩略图。
	checkJPEG(cover.Name, 480, 720)

	old := upload("media/clip.mp4", []byte("old fake video"))
	current := upload("media/clip.mp4", []byte("replacement fake video"))
	if old.SourceRevision == current.SourceRevision {
		t.Fatal("覆盖原件后版本应改变")
	}
	frame, err := json.Marshal(map[string]string{"image": base64.StdEncoding.EncodeToString(encodePNG(1920, 1080))})
	if err != nil {
		t.Fatal(err)
	}
	thumbURL := base + "/" + current.Name + "/thumb?v="
	resp = e.do("POST", thumbURL+url.QueryEscape(old.SourceRevision), root, string(frame))
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != studio.CodeFileChanged {
		t.Fatalf("旧封面的错误码 = %q", code)
	}
	resp = e.do("GET", base+"/"+current.Name+"/thumb", root, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
	resp = e.do("POST", thumbURL+url.QueryEscape(current.SourceRevision), root, string(frame))
	wantStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	checkJPEG(current.Name, 853, 480)

	var listed struct {
		Files []fileWithRevision `json:"files"`
	}
	resp = e.do("GET", base, root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &listed)
	for _, f := range listed.Files {
		if f.Name == current.Name {
			if !f.HasThumb || f.SourceRevision != current.SourceRevision {
				t.Fatalf("封面回传后的清单 = %+v", f)
			}
			return
		}
	}
	t.Fatal("清单中缺少视频")
}
