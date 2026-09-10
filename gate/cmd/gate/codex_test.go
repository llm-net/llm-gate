package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newCodexApp(t *testing.T, baseURL string) *app {
	t.Helper()
	return &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: baseURL, APIKey: fakeKey, Tools: map[string]toolState{}},
		hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
}

func TestCodexInstallDownloadsOfficialPackageThroughDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	state := &codexDeviceState{}
	srv := newCodexDeviceServerWithState(t, state)
	t.Setenv("PATH", t.TempDir())
	a := newCodexApp(t, srv.URL)
	target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	standalone := codexStandaloneDir(a.root)
	// 官方安装器时代的残留：bin 软链、current 软链、锁文件与旧版本目录。
	oldRelease := filepath.Join(standalone, "releases", "1.0.0-"+target, "bin")
	if err := os.MkdirAll(oldRelease, 0o700); err != nil {
		t.Fatal(err)
	}
	writeScript(t, oldRelease, "codex", staleCodexScript)
	if err := os.MkdirAll(filepath.Join(a.root, "tools", "codex", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(standalone, "current", "bin", "codex"), filepath.Join(a.root, "tools", "codex", "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/1.0.0-"+target, filepath.Join(standalone, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(standalone, "install.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("install: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	want := filepath.Join(standalone, "releases", "9.9.9-"+target, "bin", "codex")
	st := a.cfg.Tools["codex"]
	if st.Origin != "gate" || st.BinaryPath != want || !strings.Contains(st.DetectedVersion, "9.9.9") {
		t.Fatalf("state=%+v want %s", st, want)
	}
	if got := a.managedCodexPath(); got != want {
		t.Fatalf("managedCodexPath=%q want %q", got, want)
	}
	asset := codexPackageAsset(target)
	if state.hits["channels/latest"] != 1 || state.hits["releases/9.9.9/"+codexChecksumAsset] != 1 || state.hits["releases/9.9.9/"+asset] != 1 {
		t.Fatalf("device hits=%v", state.hits)
	}
	for _, gone := range []string{
		filepath.Join(a.root, "tools", "codex", "bin", "codex"),
		filepath.Join(standalone, "current"),
		filepath.Join(standalone, "install.lock"),
		filepath.Join(standalone, "releases", "1.0.0-"+target),
	} {
		if _, err := os.Lstat(gone); err == nil {
			t.Fatalf("legacy leftover %s survived install", gone)
		}
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("installed codex not executable: %v %v", fi, err)
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "Codex 官方制品校验通过") || !strings.Contains(out, "Codex 9.9.9 制品自检通过") {
		t.Fatalf("narration missing:\n%s", out)
	}

	// 受管升级：已是当前版本就不再下载。
	a.out = &bytes.Buffer{}
	if err := a.install("codex", false, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	if state.hits["releases/9.9.9/"+asset] != 1 {
		t.Fatalf("update re-downloaded the archive: hits=%v", state.hits)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "已是官方当前版本 9.9.9") {
		t.Fatalf("update narration: %s", a.out.(*bytes.Buffer).String())
	}
	// 重新 install（非升级）直接复用受管版本目录，不碰设备的 release 元数据。
	a.cfg.Tools = map[string]toolState{}
	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if state.hits["channels/latest"] != 2 || a.cfg.Tools["codex"].BinaryPath != want {
		t.Fatalf("reinstall should link the managed release without downloading: hits=%v state=%+v", state.hits, a.cfg.Tools["codex"])
	}
}

func TestCodexInstallRejectsDigestMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	asset := codexPackageAsset(target)
	archive := codexPackageTarGz(t, map[string]string{"bin/codex": deviceCodexScript})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/codex-helper/cli/") {
		case "channels/latest":
			_, _ = w.Write([]byte(`{"tag_name":"rust-v9.9.9","assets":[]}`))
		case "releases/9.9.9/" + codexChecksumAsset:
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + asset + "\n"))
		case "releases/9.9.9/" + asset:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := newCodexApp(t, srv.URL)
	if _, err := a.installCodex(); err == nil {
		t.Fatal("digest mismatch must fail the install")
	}
	if _, err := os.Stat(filepath.Join(codexStandaloneDir(a.root), "releases", "9.9.9-"+target)); err == nil {
		t.Fatal("mismatched archive must not be installed")
	}
}

func TestCodexVersionHelpers(t *testing.T) {
	if got := codexVersionOf("codex-cli 0.149.1"); got != "0.149.1" {
		t.Fatalf("codexVersionOf=%q", got)
	}
	if got := codexVersionOf("unknown"); got != "" {
		t.Fatalf("codexVersionOf(unknown)=%q", got)
	}
	for in, want := range map[string]string{"": "latest", "latest": "latest", "rust-v0.1.2": "0.1.2", "v0.1.2": "0.1.2", "0.1.2": "0.1.2"} {
		if got := normalizeCodexRelease(in); got != want {
			t.Fatalf("normalizeCodexRelease(%q)=%q want %q", in, got, want)
		}
	}
	if !codexVersionNewer("0.150.0", "0.149.9") || codexVersionNewer("0.149.1", "0.149.1") || codexVersionNewer("0.9.0", "0.10.0") {
		t.Fatal("codexVersionNewer ordering wrong")
	}
	if got := codexReleaseVersion("9.9.9-x86_64-unknown-linux-musl", "x86_64-unknown-linux-musl"); got != "9.9.9" {
		t.Fatalf("codexReleaseVersion=%q", got)
	}
	if got := codexReleaseVersion("9.9.9-aarch64-apple-darwin", "x86_64-unknown-linux-musl"); got != "" {
		t.Fatalf("foreign target accepted: %q", got)
	}
}

func TestApplyCatalogOverlay(t *testing.T) {
	ov := &catalogOverlay{
		Set:      map[string]json.RawMessage{"use_responses_lite": json.RawMessage(`false`)},
		Default:  map[string]json.RawMessage{"supports_parallel_tool_calls": json.RawMessage(`false`)},
		ToolsAdd: []string{"image_gen"},
	}
	raw := json.RawMessage(`{"slug":"gpt-test","use_responses_lite":true,"supports_parallel_tool_calls":true,"experimental_supported_tools":["web_search","image_gen"]}`)
	got, err := applyCatalogOverlay(raw, ov)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(got, &entry); err != nil {
		t.Fatal(err)
	}
	if entry["use_responses_lite"] != false || entry["supports_parallel_tool_calls"] != true || entry["slug"] != "gpt-test" {
		t.Fatalf("overlay result: %s", got)
	}
	tools, _ := entry["experimental_supported_tools"].([]any)
	if len(tools) != 2 || tools[0] != "web_search" || tools[1] != "image_gen" {
		t.Fatalf("tools not deduped: %v", tools)
	}
	got, err = applyCatalogOverlay(json.RawMessage(`{"slug":"x"}`), ov)
	if err != nil || !bytes.Contains(got, []byte(`"supports_parallel_tool_calls":false`)) || !bytes.Contains(got, []byte(`"experimental_supported_tools":["image_gen"]`)) {
		t.Fatalf("defaults not applied: %s %v", got, err)
	}
	if same, err := applyCatalogOverlay(raw, nil); err != nil || !bytes.Equal(same, raw) {
		t.Fatalf("nil overlay must be identity: %s %v", same, err)
	}
}

func TestCodexCatalogAppliesDeviceOverlayToBundledOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/gate-helper/v1/config":
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"codex","configured":true,"available":true,"default_model":"gpt-test"}],"tools":{"codex":{"default_model":"gpt-test","models":[{"name":"gpt-test","source":"subscription"},{"name":"deepseek-v4","source":"catalog"}]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]}}}`))
		case "/agents/codex/v1/model-catalog":
			_, _ = w.Write([]byte(`{"models":[{"slug":"deepseek-v4","display_name":"LLM Gate · DeepSeek V4","use_responses_lite":true,"experimental_supported_tools":[]}],
				"subscription_overlay":{"set":{"use_responses_lite":false},"default":{"supports_reasoning_summaries":false},"experimental_supported_tools_add":["image_gen"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	bin := writeScript(t, t.TempDir(), "codex", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 1.5.0'; exit 0; fi
if [ "$1 $2 $3" = "debug models --bundled" ]; then echo '{"models":[{"slug":"gpt-test","display_name":"GPT Test","use_responses_lite":true,"experimental_supported_tools":["web_search"]},{"slug":"gpt-other","use_responses_lite":true}]}'; exit 0; fi
if [ "$1 $2" = "debug models" ]; then exit 0; fi
exit 0
`)
	a := newCodexApp(t, srv.URL)
	if err := a.connect("codex", bin, "external"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(a.derivedDir("codex"), "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got catalogEnvelope
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("catalog should hold exactly the two visible models: %s", body)
	}
	entries := map[string]map[string]any{}
	for _, raw := range got.Models {
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		entries[e["slug"].(string)] = e
	}
	sub := entries["gpt-test"]
	if sub["display_name"] != "LLM Gate · GPT Test" || sub["use_responses_lite"] != false || sub["supports_reasoning_summaries"] != false {
		t.Fatalf("bundled subscription entry not overlaid: %v", sub)
	}
	if tools, _ := sub["experimental_supported_tools"].([]any); len(tools) != 2 || tools[1] != "image_gen" {
		t.Fatalf("image_gen not appended: %v", sub)
	}
	cat := entries["deepseek-v4"]
	if cat["display_name"] != "LLM Gate · DeepSeek V4" || cat["use_responses_lite"] != true {
		t.Fatalf("device catalog display name or capabilities changed: %v", cat)
	}
	if tools, _ := cat["experimental_supported_tools"].([]any); len(tools) != 0 {
		t.Fatalf("device catalog entry gained tools: %v", cat)
	}
	if _, ok := cat["supports_reasoning_summaries"]; ok {
		t.Fatalf("device catalog entry gained defaults: %v", cat)
	}
}

func TestTomlPatchHelpers(t *testing.T) {
	text := "# note\nmodel = \"gpt-5\"\nmodel_provider = 'openai' # comment\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"
	if v, ok := tomlTopLevel(text, "model_provider"); !ok || v != "openai" {
		t.Fatalf("literal string: %q %v", v, ok)
	}
	if v, ok := tomlTopLevel(text, "model"); !ok || v != "gpt-5" {
		t.Fatalf("basic string: %q %v", v, ok)
	}
	if _, ok := tomlTopLevel(text, "trust_level"); ok {
		t.Fatal("table keys must not read as top level")
	}
	out := tomlSetTopLevel(text, "model_provider", tomlQuote("llmgate"))
	out = tomlSetTopLevel(out, "model_catalog_json", tomlQuote("/c.json"))
	if !strings.HasPrefix(out, "model_catalog_json = \"/c.json\"\n# note\nmodel = \"gpt-5\"\nmodel_provider = \"llmgate\"\n") {
		t.Fatalf("set top level:\n%s", out)
	}
	out = tomlReplaceSection(out, "[model_providers.llmgate]", "name = \"LLM Gate\"\n")
	if !strings.HasSuffix(out, "trust_level = \"trusted\"\n\n[model_providers.llmgate]\nname = \"LLM Gate\"\n") {
		t.Fatalf("append section:\n%s", out)
	}
	// 段夹在中间时整段替换，后续表原样保留。
	mid := "a = 1\n\n[model_providers.llmgate]\nname = \"old\"\nbase_url = \"x\"\n\n[tail]\nb = 2\n"
	mid = tomlReplaceSection(mid, "[model_providers.llmgate]", "name = \"new\"\n")
	if mid != "a = 1\n\n[model_providers.llmgate]\nname = \"new\"\n\n[tail]\nb = 2\n" {
		t.Fatalf("replace section:\n%s", mid)
	}
	if got := tomlRemoveSection(mid, "[model_providers.llmgate]"); got != "a = 1\n\n[tail]\nb = 2\n" {
		t.Fatalf("remove section:\n%s", got)
	}
	if got := tomlRemoveTopLevel(out, "model_catalog_json"); strings.Contains(got, "model_catalog_json") || !strings.Contains(got, "# note") {
		t.Fatalf("remove top level:\n%s", got)
	}
	if got := tomlSetTopLevel("", "model_provider", tomlQuote("llmgate")); got != "model_provider = \"llmgate\"\n" {
		t.Fatalf("empty file: %q", got)
	}
}

func sharedTestHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	return home
}

func TestCodexSharedConnectPatchesUserHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	home := sharedTestHome(t)
	userConfig := "# my notes\nmodel = \"gpt-5\"\nmodel_provider = \"openai\"\n\n[projects.\"/work\"]\ntrust_level = \"trusted\"\n"
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(userConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	kernel := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	t.Setenv("PATH", t.TempDir())
	a := newCodexApp(t, srv.URL)
	if err := a.connectShared(kernel); err != nil {
		t.Fatalf("connect --shared: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	st, ok := a.cfg.Shared["codex"]
	if !ok || st.KernelPath != kernel || st.Home != home || st.PrevProvider != "openai" || !st.PrevProviderSet || st.ModelWritten != "" || !st.AuthWritten {
		t.Fatalf("shared state=%+v", st)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"# my notes\n", "model = \"gpt-5\"\n", "model_provider = \"llmgate\"\n",
		"model_catalog_json = " + tomlQuote(st.Catalog) + "\n",
		"[projects.\"/work\"]\ntrust_level = \"trusted\"\n",
		"[model_providers.llmgate]\nname = \"LLM Gate\"\nbase_url = " + tomlQuote(srv.URL+"/agents/codex/v1") + "\nwire_api = \"responses\"\nhttp_headers = { \"Authorization\" = \"Bearer " + fakeKey + "\", \"x-openai-actor-authorization\" = \"llmgate\" }\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config.toml missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "env_key") || strings.Contains(text, "model_provider = \"openai\"") {
		t.Fatalf("shared config must not carry env_key or the old provider:\n%s", text)
	}
	if fi, err := os.Stat(cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.toml perm=%v err=%v", fi.Mode(), err)
	}
	auth, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || !bytes.Contains(auth, []byte(`"auth_mode":"apikey"`)) || !bytes.Contains(auth, []byte(fakeKey)) {
		t.Fatalf("auth.json=%s err=%v", auth, err)
	}
	catalogBody, err := os.ReadFile(st.Catalog)
	if err != nil || !bytes.Contains(catalogBody, []byte(`"display_name": "LLM Gate · GPT Test"`)) ||
		!bytes.Contains(catalogBody, []byte(`"slug": "gpt-test"`)) {
		t.Fatalf("shared catalog missing branded model with original slug: err=%v", err)
	}
	if drift := sharedCodexDrift(st, st.Catalog); drift != "" {
		t.Fatalf("fresh shared connect reports drift %q", drift)
	}
	saved, err := os.ReadFile(filepath.Join(a.root, "config.json"))
	if err != nil || !bytes.Contains(saved, []byte(`"shared"`)) {
		t.Fatalf("shared state not persisted: %s %v", saved, err)
	}

	// 桌面 App 首次引导重写了 config.toml：漂移可见，重跑即补回，原值不丢。
	if err := os.WriteFile(cfgPath, []byte("model = \"gpt-5.6-sol\"\n\n[projects.\"/work\"]\ntrust_level = \"trusted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if drift := sharedCodexDrift(st, st.Catalog); drift == "" {
		t.Fatal("rewritten config must report drift")
	}
	if err := a.connectShared(""); err != nil {
		t.Fatalf("re-connect: %v", err)
	}
	st = a.cfg.Shared["codex"]
	if st.PrevProvider != "openai" || !st.PrevProviderSet || st.KernelPath != kernel {
		t.Fatalf("re-connect must keep the original provider and kernel: %+v", st)
	}
	body, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "model = \"gpt-5.6-sol\"\n") || !strings.Contains(string(body), "[model_providers.llmgate]") {
		t.Fatalf("re-connect clobbered the user's model or lost the section:\n%s", body)
	}
	if drift := sharedCodexDrift(st, st.Catalog); drift != "" {
		t.Fatalf("drift after re-connect: %q", drift)
	}

	a.out = &bytes.Buffer{}
	if err := a.toolStatus("codex"); err != nil {
		t.Fatal(err)
	}
	if out := a.out.(*bytes.Buffer).String(); !strings.Contains(out, "共享接入："+cfgPath) || !strings.Contains(out, "共享状态：正常") {
		t.Fatalf("status output:\n%s", out)
	}

	if err := a.disconnectShared(); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(cfgPath)
	text = string(body)
	if !strings.Contains(text, "model_provider = \"openai\"\n") || strings.Contains(text, "llmgate") || strings.Contains(text, "model_catalog_json") {
		t.Fatalf("disconnect did not restore the provider:\n%s", text)
	}
	if !strings.Contains(text, "model = \"gpt-5.6-sol\"\n") || !strings.Contains(text, "[projects.\"/work\"]\ntrust_level = \"trusted\"\n") {
		t.Fatalf("disconnect lost user content:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
		t.Fatal("auth.json written by gate must be removed on disconnect")
	}
	if _, err := os.Stat(st.Catalog); err == nil {
		t.Fatal("shared catalog must be removed on disconnect")
	}
	if _, ok := a.cfg.Shared["codex"]; ok {
		t.Fatal("shared state must be cleared")
	}
}

func TestCodexSharedKeepsChatGPTLoginAndUsersModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	home := sharedTestHome(t)
	login := `{"auth_mode":"chatgpt","tokens":{"access_token":"not-real"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	kernel := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	a := newCodexApp(t, srv.URL)
	if err := a.connectShared(kernel); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != login {
		t.Fatalf("ChatGPT login rewritten: %s", got)
	}
	st := a.cfg.Shared["codex"]
	if st.AuthWritten || st.PrevProviderSet || st.ModelWritten != "gpt-test" {
		t.Fatalf("state=%+v", st)
	}
	body, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if !strings.Contains(string(body), "model = \"gpt-test\"\n") {
		t.Fatalf("default model not written when absent:\n%s", body)
	}
	if err := a.disconnectShared(); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if strings.TrimSpace(string(body)) != "" {
		t.Fatalf("disconnect should leave nothing of its own behind:\n%q", body)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != login {
		t.Fatalf("disconnect touched the ChatGPT login: %s", got)
	}
}

func TestCodexSharedRequiresUsableKernel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	home := sharedTestHome(t)
	stale := writeScript(t, t.TempDir(), "codex", staleCodexScript)
	a := newCodexApp(t, srv.URL)
	err := a.connectShared(stale)
	if err == nil || !strings.Contains(err.Error(), "unrecognized subcommand") {
		t.Fatalf("stale kernel must be refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err == nil {
		t.Fatal("refused kernel must not touch config.toml")
	}
	t.Setenv("PATH", t.TempDir())
	if err := a.connectShared(""); err == nil || !strings.Contains(err.Error(), "--path") {
		t.Fatalf("no candidates must point at --path: %v", err)
	}
	// PATH 里有可用内核时自动绑定；桌面候选在非 macOS 上为空。
	fresh := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	t.Setenv("PATH", filepath.Dir(stale)+string(os.PathListSeparator)+filepath.Dir(fresh))
	if err := a.connectShared(""); err != nil {
		t.Fatalf("auto-discover: %v", err)
	}
	if got := a.cfg.Shared["codex"].KernelPath; got != fresh {
		t.Fatalf("bound %q want %q", got, fresh)
	}
}

func TestLaunchRefreshesSharedCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	home := sharedTestHome(t)
	kernel := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	cli := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	a := newCodexApp(t, srv.URL)
	if err := a.connect("codex", cli, "external"); err != nil {
		t.Fatal(err)
	}
	if err := a.connectShared(kernel); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.launch("codex", nil, strings.NewReader("")); err != nil {
		t.Fatalf("launch: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "[model_providers.llmgate]") || !strings.Contains(string(body), "model_provider = \"llmgate\"") {
		t.Fatalf("launch did not restore the shared config:\n%s", body)
	}
	// 内核失效只告警，不拦启动。
	if err := os.Remove(kernel); err != nil {
		t.Fatal(err)
	}
	a.err = &bytes.Buffer{}
	if err := a.launch("codex", nil, strings.NewReader("")); err != nil {
		t.Fatalf("launch with a missing shared kernel must still start the CLI: %v", err)
	}
	if !strings.Contains(a.err.(*bytes.Buffer).String(), "共享接入未能刷新") {
		t.Fatalf("missing kernel warning absent: %s", a.err.(*bytes.Buffer).String())
	}
}
