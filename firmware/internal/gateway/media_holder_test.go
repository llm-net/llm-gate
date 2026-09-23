package gateway_test

// 凭 API 密钥自证的媒体生成端点（/gate-helper/v1/media/*）的可执行验收：模型可用性按这把
// Key 裁决（订阅未钉 403、钉了但不可用 409、钉了即用钉死账号）、任务按 Key 归属（别人的与
// 无归属的一律 404）、每把 Key 的并发名额（整批受理或整批拒绝）、来源（页面 page / gate
// cli）与列表可见性、请求体里的 key_id 不作数、候选数落成同批任务。
//
// 装配与生产同形：内核 mediagen.Service 经 SetMediaJobs 注入网关；订阅授权用与数据面同一份
// devtoolpolicy 快照（生产在 admin.Server.mediaEntitlements）；后端换成不出网的假后端。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// fakeMediaBackend 是不出网的生成后端：记下每次请求，按 release 决定立刻回还是挂住。
// calls / release 可在多个后端名之间共用（同一份假后端顶 grok 与 codex 两个名字）。
type fakeMediaBackend struct {
	name    string
	calls   chan mediagen.Request
	release chan struct{}
}

func newFakeMediaBackends(block bool) (grok, codex *fakeMediaBackend) {
	calls := make(chan mediagen.Request, 16)
	var release chan struct{}
	if block {
		release = make(chan struct{})
	}
	return &fakeMediaBackend{name: mediagen.BackendGrok, calls: calls, release: release},
		&fakeMediaBackend{name: mediagen.BackendCodex, calls: calls, release: release}
}

func (g *fakeMediaBackend) Name() string                    { return g.name }
func (g *fakeMediaBackend) Validate(mediagen.Request) error { return nil }

func (g *fakeMediaBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	g.calls <- in
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return mediagen.Result{}, ctx.Err()
		}
	}
	return mediagen.Result{AccountID: in.AccountID, Status: store.MediaStatusSucceeded,
		MediaURL: "data:image/png;base64,QUJD", MediaType: "image"}, nil
}

func (g *fakeMediaBackend) Refresh(context.Context, store.MediaJob) (mediagen.Result, error) {
	return mediagen.Result{}, nil
}

// mediaEntitlementsFor 是生产 admin.Server.mediaEntitlements 的等价物：按开发工具策略快照
// 给出一把 Key 对各订阅的授权，与数据面 /agents/<tool>/ 同一份判据。
func mediaEntitlementsFor(e *routeEnv) mediagen.Entitlements {
	resolver := &devtoolpolicy.Resolver{
		Store:          e.st,
		AgentModels:    e.srv.AgentSubscriptionModels,
		PlatformModels: func(context.Context) platformcatalog.Doc { return platformcatalog.Builtin() },
	}
	return func(ctx context.Context, keyID int64) (map[string]mediagen.Entitlement, error) {
		snapshot, err := resolver.Snapshot(ctx, keyID)
		if err != nil {
			return nil, err
		}
		out := map[string]mediagen.Entitlement{}
		for _, provider := range []string{store.AgentProviderGrok, store.AgentProviderCodex} {
			sub := snapshot.Subscription(provider)
			out[provider] = mediagen.Entitlement{Configured: sub.Configured, Available: sub.Available, AccountID: sub.AccountID}
		}
		return out, nil
	}
}

// wireMedia 给路由环境接上任务内核与给定后端。
func wireMedia(t *testing.T, e *routeEnv, backends ...mediagen.Backend) *mediagen.Service {
	t.Helper()
	svc := mediagen.New(e.st, logging.New(&bytes.Buffer{}, slog.LevelDebug), http.DefaultClient, e.dir)
	svc.SetEntitlements(mediaEntitlementsFor(e))
	svc.SetBackends(backends...)
	e.srv.SetMediaJobs(svc)
	return svc
}

// wireFakeMedia 接上顶 grok / codex 两个名字的假后端，返回内核与假后端（calls / release 共用）。
func wireFakeMedia(t *testing.T, e *routeEnv, block bool) (*mediagen.Service, *fakeMediaBackend) {
	t.Helper()
	grok, codex := newFakeMediaBackends(block)
	return wireMedia(t, e, grok, codex), grok
}

const (
	mediaJobsPath   = "/gate-helper/v1/media/jobs"
	mediaSubmitBody = `{"model":"grok-imagine-image-2.0","prompt":"一只小猫"}`
)

// gateAuth 是 gate 发出的请求头：同一把 Key，多一个客户端标识头。
var gateAuth = map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json", "X-LLMGate-Client": "gate/2609171200-abcd"}

type mediaSubmitResp struct {
	BatchID string           `json:"batch_id"`
	Jobs    []store.MediaJob `json:"jobs"`
}

func decodeMediaSubmit(t *testing.T, body []byte) mediaSubmitResp {
	t.Helper()
	var out mediaSubmitResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解码受理响应: %v (%s)", err, body)
	}
	return out
}

// submitOneMediaJob 提交一条并返回受理的任务行（期望 202、恰一条、batch_id 为空）。
func submitOneMediaJob(t *testing.T, e *routeEnv, header map[string]string, body string) store.MediaJob {
	t.Helper()
	w := do(e.h, http.MethodPost, mediaJobsPath, header, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("提交 = %d %s，期望 202", w.Code, w.Body.String())
	}
	out := decodeMediaSubmit(t, w.Body.Bytes())
	if len(out.Jobs) != 1 || out.BatchID != "" || out.Jobs[0].BatchID != "" {
		t.Fatalf("受理响应 = %s，期望恰一条且 batch_id 为空", w.Body.String())
	}
	return out.Jobs[0]
}

func decodeMediaJob(t *testing.T, body []byte) store.MediaJob {
	t.Helper()
	var job store.MediaJob
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("解码任务: %v (%s)", err, body)
	}
	return job
}

func listKeyMediaJobs(t *testing.T, e *routeEnv, header map[string]string) []store.MediaJob {
	t.Helper()
	w := do(e.h, http.MethodGet, mediaJobsPath, header, "")
	var list struct {
		Jobs []store.MediaJob `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK || list.Jobs == nil {
		t.Fatalf("列表 = %d %s err=%v，期望 200 且 jobs 是数组", w.Code, w.Body.String(), err)
	}
	return list.Jobs
}

// countMediaJobsOfKey 数某把 Key 未到终态的任务（全部来源合计）：断言「被拒的提交不落库」
// 与并发名额用。
func countMediaJobsOfKey(t *testing.T, e *routeEnv, keyID int64) int64 {
	t.Helper()
	n, err := e.st.CountActiveMediaJobsByKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("CountActiveMediaJobsByKey: %v", err)
	}
	return n
}

func importOtherKey(t *testing.T, e *routeEnv) int64 {
	t.Helper()
	if _, _, err := gateway.ImportConfigKeys(t.Context(), e.st, []config.APIKey{{Key: otherKey}}, logging.New(io.Discard, slog.LevelDebug)); err != nil {
		t.Fatalf("导入第二把 Key: %v", err)
	}
	ka, err := e.st.LookupKeyByDigest(t.Context(), digestOf(otherKey))
	if err != nil {
		t.Fatalf("LookupKeyByDigest(otherKey): %v", err)
	}
	return ka.KeyID
}

func TestKeyMediaRequiresPinnedSubscription(t *testing.T) {
	e := newRouteEnv(t)
	if w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, mediaSubmitBody); w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte("media_unavailable")) {
		t.Fatalf("未接线应答 503 media_unavailable，得 %d: %s", w.Code, w.Body.String())
	}
	_, gen := wireFakeMedia(t, e, false)

	if w := do(e.h, http.MethodPost, mediaJobsPath, nil, mediaSubmitBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 应答 401，得 %d", w.Code)
	}
	// 设备上有 Grok 订阅但没钉给这把 Key：403，且不落任务。
	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	if err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, mediaSubmitBody)
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("subscription_not_allowed")) {
		t.Fatalf("未授权订阅 = %d %s，期望 403 subscription_not_allowed", w.Code, w.Body.String())
	}
	if n := countMediaJobsOfKey(t, e, testKeyID(t, e.st)); n != 0 {
		t.Fatalf("被拒的提交不该落库：%d 条", n)
	}
	// 钉上之后：202 running，后台用的正是钉死的那一行账号，任务记发起 Key。
	pinSubscription(t, e.st, store.AgentProviderGrok, acct.ID)
	w = do(e.h, http.MethodPost, mediaJobsPath, chatAuth, mediaSubmitBody)
	if w.Code != http.StatusAccepted || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("提交 = %d cache=%q %s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	out := decodeMediaSubmit(t, w.Body.Bytes())
	if len(out.Jobs) != 1 || out.BatchID != "" {
		t.Fatalf("受理响应 = %s，期望一条任务、batch_id 为空", w.Body.String())
	}
	job := out.Jobs[0]
	if job.Status != store.MediaStatusRunning || job.KeyID != testKeyID(t, e.st) || job.KeyDisplay != testKeyDisplay || job.AccountID != acct.ID {
		t.Fatalf("受理任务 = %+v", job)
	}
	if job.Origin != store.MediaOriginPage || job.Backend != mediagen.BackendGrok || job.Provider != mediagen.BackendGrok ||
		job.Kind != store.ModelKindImage || job.Model != "grok-imagine-image-2.0" || job.Operation != store.MediaOpGenerate || len(job.ID) != 26 {
		t.Fatalf("受理任务 = %+v，期望按模型解析出后端 / 种类且操作缺省 generate", job)
	}
	select {
	case in := <-gen.calls:
		if in.AccountID != acct.ID || in.Model.Backend != mediagen.BackendGrok || in.Model.ID != "grok-imagine-image-2.0" || in.Prompt != "一只小猫" {
			t.Fatalf("后台请求 = %+v，期望钉死账号 %d", in, acct.ID)
		}
		if in.KeyID != job.KeyID || in.KeyDisplay != testKeyDisplay || in.JobID != job.ID {
			t.Fatalf("后台请求归属 = %d / %q / %q，期望发起 Key 与任务 ID", in.KeyID, in.KeyDisplay, in.JobID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("后台没有发起生成")
	}
	w = do(e.h, http.MethodPost, mediaJobsPath+"/"+job.ID+"/wait", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("wait = %d %s", w.Code, w.Body.String())
	}
	if got := decodeMediaJob(t, w.Body.Bytes()); got.Status != "succeeded" || got.MediaFile != got.ID+".png" || got.FinishedAt == nil {
		t.Fatalf("wait 结果 = %+v，期望 succeeded 且结果已落盘", got)
	}
	// Codex 没钉：同一把 Key 换模型仍 403。
	w = do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"gpt-image-2","prompt":"x"}`)
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("subscription_not_allowed")) {
		t.Fatalf("未授权 Codex = %d %s", w.Code, w.Body.String())
	}
	// 能力表里没有的模型：404 model_not_found。
	w = do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"no-such-model","prompt":"x"}`)
	if w.Code != http.StatusNotFound || !bytes.Contains(w.Body.Bytes(), []byte("model_not_found")) {
		t.Fatalf("未知模型 = %d %s，期望 404 model_not_found", w.Code, w.Body.String())
	}
	// 请求体不是 JSON：400 invalid_request；旧形状（provider / kind）里没有 model：400 media_invalid。
	if w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{`); w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("invalid_request")) {
		t.Fatalf("坏 JSON = %d %s，期望 400 invalid_request", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"provider":"grok","kind":"image","prompt":"x"}`); w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("media_invalid")) {
		t.Fatalf("缺 model = %d %s，期望 400 media_invalid", w.Code, w.Body.String())
	}
	// 下载与内联读取都来自设备上的结果文件，文件名是 <ID>.png。
	w = do(e.h, http.MethodGet, mediaJobsPath+"/"+job.ID+"/download", chatAuth, "")
	if w.Code != http.StatusOK || w.Body.String() != "ABC" || w.Header().Get("Content-Disposition") != "attachment; filename="+job.ID+".png" {
		t.Fatalf("下载 = %d %q disp=%q", w.Code, w.Body.String(), w.Header().Get("Content-Disposition"))
	}
	w = do(e.h, http.MethodGet, mediaJobsPath+"/"+job.ID+"/media", chatAuth, "")
	if w.Code != http.StatusOK || w.Body.String() != "ABC" || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Disposition") != "inline; filename="+job.ID+".png" {
		t.Fatalf("内联 = %d %q ct=%q disp=%q", w.Code, w.Body.String(), w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"))
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+job.ID+"/media", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 读结果 = %d，期望 401", w.Code)
	}
	// 这组端点只挂在 /media 下，没有 /preview 别名。
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/preview/tasks", chatAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("/gate-helper/v1/preview/tasks = %d，期望 404", w.Code)
	}
}

// 钉了订阅但账号当前不可用（停用 / 凭据失效）：409 agent_not_configured，不落任务；
// models 读数里同一个模型给出同一个原因码。
func TestKeyMediaPinnedButUnavailableSubscription(t *testing.T) {
	e := newRouteEnv(t)
	wireFakeMedia(t, e, false)
	acct := connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	if err := e.st.SetAgentStatus(t.Context(), acct.ID, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, mediaSubmitBody)
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("agent_not_configured")) {
		t.Fatalf("订阅不可用 = %d %s，期望 409 agent_not_configured", w.Code, w.Body.String())
	}
	if n := countMediaJobsOfKey(t, e, testKeyID(t, e.st)); n != 0 {
		t.Fatalf("被拒的提交不该落库：%d 条", n)
	}
	for _, m := range keyMediaModels(t, e, chatAuth).Models {
		if m.Backend == mediagen.BackendGrok && (m.Available || m.ReasonCode != mediagen.ReasonAgentNotConfigured || m.Reason == "") {
			t.Fatalf("Grok 模型 %s 读数 = %+v，期望不可用 + agent_not_configured", m.ID, m)
		}
	}
}

type keyMediaModelsResp struct {
	RunningPerKey int `json:"running_per_key"`
	Models        []struct {
		ID         string               `json:"id"`
		Backend    string               `json:"backend"`
		Kind       string               `json:"kind"`
		Billing    string               `json:"billing"`
		Available  *bool                `json:"available"`
		ReasonCode *string              `json:"reason_code"`
		Reason     *string              `json:"reason"`
		Operations []mediagen.Operation `json:"operations"`
	} `json:"models"`
}

type keyMediaModelsView struct {
	RunningPerKey int
	Models        []mediagen.Availability
}

// keyMediaModels 读 GET models，并断言每个模型都带 available / reason_code / reason 三个键
// （页面与 gate 按键是否存在判形状，不能靠零值省略）。
func keyMediaModels(t *testing.T, e *routeEnv, header map[string]string) keyMediaModelsView {
	t.Helper()
	w := do(e.h, http.MethodGet, "/gate-helper/v1/media/models", header, "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("models = %d cache=%q %s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	var raw keyMediaModelsResp
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解码 models: %v (%s)", err, w.Body.String())
	}
	out := keyMediaModelsView{RunningPerKey: raw.RunningPerKey}
	for _, m := range raw.Models {
		if m.Available == nil || m.ReasonCode == nil || m.Reason == nil {
			t.Fatalf("模型 %s 缺 available / reason_code / reason 键: %s", m.ID, w.Body.String())
		}
		out.Models = append(out.Models, mediagen.Availability{
			Model:     mediagen.Model{ID: m.ID, Backend: m.Backend, Kind: m.Kind, Billing: m.Billing, Operations: m.Operations},
			Available: *m.Available, ReasonCode: *m.ReasonCode, Reason: *m.Reason,
		})
	}
	return out
}

// GET models 回并发名额与这把 Key 的能力表：每项带 available / reason_code，订阅按开发工具
// 策略裁决（钉了的可用、没钉的 subscription_not_allowed），读数不带订阅账号标识。
func TestKeyMediaModels(t *testing.T) {
	e := newRouteEnv(t)
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/media/models", chatAuth, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线 models = %d，期望 503", w.Code)
	}
	wireFakeMedia(t, e, false)
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/media/models", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key models = %d，期望 401", w.Code)
	}
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})

	view := keyMediaModels(t, e, chatAuth)
	if view.RunningPerKey != mediagen.RunningPerKey {
		t.Fatalf("running_per_key = %d，期望 %d", view.RunningPerKey, mediagen.RunningPerKey)
	}
	byID := map[string]mediagen.Availability{}
	for _, m := range view.Models {
		byID[m.ID] = m
	}
	for _, preset := range mediagen.Presets() {
		got, ok := byID[preset.ID]
		if !ok {
			t.Fatalf("models 缺预设 %s", preset.ID)
		}
		if got.Backend != preset.Backend || got.Kind != preset.Kind || got.Billing != mediagen.BillingSubscription || len(got.Operations) != len(preset.Operations) {
			t.Fatalf("模型 %s 读数 = %+v，与能力表不符", preset.ID, got)
		}
		switch preset.Backend {
		case mediagen.BackendGrok:
			if !got.Available || got.ReasonCode != "" {
				t.Fatalf("已钉的 Grok 模型 %s = %+v，期望可用", preset.ID, got)
			}
		case mediagen.BackendCodex:
			if got.Available || got.ReasonCode != mediagen.ReasonSubscriptionNotAllowed || got.Reason == "" {
				t.Fatalf("未钉的 Codex 模型 %s = %+v，期望 subscription_not_allowed", preset.ID, got)
			}
		}
	}
	// 视频预设的操作带输入角色与参数表（页面表单与 gate media models 都靠它）。
	video := byID[mediagen.GrokVideoEditModel]
	if _, ok := video.Operation(store.MediaOpExtend); !ok {
		t.Fatalf("%s 的操作 = %+v，期望含 extend", mediagen.GrokVideoEditModel, video.Operations)
	}
	gen, _ := video.Operation(store.MediaOpGenerate)
	if in, ok := gen.Input(store.MediaRoleReferenceImages); !ok || in.Max != mediagen.MaxReferenceImages {
		t.Fatalf("generate 的参考图上限 = %+v", in)
	}
	if p, ok := gen.Param("duration"); !ok || p.Type != mediagen.ParamInteger || p.Min != 1 || p.Max != 15 {
		t.Fatalf("generate 的 duration = %+v", p)
	}
	w := do(e.h, http.MethodGet, "/gate-helper/v1/media/models", chatAuth, "")
	if bytes.Contains(w.Body.Bytes(), []byte("account_id")) || bytes.Contains(w.Body.Bytes(), []byte("AccountID")) {
		t.Fatalf("models 读数不该带订阅账号标识: %s", w.Body.String())
	}
}

func TestKeyMediaJobsAreScopedToOwnerKey(t *testing.T) {
	e := newRouteEnv(t)
	wireFakeMedia(t, e, false)
	acct := connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	mine := submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/wait", chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("wait = %d %s", w.Code, w.Body.String())
	}
	otherID := importOtherKey(t, e)
	// 库里另有两条页面任务：一条没有归属密钥的历史行（KeyID 0），一条属于第二把 Key。
	// 持有人都看不见、等不到、删不掉。
	now := time.Now()
	legacy := store.MediaJob{ID: store.NewULID(now), Origin: store.MediaOriginPage, Backend: mediagen.BackendGrok, Provider: mediagen.BackendGrok,
		AccountID: acct.ID, Kind: store.ModelKindImage, Model: "grok-imagine-image-2.0", Operation: store.MediaOpGenerate,
		Prompt: "无归属的", Status: store.MediaStatusSucceeded, CreatedAt: now, UpdatedAt: now}
	theirs := legacy
	theirs.ID, theirs.KeyID, theirs.KeyDisplay, theirs.Prompt = store.NewULID(now.Add(time.Millisecond)), otherID, "sk_neigh…dcba", "别人的"
	if err := e.st.CreateMediaJobs(t.Context(), []store.MediaJob{legacy, theirs}); err != nil {
		t.Fatalf("CreateMediaJobs: %v", err)
	}

	if list := listKeyMediaJobs(t, e, chatAuth); len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("本 Key 列表 = %+v，期望只有自己的那条", list)
	}
	if list := listKeyMediaJobs(t, e, otherAuth); len(list) != 1 || list[0].ID != theirs.ID {
		t.Fatalf("另一把 Key 的列表 = %+v，期望只有它自己的那条", list)
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+mine.ID, chatAuth, ""); w.Code != http.StatusOK || decodeMediaJob(t, w.Body.Bytes()).ID != mine.ID {
		t.Fatalf("读自己的任务 = %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, ""},
		{http.MethodPost, "/wait"},
		{http.MethodPost, "/refresh"},
		{http.MethodGet, "/download"},
		{http.MethodGet, "/media"},
		{http.MethodGet, "/thumb"},
		{http.MethodDelete, ""},
	} {
		w := do(e.h, c.method, mediaJobsPath+"/"+mine.ID+c.path, otherAuth, "")
		if w.Code != http.StatusNotFound || !bytes.Contains(w.Body.Bytes(), []byte(`"not_found"`)) {
			t.Fatalf("另一把 Key %s %s = %d %s，期望 404 not_found", c.method, c.path, w.Code, w.Body.String())
		}
		// 无归属的历史行对任何 Key 都是 404。
		if w := do(e.h, c.method, mediaJobsPath+"/"+legacy.ID+c.path, chatAuth, ""); w.Code != http.StatusNotFound {
			t.Fatalf("无归属任务 %s %s = %d，期望 404", c.method, c.path, w.Code)
		}
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/01NOSUCHJOB", chatAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("不存在的任务 = %d，期望 404（与「不是你的」同一响应）", w.Code)
	}
	// 清空只清自己的：另外两条留存。
	w := do(e.h, http.MethodDelete, mediaJobsPath, chatAuth, "")
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"deleted":1`)) {
		t.Fatalf("清空 = %d %s", w.Code, w.Body.String())
	}
	all, err := e.st.ListPageMediaJobs(t.Context())
	if err != nil || len(all) != 2 {
		t.Fatalf("管理员视角剩余 = %+v err=%v，期望剩另外两条", all, err)
	}
	for _, j := range all {
		if j.ID == mine.ID {
			t.Fatalf("自己的任务应已清掉：%+v", all)
		}
	}
	// 单条删除：自己的可删。
	again := submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+again.ID+"/wait", chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("wait = %d", w.Code)
	}
	if w := do(e.h, http.MethodDelete, mediaJobsPath+"/"+again.ID, chatAuth, ""); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"deleted":1`)) {
		t.Fatalf("删除自己的任务 = %d %s", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+again.ID, chatAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("删除后再读 = %d，期望 404", w.Code)
	}
}

// 并发闸：每把 Key 同时最多 RunningPerKey 个未到终态的任务，全部来源合计；名额不够时
// 整批拒绝（running + count > RunningPerKey → 429 media_busy，一条都不建）。
func TestKeyMediaLimitsRunningJobsPerKey(t *testing.T) {
	e := newRouteEnv(t)
	_, gen := wireFakeMedia(t, e, true)
	defer close(gen.release)
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	keyID := testKeyID(t, e.st)
	// 先占两个名额：一条页面、一条 gate（来源合计）。
	submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	submitOneMediaJob(t, e, gateAuth, mediaSubmitBody)
	// 还剩 1 个名额，一次要 2 个候选：整批拒绝，不建任何任务。
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"grok-imagine-image-2.0","prompt":"一只小猫","count":2}`)
	if w.Code != http.StatusTooManyRequests || !bytes.Contains(w.Body.Bytes(), []byte("media_busy")) {
		t.Fatalf("名额不够的批量提交 = %d %s，期望 429 media_busy", w.Code, w.Body.String())
	}
	if n := countMediaJobsOfKey(t, e, keyID); n != 2 {
		t.Fatalf("整批拒绝后未到终态任务 = %d，期望仍是 2", n)
	}
	// 单条还放得下；放满之后再来一条就拒。
	submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	w = do(e.h, http.MethodPost, mediaJobsPath, chatAuth, mediaSubmitBody)
	if w.Code != http.StatusTooManyRequests || !bytes.Contains(w.Body.Bytes(), []byte("media_busy")) {
		t.Fatalf("第 4 次提交 = %d %s，期望 429 media_busy", w.Code, w.Body.String())
	}
	if n := countMediaJobsOfKey(t, e, keyID); n != mediagen.RunningPerKey {
		t.Fatalf("未到终态任务 = %d，期望 %d", n, mediagen.RunningPerKey)
	}
	// 候选数越界是参数错误，不是名额问题。
	w = do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"grok-imagine-image-2.0","prompt":"p","count":4}`)
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("media_invalid")) {
		t.Fatalf("count=4 = %d %s，期望 400 media_invalid", w.Code, w.Body.String())
	}
}

// count=2：同批两条任务，共用一个 batch_id，各自独立到终态、各有自己的结果文件。
func TestKeyMediaCountCreatesBatch(t *testing.T) {
	e := newRouteEnv(t)
	_, gen := wireFakeMedia(t, e, false)
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"grok-imagine-image-2.0","prompt":"一只小猫","count":2}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("提交 = %d %s", w.Code, w.Body.String())
	}
	out := decodeMediaSubmit(t, w.Body.Bytes())
	if len(out.Jobs) != 2 || len(out.BatchID) != 26 {
		t.Fatalf("受理响应 = %s，期望两条任务与 26 位 batch_id", w.Body.String())
	}
	if out.Jobs[0].ID == out.Jobs[1].ID {
		t.Fatalf("同批任务的 ID 不该相同：%s", out.Jobs[0].ID)
	}
	for _, j := range out.Jobs {
		if j.BatchID != out.BatchID || j.Status != store.MediaStatusRunning || j.KeyID != testKeyID(t, e.st) {
			t.Fatalf("同批任务 = %+v，期望共用 batch_id %s", j, out.BatchID)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-gen.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("后台只发起了 %d 次生成，期望 2", i)
		}
	}
	for _, j := range out.Jobs {
		w := do(e.h, http.MethodPost, mediaJobsPath+"/"+j.ID+"/wait", chatAuth, "")
		if got := decodeMediaJob(t, w.Body.Bytes()); w.Code != http.StatusOK || got.Status != "succeeded" || got.MediaFile != j.ID+".png" || got.BatchID != out.BatchID {
			t.Fatalf("wait %s = %d %+v", j.ID, w.Code, got)
		}
	}
	if list := listKeyMediaJobs(t, e, chatAuth); len(list) != 2 {
		t.Fatalf("列表 = %d 条，期望 2", len(list))
	}
}

// 来源：带 X-LLMGate-Client: gate/<版本> 头的提交记 cli，不进列表、只能凭 id 取（仍按 Key
// 归属）；不带头的记 page。清空只清页面任务，cli 任务留着由 gate 自己删。
func TestKeyMediaOriginCLIIsHiddenFromList(t *testing.T) {
	e := newRouteEnv(t)
	wireFakeMedia(t, e, false)
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	importOtherKey(t, e)

	page := submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	cli := submitOneMediaJob(t, e, gateAuth, mediaSubmitBody)
	if page.Origin != store.MediaOriginPage || cli.Origin != store.MediaOriginCLI {
		t.Fatalf("来源 = %q / %q，期望 page / cli", page.Origin, cli.Origin)
	}
	if list := listKeyMediaJobs(t, e, chatAuth); len(list) != 1 || list[0].ID != page.ID {
		t.Fatalf("列表 = %+v，期望只列页面任务", list)
	}
	// 列表不看请求头：gate 自己去列也只看得到页面任务。
	if list := listKeyMediaJobs(t, e, gateAuth); len(list) != 1 || list[0].ID != page.ID {
		t.Fatalf("gate 视角的列表 = %+v，期望只列页面任务", list)
	}
	w := do(e.h, http.MethodGet, mediaJobsPath+"/"+cli.ID, gateAuth, "")
	if got := decodeMediaJob(t, w.Body.Bytes()); w.Code != http.StatusOK || got.ID != cli.ID || got.Origin != store.MediaOriginCLI {
		t.Fatalf("凭 id 取 cli 任务 = %d %s", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+cli.ID, otherAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("另一把 Key 取 cli 任务 = %d，期望 404", w.Code)
	}
	w = do(e.h, http.MethodPost, mediaJobsPath+"/"+cli.ID+"/wait", gateAuth, "")
	if got := decodeMediaJob(t, w.Body.Bytes()); w.Code != http.StatusOK || got.Status != "succeeded" {
		t.Fatalf("wait cli 任务 = %d %s", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+cli.ID+"/download", gateAuth, ""); w.Code != http.StatusOK || w.Body.String() != "ABC" {
		t.Fatalf("下载 cli 任务 = %d %q", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+page.ID+"/wait", chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("wait 页面任务 = %d", w.Code)
	}
	w = do(e.h, http.MethodDelete, mediaJobsPath, chatAuth, "")
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"deleted":1`)) {
		t.Fatalf("清空 = %d %s，期望只清掉页面那一条", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+cli.ID, gateAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("清空后 cli 任务 = %d，期望仍在", w.Code)
	}
	if w := do(e.h, http.MethodDelete, mediaJobsPath+"/"+cli.ID, gateAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("gate 删自己的任务 = %d %s", w.Code, w.Body.String())
	}
	// 头不是 gate/ 开头的不算 gate。
	browser := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json", "X-LLMGate-Client": "browser/1"}
	if j := submitOneMediaJob(t, e, browser, mediaSubmitBody); j.Origin != store.MediaOriginPage {
		t.Fatalf("X-LLMGate-Client: browser/1 的来源 = %q，期望 page", j.Origin)
	}
}

// 请求体里的 key_id 是管理面那条路的字段：Key 持有人借它把任务与用量记到别人头上是不行的，
// 归属恒为认证身份；授权也按认证身份判（别人钉了订阅不等于我能用）。
func TestKeyMediaIgnoresBodyKeyID(t *testing.T) {
	e := newRouteEnv(t)
	_, gen := wireFakeMedia(t, e, false)
	acct := connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	otherID := importOtherKey(t, e)
	mineID := testKeyID(t, e.st)
	if otherID == mineID {
		t.Fatal("两把 Key 的行 id 不该相同")
	}
	for _, bodyKey := range []int64{otherID, 999999} {
		body, _ := json.Marshal(map[string]any{"key_id": bodyKey, "model": "grok-imagine-image-2.0", "prompt": "一只小猫"})
		job := submitOneMediaJob(t, e, chatAuth, string(body))
		if job.KeyID != mineID || job.KeyDisplay != testKeyDisplay || job.AccountID != acct.ID {
			t.Fatalf("key_id=%d 的提交落成 %+v，期望归属认证身份 %d", bodyKey, job, mineID)
		}
		select {
		case in := <-gen.calls:
			if in.KeyID != mineID || in.KeyDisplay != testKeyDisplay {
				t.Fatalf("后端收到的归属 = %d / %q，期望认证身份", in.KeyID, in.KeyDisplay)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("后台没有发起生成")
		}
		if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+job.ID+"/wait", chatAuth, ""); w.Code != http.StatusOK {
			t.Fatalf("wait = %d", w.Code)
		}
	}
	if list := listKeyMediaJobs(t, e, otherAuth); len(list) != 0 {
		t.Fatalf("第二把 Key 名下不该有任务：%+v", list)
	}
	// 反过来：第二把 Key 没钉订阅，写上我的 key_id 也借不到授权。
	body, _ := json.Marshal(map[string]any{"key_id": mineID, "model": "grok-imagine-image-2.0", "prompt": "x"})
	if w := do(e.h, http.MethodPost, mediaJobsPath, otherAuth, string(body)); w.Code != http.StatusForbidden {
		t.Fatalf("借别人的 key_id 提交 = %d %s，期望 403", w.Code, w.Body.String())
	}
}

// 缩略图端点同样按归属裁决：持有人读自己任务的缩略图、回传封面帧；别人的任务 404。
func TestKeyMediaThumbnails(t *testing.T) {
	e := newRouteEnv(t)
	wireFakeMedia(t, e, false)
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	mine := submitOneMediaJob(t, e, chatAuth, mediaSubmitBody)
	w := do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/wait", chatAuth, "")
	got := decodeMediaJob(t, w.Body.Bytes())
	// 假后端的结果 "ABC" 不是图像：设备生成不了缩略图，页面回传一帧。
	if got.Status != "succeeded" || got.ThumbFile != "" {
		t.Fatalf("任务 = %+v", got)
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+mine.ID+"/thumb", chatAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("无缩略图 = %d，期望 404", w.Code)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 640, 960))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	frame := `{"image":"` + base64.StdEncoding.EncodeToString(buf.Bytes()) + `"}`
	importOtherKey(t, e)
	if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/thumb", otherAuth, frame); w.Code != http.StatusNotFound {
		t.Fatalf("另一把 Key 回传 = %d，期望 404", w.Code)
	}
	for _, bad := range []string{`{}`, `{"image":"***"}`, `{`} {
		if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/thumb", chatAuth, bad); w.Code != http.StatusBadRequest {
			t.Fatalf("回传 %s = %d，期望 400", bad, w.Code)
		}
	}
	w = do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/thumb", chatAuth, frame)
	if saved := decodeMediaJob(t, w.Body.Bytes()); w.Code != http.StatusOK || saved.ThumbFile != mine.ID+mediagen.ThumbExt {
		t.Fatalf("回传封面帧 = %d %s", w.Code, w.Body.String())
	}
	w = do(e.h, http.MethodGet, mediaJobsPath+"/"+mine.ID+"/thumb", chatAuth, "")
	if cfg, err := jpeg.DecodeConfig(w.Body); w.Code != http.StatusOK || err != nil || cfg.Width != 480 || cfg.Height != 720 {
		t.Fatalf("缩略图 = %d %+v err=%v，期望 480×720", w.Code, cfg, err)
	}
	if w := do(e.h, http.MethodGet, mediaJobsPath+"/"+mine.ID+"/thumb", otherAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("另一把 Key 读缩略图 = %d，期望 404", w.Code)
	}
	if w := do(e.h, http.MethodPost, mediaJobsPath+"/"+mine.ID+"/thumb", chatAuth, `{"image":"bm90IGFuIGltYWdl"}`); w.Code != http.StatusOK {
		t.Fatalf("已有缩略图时再回传 = %d，期望 200 回当前行", w.Code)
	}
}

// Key 持有人的提交同样带得动首帧 / 尾帧 / 参考图与参数：按能力表校验后交给后端（参数已
// 类型化），任务行只记参数与输入形态快照、不记媒体。
func TestKeyMediaForwardsInputsAndParams(t *testing.T) {
	e := newRouteEnv(t)
	_, gen := wireFakeMedia(t, e, false)
	acct := connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	const dataURI = "data:image/png;base64,iVBORw0KGgo="
	body := `{"model":"grok-imagine-video-1.5","prompt":"walk","inputs":{"first_frame":"` + dataURI + `","last_frame":"https://example.invalid/last.png","reference_images":["` + dataURI + `"]},` +
		`"params":{"duration":15,"aspect_ratio":"9:16","generate_audio":false,"voices":["Eve","leo"]}}`
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("提交 = %d %s，期望 202", w.Code, w.Body.String())
	}
	select {
	case in := <-gen.calls:
		if in.Inputs.FirstFrame != dataURI || in.Inputs.LastFrame != "https://example.invalid/last.png" || len(in.Inputs.ReferenceImages) != 1 || in.AccountID != acct.ID {
			t.Fatalf("后端收到的输入 = %+v（账号 %d）", in.Inputs, in.AccountID)
		}
		if n, ok := in.Params.Int("duration"); !ok || n != 15 || in.Params.String("aspect_ratio") != "9:16" {
			t.Fatalf("后端收到的参数 = %+v，期望 duration 为 int64(15)", in.Params)
		}
		if b := in.Params.Bool("generate_audio"); b == nil || *b {
			t.Fatalf("generate_audio = %v，期望 false", b)
		}
		if v := in.Params.Strings("voices"); len(v) != 2 || v[0] != "eve" || v[1] != "leo" {
			t.Fatalf("voices = %v，期望归一成小写的字符串表", v)
		}
		if in.Model.Kind != store.ModelKindVideo || in.Operation != store.MediaOpGenerate {
			t.Fatalf("后端收到 = %s / %s", in.Model.Kind, in.Operation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("后台任务没有调用后端")
	}
	if bytes.Contains(w.Body.Bytes(), []byte("iVBORw0KGgo")) {
		t.Fatalf("任务行不该带媒体输入: %s", w.Body.String())
	}
	job := decodeMediaSubmit(t, w.Body.Bytes()).Jobs[0]
	if job.Inputs[store.MediaRoleFirstFrame] != 1 || job.Inputs[store.MediaRoleLastFrame] != 1 || job.Inputs[store.MediaRoleReferenceImages] != 1 {
		t.Fatalf("输入形态快照 = %+v", job.Inputs)
	}
	if job.Params["duration"] != float64(15) || job.Params["aspect_ratio"] != "9:16" || job.Params["generate_audio"] != false {
		t.Fatalf("参数快照 = %+v", job.Params)
	}
	for _, tc := range []struct{ name, body string }{
		{"图像模型带尾帧", `{"model":"grok-imagine-image-2.0","prompt":"p","inputs":{"last_frame":"` + dataURI + `"}}`},
		{"延长缺源视频", `{"model":"grok-imagine-video","prompt":"p","operation":"extend","params":{"duration":3}}`},
		{"1.5 不做延长", `{"model":"grok-imagine-video-1.5","prompt":"p","operation":"extend","inputs":{"source_video":"https://example.invalid/in.mp4"}}`},
		{"表里没有的参数", `{"model":"grok-imagine-image-2.0","prompt":"p","params":{"size":"1024x1024"}}`},
		{"枚举越界", `{"model":"grok-imagine-image-2.0","prompt":"p","params":{"resolution":"8k"}}`},
		{"整数带小数", `{"model":"grok-imagine-video-1.5","prompt":"p","params":{"duration":1.5}}`},
		{"提示词为空", `{"model":"grok-imagine-image-2.0","prompt":"  "}`},
		{"旧形状的顶层字段不作数", `{"model":"grok-imagine-video-1.5","image":"` + dataURI + `"}`},
	} {
		w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, tc.body)
		if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("media_invalid")) {
			t.Fatalf("%s = %d %s，期望 400 media_invalid", tc.name, w.Code, w.Body.String())
		}
	}
	// 带首帧时提示词可省。
	if w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"grok-imagine-video-1.5","inputs":{"first_frame":"`+dataURI+`"}}`); w.Code != http.StatusAccepted {
		t.Fatalf("只带首帧 = %d %s，期望 202", w.Code, w.Body.String())
	}
}
