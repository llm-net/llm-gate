package studio

// 生成：把 generate_image / generate_video 的入参翻成媒体生成内核（internal/mediagen）的一次
// 提交，以对话钉死的密钥名义调用订阅或上游平台——模型可用性、准入、并发上限与计量和页面、
// gate media 同一道闸。本层只管三件事：按文件名从目录里取输入、结果叫什么名字、附注写什么。
//
// 任务以 origin = studio 落库，owner 记着工作空间 / 对话 / 指令与目标文件基名，不进页面列表。
// 在同一次工具调用里陪等到终态，成功的经内核 Adopt 搬进工作空间的 media/ 目录（内核随即删任务
// 行与设备上的结果文件，结果只留目录里这一份），附注记下提示词 / 后端 / 模型 / 参数。
//
// 工具调用被引擎放弃（指令中止、引擎超时）时生成不会白做：后台接着等，落成后照样搬进目录并
// 在时间线留一条 generate 事件（adoptLater）。进程在「任务完成」与「搬进目录」之间重启时，
// 内核把这些任务交给 FinishJob 收尾——adoptLater 只是这条路径的快路。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studiomcp"
)

// jobOwner 是写进任务行 owner 列的归属：重启收尾时据它找回工作空间、对话、指令与目标文件基名
// （结果恒落在 media/ 之下）。
type jobOwner struct {
	WorkspaceID string `json:"workspace_id"`
	ChatID      string `json:"chat_id"`
	RunID       string `json:"run_id,omitempty"`
	Name        string `json:"name"`
}

// generateGrace 是工具调用之外继续等平台的上限（内核自己有 TaskTimeout）。
const generateGrace = mediagen.TaskTimeout + time.Minute

// generate 是两个生成工具的执行体。
func (s *session) generate(ctx context.Context, ws *store.Workspace, kind string, a studiomcp.GenerateArgs) mcpserve.Result {
	if s.m.media == nil {
		return mcpserve.TextResult("generation is not available on this device", true)
	}
	chat, err := s.m.st.GetStudioChat(ctx, s.chatID)
	if err != nil {
		return mcpserve.TextResult("the chat no longer exists", true)
	}
	model, models, err := s.m.pickModel(ctx, ws.ID, chat.KeyID, kind, a.Model)
	if err != nil {
		return toolErr(err)
	}
	// 媒体输入从目录里按名字读成 data URI（只在内存里经过）。
	inputs, err := s.mediaInputs(ws, a)
	if err != nil {
		return toolErr(err)
	}
	base := s.m.baseNameFor(a.Name, kind)
	owner, _ := json.Marshal(jobOwner{WorkspaceID: ws.ID, ChatID: s.chatID, RunID: s.currentRunID(), Name: base})
	prepared, err := s.m.media.Check(ctx, mediagen.Submission{Model: model, Operation: a.Operation, Prompt: a.Prompt, Inputs: inputs,
		Params: a.Params, Count: a.Count, KeyID: chat.KeyID, KeyDisplay: chat.KeyDisplay, Origin: store.MediaOriginStudio, Owner: string(owner)})
	if err != nil {
		return generateErr(err, models)
	}
	// 工具的种类与模型的种类必须一致：generate_video 不能拿图像模型出图，反之亦然。
	if got := prepared.Model().Kind; got != kind {
		return mcpserve.TextResult(fmt.Sprintf("%s is a %s model, not a %s model (models available: %s)", model, got, kind, strings.Join(models, ", ")), true)
	}
	label := "生成图像"
	if kind == store.ModelKindVideo {
		label = "生成视频"
	}
	s.setActivity(nil, label+"："+base)
	defer s.setActivity(nil, "")
	jobs, err := s.m.media.Submit(ctx, prepared)
	var busy *mediagen.BusyError
	if errors.As(err, &busy) {
		return mcpserve.TextResult(fmt.Sprintf("this API key already has %d generations running (limit %d); wait for them to finish and try again", busy.Running, mediagen.RunningPerKey), true)
	}
	if err != nil {
		return generateErr(err, models)
	}
	for _, j := range jobs {
		s.m.log.Info("已提交生成任务", "workspace_id", ws.ID, "chat_id", s.chatID, "job", j.ID, "backend", j.Backend, "kind", kind)
	}
	waitCtx, cancel := context.WithTimeout(ctx, generateGrace)
	defer cancel()
	var out mcpserve.Result
	for i, j := range jobs {
		final, err := s.m.waitJob(waitCtx, j.ID)
		if err != nil {
			if errors.Is(err, mediagen.ErrNotFound) {
				out.Content = append(out.Content, mcpserve.Text("the generation task was deleted before it finished"))
				out.IsError = true
				continue
			}
			// 工具调用被放弃（指令中止 / 引擎超时）：后台接着等，落成后搬进目录并留事件。
			for _, rest := range jobs[i:] {
				go s.m.adoptLater(rest.ID)
			}
			out.Content = append(out.Content, mcpserve.Text("the generation is still running in the background; the result will appear in the workspace when it finishes"))
			out.IsError = true
			return out
		}
		res := s.m.settleJob(context.WithoutCancel(ctx), final, true)
		out.Content = append(out.Content, res.Content...)
		out.IsError = out.IsError || res.IsError
	}
	return out
}

// generateErr 把内核的拒绝翻成给模型看的工具错误。
func generateErr(err error, models []string) mcpserve.Result {
	var (
		invalid     *mediagen.InvalidError
		unavailable *mediagen.UnavailableError
		rejected    *mediagen.RejectedError
	)
	switch {
	case errors.As(err, &invalid):
		return mcpserve.TextResult("invalid generation request: "+invalid.Msg+" (models available: "+strings.Join(models, ", ")+")", true)
	case errors.As(err, &unavailable):
		return mcpserve.TextResult("the model is not available for this chat's API key: "+unavailable.Msg, true)
	case errors.As(err, &rejected):
		return mcpserve.TextResult("the API key's quota rejected this generation ("+rejected.Code+"): "+rejected.Msg, true)
	case errors.Is(err, mediagen.ErrModelNotFound):
		return mcpserve.TextResult("unknown model (models available: "+strings.Join(models, ", ")+")", true)
	}
	return mcpserve.TextResult("failed to submit the generation: "+err.Error(), true)
}

// pickModel 按种类选模型，候选是「工作空间启用 ∩ 这把密钥可用」的那些：给了名字就须在其中
// （启用了但密钥不可用的交给内核裁决、答明原因），没给取第一个。同时回可用模型名，给出错时
// 的提示用。
func (m *Manager) pickModel(ctx context.Context, workspaceID string, keyID int64, kind, model string) (string, []string, error) {
	rows, err := m.st.ListStudioMediaModels(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	enabled := make(map[string]bool, len(rows))
	for _, r := range rows {
		enabled[r.Model] = true
	}
	avail, err := m.media.Available(ctx, keyID)
	if err != nil {
		return "", nil, err
	}
	var usable, reasons []string
	for _, a := range avail {
		if a.Kind != kind || !enabled[a.ID] {
			continue
		}
		if a.Available {
			usable = append(usable, a.ID)
		} else {
			reasons = append(reasons, a.ID+": "+a.Reason)
		}
	}
	if model = strings.TrimSpace(model); model != "" {
		if !enabled[model] {
			return "", nil, &Error{Code: CodeInvalidInput, Msg: model + " is not enabled in this workspace (models available: " + strings.Join(usable, ", ") + ")"}
		}
		return model, usable, nil
	}
	switch {
	case len(usable) > 0:
		return usable[0], usable, nil
	case len(reasons) > 0:
		return "", nil, &Error{Code: CodeInvalidInput, Msg: "no usable " + kind + " model for this chat's API key: " + strings.Join(reasons, "; ")}
	}
	return "", nil, &Error{Code: CodeInvalidInput, Msg: "no " + kind + " model is enabled in this workspace; the administrator enables models in the workspace's media generation settings"}
}

// settleJob 把一条终态任务收尾：成功的经内核 Adopt 搬进目录并留 generate 事件，失败的留失败
// 事件并删任务；withImage 为真时把图像结果交给模型看。任务已被另一条路径收走时静默返回。
func (m *Manager) settleJob(ctx context.Context, t *store.MediaJob, withImage bool) mcpserve.Result {
	var owner jobOwner
	if err := json.Unmarshal([]byte(t.Owner), &owner); err != nil || owner.WorkspaceID == "" {
		_ = m.media.Delete(ctx, t.ID)
		return mcpserve.TextResult("the generation task has no workspace", true)
	}
	ws, err := m.st.GetWorkspace(ctx, owner.WorkspaceID)
	if err != nil {
		// 工作空间已删：结果无处可去。
		_ = m.media.Delete(ctx, t.ID)
		return mcpserve.TextResult("the workspace no longer exists", true)
	}
	end := m.now()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	elapsed := end.Sub(t.CreatedAt).Milliseconds()
	event := func(title string, meta map[string]any) {
		meta["provider"], meta["model"], meta["kind"] = t.Backend, t.Model, t.Kind
		m.appendChatEvent(ctx, owner, store.StudioEvent{RunID: owner.RunID, Kind: store.StudioEventGenerate, Title: title, Body: t.Prompt,
			DurationMs: elapsed, Meta: metaJSON(meta)})
	}
	if t.Status != store.MediaStatusSucceeded {
		reason := t.Error
		if reason == "" {
			reason = "the platform reported " + t.Status
		}
		event("", map[string]any{"status": t.Status, "error": reason})
		_ = m.media.Delete(ctx, t.ID)
		return mcpserve.TextResult("generation failed: "+reason, true)
	}
	meta := FileMeta{Origin: store.StudioFileOriginGenerated, Provider: t.Backend, Model: t.Model, Prompt: t.Prompt, Params: paramsJSON(t),
		ChatID: owner.ChatID, RunID: owner.RunID}
	var f *store.StudioFile
	err = m.media.Adopt(ctx, t.ID, func(ctx context.Context, job *store.MediaJob, media *mediagen.Media) error {
		var err error
		f, err = m.adoptMedia(ctx, ws, job, media, owner.Name, meta)
		return err
	})
	if errors.Is(err, mediagen.ErrNotFound) {
		return mcpserve.TextResult("the generation result was already collected", true)
	}
	if err != nil {
		event("", map[string]any{"status": store.MediaStatusFailed, "error": err.Error()})
		return mcpserve.TextResult("the result could not be saved into the workspace: "+err.Error(), true)
	}
	event(f.Name, map[string]any{"status": store.MediaStatusSucceeded, "bytes": f.Bytes, "width": f.Width, "height": f.Height})
	m.log.Info("生成结果已保存到工作空间", "workspace_id", ws.ID, "chat_id", owner.ChatID, "file", f.Name, "kind", t.Kind, "duration_ms", elapsed)
	text := fmt.Sprintf("saved %s (%s/%s, %s", f.Name, t.Backend, t.Model, humanSize(f.Bytes))
	if f.Width > 0 {
		text += fmt.Sprintf(", %dx%d", f.Width, f.Height)
	}
	text += ")"
	content := []mcpserve.Content{mcpserve.Text(text)}
	if withImage && f.Kind == store.StudioFileImage {
		// 上限同 view_image：设备解得开的图像整份读进来缩放（thumbSourceLimit）。
		if raw, err := m.ReadFile(ctx, ws, f.Name, thumbSourceLimit); err == nil {
			if data, err := mediagen.MakeJPEG(bytes.NewReader(raw), viewShortEdgeLow, viewJPEGQuality); err == nil {
				content = append(content, mcpserve.Image(data, "image/jpeg"))
			}
		}
	}
	return mcpserve.Result{Content: content}
}

// appendChatEvent 往任务所属的对话时间线追加一条事件（经对话的会话，陪等中的页面随即醒来）；
// 对话已删时不留事件。
func (m *Manager) appendChatEvent(ctx context.Context, owner jobOwner, ev store.StudioEvent) {
	chat, err := m.st.GetStudioChat(ctx, owner.ChatID)
	if err != nil || chat.WorkspaceID != owner.WorkspaceID {
		return
	}
	m.session(chat).appendEvent(ctx, ev)
}

// adoptLater 在工具调用之外继续等一条任务，落成后搬进目录并留事件。
func (m *Manager) adoptLater(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), generateGrace)
	defer cancel()
	final, err := m.waitJob(ctx, jobID)
	if err != nil {
		m.log.Warn("后台等待生成任务失败", "job", jobID, "error", err.Error())
		return
	}
	m.settleJob(context.Background(), final, false)
}

// FinishJob 是注册给内核的收尾器（mediagen.Finisher）：进程重启后，上次进程留下的、已到终态
// 而尚未搬运的创作工作空间任务逐条到这里——成功的搬进目录并留 generate 事件，失败的留失败
// 事件，随后任务删除。
func (m *Manager) FinishJob(ctx context.Context, job store.MediaJob) {
	m.settleJob(ctx, &job, false)
}

// waitJob 陪等一条生成任务到终态。
func (m *Manager) waitJob(ctx context.Context, id string) (*store.MediaJob, error) {
	for {
		t, err := m.media.Wait(ctx, id)
		if err != nil {
			return nil, err
		}
		if !store.MediaStatusActive(t.Status) {
			return t, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// adoptMedia 把内核交出的结果写进 media/（文件名按 base + 结果扩展名去重），写附注与缩略图。
func (m *Manager) adoptMedia(ctx context.Context, ws *store.Workspace, t *store.MediaJob, media *mediagen.Media, base string, meta FileMeta) (*store.StudioFile, error) {
	ext := filepath.Ext(mediagen.DownloadName(t, media.ContentType))
	st, err := m.storage(ws)
	if err != nil {
		return nil, err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if base == "" {
		base = m.baseNameFor("", t.Kind)
	}
	name := m.uniqueName(ctx, st, DirMedia, base, ext)
	var body = media.Body
	if media.Content != nil {
		body = media.Content
	}
	return m.saveLocked(ctx, ws, st, JoinPath(DirMedia, name), body, MaxUploadBytes, meta)
}

// baseNameFor 把模型给的名字收窄成安全的基名（去目录、去扩展名、去掉不合法字符），空则自动命名。
func (m *Manager) baseNameFor(name, kind string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f:
			continue
		case r == ' ':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ".-_ ")
	if rs := []rune(out); len(rs) > 60 {
		out = string(rs[:60])
	}
	if out == "" {
		prefix := "image"
		if kind == store.ModelKindVideo {
			prefix = "video"
		}
		out = prefix + "-" + m.now().Local().Format("20060102-150405")
	}
	return out
}

// mediaInputs 把入参里的文件路径按角色读成 data URI。
func (s *session) mediaInputs(ws *store.Workspace, a studiomcp.GenerateArgs) (mediagen.Inputs, error) {
	var in mediagen.Inputs
	var err error
	if in.FirstFrame, err = s.imageDataURI(ws, a.FirstFrame); err != nil {
		return in, err
	}
	if in.LastFrame, err = s.imageDataURI(ws, a.LastFrame); err != nil {
		return in, err
	}
	for _, name := range a.ReferenceImages {
		uri, err := s.imageDataURI(ws, name)
		if err != nil {
			return in, err
		}
		if uri != "" {
			in.ReferenceImages = append(in.ReferenceImages, uri)
		}
	}
	if p := strings.TrimSpace(a.SourceVideo); p != "" {
		if !inMediaDir(p) || FileKind(p) != store.StudioFileVideo || strings.ToLower(filepath.Ext(p)) != ".mp4" {
			return in, &Error{Code: CodeFileInvalid, Msg: "the source video must be an .mp4 file in the workspace (media/<name>.mp4): " + p}
		}
		raw, err := s.m.ReadFile(context.Background(), ws, p, mediagen.MaxVideoInputBytes*3/4)
		if err != nil {
			return in, err
		}
		if !isMP4(raw) {
			return in, &Error{Code: CodeFileInvalid, Msg: "the source video is not an MP4 file (no ftyp box at the start): " + p}
		}
		in.SourceVideo = "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(raw)
	}
	return in, nil
}

func (s *session) imageDataURI(ws *store.Workspace, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if !inMediaDir(p) || FileKind(p) != store.StudioFileImage {
		return "", &Error{Code: CodeFileInvalid, Msg: "not an image file in the workspace (expected media/<name>.png|jpg|webp|gif): " + p}
	}
	raw, err := s.m.ReadFile(context.Background(), ws, p, mediagen.MaxInputBytes*3/4)
	if err != nil {
		return "", err
	}
	mimeType := MimeFor(p)
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// inMediaDir 报告路径 p 在 media/ 下：生成的文件输入按角色只从 media/ 读。
func inMediaDir(p string) bool {
	dir, _ := SplitPath(p)
	return dir == DirMedia
}

// isMP4 按 ISO BMFF 的开头判 MP4：第一个 box 是 ftyp。
func isMP4(raw []byte) bool {
	return len(raw) >= 12 && string(raw[4:8]) == "ftyp"
}

// paramsJSON 是附注里的参数快照：操作（非缺省时）与任务行的生成参数。
func paramsJSON(t *store.MediaJob) string {
	out := map[string]any{}
	for k, v := range t.Params {
		out[k] = v
	}
	if t.Operation != "" && t.Operation != store.MediaOpGenerate {
		out["operation"] = t.Operation
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
