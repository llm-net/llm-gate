// Command package cross-builds deterministic gate release archives.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type asset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int     `json:"schema_version"`
	Version       string  `json:"version"`
	Assets        []asset `json:"assets"`
}

func main() {
	version := flag.String("version", "dev", "gate version")
	output := flag.String("output", "", "output directory")
	flag.Parse()
	if *output == "" || strings.ContainsAny(*version, "\r\n\x00") {
		fatalf("-output and a valid -version are required")
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		fatalf("mkdir: %v", err)
	}
	tmp, err := os.MkdirTemp("", "gate-package-*")
	if err != nil {
		fatalf("temp: %v", err)
	}
	defer os.RemoveAll(tmp)

	targets := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}}
	result := manifest{SchemaVersion: 1, Version: *version, Assets: []asset{}}
	for _, target := range targets {
		goos, goarch := target[0], target[1]
		binName := "gate"
		if goos == "windows" {
			binName += ".exe"
		}
		binary := filepath.Join(tmp, goos+"-"+goarch+"-"+binName)
		// Generated archives are tracked in the parent repository. Omitting VCS
		// metadata keeps later targets independent of writes to earlier archives.
		cmd := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-s -w -X main.version="+*version, "-o", binary, "./cmd/gate")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fatalf("build %s/%s: %v", goos, goarch, err)
		}
		ext := ".tar.gz"
		if goos == "windows" {
			ext = ".zip"
		}
		name := "gate-" + goos + "-" + goarch + ext
		archive := filepath.Join(*output, name)
		if goos == "windows" {
			err = writeZip(archive, binary, binName)
		} else {
			err = writeTarGZ(archive, binary, binName)
		}
		if err != nil {
			fatalf("archive %s: %v", name, err)
		}
		b, err := os.ReadFile(archive)
		if err != nil {
			fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(b)
		result.Assets = append(result.Assets, asset{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
	}
	sort.Slice(result.Assets, func(i, j int) bool { return result.Assets[i].Name < result.Assets[j].Name })
	manifestBytes, _ := json.MarshalIndent(result, "", "  ")
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(*output, "stable.json"), manifestBytes, 0o644); err != nil {
		fatalf("manifest: %v", err)
	}
	var sums strings.Builder
	for _, a := range result.Assets {
		fmt.Fprintf(&sums, "%s  %s\n", a.SHA256, a.Name)
	}
	if err := os.WriteFile(filepath.Join(*output, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		fatalf("sums: %v", err)
	}
}

func writeTarGZ(dst, src, name string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	gz, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	gz.Header.ModTime = time.Unix(0, 0)
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	b, err := os.ReadFile(src)
	if err == nil {
		err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), ModTime: time.Unix(0, 0), Uid: 0, Gid: 0, Format: tar.FormatUSTAR})
	}
	if err == nil {
		_, err = tw.Write(b)
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeZip(dst, src, name string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o755)
	h.SetModTime(time.Unix(0, 0))
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	in.Close()
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "package: "+format+"\n", args...)
	os.Exit(1)
}
