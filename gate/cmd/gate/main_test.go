package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeKey = "sk_example_only_never_real"

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func TestNormalizeBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"192.0.2.10/":                         "http://192.0.2.10",
		"https://box.invalid/v1":              "https://box.invalid",
		"https://box.invalid/codex/v1":        "https://box.invalid",
		"https://box.invalid/agents/codex/v1": "https://box.invalid",
		"https://box.invalid/agents/claude":   "https://box.invalid",
		"https://box.invalid/claude":          "https://box.invalid",
	} {
		got, err := normalizeBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("normalizeBaseURL(%q)=(%q,%v), want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "ftp://box.invalid", "http://user@box.invalid", "http://box.invalid/x", "http://box.invalid?q=1", "<设备地址>"} {
		if got, err := normalizeBaseURL(raw); err == nil {
			t.Errorf("normalizeBaseURL(%q)=%q, want error", raw, got)
		}
	}
}

func TestSetURLAndKeyAreVerifiedBeforeAtomicSave(t *testing.T) {
	var acceptKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/gate-helper/v1/config":
			if !acceptKey || r.Header.Get("Authorization") != "Bearer "+fakeKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	a := &app{root: root, cfg: emptyConfig(), hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.setURL(srv.URL + "/v1"); err != nil {
		t.Fatal(err)
	}
	if err := a.setKey(fakeKey); err == nil {
		t.Fatal("rejected key should fail")
	}
	loaded, err := loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "" {
		t.Fatal("rejected key was persisted")
	}
	acceptKey = true
	if err := a.setKey(fakeKey); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(root, "config.json"))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("config mode=%v err=%v", st.Mode().Perm(), err)
		}
	}
}

func TestBootstrapUpdatesAddressAndKeyAsOnePair(t *testing.T) {
	const oldKey = "sk_old_example_only"
	const newKey = "sk_new_example_only"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/gate-helper/v1/config":
			if r.Header.Get("Authorization") != "Bearer "+newKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	a := &app{
		root: root,
		cfg: config{SchemaVersion: 1, BaseURL: "https://old.invalid", APIKey: oldKey,
			Tools: map[string]toolState{"codex": {BinaryPath: "/old/codex", Origin: "external"}}},
		hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{},
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	if err := a.bootstrap(srv.URL, "sk_rejected_example"); err == nil {
		t.Fatal("rejected address/key pair should fail")
	}
	got, err := loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://old.invalid" || got.APIKey != oldKey || got.Tools["codex"].BinaryPath != "/old/codex" {
		t.Fatalf("failed bootstrap changed config: %+v", got)
	}
	if err := a.bootstrap(srv.URL, newKey); err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != srv.URL || got.APIKey != newKey || got.Tools["codex"].BinaryPath != "/old/codex" {
		t.Fatalf("successful bootstrap did not atomically update pair: %+v", got)
	}
}

func TestCodexConnectLaunchIsolationAndRevocation(t *testing.T) {
	enabled := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey && r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/gate-helper/v1/config":
			models := `[]`
			def := ""
			if enabled {
				models, def = `[{"name":"gpt-test","source":"subscription"}]`, "gpt-test"
			}
			json.NewEncoder(w).Encode(json.RawMessage(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"codex","configured":true,"available":true,"default_model":"gpt-test"}],"tools":{"codex":{"default_model":"` + def + `","models":` + models + `},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]}}}`))
		case "/agents/codex/v1/model-catalog":
			w.Write([]byte(`{"models":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 1.0.0'; exit 0; fi
if [ "$1 $2 $3" = "debug models --bundled" ]; then echo '{"models":[{"slug":"gpt-test","display_name":"GPT Test"}]}'; exit 0; fi
if [ "$1 $2" = "debug models" ]; then exit 0; fi
printf 'args=%s codex_home=%s key=%s\n' "$*" "$CODEX_HOME" "$LLMGATE_API_KEY"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.connect("codex", bin, "external"); err != nil {
		t.Fatal(err)
	}
	managed := a.cfg.Tools["codex"]
	managed.Origin = "gate"
	a.cfg.Tools["codex"] = managed
	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("repeated managed install: %v", err)
	}
	if got := a.cfg.Tools["codex"].Origin; got != "gate" {
		t.Fatalf("repeated install changed managed origin to %q", got)
	}
	external := a.cfg.Tools["codex"]
	external.Origin = "external"
	a.cfg.Tools["codex"] = external
	if err := a.install("codex", false, true); err == nil || !strings.Contains(err.Error(), "外部渠道管理") {
		t.Fatalf("external update should be refused, got %v", err)
	}
	a.cfg.Tools["codex"] = managed
	configBody, err := os.ReadFile(filepath.Join(root, "tools", "codex", "derived", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configBody, []byte(fakeKey)) || !bytes.Contains(configBody, []byte(`env_key = "LLMGATE_API_KEY"`)) {
		t.Fatalf("Codex config leaks key or lacks env_key:\n%s", configBody)
	}
	// 内核只在 provider 带非空 x-openai-actor-authorization 时挂出内置 image_gen
	// 工具；缺了它设备画图门永远不会被打到。
	if !bytes.Contains(configBody, []byte(`http_headers = { "x-openai-actor-authorization" = "llmgate" }`)) {
		t.Fatalf("Codex config lacks the actor authorization header that unlocks image_gen:\n%s", configBody)
	}
	a.out = &bytes.Buffer{}
	if err := a.launch("codex", []string{"--dangerously-bypass-approvals-and-sandbox"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	output := a.out.(*bytes.Buffer).String()
	if !strings.Contains(output, "args=--dangerously-bypass-approvals-and-sandbox") || !strings.Contains(output, "key="+fakeKey) || !strings.Contains(output, filepath.Join(root, "tools", "codex", "derived")) {
		t.Fatalf("launch forwarding failed: %s", output)
	}
	enabled = false
	if err := a.launch("codex", nil, strings.NewReader("")); err == nil {
		t.Fatal("revoked policy should prevent a new launch")
	}
	a.out = &bytes.Buffer{}
	if err := a.toolStatus("codex"); err != nil {
		t.Fatal(err)
	}
	if got := a.out.(*bytes.Buffer).String(); !strings.Contains(got, "codex：无可用模型") || !strings.Contains(got, "可见模型：0") {
		t.Fatalf("revoked status=%q", got)
	}
}

func TestGrokAndClaudeConnectLaunchIsolation(t *testing.T) {
	t.Run("grok", func(t *testing.T) {
		managed := "# >>> SOC AGENT grok managed block >>>\n" +
			"[model.\"llmgate-grok-test\"]\nname = \"LLM Gate · Grok Test\"\n" +
			"model = \"grok-test\"\napi_key = \"" + fakeKey + "\"\n" +
			"# <<< SOC AGENT grok managed block <<<\n"
		legacy := strings.ReplaceAll(managed, "LLM Gate · Grok Test", "SOC AGENT · Grok Test")
		legacyDir := t.TempDir()
		legacyPath := filepath.Join(legacyDir, "config.toml")
		if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GROK_HOME", legacyDir)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fakeKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/gate-helper/v1/config":
				w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"grok","configured":true,"available":true,"default_model":"grok-test"}],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"grok-test","models":[{"name":"grok-test","source":"subscription"}]},"claude":{"default_model":"","models":[]}}}`))
			case "/grok-helper/managed-config":
				w.Write([]byte(managed))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		root := t.TempDir()
		bin := writeFakeTool(t, "grok", `printf 'grok_home=%s args=%s\n' "$GROK_HOME" "$*"
sed -n '/^name = /p' "$GROK_HOME/config.toml"`)
		a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		if err := a.connect("grok", bin, "external"); err != nil {
			t.Fatal(err)
		}
		// 已关联的旧派生配置也必须在启动前整份刷新。
		derivedPath := filepath.Join(a.derivedDir("grok"), "config.toml")
		if err := os.WriteFile(derivedPath, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := a.launch("grok", []string{"--example"}, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		out := a.out.(*bytes.Buffer).String()
		if !strings.Contains(out, "grok_home="+filepath.Join(root, "tools", "grok", "derived")) ||
			!strings.Contains(out, "args=--example") || !strings.Contains(out, `name = "LLM Gate · Grok Test"`) ||
			strings.Contains(out, `name = "SOC AGENT`) {
			t.Fatalf("Grok launch did not use isolated config: %s", out)
		}
		body, err := os.ReadFile(derivedPath)
		if err != nil || string(body) != managed {
			t.Fatalf("Grok managed config was not replaced with device response: err=%v", err)
		}
		original, err := os.ReadFile(legacyPath)
		if err != nil || string(original) != legacy {
			t.Fatalf("Grok original config changed: err=%v", err)
		}
	})

	t.Run("claude", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fakeKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/gate-helper/v1/config":
				w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"claude","configured":true,"available":true,"default_model":"claude-test"}],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"claude-test","models":[{"name":"claude-test","source":"subscription"}]}}}`))
			case "/agents/claude/v1/models":
				w.Write([]byte(`{"data":[{"id":"claude-test","type":"model"}]}`))
			case "/agents/claude/api/hello":
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		root := t.TempDir()
		bin := writeFakeTool(t, "claude", `printf 'claude_home=%s base=%s token=%s args=%s\n' "$CLAUDE_CONFIG_DIR" "$ANTHROPIC_BASE_URL" "$ANTHROPIC_AUTH_TOKEN" "$*"`)
		a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: srv.Client(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		if err := a.connect("claude", bin, "external"); err != nil {
			t.Fatal(err)
		}
		if err := a.launch("claude", []string{"--example"}, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		out := a.out.(*bytes.Buffer).String()
		cliSettings := filepath.Join(root, "tools", "claude", "derived", "cli-settings.json")
		if !strings.Contains(out, "claude_home="+filepath.Join(root, "tools", "claude", "derived")) ||
			!strings.Contains(out, "base="+srv.URL+"/agents/claude") || !strings.Contains(out, "token="+fakeKey) ||
			!strings.Contains(out, "args=--settings "+cliSettings+" --example") {
			t.Fatalf("Claude launch did not use isolated config or --settings injection: %s", out)
		}
		if body, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "settings.json")); err != nil || bytes.Contains(body, []byte(fakeKey)) {
			t.Fatalf("user-level derived settings leaked key or missing: err=%v body=%q", err, body)
		}
		info, err := os.Stat(cliSettings)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("cli-settings.json mode = %v, want 0600", info.Mode().Perm())
		}
		var cli struct {
			Env map[string]string `json:"env"`
		}
		body, err := os.ReadFile(cliSettings)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &cli); err != nil {
			t.Fatal(err)
		}
		apiKey, hasAPIKey := cli.Env["ANTHROPIC_API_KEY"]
		oauth, hasOAuth := cli.Env["CLAUDE_CODE_OAUTH_TOKEN"]
		if cli.Env["ANTHROPIC_BASE_URL"] != srv.URL+"/agents/claude" || cli.Env["ANTHROPIC_AUTH_TOKEN"] != fakeKey ||
			!hasAPIKey || apiKey != "" || !hasOAuth || oauth != "" ||
			cli.Env["ANTHROPIC_MODEL"] == "" || cli.Env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "1" {
			t.Fatalf("cli-settings env does not pin connectivity: %+v", cli.Env)
		}
	})
}

func TestGrokDownloadUsesArtifactTimeoutAndAtomicReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'grok 9.9.9'; exit 0; fi\nexit 0\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body[:16])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write(body[16:])
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Timeout = 10 * time.Millisecond
	root := t.TempDir()
	a := &app{root: root, cfg: config{BaseURL: srv.URL}, hc: client}
	target, err := a.downloadGrokBinary("grok-9.9.9-linux-x86_64")
	if err != nil {
		t.Fatalf("download with artifact timeout: %v", err)
	}
	wantTarget := filepath.Join(root, "tools", "grok", "bin", "grok")
	if target != wantTarget {
		t.Fatalf("target = %q, want %q", target, wantTarget)
	}
	installed, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(installed, body) {
		t.Fatalf("installed binary mismatch: err=%v body=%q", err, installed)
	}
}

func TestGrokDownloadSelfCheckFailureKeepsExistingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not an executable Grok program"))
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	target := filepath.Join(root, "tools", "grok", "bin", "grok")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	old := []byte("#!/bin/sh\necho 'grok 1.0.5'\n")
	if err := os.WriteFile(target, old, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{root: root, cfg: config{BaseURL: srv.URL}, hc: srv.Client()}
	if _, err := a.downloadGrokBinary("grok-9.9.9-linux-x86_64"); err == nil || !strings.Contains(err.Error(), "无法运行") {
		t.Fatalf("invalid Grok binary error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("existing Grok binary changed: err=%v body=%q", err, got)
	}
}

// cursorDeviceState 驱动假设备：订阅勾选/可用两开关、下发默认模型、exchange
// 探针的行为（缺省返回带 sub 的本地 JWT；可改令牌或改回错误码），以及匿名
// 公开面 /cursor-helper/cli/* 的文件表（nil 表示恒 404）。
type cursorDeviceState struct {
	configured, available bool
	defaultModel          string
	exchangeStatus        int    // 0 = 200 正常回显
	exchangeToken         string // 非空 = 覆盖 accessToken 回显
	codexInstallerHits    int
	cliFiles              map[string][]byte // /cursor-helper/cli/<path> → 正文
	cliHits               map[string]int
}

func newCursorDeviceServer(t *testing.T, state *cursorDeviceState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/codex-helper/") {
			state.codexInstallerHits++
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/cursor-helper/cli/") {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public cursor CLI request carried Authorization")
			}
			rel := strings.TrimPrefix(r.URL.Path, "/cursor-helper/cli/")
			if state.cliHits == nil {
				state.cliHits = map[string]int{}
			}
			state.cliHits[rel]++
			if body, ok := state.cliFiles[rel]; ok {
				_, _ = w.Write(body)
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/gate-helper/v1/config":
			models, def := `[]`, ""
			if state.configured && state.available {
				def = state.defaultModel
				models = `[{"name":"legacy-cursor-model","source":"subscription"},{"name":"legacy-cursor-model-fast","source":"subscription"}]`
			}
			fmt.Fprintf(w, `{"schema_version":1,"revision":1,"subscriptions":[{"provider":"cursor","configured":%t,"available":%t,"default_model":%q}],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]},"cursor":{"default_model":%q,"models":%s},"opencode":{"default_model":"","models":[]}}}`,
				state.configured, state.available, def, def, models)
		case "/agents/cursor/auth/exchange_user_api_key":
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			if state.exchangeStatus != 0 {
				w.WriteHeader(state.exchangeStatus)
				return
			}
			token := state.exchangeToken
			if token == "" {
				token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJjdXJzb3ItdGVzdC11c2VyIn0.test-signature"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"llmgate"}`, token)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeCursorFakeTool 仿真 cursor-agent：--version 只打裸版本串（真机形态），
// 每次调用把 argv 追加进 marker，透传启动时回显注入环境与参数。
func writeCursorFakeTool(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor-agent")
	script := fmt.Sprintf(`#!/bin/sh
echo "argv:$*" >> %q
if [ "$1" = "--version" ]; then echo '2026.08.11-e8db854'; exit 0; fi
printf 'endpoint=%%s key=%%s confdir=%%s datadir=%%s credstore=%%s args=%%s\n' "$CURSOR_API_ENDPOINT" "$CURSOR_API_KEY" "$CURSOR_CONFIG_DIR" "$CURSOR_DATA_DIR" "$AGENT_CLI_CREDENTIAL_STORE" "$*"
`, marker)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCursorConnectLaunchIsolationAndAutoUpdateFlag(t *testing.T) {
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model"}
	srv := newCursorDeviceServer(t, state)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "invocations.log")
	bin := writeCursorFakeTool(t, marker)
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.connect("cursor", bin, "external"); err != nil {
		t.Fatal(err)
	}
	if st := a.cfg.Tools["cursor"]; st.Origin != "external" || st.DetectedVersion != "2026.08.11-e8db854" {
		t.Fatalf("linked state=%+v", st)
	}
	derivedDir := filepath.Join(root, "tools", "cursor", "derived")
	derivedPath := filepath.Join(derivedDir, "cli-config.json")
	body, err := os.ReadFile(derivedPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(fakeKey)) {
		t.Fatalf("derived cli-config leaked key: %s", body)
	}
	var derived struct {
		Version int `json:"version"`
		Network struct {
			UseHTTP1ForAgent bool `json:"useHttp1ForAgent"`
		} `json:"network"`
	}
	if err := json.Unmarshal(body, &derived); err != nil {
		t.Fatal(err)
	}
	if derived.Version != 1 || !derived.Network.UseHTTP1ForAgent || bytes.Contains(body, []byte(`"model"`)) {
		t.Fatalf("derived cli-config=%+v", derived)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(derivedPath); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("derived mode=%v err=%v", st.Mode().Perm(), err)
		}
	}
	if st, err := os.Stat(filepath.Join(derivedDir, "data")); err != nil || !st.IsDir() {
		t.Fatalf("cursor data dir missing: %v", err)
	}
	env := a.toolEnv("cursor")
	for key, want := range map[string]string{
		"CURSOR_API_ENDPOINT":        srv.URL + "/agents/cursor",
		"CURSOR_API_KEY":             fakeKey,
		"CURSOR_CONFIG_DIR":          derivedDir,
		"CURSOR_DATA_DIR":            filepath.Join(derivedDir, "data"),
		"AGENT_CLI_CREDENTIAL_STORE": "memory",
	} {
		if got := envValue(env, key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
	if err := a.toolStatus("cursor"); err != nil {
		t.Fatal(err)
	}
	if got := a.out.(*bytes.Buffer).String(); strings.Contains(got, "可见模型") || strings.Contains(got, "无可用模型") {
		t.Fatalf("Cursor 状态不应展示模型级读数: %s", got)
	}
	if invocations, err := os.ReadFile(marker); err != nil || string(invocations) != "argv:--version\n" {
		t.Fatalf("connect/status 不得注入启动参数: %q err=%v", invocations, err)
	}
	a.out = &bytes.Buffer{}
	if err := a.launch("cursor", []string{"--example"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	output := a.out.(*bytes.Buffer).String()
	if !strings.Contains(output, "args=--disable-auto-update --example") {
		t.Fatalf("--disable-auto-update must precede user args: %s", output)
	}
	for _, want := range []string{
		"endpoint=" + srv.URL + "/agents/cursor",
		"key=" + fakeKey,
		"confdir=" + derivedDir,
		"datadir=" + filepath.Join(derivedDir, "data"),
		"credstore=memory",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("launch env missing %q: %s", want, output)
		}
	}
}

func TestCursorConnectRefusedWithoutAuthorizedSubscription(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		configured, available bool
		want                  string
	}{
		{"unconfigured", false, false, "API密钥 → 可用订阅"},
		{"unavailable", true, false, "订阅当前不可用"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &cursorDeviceState{configured: tc.configured, available: tc.available}
			srv := newCursorDeviceServer(t, state)
			root := t.TempDir()
			bin := writeCursorFakeTool(t, filepath.Join(t.TempDir(), "invocations.log"))
			a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
			err := a.connect("cursor", bin, "external")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("connect err=%v want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), fakeKey) {
				t.Fatalf("error leaked key: %v", err)
			}
			if _, ok := a.cfg.Tools["cursor"]; ok {
				t.Fatal("unauthorized cursor must not be linked")
			}
			if _, err := os.Stat(filepath.Join(root, "tools", "cursor", "derived", "cli-config.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("derived config must not be created: %v", err)
			}
			a.cfg.Tools["cursor"] = toolState{BinaryPath: bin, DetectedVersion: "2026.08.11-e8db854", Origin: "external"}
			if err := a.launch("cursor", nil, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("launch err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestCursorProbeFailureKeepsExistingDerivedConfig(t *testing.T) {
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model",
		exchangeToken: "sk_probe_mismatch_example"}
	srv := newCursorDeviceServer(t, state)
	root := t.TempDir()
	derivedPath := filepath.Join(root, "tools", "cursor", "derived", "cli-config.json")
	sentinel := []byte("{\n  \"version\": 1,\n  \"model\": \"old-model\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(derivedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(derivedPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeCursorFakeTool(t, filepath.Join(t.TempDir(), "invocations.log"))
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := a.connect("cursor", bin, "external")
	if err == nil || !strings.Contains(err.Error(), "探针") {
		t.Fatalf("mismatched exchange echo must fail connect: %v", err)
	}
	if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), "sk_probe_mismatch_example") {
		t.Fatalf("probe error leaked key material: %v", err)
	}
	state.exchangeToken = ""
	state.exchangeStatus = http.StatusInternalServerError
	if err := a.connect("cursor", bin, "external"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("exchange 500 must fail connect: %v", err)
	}
	got, err := os.ReadFile(derivedPath)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("derived config changed on probe failure: err=%v body=%q", err, got)
	}
}

func TestPrepareCursorRemovesLegacyModelAndPreservesCurrentConfig(t *testing.T) {
	state := &cursorDeviceState{configured: true, available: true}
	srv := newCursorDeviceServer(t, state)
	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey}, hc: newHTTPClient()}
	configPath := filepath.Join(root, "tools", "cursor", "derived", "cli-config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{
  "version": 1,
  "model": {"modelId":"claude-opus-4.8","displayName":"Claude Opus 4.8"},
  "authInfo": {"authId":"cursor-user-123"},
  "preferences": {"privacyMode":true},
  "network": {"useHttp1ForAgent":false,"futureOption":"keep"}
}`)
	if err := os.WriteFile(configPath, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	rc := runtimeConfig{
		SchemaVersion: 1,
		Subscriptions: []subscription{{Provider: "cursor", Configured: true, Available: true}},
		Tools:         map[string]runtimeTool{"cursor": {}},
	}
	if err := a.prepareCursor(rc); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var model, authInfo, preferences map[string]any
	var network map[string]any
	if json.Unmarshal(got["model"], &model) != nil || json.Unmarshal(got["authInfo"], &authInfo) != nil ||
		json.Unmarshal(got["preferences"], &preferences) != nil || json.Unmarshal(got["network"], &network) != nil ||
		model["modelId"] != "claude-opus-4.8" || model["displayName"] != "Claude Opus 4.8" || authInfo["authId"] != "cursor-user-123" ||
		preferences["privacyMode"] != true || network["futureOption"] != "keep" || network["useHttp1ForAgent"] != true {
		t.Fatalf("derived cli-config=%s", body)
	}

	// 旧版 gate 写入的字符串 model 会让当前 Cursor 判整份配置无效；只移除这
	// 一种旧形态，其余字段继续保留。
	if err := os.WriteFile(configPath, []byte(`{"version":1,"model":"legacy-cursor-model","authInfo":{"authId":"keep-me"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.prepareCursor(rc); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || bytes.Contains(body, []byte(`"model"`)) || !bytes.Contains(body, []byte(`"authId": "keep-me"`)) {
		t.Fatalf("legacy merge err=%v config=%s", err, body)
	}
}

func TestStatusListsCursorAndRedactsKey(t *testing.T) {
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model"}
	srv := newCursorDeviceServer(t, state)
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.status(); err != nil {
		t.Fatal(err)
	}
	out := a.out.(*bytes.Buffer).String()
	if strings.Contains(out, fakeKey) {
		t.Fatalf("status leaked full key: %s", out)
	}
	if !strings.Contains(out, maskKey(fakeKey)) {
		t.Fatalf("status missing masked key: %s", out)
	}
	if !strings.Contains(out, "cursor：已授权，未关联") || strings.Contains(out, "cursor：已授权，额外模型") {
		t.Fatalf("status missing subscription-only cursor line: %s", out)
	}
	if !strings.Contains(out, "opencode：无需订阅") {
		t.Fatalf("status lost opencode line: %s", out)
	}
}

func TestCursorInstallNeverRunsCodexInstaller(t *testing.T) {
	// 设备不提供 /cursor-helper/cli/*（cliFiles 为 nil，恒 404）时，受管安装
	// 必须以自己的失败闭合收场，绝不能落进 Codex 兜底安装器。
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model"}
	srv := newCursorDeviceServer(t, state)
	t.Setenv("PATH", t.TempDir())
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := a.install("cursor", false, false)
	if err == nil || !strings.Contains(err.Error(), "Cursor CLI 官方安装脚本") {
		t.Fatalf("managed cursor install must fail on missing installer script: %v", err)
	}
	if _, ok := a.cfg.Tools["cursor"]; ok {
		t.Fatal("failed install must not link cursor")
	}
	if err := a.install("cursor", true, true); err == nil || !strings.Contains(err.Error(), "Cursor CLI 官方安装脚本") {
		t.Fatalf("cursor update --adopt must fail on missing installer script: %v", err)
	}
	bin := writeCursorFakeTool(t, filepath.Join(t.TempDir(), "invocations.log"))
	t.Setenv("PATH", filepath.Dir(bin))
	if err := a.install("cursor", false, false); err != nil {
		t.Fatalf("install with PATH cursor-agent should link external: %v", err)
	}
	if st := a.cfg.Tools["cursor"]; st.Origin != "external" || st.BinaryPath != bin {
		t.Fatalf("linked state=%+v want external %s", st, bin)
	}
	if state.codexInstallerHits != 0 {
		t.Fatalf("cursor install reached the Codex installer paths %d times", state.codexInstallerHits)
	}
}

func TestInstallerEnvStripsCursorVariables(t *testing.T) {
	cursorVars := []string{
		"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN", "CURSOR_API_ENDPOINT",
		"CURSOR_CONFIG_DIR", "CURSOR_DATA_DIR", "AGENT_CLI_CREDENTIAL_STORE",
	}
	for _, name := range cursorVars {
		t.Setenv(name, "must-not-reach-installer")
	}
	t.Setenv("GATE_TEST_UNRELATED", "kept")
	env := installerEnv()
	for _, name := range cursorVars {
		if got := envValue(env, name); got != "" {
			t.Errorf("%s=%q must be stripped from installer env", name, got)
		}
	}
	if envValue(env, "GATE_TEST_UNRELATED") != "kept" {
		t.Error("unrelated variable must survive installer env")
	}
}

func TestRunDispatchesCursorAndHelpMentionsIt(t *testing.T) {
	var help bytes.Buffer
	printHelp(&help)
	if !strings.Contains(help.String(), "cursor") {
		t.Fatalf("help does not mention cursor: %s", help.String())
	}
	t.Setenv("GATE_CONFIG_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"cursor"}, strings.NewReader(""), &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "尚未关联") {
		t.Fatalf("run cursor dispatch: code=%d stderr=%s", code, errOut.String())
	}
}

// cursorInstallScript 按官方 install.sh 的真实行形态生成脚本正文。
func cursorInstallScript(version string) string {
	return "#!/bin/bash\nset -euo pipefail\n\ndetect_platform() { :; }\nDOWNLOAD_URL=\"https://downloads.cursor.com/lab/" + version +
		"/${OS}/${ARCH}/agent-cli-package.tar.gz\"\ncurl -fsSL \"$DOWNLOAD_URL\"\n"
}

// cursorLauncherScript 仿真整树制品入口 cursor-agent：--version 打裸版本串。
func cursorLauncherScript(version string) string {
	return "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + version + "'; exit 0; fi\nexit 0\n"
}

type cursorTarEntry struct {
	name, link, body string
	mode             int64
	typ              byte // 0 → tar.TypeReg
}

func cursorTarGz(t *testing.T, entries []cursorTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		header := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			header.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type cursorZipEntry struct {
	name, body string
	mode       os.FileMode
}

func cursorZipArchive(t *testing.T, entries []cursorZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		header.SetMode(e.mode)
		f, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cursorFixtureTree 是一份最小整树制品：入口脚本、node、bundle 与树内软链。
func cursorFixtureTree(t *testing.T, version, bundle string) []byte {
	t.Helper()
	return cursorTarGz(t, []cursorTarEntry{
		{name: "dist-package/", typ: tar.TypeDir, mode: 0o755},
		{name: "dist-package/cursor-agent", mode: 0o755, body: cursorLauncherScript(version)},
		{name: "dist-package/node", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
		{name: "dist-package/lib/index.js", mode: 0o644, body: bundle},
		{name: "dist-package/rg", typ: tar.TypeSymlink, link: "node", mode: 0o777},
	})
}

func TestParseCursorInstallerVersionFailClosed(t *testing.T) {
	if got, err := parseCursorInstallerVersion(cursorInstallScript("2026.08.11-e8db854")); err != nil || got != "2026.08.11-e8db854" {
		t.Fatalf("parse=(%q,%v)", got, err)
	}
	repeated := cursorInstallScript("2026.08.11-e8db854") + cursorInstallScript("2026.08.11-e8db854")
	if got, err := parseCursorInstallerVersion(repeated); err != nil || got != "2026.08.11-e8db854" {
		t.Fatalf("repeated identical line=(%q,%v)", got, err)
	}
	for name, script := range map[string]string{
		"missing line":      "#!/bin/bash\ncurl -fsSL https://example.invalid\n",
		"wrong host":        `DOWNLOAD_URL="https://downloads.evil.invalid/lab/2026.08.11-e8db854/${OS}/${ARCH}/agent-cli-package.tar.gz"`,
		"host suffix trick": `DOWNLOAD_URL="https://downloads.cursor.com.evil.invalid/lab/2026.08.11-e8db854/${OS}/${ARCH}/agent-cli-package.tar.gz"`,
		"bad version shape": `DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.8.11-e8db854/${OS}/${ARCH}/agent-cli-package.tar.gz"`,
		"uppercase hex":     `DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.08.11-E8DB854/${OS}/${ARCH}/agent-cli-package.tar.gz"`,
		"overlong hex":      `DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.08.11-e8db854e8db854/${OS}/${ARCH}/agent-cli-package.tar.gz"`,
		"changed asset":     `DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.08.11-e8db854/${OS}/${ARCH}/agent.tar.gz"`,
		"literal vars":      `DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.08.11-e8db854/linux/x64/agent-cli-package.tar.gz"`,
		"conflicting versions": cursorInstallScript("2026.08.11-e8db854") +
			`DOWNLOAD_URL="https://downloads.cursor.com/lab/2026.09.01-0abc123/${OS}/${ARCH}/agent-cli-package.tar.gz"` + "\n",
	} {
		if got, err := parseCursorInstallerVersion(script); err == nil || !strings.Contains(err.Error(), "拒绝猜测性安装") {
			t.Errorf("%s: parse=(%q,%v), want fail closed", name, got, err)
		}
	}
}

func TestCursorCLIAssetPathMatrix(t *testing.T) {
	const version = "2026.08.11-e8db854"
	for _, tc := range []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "lab/" + version + "/linux/x64/agent-cli-package.tar.gz"},
		{"linux", "arm64", "lab/" + version + "/linux/arm64/agent-cli-package.tar.gz"},
		{"darwin", "amd64", "lab/" + version + "/darwin/x64/agent-cli-package.tar.gz"},
		{"darwin", "arm64", "lab/" + version + "/darwin/arm64/agent-cli-package.tar.gz"},
		{"windows", "amd64", "lab/" + version + "/windows/x64/agent-cli-package.zip"},
		{"windows", "arm64", "lab/" + version + "/windows/arm64/agent-cli-package.zip"},
	} {
		got, err := cursorCLIAssetPath(tc.goos, tc.goarch, version)
		if err != nil || got != tc.want {
			t.Errorf("cursorCLIAssetPath(%s/%s)=(%q,%v) want %q", tc.goos, tc.goarch, got, err, tc.want)
		}
	}
	if _, err := cursorCLIAssetPath("freebsd", "amd64", version); err == nil {
		t.Fatal("unsupported OS should fail")
	}
	if _, err := cursorCLIAssetPath("linux", "386", version); err == nil {
		t.Fatal("unsupported architecture should fail")
	}
}

func TestCursorManagedInstallUpdateAdoptAndDisconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	const v1 = "2026.08.11-e8db854"
	const v2 = "2026.09.01-0abc123"
	assetV1, err := cursorCLIAssetPath(runtime.GOOS, runtime.GOARCH, v1)
	if err != nil {
		t.Fatal(err)
	}
	assetV2, err := cursorCLIAssetPath(runtime.GOOS, runtime.GOARCH, v2)
	if err != nil {
		t.Fatal(err)
	}
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model",
		cliFiles: map[string][]byte{
			"install.sh": []byte(cursorInstallScript(v1)),
			assetV1:      cursorFixtureTree(t, v1, "bundle-v1\n"),
		}}
	srv := newCursorDeviceServer(t, state)
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}

	// 首次受管安装：整树落盘、模式保留、原子切换后关联 origin=gate。
	if err := a.install("cursor", false, false); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(root, "tools", "cursor", "app")
	wantBinary := filepath.Join(appDir, "cursor-agent")
	if st := a.cfg.Tools["cursor"]; st.Origin != "gate" || st.BinaryPath != wantBinary || st.DetectedVersion != v1 {
		t.Fatalf("state=%+v want gate %s %s", st, wantBinary, v1)
	}
	for _, executable := range []string{"cursor-agent", "node"} {
		st, err := os.Stat(filepath.Join(appDir, executable))
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode=%v err=%v want 0755", executable, st.Mode(), err)
		}
	}
	if st, err := os.Stat(filepath.Join(appDir, "lib", "index.js")); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("index.js mode=%v err=%v want 0644", st.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(appDir, "rg")); err != nil || target != "node" {
		t.Fatalf("rg symlink=(%q,%v) want node", target, err)
	}
	toolDir := filepath.Join(root, "tools", "cursor")
	entries, err := os.ReadDir(toolDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"app", "derived"}) {
		t.Fatalf("leftovers in tools/cursor: %v", names)
	}

	// 同版本 update：只取脚本比版本，不重新下载归档。
	a.out = &bytes.Buffer{}
	if err := a.install("cursor", false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "已是官方当前版本 "+v1) {
		t.Fatalf("same-version update output=%q", a.out.(*bytes.Buffer).String())
	}
	if state.cliHits[assetV1] != 1 {
		t.Fatalf("same-version update re-downloaded archive: hits=%v", state.cliHits)
	}

	// 官方发布新版本：update 整树替换（不是覆盖合并），旧树文件不残留。
	state.cliFiles["install.sh"] = []byte(cursorInstallScript(v2))
	state.cliFiles[assetV2] = cursorTarGz(t, []cursorTarEntry{
		{name: "dist-package/", typ: tar.TypeDir, mode: 0o755},
		{name: "dist-package/cursor-agent", mode: 0o755, body: cursorLauncherScript(v2)},
		{name: "dist-package/node", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
		{name: "dist-package/lib/index.js", mode: 0o644, body: "bundle-v2\n"},
	})
	a.out = &bytes.Buffer{}
	if err := a.install("cursor", false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "从 "+v1+" 升级到 "+v2) {
		t.Fatalf("upgrade output=%q", a.out.(*bytes.Buffer).String())
	}
	if st := a.cfg.Tools["cursor"]; st.DetectedVersion != v2 || st.Origin != "gate" {
		t.Fatalf("updated state=%+v want %s", st, v2)
	}
	if body, err := os.ReadFile(filepath.Join(appDir, "lib", "index.js")); err != nil || string(body) != "bundle-v2\n" {
		t.Fatalf("bundle after update=(%q,%v)", body, err)
	}
	if _, err := os.Lstat(filepath.Join(appDir, "rg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v1-only file survived whole-tree swap: %v", err)
	}
	entries, err = os.ReadDir(toolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "app.old-") || strings.HasPrefix(entry.Name(), "app.tmp-") || strings.HasPrefix(entry.Name(), ".cursor-archive-") {
			t.Fatalf("swap leftover %s", entry.Name())
		}
	}

	// 外部安装保护：update 拒绝，update --adopt 才收编为受管。
	externalBin := writeCursorFakeTool(t, filepath.Join(t.TempDir(), "invocations.log"))
	a.cfg.Tools["cursor"] = toolState{BinaryPath: externalBin, DetectedVersion: v1, Origin: "external"}
	if err := a.install("cursor", false, true); err == nil || !strings.Contains(err.Error(), "外部渠道管理") {
		t.Fatalf("external update must be refused: %v", err)
	}
	archiveHits := state.cliHits[assetV2]
	if err := a.install("cursor", true, true); err != nil {
		t.Fatalf("update --adopt: %v", err)
	}
	if st := a.cfg.Tools["cursor"]; st.Origin != "gate" || st.BinaryPath != wantBinary || st.DetectedVersion != v2 {
		t.Fatalf("adopted state=%+v", st)
	}
	if state.cliHits[assetV2] != archiveHits {
		t.Fatalf("adopt at current version re-downloaded archive: hits=%v", state.cliHits)
	}

	// disconnect 只撤销接入字段：会话目录与受管 app/ 树保留，之后 install 直接复用不下载。
	if err := a.disconnect("cursor"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.cfg.Tools["cursor"]; ok {
		t.Fatal("disconnect must unlink cursor")
	}
	if _, err := os.Stat(filepath.Join(toolDir, "derived", "data")); err != nil {
		t.Fatalf("disconnect must preserve session directory: %v", err)
	}
	if _, err := os.Stat(wantBinary); err != nil {
		t.Fatalf("disconnect must keep managed app tree: %v", err)
	}
	scriptHits := state.cliHits["install.sh"]
	if err := a.install("cursor", false, false); err != nil {
		t.Fatalf("reinstall after disconnect: %v", err)
	}
	if st := a.cfg.Tools["cursor"]; st.Origin != "gate" || st.BinaryPath != wantBinary {
		t.Fatalf("relinked state=%+v", st)
	}
	if state.cliHits["install.sh"] != scriptHits || state.cliHits[assetV2] != archiveHits {
		t.Fatalf("managed reuse must not touch installer endpoints: hits=%v", state.cliHits)
	}
	if state.codexInstallerHits != 0 {
		t.Fatalf("cursor lifecycle reached Codex installer paths %d times", state.codexInstallerHits)
	}
}

func TestCursorInstallVersionMismatchKeepsExistingTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	const vOld = "2026.07.01-1234abc"
	const vNew = "2026.09.01-0abc123"
	assetNew, err := cursorCLIAssetPath(runtime.GOOS, runtime.GOARCH, vNew)
	if err != nil {
		t.Fatal(err)
	}
	state := &cursorDeviceState{configured: true, available: true, defaultModel: "legacy-cursor-model",
		cliFiles: map[string][]byte{
			"install.sh": []byte(cursorInstallScript(vNew)),
			// 归档自称 vNew，实际入口自检打别的版本——必须整树丢弃。
			assetNew: cursorFixtureTree(t, "2026.08.11-e8db854", "tampered\n"),
		}}
	srv := newCursorDeviceServer(t, state)
	root := t.TempDir()
	appDir := filepath.Join(root, "tools", "cursor", "app")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldLauncher := []byte(cursorLauncherScript(vOld))
	if err := os.WriteFile(filepath.Join(appDir, "cursor-agent"), oldLauncher, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey,
		Tools: map[string]toolState{"cursor": {BinaryPath: filepath.Join(appDir, "cursor-agent"), DetectedVersion: vOld, Origin: "gate"}}},
		hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err = a.install("cursor", false, true)
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("version mismatch must fail install: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(appDir, "cursor-agent")); err != nil || !bytes.Equal(body, oldLauncher) {
		t.Fatalf("old launcher changed: err=%v", err)
	}
	if body, err := os.ReadFile(filepath.Join(appDir, "keep.txt")); err != nil || string(body) != "keep" {
		t.Fatalf("old tree file lost: (%q,%v)", body, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tools", "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "app" {
			t.Fatalf("failed install left %s behind", entry.Name())
		}
	}
	if st := a.cfg.Tools["cursor"]; st.DetectedVersion != vOld || st.Origin != "gate" {
		t.Fatalf("failed install changed linked state: %+v", st)
	}
}

func TestCursorArchiveSizeCapFailsClosed(t *testing.T) {
	const version = "2026.08.11-e8db854"
	asset, err := cursorCLIAssetPath(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/cursor-helper/cli/") {
		case "install.sh":
			_, _ = w.Write([]byte(cursorInstallScript(version)))
		case asset:
			// 声明超过 512MiB 的长度；客户端必须在读取正文前失败闭合。
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(cursorCLIMaxBytes)+1))
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if _, err := a.installCursor(); err == nil || !strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("oversized archive must fail closed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tools", "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files behind: %v", entries)
	}
}

func TestExtractCursorTreeRejectsHostileArchives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixtures run on Unix test hosts")
	}
	rootDir := cursorTarEntry{name: "dist-package/", typ: tar.TypeDir, mode: 0o755}
	for name, tc := range map[string]struct {
		entries []cursorTarEntry
		want    string
	}{
		"dotdot in path": {[]cursorTarEntry{rootDir,
			{name: "dist-package/../evil", mode: 0o644, body: "x"}}, "上跳路径"},
		"top-level dotdot": {[]cursorTarEntry{rootDir,
			{name: "../evil", mode: 0o644, body: "x"}}, "上跳路径"},
		"absolute path": {[]cursorTarEntry{rootDir,
			{name: "/etc/evil", mode: 0o644, body: "x"}}, "绝对路径"},
		"two top-level roots": {[]cursorTarEntry{rootDir,
			{name: "dist-package/cursor-agent", mode: 0o755, body: "x"},
			{name: "other/evil", mode: 0o644, body: "x"}}, "并存"},
		"top-level plain file": {[]cursorTarEntry{
			{name: "cursor-agent", mode: 0o755, body: "x"}}, "顶层不是单一目录"},
		"absolute symlink target": {[]cursorTarEntry{rootDir,
			{name: "dist-package/evil", typ: tar.TypeSymlink, link: "/etc/passwd", mode: 0o777}}, "绝对路径"},
		"escaping symlink target": {[]cursorTarEntry{rootDir,
			{name: "dist-package/evil", typ: tar.TypeSymlink, link: "../../x", mode: 0o777}}, "越出安装目录"},
		"write through symlink": {[]cursorTarEntry{rootDir,
			{name: "dist-package/link", typ: tar.TypeSymlink, link: ".", mode: 0o777},
			{name: "dist-package/link/pwn", mode: 0o644, body: "x"}}, "经符号链接"},
		"unsupported entry type": {[]cursorTarEntry{rootDir,
			{name: "dist-package/fifo", typ: tar.TypeFifo, mode: 0o644}}, "类型不支持"},
		"empty archive": {nil, "归档为空"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			staging := filepath.Join(parent, "staging")
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "pkg.tar.gz")
			if err := os.WriteFile(archive, cursorTarGz(t, tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			err := extractCursorTree(archive, false, staging)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("extract err=%v want %q", err, tc.want)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "staging" {
				t.Fatalf("hostile entry escaped the staging dir: %v", entries)
			}
		})
	}
}

func TestExtractCursorTreeEnforcesUnpackBudget(t *testing.T) {
	old := cursorCLIMaxTreeBytes
	cursorCLIMaxTreeBytes = 8
	defer func() { cursorCLIMaxTreeBytes = old }()
	archive := filepath.Join(t.TempDir(), "pkg.tar.gz")
	body := cursorTarGz(t, []cursorTarEntry{
		{name: "dist-package/", typ: tar.TypeDir, mode: 0o755},
		{name: "dist-package/big", mode: 0o644, body: strings.Repeat("a", 100)},
	})
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractCursorTree(archive, false, t.TempDir()); err == nil || !strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("unpack budget must fail closed: %v", err)
	}
}

func TestExtractCursorZipStripsSingleRootDir(t *testing.T) {
	staging := t.TempDir()
	archive := filepath.Join(t.TempDir(), "pkg.zip")
	body := cursorZipArchive(t, []cursorZipEntry{
		{name: "dist-package/", mode: os.ModeDir | 0o755},
		{name: "dist-package/cursor-agent", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
		{name: "dist-package/lib/", mode: os.ModeDir | 0o755},
		{name: "dist-package/lib/data.txt", mode: 0o644, body: "data"},
	})
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractCursorTree(archive, true, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "dist-package")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root dir was not stripped: %v", err)
	}
	st, err := os.Stat(filepath.Join(staging, "cursor-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o755 {
		t.Fatalf("cursor-agent mode=%v want 0755", st.Mode())
	}
	if got, err := os.ReadFile(filepath.Join(staging, "lib", "data.txt")); err != nil || string(got) != "data" {
		t.Fatalf("lib/data.txt=(%q,%v)", got, err)
	}
	for name, entries := range map[string][]cursorZipEntry{
		"上跳路径":  {{name: "dist-package/", mode: os.ModeDir | 0o755}, {name: "dist-package/../evil", mode: 0o644, body: "x"}},
		"并存":    {{name: "dist-package/a", mode: 0o644, body: "x"}, {name: "other/b", mode: 0o644, body: "x"}},
		"类型不支持": {{name: "dist-package/", mode: os.ModeDir | 0o755}, {name: "dist-package/link", mode: os.ModeSymlink | 0o777, body: "node"}},
	} {
		archive := filepath.Join(t.TempDir(), "bad.zip")
		if err := os.WriteFile(archive, cursorZipArchive(t, entries), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extractCursorTree(archive, true, t.TempDir()); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("zip %s: err=%v", name, err)
		}
	}
}

func TestOpenCodeConnectLaunchIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/gate-helper/v1/config" {
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[],"tools":{"opencode":{"default_model":"chat-test","models":[{"name":"chat-test","source":"catalog"}]}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	bin := writeFakeTool(t, "opencode", `
if [ "$1" = "models" ]; then
  test -f "$OPENCODE_CONFIG" || exit 9
  exit 0
fi
printf 'config=%s config_dir=%s key=%s args=%s\n' "$OPENCODE_CONFIG" "$OPENCODE_CONFIG_DIR" "$LLMGATE_API_KEY" "$*"`)
	a := &app{
		root: root,
		cfg:  config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}},
		hc:   newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{},
	}
	if err := a.connect("opencode", bin, "external"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "tools", "opencode", "derived", "opencode.json")
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(fakeKey)) {
		t.Fatalf("OpenCode config leaked API key: %s", body)
	}
	var got struct {
		Model            string   `json:"model"`
		SmallModel       string   `json:"small_model"`
		AutoUpdate       bool     `json:"autoupdate"`
		EnabledProviders []string `json:"enabled_providers"`
		Provider         map[string]struct {
			Name    string            `json:"name"`
			NPM     string            `json:"npm"`
			Options map[string]string `json:"options"`
			Models  map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	providerID := a.openCodeProviderID()
	provider := got.Provider[providerID]
	if got.Model != providerID+"/chat-test" || got.SmallModel != got.Model || got.AutoUpdate ||
		!slices.Equal(got.EnabledProviders, []string{providerID}) || provider.NPM != "@ai-sdk/openai-compatible" || provider.Name != "LLM Gate" ||
		provider.Options["baseURL"] != srv.URL+"/v1" || provider.Options["apiKey"] != "{env:LLMGATE_API_KEY}" ||
		len(provider.Models) != 1 || provider.Models["chat-test"].Name != "LLM Gate · chat-test" {
		t.Fatalf("OpenCode config=%+v", got)
	}
	env := a.toolEnv("opencode")
	wantXDG := filepath.Join(root, "tools", "opencode", "derived", "xdg")
	for key, want := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(wantXDG, "config"),
		"XDG_DATA_HOME":   filepath.Join(wantXDG, "data"),
		"XDG_CACHE_HOME":  filepath.Join(wantXDG, "cache"),
		"XDG_STATE_HOME":  filepath.Join(wantXDG, "state"),
	} {
		if got := envValue(env, key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
	if content := envValue(env, "OPENCODE_CONFIG_CONTENT"); content != string(body) || strings.Contains(content, fakeKey) {
		t.Fatalf("OpenCode inline config is not the generated key-free config")
	}
	a.out = &bytes.Buffer{}
	if err := a.launch("opencode", []string{"run", "hello"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	output := a.out.(*bytes.Buffer).String()
	if !strings.Contains(output, "config="+configPath) ||
		!strings.Contains(output, "config_dir="+filepath.Dir(configPath)) ||
		!strings.Contains(output, "key="+fakeKey) || !strings.Contains(output, "args=run hello") {
		t.Fatalf("OpenCode launch did not use isolated config: %s", output)
	}
	if err := a.tool("opencode", []string{"upgrade"}, strings.NewReader("")); err == nil ||
		!strings.Contains(err.Error(), "gate opencode update") {
		t.Fatalf("native OpenCode upgrade should be redirected to gate lifecycle: %v", err)
	}
}

func TestOpenCodeRealCLIConfig(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("GATE_TEST_OPENCODE_BIN"))
	if bin == "" {
		t.Skip("set GATE_TEST_OPENCODE_BIN to an official OpenCode binary")
	}
	var inferenceCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/gate-helper/v1/config":
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[],"tools":{"opencode":{"default_model":"chat-test","models":[{"name":"chat-test","source":"catalog"}]}}}`))
		case "/v1/chat/completions":
			inferenceCalls.Add(1)
			var request struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "chat-test" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if !request.Stream {
				_, _ = w.Write([]byte(`{"id":"chatcmpl-real","object":"chat.completion","created":1,"model":"chat-test","choices":[{"index":0,"message":{"role":"assistant","content":"GATE_OPENCODE_REAL_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"GATE_OPENCODE_REAL_OK\"},\"finish_reason\":null}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := &app{
		root: t.TempDir(),
		cfg:  config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}},
		hc:   newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{},
	}
	if err := a.connect("opencode", bin, "external"); err != nil {
		t.Fatal(err)
	}
	// A normal OpenCode project and global profile may define unrelated
	// providers. The gate launch keeps project functionality available but its
	// inline enabled-provider choice and isolated XDG roots must keep those
	// providers out of this profile.
	poisonConfig := []byte(`{
  "enabled_providers": ["poison"],
  "provider": {
    "poison": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Poison",
      "options": {"baseURL": "https://poison.invalid/v1", "apiKey": "poison"},
      "models": {"poison-model": {"name": "Poison Model"}}
    }
  }
}`)
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "opencode.json"), poisonConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	originalXDG := t.TempDir()
	if err := os.MkdirAll(filepath.Join(originalXDG, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originalXDG, "opencode", "opencode.json"), poisonConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", originalXDG)
	t.Chdir(project)
	a.out = &bytes.Buffer{}
	if err := a.launch("opencode", []string{"models"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if output := a.out.(*bytes.Buffer).String(); !strings.Contains(output, a.openCodeProviderID()+"/chat-test") ||
		strings.Contains(output, "poison") {
		t.Fatalf("official OpenCode did not expose generated model: %q", output)
	}
	a.out = &bytes.Buffer{}
	if err := a.launch("opencode", []string{"run", "Return the fixed test marker."}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if output := a.out.(*bytes.Buffer).String(); inferenceCalls.Load() == 0 || !strings.Contains(output, "GATE_OPENCODE_REAL_OK") {
		t.Fatalf("official OpenCode did not complete through OpenAI-compatible chat: calls=%d output=%q", inferenceCalls.Load(), output)
	}
}

func TestOpenCodeSelfCheckRedactsKey(t *testing.T) {
	bin := writeFakeTool(t, "opencode", `printf 'failed with %s\n' "$LLMGATE_API_KEY" >&2; exit 1`)
	a := &app{
		root: t.TempDir(),
		cfg:  config{SchemaVersion: 1, BaseURL: "https://box.invalid", APIKey: fakeKey},
		hc:   newHTTPClient(),
	}
	err := a.prepareOpenCode(bin, runtimeTool{
		DefaultModel: "chat-test",
		Models:       []runtimeModel{{Name: "chat-test", Source: "catalog"}},
	})
	if err == nil || strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("OpenCode self-check error was not redacted: %v", err)
	}
}

func TestClaudeCatalogModelsUseDiscoverableClientIDs(t *testing.T) {
	const catalogName = "deepseek-v4-flash"
	clientID := claudeClientModelID(runtimeModel{Name: catalogName, Source: "catalog"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/agents/claude/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q,"type":"model","display_name":%q}]}`, clientID, catalogName)
		case "/agents/claude/api/hello":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey}, hc: srv.Client(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	tool := runtimeTool{DefaultModel: catalogName, Models: []runtimeModel{{Name: catalogName, Source: "catalog"}}}
	if err := a.prepareClaude(tool); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Model           string            `json:"model"`
		AvailableModels []string          `json:"availableModels"`
		Env             map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Model != clientID || len(settings.AvailableModels) != 1 || settings.AvailableModels[0] != clientID ||
		settings.Env["ANTHROPIC_DEFAULT_MODEL"] != clientID || settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "" ||
		settings.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "" || settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "" {
		t.Fatalf("Claude settings=%+v", settings)
	}
}

func TestClaudeDefaultAndFamiliesUseDistinctGatewayModels(t *testing.T) {
	models := []runtimeModel{
		{Name: "claude-opus-5", Source: "subscription"},
		{Name: "claude-fable-5", Source: "subscription"},
		{Name: "claude-sonnet-5", Source: "subscription"},
		{Name: "claude-haiku-4-5", Source: "subscription"},
		{Name: "claude-haiku-4-5-20251001", Source: "subscription"},
		{Name: "deepseek-v4-flash", Source: "catalog"},
	}
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = claudeClientModelID(model)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/agents/claude/v1/models":
			data := make([]map[string]string, len(ids))
			for i, id := range ids {
				data[i] = map[string]string{"id": id, "display_name": "Display " + models[i].Name}
			}
			data[0]["display_name"] = "LLM Gate · Opus"
			data[1]["display_name"] = "SOC AGENT · Fable"
			data[2]["display_name"] = ""
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/agents/claude/api/hello":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey}, hc: srv.Client()}
	if err := a.prepareClaude(runtimeTool{DefaultModel: models[1].Name, Models: models}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Model           string            `json:"model"`
		AvailableModels []string          `json:"availableModels"`
		Env             map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Model != ids[1] ||
		!slices.Equal(settings.AvailableModels, []string{ids[1], ids[0], ids[2], ids[4], ids[5]}) ||
		settings.Env["ANTHROPIC_DEFAULT_MODEL"] != ids[1] ||
		settings.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"] != ids[1] ||
		settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != ids[0] ||
		settings.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != ids[2] ||
		settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != ids[4] ||
		settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] != "LLM Gate · Opus" ||
		settings.Env["ANTHROPIC_DEFAULT_FABLE_MODEL_NAME"] != "LLM Gate · Fable" ||
		settings.Env["ANTHROPIC_DEFAULT_SONNET_MODEL_NAME"] != "LLM Gate · "+models[2].Name ||
		settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] != "LLM Gate · Display "+models[4].Name ||
		settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION"] != "From LLM Gate" {
		t.Fatalf("Claude settings=%+v", settings)
	}
	if _, ok := settings.Env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatalf("user-level settings must not carry credentials: %+v", settings.Env)
	}
	cb, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "cli-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cli struct {
		Model           string            `json:"model"`
		AvailableModels []string          `json:"availableModels"`
		Env             map[string]string `json:"env"`
	}
	if err := json.Unmarshal(cb, &cli); err != nil {
		t.Fatal(err)
	}
	if cli.Model != ids[1] || !slices.Equal(cli.AvailableModels, settings.AvailableModels) ||
		cli.Env["ANTHROPIC_MODEL"] != ids[1] || cli.Env["ANTHROPIC_BASE_URL"] != srv.URL+"/agents/claude" ||
		cli.Env["ANTHROPIC_AUTH_TOKEN"] != fakeKey ||
		cli.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != ids[4] ||
		cli.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] != settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] ||
		cli.Env["ANTHROPIC_DEFAULT_MODEL"] != ids[1] {
		t.Fatalf("Claude cli-settings=%+v", cli)
	}
}

func TestClaudeSubscriptionAliasCollapse(t *testing.T) {
	models := []runtimeModel{
		{Name: "claude-fable-5", Source: "subscription"},
		{Name: "claude-haiku-4-5", Source: "subscription"},
		{Name: "claude-haiku-4-5-20251001", Source: "subscription"},
		{Name: "deepseek-v4-flash", Source: "catalog"},
	}
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = claudeClientModelID(model)
	}
	catalogID := ids[3]
	if got := collapseClaudeSubscriptionAliases(ids, models, ids[0]); !slices.Equal(got, []string{
		"claude-fable-5", "claude-haiku-4-5-20251001", catalogID,
	}) {
		t.Fatalf("dated row should replace a non-default alias: %v", got)
	}
	if got := collapseClaudeSubscriptionAliases(ids, models, ids[1]); !slices.Equal(got, []string{
		"claude-fable-5", "claude-haiku-4-5", catalogID,
	}) {
		t.Fatalf("default alias should replace its dated row: %v", got)
	}

	multiple := append([]runtimeModel(nil), models...)
	multiple = append(multiple, runtimeModel{Name: "claude-haiku-4-5-20251101", Source: "subscription"})
	multipleIDs := make([]string, len(multiple))
	for i, model := range multiple {
		multipleIDs[i] = claudeClientModelID(model)
	}
	if got := collapseClaudeSubscriptionAliases(multipleIDs, multiple, multipleIDs[0]); !slices.Equal(got, multipleIDs) {
		t.Fatalf("multiple dated versions must remain distinct: %v", got)
	}
}

func writeFakeTool(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + name + " 1.0.0'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudePlatformMatrix(t *testing.T) {
	tests := []struct {
		goos, goarch     string
		musl, translated bool
		want             string
	}{
		{"darwin", "amd64", false, false, "darwin-x64"},
		{"darwin", "amd64", false, true, "darwin-arm64"},
		{"darwin", "arm64", false, false, "darwin-arm64"},
		{"linux", "amd64", false, false, "linux-x64"},
		{"linux", "arm64", false, false, "linux-arm64"},
		{"linux", "amd64", true, false, "linux-x64-musl"},
		{"windows", "amd64", false, false, "win32-x64"},
		{"windows", "arm64", false, false, "win32-arm64"},
	}
	for _, tc := range tests {
		got, err := claudePlatform(tc.goos, tc.goarch, tc.musl, tc.translated)
		if err != nil || got != tc.want {
			t.Errorf("claudePlatform(%s/%s, musl=%v, translated=%v) = %q, %v; want %q",
				tc.goos, tc.goarch, tc.musl, tc.translated, got, err, tc.want)
		}
	}
	if _, err := claudePlatform("freebsd", "amd64", false, false); err == nil {
		t.Fatal("unsupported OS should fail")
	}
	if _, err := claudePlatform("linux", "386", false, false); err == nil {
		t.Fatal("unsupported architecture should fail")
	}
}

func TestOpenCodeAssetMatrix(t *testing.T) {
	tests := []struct {
		goos, goarch     string
		musl, translated bool
		want             string
	}{
		{"darwin", "amd64", false, false, "opencode-darwin-x64-baseline.zip"},
		{"darwin", "amd64", false, true, "opencode-darwin-arm64.zip"},
		{"darwin", "arm64", false, false, "opencode-darwin-arm64.zip"},
		{"linux", "amd64", false, false, "opencode-linux-x64-baseline.tar.gz"},
		{"linux", "amd64", true, false, "opencode-linux-x64-baseline-musl.tar.gz"},
		{"linux", "arm64", false, false, "opencode-linux-arm64.tar.gz"},
		{"linux", "arm64", true, false, "opencode-linux-arm64-musl.tar.gz"},
		{"windows", "amd64", false, false, "opencode-windows-x64-baseline.zip"},
		{"windows", "arm64", false, false, "opencode-windows-arm64.zip"},
	}
	for _, tc := range tests {
		got, err := openCodeAsset(tc.goos, tc.goarch, tc.musl, tc.translated)
		if err != nil || got != tc.want {
			t.Errorf("openCodeAsset(%s/%s, musl=%v, translated=%v)=%q,%v want %q",
				tc.goos, tc.goarch, tc.musl, tc.translated, got, err, tc.want)
		}
	}
	if _, err := openCodeAsset("freebsd", "amd64", false, false); err == nil {
		t.Fatal("unsupported OS should fail")
	}
	if _, err := openCodeAsset("linux", "386", false, false); err == nil {
		t.Fatal("unsupported architecture should fail")
	}
}

func openCodeTar(t *testing.T, binary []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "opencode", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestOpenCodeInstallDownloadsVerifiedOfficialArchiveThroughDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	assetName, err := currentOpenCodeAsset()
	if err != nil {
		t.Fatal(err)
	}
	fakeBinary := []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode 9.9.9'; exit 0; fi
if [ "$1" = "models" ]; then exit 0; fi
exit 0
`)
	archive := openCodeTar(t, fakeBinary)
	digest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(archive))
	metadata, err := json.Marshal(openCodeRelease{
		TagName: "v9.9.9",
		Assets:  []openCodeReleaseAsset{{Name: assetName, Size: int64(len(archive)), Digest: digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var helperRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, openCodeCLIBasePath+"/") {
			helperRequests++
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public OpenCode artifact request carried Authorization")
			}
			switch strings.TrimPrefix(r.URL.Path, openCodeCLIBasePath+"/") {
			case "latest":
				_, _ = w.Write(metadata)
			case "releases/9.9.9/" + assetName:
				_, _ = w.Write(archive)
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/gate-helper/v1/config" {
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[],"tools":{"opencode":{"default_model":"chat-test","models":[{"name":"chat-test","source":"catalog"}]}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("PATH", t.TempDir())
	t.Setenv("LLMGATE_API_KEY", "must-not-reach-downloaded-program")
	t.Setenv("OPENCODE_CONFIG", "/must/not/reach/downloaded/program")
	root := t.TempDir()
	a := &app{
		root: root,
		cfg:  config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}},
		hc:   newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{},
	}
	if err := a.install("opencode", false, false); err != nil {
		t.Fatal(err)
	}
	state := a.cfg.Tools["opencode"]
	wantPath := filepath.Join(root, "tools", "opencode", "bin", "opencode")
	if state.Origin != "gate" || state.BinaryPath != wantPath || state.DetectedVersion != "opencode 9.9.9" {
		t.Fatalf("state=%+v want managed path %q", state, wantPath)
	}
	if helperRequests != 2 {
		t.Fatalf("helper requests=%d want latest + archive", helperRequests)
	}
	installed, err := os.ReadFile(wantPath)
	if err != nil || !bytes.Equal(installed, fakeBinary) {
		t.Fatalf("installed binary mismatch: err=%v", err)
	}
}

func TestExtractOpenCodeWindowsZip(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("opencode.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("windows-binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencode.zip")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var extracted bytes.Buffer
	if err := extractOpenCodeBinary(path, "opencode-windows-arm64.zip", &extracted); err != nil {
		t.Fatal(err)
	}
	if extracted.String() != "windows-binary" {
		t.Fatalf("extracted=%q", extracted.String())
	}
}

func TestClaudeInstallDownloadsVerifiedOfficialBinaryThroughDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	platform, err := currentClaudePlatform()
	if err != nil {
		t.Fatal(err)
	}
	asset := "claude"
	envMarker := filepath.Join(t.TempDir(), "version-env")
	fakeBinary := []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  if [ -n "${LLMGATE_API_KEY:-}" ] || [ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] || [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then
    echo dirty >> %q
  else
    echo clean >> %q
  fi
  echo 'claude 9.9.9'
  exit 0
fi
exit 0
`, envMarker, envMarker))
	checksum := fmt.Sprintf("%x", sha256.Sum256(fakeBinary))
	manifest := fmt.Sprintf(`{"version":"9.9.9","platforms":{%q:{"binary":%q,"checksum":%q,"size":%d}}}`,
		platform, asset, checksum, len(fakeBinary))
	var helperRequests int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, claudeCLIBasePath+"/") {
			helperRequests++
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public Claude artifact request carried Authorization")
			}
			switch strings.TrimPrefix(r.URL.Path, claudeCLIBasePath+"/") {
			case "latest":
				_, _ = w.Write([]byte("9.9.9\n"))
			case "9.9.9/manifest.json":
				_, _ = w.Write([]byte(manifest))
			case "9.9.9/" + platform + "/" + asset:
				_, _ = w.Write(fakeBinary)
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/gate-helper/v1/config":
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"claude","configured":true,"available":true,"default_model":"claude-test"}],"tools":{"codex":{"default_model":"","models":[]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"claude-test","models":[{"name":"claude-test","source":"subscription"}]}}}`))
		case "/agents/claude/v1/models":
			if r.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Errorf("Claude model discovery Anthropic-Version = %q", r.Header.Get("Anthropic-Version"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","type":"model"}]}`))
		case "/agents/claude/api/hello":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LLMGATE_API_KEY", "must-not-reach-downloaded-program")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "must-not-reach-downloaded-program")
	t.Setenv("CLAUDE_CONFIG_DIR", "must-not-reach-downloaded-program")
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	a := &app{
		root: root,
		cfg:  config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}},
		hc:   srv.Client(), out: &bytes.Buffer{}, err: &bytes.Buffer{},
	}
	if err := a.install("claude", false, false); err != nil {
		t.Fatal(err)
	}
	state := a.cfg.Tools["claude"]
	wantPath := filepath.Join(root, "tools", "claude", "bin", "claude")
	if state.Origin != "gate" || state.BinaryPath != wantPath || state.DetectedVersion != "claude 9.9.9" {
		t.Fatalf("state = %+v, want managed path %q", state, wantPath)
	}
	if marker, err := os.ReadFile(envMarker); err != nil || string(marker) != "clean\ndirty\n" {
		t.Fatalf("version probe environments = %q, err=%v", marker, err)
	}
	if helperRequests != 3 {
		t.Fatalf("helper requests = %d, want latest + manifest + binary", helperRequests)
	}
	installed, err := os.ReadFile(wantPath)
	if err != nil || !bytes.Equal(installed, fakeBinary) {
		t.Fatalf("installed binary mismatch: err=%v", err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "settings.json")); err != nil || bytes.Contains(body, []byte(fakeKey)) {
		t.Fatalf("derived settings leaked key or missing: err=%v body=%q", err, body)
	}
	if body, err := os.ReadFile(filepath.Join(root, "tools", "claude", "derived", "cli-settings.json")); err != nil || !bytes.Contains(body, []byte(fakeKey)) {
		t.Fatalf("derived cli-settings missing pinned credential: err=%v", err)
	}
}

func TestClaudeDownloadChecksumFailureKeepsExistingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture runs on Unix test hosts")
	}
	const body = "#!/bin/sh\necho should-not-install\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	target := filepath.Join(root, "tools", "claude", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	old := []byte("#!/bin/sh\necho old\n")
	if err := os.WriteFile(target, old, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{root: root, cfg: config{BaseURL: srv.URL}, hc: newHTTPClient()}
	release := claudeReleasePlatform{Binary: "claude", Checksum: strings.Repeat("0", 64), Size: int64(len(body))}
	if _, err := a.downloadClaudeBinary("9.9.9", "linux-x64", release); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("checksum mismatch = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("existing binary changed: err=%v body=%q", err, got)
	}
}

func TestMaskKey(t *testing.T) {
	got := maskKey("sk_abcdefghijklmno")
	if strings.Contains(got, "abcdefghijklmno") || !strings.Contains(got, "…") {
		t.Fatalf("maskKey=%q", got)
	}
}

const staleCodexScript = `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.20.0'; exit 0; fi
echo "error: unrecognized subcommand 'debug'" >&2
exit 2
`

const freshCodexScript = `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 1.5.0'; exit 0; fi
if [ "$1 $2 $3" = "debug models --bundled" ]; then echo '{"models":[{"slug":"gpt-test","display_name":"GPT Test"}]}'; exit 0; fi
if [ "$1 $2" = "debug models" ]; then exit 0; fi
exit 0
`

// deviceCodexScript 是盒子 release 归档里 bin/codex 的替身：--version 报 9.9.9，
// debug models 两种调用都认。
const deviceCodexScript = `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 9.9.9'; exit 0; fi
if [ "$1 $2 $3" = "debug models --bundled" ]; then echo '{"models":[{"slug":"gpt-test","display_name":"GPT Test"}]}'; exit 0; fi
if [ "$1 $2" = "debug models" ]; then exit 0; fi
exit 0
`

// codexDeviceState 记录 /codex-helper/cli/* 公开面的命中，供安装测试断言
// 「无 Authorization、只取一次归档」。
type codexDeviceState struct {
	hits map[string]int
}

func newCodexDeviceServer(t *testing.T) *httptest.Server {
	return newCodexDeviceServerWithState(t, &codexDeviceState{})
}

func newCodexDeviceServerWithState(t *testing.T, state *codexDeviceState) *httptest.Server {
	t.Helper()
	target, err := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	asset := codexPackageAsset(target)
	archive := codexPackageTarGz(t, map[string]string{"bin/codex": deviceCodexScript, "codex-package.json": "{}\n"})
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	release := fmt.Sprintf(`{"tag_name":"rust-v9.9.9","assets":[{"name":%q,"size":%d,"digest":"sha256:%s"},{"name":%q,"size":100}]}`,
		asset, len(archive), digest, codexChecksumAsset)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/codex-helper/cli/") {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public codex CLI request carried Authorization")
			}
			if state.hits == nil {
				state.hits = map[string]int{}
			}
			rel := strings.TrimPrefix(r.URL.Path, "/codex-helper/cli/")
			state.hits[rel]++
			switch rel {
			case "channels/latest", "releases/9.9.9/release.json":
				_, _ = w.Write([]byte(release))
			case "releases/9.9.9/" + codexChecksumAsset:
				fmt.Fprintf(w, "%s  %s\n", digest, asset)
			case "releases/9.9.9/" + asset:
				http.ServeContent(w, r, asset, time.Time{}, bytes.NewReader(archive))
			default:
				http.NotFound(w, r)
			}
			return
		}
		switch r.URL.Path {
		case "/gate-helper/v1/config":
			if r.Header.Get("Authorization") != "Bearer "+fakeKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"schema_version":1,"revision":1,"subscriptions":[{"provider":"codex","configured":true,"available":true,"default_model":"gpt-test"}],"tools":{"codex":{"default_model":"gpt-test","models":[{"name":"gpt-test","source":"subscription"}]},"grok":{"default_model":"","models":[]},"claude":{"default_model":"","models":[]}}}`))
		case "/agents/codex/v1/model-catalog":
			if r.Header.Get("Authorization") != "Bearer "+fakeKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// codexPackageTarGz 按官方 codex-package 布局（根下直接是 bin/ 等，无顶层目录）
// 打一个 tar.gz。
func codexPackageTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		mode := int64(0o644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookPathAllDedupesSymlinkedDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	real := writeScript(t, dirA, "codex", "#!/bin/sh\nexit 0\n")
	if err := os.Symlink(real, filepath.Join(dirB, "codex")); err != nil {
		t.Fatal(err)
	}
	other := writeScript(t, dirC, "codex", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", strings.Join([]string{dirA, dirB, dirC}, string(os.PathListSeparator)))
	if got := lookPathAll("codex"); !slices.Equal(got, []string{real, other}) {
		t.Fatalf("lookPathAll=%v want [%s %s]", got, real, other)
	}
	if alts := pathAlternatives("codex", filepath.Join(dirB, "codex")); !slices.Equal(alts, []string{other}) {
		t.Fatalf("pathAlternatives=%v want [%s]", alts, other)
	}
}

func TestCodexConnectSkipsStaleDuplicateInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	staleBin := writeScript(t, t.TempDir(), "codex", staleCodexScript)
	freshBin := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	t.Setenv("PATH", filepath.Dir(staleBin)+string(os.PathListSeparator)+filepath.Dir(freshBin))
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.connect("codex", "", "external"); err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	if got := a.cfg.Tools["codex"]; got.BinaryPath != freshBin || got.Origin != "external" {
		t.Fatalf("linked=%+v want fresh binary %s", got, freshBin)
	}
	stderr := a.err.(*bytes.Buffer).String()
	for _, want := range []string{"跳过不可用安装", staleBin, "codex-cli 0.20.0", "unrecognized subcommand", "PATH 中另有"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestCodexConnectReportsEveryUnusablePathInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	first := writeScript(t, t.TempDir(), "codex", staleCodexScript)
	second := writeScript(t, t.TempDir(), "codex", strings.ReplaceAll(staleCodexScript, "0.20.0", "0.21.0"))
	t.Setenv("PATH", filepath.Dir(first)+string(os.PathListSeparator)+filepath.Dir(second))
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := a.connect("codex", "", "external")
	if err == nil {
		t.Fatal("connect with only unusable installs must fail")
	}
	for _, want := range []string{first, second, "0.20.0", "0.21.0", "都不可用", "gate codex install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if !isCLIUnusable(err) {
		t.Fatalf("aggregate error must stay CLI-unusable for install fallback: %v", err)
	}
	if _, ok := a.cfg.Tools["codex"]; ok {
		t.Fatal("an unusable codex must not be linked")
	}
}

func TestCodexInstallFallsBackToDeviceWhenPathCodexIsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	staleBin := writeScript(t, t.TempDir(), "codex", staleCodexScript)
	t.Setenv("PATH", filepath.Dir(staleBin))
	root := t.TempDir()
	a := &app{root: root, cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey, Tools: map[string]toolState{}}, hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := a.install("codex", false, false); err != nil {
		t.Fatalf("install: %v\nstderr: %s", err, a.err.(*bytes.Buffer).String())
	}
	st := a.cfg.Tools["codex"]
	target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
	want := filepath.Join(root, "tools", "codex", "packages", "standalone", "releases", "9.9.9-"+target, "bin", "codex")
	if st.Origin != "gate" || st.BinaryPath != want || !strings.Contains(st.DetectedVersion, "9.9.9") {
		t.Fatalf("state=%+v want managed install at %s", st, want)
	}
	stderr := a.err.(*bytes.Buffer).String()
	if !strings.Contains(stderr, staleBin) || !strings.Contains(stderr, "改为经设备安装受管 codex") {
		t.Fatalf("fallback narration missing:\n%s", stderr)
	}
}

func TestLaunchReportsPathAlternativesWhenLinkedCodexIsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	srv := newCodexDeviceServer(t)
	staleBin := writeScript(t, t.TempDir(), "codex", staleCodexScript)
	otherBin := writeScript(t, t.TempDir(), "codex", freshCodexScript)
	t.Setenv("PATH", filepath.Dir(otherBin))
	a := &app{root: t.TempDir(), cfg: config{SchemaVersion: 1, BaseURL: srv.URL, APIKey: fakeKey,
		Tools: map[string]toolState{"codex": {BinaryPath: staleBin, DetectedVersion: "codex-cli 0.20.0", Origin: "external"}}},
		hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := a.launch("codex", nil, strings.NewReader(""))
	if err == nil {
		t.Fatal("unusable linked codex must fail launch")
	}
	for _, want := range []string{staleBin, "无法导出 bundled 模型目录", "unrecognized subcommand", otherBin, "gate codex install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}
