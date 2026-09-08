package gatehelper

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func request(t *testing.T, method, pattern, target, name string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.SetPathValue("name", name)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, r)
	return w
}

func TestReleaseManifestMatchesEmbeddedAssets(t *testing.T) {
	b, err := files.ReadFile("assets/stable.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion int    `json:"schema_version"`
		Version       string `json:"version"`
		Assets        []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil || manifest.SchemaVersion != 1 || manifest.Version == "" || len(manifest.Assets) != 6 {
		t.Fatalf("manifest: %v %+v", err, manifest)
	}
	sumsBody, err := files.ReadFile("assets/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range manifest.Assets {
		body, err := files.ReadFile("assets/" + asset.Name)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != asset.Size {
			t.Errorf("%s size=%d want=%d", asset.Name, len(body), asset.Size)
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(body))
		if sum != asset.SHA256 {
			t.Errorf("%s sha256=%s want=%s", asset.Name, sum, asset.SHA256)
		}
		if !bytes.Contains(sumsBody, []byte(asset.SHA256+"  "+asset.Name+"\n")) {
			t.Errorf("SHA256SUMS missing %s", asset.Name)
		}
		binary, err := archiveBinary(asset.Name, body)
		if err != nil {
			t.Errorf("%s: %v", asset.Name, err)
			continue
		}
		if !bytes.Contains(binary, []byte(manifest.Version)) {
			t.Errorf("%s binary does not contain manifest version %q", asset.Name, manifest.Version)
		}
	}
}

func archiveBinary(name string, body []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".tar.gz"):
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				return nil, err
			}
			if filepath.Base(h.Name) == "gate" {
				return io.ReadAll(tr)
			}
		}
	case strings.HasSuffix(name, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) != "gate.exe" {
				continue
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}
		return nil, fmt.Errorf("archive has no gate.exe")
	default:
		return nil, fmt.Errorf("unknown archive format")
	}
}

func TestHandlerGETHEADRangeAndRejections(t *testing.T) {
	get := request(t, http.MethodGet, "", "/gate-helper/releases/stable.json", "stable.json", nil)
	if get.Code != http.StatusOK || get.Header().Get("ETag") == "" || !strings.Contains(get.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("GET=%d headers=%v", get.Code, get.Header())
	}
	head := request(t, http.MethodHead, "", "/gate-helper/releases/stable.json", "stable.json", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatalf("HEAD=%d len=%d headers=%v", head.Code, head.Body.Len(), head.Header())
	}
	rangeResp := request(t, http.MethodGet, "", "/gate-helper/releases/gate-linux-amd64.tar.gz", "gate-linux-amd64.tar.gz", map[string]string{"Range": "bytes=0-15"})
	if rangeResp.Code != http.StatusPartialContent || rangeResp.Body.Len() != 16 {
		t.Fatalf("Range=%d len=%d", rangeResp.Code, rangeResp.Body.Len())
	}
	for _, name := range []string{"../stable.json", "gate-plan9-amd64.tar.gz", "", "gate-linux-amd64.tar.gz/extra"} {
		if got := request(t, http.MethodGet, "", "/gate-helper/releases/"+name, name, nil); got.Code != http.StatusNotFound {
			t.Errorf("name=%q status=%d", name, got.Code)
		}
	}
	post := request(t, http.MethodPost, "", "/gate-helper/install.sh", "install.sh", nil)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST=%d", post.Code)
	}
}
