package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Match the firmware mcodehelper whitelist. Node 22 is supported by MCode;
// Node 24's ObjectWrap cleanup-hook regression can abort better-sqlite3 12.x.
const mcodeNodeVersion = "22.23.2"

func mcodeNodeAsset(goos, goarch string) (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return "", errors.New("MiniMax Code Node.js 架构不受支持")
	}
	if goos == "windows" {
		return "node-v" + mcodeNodeVersion + "-win-" + arch + ".zip", nil
	}
	if goos != "linux" && goos != "darwin" {
		return "", errors.New("MiniMax Code Node.js 平台不受支持")
	}
	return "node-v" + mcodeNodeVersion + "-" + goos + "-" + arch + ".tar.gz", nil
}

func (a *app) installMCodeNode(src *originChain, root string) (string, error) {
	asset, err := mcodeNodeAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	prefix := "node/v" + mcodeNodeVersion + "/"
	a.printf("正在安装并校验 MiniMax Code 专用 Node.js %s…\n", mcodeNodeVersion)
	checksums, err := src.get(prefix+"SHASUMS256.txt", 1<<20)
	if err != nil {
		return "", err
	}
	digest := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			if digest != "" {
				return "", errors.New("Node.js 校验清单有重复条目")
			}
			digest = fields[0]
		}
	}
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != 32 {
		return "", errors.New("Node.js 校验清单缺少有效 SHA-256")
	}
	archive, err := src.fetch(prefix+asset, artifactFetch{label: "Node.js", dir: root, prefix: ".node-archive", digest: digest, max: 128 << 20, mode: 0o600})
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)
	dir := filepath.Join(root, "runtime", "node")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	w := &archiveTreeWriter{label: "Node.js", stripRoot: true, staging: dir, symlinks: map[string]bool{}, budget: 1 << 30}
	if runtime.GOOS == "windows" {
		err = extractCursorZip(archive, w)
	} else {
		err = extractTarGzTree(archive, w)
	}
	if err != nil {
		return "", err
	}
	bin, name := filepath.Join(dir, "bin"), "node"
	if runtime.GOOS == "windows" {
		bin, name = dir, "node.exe"
	}
	if got := detectVersionWithEnv(filepath.Join(bin, name), mcodeInstallerEnv(root, filepath.Join(root, "npmrc"))); got != "v"+mcodeNodeVersion {
		return "", fmt.Errorf("Node.js %s 运行时自检失败", mcodeNodeVersion)
	}
	return bin, nil
}
