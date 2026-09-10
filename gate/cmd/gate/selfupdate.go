package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	selfUpdateManifestPath = "/gate-helper/releases/stable.json"
	selfUpdateManifestMax  = int64(256 << 10)
	selfUpdateArchiveMax   = int64(64 << 20)
	selfUpdateBinaryMax    = int64(32 << 20)
)

var (
	// 版本名称形如 2608221732-2d81（YYMMDDHHMM-提交短哈希末 4 位，脏树再缀 -d，
	// 见 firmware/Makefile）。
	gateReleaseVersionRE = regexp.MustCompile(`^[0-9]{10}-[0-9a-f]{4}(?:-d)?$`)
	gateReleaseDigestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type gateReleaseAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type gateReleaseManifest struct {
	SchemaVersion int                `json:"schema_version"`
	Version       string             `json:"version"`
	Assets        []gateReleaseAsset `json:"assets"`
}

func (a *app) selfUpdate() error {
	if a.cfg.BaseURL == "" {
		return errors.New("请先执行 gate -url <设备地址>")
	}
	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前 gate: %w", err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("解析当前 gate 路径: %w", err)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("解析当前 gate 绝对路径: %w", err)
	}
	return a.selfUpdateAt(target, runtime.GOOS, runtime.GOARCH, version)
}

func (a *app) selfUpdateAt(target, goos, goarch, currentVersion string) error {
	if strings.HasPrefix(a.cfg.BaseURL, "http://") {
		fmt.Fprintln(a.err, "警告：当前地址使用明文 HTTP，gate 升级清单和程序在链路上没有 TLS 保护。")
	}
	assetName, err := gateAssetName(goos, goarch)
	if err != nil {
		return err
	}
	manifest, asset, err := a.fetchSelfUpdateManifest(assetName)
	if err != nil {
		return err
	}
	newer, err := newerGateRelease(manifest.Version, currentVersion)
	if err != nil {
		return err
	}
	if !newer {
		if manifest.Version == currentVersion {
			fmt.Fprintf(a.out, "gate 已是最新版本：%s\n", currentVersion)
		} else {
			fmt.Fprintf(a.out, "设备上的 gate 版本 %s 不比当前版本 %s 新，无需升级。\n", manifest.Version, currentVersion)
		}
		return nil
	}

	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("检查当前 gate: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("当前 gate 不是普通文件，拒绝替换")
	}
	fmt.Fprintf(a.out, "发现 gate 新版本：%s → %s\n", currentVersion, manifest.Version)

	staged, err := a.downloadAndExtractSoc(asset, target, info.Mode().Perm(), goos)
	if err != nil {
		return err
	}
	keepStaged := false
	defer func() {
		if !keepStaged {
			_ = os.Remove(staged)
		}
	}()
	if err := verifyGateVersion(staged, manifest.Version); err != nil {
		return fmt.Errorf("校验新 gate 程序: %w", err)
	}

	deferred, err := applySelfUpdate(target, staged, manifest.Version, a.root)
	if err != nil {
		return err
	}
	keepStaged = deferred
	if deferred {
		fmt.Fprintf(a.out, "gate %s 已下载并校验；Windows 将在本命令退出后完成替换。\n", manifest.Version)
		return nil
	}
	if err := a.recordExecutable(target); err != nil {
		return fmt.Errorf("gate 已更新，但安装记录刷新失败：%w", err)
	}
	fmt.Fprintf(a.out, "gate 已升级到 %s。\n", manifest.Version)
	return nil
}

func gateAssetName(goos, goarch string) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("gate 没有 %s/%s 的升级制品", goos, goarch)
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	} else if goos != "linux" && goos != "darwin" {
		return "", fmt.Errorf("gate 没有 %s/%s 的升级制品", goos, goarch)
	}
	return "gate-" + goos + "-" + goarch + ext, nil
}

func newerGateRelease(candidate, current string) (bool, error) {
	if !gateReleaseVersionRE.MatchString(candidate) {
		return false, fmt.Errorf("设备返回的 gate 版本 %q 形态非法", candidate)
	}
	if current == "dev" {
		return true, nil
	}
	if !gateReleaseVersionRE.MatchString(current) {
		return false, fmt.Errorf("当前 gate 版本 %q 无法按发布版本比较；请重新运行安装脚本", current)
	}
	return gateReleaseStamp(candidate) > gateReleaseStamp(current), nil
}

// gateReleaseStamp 取版本名称里参与比较的十位时间戳。
//
// 只有时间戳排序，连字号后的提交哈希两两之间没有先后可言，同一时间戳的两个包
// 因此互不构成升级。长度恒相等，字典序即时间序。固件侧 versionCore 是同一口径
// 的另一份实现，两边必须一起改。
func gateReleaseStamp(v string) string {
	stamp, _, _ := strings.Cut(v, "-")
	return stamp
}

func (a *app) fetchSelfUpdateManifest(assetName string) (gateReleaseManifest, gateReleaseAsset, error) {
	var manifest gateReleaseManifest
	req, err := http.NewRequest(http.MethodGet, a.cfg.BaseURL+selfUpdateManifestPath, nil)
	if err != nil {
		return manifest, gateReleaseAsset{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := a.hc.Do(req)
	if err != nil {
		return manifest, gateReleaseAsset{}, fmt.Errorf("查询 gate 版本: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return manifest, gateReleaseAsset{}, fmt.Errorf("查询 gate 版本返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > selfUpdateManifestMax {
		return manifest, gateReleaseAsset{}, errors.New("gate 版本清单超过大小限制")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, selfUpdateManifestMax+1))
	if err != nil {
		return manifest, gateReleaseAsset{}, fmt.Errorf("读取 gate 版本清单: %w", err)
	}
	if int64(len(body)) > selfUpdateManifestMax {
		return manifest, gateReleaseAsset{}, errors.New("gate 版本清单超过大小限制")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return manifest, gateReleaseAsset{}, fmt.Errorf("解析 gate 版本清单: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return manifest, gateReleaseAsset{}, fmt.Errorf("解析 gate 版本清单: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return manifest, gateReleaseAsset{}, fmt.Errorf("不支持 gate 发布清单 schema_version %d", manifest.SchemaVersion)
	}
	if !gateReleaseVersionRE.MatchString(manifest.Version) {
		return manifest, gateReleaseAsset{}, fmt.Errorf("设备返回的 gate 版本 %q 形态非法", manifest.Version)
	}
	if len(manifest.Assets) == 0 || len(manifest.Assets) > 32 {
		return manifest, gateReleaseAsset{}, errors.New("gate 发布清单的制品数量非法")
	}
	var selected gateReleaseAsset
	found := false
	for _, asset := range manifest.Assets {
		if asset.Name != assetName {
			continue
		}
		if found {
			return manifest, gateReleaseAsset{}, fmt.Errorf("gate 发布清单重复包含 %s", assetName)
		}
		found = true
		selected = asset
	}
	if !found {
		return manifest, gateReleaseAsset{}, fmt.Errorf("设备没有当前平台制品 %s", assetName)
	}
	if selected.Size <= 0 || selected.Size > selfUpdateArchiveMax || !gateReleaseDigestRE.MatchString(selected.SHA256) {
		return manifest, gateReleaseAsset{}, fmt.Errorf("设备返回的 %s 元数据非法", assetName)
	}
	return manifest, selected, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("包含多余 JSON 值")
	}
	return err
}

func (a *app) downloadAndExtractSoc(asset gateReleaseAsset, target string, mode os.FileMode, goos string) (string, error) {
	dir := filepath.Dir(target)
	archive, err := os.CreateTemp(dir, ".gate-update-archive-*")
	if err != nil {
		return "", fmt.Errorf("在 gate 所在目录创建升级文件: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	if err := archive.Chmod(0o600); err != nil {
		archive.Close()
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, a.cfg.BaseURL+"/gate-helper/releases/"+asset.Name, nil)
	if err != nil {
		archive.Close()
		return "", err
	}
	client := *a.hc
	client.Timeout = 15 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		archive.Close()
		return "", fmt.Errorf("下载 gate 制品: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		archive.Close()
		return "", fmt.Errorf("下载 gate 制品返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > selfUpdateArchiveMax || (resp.ContentLength >= 0 && resp.ContentLength != asset.Size) {
		archive.Close()
		return "", errors.New("gate 制品大小与发布清单不匹配")
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, selfUpdateArchiveMax+1))
	if copyErr != nil {
		archive.Close()
		return "", fmt.Errorf("接收 gate 制品: %w", copyErr)
	}
	if n != asset.Size || n > selfUpdateArchiveMax {
		archive.Close()
		return "", errors.New("gate 制品大小与发布清单不匹配")
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != asset.SHA256 {
		archive.Close()
		return "", errors.New("gate 制品 SHA-256 校验失败")
	}
	if err := archive.Sync(); err != nil {
		archive.Close()
		return "", err
	}
	if err := archive.Close(); err != nil {
		return "", err
	}

	pattern := ".gate-update-new-*"
	if goos == "windows" {
		pattern += ".exe"
	}
	staged, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	stagedPath := staged.Name()
	ok := false
	defer func() {
		_ = staged.Close()
		if !ok {
			_ = os.Remove(stagedPath)
		}
	}()
	if mode&0o111 == 0 {
		mode |= 0o700
	}
	if err := staged.Chmod(mode); err != nil {
		return "", err
	}
	if err := extractGateBinary(archivePath, asset.Name, staged); err != nil {
		return "", err
	}
	if err := staged.Sync(); err != nil {
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	ok = true
	return stagedPath, nil
}

func extractGateBinary(archivePath, assetName string, dst io.Writer) error {
	want := "gate"
	if strings.HasSuffix(assetName, ".zip") {
		want = "gate.exe"
	}
	copyBinary := func(name string, mode os.FileMode, size int64, src io.Reader) error {
		if filepath.ToSlash(name) != want || !mode.IsRegular() {
			return errors.New("gate 归档只能包含一个平台程序")
		}
		if size <= 0 || size > selfUpdateBinaryMax {
			return errors.New("gate 归档中的程序大小非法")
		}
		n, err := io.Copy(dst, io.LimitReader(src, selfUpdateBinaryMax+1))
		if err != nil {
			return err
		}
		if n != size || n > selfUpdateBinaryMax {
			return errors.New("gate 归档中的程序大小非法")
		}
		return nil
	}

	if strings.HasSuffix(assetName, ".tar.gz") {
		file, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer file.Close()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("打开 gate gzip: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		header, err := tr.Next()
		if err != nil {
			return fmt.Errorf("读取 gate tar: %w", err)
		}
		if err := copyBinary(header.Name, header.FileInfo().Mode(), header.Size, tr); err != nil {
			return err
		}
		if _, err := tr.Next(); !errors.Is(err, io.EOF) {
			if err == nil {
				return errors.New("gate tar 包含多余文件")
			}
			return fmt.Errorf("读取 gate tar: %w", err)
		}
		return nil
	}
	if strings.HasSuffix(assetName, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return fmt.Errorf("打开 gate zip: %w", err)
		}
		defer zr.Close()
		if len(zr.File) != 1 {
			return errors.New("gate zip 只能包含一个程序")
		}
		entry := zr.File[0]
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		return copyBinary(entry.Name, entry.Mode(), int64(entry.UncompressedSize64), reader)
	}
	return errors.New("gate 制品归档格式未知")
}

func verifyGateVersion(path, expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = installerEnv()
	output, err := cmd.Output()
	if err != nil {
		return errors.New("新程序无法运行")
	}
	if got := strings.TrimSpace(string(output)); got != expected {
		return fmt.Errorf("新程序自述版本为 %q，发布清单要求 %q", got, expected)
	}
	return nil
}

func copyFileToTemp(src, dir, pattern string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("待复制的 gate 不是普通文件")
	}
	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if _, err := io.Copy(out, io.LimitReader(in, selfUpdateBinaryMax+1)); err != nil {
		return "", err
	}
	if stat, err := out.Stat(); err != nil {
		return "", err
	} else if stat.Size() <= 0 || stat.Size() > selfUpdateBinaryMax {
		return "", errors.New("gate 程序大小非法")
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}
