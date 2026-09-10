package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func gateTarGZ(t *testing.T, binary []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "gate", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestGateUpdateUsesSavedDeviceAndAtomicallyReplacesBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture and synchronous replacement")
	}
	const oldVersion = "2608250130-a680"
	const newVersion = "2608250245-a681"
	assetName, err := gateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	newBinary := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + newVersion + "'; exit 0; fi\nexit 0\n")
	archive := gateTarGZ(t, newBinary)
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	manifest, err := json.Marshal(gateReleaseManifest{
		SchemaVersion: 1,
		Version:       newVersion,
		Assets:        []gateReleaseAsset{{Name: assetName, SHA256: digest, Size: int64(len(archive))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var manifestRequests atomic.Int32
	var archiveRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anonymous gate update request carried Authorization")
		}
		switch r.URL.Path {
		case selfUpdateManifestPath:
			if r.Header.Get("Cache-Control") != "no-cache" {
				t.Errorf("manifest request Cache-Control=%q", r.Header.Get("Cache-Control"))
			}
			manifestRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifest)
		case "/gate-helper/releases/" + assetName:
			archiveRequests.Add(1)
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	target := filepath.Join(dir, "gate")
	oldBinary := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + oldVersion + "'; exit 0; fi\nexit 0\n")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	warnings := &bytes.Buffer{}
	a := &app{root: t.TempDir(),
		cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey},
		hc:  newHTTPClient(), out: out, err: warnings,
	}
	if err := a.selfUpdateAt(target, runtime.GOOS, runtime.GOARCH, oldVersion); err != nil {
		t.Fatal(err)
	}
	if manifestRequests.Load() != 1 || archiveRequests.Load() != 1 {
		t.Fatalf("requests manifest=%d archive=%d", manifestRequests.Load(), archiveRequests.Load())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBinary) {
		t.Fatal("gate target was not replaced with the verified binary")
	}
	if output := out.String(); !strings.Contains(output, oldVersion+" → "+newVersion) || !strings.Contains(output, "gate 已升级到 "+newVersion) {
		t.Fatalf("output=%q", output)
	}
	if !strings.Contains(warnings.String(), "没有 TLS 保护") {
		t.Fatalf("HTTP update warning=%q", warnings.String())
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".gate-update-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("upgrade leftovers=%v err=%v", leftovers, err)
	}
}

func TestGateUpdateEqualVersionDoesNotDownload(t *testing.T) {
	const current = "2608250245-a681"
	assetName, err := gateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(gateReleaseManifest{
		SchemaVersion: 1, Version: current,
		Assets: []gateReleaseAsset{{Name: assetName, SHA256: strings.Repeat("a", 64), Size: 1}},
	})
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == selfUpdateManifestPath {
			_, _ = w.Write(manifest)
			return
		}
		downloads.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	out := &bytes.Buffer{}
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL}, hc: newHTTPClient(), out: out, err: &bytes.Buffer{}}
	if err := a.selfUpdateAt(filepath.Join(t.TempDir(), "does-not-need-to-exist"), runtime.GOOS, runtime.GOARCH, current); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 0 || !strings.Contains(out.String(), "已是最新版本") {
		t.Fatalf("downloads=%d output=%q", downloads.Load(), out.String())
	}
}

func TestGateUpdateBadDigestKeepsCurrentBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	const oldVersion = "2608250130-a680"
	const newVersion = "2608250245-a681"
	assetName, err := gateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := gateTarGZ(t, []byte("#!/bin/sh\necho '"+newVersion+"'\n"))
	manifest, _ := json.Marshal(gateReleaseManifest{
		SchemaVersion: 1, Version: newVersion,
		Assets: []gateReleaseAsset{{Name: assetName, SHA256: strings.Repeat("0", 64), Size: int64(len(archive))}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == selfUpdateManifestPath {
			_, _ = w.Write(manifest)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	target := filepath.Join(dir, "gate")
	oldBinary := []byte("#!/bin/sh\necho '" + oldVersion + "'\n")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.selfUpdateAt(target, runtime.GOOS, runtime.GOARCH, oldVersion); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("bad digest error=%v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, oldBinary) {
		t.Fatalf("current binary changed after rejected update: err=%v", err)
	}
}

func TestGateUpdateVersionMismatchKeepsCurrentBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	const oldVersion = "2608250130-a680"
	const newVersion = "2608250245-a681"
	assetName, err := gateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := gateTarGZ(t, []byte("#!/bin/sh\necho '2608250312-a682'\n"))
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	manifest, _ := json.Marshal(gateReleaseManifest{
		SchemaVersion: 1, Version: newVersion,
		Assets: []gateReleaseAsset{{Name: assetName, SHA256: digest, Size: int64(len(archive))}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == selfUpdateManifestPath {
			_, _ = w.Write(manifest)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	target := filepath.Join(dir, "gate")
	oldBinary := []byte("#!/bin/sh\necho '" + oldVersion + "'\n")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.selfUpdateAt(target, runtime.GOOS, runtime.GOARCH, oldVersion); err == nil || !strings.Contains(err.Error(), "自述版本") {
		t.Fatalf("version mismatch error=%v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, oldBinary) {
		t.Fatalf("current binary changed after rejected version: err=%v", err)
	}
}

func TestRunGateUpdateRequiresSavedDevice(t *testing.T) {
	t.Setenv("GATE_CONFIG_DIR", t.TempDir())
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"update"}, strings.NewReader(""), out, errOut); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "gate -url <设备地址>") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"update", "extra"}, strings.NewReader(""), out, errOut); code != 2 || !strings.Contains(errOut.String(), "用法: gate update") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestNewerGateReleaseUsesFirmwareTimestampOrdering(t *testing.T) {
	tests := []struct {
		candidate, current string
		want               bool
		wantErr            bool
	}{
		{"2608250245-a681", "2608250130-a680", true, false},
		{"2608250245-a681-d", "2608250245-a680", false, false}, // 同一分钟：哈希与脏标记都不参与比较
		{"2608250130-a680", "2608250245-a681", false, false},
		{"2608250131-a680", "2608250130-a681", true, false}, // 分钟是最小可分辨粒度
		{"2608250245-a681", "dev", true, false},
		{"latest", "2608250130-a680", false, true},
		{"2608250245-a681", "custom", false, true},
		{"26082502-a680", "2608250201-a681", false, true}, // 八位名称不是本产品的版本形状
	}
	for _, tc := range tests {
		got, err := newerGateRelease(tc.candidate, tc.current)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("newerGateRelease(%q,%q)=(%v,%v), want (%v, err=%v)", tc.candidate, tc.current, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestExtractGateWindowsZipIsStrict(t *testing.T) {
	makeZip := func(entries map[string]string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "gate.zip")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(file)
		for name, body := range entries {
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(0o755)
			entry, err := zw.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = entry.Write([]byte(body))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var got bytes.Buffer
	if err := extractGateBinary(makeZip(map[string]string{"gate.exe": "binary"}), "gate-windows-amd64.zip", &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "binary" {
		t.Fatalf("extracted=%q", got.String())
	}
	if err := extractGateBinary(makeZip(map[string]string{"gate.exe": "binary", "extra": "bad"}), "gate-windows-amd64.zip", &bytes.Buffer{}); err == nil {
		t.Fatal("zip with extra entry should be rejected")
	}
}
