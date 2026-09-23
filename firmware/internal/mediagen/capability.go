package mediagen

// capability.go 是模型知识的唯一真值：每个模型接受哪些操作、每个操作收哪些输入角色与哪些
// 参数。受理校验、页面表单、创作工作空间生成工具的参数描述、创作智能体开发者指令里的
// 模型说明、`gate media models` 的输出都由它生成，不再各写一份。
//
// 订阅后端（grok / codex）的模型是固定预设；厂商面后端（ark_image / ark_video /
// minimax_video）的模型名是管理员建的共享 API 模型行，能力表按 family 给基线
// （FamilyModel）。表达不了的搭配（如「透明背景只配 PNG」）由后端的 Validate 钩子补判，
// 表里的说明写清楚。

import (
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 后端名。订阅后端与 store.AgentProvider* 同值；厂商面后端与协议面标识同值。
const (
	BackendGrok         = store.AgentProviderGrok
	BackendCodex        = store.AgentProviderCodex
	BackendArkImage     = "ark_image"
	BackendArkVideo     = "ark_video"
	BackendMinimaxVideo = "minimax_video"
)

// 计费类别。
const (
	BillingSubscription = "subscription" // 订阅流量，恒 0 元
	BillingMetered      = "metered"      // 按量，走目录价
)

// 参数类型。
const (
	ParamEnum    = "enum"
	ParamInteger = "integer"
	ParamBoolean = "boolean"
	ParamStrings = "strings"
	ParamString  = "string"
)

// Model 是能力表的一项。
type Model struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`
	Kind    string `json:"kind"`
	Billing string `json:"billing"`
	// Note 是给人和模型看的模型备注（可空）。
	Note       string      `json:"note,omitempty" i18n:"text"`
	Operations []Operation `json:"operations"`
}

// Operation 是模型的一种操作：generate / edit / extend。
type Operation struct {
	Name   string      `json:"name"`
	Label  string      `json:"label" i18n:"text"`
	Inputs []InputSpec `json:"inputs"`
	// PromptOptionalWith：带了这些角色之一时提示词可省；空 = 提示词必填。
	PromptOptionalWith []string `json:"prompt_optional_with"`
	Params             []Param  `json:"params"`
}

// InputSpec 是一个输入角色的上限；未列的角色该操作不接受。
type InputSpec struct {
	Role     string   `json:"role"`
	Label    string   `json:"label" i18n:"text"`
	Max      int      `json:"max"`
	MaxBytes int      `json:"max_bytes"`
	Formats  []string `json:"formats"`
	Required bool     `json:"required"`
}

// Param 是一个生成参数。缺省即不带，由平台取缺省值。
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Label       string   `json:"label" i18n:"text"`
	Values      []string `json:"values,omitempty"`
	Min         int      `json:"min,omitempty"`
	Max         int      `json:"max,omitempty"`
	Unit        string   `json:"unit,omitempty" i18n:"text"`
	MaxItems    int      `json:"max_items,omitempty"`
	MaxLength   int      `json:"max_length,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty" i18n:"text"`
}

// Operation 按名字取操作。
func (m Model) Operation(name string) (Operation, bool) {
	for _, op := range m.Operations {
		if op.Name == name {
			return op, true
		}
	}
	return Operation{}, false
}

// Input 按角色取输入上限。
func (o Operation) Input(role string) (InputSpec, bool) {
	for _, in := range o.Inputs {
		if in.Role == role {
			return in, true
		}
	}
	return InputSpec{}, false
}

// Param 按名字取参数。
func (o Operation) Param(name string) (Param, bool) {
	for _, p := range o.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// 输入上限。MaxInputBytes 是单个图像输入（data URI 全文或地址）的字节上限；
// MaxVideoInputBytes 是源视频输入的上限（base64 约膨胀 1/3，对应约 48 MiB 的 MP4）。
const (
	MaxInputBytes      = 10 << 20
	MaxVideoInputBytes = 64 << 20
	// MaxReferenceImages 是 Grok 视频的参考图上限（平台 reference-to-video 每次最多 7 张）；
	// MaxEditImages 是图像提交的参考图上限（Grok 多图编辑每次最多 5 张，Codex 沿用）。
	MaxReferenceImages = 7
	MaxEditImages      = 5
	// MaxVoices 是 reference_audios 预设音色的上限（平台每次最多 3 个）。
	MaxVoices = 3
)

// Grok 视频 1.5 只做生成，classic 承载编辑 / 延长。
const (
	GrokVideoModel     = "grok-imagine-video-1.5"
	GrokVideoEditModel = "grok-imagine-video"
)

var (
	// stillFormats 是 xAI 图像输入认的格式（不收 GIF）；codexFormats 多一个 GIF。
	stillFormats = []string{"png", "jpeg", "webp"}
	codexFormats = []string{"png", "jpeg", "webp", "gif"}
	mp4Only      = []string{"mp4"}
)

func refImages(max int, formats []string) InputSpec {
	return InputSpec{Role: store.MediaRoleReferenceImages, Label: "参考图", Max: max, MaxBytes: MaxInputBytes, Formats: formats}
}

func frame(role, label string) InputSpec {
	return InputSpec{Role: role, Label: label, Max: 1, MaxBytes: MaxInputBytes, Formats: stillFormats}
}

func sourceVideo() InputSpec {
	return InputSpec{Role: store.MediaRoleSourceVideo, Label: "源视频", Max: 1, MaxBytes: MaxVideoInputBytes, Formats: mp4Only, Required: true}
}

var (
	grokImageParams = []Param{
		{Name: "aspect_ratio", Type: ParamEnum, Label: "比例", Values: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20", "21:9", "5:2"}},
		{Name: "resolution", Type: ParamEnum, Label: "分辨率", Values: []string{"1k", "2k"}},
		{Name: "quality", Type: ParamEnum, Label: "质量", Values: []string{"auto", "low", "medium"}, Description: "只在没有参考图时生效"},
	}
	grokVideoAspectRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"}
	grokVideoGenerate     = Operation{
		Name: store.MediaOpGenerate, Label: "生成",
		Inputs: []InputSpec{
			frame(store.MediaRoleFirstFrame, "首帧"), frame(store.MediaRoleLastFrame, "尾帧"), refImages(MaxReferenceImages, stillFormats),
		},
		PromptOptionalWith: []string{store.MediaRoleFirstFrame, store.MediaRoleLastFrame},
		Params: []Param{
			{Name: "duration", Type: ParamInteger, Label: "时长", Min: 1, Max: 15, Unit: "秒", Description: "平台缺省 8 秒"},
			{Name: "aspect_ratio", Type: ParamEnum, Label: "比例", Values: grokVideoAspectRatios},
			{Name: "resolution", Type: ParamEnum, Label: "分辨率", Values: []string{"480p", "720p", "1080p"}},
			{Name: "generate_audio", Type: ParamBoolean, Label: "生成音轨", Description: "平台缺省生成音轨"},
			{Name: "voices", Type: ParamStrings, Label: "音色", MaxItems: MaxVoices, Description: "预设音色 ID，至多 3 个"},
		},
	}
	grokVideoEdit = Operation{
		Name: store.MediaOpEdit, Label: "编辑",
		Inputs:             []InputSpec{sourceVideo()},
		PromptOptionalWith: []string{},
		Params:             []Param{},
	}
	grokVideoExtend = Operation{
		Name: store.MediaOpExtend, Label: "延长",
		Inputs:             []InputSpec{sourceVideo()},
		PromptOptionalWith: []string{},
		Params: []Param{
			{Name: "duration", Type: ParamInteger, Label: "延长时长", Min: 2, Max: 10, Unit: "秒", Description: "延长段的长度，平台缺省 6 秒"},
		},
	}
	codexImageGenerate = Operation{
		Name: store.MediaOpGenerate, Label: "生成",
		Inputs:             []InputSpec{refImages(MaxEditImages, codexFormats)},
		PromptOptionalWith: []string{},
		Params: []Param{
			{Name: "aspect_ratio", Type: ParamEnum, Label: "比例", Values: []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "21:9"}, Description: "折成提示词指令，像素总量由后端决定"},
			{Name: "background", Type: ParamEnum, Label: "背景", Values: []string{"transparent"}, Description: "透明背景只配 PNG 格式"},
			{Name: "output_format", Type: ParamEnum, Label: "输出格式", Values: []string{"png", "jpeg", "webp"}},
		},
	}
)

// presets 是订阅后端的固定模型预设（顺序即页面与智能体看到的缺省顺序）。
var presets = []Model{
	{ID: "gpt-image-2", Backend: BackendCodex, Kind: store.ModelKindImage, Billing: BillingSubscription, Operations: []Operation{codexImageGenerate}},
	{ID: "gpt-image-2.5-flare", Backend: BackendCodex, Kind: store.ModelKindImage, Billing: BillingSubscription,
		Note: "型号名只作请求提示，实际型号由订阅后端决定", Operations: []Operation{codexImageGenerate}},
	{ID: "gpt-image-2.5-sunburst", Backend: BackendCodex, Kind: store.ModelKindImage, Billing: BillingSubscription,
		Note: "型号名只作请求提示，实际型号由订阅后端决定", Operations: []Operation{codexImageGenerate}},
	{ID: "grok-imagine-image-2.0", Backend: BackendGrok, Kind: store.ModelKindImage, Billing: BillingSubscription,
		Operations: []Operation{{
			Name: store.MediaOpGenerate, Label: "生成",
			Inputs:             []InputSpec{refImages(MaxEditImages, stillFormats)},
			PromptOptionalWith: []string{},
			Params:             grokImageParams,
		}}},
	{ID: GrokVideoModel, Backend: BackendGrok, Kind: store.ModelKindVideo, Billing: BillingSubscription,
		Note: "只做生成；编辑 / 延长用 grok-imagine-video", Operations: []Operation{grokVideoGenerate}},
	{ID: GrokVideoEditModel, Backend: BackendGrok, Kind: store.ModelKindVideo, Billing: BillingSubscription,
		Operations: []Operation{grokVideoGenerate, grokVideoEdit, grokVideoExtend}},
}

// families 是厂商面后端的能力基线：模型名由管理员建的共享 API 模型行给出。参数取值随模型走，
// 基线只收形状（类型与长度），取值由厂商裁决、错误原样落进任务的失败原因。
var families = map[string]Model{
	BackendArkImage: {Backend: BackendArkImage, Kind: store.ModelKindImage, Billing: BillingMetered,
		Operations: []Operation{{
			Name: store.MediaOpGenerate, Label: "生成",
			Inputs:             []InputSpec{{Role: store.MediaRoleReferenceImages, Label: "参考图", Max: 10, MaxBytes: MaxInputBytes, Formats: stillFormats}},
			PromptOptionalWith: []string{},
			Params: []Param{
				{Name: "size", Type: ParamString, Label: "尺寸", MaxLength: 16, Description: "2K、4K 这样的档位或 宽x高（如 2048x2048）；可选值随模型而定"},
				{Name: "watermark", Type: ParamBoolean, Label: "水印"},
				{Name: "seed", Type: ParamInteger, Label: "随机种子", Min: -1, Max: 2147483647},
			},
		}}},
	BackendArkVideo: {Backend: BackendArkVideo, Kind: store.ModelKindVideo, Billing: BillingMetered,
		Operations: []Operation{{
			Name: store.MediaOpGenerate, Label: "生成",
			Inputs: []InputSpec{
				frame(store.MediaRoleFirstFrame, "首帧"), frame(store.MediaRoleLastFrame, "尾帧"), refImages(4, stillFormats),
			},
			PromptOptionalWith: []string{store.MediaRoleFirstFrame},
			Params: []Param{
				{Name: "resolution", Type: ParamEnum, Label: "分辨率", Values: []string{"480p", "720p", "1080p"}},
				{Name: "ratio", Type: ParamEnum, Label: "比例", Values: []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"}},
				{Name: "duration", Type: ParamInteger, Label: "时长", Min: 2, Max: 12, Unit: "秒", Description: "可选区间随模型而定"},
				{Name: "generate_audio", Type: ParamBoolean, Label: "生成音轨", Description: "只有带音频能力的模型认"},
				{Name: "camera_fixed", Type: ParamBoolean, Label: "固定镜头"},
				{Name: "watermark", Type: ParamBoolean, Label: "水印"},
				{Name: "seed", Type: ParamInteger, Label: "随机种子", Min: -1, Max: 2147483647},
			},
		}}},
	BackendMinimaxVideo: {Backend: BackendMinimaxVideo, Kind: store.ModelKindVideo, Billing: BillingMetered,
		Operations: []Operation{{
			Name: store.MediaOpGenerate, Label: "生成",
			Inputs: []InputSpec{
				frame(store.MediaRoleFirstFrame, "首帧"), frame(store.MediaRoleLastFrame, "尾帧"), refImages(4, stillFormats),
			},
			PromptOptionalWith: []string{store.MediaRoleFirstFrame},
			Params: []Param{
				{Name: "resolution", Type: ParamString, Label: "分辨率", MaxLength: 16, Required: true, Description: "如 768P、1080P、2K；可选值随模型而定"},
				{Name: "duration", Type: ParamInteger, Label: "时长", Min: -1, Max: 60, Unit: "秒", Required: true, Description: "-1 = 由模型决定"},
			},
		}}},
}

// Presets 返回订阅后端的固定模型预设（副本）。
func Presets() []Model { return append([]Model(nil), presets...) }

// Preset 按模型名取订阅后端的预设。
func Preset(id string) (Model, bool) {
	for _, m := range presets {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// FamilyModel 给一个厂商面模型行套上它所属后端的能力基线。
func FamilyModel(backend, id string) (Model, bool) {
	m, ok := families[backend]
	if !ok {
		return Model{}, false
	}
	m.ID = id
	return m, true
}

// Families 返回厂商面后端名（固定顺序）。
func Families() []string { return []string{BackendArkImage, BackendArkVideo, BackendMinimaxVideo} }
