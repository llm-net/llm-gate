package devhost

// 固件内嵌的守护进程二进制：assets/llmgate-devd-linux-{amd64,arm64}.gz、
// assets/llmgate-modeld-linux-{amd64,arm64}.gz 与共用的 VERSION，由 `make -C firmware devd-assets`
// 从 cmd/llmgate-devd 与 cmd/llmgate-modeld 交叉构建后 gzip 放进来。
// 与 gatehelper 的制品同规矩：目录只跟踪 .gitkeep，制品被 gitignore、每次
// build/test 重建；没有制品时相关端点答 CodeAssetMissing（离线测试用假二进制）。

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"strings"
	"sync"
)

//go:embed assets/*
var assetFS embed.FS

// BinaryProvider 给出某架构的守护进程二进制（原始字节）与版本。
type BinaryProvider interface {
	Binary(goarch string) (data []byte, version string, err error)
}

// Platforms 是有内嵌制品的目标平台。
var Platforms = []string{"linux-amd64", "linux-arm64"}

// EmbeddedBinaries 从 go:embed 的制品取某一种守护进程的二进制（解压结果按架构缓存一份）。
type EmbeddedBinaries struct {
	// BinName 是二进制文件名（llmgate-devd / llmgate-modeld）；空即 llmgate-devd。
	BinName string

	mu    sync.Mutex
	cache map[string][]byte
}

func (e *EmbeddedBinaries) binName() string {
	if e.BinName == "" {
		return devdSpec.BinName
	}
	return e.BinName
}

// Binary 实现 BinaryProvider。
func (e *EmbeddedBinaries) Binary(goarch string) ([]byte, string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return nil, "", &Error{Code: CodeUnsupportedArch, Msg: "不支持的目标架构 " + goarch}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cache == nil {
		e.cache = map[string][]byte{}
	}
	version := e.version()
	if b, ok := e.cache[goarch]; ok {
		return b, version, nil
	}
	gz, err := assetFS.ReadFile("assets/" + e.binName() + "-linux-" + goarch + ".gz")
	if err != nil {
		return nil, "", &Error{Code: CodeAssetMissing, Msg: "本固件没有内嵌 " + goarch + " 架构的守护进程 " + e.binName() + "（构建时未执行 make devd-assets）"}
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, "", fmt.Errorf("解压守护进程制品: %w", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, "", fmt.Errorf("解压守护进程制品: %w", err)
	}
	e.cache[goarch] = raw
	return raw, version, nil
}

// version 是制品版本；这一种守护进程的制品缺席时视为没有版本（界面据此禁用安装）。
func (e *EmbeddedBinaries) version() string {
	b, err := assetFS.ReadFile("assets/VERSION")
	if err != nil {
		return ""
	}
	if _, err := assetFS.ReadFile("assets/" + e.binName() + "-linux-amd64.gz"); err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Version 报告内嵌制品的版本（没有制品为空串）。
func (e *EmbeddedBinaries) Version() string { return e.version() }
