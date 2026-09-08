// cloudflare_test.go 钉住 Cloudflare Tunnel 管理端点：token 不出现在任何响应、
// 审计与日志；启用要过前置检查与两道确认；启用后公网接入方式随之切到
// cloudflare 且不能切走；组件制品上传是 octet-stream 且 CSRF 头照旧；
// 未注入管理器时端点如实答 503。
package admin_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"debug/elf"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// ---- 假依赖（与 internal/cloudflared 的测试桩同形，这里只要能走通端点）----

type cfEngine struct {
	slots []updated.ComponentSlot
	unit  string
	token string
}

func (f *cfEngine) status() *updated.ComponentStatus {
	st := &updated.ComponentStatus{Name: "cloudflared"}
	if len(f.slots) > 0 {
		st.Installed, st.CurrentSlot = true, "a"
		cur := f.slots[0]
		st.Current = &cur
	}
	return st
}

func (f *cfEngine) ComponentStatus(context.Context, string) (*updated.ComponentStatus, error) {
	return f.status(), nil
}

func (f *cfEngine) ComponentInstall(_ context.Context, _ string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	raw, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, &updated.EngineError{Status: 400, Message: "读取失败"}
	}
	f.slots = append([]updated.ComponentSlot{{Version: req.Version, SHA256: req.SHA256, SizeBytes: int64(len(raw))}}, f.slots...)
	return f.status(), nil
}

func (f *cfEngine) ComponentRollback(context.Context, string) (*updated.ComponentStatus, error) {
	return nil, &updated.EngineError{Status: 409, Message: updated.ErrNoPreviousSlot.Error()}
}

func (f *cfEngine) ComponentRemove(context.Context, string) (*updated.ComponentStatus, error) {
	f.slots, f.unit, f.token = nil, "inactive", ""
	return f.status(), nil
}

func (f *cfEngine) TunnelStatus(context.Context) (*updated.TunnelStatus, error) {
	state := f.unit
	if state == "" {
		state = "inactive"
	}
	return &updated.TunnelStatus{UnitState: state, TokenPresent: f.token != ""}, nil
}

func (f *cfEngine) TunnelStart(ctx context.Context, token string) (*updated.TunnelStatus, error) {
	if len(f.slots) == 0 {
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrComponentMissing.Error()}
	}
	f.token, f.unit = token, "active"
	return f.TunnelStatus(ctx)
}

func (f *cfEngine) TunnelStop(ctx context.Context) (*updated.TunnelStatus, error) {
	f.token, f.unit = "", "inactive"
	return f.TunnelStatus(ctx)
}

type cfListener struct {
	listening bool
	policy    tunnelctx.Policy
}

func (l *cfListener) StartTunnel(_, _ string, pol tunnelctx.Policy) error {
	l.listening, l.policy = true, pol
	return nil
}
func (l *cfListener) StopTunnel() error                       { l.listening = false; return nil }
func (l *cfListener) UpdateTunnelPolicy(pol tunnelctx.Policy) { l.policy = pol }
func (l *cfListener) TunnelListening() bool                   { return l.listening }
func (l *cfListener) BeginTunnelProbe() string                { return "" }
func (l *cfListener) EndTunnelProbe(string) bool              { return false }

type cfWebsite struct{ index, sig, artifact []byte }

func (w *cfWebsite) FetchComponentIndex(context.Context, string) ([]byte, []byte, error) {
	return w.index, w.sig, nil
}

func (w *cfWebsite) FetchComponentArtifact(_ context.Context, _ string, _ officialsite.ArtifactPolicy, out io.Writer) (string, int64, error) {
	out.Write(w.artifact)
	sum := sha256.Sum256(w.artifact)
	return hex.EncodeToString(sum[:]), int64(len(w.artifact)), nil
}

func minimalELF(tag string) []byte {
	machine, _ := elfcheck.HostMachine()
	b := make([]byte, 64)
	copy(b[:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4], b[5], b[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[58:], 64)
	return append(b, []byte(tag)...)
}

const fakeToken = "eyJhIjoiYWNjdCIsInQiOiIwZDZkM2UwYy0xYzJiLTRjNWQtOGU5Zi0wYTFiMmMzZDRlNWYiLCJzIjoic2VjcmV0LWJ5dGVzIn0="

type cfEnv struct {
	*env
	eng    *cfEngine
	ln     *cfListener
	web    *cfWebsite
	gate   bool
	binary []byte
}

func newCFEnv(t *testing.T) *cfEnv {
	t.Helper()
	base := newEnv(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := minimalELF("cloudflared-2026.8.2")
	sum := sha256.Sum256(binary)
	index, _ := json.Marshal(map[string]any{
		"schema": cloudflared.IndexSchema, "component": "cloudflared", "channel": "stable", "revision": 1,
		"updatedAt": "2026-08-26T00:00:00Z",
		"releases": []map[string]any{{
			"version": "2026.8.2", "platform": cloudflared.HostPlatform(),
			"artifactUrl":    "https://github.com/cloudflare/cloudflared/releases/download/2026.8.2/cloudflared-" + cloudflared.HostPlatform(),
			"artifactSha256": hex.EncodeToString(sum[:]), "sizeBytes": len(binary), "license": "Apache-2.0",
			"minComponentManager": 1, "allowInstall": true, "allowUpdate": true, "blocked": false,
		}},
	})
	sig, _ := json.Marshal(map[string]string{
		"schema": cloudflared.SignatureSchema, "keyId": "t", "algorithm": "ed25519",
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, index)),
	})
	ce := &cfEnv{env: base, eng: &cfEngine{}, ln: &cfListener{}, web: &cfWebsite{index: index, sig: sig, artifact: binary}, gate: true, binary: binary}
	mgr := cloudflared.NewManager(cloudflared.Options{
		DataDir: base.dir, Settings: base.st, Website: ce.web, Engine: ce.eng, Listener: ce.ln,
		Socket: filepath.Join(base.dir, "origin.sock"), Keys: map[string]ed25519.PublicKey{"t": pub},
		AdminGate: func(context.Context) bool { return ce.gate }, MetricsReadyURL: "http://127.0.0.1:1/ready",
	})
	base.srv.SetCloudflareTunnel(mgr)
	return ce
}

func (e *cfEnv) tunnel(cookie string) map[string]any {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/system/cloudflare-tunnel", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var body struct {
		Tunnel map[string]any `json:"tunnel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		e.t.Fatal(err)
	}
	return body.Tunnel
}

func TestCloudflareUnavailableWithoutManager(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/v1/system/cloudflare-tunnel"},
		{"PUT", "/admin/v1/system/cloudflare-tunnel"},
		{"POST", "/admin/v1/system/cloudflare-tunnel/enable"},
		{"GET", "/admin/v1/system/components/cloudflared"},
	} {
		resp := e.do(c.method, c.path, root, "{}")
		wantStatus(t, resp, http.StatusServiceUnavailable)
		if got := errCode(t, resp); got != "cloudflare_unavailable" {
			t.Errorf("%s %s error.code = %q", c.method, c.path, got)
		}
	}
	// 未注入时公网接入方式照常可切（cloudflare_enabled 恒假）。
	resp := e.do("PUT", "/admin/v1/system/external", root, setExternalBody("cloudflare", ""))
	wantStatus(t, resp, http.StatusOK)
}

func TestCloudflareTokenNeverEchoedAndLifecycle(t *testing.T) {
	e := newCFEnv(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/system/cloudflare-tunnel", "", "")
	wantStatus(t, resp, http.StatusUnauthorized)

	// 配置：hostname + token；响应与状态里没有 token。
	resp = e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, `{"hostname":"Box.Example.com","exposure":"api_only","token":"`+fakeToken+`"}`)
	wantStatus(t, resp, http.StatusOK)
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), fakeToken) || strings.Contains(string(raw), "secret-bytes") {
		t.Fatal("响应里出现了 token")
	}
	st := e.tunnel(root)
	cfg := st["config"].(map[string]any)
	if cfg["hostname"] != "box.example.com" || cfg["exposure"] != "api_only" || cfg["auto_update"] != true {
		t.Fatalf("配置读数不对: %+v", cfg)
	}
	if st["credential"].(map[string]any)["state"] != "sealed" || st["service_url"] != "unix:"+filepath.Join(e.dir, "origin.sock") {
		t.Fatalf("状态读数不对: %+v", st)
	}
	for _, bad := range []string{
		`{"hostname":"*.example.com"}`, `{"hostname":"box.local"}`, `{"exposure":"everything"}`,
		`{"token":"cloudflared tunnel run --token ` + fakeToken + `"}`, `{"token":"nope"}`,
	} {
		resp := e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, bad)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "invalid_cloudflare_config" {
			t.Errorf("%s error.code = %q", bad, got)
		}
	}

	// 启用：组件未装 → 409。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/enable", root, `{"accept_third_party":true}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "component_missing" {
		t.Fatalf("error.code = %q，期望 component_missing", got)
	}
	// 组件：检查清单 → 下载 → 安装。
	resp = e.do("POST", "/admin/v1/system/components/cloudflared/check", root, "{}")
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/components/cloudflared/download", root, "{}")
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/components/cloudflared/install", root, "{}")
	wantStatus(t, resp, http.StatusOK)
	var comp struct {
		Component struct {
			Installed bool   `json:"installed"`
			Version   string `json:"version"`
			State     string `json:"state"`
		} `json:"component"`
	}
	json.NewDecoder(resp.Body).Decode(&comp)
	if !comp.Component.Installed || comp.Component.Version != "2026.8.2" || comp.Component.State != "installed" {
		t.Fatalf("组件状态不对: %+v", comp)
	}
	// 未确认第三方 → 409。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/enable", root, `{}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "confirmation_required" {
		t.Fatalf("error.code = %q，期望 confirmation_required", got)
	}
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/enable", root, `{"accept_third_party":true}`)
	wantStatus(t, resp, http.StatusOK)
	if !e.ln.listening || e.ln.policy.Hostname != "box.example.com" || e.eng.token != fakeToken {
		t.Fatalf("启用后 listener/引擎状态不对: %+v token=%q", e.ln.policy, e.eng.token)
	}
	// 二选一：方式随之切到 cloudflare，接入读数公布 https://box.example.com，
	// 切走被拒。
	if ext := e.external(root); ext.Mode != "cloudflare" || !ext.CloudflareEnabled || ext.ExternalURL != "https://box.example.com" {
		t.Fatalf("启用后公网接入读数不对: %+v", ext)
	}
	if got := e.externalURL(root); got != "https://box.example.com" {
		t.Fatalf("endpoints external_url = %q", got)
	}
	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("manual", "https://other.example.com"))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "cloudflare_tunnel_active" {
		t.Fatalf("error.code = %q", got)
	}
	// 启用中改成 api_and_admin：缺省口令挡（rootSession 已改密，闸门由 gate 假装）。
	e.gate = false
	resp = e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, `{"exposure":"api_and_admin"}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "default_password" {
		t.Fatalf("error.code = %q，期望 default_password", got)
	}
	e.gate = true
	resp = e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, `{"exposure":"api_and_admin"}`)
	wantStatus(t, resp, http.StatusOK)
	if e.ln.policy.Profile != tunnelctx.ProfileAPIAndAdmin {
		t.Fatal("启用中改 exposure 应即时更新 listener 策略")
	}
	// 启用中清 token 被拒。
	resp = e.do("PUT", "/admin/v1/system/cloudflare-tunnel", root, `{"clear_token":true}`)
	wantStatus(t, resp, http.StatusConflict)
	// 自检（本地）。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/test", root, `{"public":false}`)
	wantStatus(t, resp, http.StatusOK)
	var test struct {
		Report struct {
			OK     bool `json:"ok"`
			Checks []struct {
				Layer string `json:"layer"`
				OK    bool   `json:"ok"`
			} `json:"checks"`
		} `json:"report"`
	}
	json.NewDecoder(resp.Body).Decode(&test)
	if len(test.Report.Checks) != 3 || test.Report.OK {
		t.Fatalf("本地自检应三项且（connector 未就绪）整体失败: %+v", test.Report)
	}
	// 停用保留 token → 方式仍是 cloudflare、无生效地址、可切走。
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/disable", root, `{"keep_token":true}`)
	wantStatus(t, resp, http.StatusOK)
	if ext := e.external(root); ext.Mode != "cloudflare" || ext.CloudflareEnabled || ext.ExternalURL != "" {
		t.Fatalf("停用后公网接入读数不对: %+v", ext)
	}
	if e.tunnel(root)["credential"].(map[string]any)["state"] != "sealed" {
		t.Fatal("keep_token 应保留密封 token")
	}
	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("manual", "https://other.example.com"))
	wantStatus(t, resp, http.StatusOK)
	if got := e.externalURL(root); got != "https://other.example.com" {
		t.Fatalf("切回外网映射后 external_url = %q", got)
	}
	// 删除本机配置（含卸载组件）：token 销毁、组件没了。
	resp = e.do("PUT", "/admin/v1/system/external", root, setExternalBody("cloudflare", ""))
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("POST", "/admin/v1/system/cloudflare-tunnel/delete", root, `{"remove_component":true}`)
	wantStatus(t, resp, http.StatusOK)
	st = e.tunnel(root)
	if st["credential"].(map[string]any)["state"] != "unset" || st["component"].(map[string]any)["installed"] != false || st["mode"] != "none" {
		t.Fatalf("删除后状态不对: %+v", st)
	}

	// §15.1：审计与日志里搜不到 token。
	for _, row := range e.auditRows() {
		if strings.Contains(row.Detail, fakeToken) || strings.Contains(row.Detail, "secret-bytes") {
			t.Fatalf("审计 detail 出现 token: %q", row.Detail)
		}
	}
	if strings.Contains(e.buf.String(), fakeToken) || strings.Contains(e.buf.String(), "secret-bytes") {
		t.Fatal("日志出现 token")
	}
	var sawEnable, sawUpdate bool
	for _, row := range e.auditRows() {
		switch row.Event {
		case "system.cloudflare_enable":
			sawEnable = strings.Contains(row.Detail, "box.example.com")
		case "system.cloudflare_update":
			sawUpdate = sawUpdate || strings.Contains(row.Detail, "token 已替换")
		}
	}
	if !sawEnable || !sawUpdate {
		t.Fatal("审计应记录启用（含 hostname）与 token 替换事件")
	}
}

func TestComponentUploadIsOctetStreamWithCSRF(t *testing.T) {
	e := newCFEnv(t)
	root := e.rootSession()
	resp := e.do("POST", "/admin/v1/system/components/cloudflared/check", root, "{}")
	wantStatus(t, resp, http.StatusOK)

	upload := func(ct string, csrf bool, body []byte) *http.Response {
		r := httptest.NewRequest(http.MethodPost, "/admin/v1/system/components/cloudflared/upload", bytes.NewReader(body))
		r.Header.Set("Content-Type", ct)
		if csrf {
			r.Header.Set("X-LlmGate-CSRF", "1")
		}
		r.AddCookie(&http.Cookie{Name: "llmgate_admin_session", Value: root})
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		return rec.Result()
	}
	if resp := upload("application/octet-stream", false, e.binary); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 头应 403: %d", resp.StatusCode)
	}
	if resp := upload("application/json", true, e.binary); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("JSON 体应 415: %d", resp.StatusCode)
	}
	if resp := upload("application/octet-stream", true, minimalELF("other")); resp.StatusCode != http.StatusConflict || errCode(t, resp) != "component_not_listed" {
		t.Fatalf("不在清单里的制品应 409 component_not_listed: %d", resp.StatusCode)
	}
	resp = upload("application/octet-stream", true, e.binary)
	wantStatus(t, resp, http.StatusOK)
	var comp struct {
		Component struct {
			State  string `json:"state"`
			Staged struct {
				Version string `json:"version"`
				Source  string `json:"source"`
			} `json:"staged"`
		} `json:"component"`
	}
	json.NewDecoder(resp.Body).Decode(&comp)
	if comp.Component.State != "staged" || comp.Component.Staged.Version != "2026.8.2" || comp.Component.Staged.Source != "upload" {
		t.Fatalf("上传后状态不对: %+v", comp)
	}
	resp = e.do("DELETE", "/admin/v1/system/components/cloudflared/staged", root, "")
	wantStatus(t, resp, http.StatusOK)
	// 离线导入清单：坏签名被拒。
	resp = e.do("POST", "/admin/v1/system/components/cloudflared/manifest", root, `{"index":"{}","signature":"{}"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "manifest_rejected" {
		t.Fatalf("error.code = %q", got)
	}
}

// auditRow 是审计表的一行（直读 sqlite，与板上走查的 sqlite3 同视角）。
type auditRow struct {
	Event  string
	Entity string
	Detail string
}

func (e *env) auditRows() []auditRow {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		e.t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		e.t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.Event, &a.Entity, &a.Detail); err != nil {
			e.t.Fatalf("扫描审计行: %v", err)
		}
		out = append(out, a)
	}
	return out
}
