package studio

// 工作空间的媒体生成能力：管理员显式指定哪些生成模型能在这个空间里用（白名单，没配即不能
// 生成），并可给每个模型写一段「什么情况下用它」的说明。配置属于工作空间，不属于对话：
//
//   - 生成工具每次调用都按当前配置裁决（generate.go pickModel），改完立即生效；
//   - 给引擎看的模型说明不存进对话行：引擎会话启动时按「当前配置 ∩ 对话密钥此刻可用的模型」
//     渲染一节附在开发者指令后面；会话还开着时配置变了，下一条指令执行前把新的一节作为设备
//     通知放在指令前面交给引擎（session.go execute），时间线留一条 session 事件。
//
// 说明文字是给模型看的，也显示在配置对话框里；缺省说明（DefaultUsage）只是对话框里的起点，
// 存下来的才作数。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	// MaxUsageRunes 是一个模型使用场景说明的长度上限。
	MaxUsageRunes = 400
	// MaxMediaModels 是一个工作空间启用的模型数上限。
	MaxMediaModels = 64
	// CodeChatArchived：对话已归档，不再接受指令（409）。
	CodeChatArchived = "agent_chat_archived"
	// sessionMediaUpdated 是「已把新的媒体生成能力告诉引擎」那条 session 事件的 title。
	sessionMediaUpdated = "media_updated"
)

// defaultUsages 是订阅后端固定预设的缺省使用场景说明；厂商面模型按后端给（familyUsages）。
var defaultUsages = map[string]string{
	"gpt-image-2":               "图像首选：海报、信息图、界面稿、带文字排版的画面，以及要求严格遵循指令或保持参考图主体一致的改图；需要透明背景时用它。",
	"gpt-image-2.5-flare":       "gpt-image-2 的备选：同一提示词想对比另一种出图风格时使用。",
	"gpt-image-2.5-sunburst":    "gpt-image-2 的备选：同一提示词想对比另一种出图风格时使用。",
	"grok-imagine-image-2.0":    "写实摄影、人物与氛围感画面；出图快，适合探索阶段一次多出几个候选，也适合需要特殊比例（如 21:9、9:19.5）的画面。",
	mediagen.GrokVideoModel:     "视频首选：文生视频，或用首帧 / 尾帧 / 参考图生成视频，可带音轨，单段至多 15 秒。",
	mediagen.GrokVideoEditModel: "只在编辑或延长已有视频（MP4）时使用；新生成视频用 grok-imagine-video-1.5。",
}

var familyUsages = map[string]string{
	mediagen.BackendArkImage:     "按量计费：成稿需要 2K / 4K 高分辨率或画面里有大段中文文字时使用；探索与草图阶段优先用订阅模型。",
	mediagen.BackendArkVideo:     "按量计费：订阅视频模型满足不了要求（固定镜头、自适应比例、指定随机种子复现）时使用；先用低分辨率、短时长试一版，确认后再出成片。",
	mediagen.BackendMinimaxVideo: "按量计费：管理员点名要用它，或订阅视频模型不可用时使用；resolution 与 duration 必填，先用低分辨率试一版。",
}

// DefaultUsage 给出一个模型的缺省使用场景说明（没有即空串）。
func DefaultUsage(m mediagen.Model) string {
	if u, ok := defaultUsages[m.ID]; ok {
		return u
	}
	return familyUsages[m.Backend]
}

// MediaModel 是配置对话框里的一行：能力表项 + 这个空间是否启用、存下来的说明与缺省说明。
type MediaModel struct {
	mediagen.Model
	Enabled      bool   `json:"enabled"`
	Usage        string `json:"usage"`
	DefaultUsage string `json:"default_usage" i18n:"text"`
	// Missing：启用过、但设备上已经没有这个模型了（厂商面模型行被删）；保存时会被丢掉。
	Missing bool `json:"missing,omitempty"`
}

// MediaConfig 读一个工作空间的媒体生成能力：设备能力表里的全部模型（不看任何密钥）各带
// 启用状态，后面跟着启用过但已不存在的。
func (m *Manager) MediaConfig(ctx context.Context, ws *store.Workspace) ([]MediaModel, error) {
	var catalog []mediagen.Model
	if m.media != nil {
		var err error
		if catalog, err = m.media.Catalog(ctx); err != nil {
			return nil, err
		}
	}
	rows, err := m.st.ListStudioMediaModels(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	enabled := make(map[string]string, len(rows))
	for _, r := range rows {
		enabled[r.Model] = r.Usage
	}
	out := make([]MediaModel, 0, len(catalog)+len(rows))
	for _, c := range catalog {
		usage, on := enabled[c.ID]
		delete(enabled, c.ID)
		out = append(out, MediaModel{Model: c, Enabled: on, Usage: usage, DefaultUsage: DefaultUsage(c)})
	}
	for _, r := range rows {
		if usage, left := enabled[r.Model]; left {
			out = append(out, MediaModel{Model: mediagen.Model{ID: r.Model, Operations: []mediagen.Operation{}}, Enabled: true, Usage: usage, Missing: true})
		}
	}
	return out, nil
}

// SetMediaConfig 整份替换一个工作空间启用的生成模型。模型须在设备能力表里，不重复；说明
// 去首尾空白、不超 MaxUsageRunes。存下即生效：生成工具按它裁决，开着的引擎会话在下一条指令
// 执行前收到新的模型说明。
func (m *Manager) SetMediaConfig(ctx context.Context, ws *store.Workspace, models []store.StudioMediaModel) error {
	if m.media == nil {
		return &Error{Code: CodeUnavailable, Msg: "本设备未接入媒体生成"}
	}
	if len(models) > MaxMediaModels {
		return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("一个工作空间最多启用 %d 个生成模型", MaxMediaModels)}
	}
	catalog, err := m.media.Catalog(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(catalog))
	for _, c := range catalog {
		known[c.ID] = true
	}
	seen := make(map[string]bool, len(models))
	clean := make([]store.StudioMediaModel, 0, len(models))
	for _, in := range models {
		in.Model, in.Usage = strings.TrimSpace(in.Model), strings.TrimSpace(in.Usage)
		switch {
		case !known[in.Model]:
			return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("设备上没有生成模型 %s", in.Model)}
		case seen[in.Model]:
			return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("生成模型 %s 重复", in.Model)}
		case len([]rune(in.Usage)) > MaxUsageRunes:
			return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("使用场景说明最多 %d 个字符", MaxUsageRunes)}
		}
		seen[in.Model] = true
		clean = append(clean, in)
	}
	if err := m.st.ReplaceStudioMediaModels(ctx, ws.ID, clean); err != nil {
		return err
	}
	m.log.Info("已更新工作空间的媒体生成能力", "workspace_id", ws.ID, "enabled", len(clean))
	return nil
}

// usableModel 是一个对话此刻能用的生成模型：空间启用了、对话的密钥也可用。
type usableModel struct {
	mediagen.Model
	Usage string
}

// usableModels 给出「空间启用 ∩ 密钥可用」的模型（内核裁决的顺序），以及这个空间启用了几个。
func (m *Manager) usableModels(ctx context.Context, workspaceID string, keyID int64) ([]usableModel, int, error) {
	rows, err := m.st.ListStudioMediaModels(ctx, workspaceID)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 || m.media == nil {
		return nil, len(rows), nil
	}
	enabled := make(map[string]string, len(rows))
	for _, r := range rows {
		enabled[r.Model] = r.Usage
	}
	avail, err := m.media.Available(ctx, keyID)
	if err != nil {
		return nil, 0, err
	}
	var out []usableModel
	for _, a := range avail {
		if usage, on := enabled[a.ID]; on && a.Available {
			out = append(out, usableModel{Model: a.Model, Usage: usage})
		}
	}
	return out, len(rows), nil
}

// mediaSection 渲染给引擎看的「可用的生成模型」一节（prompt.md 的 media 模板）。
func (m *Manager) mediaSection(ctx context.Context, workspaceID string, keyID int64) (string, error) {
	models, _, err := m.usableModels(ctx, workspaceID, keyID)
	if err != nil {
		return "", err
	}
	return renderMediaSection(models), nil
}

func renderMediaSection(models []usableModel) string {
	type entry struct{ Describe, Usage string }
	entries := make([]entry, 0, len(models))
	for _, u := range models {
		entries = append(entries, entry{Describe: mediagen.Describe([]mediagen.Model{u.Model}), Usage: strings.Join(strings.Fields(u.Usage), " ")})
	}
	var b strings.Builder
	if err := promptTemplate.ExecuteTemplate(&b, "media", entries); err != nil {
		panic(err)
	}
	return b.String()
}

// mediaUpdateInput 把新的模型说明作为设备通知放在管理员指令前面（prompt.md 的 media_update 模板）。
func mediaUpdateInput(section, text string) string {
	var b strings.Builder
	if err := promptTemplate.ExecuteTemplate(&b, "media_update", struct{ Section, Text string }{section, text}); err != nil {
		panic(err)
	}
	return b.String()
}

// ArchiveChat 归档一个对话：结束它的会话（中止执行中的、取消排队的、关引擎），之后只能查看、
// 不再接受指令。重复归档无害。
func (m *Manager) ArchiveChat(ctx context.Context, workspaceID, chatID string) (*store.StudioChat, error) {
	chat, err := m.GetChat(ctx, workspaceID, chatID)
	if err != nil {
		return nil, err
	}
	if err := m.st.ArchiveStudioChat(ctx, chat.ID, m.now()); err != nil {
		if isNotFound(err) {
			return nil, &Error{Code: CodeChatNotFound, Msg: "对话不存在"}
		}
		return nil, err
	}
	if s := m.peekSession(chat.ID); s != nil {
		_ = s.stop(ctx)
		s.waitIdle(5 * time.Second)
	}
	m.log.Info("已归档对话", "workspace_id", workspaceID, "chat_id", chat.ID)
	return m.GetChat(ctx, workspaceID, chatID)
}
