package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestMain 让全部测试缺省不碰真实官方地址：来源链只剩测试自己起的假设备。
// 要验证官方源优先的用例用 useOfficialSources 指到假上游。
func TestMain(m *testing.M) {
	clearOfficialSources()
	os.Exit(m.Run())
}

func clearOfficialSources() {
	codexOfficialReleasesBase = ""
	grokOfficialPrimaryBase = ""
	grokOfficialFallbackBase = ""
	claudeOfficialReleasesBase = ""
	cursorOfficialInstallerURL = ""
	cursorOfficialLabBase = ""
	openCodeOfficialAPIBase = ""
	openCodeOfficialReleasesBase = ""
	mcodeOfficialBase = ""
	mcodeNodeOfficialBase = ""
}

// useOfficialSources 把六种工具的官方基址都指到 base（假上游按各自映射的路径
// 作答），用例结束后清空。
func useOfficialSources(t *testing.T, base string) {
	t.Helper()
	codexOfficialReleasesBase = base + "/codex"
	grokOfficialPrimaryBase = base + "/grok-primary"
	grokOfficialFallbackBase = base + "/grok-fallback"
	claudeOfficialReleasesBase = base + "/claude-code-releases"
	cursorOfficialInstallerURL = base + "/install"
	cursorOfficialLabBase = base + "/lab"
	openCodeOfficialAPIBase = base + "/api/releases"
	openCodeOfficialReleasesBase = base + "/download"
	mcodeOfficialBase = base + "/mcode"
	mcodeNodeOfficialBase = base + "/node"
	t.Cleanup(clearOfficialSources)
}

// 来源链：官方源在前；某次请求失败后整条链前进到设备，之后不再回头。
func TestOriginChainPrefersOfficialThenSticksToDevice(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	count := func(where, rel string) {
		mu.Lock()
		hits[where+":"+rel]++
		mu.Unlock()
	}
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/official/")
		count("official", rel)
		if rel == "broken" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "official:"+rel)
	}))
	defer official.Close()
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/x-helper/cli/")
		count("device", rel)
		fmt.Fprint(w, "device:"+rel)
	}))
	defer device.Close()
	errOut := &bytes.Buffer{}
	a := &app{root: t.TempDir(), cfg: config{BaseURL: device.URL}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: errOut}
	chain := a.newOriginChain("X", "/x-helper/cli", a.hc, artifactOrigin{
		label: "官方源", url: underBase(official.URL + "/official"), client: officialHTTPClient(0, nil),
	})
	if chain.label() != "官方源" {
		t.Fatalf("label=%q", chain.label())
	}
	if body, err := chain.get("first", 1024); err != nil || string(body) != "official:first" {
		t.Fatalf("first: %q %v", body, err)
	}
	if body, err := chain.get("broken", 1024); err != nil || string(body) != "device:broken" {
		t.Fatalf("broken should fall back to device: %q %v", body, err)
	}
	if chain.label() != "设备" {
		t.Fatalf("after fallback label=%q", chain.label())
	}
	if body, err := chain.get("third", 1024); err != nil || string(body) != "device:third" {
		t.Fatalf("third: %q %v", body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["official:first"] != 1 || hits["official:broken"] != 1 || hits["official:third"] != 0 ||
		hits["device:first"] != 0 || hits["device:broken"] != 1 || hits["device:third"] != 1 {
		t.Fatalf("hits=%v", hits)
	}
	if !strings.Contains(errOut.String(), "官方源不可用") || !strings.Contains(errOut.String(), "改经设备取回 broken") {
		t.Fatalf("fallback narration missing: %s", errOut.String())
	}
}

// 官方基址清空时链只剩设备；设备是最后一个来源，失败原样透出。
func TestOriginChainDeviceOnlyWhenOfficialCleared(t *testing.T) {
	device := httptest.NewServer(http.NotFoundHandler())
	defer device.Close()
	a := &app{root: t.TempDir(), cfg: config{BaseURL: device.URL}, hc: newHTTPClient(), err: &bytes.Buffer{}}
	for name, chain := range map[string]*originChain{
		"codex": a.codexOrigins(), "grok": a.grokOrigins(), "claude": a.claudeOrigins(),
		"cursor": a.cursorOrigins(), "opencode": a.openCodeOrigins(),
	} {
		if len(chain.origins) != 1 || chain.label() != "设备" {
			t.Fatalf("%s: origins=%d label=%q", name, len(chain.origins), chain.label())
		}
		if _, err := chain.get("latest", 64); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("%s: device failure must surface: %v", name, err)
		}
	}
}

// 各工具的官方路径映射与固件 helper 的 open() 逐条一致。
func TestOfficialURLMappingsMirrorFirmwareHelpers(t *testing.T) {
	useOfficialSources(t, "https://up.example")
	a := &app{cfg: config{BaseURL: "https://box.example"}, hc: newHTTPClient()}
	cases := []struct {
		chain *originChain
		rel   string
		want  []string // 各来源（官方…设备）应给出的 URL；空串表示该来源不提供
	}{
		{a.codexOrigins(), "channels/latest", []string{"https://up.example/codex/channels/latest", "https://box.example/codex-helper/cli/channels/latest"}},
		{a.codexOrigins(), "releases/1.2.3/codex-package_SHA256SUMS", []string{"https://up.example/codex/releases/1.2.3/codex-package_SHA256SUMS", "https://box.example/codex-helper/cli/releases/1.2.3/codex-package_SHA256SUMS"}},
		{a.grokOrigins(), "stable", []string{"https://up.example/grok-primary/stable", "https://up.example/grok-fallback/stable", "https://box.example/grok-helper/cli/stable"}},
		{a.claudeOrigins(), "1.2.3/linux-x64/claude", []string{"https://up.example/claude-code-releases/1.2.3/linux-x64/claude", "https://box.example/claude-helper/cli/1.2.3/linux-x64/claude"}},
		{a.mcodeOrigins(), "install.sh", []string{"https://up.example/mcode/install.sh", "https://box.example/mcode-helper/cli/install.sh"}},
		{a.mcodeOrigins(), "node/v" + mcodeNodeVersion + "/SHASUMS256.txt", []string{"https://up.example/node/v" + mcodeNodeVersion + "/SHASUMS256.txt", "https://box.example/mcode-helper/cli/node/v" + mcodeNodeVersion + "/SHASUMS256.txt"}},
		{a.cursorOrigins(), "install.sh", []string{"https://up.example/install", "https://box.example/cursor-helper/cli/install.sh"}},
		{a.cursorOrigins(), "install.ps1", []string{"https://up.example/install?win32=true", "https://box.example/cursor-helper/cli/install.ps1"}},
		{a.cursorOrigins(), "lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz", []string{"https://up.example/lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz", "https://box.example/cursor-helper/cli/lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz"}},
		{a.cursorOrigins(), "other", []string{"", "https://box.example/cursor-helper/cli/other"}},
		{a.openCodeOrigins(), "latest", []string{"https://up.example/api/releases/latest", "https://box.example/opencode-helper/cli/latest"}},
		{a.openCodeOrigins(), "releases/1.2.3/opencode-linux-x64.tar.gz", []string{"https://up.example/download/v1.2.3/opencode-linux-x64.tar.gz", "https://box.example/opencode-helper/cli/releases/1.2.3/opencode-linux-x64.tar.gz"}},
		{a.openCodeOrigins(), "other", []string{"", "https://box.example/opencode-helper/cli/other"}},
	}
	for _, tc := range cases {
		if len(tc.chain.origins) != len(tc.want) {
			t.Fatalf("%s %s: %d origins, want %d", tc.chain.tool, tc.rel, len(tc.chain.origins), len(tc.want))
		}
		for i, origin := range tc.chain.origins {
			if got := origin.url(tc.rel); got != tc.want[i] {
				t.Fatalf("%s %s origin %d: %q want %q", tc.chain.tool, tc.rel, i, got, tc.want[i])
			}
		}
	}
	h := http.Header{}
	oc := a.openCodeOrigins()
	oc.origins[0].header("latest", h)
	if h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") == "" {
		t.Fatalf("GitHub API headers missing: %v", h)
	}
	h = http.Header{}
	oc.origins[0].header("releases/1.2.3/opencode-linux-x64.tar.gz", h)
	if len(h) != 0 {
		t.Fatalf("archive request must not carry API headers: %v", h)
	}
	for _, name := range []string{"codex", "grok", "claude", "cursor", "opencode"} {
		var chain *originChain
		switch name {
		case "codex":
			chain = a.codexOrigins()
		case "grok":
			chain = a.grokOrigins()
		case "claude":
			chain = a.claudeOrigins()
		case "cursor":
			chain = a.cursorOrigins()
		default:
			chain = a.openCodeOrigins()
		}
		if chain.origins[0].client == a.hc || chain.origins[0].client.Transport == nil {
			t.Fatalf("%s: official origin must use its own direct client", name)
		}
	}
}

// Codex 受管安装：官方源可达时全部元数据与归档都直连官方，设备的 helper 一次不碰。
func TestCodexInstallPrefersOfficialSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	state := &codexDeviceState{}
	device := newCodexDeviceServerWithState(t, state)
	// 真正经网络打到设备 helper 的请求另计；假官方上游复用设备 handler 的
	// 应答（挂在 /codex/ 下），但不经网络、不进这份计数。
	inner := device.Config.Handler
	var mu sync.Mutex
	deviceHelperHits := map[string]int{}
	device.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rel, ok := strings.CutPrefix(r.URL.Path, "/codex-helper/cli/"); ok {
			mu.Lock()
			deviceHelperHits[rel]++
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	})
	var officialHits sync.Map
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel, ok := strings.CutPrefix(r.URL.Path, "/codex/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		officialHits.Store(rel, true)
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/codex-helper/cli/" + rel
		proxy := httptest.NewRecorder()
		inner.ServeHTTP(proxy, r2)
		for k, v := range proxy.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(proxy.Code)
		_, _ = w.Write(proxy.Body.Bytes())
	}))
	defer official.Close()
	useOfficialSources(t, official.URL)
	t.Setenv("PATH", t.TempDir())
	a := newCodexApp(t, device.URL)
	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("install: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	mu.Lock()
	if len(deviceHelperHits) != 0 {
		t.Fatalf("device helper must stay untouched when the official source works: %v", deviceHelperHits)
	}
	mu.Unlock()
	target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	for _, rel := range []string{"channels/latest", "releases/9.9.9/" + codexChecksumAsset, "releases/9.9.9/" + codexPackageAsset(target)} {
		if _, ok := officialHits.Load(rel); !ok {
			t.Fatalf("official source never asked for %s", rel)
		}
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "正在从官方源读取 Codex 官方 release") || !strings.Contains(out, "正在从官方源下载 Codex 9.9.9") {
		t.Fatalf("narration:\n%s", out)
	}
	if strings.Contains(a.err.(*bytes.Buffer).String(), "不可用") {
		t.Fatalf("no fallback expected: %s", a.err.(*bytes.Buffer).String())
	}
	if a.cfg.Tools["codex"].Origin != "gate" {
		t.Fatalf("state=%+v", a.cfg.Tools["codex"])
	}
}

// 官方源不可达（连接被拒）时改经设备完成安装，且只在第一次请求上付一次失败。
func TestCodexInstallFallsBackToDeviceWhenOfficialUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	state := &codexDeviceState{}
	device := newCodexDeviceServerWithState(t, state)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	useOfficialSources(t, deadURL)
	t.Setenv("PATH", t.TempDir())
	a := newCodexApp(t, device.URL)
	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("install: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	asset := codexPackageAsset(target)
	if state.hits["channels/latest"] != 1 || state.hits["releases/9.9.9/"+codexChecksumAsset] != 1 || state.hits["releases/9.9.9/"+asset] != 1 {
		t.Fatalf("device hits=%v", state.hits)
	}
	stderr := a.err.(*bytes.Buffer).String()
	if strings.Count(stderr, "官方源不可用") != 1 || !strings.Contains(stderr, "改经设备取回 channels/latest") {
		t.Fatalf("fallback must be narrated exactly once:\n%s", stderr)
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "正在从官方源读取 Codex 官方 release") || !strings.Contains(out, "正在从设备下载 Codex 9.9.9") {
		t.Fatalf("narration:\n%s", out)
	}
	if a.cfg.Tools["codex"].Origin != "gate" || !strings.Contains(a.cfg.Tools["codex"].DetectedVersion, "9.9.9") {
		t.Fatalf("state=%+v", a.cfg.Tools["codex"])
	}
}

// 官方源半途掐断且不再恢复时，改经设备从断点接着传，不重下已收的字节。
func TestOriginChainResumesPartAcrossOrigins(t *testing.T) {
	body := artifactBody(4096)
	var officialN int
	var mu sync.Mutex
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		officialN++
		n := officialN
		mu.Unlock()
		if n > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:1000])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer official.Close()
	dev := &artifactServer{body: body}
	device := httptest.NewServer(dev.handler())
	defer device.Close()

	a, out := newFetchApp(t)
	a.cfg.BaseURL = device.URL
	a.hc = newHTTPClient()
	chain := a.newOriginChain("Grok", "/grok-helper/cli", a.hc, artifactOrigin{
		label: "官方源", url: underBase(official.URL), client: officialHTTPClient(0, nil),
	})
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	path, err := chain.fetch("grok-9.9.9-linux-x86_64", artifactFetch{
		label: "Grok", dir: dir, prefix: ".grok-download", max: 1 << 20, mode: 0o700,
		size: int64(len(body)), digest: hexDigest(body),
	})
	if err != nil {
		t.Fatalf("fetch: %v\n%s\n%s", err, out.String(), a.err.(*bytes.Buffer).String())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: %v", err)
	}
	if seen := dev.seen(); len(seen) != 1 || seen[0] != "bytes=1000-" {
		t.Fatalf("device must be asked only for the remainder, got %v", seen)
	}
	if !strings.Contains(a.err.(*bytes.Buffer).String(), "改经设备取回 grok-9.9.9-linux-x86_64") {
		t.Fatalf("fallback narration missing: %s", a.err.(*bytes.Buffer).String())
	}
}
