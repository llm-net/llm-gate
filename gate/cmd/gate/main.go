// gate connects locally installed development tools to one LLM Gate device.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

var version = "dev"

const schemaVersion = 1

const claudeCatalogModelPrefix = "anthropic/llmgate/"

type toolState struct {
	BinaryPath      string `json:"binary_path"`
	DetectedVersion string `json:"detected_version"`
	Origin          string `json:"origin"`
}

type config struct {
	SchemaVersion int                  `json:"schema_version"`
	BaseURL       string               `json:"base_url"`
	APIKey        string               `json:"api_key"`
	Tools         map[string]toolState `json:"tools,omitempty"`
	// Shared 记录写进使用者自己配置目录的接入（今天只有 codex，见 codex.go）。
	Shared map[string]sharedCodex `json:"shared,omitempty"`
}

type subscription struct {
	Provider     string `json:"provider"`
	Configured   bool   `json:"configured"`
	Available    bool   `json:"available"`
	DefaultModel string `json:"default_model"`
}

type runtimeModel struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type runtimeTool struct {
	DefaultModel string         `json:"default_model"`
	Models       []runtimeModel `json:"models"`
}

type runtimeConfig struct {
	SchemaVersion int                    `json:"schema_version"`
	Revision      int64                  `json:"revision"`
	Subscriptions []subscription         `json:"subscriptions"`
	Tools         map[string]runtimeTool `json:"tools"`
}

func claudeClientModelID(model runtimeModel) string {
	if model.Source != "catalog" {
		return model.Name
	}
	return claudeCatalogModelPrefix + base64.RawURLEncoding.EncodeToString([]byte(model.Name))
}

type app struct {
	root     string
	cfg      config
	hc       *http.Client
	out      io.Writer
	err      io.Writer
	selfPath string
	lease    *operationLease
}

func main() {
	if handled, code := runSelfUpdateInternal(os.Args[1:]); handled {
		os.Exit(code)
	}
	code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, in io.Reader, out, errOut io.Writer) int {
	root, err := configRoot()
	if err != nil {
		fmt.Fprintln(errOut, "gate:", err)
		return 1
	}
	a := &app{root: root, out: out, err: errOut, hc: newHTTPClient()}
	if len(args) > 0 && args[0] == "__installer-exec" {
		if err := runInstaller(root, args[1:], in, out, errOut); err != nil {
			fmt.Fprintln(errOut, "gate: 安装提交失败")
			return 1
		}
		return 0
	}
	if len(args) > 0 && (args[0] == "uninstall" || (len(args) > 1 && slices.Contains(toolNames, args[0]) && args[1] == "uninstall")) {
		target, e := a.currentExecutable()
		if e == nil {
			a.root, e = uninstallRootForExecutable(root, target)
		}
		if e != nil {
			fmt.Fprintln(errOut, "gate:", e)
			return 1
		}
		root = a.root
		a.cfg, err = loadConfig(root)
		if err != nil {
			fmt.Fprintln(out, "阻塞：连接配置损坏，无法确认共享接入的恢复范围；未作修改。")
			return 1
		}
		name, rest := "", args[1:]
		if args[0] != "uninstall" {
			name, rest = args[0], args[2:]
		}
		if err := a.uninstall(name, rest, in); err != nil {
			fmt.Fprintln(errOut, "gate:", err)
			return exitCode(err)
		}
		return 0
	}
	if commandMutates(args) && !installerChild(root, args) {
		a.lease, err = acquireOperation(root, commandLaunches(args))
		if err != nil {
			fmt.Fprintln(errOut, "gate:", err)
			return 1
		}
		defer a.lease.close()
	}
	a.cfg, err = loadConfig(root)
	if err != nil {
		fmt.Fprintln(errOut, "gate:", err)
		return 1
	}
	if len(args) == 0 {
		printHelp(out)
		return 0
	}
	if args[0] == "-url" {
		if len(args) != 2 {
			fmt.Fprintln(errOut, "用法: gate -url <设备地址>")
			return 2
		}
		if err := a.setURL(args[1]); err != nil {
			fmt.Fprintln(errOut, "gate:", err)
			return 1
		}
		return 0
	}
	if args[0] == "-sk" {
		if len(args) > 2 {
			fmt.Fprintln(errOut, "用法: gate -sk [API Key]")
			return 2
		}
		key := ""
		if len(args) == 2 {
			key = args[1]
		} else {
			key, err = readSecret("请输入 LLM Gate API Key：", errOut)
			if err != nil {
				fmt.Fprintln(errOut, "gate:", err)
				return 1
			}
		}
		if err := a.setKey(key); err != nil {
			fmt.Fprintln(errOut, "gate:", err)
			return 1
		}
		return 0
	}
	if args[0] == "bootstrap" {
		if len(args) < 2 || len(args) > 3 {
			fmt.Fprintln(errOut, "用法: gate bootstrap <设备地址> [API Key]")
			return 2
		}
		key := ""
		if len(args) == 3 {
			key = args[2]
		} else if a.cfg.APIKey != "" {
			key = a.cfg.APIKey
		} else {
			key, err = readSecret("请输入 LLM Gate API Key：", errOut)
			if err != nil {
				fmt.Fprintln(errOut, "gate:", err)
				return 1
			}
		}
		if err := a.bootstrap(args[1], key); err != nil {
			fmt.Fprintln(errOut, "gate:", err)
			return 1
		}
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp(out)
		return 0
	case "version", "--version":
		fmt.Fprintln(out, version)
		return 0
	case "__record-install":
		err = a.recordInstaller(args[1:])
	case "status":
		err = a.status()
	case "update":
		if len(args) != 1 {
			fmt.Fprintln(errOut, "用法: gate update")
			return 2
		}
		err = a.selfUpdate()
	case "codex", "grok", "claude", "cursor", "opencode":
		err = a.tool(args[0], args[1:], in)
	default:
		fmt.Fprintf(errOut, "gate: 未知命令 %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(errOut, "gate:", err)
		return exitCode(err)
	}
	return 0
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `LLM Gate 开发工具引导器

用法:
  gate [<工具>] uninstall [--purge] [--dry-run] [--yes]
    默认保留会话；--purge 额外删除受管会话与缓存；--dry-run 只读预览。
  gate -url <设备地址>       设置当前设备地址
  gate -sk [API Key]        设置 API Key；省略参数时隐藏输入
  gate status               显示设备策略和工具状态
  gate update               从当前设备检查并升级 gate
  gate <工具> connect       关联 PATH 中已有工具
  gate <工具> install       缺失时经设备安装
  gate <工具> update [--adopt]
  gate <工具> disconnect
  gate <工具> status
  gate <工具> [参数...]      启动 codex、grok、claude、cursor 或 opencode
  gate codex connect --shared [--path <内核路径>]
                           把设备接入写进使用者自己的 ~/.codex，供 ChatGPT.app
                           桌面版、IDE 插件等不经 gate 启动的 Codex 使用
  gate codex disconnect --shared
`)
}

func configRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("GATE_CONFIG_DIR")); v != "" {
		return filepath.Abs(v)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录: %w", err)
	}
	return filepath.Join(dir, "gate"), nil
}

func emptyConfig() config {
	return config{SchemaVersion: schemaVersion, Tools: map[string]toolState{}}
}

func loadConfig(root string) (config, error) {
	cfg := emptyConfig()
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("读取配置: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("配置文件损坏: %w", err)
	}
	if cfg.SchemaVersion != schemaVersion {
		return cfg, fmt.Errorf("不支持的本地配置 schema_version %d", cfg.SchemaVersion)
	}
	if cfg.Tools == nil {
		cfg.Tools = map[string]toolState{}
	}
	return cfg, nil
}

func (a *app) save() error {
	if err := os.MkdirAll(a.root, 0o700); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}
	b, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("编码配置: %w", err)
	}
	b = append(b, '\n')
	return atomicWrite(filepath.Join(a.root, "config.json"), b, 0o600)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := securePath(filepath.Dir(path), true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gate-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := securePath(name, false); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n\t<>{}") || strings.Contains(raw, "<设备地址>") {
		return "", errors.New("设备地址为空或含占位符/控制字符")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("设备地址必须是有效的 http 或 https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("设备地址不能包含用户信息、query 或 fragment")
	}
	p := strings.TrimRight(u.EscapedPath(), "/")
	for _, suffix := range []string{"/agents/codex/v1", "/agents/grok/v1", "/agents/claude/v1", "/agents/claude",
		"/agents/cursor", "/agents/v1", "/codex/v1", "/claude/v1", "/claude", "/v1"} {
		if p == suffix {
			p = ""
			break
		}
	}
	if p != "" {
		return "", errors.New("设备地址不能包含接口路径")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 || sameOrigin(req.URL, via[0].URL) {
				if len(via) < 5 {
					return nil
				}
			}
			return http.ErrUseLastResponse
		},
	}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (a *app) setURL(raw string) error {
	base, err := normalizeBaseURL(raw)
	if err != nil {
		return err
	}
	if strings.HasPrefix(base, "http://") {
		fmt.Fprintln(a.err, "警告：当前地址使用明文 HTTP，API Key 在链路上没有 TLS 保护。")
	}
	if err := a.health(base); err != nil {
		return err
	}
	if a.cfg.APIKey != "" {
		if _, err := a.fetchRuntimeAt(base, a.cfg.APIKey); err != nil {
			return fmt.Errorf("新地址未接受已保存的 API Key: %w", err)
		}
	}
	a.cfg.BaseURL = base
	if err := a.save(); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "设备地址已更新：", base)
	return nil
}

func (a *app) setKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return errors.New("API Key 为空或形态非法")
	}
	if a.cfg.BaseURL == "" {
		return errors.New("请先执行 gate -url <设备地址>")
	}
	if _, err := a.fetchRuntimeAt(a.cfg.BaseURL, key); err != nil {
		return fmt.Errorf("设备拒绝 API Key 或配置不可用: %w", err)
	}
	a.cfg.APIKey = key
	if err := a.save(); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "API Key 已验证并保存。")
	return nil
}

// bootstrap validates a new device address and API key as one pair, then
// persists both fields with one atomic config replacement. The bootstrap
// scripts use this command so moving to a different device never has to save
// the new address while the old device's key is still active.
func (a *app) bootstrap(raw, key string) error {
	base, err := normalizeBaseURL(raw)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return errors.New("API Key 为空或形态非法")
	}
	if strings.HasPrefix(base, "http://") {
		fmt.Fprintln(a.err, "警告：当前地址使用明文 HTTP，API Key 在链路上没有 TLS 保护。")
	}
	if err := a.health(base); err != nil {
		return err
	}
	if _, err := a.fetchRuntimeAt(base, key); err != nil {
		return fmt.Errorf("设备拒绝 API Key 或配置不可用: %w", err)
	}
	previous := a.cfg
	a.cfg.BaseURL = base
	a.cfg.APIKey = key
	if err := a.save(); err != nil {
		a.cfg = previous
		return err
	}
	fmt.Fprintln(a.out, "设备地址与 API Key 已验证并保存。")
	return nil
}

func (a *app) health(base string) error {
	req, _ := http.NewRequest(http.MethodGet, base+"/healthz", nil)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("访问 %s/healthz: %w", base, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("设备健康检查返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *app) requireDevice() error {
	if a.cfg.BaseURL == "" || a.cfg.APIKey == "" {
		return errors.New("请先设置设备地址和 API Key")
	}
	return nil
}

func (a *app) fetchRuntime() (runtimeConfig, error) {
	if err := a.requireDevice(); err != nil {
		return runtimeConfig{}, err
	}
	return a.fetchRuntimeAt(a.cfg.BaseURL, a.cfg.APIKey)
}

func (a *app) fetchRuntimeAt(base, key string) (runtimeConfig, error) {
	var got runtimeConfig
	req, _ := http.NewRequest(http.MethodGet, base+"/gate-helper/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return got, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return got, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	if err := dec.Decode(&got); err != nil {
		return got, fmt.Errorf("解析设备配置: %w", err)
	}
	if got.SchemaVersion != schemaVersion {
		return got, fmt.Errorf("不支持设备 schema_version %d", got.SchemaVersion)
	}
	if got.Tools == nil {
		return got, errors.New("设备配置缺少 tools")
	}
	return got, nil
}

func (a *app) status() error {
	fmt.Fprintln(a.out, "gate:", version)
	if a.cfg.BaseURL == "" {
		fmt.Fprintln(a.out, "设备地址：未设置")
		fmt.Fprintln(a.out, "API Key：未设置")
		return nil
	}
	fmt.Fprintln(a.out, "设备地址：", a.cfg.BaseURL)
	fmt.Fprintln(a.out, "API Key：", maskKey(a.cfg.APIKey))
	rc, err := a.fetchRuntime()
	if err != nil {
		return fmt.Errorf("读取设备配置: %w", err)
	}
	for _, name := range []string{"codex", "grok", "claude", "cursor", "opencode"} {
		sub := findSubscription(rc, name)
		state := "未授权"
		if name == "opencode" {
			state = "无需订阅"
		} else if sub.Configured && sub.Available {
			state = "已授权"
		} else if sub.Configured {
			state = "已配置，当前不可用"
		}
		extra := 0
		for _, m := range rc.Tools[name].Models {
			if m.Source == "catalog" {
				extra++
			}
		}
		linked := "未关联"
		if st, ok := a.cfg.Tools[name]; ok {
			linked = st.Origin + " " + st.BinaryPath
		}
		if sh, ok := a.cfg.Shared[name]; ok {
			linked += "，共享接入 " + filepath.Join(sh.Home, "config.toml")
		}
		if name == "cursor" {
			fmt.Fprintf(a.out, "cursor：%s，%s\n", state, linked)
		} else {
			fmt.Fprintf(a.out, "%s：%s，额外模型 %d，%s\n", name, state, extra, linked)
		}
	}
	return nil
}

func maskKey(key string) string {
	if key == "" {
		return "未设置"
	}
	if len(key) <= 10 {
		return "****"
	}
	return key[:min(9, len(key)-4)] + "…" + key[len(key)-4:]
}

func findSubscription(rc runtimeConfig, name string) subscription {
	for _, sub := range rc.Subscriptions {
		if sub.Provider == name {
			return sub
		}
	}
	return subscription{Provider: name}
}

func (a *app) tool(name string, args []string, in io.Reader) error {
	if len(args) > 0 {
		if name == "opencode" && args[0] == "upgrade" {
			return errors.New("OpenCode 的受管升级请使用 gate opencode update")
		}
		switch args[0] {
		case "uninstall":
			return a.uninstall(name, args[1:], in)
		case "connect":
			if name == "codex" && len(args) >= 2 && args[1] == "--shared" {
				explicit := ""
				switch {
				case len(args) == 2:
				case len(args) == 4 && args[2] == "--path":
					explicit = args[3]
				default:
					return errors.New("用法: gate codex connect --shared [--path <内核路径>]")
				}
				return a.connectShared(explicit)
			}
			if len(args) != 1 {
				return errors.New("connect 不接受其他参数")
			}
			return a.connect(name, "", "external")
		case "install":
			if len(args) != 1 {
				return errors.New("install 不接受其他参数")
			}
			return a.install(name, false, false)
		case "update":
			adopt := len(args) == 2 && args[1] == "--adopt"
			if len(args) > 2 || (len(args) == 2 && !adopt) {
				return errors.New("用法: gate <tool> update [--adopt]")
			}
			return a.install(name, adopt, true)
		case "disconnect":
			if name == "codex" && len(args) == 2 && args[1] == "--shared" {
				return a.disconnectShared()
			}
			if len(args) != 1 {
				return errors.New("disconnect 不接受其他参数")
			}
			return a.disconnect(name)
		case "status":
			if len(args) != 1 {
				return errors.New("status 不接受其他参数")
			}
			return a.toolStatus(name)
		}
	}
	return a.launch(name, args, in)
}

func toolBinaryName(name string) string {
	// cursor 的 CLI 程序名是 cursor-agent；官方安装另有 agent 软链，但太泛
	// （grok 也装同名 agent 软链），不作候选。
	if name == "cursor" {
		name = "cursor-agent"
	}
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func (a *app) connect(name, explicit, origin string) error {
	rc, err := a.fetchRuntime()
	if err != nil {
		return err
	}
	// cursor 恒订阅制：成员侧模型由 CLI 经订阅面自发现，不看目录模型投影；
	// 授权门在 prepareCursor（订阅勾选/可用 + exchange 探针）。
	if name != "cursor" && len(rc.Tools[name].Models) == 0 {
		return fmt.Errorf("这把 API Key 当前没有可用于 %s 的模型；请在管理台配置", name)
	}
	path := explicit
	if path == "" {
		path, err = a.usablePathCandidate(name, rc)
		if err != nil {
			return err
		}
	} else {
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := a.prepareTool(name, path, rc); err != nil {
			return err
		}
	}
	ver := detectVersion(path)
	a.cfg.Tools[name] = toolState{BinaryPath: path, DetectedVersion: ver, Origin: origin}
	if err := a.save(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s 已关联：%s (%s)\n", name, path, ver)
	if origin == "external" {
		if alts := pathAlternatives(toolBinaryName(name), path); len(alts) > 0 {
			fmt.Fprintf(a.err, "提示：PATH 中另有 %s：%s；gate 固定使用已关联路径，经其他渠道升级不会改变它。\n",
				name, strings.Join(alts, "、"))
		}
	}
	return nil
}

// usablePathCandidate links the first PATH installation that passes the
// tool's self checks. Only a failure pinned to one binary (cliUnusableError,
// e.g. an outdated npm codex shadowing a freshly upgraded standalone one)
// moves on to the next distinct installation; a device-side error returns
// immediately so one device fault is not retried against every installation.
func (a *app) usablePathCandidate(name string, rc runtimeConfig) (string, error) {
	binary := toolBinaryName(name)
	candidates := lookPathAll(binary)
	if len(candidates) == 0 {
		return "", fmt.Errorf("PATH 中没有 %s；可执行 gate %s install", binary, name)
	}
	var failures []error
	for i, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		err = a.prepareTool(name, abs, rc)
		if err == nil {
			return abs, nil
		}
		if !isCLIUnusable(err) {
			return "", err
		}
		failures = append(failures, err)
		if i < len(candidates)-1 {
			fmt.Fprintf(a.err, "跳过不可用安装：%v\n", err)
		}
	}
	return "", &noUsableCLIError{tool: name, failures: failures}
}

// lookPathAll returns every executable named binary reachable through PATH in
// PATH order. Entries resolving through symlinks to the same file as an
// earlier entry are dropped; the returned strings are the PATH-visible paths.
func lookPathAll(binary string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(candidate string) {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return
		}
		key := abs
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			key = resolved
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, abs)
		}
	}
	// Plain LookPath first: it understands platform quirks (quoted Windows
	// PATH entries, PATHEXT), so its hit is never missed by the manual scan,
	// and as the true first PATH match it keeps the order intact.
	if found, err := exec.LookPath(binary); err == nil && filepath.IsAbs(found) {
		add(found)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if found, err := exec.LookPath(filepath.Join(dir, binary)); err == nil {
			add(found)
		}
	}
	return out
}

// pathAlternatives lists PATH installations of binary that are different
// files from used. These are where an upgrade through another channel (npm,
// brew, official script) lands — never in used itself.
func pathAlternatives(binary, used string) []string {
	usedKey := used
	if resolved, err := filepath.EvalSymlinks(used); err == nil {
		usedKey = resolved
	}
	var out []string
	for _, candidate := range lookPathAll(binary) {
		key := candidate
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			key = resolved
		}
		if key != usedKey {
			out = append(out, candidate)
		}
	}
	return out
}

// cliUnusableError reports a self-check failure pinned to one specific tool
// binary — too old, broken, or emitting an unknown shape — so a different
// installation of the same tool may still work. Device-side and policy
// failures never use this type.
type cliUnusableError struct {
	tool    string // display name, e.g. "Codex"
	path    string
	version string
	reason  string
}

func (e *cliUnusableError) Error() string {
	return fmt.Sprintf("%s（%s，版本 %s）%s", e.tool, e.path, e.version, e.reason)
}

// noUsableCLIError aggregates the per-binary failures after every PATH
// installation of one tool failed its self check.
type noUsableCLIError struct {
	tool     string // command name, e.g. "codex"
	failures []error
}

func (e *noUsableCLIError) Error() string {
	parts := make([]string, len(e.failures))
	for i, err := range e.failures {
		parts[i] = err.Error()
	}
	if len(parts) == 1 {
		return fmt.Sprintf("%s；可升级或卸载后重试，或执行 gate %s install 经设备安装受管副本", parts[0], e.tool)
	}
	return fmt.Sprintf("PATH 中的 %d 个 %s 都不可用：%s。多渠道安装并存时，升级请认准 gate 实际使用的路径；可卸载多余安装，或执行 gate %s install 经设备安装受管副本",
		len(parts), e.tool, strings.Join(parts, "；"), e.tool)
}

func (e *noUsableCLIError) Unwrap() []error { return e.failures }

func isCLIUnusable(err error) bool {
	var unusable *cliUnusableError
	return errors.As(err, &unusable)
}

// withReconnectHint appends recovery guidance when a linked binary fails its
// self check: upgrades made through another channel land in the PATH
// alternatives listed here, never in the linked path, and gate <tool> install
// re-picks a usable installation.
func (a *app) withReconnectHint(err error, name string) error {
	var unusable *cliUnusableError
	if !errors.As(err, &unusable) {
		return err
	}
	hint := ""
	if alts := pathAlternatives(toolBinaryName(name), unusable.path); len(alts) > 0 {
		hint = fmt.Sprintf("；PATH 中另有 %s：%s，经其他渠道升级改动的是它们而不是已关联路径", name, strings.Join(alts, "、"))
	}
	return fmt.Errorf("%w%s；执行 gate %s install 可重新选择可用安装", err, hint, name)
}

func detectVersion(path string) string {
	return detectVersionWithEnv(path, nil)
}

func detectVersionWithEnv(path string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	if env != nil {
		cmd.Env = env
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 120 {
		line = line[:120]
	}
	if line == "" {
		return "unknown"
	}
	return line
}

func (a *app) toolStatus(name string) error {
	rc, err := a.fetchRuntime()
	if err != nil {
		return err
	}
	st, ok := a.cfg.Tools[name]
	if !ok {
		if name == "cursor" {
			fmt.Fprintln(a.out, "cursor：未关联")
		} else {
			fmt.Fprintf(a.out, "%s：未关联，可见模型 %d\n", name, len(rc.Tools[name].Models))
		}
		if paths := lookPathAll(toolBinaryName(name)); len(paths) > 0 {
			fmt.Fprintf(a.out, "PATH 检测到：%s\n", strings.Join(paths, "、"))
		}
		if name == "codex" {
			a.printSharedStatus()
		}
		return nil
	}
	state := "可用"
	if _, err := os.Stat(st.BinaryPath); err != nil {
		state = "路径失效"
	} else if name != "cursor" && len(rc.Tools[name].Models) == 0 {
		state = "无可用模型"
	}
	fmt.Fprintf(a.out, "%s：%s\n路径：%s\n版本：%s\n来源：%s\n",
		name, state, st.BinaryPath, st.DetectedVersion, st.Origin)
	if name != "cursor" {
		fmt.Fprintf(a.out, "可见模型：%d\n", len(rc.Tools[name].Models))
	}
	if alts := pathAlternatives(toolBinaryName(name), st.BinaryPath); len(alts) > 0 {
		fmt.Fprintf(a.out, "PATH 其他安装：%s\n", strings.Join(alts, "、"))
	}
	if name == "codex" {
		a.printSharedStatus()
	}
	return nil
}

func (a *app) disconnect(name string) error { return a.disconnectDerived(name) }

func (a *app) launch(name string, args []string, in io.Reader) error {
	st, ok := a.cfg.Tools[name]
	if !ok {
		return fmt.Errorf("%s 尚未关联；请先执行 gate %s connect 或 install", name, name)
	}
	if _, err := os.Stat(st.BinaryPath); err != nil {
		return fmt.Errorf("已关联路径不可用: %w", err)
	}
	rc, err := a.fetchRuntime()
	if err != nil {
		return fmt.Errorf("启动前刷新设备配置: %w", err)
	}
	// 同 connect：cursor 的授权门在 prepareCursor，不看目录模型投影。
	if name != "cursor" && len(rc.Tools[name].Models) == 0 {
		return fmt.Errorf("这把 API Key 当前没有可用于 %s 的模型", name)
	}
	if err := a.prepareTool(name, st.BinaryPath, rc); err != nil {
		return a.withReconnectHint(err, name)
	}
	// 共享接入的 ensure 语义：桌面 App 重写过 config.toml 就在这里补回；
	// 补不回只告警，不拦 CLI 启动。
	if name == "codex" {
		if err := a.ensureSharedCodex(rc.Tools[name]); err != nil {
			fmt.Fprintf(a.err, "提示：共享接入未能刷新：%v\n", err)
		}
	}
	// cursor-agent 自更新经订阅面 RPC 拿 downloads.cursor.com 直链、绕过盒子，
	// 受管启动必须禁用（升级统一 gate cursor update）。恒注入在用户参数之前；
	// 用户重复传同一布尔开关无害。agent.v1 的设备选路由 prepareCursor 恒设的
	// useHttp1ForAgent 完成，不依赖 Cursor 的隐藏参数。生命周期子命令在 tool()
	// 已分流，launch 只处理透传启动。
	if name == "cursor" {
		args = append([]string{"--disable-auto-update"}, args...)
	}
	// Claude Code 把 cwd 项目的 .claude/settings.json（从家目录启动时正是使用者
	// 自己的 ~/.claude/settings.json）当作项目设置加载，其 env 覆盖 gate 注入的
	// 进程环境变量，足以把 ANTHROPIC_BASE_URL/AUTH_TOKEN 改道到别的中转。命令行
	// 层 --settings 高于项目设置，恒注入在用户参数之前：项目设置里权限、hooks
	// 等其余内容照常生效，使用者显式传入的 --settings/--model 位于其后，仍按
	// CLI 语义生效。文件内容由 prepareClaude 在每次启动前重新生成。
	if name == "claude" {
		args = append([]string{"--settings", filepath.Join(a.derivedDir("claude"), "cli-settings.json")}, args...)
	}
	cmd := exec.Command(st.BinaryPath, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, a.out, a.err
	cmd.Env = a.toolEnv(name)
	cmd.Dir, _ = os.Getwd()
	if a.lease != nil {
		a.lease.releaseOperation()
	}
	err = runTool(cmd)
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitStatusError{code: ee.ExitCode()}
	}
	return fmt.Errorf("启动 %s: %w", name, err)
}

type exitStatusError struct{ code int }

func (e exitStatusError) Error() string { return fmt.Sprintf("子进程退出码 %d", e.code) }
func exitCode(err error) int {
	var e exitStatusError
	if errors.As(err, &e) && e.code > 0 {
		return e.code
	}
	return 1
}

func (a *app) prepareTool(name, path string, rc runtimeConfig) error {
	if err := a.recordProfile(name); err != nil {
		return err
	}
	if err := a.prepareToolConfig(name, path, rc); err != nil {
		return err
	}
	return a.recordGenerated(name)
}

func (a *app) prepareToolConfig(name, path string, rc runtimeConfig) error {
	switch name {
	case "codex":
		return a.prepareCodex(path, rc.Tools[name])
	case "grok":
		return a.prepareGrok(rc.Tools[name])
	case "claude":
		return a.prepareClaude(rc.Tools[name])
	case "cursor":
		return a.prepareCursor(rc)
	case "opencode":
		return a.prepareOpenCode(path, rc.Tools[name])
	default:
		return errors.New("未知工具")
	}
}

func (a *app) derivedDir(name string) string {
	return filepath.Join(a.root, "tools", name, "derived")
}

func (a *app) toolEnv(name string) []string {
	env := append([]string(nil), os.Environ()...)
	set := func(k, v string) {
		prefix := k + "="
		for i := range env {
			if strings.HasPrefix(strings.ToUpper(env[i]), strings.ToUpper(prefix)) {
				env[i] = prefix + v
				return
			}
		}
		env = append(env, prefix+v)
	}
	switch name {
	case "codex":
		set("CODEX_HOME", a.derivedDir(name))
		set("LLMGATE_API_KEY", a.cfg.APIKey)
	case "grok":
		set("GROK_HOME", a.derivedDir(name))
	case "claude":
		set("CLAUDE_CONFIG_DIR", a.derivedDir(name))
		set("ANTHROPIC_BASE_URL", a.cfg.BaseURL+"/agents/claude")
		set("ANTHROPIC_AUTH_TOKEN", a.cfg.APIKey)
		set("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1")
		set("CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT", "1")
	case "cursor":
		// CURSOR_CONFIG_DIR 只管 cli-config.json/mcp.json，auth.json 恒在
		// $XDG_CONFIG_HOME/cursor/ 之下——所以磁盘登录态靠 memory 凭据库整体
		// 屏蔽（无 Key 时零网络请求、失败闭合），env Key 恒优先于登录文件；
		// 使用者原 ~/.cursor 与原厂登录零接触。Key 只进子进程环境。
		set("CURSOR_API_ENDPOINT", a.cfg.BaseURL+"/agents/cursor")
		set("CURSOR_API_KEY", a.cfg.APIKey)
		set("CURSOR_CONFIG_DIR", a.derivedDir(name))
		set("CURSOR_DATA_DIR", filepath.Join(a.derivedDir(name), "data"))
		set("AGENT_CLI_CREDENTIAL_STORE", "memory")
	case "opencode":
		configPath := filepath.Join(a.derivedDir(name), "opencode.json")
		xdgRoot := filepath.Join(a.derivedDir(name), "xdg")
		set("OPENCODE_CONFIG", configPath)
		set("OPENCODE_CONFIG_DIR", a.derivedDir(name))
		content := "{}"
		if body, err := os.ReadFile(configPath); err == nil && len(body) <= 2<<20 {
			content = string(body)
		}
		set("OPENCODE_CONFIG_CONTENT", content)
		set("LLMGATE_API_KEY", a.cfg.APIKey)
		// OpenCode always loads its XDG global configuration in addition to
		// OPENCODE_CONFIG. Give the gate profile isolated state roots so the
		// user's normal providers, plugins, sessions, and cache remain untouched.
		// Project configuration still loads, while the high-priority inline
		// config below fixes the enabled provider and active model.
		set("XDG_CONFIG_HOME", filepath.Join(xdgRoot, "config"))
		set("XDG_DATA_HOME", filepath.Join(xdgRoot, "data"))
		set("XDG_CACHE_HOME", filepath.Join(xdgRoot, "cache"))
		set("XDG_STATE_HOME", filepath.Join(xdgRoot, "state"))
	}
	return env
}

func tomlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// gateModelDisplayName 只用于菜单显示，不改请求模型 ID 或目录键。
// 设备可能已加前缀，也可能返回旧名称，重复刷新不能叠加品牌名。
func gateModelDisplayName(name string) string {
	name = strings.TrimSpace(name)
	for _, prefix := range []string{"LLM Gate · ", "SOC AGENT · "} {
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			name = strings.TrimSpace(name[len(prefix):])
		}
	}
	return "LLM Gate · " + name
}

func (a *app) prepareCodex(path string, tool runtimeTool) error {
	dir := a.derivedDir("codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	catalog, err := a.codexCatalog(path, tool)
	if err != nil {
		return err
	}
	catalogPath := filepath.Join(dir, "model-catalog.json")
	if err := a.writeCheckedCodexCatalog(path, catalog, catalogPath); err != nil {
		return err
	}
	configBody := fmt.Sprintf(`model_provider = "llmgate"
model = %s
model_catalog_json = %s

[model_providers.llmgate]
name = "LLM Gate"
base_url = %s
wire_api = "responses"
env_key = "LLMGATE_API_KEY"
http_headers = { %s = %s }
`, tomlQuote(tool.DefaultModel), tomlQuote(catalogPath), tomlQuote(a.cfg.BaseURL+"/agents/codex/v1"),
		tomlQuote(codexActorAuthorizationHeader), tomlQuote(codexActorAuthorizationValue))
	return atomicWrite(filepath.Join(dir, "config.toml"), []byte(configBody), 0o600)
}

type catalogEnvelope struct {
	Models []json.RawMessage `json:"models"`
	// SubscriptionOverlay 由设备下发，盖到 bundled 订阅模型条目上（codex.go）。
	SubscriptionOverlay *catalogOverlay `json:"subscription_overlay,omitempty"`
}

func rawSlug(raw json.RawMessage) string {
	var v struct {
		Slug string `json:"slug"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Slug
}

func (a *app) codexCatalog(path string, tool runtimeTool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "debug", "models", "--bundled")
	cmd.Env = a.toolEnv("codex")
	bundled, err := cmd.Output()
	if err != nil {
		return nil, codexUnusable(path, "无法导出 bundled 模型目录，需要升级 CLI："+execFailureDetail(err))
	}
	var base catalogEnvelope
	if err := json.Unmarshal(bundled, &base); err != nil {
		return nil, codexUnusable(path, fmt.Sprintf("导出的 bundled 模型目录形态未知：%v", err))
	}
	byName := map[string]json.RawMessage{}
	for _, raw := range base.Models {
		if name := rawSlug(raw); name != "" {
			byName[name] = raw
		}
	}
	extra, err := a.fetchCodexDeviceCatalog()
	if err != nil {
		return nil, err
	}
	fromDevice := map[string]bool{}
	for _, raw := range extra.Models {
		if name := rawSlug(raw); name != "" {
			byName[name] = raw
			fromDevice[name] = true
		}
	}
	wanted := make([]string, 0, len(tool.Models))
	for _, model := range tool.Models {
		wanted = append(wanted, model.Name)
		if _, ok := byName[model.Name]; !ok {
			return nil, codexUnusable(path, fmt.Sprintf("缺少模型 %q 的能力元数据，需要升级 CLI 或调整设备模型选择", model.Name))
		}
	}
	sort.Strings(wanted)
	out := catalogEnvelope{Models: make([]json.RawMessage, 0, len(wanted))}
	for _, name := range wanted {
		raw := byName[name]
		if !fromDevice[name] {
			// bundled 里的订阅条目是给 OpenAI 自家后端写的；盒子承受什么由
			// 设备覆盖片说了算（codex.go applyCatalogOverlay）。
			if raw, err = applyCatalogOverlay(raw, extra.SubscriptionOverlay); err != nil {
				return nil, codexUnusable(path, err.Error())
			}
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, codexUnusable(path, fmt.Sprintf("模型目录条目形态未知：%v", err))
		}
		var displayName string
		_ = json.Unmarshal(entry["display_name"], &displayName)
		if strings.TrimSpace(displayName) == "" {
			displayName = name
		}
		entry["display_name"], _ = json.Marshal(gateModelDisplayName(displayName))
		raw, err = json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		out.Models = append(out.Models, raw)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), err
}

func (a *app) fetchCodexDeviceCatalog() (catalogEnvelope, error) {
	var got catalogEnvelope
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+"/agents/codex/v1/model-catalog", nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return got, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return got, fmt.Errorf("读取 Codex 模型能力返回 HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&got)
	return got, err
}

func (a *app) prepareGrok(_ runtimeTool) error {
	dir := a.derivedDir("grok")
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+"/grok-helper/managed-config", nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("读取 Grok 受管配置返回 HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	// 固件保留 SOC AGENT 注释边界供已安装的 gate 校验；模型显示名由固件生成。
	if !bytes.Contains(b, []byte("SOC AGENT")) {
		return errors.New("设备返回的 Grok 配置形态未知")
	}
	return atomicWrite(filepath.Join(dir, "config.toml"), b, 0o600)
}

// prepareCursor 是 cursor 的授权门与派生配置生成器。cursor 恒订阅制（同
// Grok，无额外目录模型）：先要求该 Key 已勾选且订阅可用，再以本机 Key 打
// exchange 探针。设备返回的是带 sub 的设备本地 JWT，客户端 Key 与上游 token
// 都不落配置。探针通过后合并 cli-config.json：保留 Cursor 自己维护的模型选择、
// authInfo 与偏好，只清理由旧版 gate 写入的字符串 model，并恒打开官方
// HTTP/1.1 Agent 开关：Cursor 的 agent.v1 是双向 POST，当前客户 HTTPS 入口
// 链的 HTTP/2 转发会在正常 200 流中途被 CLI 判成 connection lost；HTTP/1.1
// 路径经真机端到端验证可用。任何失败都保留旧派生文件。
func (a *app) prepareCursor(rc runtimeConfig) error {
	sub := findSubscription(rc, "cursor")
	if !sub.Configured {
		return errors.New("这把 API Key 未勾选 Cursor 订阅；请在管理台「API密钥 → 可用订阅」勾选")
	}
	if !sub.Available {
		return errors.New("Cursor 订阅当前不可用；请在管理台「订阅接入」检查 Cursor 账号（勾选入口在「API密钥 → 可用订阅」）")
	}
	req, _ := http.NewRequest(http.MethodPost,
		a.cfg.BaseURL+"/agents/cursor/auth/exchange_user_api_key", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("Cursor 接入探针: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Cursor 接入探针返回 HTTP %d", resp.StatusCode)
	}
	var exchanged struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&exchanged); err != nil {
		return fmt.Errorf("Cursor 接入探针响应无效: %w", err)
	}
	if exchanged.AccessToken == a.cfg.APIKey || cursorTokenSubject(exchanged.AccessToken) == "" {
		return errors.New("Cursor 接入探针未返回带身份主体的本地令牌，拒绝生成派生配置")
	}
	dir := a.derivedDir("cursor")
	// CURSOR_DATA_DIR（会话/项目数据根）指向 derived/data，随准备预建。
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "cli-config.json")
	derived := map[string]json.RawMessage{}
	if existing, err := os.ReadFile(configPath); err == nil {
		if len(existing) > 2<<20 {
			return errors.New("现有 Cursor 派生配置过大，拒绝覆盖")
		}
		if err := json.Unmarshal(existing, &derived); err != nil || derived == nil {
			return errors.New("现有 Cursor 派生配置不是有效 JSON 对象，拒绝覆盖")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	derived["version"] = json.RawMessage("1")
	// Cursor 当前 schema 的 model 是对象。旧版 gate 写过字符串，Cursor 会把
	// 整份文件改名为 .bad；只移除这一种已知旧形态，让订阅面自行发现并保留
	// 用户此后选择的对象形 model。
	if raw, ok := derived["model"]; ok {
		var legacy string
		if json.Unmarshal(raw, &legacy) == nil {
			delete(derived, "model")
		}
	}
	network := map[string]json.RawMessage{}
	if raw, ok := derived["network"]; ok {
		if err := json.Unmarshal(raw, &network); err != nil || network == nil {
			return errors.New("现有 Cursor network 配置不是有效 JSON 对象，拒绝覆盖")
		}
	}
	network["useHttp1ForAgent"] = json.RawMessage("true")
	raw, err := json.Marshal(network)
	if err != nil {
		return err
	}
	derived["network"] = raw
	b, err := json.MarshalIndent(derived, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWrite(configPath, b, 0o600)
}

func cursorTokenSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > 16<<10 {
		return ""
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	claims.Subject = strings.TrimSpace(claims.Subject)
	if len(claims.Subject) > 1024 {
		return ""
	}
	return claims.Subject
}

func (a *app) prepareOpenCode(path string, tool runtimeTool) error {
	if len(tool.Models) == 0 || tool.DefaultModel == "" {
		return errors.New("OpenCode 配置缺少可见默认模型")
	}
	type openCodeModel struct {
		Name string `json:"name"`
	}
	type openCodeProvider struct {
		NPM     string                   `json:"npm"`
		Name    string                   `json:"name"`
		Options map[string]string        `json:"options"`
		Models  map[string]openCodeModel `json:"models"`
	}
	models := make(map[string]openCodeModel, len(tool.Models))
	for _, model := range tool.Models {
		if strings.TrimSpace(model.Name) == "" || model.Source != "catalog" {
			return fmt.Errorf("OpenCode 收到无效目录模型 %q", model.Name)
		}
		models[model.Name] = openCodeModel{Name: gateModelDisplayName(model.Name)}
	}
	if _, ok := models[tool.DefaultModel]; !ok {
		return errors.New("OpenCode 默认模型不在可见模型中")
	}
	providerID := a.openCodeProviderID()
	configBody := struct {
		Schema           string                      `json:"$schema"`
		Model            string                      `json:"model"`
		SmallModel       string                      `json:"small_model"`
		AutoUpdate       bool                        `json:"autoupdate"`
		EnabledProviders []string                    `json:"enabled_providers"`
		Provider         map[string]openCodeProvider `json:"provider"`
	}{
		Schema:           "https://opencode.ai/config.json",
		Model:            providerID + "/" + tool.DefaultModel,
		SmallModel:       providerID + "/" + tool.DefaultModel,
		AutoUpdate:       false,
		EnabledProviders: []string{providerID},
		Provider: map[string]openCodeProvider{
			providerID: {
				NPM:  "@ai-sdk/openai-compatible",
				Name: "LLM Gate",
				Options: map[string]string{
					"baseURL": a.cfg.BaseURL + "/v1",
					"apiKey":  "{env:LLMGATE_API_KEY}",
				},
				Models: models,
			},
		},
	}
	body, err := json.MarshalIndent(configBody, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	dir := a.derivedDir("opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".opencode-config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, path, "models", providerID)
	check.Dir = dir
	check.Env = envWith(a.toolEnv("opencode"), "OPENCODE_CONFIG", tmpName)
	check.Env = envWith(check.Env, "OPENCODE_CONFIG_CONTENT", string(body))
	if output, err := check.CombinedOutput(); err != nil {
		return openCodeUnusable(path, "拒绝生成的模型配置："+clippedWithout(output, a.cfg.APIKey))
	}
	return atomicWrite(filepath.Join(dir, "opencode.json"), body, 0o600)
}

// openCodeProviderID avoids a deep-merge collision with a project's own
// provider map while keeping the visible ID stable for one configured device.
// The suffix contains no credential material.
func (a *app) openCodeProviderID() string {
	sum := sha256.Sum256([]byte(a.cfg.BaseURL))
	return fmt.Sprintf("llmgate-%x", sum[:6])
}

func (a *app) prepareClaude(tool runtimeTool) error {
	if !strings.HasPrefix(a.cfg.BaseURL, "https://") {
		return errors.New("Claude Code 要求设备使用系统信任的 HTTPS 地址")
	}
	modelsReq, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+"/agents/claude/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	modelsReq.Header.Set("Anthropic-Version", "2023-06-01")
	modelsResp, err := a.hc.Do(modelsReq)
	if err != nil {
		return fmt.Errorf("Claude Code 模型发现: %w", err)
	}
	defer modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(modelsResp.Body, 4096))
		return fmt.Errorf("Claude Code 模型发现返回 HTTP %d", modelsResp.StatusCode)
	}
	var discovered struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(modelsResp.Body, 4<<20)).Decode(&discovered); err != nil {
		return fmt.Errorf("Claude Code 模型发现响应无效: %w", err)
	}
	seen := make(map[string]string, len(discovered.Data))
	for _, model := range discovered.Data {
		seen[model.ID] = model.DisplayName
	}
	clientModels := make([]string, 0, len(tool.Models))
	defaultModel := ""
	for _, model := range tool.Models {
		clientID := claudeClientModelID(model)
		if _, ok := seen[clientID]; !ok {
			return fmt.Errorf("Claude Code 模型发现缺少授权模型 %q", model.Name)
		}
		clientModels = append(clientModels, clientID)
		if model.Name == tool.DefaultModel {
			defaultModel = clientID
		}
	}
	if defaultModel == "" {
		return errors.New("Claude Code 配置缺少可见默认模型")
	}
	clientModels = collapseClaudeSubscriptionAliases(clientModels, tool.Models, defaultModel)
	// Keep the device-selected model first without disturbing the relative
	// order of the remaining gateway choices.
	if i := slices.Index(clientModels, defaultModel); i > 0 {
		copy(clientModels[1:i+1], clientModels[:i])
		clientModels[0] = defaultModel
	}
	req, _ := http.NewRequest(http.MethodHead, a.cfg.BaseURL+"/agents/claude/api/hello", nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("Claude Code 接入探针: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Claude Code 接入探针返回 HTTP %d", resp.StatusCode)
	}
	env := map[string]string{
		"ANTHROPIC_DEFAULT_MODEL":                              defaultModel,
		"CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": "1",
	}
	selected := make(map[string]bool, len(clientModels))
	for _, id := range clientModels {
		selected[id] = true
	}
	for _, model := range tool.Models {
		clientID := claudeClientModelID(model)
		family := claudeModelFamily(model)
		if family == "" || !selected[clientID] || env["ANTHROPIC_DEFAULT_"+family+"_MODEL"] != "" {
			continue
		}
		prefix := "ANTHROPIC_DEFAULT_" + family + "_MODEL"
		env[prefix] = clientID
		displayName := strings.TrimSpace(seen[clientID])
		if displayName == "" {
			displayName = model.Name
		}
		env[prefix+"_NAME"] = gateModelDisplayName(displayName)
		env[prefix+"_DESCRIPTION"] = "From LLM Gate"
	}
	writeSettings := func(file string, env map[string]string) error {
		settings := struct {
			Model           string            `json:"model"`
			AvailableModels []string          `json:"availableModels"`
			Env             map[string]string `json:"env"`
		}{
			// The explicit Default setting keeps Claude Code's first row on the
			// device-selected model. Map each visible Claude family to its own
			// gateway model so the picker does not manufacture several differently
			// labelled aliases that all point at one default model.
			Model:           defaultModel,
			AvailableModels: clientModels,
			Env:             env,
		}
		b, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		return atomicWrite(filepath.Join(a.derivedDir("claude"), file), b, 0o600)
	}
	if err := writeSettings("settings.json", env); err != nil {
		return err
	}
	// cli-settings.json is injected by launch() as a command-line --settings
	// layer, which outranks the project settings Claude Code loads from the
	// cwd's .claude/settings.json (when launched from the home directory that
	// file is the user's own ~/.claude/settings.json). Settings-file env beats
	// process env, so connectivity, credentials and model mapping must all be
	// pinned here or a project file could redirect requests to another relay.
	// An empty string clears a credential var (verified against the real CLI:
	// empty means unset), so the two vars gate does not use are forced empty
	// instead of inherited. The API key therefore lives in this derived file;
	// it carries the same 0600 protection as gate's own config.json.
	cliEnv := make(map[string]string, len(env)+6)
	for k, v := range env {
		cliEnv[k] = v
	}
	cliEnv["ANTHROPIC_BASE_URL"] = a.cfg.BaseURL + "/agents/claude"
	cliEnv["ANTHROPIC_AUTH_TOKEN"] = a.cfg.APIKey
	cliEnv["ANTHROPIC_API_KEY"] = ""
	cliEnv["CLAUDE_CODE_OAUTH_TOKEN"] = ""
	cliEnv["ANTHROPIC_MODEL"] = defaultModel
	cliEnv["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	return writeSettings("cli-settings.json", cliEnv)
}

func claudeModelFamily(model runtimeModel) string {
	if model.Source != "subscription" || !strings.HasPrefix(model.Name, "claude-") {
		return ""
	}
	for _, family := range []string{"fable", "opus", "sonnet", "haiku"} {
		if strings.Contains(model.Name, "-"+family+"-") || strings.HasSuffix(model.Name, "-"+family) {
			return strings.ToUpper(family)
		}
	}
	return ""
}

// collapseClaudeSubscriptionAliases removes the duplicate picker row produced
// when Anthropic exposes one unversioned alias and exactly one dated ID for the
// same subscription model. Keep an explicitly selected alias as the default;
// otherwise prefer the dated row because gateway discovery supplies its proper
// display name. Multiple dated versions remain distinct choices.
func collapseClaudeSubscriptionAliases(ids []string, models []runtimeModel, defaultID string) []string {
	if len(ids) != len(models) {
		return ids
	}
	aliases := make(map[string]int)
	dated := make(map[string][]int)
	for i, model := range models {
		if model.Source != "subscription" || !strings.HasPrefix(ids[i], "claude-") {
			continue
		}
		if base, ok := claudeDatedModelBase(ids[i]); ok {
			dated[base] = append(dated[base], i)
		} else {
			aliases[ids[i]] = i
		}
	}
	drop := make([]bool, len(ids))
	for base, versions := range dated {
		alias, ok := aliases[base]
		if !ok || len(versions) != 1 {
			continue
		}
		if ids[alias] == defaultID {
			drop[versions[0]] = true
		} else {
			drop[alias] = true
		}
	}
	out := make([]string, 0, len(ids))
	for i, id := range ids {
		if !drop[i] {
			out = append(out, id)
		}
	}
	return out
}

func claudeDatedModelBase(id string) (string, bool) {
	dash := strings.LastIndexByte(id, '-')
	if dash <= len("claude-") || len(id)-dash-1 != 8 {
		return "", false
	}
	for _, c := range id[dash+1:] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return id[:dash], true
}

func clipped(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		return "自检失败"
	}
	return s
}

func clippedWithout(body []byte, secret string) string {
	if secret != "" {
		body = bytes.ReplaceAll(body, []byte(secret), []byte("[redacted]"))
	}
	return clipped(body)
}

func codexUnusable(path, reason string) error {
	return &cliUnusableError{tool: "Codex", path: path, version: detectVersion(path), reason: reason}
}

func openCodeUnusable(path, reason string) error {
	return &cliUnusableError{tool: "OpenCode", path: path, version: detectVersion(path), reason: reason}
}

// execFailureDetail prefers the subprocess's own stderr so output like
// "unrecognized subcommand" from an outdated CLI reaches the user.
func execFailureDetail(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
		return clipped(exit.Stderr)
	}
	return err.Error()
}

var grokVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9._]+)?$`)

const (
	grokCLIBasePath     = "/grok-helper/cli"
	grokCLIMaxBytes     = 512 << 20
	grokCLITimeout      = 60 * time.Minute
	claudeCLIBasePath   = "/claude-helper/cli"
	claudeCLIMaxBytes   = 512 << 20
	claudeCLITimeout    = 60 * time.Minute
	openCodeCLIBasePath = "/opencode-helper/cli"
	openCodeCLIMaxBytes = 512 << 20
	openCodeCLITimeout  = 60 * time.Minute
	// cursor 整树归档当前压缩 ~84MB；上限与盒子 /cursor-helper/cli/* 透传的
	// 512MiB 一致，超时同样覆盖慢链路整树下载。
	cursorCLIBasePath = "/cursor-helper/cli"
	cursorCLIMaxBytes = 512 << 20
	cursorCLITimeout  = 60 * time.Minute
)

// cursorCLIMaxTreeBytes 限制整树解包总量（当前制品解开 ~190MB），防解压炸弹。
// var 仅供测试收紧验证上限执行。
var cursorCLIMaxTreeBytes int64 = 2 << 30

var (
	claudeVersionRE   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z._-]+)?$`)
	claudeChecksumRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	openCodeVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z._-]+)?$`)
	openCodeDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type claudeReleaseManifest struct {
	Version   string                           `json:"version"`
	Platforms map[string]claudeReleasePlatform `json:"platforms"`
}

type claudeReleasePlatform struct {
	Binary   string `json:"binary"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

type openCodeRelease struct {
	TagName    string                 `json:"tag_name"`
	Draft      bool                   `json:"draft"`
	Prerelease bool                   `json:"prerelease"`
	Assets     []openCodeReleaseAsset `json:"assets"`
}

type openCodeReleaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// managedBinaryPath 是 gate 受管安装物的固定落点：单文件工具在 tools/<name>/bin/
// 下；cursor 是整目录 Node 运行时包，入口脚本在 tools/cursor/app/ 树里。
func (a *app) managedBinaryPath(name string) string {
	if name == "cursor" {
		return filepath.Join(a.root, "tools", "cursor", "app", toolBinaryName("cursor"))
	}
	return filepath.Join(a.root, "tools", name, "bin", toolBinaryName(name))
}

func (a *app) install(name string, adopt, update bool) error {
	current, linked := a.cfg.Tools[name]
	if update && linked && current.Origin == "external" && !adopt {
		return fmt.Errorf("%s 由外部渠道管理；请沿原渠道升级，或显式使用 update --adopt", name)
	}
	// Each reuse step below falls through to the next source only when the
	// failure is pinned to that binary (isCLIUnusable); anything else — device
	// errors, policy refusals — surfaces immediately.
	if !update && linked {
		if st, err := os.Stat(current.BinaryPath); err == nil && !st.IsDir() {
			err := a.connect(name, current.BinaryPath, current.Origin)
			if err == nil || !isCLIUnusable(err) {
				return err
			}
			fmt.Fprintf(a.err, "已关联的 %s 不可用，改从 PATH 与设备重新解析：%v\n", name, err)
		}
	}
	if !update && !adopt && (!linked || current.Origin == "external") {
		if len(lookPathAll(toolBinaryName(name))) > 0 {
			err := a.connect(name, "", "external")
			if err == nil || !isCLIUnusable(err) {
				return err
			}
			fmt.Fprintf(a.err, "%v\n改为经设备安装受管 %s。\n", err, name)
		}
		path := ""
		switch name {
		case "grok", "claude", "opencode", "cursor":
			path = a.managedBinaryPath(name)
		case "codex":
			path = a.managedCodexPath()
		}
		if path != "" {
			if st, err := os.Stat(path); err == nil && !st.IsDir() && !(linked && current.BinaryPath == path) {
				err := a.connect(name, path, "gate")
				if err == nil || !isCLIUnusable(err) {
					return err
				}
				fmt.Fprintf(a.err, "受管 %s 不可用，改为重新安装：%v\n", name, err)
			}
		}
	}
	if update && !linked && !adopt {
		if _, err := exec.LookPath(toolBinaryName(name)); err == nil {
			return fmt.Errorf("%s 尚未由 gate 管理；请沿原渠道升级，或显式使用 update --adopt", name)
		}
		return fmt.Errorf("%s 尚未安装；请先执行 gate %s install", name, name)
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	var path string
	var err error
	if name == "grok" {
		path, err = a.installGrok()
	} else if name == "claude" {
		path, err = a.installClaude()
	} else if name == "opencode" {
		path, err = a.installOpenCode()
	} else if name == "cursor" {
		path, err = a.installCursor()
	} else {
		path, err = a.installCodex()
	}
	if err != nil {
		return err
	}
	if err := a.recordProgram(name, path); err != nil {
		return err
	}
	return a.connect(name, path, "gate")
}

func (a *app) installClaude() (string, error) {
	platform, err := currentClaudePlatform()
	if err != nil {
		return "", err
	}
	if a.out != nil {
		fmt.Fprintln(a.out, "正在经设备读取 Claude Code 官方 release…")
	}
	latest, err := a.getClaudePublic("latest", 4096)
	if err != nil {
		return "", fmt.Errorf("读取 Claude Code 最新版本: %w", err)
	}
	version := strings.TrimSpace(string(latest))
	if !claudeVersionRE.MatchString(version) {
		return "", errors.New("设备返回的 Claude Code 版本形态未知")
	}
	manifestBody, err := a.getClaudePublic(version+"/manifest.json", 2<<20)
	if err != nil {
		return "", fmt.Errorf("读取 Claude Code release manifest: %w", err)
	}
	var manifest claudeReleaseManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		return "", fmt.Errorf("解析 Claude Code release manifest: %w", err)
	}
	if manifest.Version != version {
		return "", errors.New("Claude Code release manifest 版本不匹配")
	}
	release, ok := manifest.Platforms[platform]
	if !ok {
		return "", fmt.Errorf("Claude Code release 没有 %s 平台", platform)
	}
	wantBinary := "claude"
	if strings.HasPrefix(platform, "win32-") {
		wantBinary = "claude.exe"
	}
	if release.Binary != wantBinary || !claudeChecksumRE.MatchString(release.Checksum) ||
		release.Size <= 0 || release.Size > claudeCLIMaxBytes {
		return "", errors.New("Claude Code release manifest 的平台条目无效")
	}
	if a.out != nil {
		fmt.Fprintf(a.out, "正在经设备下载 Claude Code %s（约 %d MiB）…\n", version, (release.Size+(1<<20)-1)/(1<<20))
	}
	path, err := a.downloadClaudeBinary(version, platform, release)
	if err != nil {
		return "", err
	}
	if a.out != nil {
		fmt.Fprintln(a.out, "Claude Code 官方制品校验通过。")
	}
	return path, nil
}

func currentClaudePlatform() (string, error) {
	musl, translated := false, false
	if runtime.GOOS == "linux" {
		for _, path := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
			if _, err := os.Stat(path); err == nil {
				musl = true
				break
			}
		}
		if !musl {
			if body, err := exec.Command("ldd", "/bin/ls").CombinedOutput(); err == nil || len(body) > 0 {
				musl = bytes.Contains(bytes.ToLower(body), []byte("musl"))
			}
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		body, err := exec.Command("sysctl", "-in", "sysctl.proc_translated").Output()
		translated = err == nil && strings.TrimSpace(string(body)) == "1"
	}
	return claudePlatform(runtime.GOOS, runtime.GOARCH, musl, translated)
}

func claudePlatform(goos, goarch string, musl, translated bool) (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return "", fmt.Errorf("Claude Code 没有 %s/%s 的官方安装物", goos, goarch)
	}
	switch goos {
	case "darwin":
		if translated {
			arch = "arm64"
		}
		return "darwin-" + arch, nil
	case "linux":
		platform := "linux-" + arch
		if musl {
			platform += "-musl"
		}
		return platform, nil
	case "windows":
		return "win32-" + arch, nil
	default:
		return "", fmt.Errorf("Claude Code 没有 %s/%s 的官方安装物", goos, goarch)
	}
}

func currentOpenCodeAsset() (string, error) {
	musl, translated := false, false
	if runtime.GOOS == "linux" {
		for _, path := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
			if _, err := os.Stat(path); err == nil {
				musl = true
				break
			}
		}
		if !musl {
			if body, err := exec.Command("ldd", "/bin/ls").CombinedOutput(); err == nil || len(body) > 0 {
				musl = bytes.Contains(bytes.ToLower(body), []byte("musl"))
			}
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		body, err := exec.Command("sysctl", "-in", "sysctl.proc_translated").Output()
		translated = err == nil && strings.TrimSpace(string(body)) == "1"
	}
	return openCodeAsset(runtime.GOOS, runtime.GOARCH, musl, translated)
}

func openCodeAsset(goos, goarch string, musl, translated bool) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("OpenCode 没有 %s/%s 的官方安装物", goos, goarch)
	}
	switch goos {
	case "darwin":
		if goarch == "arm64" || translated {
			return "opencode-darwin-arm64.zip", nil
		}
		return "opencode-darwin-x64-baseline.zip", nil
	case "linux":
		if goarch == "arm64" {
			name := "opencode-linux-arm64"
			if musl {
				name += "-musl"
			}
			return name + ".tar.gz", nil
		}
		name := "opencode-linux-x64-baseline"
		if musl {
			name += "-musl"
		}
		return name + ".tar.gz", nil
	case "windows":
		if goarch == "arm64" {
			return "opencode-windows-arm64.zip", nil
		}
		return "opencode-windows-x64-baseline.zip", nil
	default:
		return "", fmt.Errorf("OpenCode 没有 %s/%s 的官方安装物", goos, goarch)
	}
}

func (a *app) installOpenCode() (string, error) {
	assetName, err := currentOpenCodeAsset()
	if err != nil {
		return "", err
	}
	if a.out != nil {
		fmt.Fprintln(a.out, "正在经设备读取 OpenCode 官方 release…")
	}
	body, err := a.getOpenCodePublic("latest", 4<<20)
	if err != nil {
		return "", fmt.Errorf("读取 OpenCode 最新 release: %w", err)
	}
	var release openCodeRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("解析 OpenCode release: %w", err)
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if release.TagName != "v"+version || !openCodeVersionRE.MatchString(version) || release.Draft || release.Prerelease {
		return "", errors.New("设备返回的 OpenCode 稳定版本形态未知")
	}
	var asset openCodeReleaseAsset
	for _, candidate := range release.Assets {
		if candidate.Name == assetName {
			asset = candidate
			break
		}
	}
	if asset.Name == "" {
		return "", fmt.Errorf("OpenCode release 没有 %s", assetName)
	}
	if asset.Size <= 0 || asset.Size > openCodeCLIMaxBytes || !openCodeDigestRE.MatchString(asset.Digest) {
		return "", errors.New("OpenCode release 的平台条目无效")
	}
	if a.out != nil {
		fmt.Fprintf(a.out, "正在经设备下载 OpenCode %s（约 %d MiB）…\n", version, (asset.Size+(1<<20)-1)/(1<<20))
	}
	path, err := a.downloadOpenCodeBinary(version, asset)
	if err != nil {
		return "", err
	}
	if a.out != nil {
		fmt.Fprintln(a.out, "OpenCode 官方制品校验通过。")
	}
	return path, nil
}

func (a *app) openCodeHTTPClient() *http.Client {
	client := *a.hc
	client.Timeout = openCodeCLITimeout
	return &client
}

func (a *app) getOpenCodePublic(path string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+openCodeCLIBasePath+"/"+path, nil)
	resp, err := a.openCodeHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return body, err
}

func (a *app) downloadOpenCodeBinary(version string, asset openCodeReleaseAsset) (string, error) {
	dir := filepath.Join(a.root, "tools", "opencode", "bin")
	digest, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok {
		return "", errors.New("OpenCode release 元数据的摘要形态未知")
	}
	archivePath, err := a.fetchArtifact(artifactFetch{
		client: a.openCodeHTTPClient(),
		url:    a.cfg.BaseURL + openCodeCLIBasePath + "/" + fmt.Sprintf("releases/%s/%s", version, asset.Name),
		label:  "OpenCode",
		dir:    dir,
		prefix: ".opencode-archive",
		size:   asset.Size,
		digest: digest,
		max:    openCodeCLIMaxBytes,
		mode:   0o600,
	})
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)

	pattern := ".opencode-download-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o700); err != nil {
		return "", err
	}
	if err := securePath(tmpPath, false); err != nil {
		return "", err
	}
	if err := extractOpenCodeBinary(archivePath, asset.Name, tmp); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if detectVersionWithEnv(tmpPath, installerEnv()) == "unknown" {
		return "", errors.New("取回的 OpenCode 程序无法运行")
	}
	target := filepath.Join(dir, toolBinaryName("opencode"))
	if err := replaceFile(tmpPath, target); err != nil {
		return "", err
	}
	committed = true
	return target, nil
}

func extractOpenCodeBinary(archivePath, assetName string, dst io.Writer) error {
	want := "opencode"
	if strings.HasPrefix(assetName, "opencode-windows-") {
		want = "opencode.exe"
	}
	copyEntry := func(name string, mode os.FileMode, reader io.Reader) (bool, error) {
		if filepath.ToSlash(name) != want {
			return false, nil
		}
		if !mode.IsRegular() {
			return true, errors.New("OpenCode 归档中的程序不是普通文件")
		}
		n, err := io.Copy(dst, io.LimitReader(reader, openCodeCLIMaxBytes+1))
		if err != nil {
			return true, err
		}
		if n == 0 || n > openCodeCLIMaxBytes {
			return true, errors.New("OpenCode 归档中的程序大小非法")
		}
		return true, nil
	}
	if strings.HasSuffix(assetName, ".tar.gz") {
		file, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer file.Close()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("打开 OpenCode gzip: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("读取 OpenCode tar: %w", err)
			}
			found, err := copyEntry(header.Name, header.FileInfo().Mode(), tr)
			if found {
				return err
			}
		}
		return errors.New("OpenCode 归档缺少程序")
	}
	if strings.HasSuffix(assetName, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return fmt.Errorf("打开 OpenCode zip: %w", err)
		}
		defer zr.Close()
		for _, entry := range zr.File {
			if filepath.ToSlash(entry.Name) != want {
				continue
			}
			reader, err := entry.Open()
			if err != nil {
				return err
			}
			found, copyErr := copyEntry(entry.Name, entry.Mode(), reader)
			closeErr := reader.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if found {
				return nil
			}
		}
		return errors.New("OpenCode 归档缺少程序")
	}
	return errors.New("OpenCode 归档格式未知")
}

func (a *app) claudeHTTPClient() *http.Client {
	client := *a.hc
	client.Timeout = claudeCLITimeout
	return &client
}

func (a *app) getClaudePublic(path string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+claudeCLIBasePath+"/"+path, nil)
	resp, err := a.claudeHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return body, err
}

func (a *app) downloadClaudeBinary(version, platform string, release claudeReleasePlatform) (string, error) {
	dir := filepath.Join(a.root, "tools", "claude", "bin")
	tmpPath, err := a.fetchArtifact(artifactFetch{
		client: a.claudeHTTPClient(),
		url:    a.cfg.BaseURL + claudeCLIBasePath + "/" + version + "/" + platform + "/" + release.Binary,
		label:  "Claude Code",
		dir:    dir,
		prefix: ".claude-download",
		size:   release.Size,
		digest: release.Checksum,
		max:    claudeCLIMaxBytes,
		mode:   0o700,
	})
	if err != nil {
		return "", err
	}
	if detectVersionWithEnv(tmpPath, installerEnv()) == "unknown" {
		_ = os.Remove(tmpPath)
		return "", errors.New("取回的 Claude Code 程序无法运行")
	}
	target := filepath.Join(dir, toolBinaryName("claude"))
	if err := replaceFile(tmpPath, target); err != nil {
		return "", err
	}
	return target, nil
}

// 官方 Cursor 安装脚本把版本钉死在 DOWNLOAD_URL 行里（真实脚本行形态）：
//
//	DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.08.11-e8db854/${OS}/${ARCH}/agent-cli-package.tar.gz"
//
// 正则钉死主机、路径形态与版本形态（YYYY.MM.DD-hex，同盒子白名单）；结构对不上
// 即失败闭合，绝不猜测性改写（同 Codex 安装器纪律）。
var cursorInstallerURLRE = regexp.MustCompile(`(?m)^[ \t]*DOWNLOAD_URL="https://downloads\.cursor\.com/lab/([0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{5,12})/\$\{OS\}/\$\{ARCH\}/agent-cli-package\.tar\.gz"[ \t]*\r?$`)

func parseCursorInstallerVersion(script string) (string, error) {
	matches := cursorInstallerURLRE.FindAllStringSubmatch(script, -1)
	if len(matches) == 0 {
		return "", errors.New("官方 Cursor 安装脚本契约变化，拒绝猜测性安装；可先按官方方式安装 cursor-agent，再执行 gate cursor connect")
	}
	version := matches[0][1]
	for _, match := range matches[1:] {
		if match[1] != version {
			return "", errors.New("官方 Cursor 安装脚本内版本不一致，拒绝猜测性安装")
		}
	}
	return version, nil
}

// cursorCLIAssetPath 拼当前平台在盒子 /cursor-helper/cli/ 下的整树归档路径：
// lab/<版本>/<os>/<arch>/agent-cli-package.tar.gz|zip（windows 用 zip），与固件
// 白名单逐段一致。
func cursorCLIAssetPath(goos, goarch, version string) (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	asset := map[string]string{
		"linux":   "agent-cli-package.tar.gz",
		"darwin":  "agent-cli-package.tar.gz",
		"windows": "agent-cli-package.zip",
	}[goos]
	if arch == "" || asset == "" {
		return "", fmt.Errorf("Cursor CLI 没有 %s/%s 的官方安装物", goos, goarch)
	}
	return "lab/" + version + "/" + goos + "/" + arch + "/" + asset, nil
}

// cursorVersionNewer 只按 YYYY.MM.DD 日期前缀判新旧；同日不同 hex 后缀无法排
// 序，调用方只据它选提示措辞，是否重装看完整串是否相等。
func cursorVersionNewer(candidate, current string) bool {
	if len(candidate) < 10 || len(current) < 10 {
		return false
	}
	return candidate[:10] > current[:10]
}

func (a *app) cursorHTTPClient() *http.Client {
	client := *a.hc
	client.Timeout = cursorCLITimeout
	return &client
}

func (a *app) getCursorPublic(path string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+cursorCLIBasePath+"/"+path, nil)
	resp, err := a.cursorHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return body, err
}

// installCursor 经盒子受管安装官方 Cursor CLI 整树制品。官方没有独立版本清单
// 端点，脚本与制品也没有上游 SHA-256 清单可核（同 Grok 安装链的完整性水位）：
// 版本从安装脚本正文解析，完整性依赖 gzip/zip 流式自校验加暂存树 --version
// 与解析版本严格相等；暂存与原子切换都在 tools/cursor/ 同一文件系统内完成，
// 任何失败保留现有 app/ 树。
func (a *app) installCursor() (string, error) {
	if a.out != nil {
		fmt.Fprintln(a.out, "正在经设备读取 Cursor CLI 官方安装脚本…")
	}
	script, err := a.getCursorPublic("install.sh", 4<<20)
	if err != nil {
		return "", fmt.Errorf("读取 Cursor CLI 官方安装脚本: %w", err)
	}
	version, err := parseCursorInstallerVersion(string(script))
	if err != nil {
		return "", err
	}
	assetPath, err := cursorCLIAssetPath(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		return "", err
	}
	target := a.managedBinaryPath("cursor")
	current := detectVersionWithEnv(target, installerEnv())
	if current == version {
		if a.out != nil {
			fmt.Fprintf(a.out, "受管 Cursor CLI 已是官方当前版本 %s。\n", version)
		}
		return target, nil
	}
	if a.out != nil {
		switch {
		case current == "unknown":
			fmt.Fprintf(a.out, "正在经设备下载 Cursor CLI %s 整树制品…\n", version)
		case cursorVersionNewer(version, current):
			fmt.Fprintf(a.out, "正在把受管 Cursor CLI 从 %s 升级到 %s…\n", current, version)
		default:
			fmt.Fprintf(a.out, "正在按官方当前版本重装受管 Cursor CLI（%s → %s）…\n", current, version)
		}
	}
	dir := filepath.Join(a.root, "tools", "cursor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := securePath(dir, true); err != nil {
		return "", err
	}
	cleanupCursorInstallLeftovers(dir)
	archivePath, err := a.downloadCursorArchive(assetPath, dir)
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)
	staging, err := os.MkdirTemp(dir, "app.tmp-")
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(staging)
		}
	}()
	if err := extractCursorTree(archivePath, strings.HasSuffix(assetPath, ".zip"), staging); err != nil {
		return "", err
	}
	// 版本完整性的唯一闸门：暂存树入口的 --version 必须与脚本钉死版本严格相等。
	entry := filepath.Join(staging, toolBinaryName("cursor"))
	if got := detectVersionWithEnv(entry, installerEnv()); got != version {
		return "", fmt.Errorf("Cursor CLI 制品自检版本 %q 与官方脚本钉死的 %q 不一致，已丢弃暂存树", got, version)
	}
	appDir := filepath.Join(dir, "app")
	backup := ""
	if _, err := os.Lstat(appDir); err == nil {
		backup = filepath.Join(dir, "app.old-"+strings.TrimPrefix(filepath.Base(staging), "app.tmp-"))
		if err := os.Rename(appDir, backup); err != nil {
			return "", fmt.Errorf("备份现有 Cursor CLI 安装树: %w", err)
		}
	}
	if err := os.Rename(staging, appDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, appDir)
		}
		return "", fmt.Errorf("切换 Cursor CLI 安装树: %w", err)
	}
	committed = true
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	if a.out != nil {
		fmt.Fprintf(a.out, "Cursor CLI %s 制品自检通过。\n", version)
	}
	return target, nil
}

// cleanupCursorInstallLeftovers 清掉上次中断安装留下的暂存树、备份树与归档临时
// 文件。app.old-* 只在 app/ 树存在时才清：切换双双失败时它可能是唯一副本。
func cleanupCursorInstallLeftovers(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	_, appErr := os.Lstat(filepath.Join(dir, "app"))
	for _, entry := range entries {
		name := entry.Name()
		// *.part 是续传件，恰恰要留到下一次接着传。换了版本的旧续传件由
		// fetch.go 在取回成功后清。
		if strings.HasSuffix(name, ".part") {
			continue
		}
		stale := strings.HasPrefix(name, "app.tmp-") || strings.HasPrefix(name, ".cursor-archive-")
		if strings.HasPrefix(name, "app.old-") && appErr == nil {
			stale = true
		}
		if stale {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}
}

// downloadCursorArchive 把整树归档取进 dir 下的续传件（与最终 app/ 同一文件
// 系统），返回该文件路径；链路被掐时保留断点，见 fetch.go。
func (a *app) downloadCursorArchive(requestPath, dir string) (string, error) {
	return a.fetchArtifact(artifactFetch{
		client: a.cursorHTTPClient(),
		url:    a.cfg.BaseURL + cursorCLIBasePath + "/" + requestPath,
		label:  "Cursor CLI 整树归档",
		dir:    dir,
		prefix: ".cursor-archive",
		max:    cursorCLIMaxBytes,
		mode:   0o600,
	})
}

func extractCursorTree(archivePath string, zipFormat bool, staging string) error {
	w := &archiveTreeWriter{label: "Cursor CLI", stripRoot: true, staging: staging, symlinks: map[string]bool{}, budget: cursorCLIMaxTreeBytes}
	if zipFormat {
		return extractCursorZip(archivePath, w)
	}
	return extractTarGzTree(archivePath, w)
}

func extractCursorZip(archivePath string, w *archiveTreeWriter) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开 %s zip: %w", w.label, err)
	}
	defer zr.Close()
	seen := false
	for _, entry := range zr.File {
		isDir := entry.FileInfo().IsDir()
		if !isDir && !entry.Mode().IsRegular() {
			return fmt.Errorf("%s 归档条目 %q 类型不支持，拒绝安装", w.label, entry.Name)
		}
		rel, ok, err := w.rel(entry.Name, isDir)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		seen = true
		if isDir {
			if err := w.dir(rel); err != nil {
				return err
			}
			continue
		}
		r, err := entry.Open()
		if err != nil {
			return err
		}
		err = w.file(rel, entry.Mode(), r)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if !seen {
		return fmt.Errorf("%s 归档为空，拒绝安装", w.label)
	}
	return nil
}

func installerEnv() []string {
	return appendWithout(os.Environ(),
		"LLMGATE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY",
		"CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
		"OPENCODE_CONFIG_CONTENT",
		"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN", "CURSOR_API_ENDPOINT",
		"CURSOR_CONFIG_DIR", "CURSOR_DATA_DIR", "AGENT_CLI_CREDENTIAL_STORE")
}

func (a *app) installGrok() (string, error) {
	b, err := a.getGrokPublic("stable", 4096)
	if err != nil {
		return "", err
	}
	ver := strings.TrimSpace(string(b))
	if !grokVersionRE.MatchString(ver) {
		return "", errors.New("设备返回的 Grok 版本形态未知")
	}
	osName := map[string]string{"darwin": "macos", "linux": "linux", "windows": "windows"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("Grok 没有 %s/%s 的盒子安装物", runtime.GOOS, runtime.GOARCH)
	}
	asset := fmt.Sprintf("grok-%s-%s-%s", ver, osName, arch)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	if a.out != nil {
		fmt.Fprintf(a.out, "正在经设备下载 Grok %s…\n", ver)
	}
	return a.downloadGrokBinary(asset)
}

func (a *app) grokHTTPClient() *http.Client {
	client := *a.hc
	client.Timeout = grokCLITimeout
	return &client
}

func (a *app) getGrokPublic(path string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+grokCLIBasePath+"/"+path, nil)
	resp, err := a.grokHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(b)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return b, err
}

// downloadGrokBinary 把官方单文件制品取进目标目录内的续传件（链路被掐时保留
// 断点，见 fetch.go），完成版本自检后再原子替换。普通配置/API 请求仍使用短
// 超时；只有安装物使用长时限。
func (a *app) downloadGrokBinary(asset string) (string, error) {
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	tmpPath, err := a.fetchArtifact(artifactFetch{
		client: a.grokHTTPClient(),
		url:    a.cfg.BaseURL + grokCLIBasePath + "/" + asset,
		label:  "Grok",
		dir:    dir,
		prefix: ".grok-download",
		max:    grokCLIMaxBytes,
		mode:   0o700,
	})
	if err != nil {
		return "", err
	}
	if detectVersionWithEnv(tmpPath, installerEnv()) == "unknown" {
		// 官方没给 grok 发 SHA-256 清单，跑不跑得起来是这条链上唯一的完整性
		// 闸门；没过的件不能留着当下次的续传起点。
		_ = os.Remove(tmpPath)
		return "", errors.New("取回的 Grok 程序无法运行")
	}
	target := filepath.Join(dir, toolBinaryName("grok"))
	if err := replaceFile(tmpPath, target); err != nil {
		return "", err
	}
	return target, nil
}

func appendWithout(env []string, names ...string) []string {
	result := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(names, func(blocked string) bool { return strings.EqualFold(name, blocked) }) {
			result = append(result, kv)
		}
	}
	return result
}

func envWith(env []string, name, value string) []string {
	env = appendWithout(env, name)
	return append(env, name+"="+value)
}

func (a *app) getPublic(path string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+path, nil)
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(b)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return b, err
}

func readSecret(prompt string, errOut io.Writer) (string, error) {
	fmt.Fprint(errOut, prompt)
	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			`$s=Read-Host -AsSecureString; $b=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($s); try {[Runtime.InteropServices.Marshal]::PtrToStringBSTR($b)} finally {[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($b)}`)
		cmd.Stderr = errOut
		b, err := cmd.Output()
		fmt.Fprintln(errOut)
		return strings.TrimSpace(string(b)), err
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("无法打开控制终端；自动化请使用 gate -sk <API Key>")
	}
	defer tty.Close()
	stty := exec.Command("stty", "-echo")
	stty.Stdin = tty
	if err := stty.Run(); err != nil {
		return "", errors.New("无法关闭终端回显")
	}
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		_ = on.Run()
		fmt.Fprintln(errOut)
	}()
	return bufio.NewReader(tty).ReadString('\n')
}
