// codexappserver_test.go 钉住 Codex App Server 组件端点：路由档位（读数 Admin、变更 LAN）、
// 未注入管理器时 503、上传路径的 octet-stream 放行与 CSRF 照旧，以及一条完整的
// 检查清单 → 下载 → 安装 → 升级 → 回退 → 卸载流程；卸载没有 component_in_use 守卫。
// 官网与引擎皆为假实现，全程离线。
package admin_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// casEngine 只有槽位四件事，带回退；启停方法不存在——组件没有 unit。
type casEngine struct {
	slots []updated.ComponentSlot // [0] current, [1] previous
	names map[string]int
}

func (f *casEngine) note(name string) {
	if f.names == nil {
		f.names = map[string]int{}
	}
	f.names[name]++
}

func (f *casEngine) status() *updated.ComponentStatus {
	st := &updated.ComponentStatus{Name: "codex-app-server"}
	if len(f.slots) > 0 {
		st.Installed, st.CurrentSlot = true, "a"
		cur := f.slots[0]
		st.Current = &cur
	}
	if len(f.slots) > 1 {
		prev := f.slots[1]
		st.PreviousSlot, st.Previous = "b", &prev
	}
	return st
}

func (f *casEngine) ComponentStatus(_ context.Context, name string) (*updated.ComponentStatus, error) {
	f.note(name)
	return f.status(), nil
}

func (f *casEngine) ComponentInstall(_ context.Context, name string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	f.note(name)
	// 引擎收到的是目录包：入口在 bin/codex-app-server，旁边有 code-mode host。
	raw, err := os.ReadFile(filepath.Join(req.Path, "bin", "codex-app-server"))
	if err != nil {
		return nil, &updated.EngineError{Status: 400, Message: "读取失败"}
	}
	if _, err := os.Stat(filepath.Join(req.Path, "bin", "codex-code-mode-host")); err != nil {
		return nil, &updated.EngineError{Status: 400, Message: "缺 code-mode host"}
	}
	f.slots = append([]updated.ComponentSlot{{Version: req.Version, SHA256: req.SHA256, SizeBytes: int64(len(raw))}}, f.slots...)
	if len(f.slots) > 2 {
		f.slots = f.slots[:2]
	}
	return f.status(), nil
}

func (f *casEngine) ComponentRollback(_ context.Context, name string) (*updated.ComponentStatus, error) {
	f.note(name)
	if len(f.slots) < 2 {
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrNoPreviousSlot.Error()}
	}
	f.slots[0], f.slots[1] = f.slots[1], f.slots[0]
	return f.status(), nil
}

func (f *casEngine) ComponentRemove(_ context.Context, name string) (*updated.ComponentStatus, error) {
	f.note(name)
	f.slots = nil
	return f.status(), nil
}

type casEnv struct {
	*env
	eng  *casEngine
	web  *cfWebsite
	tgz1 []byte
	tgz2 []byte
	priv ed25519.PrivateKey
}

// casTGZ 造官方整包形态的 tar.gz：入口 bin/codex-app-server 正文为 body，旁边是 code-mode host、
// 自述、rg、bwrap、zsh（都是本机架构的最小 ELF）。
func casTGZ(t *testing.T, version string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := []struct {
		name string
		body []byte
		mode int64
	}{
		{"bin/codex-app-server", body, 0o755},
		{"bin/codex-code-mode-host", minimalELF("host"), 0o755},
		{"codex-package.json", []byte(`{"layoutVersion":1,"version":"` + version + `","entrypoint":"bin/codex-app-server"}`), 0o644},
		{"codex-path/rg", minimalELF("rg"), 0o755},
		{"codex-resources/bwrap", minimalELF("bwrap"), 0o755},
		{"codex-resources/zsh/bin/zsh", minimalELF("zsh"), 0o755},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
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

func casRelease(version string, tgz, elf []byte) map[string]any {
	tsum, esum := sha256.Sum256(tgz), sha256.Sum256(elf)
	return map[string]any{
		"version": version, "platform": codexappserver.HostPlatform(),
		"artifactUrl":    "https://releases.openai.com/codex/releases/" + version + "/" + codexappserver.AssetName(codexappserver.HostPlatform()) + ".tar.gz",
		"artifactSha256": hex.EncodeToString(tsum[:]), "sizeBytes": len(tgz), "packaging": "tar.gz",
		"unpackedSha256": hex.EncodeToString(esum[:]), "unpackedSizeBytes": len(elf),
		"license": "Apache-2.0", "sourceUrl": "https://github.com/openai/codex/tree/rust-v" + version,
		"minComponentManager": codexappserver.ComponentManagerVersion, "allowInstall": true, "allowUpdate": true, "blocked": false,
	}
}

func (e *casEnv) signIndex(revision int, releases ...map[string]any) (index, sig []byte) {
	index, _ = json.Marshal(map[string]any{
		"schema": codexappserver.IndexSchema, "component": "codex-app-server", "channel": "stable", "revision": revision,
		"updatedAt": "2026-09-10T00:00:00Z", "releases": releases,
	})
	sig, _ = json.Marshal(map[string]string{
		"schema": codexappserver.SignatureSchema, "keyId": "t", "algorithm": "ed25519",
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(e.priv, index)),
	})
	return index, sig
}

func newCASEnv(t *testing.T) *casEnv {
	t.Helper()
	base := newEnv(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	elf1, elf2 := minimalELF("codex-app-server 0.154.0"), minimalELF("codex-app-server 0.155.0")
	ce := &casEnv{env: base, eng: &casEngine{}, priv: priv,
		tgz1: casTGZ(t, "0.154.0", elf1), tgz2: casTGZ(t, "0.155.0", elf2)}
	index, sig := ce.signIndex(1, casRelease("0.154.0", ce.tgz1, elf1))
	ce.web = &cfWebsite{index: index, sig: sig, artifact: ce.tgz1}
	mgr := codexappserver.NewManager(codexappserver.Options{
		DataDir: base.dir, Settings: base.st, Website: ce.web, Engine: ce.eng,
		Keys:          map[string]ed25519.PublicKey{"t": pub},
		ComponentsDir: filepath.Join(base.dir, "components-root"), // 假引擎不铺槽位：运行期视为未装
	})
	base.srv.SetCodexAppServer(mgr)
	return ce
}

type casComponent struct {
	State           string `json:"state"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version"`
	Staged          *struct {
		Version string `json:"version"`
		Source  string `json:"source"`
	} `json:"staged"`
	Advisory *struct {
		Revision int64 `json:"revision"`
		Latest   *struct {
			Version string `json:"version"`
		} `json:"latest"`
	} `json:"advisory"`
}

func (e *casEnv) component(t *testing.T, resp *http.Response) casComponent {
	t.Helper()
	var body struct {
		Component casComponent `json:"component"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Component
}

func TestCodexAppServerRouteTiersAndUnavailable(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	if got := tiers["GET /admin/v1/system/components/codex-app-server"]; got != tunnelctx.Admin {
		t.Errorf("读数档位 = %v，期望 Admin", got)
	}
	for _, pat := range []string{
		"POST /admin/v1/system/components/codex-app-server/check",
		"POST /admin/v1/system/components/codex-app-server/manifest",
		"POST /admin/v1/system/components/codex-app-server/download",
		"POST " + admin.CodexAppServerUploadPath,
		"POST /admin/v1/system/components/codex-app-server/install",
		"POST /admin/v1/system/components/codex-app-server/rollback",
		"POST /admin/v1/system/components/codex-app-server/selfcheck",
		"DELETE /admin/v1/system/components/codex-app-server/staged",
		"DELETE /admin/v1/system/components/codex-app-server",
	} {
		if got, ok := tiers[pat]; !ok || got != tunnelctx.LANOnly {
			t.Errorf("%s 档位 = %v（登记 %v），期望 LANOnly", pat, got, ok)
		}
	}
	// 未注入管理器：如实 503。
	root := e.rootSession()
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/v1/system/components/codex-app-server"},
		{"POST", "/admin/v1/system/components/codex-app-server/check"},
		{"POST", "/admin/v1/system/components/codex-app-server/selfcheck"},
		{"DELETE", "/admin/v1/system/components/codex-app-server"},
	} {
		resp := e.do(c.method, c.path, root, "{}")
		wantStatus(t, resp, http.StatusServiceUnavailable)
		if got := errCode(t, resp); got != "codex_app_server_unavailable" {
			t.Errorf("%s %s error.code = %q", c.method, c.path, got)
		}
	}
	r := e.req("POST", admin.CodexAppServerUploadPath, root, "xx")
	r.Header.Set("Content-Type", "application/octet-stream")
	wantStatus(t, e.send(r), http.StatusServiceUnavailable)
}

// casWebsiteDown 模拟官网尚未发布清单（404）或连不上：两者都应是 502 website_error，不是 500。
type casWebsiteDown struct{ err error }

func (w casWebsiteDown) FetchComponentIndex(context.Context, string) ([]byte, []byte, error) {
	return nil, nil, w.err
}

func (w casWebsiteDown) FetchComponentArtifact(context.Context, string, officialsite.ArtifactPolicy, io.Writer) (string, int64, error) {
	return "", 0, w.err
}

func TestCodexAppServerWebsiteErrorsAre502(t *testing.T) {
	for _, werr := range []error{
		&officialsite.HTTPError{Status: 404, Path: "/updates/components/codex-app-server/stable.json"},
		errors.New("无法连接 LLM Gate官网"),
		errors.New("连接 LLM Gate官网超时"),
	} {
		e := newEnv(t)
		e.srv.SetCodexAppServer(codexappserver.NewManager(codexappserver.Options{
			DataDir: e.dir, Settings: e.st, Website: casWebsiteDown{err: werr}, Engine: &casEngine{},
		}))
		root := e.rootSession()
		resp := e.do("POST", "/admin/v1/system/components/codex-app-server/check", root, "{}")
		wantStatus(t, resp, http.StatusBadGateway)
		if got := errCode(t, resp); got != "website_error" {
			t.Fatalf("%v: error.code = %q，期望 website_error", werr, got)
		}
		for _, row := range e.auditRows() {
			if row.Entity == "component:codex-app-server" {
				t.Fatalf("失败的检查不该留审计: %+v", row)
			}
		}
	}
}

func TestCodexAppServerLifecycle(t *testing.T) {
	e := newCASEnv(t)
	root := e.rootSession()
	wantStatus(t, e.do("GET", "/admin/v1/system/components/codex-app-server", "", ""), http.StatusUnauthorized)

	comp := e.component(t, e.do("GET", "/admin/v1/system/components/codex-app-server", root, ""))
	if comp.State != "not_installed" || comp.Advisory != nil {
		t.Fatalf("初始读数: %+v", comp)
	}
	// 没有清单：下载 409 component_not_ready。
	resp := e.do("POST", "/admin/v1/system/components/codex-app-server/download", root, "{}")
	wantStatus(t, resp, http.StatusConflict)
	if errCode(t, resp) != "component_not_ready" {
		t.Fatal("没有清单就下载应 component_not_ready")
	}
	for _, step := range []string{"check", "download"} {
		wantStatus(t, e.do("POST", "/admin/v1/system/components/codex-app-server/"+step, root, "{}"), http.StatusOK)
	}
	comp = e.component(t, e.do("GET", "/admin/v1/system/components/codex-app-server", root, ""))
	if comp.State != "staged" || comp.Staged == nil || comp.Staged.Version != "0.154.0" || comp.Staged.Source != "website" {
		t.Fatalf("下载后读数: %+v", comp)
	}
	comp = e.component(t, e.do("POST", "/admin/v1/system/components/codex-app-server/install", root, "{}"))
	if comp.State != "installed" || comp.Version != "0.154.0" || comp.Staged != nil {
		t.Fatalf("安装后读数: %+v", comp)
	}
	// 自检：假引擎没有真的铺槽位树，运行期解析不到 current → 409，读数如实 ok=false 并点明原因；
	// 读数里的 runtime 也报 layout_ok=false。
	resp = e.do("POST", "/admin/v1/system/components/codex-app-server/selfcheck", root, "{}")
	wantStatus(t, resp, http.StatusConflict)
	var sc struct {
		SelfCheck struct {
			OK        bool   `json:"ok"`
			Handshake bool   `json:"handshake"`
			Error     string `json:"error"`
		} `json:"selfcheck"`
		Component struct {
			Runtime *struct {
				LayoutOK bool `json:"layout_ok"`
				Running  int  `json:"running"`
			} `json:"runtime"`
		} `json:"component"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		t.Fatal(err)
	}
	if sc.SelfCheck.OK || sc.SelfCheck.Handshake || !strings.Contains(sc.SelfCheck.Error, "尚未安装") || sc.Component.Runtime == nil || sc.Component.Runtime.LayoutOK || sc.Component.Runtime.Running != 0 {
		t.Fatalf("自检读数: %+v", sc)
	}
	// 没有上一 slot 的回退：409 engine_rejected。
	resp = e.do("POST", "/admin/v1/system/components/codex-app-server/rollback", root, "{}")
	wantStatus(t, resp, http.StatusConflict)
	if errCode(t, resp) != "engine_rejected" {
		t.Fatalf("无上一 slot 回退 code = %q", errCode(t, resp))
	}

	// 离线导入 revision 2（含 0.155.0），上传官方 tar.gz 升级，再回退。
	elf2 := minimalELF("codex-app-server 0.155.0")
	elf1 := minimalELF("codex-app-server 0.154.0")
	index, sig := e.signIndex(2, casRelease("0.154.0", e.tgz1, elf1), casRelease("0.155.0", e.tgz2, elf2))
	body, _ := json.Marshal(map[string]string{"index": string(index), "signature": string(sig)})
	comp = e.component(t, e.do("POST", "/admin/v1/system/components/codex-app-server/manifest", root, string(body)))
	if comp.State != "update_available" || comp.Advisory == nil || comp.Advisory.Latest == nil || comp.Advisory.Latest.Version != "0.155.0" {
		t.Fatalf("导入清单后读数: %+v", comp)
	}
	// 篡改签名：400 component_manifest_rejected。
	bad, _ := json.Marshal(map[string]string{"index": string(index) + " ", "signature": string(sig)})
	resp = e.do("POST", "/admin/v1/system/components/codex-app-server/manifest", root, string(bad))
	wantStatus(t, resp, http.StatusBadRequest)
	if errCode(t, resp) != "component_manifest_rejected" {
		t.Fatalf("篡改清单 code = %q", errCode(t, resp))
	}

	upload := func(ct string, csrf bool, body []byte) *http.Response {
		r := httptest.NewRequest(http.MethodPost, admin.CodexAppServerUploadPath, bytes.NewReader(body))
		r.Header.Set("Content-Type", ct)
		if csrf {
			r.Header.Set("X-LlmGate-CSRF", "1")
		}
		r.AddCookie(&http.Cookie{Name: "llmgate_admin_session", Value: root})
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		return rec.Result()
	}
	if resp := upload("application/octet-stream", false, e.tgz2); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 头应 403: %d", resp.StatusCode)
	}
	if resp := upload("application/json", true, e.tgz2); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("JSON 体应 415: %d", resp.StatusCode)
	}
	if resp := upload("application/octet-stream", true, casTGZ(t, "9.9.9", minimalELF("other"))); resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "component_not_listed" {
		t.Fatalf("不在清单里的制品应 400 component_not_listed: %d", resp.StatusCode)
	}
	if resp := upload("application/octet-stream", true, elf2); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("裸 ELF（不是 tar.gz）应 400: %d", resp.StatusCode)
	}
	comp = e.component(t, upload("application/octet-stream", true, e.tgz2))
	if comp.State != "staged" || comp.Staged == nil || comp.Staged.Version != "0.155.0" || comp.Staged.Source != "upload" {
		t.Fatalf("上传后读数: %+v", comp)
	}
	comp = e.component(t, e.do("POST", "/admin/v1/system/components/codex-app-server/install", root, "{}"))
	if comp.Version != "0.155.0" || comp.PreviousVersion != "0.154.0" || comp.State != "installed" {
		t.Fatalf("升级后读数: %+v", comp)
	}
	comp = e.component(t, e.do("POST", "/admin/v1/system/components/codex-app-server/rollback", root, "{}"))
	if comp.Version != "0.154.0" || comp.PreviousVersion != "0.155.0" {
		t.Fatalf("回退后读数: %+v", comp)
	}
	// 丢弃：没有就绪制品时也 200；有则删并留审计。
	wantStatus(t, e.do("DELETE", "/admin/v1/system/components/codex-app-server/staged", root, ""), http.StatusOK)
	comp = e.component(t, upload("application/octet-stream", true, e.tgz2))
	if comp.Staged == nil {
		t.Fatal("上传后应就绪")
	}
	comp = e.component(t, e.do("DELETE", "/admin/v1/system/components/codex-app-server/staged", root, ""))
	if comp.Staged != nil {
		t.Fatalf("丢弃后读数: %+v", comp)
	}
	// 卸载：无守卫，直接 200；读数回 not_installed，清单保留。
	comp = e.component(t, e.do("DELETE", "/admin/v1/system/components/codex-app-server", root, ""))
	if comp.Installed || comp.State != "not_installed" || comp.Advisory == nil || comp.Advisory.Revision != 2 {
		t.Fatalf("卸载后读数: %+v", comp)
	}
	if len(e.eng.slots) != 0 {
		t.Fatalf("引擎应已删掉组件目录: slots=%d", len(e.eng.slots))
	}
	for name := range e.eng.names {
		if name != "codex-app-server" {
			t.Fatalf("引擎收到别的组件名: %q", name)
		}
	}
	// 审计：只有动作、版本与来源，没有地址或摘要以外的内容。
	want := map[string]string{
		"system.component_check":     "官网组件清单 revision 1，可安装 0.154.0",
		"system.component_download":  "下载 Codex App Server 官方制品 0.154.0",
		"system.component_install":   "安装 Codex App Server 0.154.0（来源 website）",
		"system.component_upload":    "上传 Codex App Server 官方制品 0.155.0",
		"system.component_rollback":  "Codex App Server 回退到上一 slot（0.154.0）",
		"system.component_discard":   "丢弃 Codex App Server 制品 0.155.0",
		"system.component_remove":    "卸载 Codex App Server 组件（0.154.0）",
		"system.component_selfcheck": "Codex App Server 自检：失败",
	}
	seen := map[string]bool{}
	for _, row := range e.auditRows() {
		if row.Entity != "component:codex-app-server" {
			continue
		}
		if exp, ok := want[row.Event]; ok && row.Detail == exp {
			seen[row.Event] = true
		}
	}
	for ev := range want {
		if !seen[ev] {
			t.Errorf("缺审计 %s（期望 detail %q）", ev, want[ev])
		}
	}
}
