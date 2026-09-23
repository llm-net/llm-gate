package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mcodeTestTool() runtimeTool {
	return runtimeTool{DefaultModel: "chat-test", Models: []runtimeModel{{Name: "chat-test", Source: "catalog"}}}
}

func mcodeTestSnapshot(base string) string {
	b, _ := json.Marshal(map[string]any{"providers": []any{map[string]any{
		"providerId": mcodeProviderID, "apiFormat": "openai-completions", "baseUrl": base + "/v1",
		"enabled": true, "hasApiKey": true, "models": []any{map[string]any{"modelId": "chat-test", "selected": true}},
	}}})
	return string(b)
}

func TestMCodeConfigAndEnvironment(t *testing.T) {
	body, err := mcodeConfig("https://device.invalid", fakeKey, mcodeTestTool())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["defaultModel"] != mcodeProviderID+"/chat-test" {
		t.Fatal("wrong default model")
	}
	provider := got["custom_provider"].(map[string]any)["llmgate"].(map[string]any)
	options := provider["options"].(map[string]any)
	if options["baseURL"] != "https://device.invalid/v1" || options["apiKey"] != fakeKey || provider["api"] != "openai-completions" {
		t.Fatal("wrong provider configuration")
	}
	for _, bad := range []runtimeTool{
		{}, {DefaultModel: "missing", Models: mcodeTestTool().Models},
		{DefaultModel: "chat-test", Models: []runtimeModel{{Name: "chat-test", Source: "subscription"}}},
	} {
		if _, err := mcodeConfig("https://device.invalid", fakeKey, bad); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	env := mcodeEnv([]string{"PATH=/bin", "MINIMAX_DATA_DIR=/original", "MAVIS_DATA_DIR=/other", "MCODE_API_BASE_URL=https://wrong.invalid", "__MAVIS_RUNTIME_DATA_DIR=/wrong", "MCODE_PROVIDER_API_KEY=fake-other"}, "/derived")
	if envValue(env, "PATH") != "/bin" || envValue(env, "MINIMAX_DATA_DIR") != "/derived" || envValue(env, "MAVIS_DATA_DIR") != "/derived" || envValue(env, "MCODE_DISABLE_TELEMETRY") != "1" {
		t.Fatal("profile isolation failed")
	}
	if envValue(env, "MCODE_API_BASE_URL") != "" || envValue(env, "MCODE_PROVIDER_API_KEY") != "" || envValue(env, "__MAVIS_RUNTIME_DATA_DIR") != "" {
		t.Fatal("inherited override survived")
	}
	if toolBinaryNameFor("windows", "mcode") != "mcode.cmd" || toolBinaryNameFor("linux", "mcode") != "mcode" {
		t.Fatal("wrong platform entry")
	}
}

func TestMCodeSnapshotRejectsWrongModelOrRoute(t *testing.T) {
	body := mcodeTestSnapshot("https://device.invalid")
	if !validMCodeSnapshot([]byte(body), "https://device.invalid", mcodeTestTool()) {
		t.Fatal("valid snapshot rejected")
	}
	for _, bad := range []string{"{}", "not json", strings.ReplaceAll(body, "chat-test", "other"), strings.ReplaceAll(body, "openai-completions", "anthropic-messages"), strings.ReplaceAll(body, "device.invalid", "wrong.invalid"), strings.ReplaceAll(body, `"selected":true`, `"selected":false`)} {
		if validMCodeSnapshot([]byte(bad), "https://device.invalid", mcodeTestTool()) {
			t.Fatal("invalid snapshot accepted")
		}
	}
}

func TestMCodeLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake CLI")
	}
	tool := mcodeTestTool()
	allow := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gate-helper/v1/config" || r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Error("unexpected request")
			w.WriteHeader(401)
			return
		}
		current := tool
		if !allow {
			current = runtimeTool{}
		}
		// Exercise fallback for firmware without tools.mcode.
		json.NewEncoder(w).Encode(runtimeConfig{SchemaVersion: schemaVersion, Tools: map[string]runtimeTool{"opencode": current}})
	}))
	defer srv.Close()
	root := t.TempDir()
	out := &bytes.Buffer{}
	a := &app{root: root, cfg: emptyConfig(), hc: srv.Client(), out: out, err: out}
	a.cfg.BaseURL, a.cfg.APIKey = srv.URL, fakeKey
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "mcode")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 0.4.12; exit 0; fi\nif [ \"$1\" = provider ]; then\ncat <<'JSON'\n" + mcodeTestSnapshot(srv.URL) + "\nJSON\nexit 0\nfi\nprintf '%s\\n' \"$MINIMAX_DATA_DIR\" \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := a.install("mcode", false, false); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Tools["mcode"].Origin != "external" {
		t.Fatal("external CLI ownership changed")
	}
	configPath := filepath.Join(a.derivedDir("mcode"), "config.yaml")
	original, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(original, []byte(fakeKey)) {
		t.Fatal("config missing")
	}
	if st, err := os.Stat(configPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("insecure config permissions")
	}
	if err := a.launch("mcode", []string{"exec", "fake test prompt"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), a.derivedDir("mcode")+"\nexec\nfake test prompt") || strings.Contains(out.String(), fakeKey) {
		t.Fatal("launch isolation, arguments or redaction failed")
	}
	allow = false
	if err := a.launch("mcode", nil, nil); err == nil {
		t.Fatal("revoked policy launched")
	}
	allow = true
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'untrusted output'; exit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.prepareMCode(bin, tool); !isCLIUnusable(err) || strings.Contains(err.Error(), "untrusted output") {
		t.Fatal("self check must fail without echoing output")
	}
	if after, _ := os.ReadFile(configPath); !bytes.Equal(original, after) {
		t.Fatal("failed check replaced working configuration")
	}
	if err := a.install("mcode", false, true); err == nil {
		t.Fatal("external update adopted unexpectedly")
	}
	// The CLI can rewrite JSON into YAML when the user selects another model.
	if err := os.WriteFile(configPath, []byte("custom_provider:\n  llmgate:\n    options:\n      apiKey: "+fakeKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(a.derivedDir("mcode"), "session-test.json")
	os.WriteFile(session, []byte("fake session"), 0o600)
	if err := a.disconnect("mcode"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatal("disconnect left credential configuration")
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatal("disconnect removed session")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("disconnect removed external CLI")
	}
}

// Explicitly opt in on a customer-side host with an official CLI installed.
// The probe uses fake credentials and a local fake OpenAI Chat service.
func TestMCodeRealCLI(t *testing.T) {
	path := os.Getenv("LLMGATE_TEST_MCODE_PATH")
	if path == "" {
		t.Skip("set LLMGATE_TEST_MCODE_PATH to test the official CLI")
	}
	testMCodeRealInference(t, path)
}

func testMCodeRealInference(t *testing.T, path string) {
	t.Helper()
	workspace := t.TempDir()
	t.Chdir(workspace)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses/input_tokens" {
			// Optional token counting; firmware does not expose this endpoint.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Errorf("unexpected request route or key: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "chat-test" {
			t.Error("unexpected model")
			w.WriteHeader(400)
			return
		}
		requests.Add(1)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"MCODE_GATE_OK\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","model":"chat-test","choices":[{"index":0,"message":{"role":"assistant","content":"MCODE_GATE_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
		}
	}))
	defer srv.Close()
	a := &app{root: t.TempDir(), cfg: emptyConfig()}
	a.cfg.BaseURL, a.cfg.APIKey = srv.URL, fakeKey
	if err := a.prepareMCode(path, mcodeTestTool()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := mcodeCommand(ctx, path, "exec", "--max-steps", "1", "--output-format", "json", "Return a short greeting.")
	cmd.Env, cmd.Dir = a.toolEnv("mcode"), workspace
	var output mcodeCheckOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("official exec failed: %v; local model requests=%d", err, requests.Load())
	}
	if requests.Load() == 0 || !bytes.Contains(output.Bytes(), []byte("MCODE_GATE_OK")) {
		t.Fatal("official CLI did not complete through the configured model service")
	}
}

func TestMCodeRealManagedInstall(t *testing.T) {
	if os.Getenv("LLMGATE_TEST_MCODE_INSTALL") != "1" {
		t.Skip("set LLMGATE_TEST_MCODE_INSTALL=1 for official network installation")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(runtimeConfig{SchemaVersion: schemaVersion, Tools: map[string]runtimeTool{"mcode": mcodeTestTool()}})
	}))
	defer srv.Close()
	a := &app{root: t.TempDir(), cfg: emptyConfig(), hc: srv.Client(), out: os.Stdout, err: os.Stderr}
	a.cfg.BaseURL, a.cfg.APIKey = srv.URL, fakeKey
	old, oldNode := mcodeOfficialBase, mcodeNodeOfficialBase
	mcodeOfficialBase = "https://filecdn.minimax.chat/public"
	mcodeNodeOfficialBase = "https://nodejs.org/dist"
	t.Cleanup(func() { mcodeOfficialBase, mcodeNodeOfficialBase = old, oldNode })
	if err := a.install("mcode", true, true); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Tools["mcode"].Origin != "gate" || a.managedMCodePath() == "" {
		t.Fatal("official installation was not recorded")
	}
	testMCodeRealInference(t, a.managedMCodePath())
}

func TestMCodeManagedInstallAndFailedUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake installer")
	}
	failed, badHash := false, false
	asset, _ := mcodeNodeAsset(runtime.GOOS, runtime.GOARCH)
	archive := cursorTarGz(t, []cursorTarEntry{{name: "node/bin/node", mode: 0700, body: "#!/bin/sh\necho v" + mcodeNodeVersion + "\n"}})
	checksum := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)
	var installer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gate-helper/v1/config" {
			json.NewEncoder(w).Encode(runtimeConfig{SchemaVersion: schemaVersion, Tools: map[string]runtimeTool{"mcode": mcodeTestTool()}})
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("artifact request contains credentials")
		}
		if strings.HasSuffix(r.URL.Path, "/SHASUMS256.txt") {
			io.WriteString(w, checksum)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/"+asset) {
			if badHash {
				w.Write([]byte("tampered archive"))
				return
			}
			w.Write(archive)
			return
		}
		if r.URL.Path != "/mcode-helper/cli/install.sh" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected installer request or credential forwarding")
			w.WriteHeader(404)
			return
		}
		if failed {
			w.Write([]byte("#!/bin/sh\n# MCODE_INSTALL_DIR MCODE_NO_MODIFY_PATH @minimax-ai/code\nexit 1\n"))
			return
		}
		w.Write([]byte(installer))
	}))
	defer srv.Close()
	installer = `#!/bin/sh
# @minimax-ai/code MCODE_INSTALL_DIR MCODE_NO_MODIFY_PATH
set -eu
[ "$MCODE_NO_MODIFY_PATH" = 1 ]
[ -z "${NPM_TOKEN:-}" ]
[ -z "${BASH_ENV:-}" ]
[ -z "${NODE_OPTIONS:-}" ]
mkdir -p "$MCODE_INSTALL_DIR/bin"
cat > "$MCODE_INSTALL_DIR/bin/mcode" <<'CLI'
#!/bin/sh
if [ "$1" = --version ]; then echo 0.4.12; exit 0; fi
cat <<'JSON'
` + mcodeTestSnapshot(srv.URL) + `
JSON
CLI
chmod 700 "$MCODE_INSTALL_DIR/bin/mcode"
`
	old := mcodeOfficialBase
	mcodeOfficialBase = ""
	t.Cleanup(func() { mcodeOfficialBase = old })
	t.Setenv("NPM_TOKEN", "fake-npm-token")
	t.Setenv("BASH_ENV", "/nonexistent-untrusted")
	t.Setenv("NODE_OPTIONS", "--untrusted")
	a := &app{root: t.TempDir(), cfg: emptyConfig(), hc: srv.Client(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	a.cfg.BaseURL, a.cfg.APIKey = srv.URL, fakeKey
	// --adopt deliberately bypasses any external installation on the test host.
	if err := a.install("mcode", true, true); err != nil {
		t.Fatal(err)
	}
	first := a.cfg.Tools["mcode"]
	if first.Origin != "gate" || a.managedMCodePath() != first.BinaryPath {
		t.Fatal("managed installation not recorded")
	}
	state, err := readInstallState(a.root)
	if err != nil || len(state.Programs["mcode"].Nodes) < 4 {
		t.Fatal("dependency tree not inventoried")
	}
	failed = true
	if err := a.install("mcode", false, true); err == nil {
		t.Fatal("failed installer accepted")
	}
	if a.cfg.Tools["mcode"] != first || a.managedMCodePath() != first.BinaryPath {
		t.Fatal("failed update changed active installation")
	}
	entries, _ := os.ReadDir(filepath.Join(a.root, "tools", "mcode", "releases"))
	if len(entries) != 1 {
		t.Fatal("failed install left a partial tree")
	}
	failed, badHash = false, true
	if err := a.install("mcode", false, true); err == nil {
		t.Fatal("bad Node.js digest accepted")
	}
	if a.cfg.Tools["mcode"] != first {
		t.Fatal("bad digest changed active installation")
	}
	badHash = false
	if err := a.install("mcode", false, true); err != nil {
		t.Fatal(err)
	}
	second := a.cfg.Tools["mcode"]
	if second.BinaryPath == first.BinaryPath || second.Origin != "gate" {
		t.Fatal("update did not switch to a new managed tree")
	}
	if err := a.uninstall("mcode", []string{"--yes"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.BinaryPath); !os.IsNotExist(err) {
		t.Fatal("managed program survived uninstall")
	}
	if _, err := os.Stat(second.BinaryPath); !os.IsNotExist(err) {
		t.Fatal("updated program survived uninstall")
	}
}

func TestMCodeExplicitEmptyPolicyDoesNotFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(runtimeConfig{SchemaVersion: schemaVersion, Tools: map[string]runtimeTool{"mcode": {}, "opencode": mcodeTestTool()}})
	}))
	defer srv.Close()
	a := &app{hc: srv.Client()}
	got, err := a.fetchRuntimeAt(srv.URL, fakeKey)
	if err != nil || len(got.Tools["mcode"].Models) != 0 {
		t.Fatal("explicit empty policy expanded")
	}
}

func TestMCodeWindowsLauncherArguments(t *testing.T) {
	args := []string{"exec", "a quoted prompt & $(do-not-run); %PATH%"}
	program, argv := mcodeCommandArgs("C:/Tool Dir/mcode.cmd", "windows", args)
	want := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "C:/Tool Dir/mcode.ps1", args[0], args[1]}
	if program != "powershell.exe" || !reflect.DeepEqual(argv, want) {
		t.Fatal("Windows launcher must pass literal arguments through -File")
	}
}
