// firmware_test.go 是固件升级管理面的可执行验收（docs/firmware-update.md）：
// 端点默认 admin-only、未注入升级管理器时 503、聚合快照形状、引擎不可达的
// 降级、upload 的 CSRF 例外（octet-stream 放行、JSON 拒绝，且只对那一条路径）
// 与假固件包的形状拒绝。官网与引擎皆为假实现，全程离线。
package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/update"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

type fwFakeWebsite struct {
	check *officialsite.FirmwareCheckResult
	err   error
}

func (f *fwFakeWebsite) FirmwareCheck(context.Context) (*officialsite.FirmwareCheckResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.check, nil
}
func (f *fwFakeWebsite) FetchFirmware(context.Context, string, io.Writer) (string, int64, error) {
	return "", 0, f.err
}

type fwFakeEngine struct {
	status *updated.Status
	err    error
}

func (f *fwFakeEngine) Status(context.Context) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}
func (f *fwFakeEngine) Install(context.Context, updated.InstallRequest) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}
func (f *fwFakeEngine) Rollback(context.Context) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}

func withUpdater(e *env, site update.Website, engine update.Engine) *update.Manager {
	m := update.NewManager(e.dir, site, engine, slog.New(slog.DiscardHandler))
	e.srv.SetFirmwareUpdater(m)
	return m
}

func TestFirmwareUnavailableWithoutUpdater(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()
	resp := e.do(http.MethodGet, "/admin/v1/system/firmware", admin, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未注入升级管理器应 503，得到 %d", resp.StatusCode)
	}
}

func TestFirmwareStatusShapeAndScope(t *testing.T) {
	e := newEnv(t)
	adminCookie := e.rootSession()
	withUpdater(e, &fwFakeWebsite{}, &fwFakeEngine{status: &updated.Status{
		Phase:            updated.PhaseIdle,
		InstalledVersion: "v1.0.0",
		Prev:             &updated.Slot{Version: "v0.9.0"},
		LastResult: &updated.Result{
			Kind: updated.KindInstall, Outcome: updated.OutcomeSuccess,
			FromVersion: "v0.9.0", ToVersion: "v1.0.0",
		},
	}})

	// 无会话走默认拒绝。
	if resp := e.do(http.MethodGet, "/admin/v1/system/firmware", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话应 401，得到 %d", resp.StatusCode)
	}

	resp := e.do(http.MethodGet, "/admin/v1/system/firmware", adminCookie, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态读数应 200，得到 %d", resp.StatusCode)
	}
	var body struct {
		CurrentVersion string `json:"current_version"`
		Engine         struct {
			Available   bool   `json:"available"`
			PrevVersion string `json:"prev_version"`
		} `json:"engine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.CurrentVersion == "" {
		t.Fatal("快照缺 current_version")
	}
	if !body.Engine.Available || body.Engine.PrevVersion != "v0.9.0" {
		t.Fatalf("引擎读数不对: %+v", body.Engine)
	}
}

func TestFirmwareEngineUnavailableDegrades(t *testing.T) {
	e := newEnv(t)
	adminCookie := e.rootSession()
	withUpdater(e, &fwFakeWebsite{}, &fwFakeEngine{err: updated.ErrUnavailable})

	// 状态页如实降级为 available:false。
	resp := e.do(http.MethodGet, "/admin/v1/system/firmware", adminCookie, "")
	var body struct {
		Engine struct {
			Available bool `json:"available"`
		} `json:"engine"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Engine.Available {
		t.Fatal("引擎不可达时 available 应为 false")
	}
	// 回退动作答 503 engine_unavailable。
	resp = e.do(http.MethodPost, "/admin/v1/system/firmware/rollback", adminCookie, "{}")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("引擎不可达的回退应 503，得到 %d", resp.StatusCode)
	}
}

func TestFirmwareInstallDemandsStaged(t *testing.T) {
	e := newEnv(t)
	adminCookie := e.rootSession()
	withUpdater(e, &fwFakeWebsite{}, &fwFakeEngine{status: &updated.Status{Phase: updated.PhaseIdle}})
	resp := e.do(http.MethodPost, "/admin/v1/system/firmware/install", adminCookie, "{}")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("无 staged 包的安装应 409，得到 %d", resp.StatusCode)
	}
}

func TestFirmwareUploadCSRFCarveOut(t *testing.T) {
	e := newEnv(t)
	adminCookie := e.rootSession()
	withUpdater(e, &fwFakeWebsite{}, &fwFakeEngine{status: &updated.Status{Phase: updated.PhaseIdle}})

	// upload 路径：JSON Content-Type 反而 415（那不是固件包的体裁）。
	r := e.req(http.MethodPost, "/admin/v1/system/firmware/upload", adminCookie, "{}")
	if resp := e.send(r); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("upload 收 JSON 应 415，得到 %d", resp.StatusCode)
	}
	// octet-stream + CSRF 头放行到 handler；垃圾字节被 fwimage 形状闸拒绝。
	r = e.req(http.MethodPost, "/admin/v1/system/firmware/upload", adminCookie, "")
	r.Body = io.NopCloser(strings.NewReader("definitely-not-an-elf"))
	r.Header.Set("Content-Type", "application/octet-stream")
	resp := e.send(r)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("垃圾固件包应 400，得到 %d", resp.StatusCode)
	}
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if eb.Error.Code != "invalid_firmware" {
		t.Fatalf("错误码应为 invalid_firmware，得到 %q", eb.Error.Code)
	}
	// 例外只对 upload 那一条路径：别的变更端点收 octet-stream 仍 415。
	r = e.req(http.MethodPost, "/admin/v1/system/firmware/check", adminCookie, "{}")
	r.Header.Set("Content-Type", "application/octet-stream")
	if resp := e.send(r); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("check 收 octet-stream 应 415，得到 %d", resp.StatusCode)
	}
	// 缺 CSRF 头照旧 403，upload 不例外。
	r = e.req(http.MethodPost, "/admin/v1/system/firmware/upload", adminCookie, "")
	r.Header.Del("X-LlmGate-CSRF")
	r.Header.Set("Content-Type", "application/octet-stream")
	if resp := e.send(r); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头应 403，得到 %d", resp.StatusCode)
	}
}
