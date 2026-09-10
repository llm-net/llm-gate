package main

// codex.go 收拢 Codex 适配器里不经官方脚本的三段：
//
//   - 受管安装：经盒子白名单读官方 release 元数据，用 gate 自己的断点续传
//     下载器取 codex-package 归档，按官方 SHA256SUMS 校验、解包、自检后原子
//     切换（同 Claude / Cursor / OpenCode 一条纪律；官方安装器的 300 秒单次
//     下载在慢链路上撞墙，这里不再依赖它）。
//   - 目录覆盖片：设备 /agents/codex/v1/model-catalog 的 subscription_overlay
//     盖到从 CLI bundled 目录取来的订阅模型条目上——盒子后端承受什么由盒子说，
//     不由 OpenAI 自家后端的默认值说。
//   - 共享接入：把设备 provider 写进使用者自己的 CODEX_HOME（缺省 ~/.codex），
//     供不经 gate 启动的内核（ChatGPT.app 桌面版、IDE 插件）读取。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------- 受管安装 ----------

const (
	codexCLIBasePath   = "/codex-helper/cli"
	codexCLIMaxBytes   = 512 << 20
	codexCLITimeout    = 60 * time.Minute
	codexChecksumAsset = "codex-package_SHA256SUMS"
)

var (
	codexVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z._-]+)?$`)
	codexDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	codexHexRE     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// codexRelease 是 releases.openai.com/codex 的 channels/latest 与
// releases/<v>/release.json 共同的形态（GitHub release 同构）。
type codexRelease struct {
	TagName string              `json:"tag_name"`
	Assets  []codexReleaseAsset `json:"assets"`
}

type codexReleaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// codexVendorTarget 是官方安装器的 OS/arch → Rust target 映射；六平台归档一律
// 是 codex-package-<target>.tar.gz（Windows 亦为 tar.gz，内含 bin\codex.exe）。
func codexVendorTarget(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "aarch64-apple-darwin", nil
	case "darwin/amd64":
		return "x86_64-apple-darwin", nil
	case "linux/arm64":
		return "aarch64-unknown-linux-musl", nil
	case "linux/amd64":
		return "x86_64-unknown-linux-musl", nil
	case "windows/arm64":
		return "aarch64-pc-windows-msvc", nil
	case "windows/amd64":
		return "x86_64-pc-windows-msvc", nil
	}
	return "", fmt.Errorf("Codex 官方没有 %s/%s 制品", goos, goarch)
}

func codexPackageAsset(target string) string { return "codex-package-" + target + ".tar.gz" }

// normalizeCodexRelease 同官方安装器的 CODEX_RELEASE 语义：空 / latest 取最新，
// rust-v 与 v 前缀都剥掉。
func normalizeCodexRelease(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case v == "" || v == "latest":
		return "latest"
	case strings.HasPrefix(v, "rust-v"):
		return strings.TrimPrefix(v, "rust-v")
	case strings.HasPrefix(v, "v"):
		return strings.TrimPrefix(v, "v")
	}
	return v
}

// codexVersionOf 从 `codex --version` 的首行（"codex-cli 0.149.1"）取版本号；
// 取不到返回空串。
func codexVersionOf(line string) string {
	fields := strings.Fields(line)
	for i := len(fields) - 1; i >= 0; i-- {
		v := strings.TrimPrefix(fields[i], "v")
		if codexVersionRE.MatchString(v) {
			return v
		}
	}
	return ""
}

func (a *app) codexHTTPClient() *http.Client {
	client := *a.hc
	client.Timeout = codexCLITimeout
	return &client
}

func (a *app) getCodexPublic(rel string, maxBytes int64) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.cfg.BaseURL+codexCLIBasePath+"/"+rel, nil)
	resp, err := a.codexHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", codexCLIBasePath+"/"+rel, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return body, err
}

// resolveCodexRelease 按官方安装器同一顺序解析版本：latest 走 channels/latest，
// 钉死版本走 releases/<v>/release.json；tag_name 必须是 rust-v<semver>。
func (a *app) resolveCodexRelease(requested string) (string, codexRelease, error) {
	metaPath := "channels/latest"
	if requested != "latest" {
		if !codexVersionRE.MatchString(requested) {
			return "", codexRelease{}, fmt.Errorf("CODEX_RELEASE=%q 不是合法版本号", requested)
		}
		metaPath = "releases/" + requested + "/release.json"
	}
	body, err := a.getCodexPublic(metaPath, 4<<20)
	if err != nil {
		return "", codexRelease{}, fmt.Errorf("读取 Codex 官方 release: %w", err)
	}
	var release codexRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return "", codexRelease{}, fmt.Errorf("解析 Codex release: %w", err)
	}
	version := strings.TrimPrefix(release.TagName, "rust-v")
	if release.TagName != "rust-v"+version || !codexVersionRE.MatchString(version) {
		return "", codexRelease{}, errors.New("设备返回的 Codex 版本形态未知")
	}
	if requested != "latest" && version != requested {
		return "", codexRelease{}, fmt.Errorf("设备返回的 Codex 版本 %s 与请求的 %s 不一致", version, requested)
	}
	return version, release, nil
}

// codexPackageDigest 取归档的期望 SHA-256：先按官方安装器读 codex-package_SHA256SUMS
// 清单，清单缺项时退回 release 元数据里该 asset 的 digest；两处都没有就拒绝安装。
func (a *app) codexPackageDigest(version, asset string, release codexRelease) (string, error) {
	if sums, err := a.getCodexPublic("releases/"+version+"/"+codexChecksumAsset, 256<<10); err == nil {
		for _, line := range strings.Split(string(sums), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.TrimPrefix(fields[1], "*") == asset && codexHexRE.MatchString(fields[0]) {
				return strings.ToLower(fields[0]), nil
			}
		}
	}
	for _, candidate := range release.Assets {
		if candidate.Name == asset && codexDigestRE.MatchString(candidate.Digest) {
			return strings.TrimPrefix(candidate.Digest, "sha256:"), nil
		}
	}
	return "", fmt.Errorf("Codex release 没有 %s 的 SHA-256", asset)
}

func codexStandaloneDir(root string) string {
	return filepath.Join(root, "tools", "codex", "packages", "standalone")
}

// codexReleaseVersion 从 releases/ 下的目录名（<版本>-<target>）取版本；不是
// 本平台的目录返回空串。
func codexReleaseVersion(name, target string) string {
	version, ok := strings.CutSuffix(name, "-"+target)
	if !ok || !codexVersionRE.MatchString(version) {
		return ""
	}
	return version
}

// codexVersionNewer 只比较主.次.修订三段数字；预发布后缀不参与。
func codexVersionNewer(a, b string) bool {
	parse := func(v string) [3]int {
		var out [3]int
		core, _, _ := strings.Cut(v, "-")
		for i, part := range strings.SplitN(core, ".", 3) {
			if i < 3 {
				out[i], _ = strconv.Atoi(part)
			}
		}
		return out
	}
	x, y := parse(a), parse(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// managedCodexPath 返回受管目录里版本最高的已安装 Codex 程序；没有返回空串。
func (a *app) managedCodexPath() string {
	target, err := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(codexStandaloneDir(a.root), "releases"))
	if err != nil {
		return ""
	}
	best, bestVersion := "", ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		version := codexReleaseVersion(entry.Name(), target)
		if version == "" {
			continue
		}
		bin := filepath.Join(codexStandaloneDir(a.root), "releases", entry.Name(), "bin", toolBinaryName("codex"))
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			continue
		}
		if best == "" || codexVersionNewer(version, bestVersion) {
			best, bestVersion = bin, version
		}
	}
	return best
}

// cleanupCodexReleases 清掉 keep 之外的版本目录、中断安装留下的暂存树，以及
// 官方安装器时代留下的 current 软链、bin/codex 软链和锁文件。
func cleanupCodexReleases(root, keep string) {
	standalone := codexStandaloneDir(root)
	releases := filepath.Join(standalone, "releases")
	if entries, err := os.ReadDir(releases); err == nil {
		for _, entry := range entries {
			path := filepath.Join(releases, entry.Name())
			if path != keep {
				_ = os.RemoveAll(path)
			}
		}
	}
	for _, name := range []string{"install.lock", "install.lock.d", "current"} {
		_ = os.RemoveAll(filepath.Join(standalone, name))
	}
	legacy := filepath.Join(root, "tools", "codex", "bin", toolBinaryName("codex"))
	if st, err := os.Lstat(legacy); err == nil && st.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(legacy)
	}
}

func extractCodexPackage(archivePath, staging string) error {
	w := &archiveTreeWriter{label: "Codex", staging: staging, symlinks: map[string]bool{}, budget: codexCLIMaxBytes}
	return extractTarGzTree(archivePath, w)
}

// installCodex 经盒子安装或升级受管 Codex，返回程序路径。版本目录整树暂存、
// 自检通过后才改名进位；同版本重装先把旧目录挪开，切换失败再挪回来。
func (a *app) installCodex() (string, error) {
	target, err := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	asset := codexPackageAsset(target)
	a.printf("正在经设备读取 Codex 官方 release…\n")
	version, release, err := a.resolveCodexRelease(normalizeCodexRelease(os.Getenv("CODEX_RELEASE")))
	if err != nil {
		return "", err
	}
	standalone := codexStandaloneDir(a.root)
	releasesDir := filepath.Join(standalone, "releases")
	releaseDir := filepath.Join(releasesDir, version+"-"+target)
	entry := filepath.Join(releaseDir, "bin", toolBinaryName("codex"))
	if st, err := os.Stat(entry); err == nil && !st.IsDir() {
		if codexVersionOf(detectVersionWithEnv(entry, installerEnv())) == version {
			a.printf("受管 Codex 已是官方当前版本 %s。\n", version)
			cleanupCodexReleases(a.root, releaseDir)
			return entry, nil
		}
	}
	var size int64
	for _, candidate := range release.Assets {
		if candidate.Name == asset {
			size = candidate.Size
		}
	}
	if size < 0 || size > codexCLIMaxBytes {
		return "", errors.New("Codex release 的平台条目无效")
	}
	digest, err := a.codexPackageDigest(version, asset, release)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(releasesDir, 0o700); err != nil {
		return "", err
	}
	if err := securePath(filepath.Join(a.root, "tools", "codex"), true); err != nil {
		return "", err
	}
	if size > 0 {
		a.printf("正在经设备下载 Codex %s（约 %d MiB）…\n", version, (size+(1<<20)-1)/(1<<20))
	} else {
		a.printf("正在经设备下载 Codex %s…\n", version)
	}
	archivePath, err := a.fetchArtifact(artifactFetch{
		client: a.codexHTTPClient(),
		url:    a.cfg.BaseURL + codexCLIBasePath + "/releases/" + version + "/" + asset,
		label:  "Codex",
		dir:    standalone,
		prefix: ".codex-package",
		size:   size,
		digest: digest,
		max:    codexCLIMaxBytes,
		mode:   0o600,
	})
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)
	a.printf("Codex 官方制品校验通过。\n")

	staging, err := os.MkdirTemp(releasesDir, ".release.tmp-")
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(staging)
		}
	}()
	if err := extractCodexPackage(archivePath, staging); err != nil {
		return "", err
	}
	// 官方安装器在解包后显式补可执行位；tar 位通常已在，这里保底。
	for _, rel := range []string{"bin/codex", "bin/codex-code-mode-host", "codex-path/rg", "codex-resources/rg", "codex-resources/bwrap"} {
		p := filepath.Join(staging, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			_ = os.Chmod(p, st.Mode().Perm()|0o700)
		}
	}
	stagedEntry := filepath.Join(staging, "bin", toolBinaryName("codex"))
	if st, err := os.Stat(stagedEntry); err != nil || st.IsDir() {
		return "", errors.New("Codex 归档里没有 bin/codex，拒绝安装")
	}
	if got := codexVersionOf(detectVersionWithEnv(stagedEntry, installerEnv())); got != version {
		return "", fmt.Errorf("Codex 制品自检版本 %q 与 release 钉死的 %q 不一致，已丢弃暂存树", got, version)
	}
	backup := ""
	if _, err := os.Lstat(releaseDir); err == nil {
		backup = releaseDir + ".old-" + strings.TrimPrefix(filepath.Base(staging), ".release.tmp-")
		if err := os.Rename(releaseDir, backup); err != nil {
			return "", fmt.Errorf("挪开现有 Codex 版本目录: %w", err)
		}
	}
	if err := os.Rename(staging, releaseDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, releaseDir)
		}
		return "", fmt.Errorf("切换 Codex 版本目录: %w", err)
	}
	committed = true
	cleanupCodexReleases(a.root, releaseDir)
	if d, err := os.Open(releasesDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	a.printf("Codex %s 制品自检通过。\n", version)
	return entry, nil
}

func (a *app) printf(format string, args ...any) {
	if a.out != nil {
		fmt.Fprintf(a.out, format, args...)
	}
}

func (a *app) warnf(format string, args ...any) {
	if a.err != nil {
		fmt.Fprintf(a.err, format, args...)
	}
}

// ---------- 目录覆盖片 ----------

// catalogOverlay 是设备 model-catalog 的 subscription_overlay：set 覆盖、default
// 只在缺席时补、experimental_supported_tools_add 去重追加。只盖到从 bundled
// 取来的订阅模型条目，设备自己生成的目录模型条目不动。
type catalogOverlay struct {
	Set      map[string]json.RawMessage `json:"set"`
	Default  map[string]json.RawMessage `json:"default"`
	ToolsAdd []string                   `json:"experimental_supported_tools_add"`
}

func applyCatalogOverlay(raw json.RawMessage, ov *catalogOverlay) (json.RawMessage, error) {
	if ov == nil {
		return raw, nil
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("bundled 模型条目形态未知: %w", err)
	}
	for k, v := range ov.Set {
		entry[k] = v
	}
	for k, v := range ov.Default {
		if _, ok := entry[k]; !ok {
			entry[k] = v
		}
	}
	if len(ov.ToolsAdd) > 0 {
		var tools []string
		if cur, ok := entry["experimental_supported_tools"]; ok {
			_ = json.Unmarshal(cur, &tools) // 形态不对就当空列表重建
		}
		for _, t := range ov.ToolsAdd {
			if !slices.Contains(tools, t) {
				tools = append(tools, t)
			}
		}
		b, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		entry["experimental_supported_tools"] = b
	}
	return json.Marshal(entry)
}

// writeCheckedCodexCatalog 先让 path 指向的内核用 debug models 自检临时目录，
// 通过后才原子替换 dest。任何失败都保留旧文件。
func (a *app) writeCheckedCodexCatalog(path string, catalog []byte, dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmpCatalog, err := os.CreateTemp(dir, ".model-catalog-*.json")
	if err != nil {
		return err
	}
	tmpName := tmpCatalog.Name()
	defer os.Remove(tmpName)
	if err := tmpCatalog.Chmod(0o600); err != nil {
		tmpCatalog.Close()
		return err
	}
	if _, err := tmpCatalog.Write(catalog); err != nil {
		tmpCatalog.Close()
		return err
	}
	if err := tmpCatalog.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, path, "debug", "models", "-c", "model_catalog_json="+tomlQuote(tmpName))
	check.Env = a.toolEnv("codex")
	if b, err := check.CombinedOutput(); err != nil {
		return codexUnusable(path, "拒绝生成的模型目录："+clipped(b))
	}
	return atomicWrite(dest, catalog, 0o600)
}

// ---------- 共享接入（ChatGPT.app 桌面版 / IDE 插件） ----------

// sharedCodex 记录写进使用者 CODEX_HOME 的那份接入，以及回滚所需的原值。
type sharedCodex struct {
	Home            string `json:"home"`
	KernelPath      string `json:"kernel_path"`
	KernelVersion   string `json:"kernel_version"`
	Catalog         string `json:"catalog"`
	PrevProvider    string `json:"prev_model_provider,omitempty"`
	PrevProviderSet bool   `json:"prev_model_provider_set"`
	ModelWritten    string `json:"model_written,omitempty"`
	AuthWritten     bool   `json:"auth_written"`
	SectionHash     string `json:"section_sha256,omitempty"`
	PrevCatalog     string `json:"prev_model_catalog,omitempty"`
	PrevCatalogSet  bool   `json:"prev_model_catalog_set,omitempty"`
}

const sharedProviderHeader = "[model_providers.llmgate]"

// codexActorAuthorizationHeader 是 Codex 内核判定「这个 provider 能画图」的开关。
// 内核的 image-generation 扩展只在三种 provider 上挂出内置 image_gen 工具：
// 内建 openai provider、requires_openai_auth 的 provider，或 http_headers 里带
// 非空 x-openai-actor-authorization 的 provider（codex-rs ext/image-generation
// extension.rs 的 available 判定与 core tools/spec_plan.rs 的
// image_generation_available 同一口径）。设备 provider 是第三种，所以派生配置
// 与共享接入都必须写这个头；值本身内核不解释，设备侧在转发上游前剥掉
// （firmware responses.go buildAgentRequest），永远不会到达 ChatGPT 后端。
// 没有这个头时内核根本不挂 image_gen，模型只能读 imagegen 技能文档后回答
// 「built-in image generation is unavailable」，设备画图门一次都不会被打到。
const (
	codexActorAuthorizationHeader = "x-openai-actor-authorization"
	codexActorAuthorizationValue  = "llmgate"
)

// codexHome 是不经 gate 启动的内核实际读取的配置目录：使用者环境里的
// CODEX_HOME，缺省 ~/.codex。gate 自己给子进程设的 CODEX_HOME 不在这里。
func codexHome() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// desktopCodexCandidates 列出桌面 App 自带内核的固定位置（只有 macOS 的
// ChatGPT.app 有公开可依赖的路径；其它形态用 --path 指定）。
func desktopCodexCandidates() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	candidates := []string{"/Applications/ChatGPT.app/Contents/Resources/codex"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", "ChatGPT.app", "Contents", "Resources", "codex"))
	}
	var out []string
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

func (a *app) sharedCatalogPath() string {
	return filepath.Join(a.derivedDir("codex"), "shared-model-catalog.json")
}

// connectShared 绑定一个内核并把设备接入写进使用者 CODEX_HOME。候选顺序：
// 显式 --path → 桌面 App 内核 → 已关联的 CLI → PATH；逐个用该内核自检目录，
// 失败钉在某个二进制上时换下一个。重复执行即 ensure：桌面 App 首次引导会重写
// config.toml，重跑就补回来。
func (a *app) connectShared(explicit string) error {
	rc, err := a.fetchRuntime()
	if err != nil {
		return err
	}
	tool := rc.Tools["codex"]
	if len(tool.Models) == 0 {
		return errors.New("这把 API Key 当前没有可用于 codex 的模型；请在管理台配置")
	}
	var candidates []string
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return err
		}
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			return fmt.Errorf("--path 指定的内核不存在: %s", abs)
		}
		candidates = []string{abs}
	}
	prev := a.cfg.Shared["codex"]
	if explicit == "" {
		// 重跑（ensure）优先沿用已绑定的内核，其后才是桌面 App、已关联 CLI 与 PATH。
		candidates = append(candidates, prev.KernelPath)
		candidates = append(candidates, desktopCodexCandidates()...)
		if st, ok := a.cfg.Tools["codex"]; ok {
			candidates = append(candidates, st.BinaryPath)
		}
		candidates = append(candidates, lookPathAll(toolBinaryName("codex"))...)
		candidates = dedupePaths(candidates)
		if len(candidates) == 0 {
			return errors.New("没有找到可绑定的 Codex 内核：未检测到 ChatGPT.app，PATH 中也没有 codex；可用 --path 指定内核路径")
		}
	}
	home, err := codexHome()
	if err != nil {
		return err
	}
	if prev.Home != "" && prev.Home != home {
		return errors.New("Codex 共享目录已改变；请先使用原目录 disconnect --shared")
	}
	st := sharedCodex{
		Home: home, Catalog: a.sharedCatalogPath(),
		PrevCatalog: prev.PrevCatalog, PrevCatalogSet: prev.PrevCatalogSet, SectionHash: prev.SectionHash,
		PrevProvider: prev.PrevProvider, PrevProviderSet: prev.PrevProviderSet,
		ModelWritten: prev.ModelWritten, AuthWritten: prev.AuthWritten,
	}
	var failures []error
	for i, candidate := range candidates {
		err := a.refreshSharedCatalog(candidate, tool, st.Catalog)
		if err == nil {
			st.KernelPath, st.KernelVersion = candidate, detectVersion(candidate)
			break
		}
		if !isCLIUnusable(err) {
			return err
		}
		failures = append(failures, err)
		if i < len(candidates)-1 {
			a.warnf("跳过不可用内核：%v\n", err)
		}
	}
	if st.KernelPath == "" {
		return fmt.Errorf("没有能通过目录自检的 Codex 内核：%w", errors.Join(failures...))
	}
	if err := a.applySharedCodex(&st, tool, prev.Home == ""); err != nil {
		return err
	}
	if a.cfg.Shared == nil {
		a.cfg.Shared = map[string]sharedCodex{}
	}
	a.cfg.Shared["codex"] = st
	if err := a.save(); err != nil {
		return err
	}
	a.printf("codex 共享接入已写入：%s\n内核：%s (%s)\n", filepath.Join(st.Home, "config.toml"), st.KernelPath, st.KernelVersion)
	a.printf("提示：该文件的默认 provider 现在是 llmgate，直接运行 codex 也会经设备；API Key 以 http_headers 形式存于其中（仅属主可读）。\n")
	a.printf("提示：ChatGPT.app 首次引导可能重写 config.toml；重跑 gate codex connect --shared（任意 gate codex 启动也会补回）。请完全退出并重启 ChatGPT.app。\n")
	return nil
}

func dedupePaths(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		key := p
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			key = resolved
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// refreshSharedCatalog 用绑定内核的 bundled 目录 + 设备覆盖片生成共享目录，
// 并由同一个内核自检。目录只对自检过的内核负责：gate codex 启动用的派生目录
// 另有一份，两者互不覆盖。
func (a *app) refreshSharedCatalog(kernel string, tool runtimeTool, dest string) error {
	catalog, err := a.codexCatalog(kernel, tool)
	if err != nil {
		return err
	}
	return a.writeCheckedCodexCatalog(kernel, catalog, dest)
}

// ensureSharedCodex 在每次 gate codex 启动时把共享接入补回去；内核失效只告警，
// 不阻断 CLI 启动。
func (a *app) ensureSharedCodex(tool runtimeTool) error {
	st, ok := a.cfg.Shared["codex"]
	if !ok {
		return nil
	}
	if fi, err := os.Stat(st.KernelPath); err != nil || fi.IsDir() {
		return fmt.Errorf("共享接入绑定的内核不可用：%s；重跑 gate codex connect --shared 重新绑定", st.KernelPath)
	}
	if err := a.refreshSharedCatalog(st.KernelPath, tool, st.Catalog); err != nil {
		return err
	}
	st.KernelVersion = detectVersion(st.KernelPath)
	if err := a.applySharedCodex(&st, tool, false); err != nil {
		return err
	}
	a.cfg.Shared["codex"] = st
	return a.save()
}

// applySharedCodex 增量修补 <home>/config.toml 与 auth.json：
//
//   - 顶层 model_provider 钉成 llmgate（首次记下原值供回滚），model_catalog_json
//     指向共享目录，model 只在缺席时补默认模型、从不覆盖使用者的选择；
//   - [model_providers.llmgate] 整段替换，Key 走静态 http_headers——GUI 进程看
//     不到 shell 环境，env_key 在这里没有意义；
//   - auth.json 缺席或已是 apikey 模式时写 apikey 模式让 App 免登录；检测到
//     ChatGPT 登录态（auth_mode=chatgpt 或有 tokens）则原样保留。
func (a *app) applySharedCodex(st *sharedCodex, tool runtimeTool, first bool) error {
	if err := os.MkdirAll(st.Home, 0o700); err != nil {
		return err
	}
	cfgPath := filepath.Join(st.Home, "config.toml")
	text := ""
	if b, err := os.ReadFile(cfgPath); err == nil {
		if len(b) > 4<<20 {
			return fmt.Errorf("%s 超过 4 MiB，拒绝修补", cfgPath)
		}
		text = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if first {
		st.PrevProvider, st.PrevProviderSet = tomlTopLevel(text, "model_provider")
		st.PrevCatalog, st.PrevCatalogSet = tomlTopLevel(text, "model_catalog_json")
	}
	text = tomlSetTopLevel(text, "model_provider", tomlQuote("llmgate"))
	text = tomlSetTopLevel(text, "model_catalog_json", tomlQuote(st.Catalog))
	if _, ok := tomlTopLevel(text, "model"); !ok && tool.DefaultModel != "" {
		text = tomlSetTopLevel(text, "model", tomlQuote(tool.DefaultModel))
		st.ModelWritten = tool.DefaultModel
	}
	section := a.sharedSectionBody()
	st.SectionHash = digest([]byte(strings.TrimSpace(sharedProviderHeader + "\n" + section)))
	if a.cfg.Shared == nil {
		a.cfg.Shared = map[string]sharedCodex{}
	}
	a.cfg.Shared["codex"] = *st
	if err := a.save(); err != nil {
		return err
	}
	text = tomlReplaceSection(text, sharedProviderHeader, section)
	if err := writeUserFile(cfgPath, []byte(text), 0o600); err != nil {
		return err
	}

	authPath := filepath.Join(st.Home, "auth.json")
	if b, err := os.ReadFile(authPath); err == nil {
		var auth map[string]any
		_ = json.Unmarshal(b, &auth)
		mode, _ := auth["auth_mode"].(string)
		if _, hasTokens := auth["tokens"]; hasTokens || mode == "chatgpt" {
			st.AuthWritten = false
			a.printf("检测到 ChatGPT 登录态，%s 保留不动。\n", authPath)
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	auth, err := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": a.cfg.APIKey})
	if err != nil {
		return err
	}
	st.AuthWritten = true
	a.cfg.Shared["codex"] = *st
	if err := a.save(); err != nil {
		return err
	}
	if err := writeUserFile(authPath, append(auth, '\n'), 0o600); err != nil {
		return err
	}
	st.AuthWritten = true
	return nil
}

// disconnectShared 只撤掉自己写的东西：provider 段、指向共享目录的
// model_catalog_json、自己补的 model，并把 model_provider 还原成原值；auth.json
// 只在是自己写的且仍是自己那把 Key 时删除。
func (a *app) disconnectShared() error {
	if _, ok := a.cfg.Shared["codex"]; !ok {
		return errors.New("codex 没有共享接入")
	}
	p := &uninstallPlan{}
	if err := a.planShared(p); err != nil {
		return err
	}
	if err := p.applyFiles(); err != nil {
		return err
	}
	delete(a.cfg.Shared, "codex")
	if err := a.save(); err != nil {
		return err
	}
	a.printf("codex 共享接入已撤销；内核程序与用户登录态已保留。\n")
	return nil
}

// sharedCodexDrift 报告共享接入是否仍完整；空串表示正常。
func sharedCodexDrift(st sharedCodex, catalogPath string) string {
	if fi, err := os.Stat(st.KernelPath); err != nil || fi.IsDir() {
		return "内核路径失效"
	}
	b, err := os.ReadFile(filepath.Join(st.Home, "config.toml"))
	if err != nil {
		return "config.toml 不存在"
	}
	text := string(b)
	if start, _ := tomlSectionBounds(text, sharedProviderHeader); start < 0 {
		return "缺少 " + sharedProviderHeader + " 段"
	}
	if v, ok := tomlTopLevel(text, "model_provider"); !ok || v != "llmgate" {
		return "model_provider 不是 llmgate"
	}
	if v, ok := tomlTopLevel(text, "model_catalog_json"); !ok || v != catalogPath {
		return "model_catalog_json 未指向共享目录"
	}
	if _, err := os.Stat(catalogPath); err != nil {
		return "共享目录文件缺失"
	}
	return ""
}

func (a *app) printSharedStatus() {
	st, ok := a.cfg.Shared["codex"]
	if !ok {
		return
	}
	a.printf("共享接入：%s → %s (%s)\n", filepath.Join(st.Home, "config.toml"), st.KernelPath, st.KernelVersion)
	if drift := sharedCodexDrift(st, st.Catalog); drift != "" {
		a.printf("共享状态：已被改写（%s）；重跑 gate codex connect --shared 可补回\n", drift)
	} else {
		a.printf("共享状态：正常\n")
	}
}

// writeUserFile 原子写使用者自己目录里的文件：不动目录权限，只把文件本身
// 收紧到属主可读写。
func writeUserFile(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gate-tmp-*")
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
	return securePath(path, false)
}

// ---------- 最小 TOML 文本修补 ----------
//
// 只处理本适配器写入的形态：顶层单行 key = value 与 [表头] 起始的段。不解析
// 整份 TOML，使用者其余内容一字不动。

// tomlSplitHeader 把文本切成第一张表之前的顶层部分与其余部分。
func tomlSplitHeader(text string) (string, string) {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "[") {
			return strings.Join(lines[:i], ""), strings.Join(lines[i:], "")
		}
	}
	return text, ""
}

func tomlKeyLine(line, key string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	rest := strings.TrimLeft(trimmed[len(key):], " \t")
	return strings.HasPrefix(rest, "=")
}

// tomlTopLevel 取顶层 key 的字符串值（基本字符串与字面字符串都解引号；其它
// 值原样返回）。
func tomlTopLevel(text, key string) (string, bool) {
	header, _ := tomlSplitHeader(text)
	for _, line := range strings.Split(header, "\n") {
		if !tomlKeyLine(line, key) {
			continue
		}
		_, v, _ := strings.Cut(line, "=")
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' {
			if end := strings.Index(v[1:], `"`); end >= 0 {
				// 基本字符串的转义与 JSON 同形；解不开就退回原文。
				var s string
				if err := json.Unmarshal([]byte(v[:end+2]), &s); err == nil {
					return s, true
				}
				return v[1 : end+1], true
			}
		}
		if len(v) >= 2 && v[0] == '\'' {
			if end := strings.Index(v[1:], "'"); end >= 0 {
				return v[1 : end+1], true
			}
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v, true
	}
	return "", false
}

// tomlSetTopLevel 替换或在顶层最前面插入一行 key = quoted。
func tomlSetTopLevel(text, key, quoted string) string {
	header, rest := tomlSplitHeader(text)
	lines := strings.SplitAfter(header, "\n")
	replaced := false
	for i, line := range lines {
		if tomlKeyLine(line, key) {
			nl := ""
			if strings.HasSuffix(line, "\n") {
				nl = "\n"
			}
			lines[i] = key + " = " + quoted + nl
			replaced = true
			break
		}
	}
	header = strings.Join(lines, "")
	if !replaced {
		if header != "" && !strings.HasSuffix(header, "\n") {
			header += "\n"
		}
		header = key + " = " + quoted + "\n" + header
	}
	return header + rest
}

func tomlRemoveTopLevel(text, key string) string {
	header, rest := tomlSplitHeader(text)
	lines := strings.SplitAfter(header, "\n")
	out := lines[:0]
	for _, line := range lines {
		if !tomlKeyLine(line, key) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "") + rest
}

// tomlSectionBounds 返回 header 那一段（含表头行，直到下一张表或文末）的字节
// 区间；找不到返回 -1。
func tomlSectionBounds(text, header string) (int, int) {
	offset := 0
	start := -1
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == header {
				start = offset
			}
		} else if strings.HasPrefix(trimmed, "[") {
			return start, offset
		}
		offset += len(line)
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(text)
}

// tomlReplaceSection 整段替换（或在文末追加）header 段；body 以换行结尾。
func tomlReplaceSection(text, header, body string) string {
	block := header + "\n" + body
	start, end := tomlSectionBounds(text, header)
	if start < 0 {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" && !strings.HasSuffix(text, "\n\n") {
			text += "\n"
		}
		return text + block
	}
	tail := text[end:]
	if tail != "" && !strings.HasSuffix(block, "\n\n") {
		block += "\n"
	}
	return text[:start] + block + tail
}

func tomlRemoveSection(text, header string) string {
	start, end := tomlSectionBounds(text, header)
	if start < 0 {
		return text
	}
	return text[:start] + text[end:]
}

// ---------- 归档整树解包（Codex 与 Cursor 共用） ----------

// archiveTreeWriter 把归档条目安全地落到暂存目录：拒绝绝对路径、上跳、经符号
// 链接写入与越界的链接目标，并按 budget 限制总字节。stripRoot 时要求单一顶层
// 目录并剥掉它（Cursor 制品形态）；否则条目相对路径原样落盘（Codex 制品形态）。
type archiveTreeWriter struct {
	label     string
	staging   string
	stripRoot bool
	root      string
	symlinks  map[string]bool
	budget    int64
}

// rel 校验一个条目名并返回落盘相对路径；ok=false 表示条目无需落盘（顶层目录
// 本身或 "."）。
func (w *archiveTreeWriter) rel(name string, dir bool) (string, bool, error) {
	slashed := filepath.ToSlash(name)
	if strings.HasPrefix(slashed, "/") || strings.Contains(slashed, "\x00") {
		return "", false, fmt.Errorf("%s 归档条目 %q 使用绝对路径，拒绝安装", w.label, name)
	}
	segments := make([]string, 0, 8)
	for _, segment := range strings.Split(slashed, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", false, fmt.Errorf("%s 归档条目 %q 含上跳路径，拒绝安装", w.label, name)
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return "", false, nil
	}
	if w.stripRoot {
		if w.root == "" {
			w.root = segments[0]
		}
		if segments[0] != w.root {
			return "", false, fmt.Errorf("%s 归档不是单一顶层目录结构（%q 与 %q 并存），拒绝安装", w.label, segments[0], w.root)
		}
		if len(segments) == 1 {
			if !dir {
				return "", false, fmt.Errorf("%s 归档顶层不是单一目录，拒绝安装", w.label)
			}
			return "", false, nil
		}
		segments = segments[1:]
	}
	rel := strings.Join(segments, "/")
	for prefix := rel; prefix != "."; prefix = slashDir(prefix) {
		if w.symlinks[prefix] {
			return "", false, fmt.Errorf("%s 归档试图经符号链接 %q 写入，拒绝安装", w.label, prefix)
		}
	}
	dest := filepath.Join(w.staging, filepath.FromSlash(rel))
	if dest != w.staging && !strings.HasPrefix(dest, w.staging+string(os.PathSeparator)) {
		return "", false, fmt.Errorf("%s 归档条目 %q 越出安装目录，拒绝安装", w.label, name)
	}
	return rel, true, nil
}

func slashDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

func (w *archiveTreeWriter) dir(rel string) error {
	return os.MkdirAll(filepath.Join(w.staging, filepath.FromSlash(rel)), 0o700)
}

func (w *archiveTreeWriter) file(rel string, perm os.FileMode, r io.Reader) error {
	dest := filepath.Join(w.staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, w.budget+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	w.budget -= n
	if w.budget < 0 {
		return fmt.Errorf("%s 归档解包超过大小限制", w.label)
	}
	// 保留官方文件位：入口程序必须保持可执行；显式 Chmod 绕开 umask，并保底
	// 属主可读写。
	return os.Chmod(dest, perm.Perm()|0o600)
}

func (w *archiveTreeWriter) symlink(rel, target string) error {
	link := filepath.ToSlash(target)
	if link == "" || strings.HasPrefix(link, "/") || strings.Contains(link, ":") || strings.Contains(link, "\x00") {
		return fmt.Errorf("%s 归档符号链接 %q 指向绝对路径，拒绝安装", w.label, rel)
	}
	resolved := path.Clean(slashDir(rel) + "/" + link)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%s 归档符号链接 %q 越出安装目录，拒绝安装", w.label, rel)
	}
	dest := filepath.Join(w.staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := os.Symlink(filepath.FromSlash(link), dest); err != nil {
		return err
	}
	w.symlinks[rel] = true
	return nil
}

func extractTarGzTree(archivePath string, w *archiveTreeWriter) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("打开 %s gzip: %w", w.label, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := false
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 %s tar: %w", w.label, err)
		}
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
		case tar.TypeXGlobalHeader:
			continue
		default:
			return fmt.Errorf("%s 归档条目 %q 类型不支持，拒绝安装", w.label, header.Name)
		}
		rel, ok, err := w.rel(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		seen = true
		switch header.Typeflag {
		case tar.TypeDir:
			err = w.dir(rel)
		case tar.TypeSymlink:
			err = w.symlink(rel, header.Linkname)
		default:
			err = w.file(rel, header.FileInfo().Mode(), tr)
		}
		if err != nil {
			return err
		}
	}
	if !seen {
		return fmt.Errorf("%s 归档为空，拒绝安装", w.label)
	}
	return nil
}
