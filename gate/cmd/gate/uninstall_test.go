package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const uninstallFixtureKey = "sk_uninstall_fixture_only"

type noUninstallNetwork struct{ t *testing.T }

func (n noUninstallNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Error("uninstall attempted network access")
	return nil, errors.New("offline")
}

func uninstallApp(t *testing.T) *app {
	t.Helper()
	a := &app{root: t.TempDir(), cfg: emptyConfig(), out: &bytes.Buffer{}, err: &bytes.Buffer{}, hc: &http.Client{Transport: noUninstallNetwork{t}}}
	a.cfg.BaseURL = "https://offline.invalid"
	a.cfg.APIKey = uninstallFixtureKey
	a.selfPath = filepath.Join(t.TempDir(), toolExecutableName())
	uninstallWrite(t, a.selfPath, "gate fixture")
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	return a
}
func toolExecutableName() string {
	if runtime.GOOS == "windows" {
		return "gate.exe"
	}
	return "gate"
}
func uninstallWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func uninstallRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func uninstallMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing %s: %v", path, err)
	}
}

func ownedFixture(t *testing.T, a *app, name string) string {
	t.Helper()
	entry := a.managedBinaryPath(name)
	if name == "codex" {
		target, _ := codexVendorTarget(runtime.GOOS, runtime.GOARCH)
		entry = filepath.Join(codexStandaloneDir(a.root), "releases", "1.2.3-"+target, "bin", toolBinaryName(name))
	}
	uninstallWrite(t, entry, "official fixture "+name)
	a.cfg.Tools[name] = toolState{BinaryPath: entry, Origin: "gate"}
	if err := a.recordProgram(name, entry); err != nil {
		t.Fatal(err)
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	return entry
}

func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		v := st.Mode().String() + st.ModTime().String()
		if !d.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			v += digest(b)
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUninstallOwnedToolsPreserveUserFiles(t *testing.T) {
	for _, name := range toolNames {
		t.Run(name, func(t *testing.T) {
			a := uninstallApp(t)
			entry := ownedFixture(t, a, name)
			unknown := filepath.Join(filepath.Dir(entry), "my-notes.txt")
			uninstallWrite(t, unknown, "user notes")
			session := filepath.Join(a.derivedDir(name), purgeNames(name)[0], "session.fixture")
			uninstallWrite(t, session, "private fixture session")
			before := treeFingerprint(t, a.root)
			if err := a.uninstall(name, []string{"--dry-run"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
				t.Fatal("dry run wrote files")
			}
			if err := a.uninstall(name, nil, strings.NewReader("y\n")); exitCode(err) != 2 {
				t.Fatalf("nonterminal must require --yes: %v", err)
			}
			if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
				t.Fatal("unconfirmed uninstall wrote files")
			}
			if err := a.disconnect(name); err != nil {
				t.Fatal(err)
			}
			if err := a.uninstall(name, []string{"--yes"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			uninstallMissing(t, entry)
			if uninstallRead(t, unknown) != "user notes" || uninstallRead(t, session) != "private fixture session" {
				t.Fatal("default uninstall lost user state")
			}
			before = treeFingerprint(t, a.root)
			if err := a.uninstall(name, []string{"--yes", "--purge"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
				t.Fatal("repeated uninstall must be a no-op")
			}
		})
	}
}

func TestUninstallPurgeOnlySelectedManagedState(t *testing.T) {
	for _, name := range toolNames {
		t.Run(name, func(t *testing.T) {
			a := uninstallApp(t)
			entry := ownedFixture(t, a, name)
			session := filepath.Join(a.derivedDir(name), purgeNames(name)[0], "session.fixture")
			uninstallWrite(t, session, "private session fixture")
			unknown := filepath.Join(a.derivedDir(name), "my-notes.txt")
			uninstallWrite(t, unknown, "user")
			external := filepath.Join(t.TempDir(), toolBinaryName(name))
			uninstallWrite(t, external, "external")
			a.cfg.Tools[name] = toolState{BinaryPath: external, Origin: "external"}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			if err := a.uninstall(name, []string{"--yes", "--purge"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			uninstallMissing(t, entry)
			uninstallMissing(t, session)
			if uninstallRead(t, external) != "external" || uninstallRead(t, unknown) != "user" {
				t.Fatal("purge crossed ownership boundary")
			}
		})
	}
}

func TestUninstallExternalOnlyIsNoop(t *testing.T) {
	for _, name := range toolNames {
		t.Run(name, func(t *testing.T) {
			a := uninstallApp(t)
			path := filepath.Join(t.TempDir(), toolBinaryName(name))
			uninstallWrite(t, path, "external")
			a.cfg.Tools[name] = toolState{BinaryPath: path, Origin: "external"}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			before := treeFingerprint(t, a.root)
			if err := a.uninstall(name, []string{"--yes", "--purge"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
				t.Fatal("external-only uninstall changed connection")
			}
			if uninstallRead(t, path) != "external" {
				t.Fatal("deleted external")
			}
		})
	}
}

func TestDisconnectCleansCredentialsAndPreservesPreferences(t *testing.T) {
	for _, name := range []string{"claude", "cursor", "grok", "codex", "opencode"} {
		t.Run(name, func(t *testing.T) {
			a := uninstallApp(t)
			ownedFixture(t, a, name)
			file := generatedNames(name)[0]
			if name == "claude" {
				file = "cli-settings.json"
			}
			path := filepath.Join(a.derivedDir(name), file)
			body := `{"model":"gate-model","env":{"ANTHROPIC_AUTH_TOKEN":"` + uninstallFixtureKey + `","ANTHROPIC_BASE_URL":"https://offline.invalid/agents/claude"}}`
			switch name {
			case "cursor":
				body = `{"version":1,"network":{"useHttp1ForAgent":true},"model":{"id":"auto"},"authInfo":{"email":"fixture@example.invalid"}}`
			case "grok":
				body = "# >>> SOC AGENT grok managed block >>>\napi_key = \"" + uninstallFixtureKey + "\"\n# <<< SOC AGENT grok managed block <<<\n"
			case "codex":
				body = "model_provider = \"llmgate\"\nmodel = \"gate-model\"\n[model_providers.llmgate]\nname = \"LLM Gate\"\nenv_key = \"LLMGATE_API_KEY\"\n"
			case "opencode":
				body = `{"model":"llmgate-fixture/model","provider":{"llmgate-fixture":{"name":"LLM Gate","options":{"apiKey":"{env:LLMGATE_API_KEY}"}}}}`
			}
			uninstallWrite(t, path, body)
			if err := a.recordGenerated(name); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(file, ".json") {
				var obj map[string]any
				json.Unmarshal([]byte(body), &obj)
				obj["userPreference"] = "keep"
				if name == "claude" {
					obj["env"].(map[string]any)["CUSTOM"] = "keep"
				}
				b, _ := json.Marshal(obj)
				uninstallWrite(t, path, string(b))
			} else {
				uninstallWrite(t, path, body+"\n[user_preferences]\ncustom = \"keep\"\n")
			}
			session := filepath.Join(a.derivedDir(name), "sessions", "keep.json")
			uninstallWrite(t, session, "private fixture")
			if err := a.disconnect(name); err != nil {
				t.Fatal(err)
			}
			got := uninstallRead(t, path)
			if strings.Contains(got, uninstallFixtureKey) || !strings.Contains(got, "keep") {
				t.Fatal("credentials or user preference cleanup incorrect")
			}
			if name == "cursor" && (!strings.Contains(got, "authInfo") || !strings.Contains(got, "auto")) {
				t.Fatal("cursor state lost")
			}
			if uninstallRead(t, session) != "private fixture" {
				t.Fatal("session lost")
			}
			receipt := uninstallRead(t, filepath.Join(a.root, "install-state.json"))
			if strings.Contains(receipt, uninstallFixtureKey) || strings.Contains(receipt, "private fixture") {
				t.Fatal("receipt contains private data")
			}
		})
	}
}

func TestUninstallSelfDefaultAndPurge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("self removal is exercised in a separate Windows process")
	}
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "purge"}[purge], func(t *testing.T) {
			a := uninstallApp(t)
			entry := ownedFixture(t, a, "claude")
			settings := filepath.Join(a.derivedDir("claude"), "cli-settings.json")
			uninstallWrite(t, settings, `{"env":{"ANTHROPIC_AUTH_TOKEN":"`+uninstallFixtureKey+`"}}`)
			if err := a.recordGenerated("claude"); err != nil {
				t.Fatal(err)
			}
			session := filepath.Join(a.derivedDir("claude"), "projects", "session.fixture")
			uninstallWrite(t, session, "private session")
			args := []string{"--yes"}
			if purge {
				args = append(args, "--purge")
			}
			if err := a.uninstall("", args, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			uninstallMissing(t, a.selfPath)
			uninstallMissing(t, filepath.Join(a.root, "config.json"))
			uninstallMissing(t, settings)
			if purge {
				uninstallMissing(t, entry)
				uninstallMissing(t, session)
			} else {
				uninstallRead(t, entry)
				uninstallRead(t, session)
			}
			for _, file := range []string{filepath.Join(a.root, "install-state.json")} {
				if strings.Contains(uninstallRead(t, file), uninstallFixtureKey) {
					t.Fatal("secret in receipt")
				}
			}
			if strings.Contains(a.out.(*bytes.Buffer).String()+a.err.(*bytes.Buffer).String(), uninstallFixtureKey) {
				t.Fatal("secret in diagnostics")
			}
		})
	}
}

func TestUninstallRejectsSharedConflictBeforeDeleting(t *testing.T) {
	a := uninstallApp(t)
	entry := ownedFixture(t, a, "codex")
	home := t.TempDir()
	catalog := a.sharedCatalogPath()
	uninstallWrite(t, catalog, `{"models":[]}`)
	st := sharedCodex{Home: home, Catalog: catalog, PrevProvider: "original", PrevProviderSet: true}
	if err := a.applySharedCodex(&st, runtimeTool{DefaultModel: "fixture"}, true); err != nil {
		t.Fatal(err)
	}
	a.cfg.Shared = map[string]sharedCodex{"codex": st}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	text := uninstallRead(t, configPath) + "extra_owned_field = true\n"
	uninstallWrite(t, configPath, text)
	before := treeFingerprint(t, a.root)
	if err := a.uninstall("codex", []string{"--yes", "--purge"}, strings.NewReader("")); err == nil {
		t.Fatal("shared conflict must block")
	}
	if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
		t.Fatal("preflight conflict changed managed files")
	}
	uninstallRead(t, entry)
	uninstallRead(t, catalog)
	if uninstallRead(t, configPath) != text {
		t.Fatal("shared user change lost")
	}
	if strings.Contains(a.out.(*bytes.Buffer).String(), uninstallFixtureKey) {
		t.Fatal("shared error leaked key")
	}
}

func TestUninstallSharedAuthPreservesAddedFields(t *testing.T) {
	a := uninstallApp(t)
	ownedFixture(t, a, "codex")
	home := t.TempDir()
	uninstallWrite(t, filepath.Join(home, "config.toml"), "model_provider = \"original\"\nmodel_catalog_json = \"original.json\"\n[projects.fixture]\ntrust_level = \"trusted\"\n")
	st := sharedCodex{Home: home, Catalog: a.sharedCatalogPath()}
	uninstallWrite(t, st.Catalog, `{"models":[]}`)
	if err := a.applySharedCodex(&st, runtimeTool{DefaultModel: "fixture"}, true); err != nil {
		t.Fatal(err)
	}
	a.cfg.Shared = map[string]sharedCodex{"codex": st}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, filepath.Join(home, "auth.json"), `{"auth_mode":"apikey","OPENAI_API_KEY":"`+uninstallFixtureKey+`","custom":"keep"}`)
	if err := a.uninstall("codex", []string{"--yes"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	config := uninstallRead(t, filepath.Join(home, "config.toml"))
	if !strings.Contains(config, `model_provider = "original"`) || !strings.Contains(config, `model_catalog_json = "original.json"`) || !strings.Contains(config, "trust_level") {
		t.Fatal("shared restore incorrect")
	}
	if got := uninstallRead(t, filepath.Join(home, "auth.json")); strings.Contains(got, uninstallFixtureKey) || !strings.Contains(got, "keep") {
		t.Fatal("shared auth cleanup incorrect")
	}
	uninstallMissing(t, st.Catalog)
}

func TestUninstallRejectsSymlinkAndIdentityChange(t *testing.T) {
	a := uninstallApp(t)
	entry := ownedFixture(t, a, "grok")
	p, err := a.buildUninstallPlan("grok", false)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(entry)
	uninstallWrite(t, entry, "reinstalled")
	if err := p.applyFiles(); err == nil {
		t.Fatal("replacement after preview must block")
	}
	if uninstallRead(t, entry) != "reinstalled" {
		t.Fatal("new installation deleted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	external := t.TempDir()
	uninstallWrite(t, filepath.Join(external, "grok"), "external")
	dir := filepath.Dir(entry)
	os.Rename(dir, dir+".saved")
	if err := os.Symlink(external, dir); err != nil {
		t.Fatal(err)
	}
	if err := a.uninstall("grok", []string{"--yes", "--purge"}, strings.NewReader("")); err == nil {
		t.Fatal("linked program root must block")
	}
	if uninstallRead(t, filepath.Join(external, "grok")) != "external" {
		t.Fatal("followed symlink")
	}
}

func TestUninstallLeaseAndDryRunNoWrites(t *testing.T) {
	a := uninstallApp(t)
	ownedFixture(t, a, "grok")
	l, err := acquireOperation(a.root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	l.releaseOperation()
	before := treeFingerprint(t, a.root)
	if err := a.uninstall("grok", []string{"--dry-run"}, strings.NewReader("")); err == nil {
		t.Fatal("running tool must block uninstall")
	}
	if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
		t.Fatal("busy dry-run wrote files")
	}
	second, err := acquireOperation(a.root, true)
	if err != nil {
		t.Fatal("concurrent launches should share a usage lease", err)
	}
	second.close()
}

func TestUninstallReceiptCannotExpandScope(t *testing.T) {
	a := uninstallApp(t)
	ownedFixture(t, a, "claude")
	s, err := readInstallState(a.root)
	if err != nil {
		t.Fatal(err)
	}
	program := s.Programs["claude"]
	program.Nodes["../outside"] = ownedNode{Kind: "file", Hash: digest([]byte("outside"))}
	s.Programs["claude"] = program
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := treeFingerprint(t, a.root)
	if err := a.uninstall("claude", []string{"--yes"}, strings.NewReader("")); err == nil {
		t.Fatal("receipt escaped root")
	}
	if !reflect.DeepEqual(before, treeFingerprint(t, a.root)) {
		t.Fatal("invalid receipt wrote files")
	}
}

func TestUserPathPreservesRawSegments(t *testing.T) {
	got, changed := removeUserPathSegment(`;C:\First;;C:\Gate; %USERPROFILE%\bin ;C:\Last;`, `c:\gate`)
	if !changed || got != `;C:\First;; %USERPROFILE%\bin ;C:\Last;` {
		t.Fatalf("PATH segments changed: %q", got)
	}
}

func TestUninstallShellBlockAndSharedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell PATH")
	}
	a := uninstallApp(t)
	s, err := readInstallState(a.root)
	if err != nil {
		t.Fatal(err)
	}
	s.Executable = a.selfPath
	s.ExecutableHash, _ = pathDigest(a.selfPath)
	rc := filepath.Join(t.TempDir(), ".profile")
	block := "\n# gate: LLM Gate 开发工具引导器\nexport PATH=\"" + filepath.Dir(a.selfPath) + ":$PATH\"\n"
	uninstallWrite(t, rc, "# user\n"+block)
	s.Path = []pathAddition{{File: rc, Directory: filepath.Dir(a.selfPath), Block: block}}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if err := a.uninstall("", []string{"--yes"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if uninstallRead(t, rc) != "# user\n" {
		t.Fatal("shell content changed")
	}
	if !sharedInstallDirectory(filepath.Join(mustUserHome(t), ".local", "bin")) {
		t.Fatal("shared bin directory not recognized")
	}
}
func mustUserHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestUninstallLocatorIgnoresChangedConfigEnvironment(t *testing.T) {
	a := uninstallApp(t)
	if err := a.recordExecutable(a.selfPath); err != nil {
		t.Fatal(err)
	}
	unrelated := t.TempDir()
	got, err := uninstallRootForExecutable(unrelated, a.selfPath)
	if err != nil || got != a.root {
		t.Fatalf("locator selected wrong root: %q %v", got, err)
	}
	p, err := a.buildUninstallPlan("", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range p.changes {
		if strings.HasPrefix(change.path, unrelated+string(filepath.Separator)) {
			t.Fatal("changed environment expanded deletion scope")
		}
	}
}

func TestPurgeRemovesModifiedManagedSettings(t *testing.T) {
	a := uninstallApp(t)
	ownedFixture(t, a, "claude")
	file := filepath.Join(a.derivedDir("claude"), "settings.json")
	uninstallWrite(t, file, `{"model":"gate-model"}`)
	if err := a.recordGenerated("claude"); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, file, `{"model":"user-model","custom":"user preference"}`)
	if err := a.uninstall("claude", []string{"--yes", "--purge"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	uninstallMissing(t, file)
}

func TestUninstallDetectsDirectlyLaunchedManagedBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process identity check")
	}
	a := uninstallApp(t)
	entry := ownedFixture(t, a, "grok")
	b, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, b, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(entry, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.recordProgram("grok", entry); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(entry, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := a.uninstall("grok", []string{"--yes"}, strings.NewReader("")); err == nil {
		t.Fatal("directly launched binary must block uninstall")
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("active binary removed")
	}
}

func TestProgramUpdateDoesNotClaimUnknownBinFiles(t *testing.T) {
	a := uninstallApp(t)
	entry := ownedFixture(t, a, "grok")
	unknown := filepath.Join(filepath.Dir(entry), "user-notes.txt")
	uninstallWrite(t, unknown, "keep")
	uninstallWrite(t, entry, "updated binary")
	if err := a.recordProgram("grok", entry); err != nil {
		t.Fatal(err)
	}
	if err := a.uninstall("grok", []string{"--yes", "--purge"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if uninstallRead(t, unknown) != "keep" {
		t.Fatal("update claimed unrelated file")
	}
}
