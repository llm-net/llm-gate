package admin_test

// 媒体生成管理端点（/admin/v1/media/*）验收：假后端 + 真内核（internal/mediagen），全程离线。
//   - 能力表与可用性（models）、提交（单个 / 同批候选）、归属密钥裁决、并发闸与准入闸；
//   - 陪等、人工查询平台任务、单条读取、列表只列页面来源、删除 / 清空与审计；
//   - 结果下载 / 内联 / 缩略图与封面帧回传、启动时的残留任务处理与孤儿文件清扫。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/sysinfo"
)

// fakeMedia 是一台「平台还没回」的数据面（admin.MediaGateway）：grok 与 codex 两个假后端共用
// 一份状态，Generate 一直等到 release 被关闭才返回，模拟画图平台一两分钟的生成时间；
// 准入闸缺省放行，admit 非空时按它裁决。
type fakeMedia struct {
	release chan struct{}
	result  mediagen.Result
	err     error
	calls   chan mediagen.Request

	mu      sync.Mutex
	admit   func(keyID int64, m mediagen.Model) error
	refresh mediagen.Result
	// admitted / refreshed 数准入闸与平台查询各被调了几次。
	admitted  atomic.Int64
	refreshed atomic.Int64
}

func newFakeMedia(res mediagen.Result) *fakeMedia {
	return &fakeMedia{release: make(chan struct{}), result: res, calls: make(chan mediagen.Request, 16)}
}

// readyMedia 是立刻出结果的假数据面。
func readyMedia(res mediagen.Result) *fakeMedia {
	p := newFakeMedia(res)
	close(p.release)
	return p
}

func (p *fakeMedia) MediaBackends() []mediagen.Backend {
	return []mediagen.Backend{fakeBackend{p, mediagen.BackendGrok}, fakeBackend{p, mediagen.BackendCodex}}
}

func (p *fakeMedia) AdmitMediaJob(_ context.Context, keyID int64, m mediagen.Model) error {
	p.admitted.Add(1)
	p.mu.Lock()
	admit := p.admit
	p.mu.Unlock()
	if admit != nil {
		return admit(keyID, m)
	}
	return nil
}

func (p *fakeMedia) setRefresh(res mediagen.Result) {
	p.mu.Lock()
	p.refresh = res
	p.mu.Unlock()
}

type fakeBackend struct {
	p    *fakeMedia
	name string
}

func (b fakeBackend) Name() string { return b.name }

// Validate 模拟后端的搭配约束钩子（能力表表达不了的那一类）：透明背景只配 PNG。
func (b fakeBackend) Validate(in mediagen.Request) error {
	if in.Params.String("background") == "transparent" {
		if f := in.Params.String("output_format"); f != "" && f != "png" {
			return &mediagen.InvalidError{Msg: "透明背景只配 PNG 格式"}
		}
	}
	return nil
}

func (b fakeBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	b.p.calls <- in
	select {
	case <-b.p.release:
		return b.p.result, b.p.err
	case <-ctx.Done():
		return mediagen.Result{}, ctx.Err()
	}
}

func (b fakeBackend) Refresh(context.Context, store.MediaJob) (mediagen.Result, error) {
	b.p.refreshed.Add(1)
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	return b.p.refresh, nil
}

// pngResult 是一次成功的图像结果（data URI 解码后是 "ABC"）。
var pngResult = mediagen.Result{Status: store.MediaStatusSucceeded, MediaURL: "data:image/png;base64,QUJD", MediaType: "image", AccountID: 7}

// mediaAccounts 录入 Codex 与 Grok 各一份订阅账号（假凭据），回两行的 id。
func mediaAccounts(t *testing.T, e *env) (codexID, grokID int64) {
	t.Helper()
	codex, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCodex, Label: "codex", AuthJSON: `{"tokens":{"access_token":"fake"}}`})
	if err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	grok, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderGrok, Label: "grok", AuthJSON: `{"tokens":{"access_token":"fake"}}`})
	if err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	return codex.ID, grok.ID
}

// mediaKey 建一把 API 密钥并按给定账号钉订阅（0 = 不钉）。
func mediaKey(t *testing.T, e *env, label, digestChar, prefix, last4 string, codexID, grokID int64) int64 {
	t.Helper()
	key, err := e.st.CreateAPIKey(t.Context(), label, strings.Repeat(digestChar, 64), prefix, last4, "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if codexID != 0 || grokID != 0 {
		if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
			KeyID: key.ID, CodexAccountID: codexID, GrokAccountID: grokID}); err != nil {
			t.Fatalf("ReplaceDevToolConfig: %v", err)
		}
	}
	return key.ID
}

// mediaOwner 建一把可用的 API 密钥，并给它钉上 Grok 与 Codex 两份订阅账号：
// 管理面提交必须选一把密钥（生成以它的名义调用平台），用例都用这一把。
func mediaOwner(t *testing.T, e *env) int64 {
	t.Helper()
	codexID, grokID := mediaAccounts(t, e)
	return mediaKey(t, e, "生成", "e", "sk_media001", "9999", codexID, grokID)
}

// mediaOwnerDisplay 是 mediaOwner 那把密钥的展示串（账本与任务行的密钥维度）。
const mediaOwnerDisplay = "sk_media001…9999"

// withKey 把归属密钥拼进提交体（管理面必填）。
func withKey(keyID int64, body string) string {
	return `{"key_id":` + jsonNumber(keyID) + "," + strings.TrimPrefix(body, "{")
}

func decodeMediaJob(t *testing.T, resp *http.Response) store.MediaJob {
	t.Helper()
	var job store.MediaJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("解码任务: %v", err)
	}
	return job
}

// mediaBatch 是提交端点的 202 响应。
type mediaBatch struct {
	BatchID string           `json:"batch_id"`
	Jobs    []store.MediaJob `json:"jobs"`
}

// submitMedia 提交并断言 202，回同批任务。
func submitMedia(t *testing.T, e *env, cookie, body string) mediaBatch {
	t.Helper()
	resp := e.do("POST", "/admin/v1/media/jobs", cookie, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交状态 = %d，期望 202；body: %s", resp.StatusCode, readAll(t, resp))
	}
	var out mediaBatch
	decodeInto(t, resp, &out)
	if len(out.Jobs) == 0 {
		t.Fatal("提交返回里没有任务")
	}
	return out
}

func listMediaJobs(t *testing.T, e *env, cookie string) []store.MediaJob {
	t.Helper()
	resp := e.do("GET", "/admin/v1/media/jobs", cookie, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Jobs []store.MediaJob `json:"jobs"`
	}
	decodeInto(t, resp, &list)
	return list.Jobs
}

// countMediaJobs 从库里数任务行（全部来源），验「被拒的提交不落库」。
func countMediaJobs(t *testing.T, e *env) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开任务视角连接: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_jobs`).Scan(&n); err != nil {
		t.Fatalf("数任务行: %v", err)
	}
	return n
}

// countAudit 数某个审计事件的条数，回最后一条的 entity 与 detail。
func countAudit(t *testing.T, e *env, event string) (n int, entity, detail string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(entity), ''), COALESCE(MAX(detail), '') FROM audit_events WHERE event = ?`,
		event).Scan(&n, &entity, &detail); err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	return n, entity, detail
}

// 能力表端点：不带 key_id 只回能力表（每项 available=false、原因为空）；带 key_id 逐模型给出那把
// 密钥的可用性；密钥不存在 404、停用 409、key_id 不是正整数 400。
func TestMediaModels(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	e.srv.SetMediaGateway(newFakeMedia(mediagen.Result{}))
	codexID, grokID := mediaAccounts(t, e)
	both := mediaKey(t, e, "全部", "e", "sk_media001", "9999", codexID, grokID)
	grokOnly := mediaKey(t, e, "只有 Grok", "f", "sk_grok0001", "0000", 0, grokID)

	type modelsResp struct {
		RunningPerKey int                     `json:"running_per_key"`
		Models        []mediagen.Availability `json:"models"`
	}
	var bare modelsResp
	resp := e.do("GET", "/admin/v1/media/models", cookie, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &bare)
	presets := mediagen.Presets()
	if bare.RunningPerKey != mediagen.RunningPerKey || len(bare.Models) != len(presets) {
		t.Fatalf("能力表 = running_per_key %d、%d 个模型，期望 %d / %d", bare.RunningPerKey, len(bare.Models), mediagen.RunningPerKey, len(presets))
	}
	for i, m := range bare.Models {
		if m.ID != presets[i].ID || m.Backend != presets[i].Backend || m.Kind != presets[i].Kind || m.Billing != mediagen.BillingSubscription || len(m.Operations) == 0 {
			t.Fatalf("能力表第 %d 项 = %+v，期望预设 %s", i, m, presets[i].ID)
		}
		if m.Available || m.ReasonCode != "" || m.Reason != "" {
			t.Fatalf("不带 key_id 时 %s 不该有可用性裁决：%+v", m.ID, m)
		}
	}
	// 能力表的形状：视频生成带首帧 / 尾帧 / 参考图三个角色与时长参数，页面表单据此渲染。
	raw := readAll(t, e.do("GET", "/admin/v1/media/models", cookie, ""))
	for _, want := range []string{`"role":"first_frame"`, `"role":"source_video"`, `"prompt_optional_with":["first_frame","last_frame"]`, `"name":"duration"`, `"type":"integer"`, `"type":"enum"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("能力表 JSON 缺少 %s: %s", want, raw)
		}
	}

	var all modelsResp
	resp = e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(both), cookie, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &all)
	for _, m := range all.Models {
		if !m.Available || m.ReasonCode != "" {
			t.Fatalf("钉了两份订阅的密钥：%s 应可用，得到 %+v", m.ID, m)
		}
	}
	var partial modelsResp
	resp = e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(grokOnly), cookie, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &partial)
	if len(partial.Models) != len(presets) {
		t.Fatalf("可用性读数 = %d 项，期望 %d", len(partial.Models), len(presets))
	}
	for _, m := range partial.Models {
		switch m.Backend {
		case mediagen.BackendGrok:
			if !m.Available {
				t.Fatalf("%s 应可用：%+v", m.ID, m)
			}
		case mediagen.BackendCodex:
			if m.Available || m.ReasonCode != mediagen.ReasonSubscriptionNotAllowed || m.Reason == "" {
				t.Fatalf("%s 应因未获授权不可用：%+v", m.ID, m)
			}
		}
	}
	// 钉着的账号被停用：订阅当前不可用。
	if err := e.st.SetAgentStatus(t.Context(), grokID, store.AgentStatusDisabled); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp = e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(grokOnly), cookie, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &partial)
	for _, m := range partial.Models {
		if m.Backend == mediagen.BackendGrok && (m.Available || m.ReasonCode != mediagen.ReasonAgentNotConfigured) {
			t.Fatalf("账号停用后 %s = %+v，期望 agent_not_configured", m.ID, m)
		}
	}

	resp = e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(both+9999), cookie, "")
	if resp.StatusCode != http.StatusNotFound || errCode(t, resp) != "not_found" {
		t.Fatalf("密钥不存在 = %d，期望 404 not_found", resp.StatusCode)
	}
	if err := e.st.SetAPIKeyDisabled(t.Context(), grokOnly, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	resp = e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(grokOnly), cookie, "")
	if resp.StatusCode != http.StatusConflict || errCode(t, resp) != "key_disabled" {
		t.Fatalf("停用的密钥 = %d，期望 409 key_disabled", resp.StatusCode)
	}
	for _, bad := range []string{"abc", "0", "-3"} {
		resp = e.do("GET", "/admin/v1/media/models?key_id="+bad, cookie, "")
		if resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "invalid_request" {
			t.Fatalf("key_id=%s → %d，期望 400 invalid_request", bad, resp.StatusCode)
		}
	}
}

// 没接数据面（窄进程）：能力表照给，但每个模型都因生成服务不可用而不可用，提交答 503。
func TestMediaWithoutGateway(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	owner := mediaOwner(t, e)
	var out struct {
		Models []mediagen.Availability `json:"models"`
	}
	resp := e.do("GET", "/admin/v1/media/models?key_id="+jsonNumber(owner), cookie, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &out)
	for _, m := range out.Models {
		if m.Available || m.ReasonCode != mediagen.ReasonBackendUnavailable {
			t.Fatalf("没接后端时 %s = %+v", m.ID, m)
		}
	}
	resp = e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"p"}`))
	if resp.StatusCode != http.StatusServiceUnavailable || errCode(t, resp) != "media_unavailable" {
		t.Fatalf("没接后端时提交 = %d，期望 503 media_unavailable", resp.StatusCode)
	}
}

// 提交不同步等平台：请求立刻以 202 回 running 任务，生成在后台跑；
// 页面用 wait 陪等，任务一落终态就拿到结果。
func TestMediaCreateReturnsImmediatelyAndWaitFollows(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(pngResult)
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)

	batch := submitMedia(t, e, cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"一只小猫"}`))
	if batch.BatchID != "" || len(batch.Jobs) != 1 {
		t.Fatalf("单个提交 = batch %q、%d 条，期望空批次一条", batch.BatchID, len(batch.Jobs))
	}
	job := batch.Jobs[0]
	if job.Status != store.MediaStatusRunning || job.ID == "" || job.Model != "gpt-image-2" || job.CreatedAt.IsZero() ||
		job.Origin != store.MediaOriginPage || job.Backend != mediagen.BackendCodex || job.Provider != mediagen.BackendCodex ||
		job.Kind != store.ModelKindImage || job.Operation != store.MediaOpGenerate || job.BatchID != "" {
		t.Fatalf("提交返回 = %+v，期望 running 的页面任务", job)
	}
	select {
	case in := <-p.calls:
		if in.Prompt != "一只小猫" || in.Model.ID != "gpt-image-2" || in.Model.Backend != mediagen.BackendCodex || in.JobID != job.ID || in.Operation != store.MediaOpGenerate {
			t.Fatalf("后台请求 = %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("后台没有发起平台请求")
	}

	// 平台还没回：列表里是 running，任务行不带结果；单条读取同一帧。
	if jobs := listMediaJobs(t, e, cookie); len(jobs) != 1 || jobs[0].Status != store.MediaStatusRunning {
		t.Fatalf("列表 = %+v，期望一条 running", jobs)
	}
	resp := e.do("GET", "/admin/v1/media/jobs/"+job.ID, cookie, "")
	wantStatus(t, resp, http.StatusOK)
	if got := decodeMediaJob(t, resp); got.ID != job.ID || got.Status != store.MediaStatusRunning || got.Prompt != "一只小猫" {
		t.Fatalf("单条读取 = %+v", got)
	}
	resp = e.do("GET", "/admin/v1/media/jobs/NOPE", cookie, "")
	if resp.StatusCode != http.StatusNotFound || errCode(t, resp) != "not_found" {
		t.Fatalf("不存在的任务 = %d，期望 404 not_found", resp.StatusCode)
	}

	// 陪等：先挂着，平台一回就醒。
	done := make(chan store.MediaJob, 1)
	go func() {
		done <- decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/wait", cookie, `{}`))
	}()
	select {
	case got := <-done:
		t.Fatalf("平台未返回时陪等就回了：%+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	close(p.release)
	select {
	case got := <-done:
		// 结果落在数据目录 preview/<ID>.png，行里不再留 data URI。
		if got.Status != store.MediaStatusSucceeded || got.MediaFile != got.ID+".png" || got.MediaURL != "" || got.AccountID != 7 || got.FinishedAt == nil {
			t.Fatalf("陪等结果 = %+v，期望 succeeded 且带本地文件与账号", got)
		}
		if body, err := os.ReadFile(filepath.Join(e.dir, mediagen.MediaDirName, got.MediaFile)); err != nil || string(body) != "ABC" {
			t.Fatalf("结果文件 = %q / %v，期望解码后的 data URI", body, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("平台返回后陪等没有醒来")
	}
	// 终态后再陪等立即返回同一帧。
	again := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/wait", cookie, `{}`))
	if again.Status != store.MediaStatusSucceeded {
		t.Fatalf("终态陪等 = %+v", again)
	}
}

// 参数错误在提交时同步拒绝（400 media_invalid，JSON 坏是 invalid_request），模型不在能力表里
// 是 404 model_not_found；都不落库、不过准入闸、不起后台任务。
func TestMediaCreateRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(mediagen.Result{})
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)
	for _, tc := range []struct {
		body, code string
		want       int
	}{
		{`{"model":"","prompt":"p"}`, "media_invalid", http.StatusBadRequest},
		{`{"model":"gpt-image-2","prompt":"  "}`, "media_invalid", http.StatusBadRequest},
		{`{"model":"gpt-image-2","prompt":"p","count":4}`, "media_invalid", http.StatusBadRequest},
		{`{"model":"gpt-image-2","prompt":"p","count":-1}`, "media_invalid", http.StatusBadRequest},
		{`{"model":"gpt-image-2","prompt":"p","operation":"remix"}`, "media_invalid", http.StatusBadRequest},
		{`{"model":"claude-image","prompt":"p"}`, "model_not_found", http.StatusNotFound},
	} {
		resp := e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, tc.body))
		if resp.StatusCode != tc.want {
			t.Fatalf("%s → %d，期望 %d；body: %s", tc.body, resp.StatusCode, tc.want, readAll(t, resp))
		}
		if code := errCode(t, resp); code != tc.code {
			t.Fatalf("%s → 错误码 %q，期望 %q", tc.body, code, tc.code)
		}
	}
	resp := e.do("POST", "/admin/v1/media/jobs", cookie, `{"key_id":`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "invalid_request" {
		t.Fatalf("坏 JSON → %d，期望 400 invalid_request", resp.StatusCode)
	}
	if n := countMediaJobs(t, e); n != 0 {
		t.Fatalf("坏输入不该落库：%d 条", n)
	}
	if len(p.calls) != 0 || p.admitted.Load() != 0 {
		t.Fatalf("坏输入不该起后台任务 / 过准入闸：calls=%d admitted=%d", len(p.calls), p.admitted.Load())
	}
}

// 媒体输入（首帧 / 尾帧 / 参考图）按角色原样交给后端，任务行只记输入形态与参数快照；
// 不合法的输入与参数在提交时同步拒绝（400 media_invalid）。
func TestMediaCreateForwardsInputs(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(mediagen.Result{})
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)
	const dataURI = "data:image/png;base64,iVBORw0KGgo="
	body := `{"model":"grok-imagine-video-1.5","prompt":"walk","inputs":{"first_frame":"` + dataURI + `","last_frame":"https://example.invalid/last.png","reference_images":["https://example.invalid/a.png","` + dataURI + `"]},"params":{"duration":10,"resolution":"720P","generate_audio":false,"voices":["eve"]}}`
	job := submitMedia(t, e, cookie, withKey(owner, body)).Jobs[0]
	if job.Params["duration"] != float64(10) || job.Params["resolution"] != "720p" || job.Params["generate_audio"] != false || len(job.Params) != 4 {
		t.Fatalf("任务行的参数快照 = %+v", job.Params)
	}
	if voices, _ := job.Params["voices"].([]any); len(voices) != 1 || voices[0] != "eve" {
		t.Fatalf("任务行的音色 = %+v", job.Params["voices"])
	}
	if job.Inputs[store.MediaRoleFirstFrame] != 1 || job.Inputs[store.MediaRoleLastFrame] != 1 || job.Inputs[store.MediaRoleReferenceImages] != 2 || len(job.Inputs) != 3 {
		t.Fatalf("任务行的输入形态 = %+v", job.Inputs)
	}
	if job.Kind != store.ModelKindVideo || job.Backend != mediagen.BackendGrok {
		t.Fatalf("任务行 = %+v", job)
	}
	select {
	case in := <-p.calls:
		if in.Inputs.FirstFrame != dataURI || in.Inputs.LastFrame != "https://example.invalid/last.png" || len(in.Inputs.ReferenceImages) != 2 || in.Inputs.ReferenceImages[1] != dataURI {
			t.Fatalf("后端收到的输入 = %+v", in.Inputs)
		}
		audio := in.Params.Bool("generate_audio")
		if d, ok := in.Params.Int("duration"); !ok || d != 10 || in.Params.String("resolution") != "720p" || audio == nil || *audio || len(in.Params.Strings("voices")) != 1 || in.Params.Strings("voices")[0] != "eve" {
			t.Fatalf("后端收到的参数 = %+v", in.Params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("后台任务没有调用后端")
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("iVBORw0KGgo")) || bytes.Contains(raw, []byte("last.png")) {
		t.Fatalf("任务行不该带媒体输入: %s", raw)
	}

	// Codex 图像：参考图与输出格式被受理并交给后端，任务行只记格式与参考图数。
	codexBody := `{"model":"gpt-image-2.5-flare","prompt":"p","inputs":{"reference_images":["` + dataURI + `"]},"params":{"output_format":"png","aspect_ratio":"16:9","background":"transparent"}}`
	codexJob := submitMedia(t, e, cookie, withKey(owner, codexBody)).Jobs[0]
	if codexJob.KeyID != owner || codexJob.KeyDisplay != mediaOwnerDisplay {
		t.Fatalf("任务行的归属 = key %d/%q", codexJob.KeyID, codexJob.KeyDisplay)
	}
	if codexJob.Params["output_format"] != "png" || codexJob.Params["aspect_ratio"] != "16:9" || codexJob.Params["background"] != "transparent" || codexJob.Inputs[store.MediaRoleReferenceImages] != 1 {
		t.Fatalf("Codex 任务行的快照 = %+v / %+v", codexJob.Params, codexJob.Inputs)
	}
	select {
	case in := <-p.calls:
		if in.Model.Backend != mediagen.BackendCodex || in.Params.String("output_format") != "png" || in.Params.String("background") != "transparent" ||
			len(in.Inputs.ReferenceImages) != 1 || in.Inputs.ReferenceImages[0] != dataURI {
			t.Fatalf("后端收到的 Codex 请求 = %+v / %+v", in.Inputs, in.Params)
		}
		// 归属：选中的密钥与它钉死的 Codex 账号一路传到后端（用量记在这把密钥头上）。
		if in.KeyID != owner || in.KeyDisplay != mediaOwnerDisplay || in.AccountID == 0 {
			t.Fatalf("后端收到的归属 = key %d/%q account %d", in.KeyID, in.KeyDisplay, in.AccountID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Codex 后台任务没有调用后端")
	}
	close(p.release)

	for _, body := range []string{
		`{"model":"gpt-image-2","prompt":"p","inputs":{"first_frame":"` + dataURI + `"}}`,
		`{"model":"gpt-image-2","prompt":"p","params":{"output_format":"gif"}}`,
		`{"model":"grok-imagine-image-2.0","prompt":"p","params":{"output_format":"png"}}`,
		`{"model":"grok-imagine-image-2.0","prompt":"p","inputs":{"first_frame":"` + dataURI + `"}}`,
		`{"model":"grok-imagine-video-1.5","prompt":"p","inputs":{"first_frame":"http://example.invalid/a.png"}}`,
		`{"model":"grok-imagine-video-1.5","prompt":"p","params":{"duration":20}}`,
		`{"model":"grok-imagine-video-1.5","prompt":"p","params":{"duration":2.5}}`,
		`{"model":"grok-imagine-video-1.5","prompt":"p","operation":"edit"}`,
		`{"model":"grok-imagine-video","prompt":"p","operation":"extend"}`,
		`{"model":"grok-imagine-image-2.0","prompt":"p","params":{"resolution":"4k"}}`,
		`{"model":"gpt-image-2","prompt":"p","params":{"aspect_ratio":"5:2"}}`,
		`{"model":"gpt-image-2","prompt":"p","params":{"resolution":"2k"}}`,
		// 能力表表达不了的搭配由后端的 Validate 钩子补判，同样是 400 media_invalid。
		`{"model":"gpt-image-2","prompt":"p","params":{"background":"transparent","output_format":"jpeg"}}`,
	} {
		resp := e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s → %d，期望 400", body, resp.StatusCode)
		}
		if code := errCode(t, resp); code != "media_invalid" {
			t.Fatalf("%s → 错误码 %q，期望 media_invalid", body, code)
		}
	}
	// 带首帧时视频的提示词可省。
	submitMedia(t, e, cookie, withKey(owner, `{"model":"grok-imagine-video-1.5","inputs":{"first_frame":"`+dataURI+`"}}`))
}

// 管理面提交必须选一把 API 密钥，且这把密钥要获授权用这份订阅：判据与 Key 持有人
// 那条路同一份策略快照（错误码也同一套），被拒的提交不落库、不起后台任务。
func TestMediaCreateRequiresGrantedAPIKey(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(mediagen.Result{})
	e.srv.SetMediaGateway(p)
	codexID, grokID := mediaAccounts(t, e)
	owner := mediaKey(t, e, "生成", "e", "sk_media001", "9999", codexID, grokID)
	const grokBody = `{"model":"grok-imagine-image-2.0","prompt":"p"}`
	const codexBody = `{"model":"gpt-image-2","prompt":"p"}`

	bare := mediaKey(t, e, "无订阅", "f", "sk_bare0001", "0000", 0, 0)
	off := mediaKey(t, e, "已停用", "a", "sk_off00001", "1111", codexID, grokID)
	if err := e.st.SetAPIKeyDisabled(t.Context(), off, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	archived := mediaKey(t, e, "已归档", "b", "sk_arch0001", "2222", codexID, grokID)
	if err := e.st.ArchiveAPIKey(t.Context(), archived); err != nil {
		t.Fatalf("ArchiveAPIKey: %v", err)
	}
	// 钉着的 Codex 账号被管理员停用：订阅当前不可用（Grok 那份照常）。
	if err := e.st.SetAgentStatus(t.Context(), codexID, store.AgentStatusDisabled); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	for _, tc := range []struct {
		name, body, code string
		want             int
	}{
		{"未选密钥", grokBody, "media_key_required", http.StatusBadRequest},
		{"密钥不存在", withKey(owner+9999, grokBody), "not_found", http.StatusNotFound},
		{"密钥已停用", withKey(off, grokBody), "key_disabled", http.StatusConflict},
		{"密钥已归档", withKey(archived, grokBody), "key_archived", http.StatusConflict},
		{"未获授权该订阅", withKey(bare, grokBody), "subscription_not_allowed", http.StatusForbidden},
		{"订阅当前不可用", withKey(owner, codexBody), "agent_not_configured", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do("POST", "/admin/v1/media/jobs", cookie, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("状态 = %d，期望 %d；body: %s", resp.StatusCode, tc.want, readAll(t, resp))
			}
			if code := errCode(t, resp); code != tc.code {
				t.Fatalf("错误码 = %q，期望 %q", code, tc.code)
			}
		})
	}
	if n := countMediaJobs(t, e); n != 0 {
		t.Fatalf("被拒的提交不该落库：%d 条", n)
	}
	if len(p.calls) != 0 || p.admitted.Load() != 0 {
		t.Fatalf("被拒的提交不该起后台任务 / 过准入闸：calls=%d admitted=%d", len(p.calls), p.admitted.Load())
	}
	close(p.release)
}

// 一次提交出多个候选：同批任务共用 batch_id、各自独立到终态；并发闸按「未到终态 + 本次候选数」
// 判，超出 mediagen.RunningPerKey 整批拒绝（429 media_busy），不建任何任务；全部来源合计。
func TestMediaCreateCountAndBusy(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(pngResult)
	e.srv.SetMediaGateway(p)
	codexID, grokID := mediaAccounts(t, e)
	owner := mediaKey(t, e, "生成", "e", "sk_media001", "9999", codexID, grokID)
	other := mediaKey(t, e, "另一把", "c", "sk_other001", "3333", 0, grokID)
	const one = `{"model":"grok-imagine-image-2.0","prompt":"p"}`
	const two = `{"model":"grok-imagine-image-2.0","prompt":"p","count":2}`

	batch := submitMedia(t, e, cookie, withKey(owner, two))
	if len(batch.Jobs) != 2 || batch.BatchID == "" || len(batch.BatchID) != 26 {
		t.Fatalf("count=2 → batch %q、%d 条", batch.BatchID, len(batch.Jobs))
	}
	if a, b := batch.Jobs[0], batch.Jobs[1]; a.BatchID != batch.BatchID || b.BatchID != batch.BatchID || a.ID == b.ID ||
		a.Status != store.MediaStatusRunning || b.Status != store.MediaStatusRunning {
		t.Fatalf("同批任务 = %+v / %+v", a, b)
	}
	if p.admitted.Load() != 2 {
		t.Fatalf("准入闸应每个候选过一次，得到 %d", p.admitted.Load())
	}
	// 名额只剩 1：count=2 整批拒绝，库里不多任何任务。
	resp := e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, two))
	if resp.StatusCode != http.StatusTooManyRequests || errCode(t, resp) != "media_busy" {
		t.Fatalf("名额不够的 count=2 = %d，期望 429 media_busy", resp.StatusCode)
	}
	if n := countMediaJobs(t, e); n != 2 {
		t.Fatalf("整批拒绝后库里有 %d 条，期望仍是 2", n)
	}
	if p.admitted.Load() != 2 {
		t.Fatalf("并发闸拒绝的提交不该过准入闸：%d", p.admitted.Load())
	}
	// 剩下的一个名额照常受理；再来一个 429。
	if last := submitMedia(t, e, cookie, withKey(owner, one)); last.BatchID != "" || len(last.Jobs) != 1 {
		t.Fatalf("第三个任务 = %+v", last)
	}
	resp = e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, one))
	if resp.StatusCode != http.StatusTooManyRequests || errCode(t, resp) != "media_busy" {
		t.Fatalf("超出并发上限 = %d，期望 429 media_busy", resp.StatusCode)
	}
	if n := countMediaJobs(t, e); n != mediagen.RunningPerKey {
		t.Fatalf("库里有 %d 条，期望 %d", n, mediagen.RunningPerKey)
	}
	// 平台返回：同批两条各自落成各自的文件；名额腾出来后又能提交。
	close(p.release)
	for _, j := range batch.Jobs {
		got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+j.ID+"/wait", cookie, `{}`))
		if got.Status != store.MediaStatusSucceeded || got.MediaFile != j.ID+".png" || got.BatchID != batch.BatchID {
			t.Fatalf("同批任务终态 = %+v", got)
		}
	}
	if full := submitMedia(t, e, cookie, withKey(owner, `{"model":"grok-imagine-image-2.0","prompt":"p","count":2}`)); len(full.Jobs) != 2 {
		t.Fatalf("名额腾出后的 count=2 = %+v", full)
	}

	// 创作工作空间 / gate media 的未到终态任务同样占这把密钥的名额。
	now := time.Now()
	for i, origin := range []string{store.MediaOriginStudio, store.MediaOriginCLI, store.MediaOriginStudio} {
		if err := e.st.CreateMediaJob(t.Context(), store.MediaJob{ID: store.NewULID(now.Add(time.Duration(i) * time.Millisecond)), Origin: origin,
			Backend: mediagen.BackendGrok, Provider: mediagen.BackendGrok, KeyID: other, Kind: store.ModelKindImage, Model: "grok-imagine-image-2.0",
			Operation: store.MediaOpGenerate, Prompt: "p", Status: store.MediaStatusRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMediaJob: %v", err)
		}
	}
	resp = e.do("POST", "/admin/v1/media/jobs", cookie, withKey(other, one))
	if resp.StatusCode != http.StatusTooManyRequests || errCode(t, resp) != "media_busy" {
		t.Fatalf("其它来源占满名额后 = %d，期望 429 media_busy", resp.StatusCode)
	}
}

// mustGrokAccount 取库里已有的那份 Grok 订阅账号 id。
func mustGrokAccount(t *testing.T, e *env) int64 {
	t.Helper()
	accounts, err := e.st.ListAgentAccounts(t.Context())
	if err != nil {
		t.Fatalf("ListAgentAccounts: %v", err)
	}
	for _, a := range accounts {
		if a.Provider == store.AgentProviderGrok {
			return a.ID
		}
	}
	t.Fatal("库里没有 Grok 订阅账号")
	return 0
}

// 受理时过数据面同一道准入闸：被拒答 429 + 闸给的错误码与 Retry-After，不建任何任务；
// 同批候选任一被拒则整批不建。
func TestMediaCreateAdmissionRejected(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := newFakeMedia(pngResult)
	var seen []string
	p.admit = func(keyID int64, m mediagen.Model) error {
		seen = append(seen, jsonNumber(keyID)+"/"+m.ID)
		return &mediagen.RejectedError{Code: "rate_limited", Msg: "请求过于频繁", RetryAfterSec: 7}
	}
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)

	resp := e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"p"}`))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("准入拒绝 = %d Retry-After=%q，期望 429 / 7", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if code := errCode(t, resp); code != "rate_limited" {
		t.Fatalf("错误码 = %q，期望 rate_limited", code)
	}
	if len(seen) != 1 || seen[0] != jsonNumber(owner)+"/gpt-image-2" {
		t.Fatalf("准入闸收到 = %v", seen)
	}
	// 预算类拒绝不带 Retry-After；第二个候选被拒时整批不建。
	calls := 0
	p.mu.Lock()
	p.admit = func(int64, mediagen.Model) error {
		if calls++; calls == 2 {
			return &mediagen.RejectedError{Code: "budget_exceeded", Msg: "本月预算已用完"}
		}
		return nil
	}
	p.mu.Unlock()
	resp = e.do("POST", "/admin/v1/media/jobs", cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"p","count":2}`))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("预算拒绝 = %d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if code := errCode(t, resp); code != "budget_exceeded" {
		t.Fatalf("错误码 = %q，期望 budget_exceeded", code)
	}
	if n := countMediaJobs(t, e); n != 0 {
		t.Fatalf("准入拒绝不该建任务：%d 条", n)
	}
	if len(p.calls) != 0 {
		t.Fatal("准入拒绝不该起后台任务")
	}
	if jobs := listMediaJobs(t, e, cookie); len(jobs) != 0 {
		t.Fatalf("列表 = %+v", jobs)
	}
}

// 后台生成出错（后端返回 error）时任务判失败，原因写进任务行。
func TestMediaBackgroundErrorMarksFailed(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	p := readyMedia(mediagen.Result{})
	p.err = errors.New("订阅未连接或已失效")
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)
	job := submitMedia(t, e, cookie, withKey(owner, `{"model":"grok-imagine-image-2.0","prompt":"p"}`)).Jobs[0]
	got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/wait", cookie, `{}`))
	if got.Status != store.MediaStatusFailed || got.Error != "订阅未连接或已失效" || got.FinishedAt == nil {
		t.Fatalf("陪等结果 = %+v，期望 failed 且带原因", got)
	}
}

// 平台先受理再异步生成：任务落成 queued 并记平台任务 ID；「查询平台任务」由人发起、立刻向平台
// 查一次——平台仍在排队时行不变，平台到终态时结果取回落盘；已到终态的行直接回当前帧。
func TestMediaRefreshQueuedJob(t *testing.T) {
	old := mediagen.PollInterval
	mediagen.PollInterval = 20 * time.Millisecond
	t.Cleanup(func() { mediagen.PollInterval = old })
	e := newEnv(t)
	cookie := e.rootSession()
	p := readyMedia(mediagen.Result{Status: store.MediaStatusQueued, VendorID: "req-42"})
	p.setRefresh(mediagen.Result{Status: store.MediaStatusQueued})
	e.srv.SetMediaGateway(p)
	owner := mediaOwner(t, e)
	job := submitMedia(t, e, cookie, withKey(owner, `{"model":"grok-imagine-video-1.5","prompt":"walk"}`)).Jobs[0]

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := decodeMediaJob(t, e.do("GET", "/admin/v1/media/jobs/"+job.ID, cookie, ""))
		if got.Status == store.MediaStatusQueued && got.VendorID == "req-42" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务没有落成 queued：%+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp := e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/refresh", cookie, `{}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeMediaJob(t, resp); got.Status != store.MediaStatusQueued {
		t.Fatalf("平台仍在排队时 refresh = %+v，期望保持 queued", got)
	}
	if p.refreshed.Load() == 0 {
		t.Fatal("refresh 没有向平台查询")
	}
	mp4 := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("not-really-mp4"))
	p.setRefresh(mediagen.Result{Status: store.MediaStatusSucceeded, MediaURL: mp4, MediaType: "video"})
	resp = e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/refresh", cookie, `{}`)
	wantStatus(t, resp, http.StatusOK)
	got := decodeMediaJob(t, resp)
	if got.Status != store.MediaStatusSucceeded || got.MediaFile != job.ID+".mp4" || got.MediaURL != "" || got.VendorID != "req-42" {
		t.Fatalf("平台到终态后 refresh = %+v", got)
	}
	if body, err := os.ReadFile(filepath.Join(e.dir, mediagen.MediaDirName, got.MediaFile)); err != nil || string(body) != "not-really-mp4" {
		t.Fatalf("结果文件 = %q / %v", body, err)
	}
	// 终态后再查：直接回当前帧，不再打平台。
	before := p.refreshed.Load()
	if again := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/refresh", cookie, `{}`)); again.Status != store.MediaStatusSucceeded {
		t.Fatalf("终态 refresh = %+v", again)
	}
	time.Sleep(60 * time.Millisecond)
	if p.refreshed.Load() != before {
		t.Fatal("任务到终态后不该再向平台查询")
	}
	resp = e.do("POST", "/admin/v1/media/jobs/NOPE/refresh", cookie, `{}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的任务 refresh = %d，期望 404", resp.StatusCode)
	}
}

// 进程重启后没人再更新上次留下的 running 任务：管理面装配时把它们判失败。
func TestMediaRunningJobsFailOnStartup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.st.CreateMediaJob(ctx, store.MediaJob{ID: "media-1", Origin: store.MediaOriginPage, Backend: "codex", Provider: "codex", Kind: "image", Model: "gpt-image-2", Status: store.MediaStatusRunning}); err != nil {
		t.Fatalf("CreateMediaJob: %v", err)
	}
	if err := e.st.CreateMediaJob(ctx, store.MediaJob{ID: "media-2", Origin: store.MediaOriginPage, Backend: "grok", Provider: "grok", Kind: "video", Model: "v", Status: store.MediaStatusQueued, VendorID: "req-1"}); err != nil {
		t.Fatalf("CreateMediaJob: %v", err)
	}
	// 用同一个库再装配一次管理面，相当于进程重启。
	admin.New(&config.Config{}, slog.New(slog.DiscardHandler), e.st, e.as, sysinfo.NewCollector(), nil, nil, "", nil)
	got, err := e.st.GetMediaJob(ctx, "media-1")
	if err != nil || got.Status != store.MediaStatusFailed || got.Error == "" {
		t.Fatalf("重启后 running 任务 = %+v / %v，期望 failed 且带原因", got, err)
	}
	queued, err := e.st.GetMediaJob(ctx, "media-2")
	if err != nil || queued.Status != store.MediaStatusQueued {
		t.Fatalf("平台侧排队的任务不该被动：%+v / %v", queued, err)
	}
}

// 任务 ID 是 ULID，下载文件名就是「ID.扩展名」；单条删除与清空都把记录与结果文件移除并记审计，
// 删除不存在的任务答 404。
func TestMediaJobIDIsULIDAndDeleteClear(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	e.srv.SetMediaGateway(readyMedia(pngResult))
	owner := mediaOwner(t, e)

	var ids []string
	for i := 0; i < 3; i++ {
		job := submitMedia(t, e, cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"一只小猫"}`)).Jobs[0]
		if _, ok := store.ULIDTime(job.ID); !ok || len(job.ID) != 26 {
			t.Fatalf("任务 ID = %q，期望 26 位 ULID", job.ID)
		}
		got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+job.ID+"/wait", cookie, `{}`))
		if got.Status != store.MediaStatusSucceeded {
			t.Fatalf("陪等结果 = %+v", got)
		}
		ids = append(ids, job.ID)
	}

	dl := e.do("GET", "/admin/v1/media/jobs/"+ids[0]+"/download", cookie, "")
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("下载状态 = %d；body: %s", dl.StatusCode, readAll(t, dl))
	}
	if cd := dl.Header.Get("Content-Disposition"); cd != "attachment; filename="+ids[0]+".png" {
		t.Fatalf("Content-Disposition = %q，期望以任务 ID 命名", cd)
	}
	if ct := dl.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := dl.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", cc)
	}
	if body := readAll(t, dl); body != "ABC" {
		t.Fatalf("下载正文 = %q，期望解码后的 data URI", body)
	}

	// 内联读取给页面 <img> 用：同一份文件，inline 而非 attachment，支持 Range。
	inline := e.do("GET", "/admin/v1/media/jobs/"+ids[0]+"/media", cookie, "")
	if inline.StatusCode != http.StatusOK || inline.Header.Get("Content-Disposition") != "inline; filename="+ids[0]+".png" || inline.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("内联状态 = %d disp=%q ranges=%q", inline.StatusCode, inline.Header.Get("Content-Disposition"), inline.Header.Get("Accept-Ranges"))
	}
	for _, suffix := range []string{"/download", "/media", "/thumb"} {
		resp := e.do("GET", "/admin/v1/media/jobs/NOPE"+suffix, cookie, "")
		if resp.StatusCode != http.StatusNotFound || errCode(t, resp) != "media_not_found" {
			t.Fatalf("不存在任务的 %s = %d，期望 404 media_not_found", suffix, resp.StatusCode)
		}
	}
	mediaPath := filepath.Join(e.dir, mediagen.MediaDirName, ids[0]+".png")
	if _, err := os.Stat(mediaPath); err != nil {
		t.Fatalf("结果文件应在数据目录：%v", err)
	}

	resp := e.do("DELETE", "/admin/v1/media/jobs/"+ids[0], cookie, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除状态 = %d；body: %s", resp.StatusCode, readAll(t, resp))
	}
	var deleted struct{ Deleted int64 }
	decodeInto(t, resp, &deleted)
	if deleted.Deleted != 1 {
		t.Fatalf("删除返回 = %+v，期望 deleted=1", deleted)
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("删除任务后结果文件应一并删除：%v", err)
	}
	if again := e.do("DELETE", "/admin/v1/media/jobs/"+ids[0], cookie, ""); again.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除状态 = %d，期望 404", again.StatusCode)
	}
	if wait := e.do("POST", "/admin/v1/media/jobs/"+ids[0]+"/wait", cookie, `{}`); wait.StatusCode != http.StatusNotFound {
		t.Fatalf("已删任务陪等状态 = %d，期望 404", wait.StatusCode)
	}
	if n := countMediaJobs(t, e); n != 2 {
		t.Fatalf("删除后剩 %d 条，期望 2", n)
	}
	// 审计：只有成功的那一次删除留痕，entity 是任务 id，不带提示词。
	if n, entity, detail := countAudit(t, e, "media.job.delete"); n != 1 || entity != "media:"+ids[0] || strings.Contains(detail, "小猫") {
		t.Fatalf("media.job.delete 审计 = %d 条 entity=%q detail=%q", n, entity, detail)
	}

	clear := e.do("DELETE", "/admin/v1/media/jobs", cookie, "")
	if clear.StatusCode != http.StatusOK {
		t.Fatalf("清空状态 = %d；body: %s", clear.StatusCode, readAll(t, clear))
	}
	var out struct{ Deleted int64 }
	if err := json.NewDecoder(clear.Body).Decode(&out); err != nil || out.Deleted != 2 {
		t.Fatalf("清空返回 = %+v / %v，期望 deleted=2", out, err)
	}
	if n := countMediaJobs(t, e); n != 0 {
		t.Fatalf("清空后剩 %d 条", n)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName)); len(left) != 0 {
		t.Fatalf("清空后结果目录应为空，剩 %d 个文件", len(left))
	}
	if n, entity, detail := countAudit(t, e, "media.jobs.clear"); n != 1 || entity != "media:*" || detail != "count=2" {
		t.Fatalf("media.jobs.clear 审计 = %d 条 entity=%q detail=%q", n, entity, detail)
	}
}

// 页面列表只列 origin=page：创作工作空间与 gate media 的任务不进列表，只能凭 id 访问；
// 清空也只清页面任务，不动它们的行与结果文件。
func TestMediaListAndClearOnlyPageJobs(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	e.srv.SetMediaGateway(readyMedia(pngResult))
	owner := mediaOwner(t, e)
	page := submitMedia(t, e, cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"页面的"}`)).Jobs[0]
	if got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+page.ID+"/wait", cookie, `{}`)); got.Status != store.MediaStatusSucceeded {
		t.Fatalf("页面任务 = %+v", got)
	}
	now := time.Now()
	studioJob := store.MediaJob{ID: store.NewULID(now), Origin: store.MediaOriginStudio, Owner: `{"workspace_id":"ws","chat_id":"chat","name":"hero"}`,
		Backend: "codex", Provider: "codex", KeyID: owner, KeyDisplay: mediaOwnerDisplay, Kind: store.ModelKindImage, Model: "gpt-image-2",
		Operation: store.MediaOpGenerate, Prompt: "工作空间的", Status: store.MediaStatusSucceeded, MediaType: "image", CreatedAt: now, UpdatedAt: now}
	studioJob.MediaFile = studioJob.ID + ".png"
	cliJob := studioJob
	cliJob.ID, cliJob.Origin, cliJob.Owner, cliJob.Prompt = store.NewULID(now.Add(time.Millisecond)), store.MediaOriginCLI, "", "命令行的"
	cliJob.MediaFile = cliJob.ID + ".png"
	for _, j := range []store.MediaJob{studioJob, cliJob} {
		if err := e.st.CreateMediaJob(t.Context(), j); err != nil {
			t.Fatalf("CreateMediaJob: %v", err)
		}
		if err := os.WriteFile(filepath.Join(e.dir, mediagen.MediaDirName, j.MediaFile), []byte("XYZ"), 0o600); err != nil {
			t.Fatalf("写结果文件: %v", err)
		}
	}

	jobs := listMediaJobs(t, e, cookie)
	if len(jobs) != 1 || jobs[0].ID != page.ID || jobs[0].Origin != store.MediaOriginPage {
		t.Fatalf("列表 = %+v，期望只有页面任务", jobs)
	}
	// 凭 id 仍取得到；owner 不给页面。
	resp := e.do("GET", "/admin/v1/media/jobs/"+studioJob.ID, cookie, "")
	wantStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	if !strings.Contains(raw, `"origin":"studio"`) || strings.Contains(raw, "workspace_id") {
		t.Fatalf("凭 id 读创作空间任务 = %s", raw)
	}
	if resp := e.do("GET", "/admin/v1/media/jobs/"+cliJob.ID+"/media", cookie, ""); resp.StatusCode != http.StatusOK || readAll(t, resp) != "XYZ" {
		t.Fatalf("凭 id 取 cli 任务的结果 = %d", resp.StatusCode)
	}

	clear := e.do("DELETE", "/admin/v1/media/jobs", cookie, "")
	wantStatus(t, clear, http.StatusOK)
	var out struct{ Deleted int64 }
	decodeInto(t, clear, &out)
	if out.Deleted != 1 {
		t.Fatalf("清空返回 deleted=%d，期望 1（只有页面任务）", out.Deleted)
	}
	for _, j := range []store.MediaJob{studioJob, cliJob} {
		if got, err := e.st.GetMediaJob(t.Context(), j.ID); err != nil || got.Origin != j.Origin {
			t.Fatalf("清空不该删 %s 任务：%+v / %v", j.Origin, got, err)
		}
		if _, err := os.Stat(filepath.Join(e.dir, mediagen.MediaDirName, j.MediaFile)); err != nil {
			t.Fatalf("清空不该删 %s 任务的结果文件：%v", j.Origin, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, mediagen.MediaDirName, page.ID+".png")); !os.IsNotExist(err) {
		t.Fatalf("页面任务的结果文件应随清空删除：%v", err)
	}
}

// 管理员看得到各把 API 密钥发起的任务（带发起密钥展示串），也能删除它们；
// 启动时目录里没有任务行引用的文件（半成品、残留）被清掉。
func TestMediaAdminSeesAndDeletesKeyJobs(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	e.srv.SetMediaGateway(readyMedia(mediagen.Result{Status: store.MediaStatusSucceeded, MediaURL: "data:image/png;base64,QUJD", MediaType: "image"}))
	_, grokID := mediaAccounts(t, e)
	holder := mediaKey(t, e, "持有人", "d", "sk_abcd", "wxyz", 0, grokID)
	// 模拟 Key 持有人经数据面提交：同一份内核，只是归属由数据面按自证的 Key 填。
	svc := e.srv.MediaJobs()
	prepared, err := svc.Check(context.Background(), mediagen.Submission{Model: "grok-imagine-image-2.0", Prompt: "持有人的",
		KeyID: holder, KeyDisplay: "sk_abcd…wxyz", Origin: store.MediaOriginPage})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	submitted, err := svc.Submit(context.Background(), prepared)
	if err != nil || len(submitted) != 1 {
		t.Fatalf("Submit: %v / %d", err, len(submitted))
	}
	keyJob := submitted[0]
	got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+keyJob.ID+"/wait", cookie, `{}`))
	if got.Status != store.MediaStatusSucceeded || got.KeyID != holder || got.KeyDisplay != "sk_abcd…wxyz" || got.MediaFile == "" || got.AccountID != grokID {
		t.Fatalf("管理员陪等 Key 任务 = %+v", got)
	}
	if jobs := listMediaJobs(t, e, cookie); len(jobs) != 1 || jobs[0].KeyDisplay != "sk_abcd…wxyz" {
		t.Fatalf("管理员列表 = %+v，期望含 Key 任务及发起密钥", jobs)
	}
	// 目录里塞一个没有任务行引用的残留文件：删除任务后的清扫把它一并带走。
	orphan := filepath.Join(e.dir, mediagen.MediaDirName, "01ORPHAN00000000000000000.png")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatalf("写残留文件: %v", err)
	}
	mediaPath := filepath.Join(e.dir, mediagen.MediaDirName, got.MediaFile)
	if resp := e.do("DELETE", "/admin/v1/media/jobs/"+keyJob.ID, cookie, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("管理员删除 Key 任务 = %d；body: %s", resp.StatusCode, readAll(t, resp))
	}
	for _, path := range []string{mediaPath, orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s 应已删除：%v", filepath.Base(path), err)
		}
	}
	// 再装配一次（进程重启）：新的残留文件在启动清扫时被清掉。
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatalf("写残留文件: %v", err)
	}
	admin.New(&config.Config{DataDir: e.dir}, slog.New(slog.DiscardHandler), e.st, e.as, sysinfo.NewCollector(), nil, nil, "", nil)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("启动清扫应删掉残留文件：%v", err)
	}
}

func mediaPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 图像结果落盘时生成缩略图，…/thumb 内联提供 JPEG；列表 JSON 里旧行的 data URI 只留头
// 不带正文；视频的封面帧由页面回传 …/thumb，设备缩放后保存并回更新后的任务行。
func TestMediaThumbnails(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaPNG(t, 1024, 2048))
	e.srv.SetMediaGateway(readyMedia(mediagen.Result{Status: store.MediaStatusSucceeded, MediaURL: uri, MediaType: "image"}))
	owner := mediaOwner(t, e)
	created := submitMedia(t, e, cookie, withKey(owner, `{"model":"gpt-image-2","prompt":"一只小猫"}`)).Jobs[0]
	got := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+created.ID+"/wait", cookie, `{}`))
	if got.Status != store.MediaStatusSucceeded || got.MediaFile != created.ID+".png" || got.ThumbFile != created.ID+mediagen.ThumbExt || got.MediaURL != "" {
		t.Fatalf("任务 = %+v", got)
	}
	resp := e.do("GET", "/admin/v1/media/jobs/"+created.ID+"/thumb", cookie, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("缩略图 = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	cfg, err := jpeg.DecodeConfig(resp.Body)
	if err != nil || cfg.Width != 480 || cfg.Height != 960 {
		t.Fatalf("缩略图尺寸 = %+v err=%v，期望 480×960", cfg, err)
	}
	// 早于本地文件的旧行：结果仍是 data URI。列表 JSON 只带头不带正文，媒体经 …/media 取。
	now := time.Now()
	legacy := store.MediaJob{ID: "01LEGACY0000000000000000A", Origin: store.MediaOriginPage, Backend: "codex", Provider: "codex", Kind: "image", Model: "m", Prompt: "p", Status: store.MediaStatusSucceeded, MediaURL: "data:image/png;base64,QUJD", MediaType: "image", CreatedAt: now, UpdatedAt: now}
	if err := e.st.CreateMediaJob(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	body := readAll(t, e.do("GET", "/admin/v1/media/jobs", cookie, ""))
	if !strings.Contains(body, `"media_url":"data:image/png;base64,"`) || strings.Contains(body, "QUJD") {
		t.Fatalf("列表 JSON 不该带 base64 正文: %s", body)
	}
	if resp := e.do("GET", "/admin/v1/media/jobs/"+legacy.ID+"/media", cookie, ""); resp.StatusCode != http.StatusOK || readAll(t, resp) != "ABC" {
		t.Fatalf("旧行媒体 = %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/admin/v1/media/jobs/"+legacy.ID+"/thumb", cookie, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("没有缩略图 = %d，期望 404", resp.StatusCode)
	}
	// 视频：页面回传封面帧。
	video := store.MediaJob{ID: "01VIDEO00000000000000000A", Origin: store.MediaOriginPage, Backend: "grok", Provider: "grok", Kind: "video", Model: "m", Prompt: "p", Status: store.MediaStatusSucceeded, MediaFile: "01VIDEO00000000000000000A.mp4", MediaType: "video", CreatedAt: now, UpdatedAt: now}
	if err := e.st.CreateMediaJob(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, mediagen.MediaDirName, video.MediaFile), []byte("notavideo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp := e.do("POST", "/admin/v1/media/jobs/"+video.ID+"/thumb", cookie, `{"image":"bm90IGFuIGltYWdl"}`); resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "media_invalid" {
		t.Fatalf("非图像封面帧 = %d，期望 400 media_invalid", resp.StatusCode)
	}
	if resp := e.do("POST", "/admin/v1/media/jobs/"+video.ID+"/thumb", cookie, `{"image":"***"}`); resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "invalid_request" {
		t.Fatalf("非 base64 = %d，期望 400 invalid_request", resp.StatusCode)
	}
	frame := base64.StdEncoding.EncodeToString(mediaPNG(t, 1280, 720))
	saved := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+video.ID+"/thumb", cookie, `{"image":"`+frame+`"}`))
	if saved.ThumbFile != video.ID+mediagen.ThumbExt {
		t.Fatalf("回传封面帧后 = %+v", saved)
	}
	resp = e.do("GET", "/admin/v1/media/jobs/"+video.ID+"/thumb", cookie, "")
	if cfg, err := jpeg.DecodeConfig(resp.Body); resp.StatusCode != http.StatusOK || err != nil || cfg.Width != 853 || cfg.Height != 480 {
		t.Fatalf("视频封面缩略图 = %d %+v err=%v", resp.StatusCode, cfg, err)
	}
	// 已有缩略图时再回传不覆盖，回当前行。
	if again := decodeMediaJob(t, e.do("POST", "/admin/v1/media/jobs/"+video.ID+"/thumb", cookie, `{"image":"`+frame+`"}`)); again.ThumbFile != saved.ThumbFile {
		t.Fatalf("重复回传 = %+v", again)
	}
	if resp := e.do("POST", "/admin/v1/media/jobs/NOPE/thumb", cookie, `{"image":"`+frame+`"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在任务 = %d，期望 404", resp.StatusCode)
	}
	// 删除任务连带删掉缩略图。
	if resp := e.do("DELETE", "/admin/v1/media/jobs/"+video.ID, cookie, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除 = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(e.dir, mediagen.MediaDirName, saved.ThumbFile)); !os.IsNotExist(err) {
		t.Fatalf("缩略图应随任务删除：%v", err)
	}
}
