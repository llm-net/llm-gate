package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var mcodeInstallNameRE = regexp.MustCompile(`^install-[0-9]+$`)

func mcodeEntry(root, goos string) string {
	if goos == "windows" {
		return filepath.Join(root, "mcode.cmd")
	}
	return filepath.Join(root, "bin", "mcode")
}

func (a *app) managedMCodePath() string {
	state, err := readInstallState(a.root)
	if err != nil {
		return ""
	}
	entry := state.Programs["mcode"].Entry
	if entry == "" {
		return ""
	}
	if _, err := a.programRoot("mcode", entry); err != nil {
		return ""
	}
	return entry
}

// Each invocation uses a new permanent path: the official installer records
// absolute Node paths in launchers, so moving its tree would break the install.
// Only after official integrity checks and our provider check pass does install
// record and connect this entry. An existing installation is never overwritten.
func (a *app) installMCode() (string, error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "", errors.New("MiniMax Code 安装仅支持 amd64 / arm64")
	}
	rc, err := a.fetchRuntime()
	if err != nil {
		return "", err
	}
	if _, err := mcodeConfig(a.cfg.BaseURL, a.cfg.APIKey, rc.Tools["mcode"]); err != nil {
		return "", err
	}
	scriptName, shell, flags := "install.sh", "bash", []string{}
	if runtime.GOOS == "windows" {
		scriptName, shell = "install.ps1", "powershell"
		flags = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}
	} else if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", errors.New("MiniMax Code 没有当前系统的官方安装器")
	}
	interpreter, err := exec.LookPath(shell)
	if err != nil {
		return "", fmt.Errorf("MiniMax Code 官方安装器需要 %s", shell)
	}
	src := a.mcodeOrigins()
	a.printf("正在从%s读取 MiniMax Code 官方安装器…\n", src.label())
	script, err := src.get(scriptName, 2<<20)
	if err != nil {
		return "", err
	}
	for _, marker := range []string{"MCODE_INSTALL_DIR", "MCODE_NO_MODIFY_PATH", "@minimax-ai/code"} {
		if !strings.Contains(string(script), marker) {
			return "", errors.New("MiniMax Code 官方安装器契约变化，拒绝安装")
		}
	}
	parent := filepath.Join(a.root, "tools", "mcode", "releases")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	if err := noLinkPath(parent); err != nil {
		return "", err
	}
	root, err := os.MkdirTemp(parent, "install-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()
	if err := securePath(root, true); err != nil {
		return "", err
	}
	scriptPath := filepath.Join(root, scriptName)
	if err := atomicWrite(scriptPath, script, 0o600); err != nil {
		return "", err
	}
	// Empty npm configuration and isolated cache prevent inherited registries,
	// auth tokens or lifecycle overrides from entering this public install.
	npmrc := filepath.Join(root, "npmrc")
	if err := atomicWrite(npmrc, nil, 0o600); err != nil {
		return "", err
	}
	env := mcodeInstallerEnv(root, npmrc)
	nodeBin, err := a.installMCodeNode(src, root)
	if err != nil {
		return "", err
	}
	env = envWith(env, "PATH", nodeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if runtime.GOOS != "windows" {
		// Official tarballs include matching headers, so node-gyp needs no
		// separate headers download and cannot pick headers for another runtime.
		env = envWith(env, "npm_config_nodedir", filepath.Dir(nodeBin))
	}
	a.printf("正在安装 MiniMax Code、Node.js 与 npm 依赖；首次安装可能需要编译 SQLite，请稍候…\n")
	cmd := exec.Command(interpreter, append(flags, scriptPath)...)
	cmd.Dir, cmd.Env = root, env
	// Installer and dependency output is not a gate log/API contract. Do not
	// expose arbitrary dependency diagnostics or write a second copy to disk.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := runTool(cmd); err != nil {
		return "", errors.New("MiniMax Code 官方安装失败，原程序保留；请检查官方 npm / Node.js 下载网络及平台原生构建工具（Linux 需要 Python 3、make、C++ 编译器）")
	}
	entry := mcodeEntry(root, runtime.GOOS)
	version := detectVersionWithEnv(entry, env)
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return "", errors.New("MiniMax Code 安装后版本自检失败，原程序保留")
	}
	if err := a.prepareTool("mcode", entry, rc); err != nil {
		return "", err
	}
	_ = os.Remove(scriptPath)
	_ = os.Remove(npmrc)
	_ = os.RemoveAll(filepath.Join(root, ".npm-cache"))
	_ = os.RemoveAll(filepath.Join(root, ".installer-data"))
	ok = true
	a.printf("MiniMax Code %s 官方制品与派生配置自检通过。\n", version)
	return entry, nil
}

func mcodeInstallerEnv(root, npmrc string) []string {
	env := mcodeEnv(installerEnv(), filepath.Join(root, ".installer-data"))
	clean := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "NPM_CONFIG_") || upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "NODE_TLS_REJECT_UNAUTHORIZED" ||
			upper == "BASH_ENV" || upper == "ENV" || strings.Contains(upper, "TOKEN") ||
			strings.HasSuffix(upper, "API_KEY") || strings.HasPrefix(upper, "BASH_FUNC_") {
			continue
		}
		clean = append(clean, kv)
	}
	return append(clean,
		"MCODE_INSTALL_DIR="+root, "MCODE_NO_MODIFY_PATH=1", "MCODE_DOWNLOAD_MIRROR=global",
		"npm_config_userconfig="+npmrc, "npm_config_globalconfig="+filepath.Join(root, "global-npmrc"),
		"npm_config_cache="+filepath.Join(root, ".npm-cache"), "NO_COLOR=1")
}
