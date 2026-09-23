package mediagen

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func mustPreset(t *testing.T, id string) Model {
	t.Helper()
	m, ok := Preset(id)
	if !ok {
		t.Fatalf("能力表里没有预设 %s", id)
	}
	return m
}

// wantInvalid 断言 err 是含 want 的 *InvalidError。
func wantInvalid(t *testing.T, err error, want string) {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Msg, want) {
		t.Fatalf("err = %v，期望含 %q 的 InvalidError", err, want)
	}
}

const (
	grokImageModel  = "grok-imagine-image-2.0"
	codexImageModel = "gpt-image-2"
)

// 媒体输入按能力表校验：未列的角色不接受；每个值必须是 https 地址或 base64 data URI；个数、
// 单值体积与格式按表（Grok 不收 GIF、Codex 收）；必填角色必须给；空白项被剔除。
func TestNormalizeInputs(t *testing.T) {
	ok := Submission{Model: GrokVideoModel, Prompt: "p", Inputs: Inputs{
		FirstFrame: " " + tinyJPEG + " ", LastFrame: "https://example.invalid/last.png",
		ReferenceImages: []string{"", tinyJPEG, "  ", "https://example.invalid/a.webp"}}}
	got, _, err := Normalize(mustPreset(t, GrokVideoModel), ok)
	if err != nil {
		t.Fatalf("合法输入被拒: %v", err)
	}
	if got.Inputs.FirstFrame != tinyJPEG || got.Inputs.LastFrame != "https://example.invalid/last.png" ||
		len(got.Inputs.ReferenceImages) != 2 || got.Inputs.ReferenceImages[1] != "https://example.invalid/a.webp" {
		t.Fatalf("规整后的输入 = %+v", got.Inputs)
	}
	if shape := got.Inputs.Shape(); !reflect.DeepEqual(shape, map[string]int{store.MediaRoleFirstFrame: 1, store.MediaRoleLastFrame: 1, store.MediaRoleReferenceImages: 2}) {
		t.Fatalf("输入形态 = %v", shape)
	}
	// jpg 归一成 jpeg；全空白的参考图视为没有输入。
	if _, _, err := Normalize(mustPreset(t, grokImageModel), Submission{Model: grokImageModel, Prompt: "p", Inputs: Inputs{ReferenceImages: []string{"data:image/JPG;base64,/9j/4AAQ"}}}); err != nil {
		t.Fatalf("JPG 参考图被拒: %v", err)
	}
	if got, _, err := Normalize(mustPreset(t, grokImageModel), Submission{Model: grokImageModel, Prompt: "p", Inputs: Inputs{ReferenceImages: []string{"", " "}}}); err != nil || len(got.Inputs.Shape()) != 0 {
		t.Fatalf("全空白的参考图应视为没有输入: %+v / %v", got.Inputs, err)
	}
	// GIF：xAI 图像输入不认，Grok 在受理时拦下；Codex 的 input_image 收 GIF。
	const gifURI = "data:image/gif;base64,R0lGODdh"
	if _, _, err := Normalize(mustPreset(t, codexImageModel), Submission{Model: codexImageModel, Prompt: "p", Inputs: Inputs{ReferenceImages: []string{gifURI, "https://example.invalid/b.png"}}}); err != nil {
		t.Fatalf("Codex GIF 参考图被拒: %v", err)
	}
	// 编辑 / 延长：源视频必填，MP4 的 data URI 或 https 地址都收。
	edit := mustPreset(t, GrokVideoEditModel)
	if got, _, err := Normalize(edit, Submission{Model: edit.ID, Prompt: "p", Operation: " Edit ", Inputs: Inputs{SourceVideo: "https://example.invalid/in.mp4"}}); err != nil || got.Operation != store.MediaOpEdit {
		t.Fatalf("编辑提交 = %+v / %v", got, err)
	}

	tooMany := make([]string, MaxReferenceImages+1)
	for i := range tooMany {
		tooMany[i] = tinyJPEG
	}
	huge := "data:image/png;base64," + strings.Repeat("A", MaxInputBytes)
	bad := []struct {
		name  string
		model string
		op    string
		in    Inputs
		want  string
	}{
		{"Grok 图像不收 GIF 参考图", grokImageModel, "", Inputs{ReferenceImages: []string{gifURI}}, "参考图只接受 PNG、JPEG、WEBP"},
		{"Grok 视频不收 GIF 首帧", GrokVideoModel, "", Inputs{FirstFrame: gifURI}, "首帧只接受 PNG、JPEG、WEBP"},
		{"图像模型不收首帧", grokImageModel, "", Inputs{FirstFrame: tinyJPEG}, "生成操作不接受首帧"},
		{"图像模型不收尾帧", codexImageModel, "", Inputs{LastFrame: tinyJPEG}, "生成操作不接受尾帧"},
		{"图像模型不收源视频", grokImageModel, "", Inputs{SourceVideo: tinyMP4}, "生成操作不接受源视频"},
		{"视频生成不收源视频", GrokVideoEditModel, "", Inputs{SourceVideo: tinyMP4}, "生成操作不接受源视频"},
		{"编辑不收首帧", GrokVideoEditModel, "edit", Inputs{SourceVideo: tinyMP4, FirstFrame: tinyJPEG}, "编辑操作不接受首帧"},
		{"延长不收参考图", GrokVideoEditModel, "extend", Inputs{SourceVideo: tinyMP4, ReferenceImages: []string{tinyJPEG}}, "延长操作不接受参考图"},
		{"编辑缺源视频", GrokVideoEditModel, "edit", Inputs{}, "编辑操作需要源视频"},
		{"延长缺源视频", GrokVideoEditModel, "extend", Inputs{}, "延长操作需要源视频"},
		{"视频参考图过多", GrokVideoModel, "", Inputs{ReferenceImages: tooMany}, "参考图最多 7 个"},
		{"图像参考图过多", grokImageModel, "", Inputs{ReferenceImages: tooMany[:MaxEditImages+1]}, "参考图最多 5 个"},
		{"Codex 参考图过多", codexImageModel, "", Inputs{ReferenceImages: tooMany[:MaxEditImages+1]}, "参考图最多 5 个"},
		{"单张过大", GrokVideoModel, "", Inputs{FirstFrame: huge}, "单个首帧不能超过 10 MiB"},
		{"http 地址", GrokVideoModel, "", Inputs{FirstFrame: "http://example.invalid/a.png"}, "首帧必须是 https"},
		{"帧给了视频 data URI", GrokVideoModel, "", Inputs{LastFrame: tinyMP4}, "尾帧必须是 https"},
		{"非 base64 的 data URI", GrokVideoModel, "", Inputs{ReferenceImages: []string{"data:image/png,raw"}}, "参考图必须是 https"},
		{"data URI 正文非法", GrokVideoModel, "", Inputs{FirstFrame: "data:image/png;base64,ab cd"}, "必须是 https"},
		{"任意文本", GrokVideoModel, "", Inputs{FirstFrame: "not-an-image"}, "必须是 https"},
		{"源视频非 MP4", GrokVideoEditModel, "edit", Inputs{SourceVideo: "data:video/webm;base64,AAAA"}, "源视频只接受 MP4"},
		{"源视频给了图像 data URI", GrokVideoEditModel, "edit", Inputs{SourceVideo: tinyJPEG}, "源视频必须是 https"},
		{"源视频 http", GrokVideoEditModel, "extend", Inputs{SourceVideo: "http://example.invalid/a.mp4"}, "源视频必须是 https"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Normalize(mustPreset(t, tc.model), Submission{Model: tc.model, Operation: tc.op, Prompt: "p", Inputs: tc.in})
			wantInvalid(t, err, tc.want)
		})
	}
}

// 参数按能力表校验：表里没有的键一律拒绝；enum 不分大小写、落成表里的写法；integer 按区间、
// 不收小数；boolean 只收布尔；strings 收单个字符串与数组、超 max_items 被拒；必填参数必须给；
// null 与空串当没带。
func TestNormalizeParams(t *testing.T) {
	video := mustPreset(t, GrokVideoEditModel)
	_, params, err := Normalize(video, Submission{Model: video.ID, Inputs: Inputs{FirstFrame: tinyJPEG}, Params: map[string]any{
		"duration": json.Number("12"), "aspect_ratio": " 16:9 ", "resolution": "1080P", "generate_audio": false, "voices": []any{" Eve ", "", "leo"}}})
	if err != nil {
		t.Fatalf("合法的视频生成参数被拒: %v", err)
	}
	want := Params{"duration": int64(12), "aspect_ratio": "16:9", "resolution": "1080p", "generate_audio": false, "voices": []string{"eve", "leo"}}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("规整后的参数 = %#v，期望 %#v", params, want)
	}
	// 整数：进程内调用方给的 int / int64 与 JSON 的 float64 整数值都收。
	for _, v := range []any{6, int64(6), float64(6), json.Number("6")} {
		_, p, err := Normalize(video, Submission{Model: video.ID, Prompt: "p", Operation: "Extend", Inputs: Inputs{SourceVideo: tinyMP4}, Params: map[string]any{"duration": v}})
		if n, ok := p.Int("duration"); err != nil || !ok || n != 6 {
			t.Fatalf("延长时长 %T = %v / %v", v, p, err)
		}
	}
	// strings 收单个字符串（当一项）与 []string。
	for _, v := range []any{"Eve", []string{"eve"}, []any{"EVE"}} {
		_, p, err := Normalize(video, Submission{Model: video.ID, Prompt: "p", Params: map[string]any{"voices": v}})
		if got := p.Strings("voices"); err != nil || len(got) != 1 || got[0] != "eve" {
			t.Fatalf("音色 %T = %v / %v", v, p, err)
		}
	}
	// null、空串与空表当没带：参数表保持为空。
	_, p, err := Normalize(video, Submission{Model: video.ID, Prompt: "p", Params: map[string]any{"duration": nil, "resolution": "  ", "voices": []any{" "}}})
	if err != nil || len(p) != 0 || p.snapshot() != nil {
		t.Fatalf("空值参数 = %v / %v", p, err)
	}
	// 图像：Grok 与 Codex 各认自己的表；Codex 的枚举同样不分大小写。
	img := mustPreset(t, grokImageModel)
	if _, p, err = Normalize(img, Submission{Model: img.ID, Prompt: "p", Inputs: Inputs{ReferenceImages: []string{tinyJPEG}},
		Params: map[string]any{"aspect_ratio": "21:9", "resolution": "2K", "quality": "Medium"}}); err != nil || p.String("resolution") != "2k" || p.String("quality") != "medium" {
		t.Fatalf("Grok 图像参数 = %v / %v", p, err)
	}
	codex := mustPreset(t, codexImageModel)
	if _, p, err = Normalize(codex, Submission{Model: codex.ID, Prompt: "p",
		Params: map[string]any{"aspect_ratio": " 16:9 ", "background": " Transparent ", "output_format": "PNG"}}); err != nil ||
		p.String("background") != "transparent" || p.String("output_format") != "png" || p.String("aspect_ratio") != "16:9" {
		t.Fatalf("Codex 图像参数 = %v / %v", p, err)
	}
	// 厂商面基线：必填参数给全即过，string 参数原样保留（取值由厂商裁决）。
	minimax, ok := FamilyModel(BackendMinimaxVideo, "x")
	if !ok || minimax.ID != "x" || minimax.Backend != BackendMinimaxVideo || minimax.Billing != BillingMetered || minimax.Kind != store.ModelKindVideo {
		t.Fatalf("FamilyModel = %+v / %v", minimax, ok)
	}
	if _, p, err = Normalize(minimax, Submission{Model: "x", Prompt: "p", Params: map[string]any{"resolution": " 768P ", "duration": -1}}); err != nil || p.String("resolution") != "768P" {
		t.Fatalf("MiniMax 参数 = %v / %v", p, err)
	}
	if _, ok := FamilyModel("nope", "x"); ok {
		t.Fatal("未知后端不该有能力基线")
	}

	bad := []struct {
		name   string
		model  Model
		op     string
		params map[string]any
		want   string
	}{
		{"未知参数键", video, "", map[string]any{"fps": 24}, "生成操作不接受参数 fps"},
		{"未知参数键给 null 也拒", video, "", map[string]any{"fps": nil}, "不接受参数 fps"},
		{"视频不收图像质量", video, "", map[string]any{"quality": "low"}, "不接受参数 quality"},
		{"图像不收视频时长", img, "", map[string]any{"duration": 5}, "不接受参数 duration"},
		{"Grok 图像不收背景", img, "", map[string]any{"background": "transparent"}, "不接受参数 background"},
		{"Codex 不收分辨率", codex, "", map[string]any{"resolution": "2k"}, "不接受参数 resolution"},
		{"编辑不收任何参数", video, "edit", map[string]any{"resolution": "720p"}, "编辑操作不接受参数 resolution"},
		{"延长不收比例", video, "extend", map[string]any{"aspect_ratio": "16:9"}, "延长操作不接受参数 aspect_ratio"},
		{"enum 取值非法", video, "", map[string]any{"resolution": "2k"}, "参数 resolution 不支持取值 2k（可用：480p、720p、1080p）"},
		{"视频比例非法", video, "", map[string]any{"aspect_ratio": "21:9"}, "参数 aspect_ratio 不支持取值"},
		{"图像比例非法", img, "", map[string]any{"aspect_ratio": "7:3"}, "参数 aspect_ratio 不支持取值"},
		{"图像质量非法", img, "", map[string]any{"quality": "high"}, "参数 quality 不支持取值"},
		{"Codex 背景非法", codex, "", map[string]any{"background": "blur"}, "参数 background 不支持取值"},
		{"Codex 比例非法", codex, "", map[string]any{"aspect_ratio": "5:2"}, "参数 aspect_ratio 不支持取值"},
		{"Codex 输出格式非法", codex, "", map[string]any{"output_format": "gif"}, "参数 output_format 不支持取值"},
		{"enum 给了数字", video, "", map[string]any{"resolution": 720}, "参数 resolution 须是字符串"},
		{"时长越上界", video, "", map[string]any{"duration": 16}, "参数 duration 须在 1–15 之间"},
		{"时长越下界", video, "", map[string]any{"duration": 0}, "参数 duration 须在 1–15 之间"},
		{"延长时长越界", video, "extend", map[string]any{"duration": json.Number("11")}, "参数 duration 须在 2–10 之间"},
		{"整数给了小数", video, "", map[string]any{"duration": 6.5}, "参数 duration 须是整数"},
		{"整数给了小数 JSON", video, "", map[string]any{"duration": json.Number("6.5")}, "参数 duration 须是整数"},
		{"整数给了字符串", video, "", map[string]any{"duration": "6"}, "参数 duration 须是整数"},
		{"布尔给了字符串", video, "", map[string]any{"generate_audio": "false"}, "参数 generate_audio 须是 true 或 false"},
		{"布尔给了数字", video, "", map[string]any{"generate_audio": 0}, "参数 generate_audio 须是 true 或 false"},
		{"字符串表超上限", video, "", map[string]any{"voices": []any{"a", "b", "c", "d"}}, "参数 voices 最多 3 项"},
		{"字符串表元素非法", video, "", map[string]any{"voices": []any{"e ve"}}, "参数 voices 的取值只能是"},
		{"字符串表元素不是字符串", video, "", map[string]any{"voices": []any{"eve", 3}}, "参数 voices 须是字符串表"},
		{"字符串表给了数字", video, "", map[string]any{"voices": 3}, "参数 voices 须是字符串表"},
		{"必填参数全缺", minimax, "", nil, "缺少参数 resolution（分辨率）"},
		{"必填参数缺一个", minimax, "", map[string]any{"resolution": "768P"}, "缺少参数 duration（时长）"},
		{"必填参数给了空串", minimax, "", map[string]any{"resolution": " ", "duration": 6}, "缺少参数 resolution"},
		{"string 参数过长", minimax, "", map[string]any{"resolution": strings.Repeat("P", 17), "duration": 6}, "参数 resolution 过长或含非法字符"},
		{"string 参数含换行", minimax, "", map[string]any{"resolution": "768P\nx", "duration": 6}, "参数 resolution 过长或含非法字符"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			in := Submission{Model: tc.model.ID, Operation: tc.op, Prompt: "p", Params: tc.params}
			if tc.op != "" {
				in.Inputs.SourceVideo = tinyMP4
			}
			_, _, err := Normalize(tc.model, in)
			wantInvalid(t, err, tc.want)
		})
	}
}

// 操作、提示词与候选数：1.5 只做生成，编辑 / 延长由 classic 承载；提示词只在带了
// prompt_optional_with 里的角色时可省；候选数 1–RunningPerKey，缺省 1。
func TestNormalizeOperationPromptAndCount(t *testing.T) {
	v15, classic := mustPreset(t, GrokVideoModel), mustPreset(t, GrokVideoEditModel)
	for _, op := range []string{store.MediaOpEdit, store.MediaOpExtend} {
		_, _, err := Normalize(v15, Submission{Model: v15.ID, Prompt: "p", Operation: op, Inputs: Inputs{SourceVideo: tinyMP4}})
		wantInvalid(t, err, "模型 grok-imagine-video-1.5 不支持操作 "+op+"（可用：generate）")
		if got, _, err := Normalize(classic, Submission{Model: classic.ID, Prompt: "p", Operation: op, Inputs: Inputs{SourceVideo: tinyMP4}}); err != nil || got.Operation != op {
			t.Fatalf("classic %s = %+v / %v", op, got, err)
		}
	}
	_, _, err := Normalize(classic, Submission{Model: classic.ID, Prompt: "p", Operation: "remix", Inputs: Inputs{SourceVideo: tinyMP4}})
	wantInvalid(t, err, "不支持操作 remix（可用：generate、edit、extend）")

	// 带首帧或尾帧时提示词可省；只带参考图、编辑 / 延长、文生视频与图像都必填。
	for _, in := range []Inputs{{FirstFrame: tinyJPEG}, {LastFrame: tinyJPEG}} {
		if got, _, err := Normalize(v15, Submission{Model: v15.ID, Prompt: "  ", Inputs: in}); err != nil || got.Prompt != "" {
			t.Fatalf("带帧的视频生成应允许空提示词: %+v / %v", got, err)
		}
	}
	ark, _ := FamilyModel(BackendArkVideo, "seedance")
	if _, _, err := Normalize(ark, Submission{Model: ark.ID, Inputs: Inputs{FirstFrame: tinyJPEG}}); err != nil {
		t.Fatalf("方舟视频带首帧应允许空提示词: %v", err)
	}
	needPrompt := []struct {
		name string
		m    Model
		op   string
		in   Inputs
	}{
		{"文生视频", v15, "", Inputs{}},
		{"参考图生视频", v15, "", Inputs{ReferenceImages: []string{tinyJPEG}}},
		{"编辑", classic, "edit", Inputs{SourceVideo: tinyMP4}},
		{"延长", classic, "extend", Inputs{SourceVideo: tinyMP4}},
		{"图像带参考图", mustPreset(t, grokImageModel), "", Inputs{ReferenceImages: []string{tinyJPEG}}},
		{"方舟视频只带尾帧", ark, "", Inputs{LastFrame: tinyJPEG}},
	}
	for _, tc := range needPrompt {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Normalize(tc.m, Submission{Model: tc.m.ID, Operation: tc.op, Inputs: tc.in})
			wantInvalid(t, err, "提示词不能为空")
		})
	}

	head, err := NormalizeHead(Submission{Model: " " + v15.ID + " ", Operation: " ", Prompt: " p "})
	if err != nil || head.Model != v15.ID || head.Operation != store.MediaOpGenerate || head.Prompt != "p" || head.Count != 1 {
		t.Fatalf("NormalizeHead = %+v / %v", head, err)
	}
	if head, err = NormalizeHead(Submission{Model: v15.ID, Count: RunningPerKey}); err != nil || head.Count != RunningPerKey {
		t.Fatalf("候选数上限 = %+v / %v", head, err)
	}
	for _, n := range []int{-1, RunningPerKey + 1} {
		_, err := NormalizeHead(Submission{Model: v15.ID, Count: n})
		wantInvalid(t, err, "候选数须在 1–3 之间")
	}
	_, err = NormalizeHead(Submission{Model: "  "})
	wantInvalid(t, err, "请选择模型")
}

// 绊线：给模型看的文本与受理校验同源——能力表里每个模型名、输入角色、参数名与枚举取值都
// 出现在 DescribeKind 的输出里；往表里加了东西而描述没跟上就在这里失败。
func TestDescribeCoversCapabilityTable(t *testing.T) {
	models := Presets()
	placeholders := map[string]string{}
	for _, name := range Families() {
		m, ok := FamilyModel(name, "<"+name+" 模型>")
		if !ok {
			t.Fatalf("Families 列了 %s，却没有能力基线", name)
		}
		placeholders[m.ID] = name
		models = append(models, m)
	}
	texts := map[string]string{store.ModelKindImage: DescribeKind(store.ModelKindImage), store.ModelKindVideo: DescribeKind(store.ModelKindVideo)}
	seen := map[string]int{}
	for _, m := range models {
		text, ok := texts[m.Kind]
		if !ok {
			t.Fatalf("模型 %s 的种类 %q 不是 image / video", m.ID, m.Kind)
		}
		seen[m.Kind]++
		other := texts[store.ModelKindVideo]
		if m.Kind == store.ModelKindVideo {
			other = texts[store.ModelKindImage]
		}
		if !strings.Contains(text, "`"+m.ID+"`") || strings.Contains(other, "`"+m.ID+"`") {
			t.Fatalf("模型 %s 应只出现在 %s 的描述里", m.ID, m.Kind)
		}
		if !strings.Contains(text, m.Backend) || (m.Note != "" && !strings.Contains(text, m.Note)) {
			t.Fatalf("模型 %s 的后端或备注没有进描述", m.ID)
		}
		// 只在本模型那一段里找，免得被别的模型的同名参数蒙混过去。
		section := describeSection(t, text, m.ID)
		for _, op := range m.Operations {
			if !strings.Contains(section, "operation `"+op.Name+"`") {
				t.Fatalf("模型 %s 的操作 %s 没有进描述", m.ID, op.Name)
			}
			for _, in := range op.Inputs {
				if !strings.Contains(section, in.Role) {
					t.Fatalf("模型 %s 操作 %s 的输入角色 %s 没有进描述", m.ID, op.Name, in.Role)
				}
			}
			for _, role := range op.PromptOptionalWith {
				if !strings.Contains(section, role+" ") && !strings.Contains(section, role+" 时提示词可省") {
					t.Fatalf("模型 %s 操作 %s 的提示词可省条件 %s 没有进描述", m.ID, op.Name, role)
				}
			}
			for _, p := range op.Params {
				if !strings.Contains(section, p.Name+"（") {
					t.Fatalf("模型 %s 操作 %s 的参数 %s 没有进描述", m.ID, op.Name, p.Name)
				}
				for _, v := range p.Values {
					if !strings.Contains(section, v) {
						t.Fatalf("模型 %s 参数 %s 的取值 %s 没有进描述", m.ID, p.Name, v)
					}
				}
				if p.Description != "" && !strings.Contains(section, p.Description) {
					t.Fatalf("模型 %s 参数 %s 的说明没有进描述", m.ID, p.Name)
				}
			}
		}
	}
	if seen[store.ModelKindImage] == 0 || seen[store.ModelKindVideo] == 0 {
		t.Fatalf("能力表应同时有图像与视频模型: %v", seen)
	}
	// 描述里的每个模型名都来自能力表（没有手写的漏网之鱼）。
	for kind, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "- `") {
				continue
			}
			id := strings.SplitN(strings.TrimPrefix(line, "- `"), "`", 2)[0]
			if _, ok := Preset(id); !ok && placeholders[id] == "" {
				t.Fatalf("%s 的描述里出现了能力表之外的模型 %s", kind, id)
			}
		}
	}
	// Describe 按给定的模型渲染：页面与开发者指令拿 Available 的结果喂它。
	if one := Describe([]Model{mustPreset(t, codexImageModel)}); !strings.Contains(one, "`gpt-image-2`") || strings.Contains(one, "grok") {
		t.Fatalf("Describe 单模型 = %q", one)
	}
}

// describeSection 取描述里某个模型那一段（从它的列表项到下一个模型的列表项之前）。
func describeSection(t *testing.T, text, id string) string {
	t.Helper()
	start := strings.Index(text, "- `"+id+"`")
	if start < 0 {
		t.Fatalf("描述里没有模型 %s", id)
	}
	rest := text[start+1:]
	if end := strings.Index(rest, "\n- `"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// 绊线：预设只属于订阅后端；能力表编成 JSON 给页面与 gate media 时不出 null（切片非 nil），
// 每个模型至少有 generate，枚举参数必有取值，整数参数区间有效。
func TestCapabilityTableShape(t *testing.T) {
	models := Presets()
	if len(models) == 0 {
		t.Fatal("能力表没有预设")
	}
	ids := map[string]bool{}
	for _, m := range models {
		if m.Backend != BackendGrok && m.Backend != BackendCodex {
			t.Fatalf("预设 %s 的后端 %q 不是订阅后端", m.ID, m.Backend)
		}
		if m.Billing != BillingSubscription {
			t.Fatalf("预设 %s 的计费类别 = %q", m.ID, m.Billing)
		}
		if ids[m.ID] {
			t.Fatalf("预设 %s 重复", m.ID)
		}
		ids[m.ID] = true
		if got, ok := Preset(m.ID); !ok || got.Backend != m.Backend {
			t.Fatalf("Preset(%s) = %+v / %v", m.ID, got, ok)
		}
	}
	if _, ok := Preset("nope"); ok {
		t.Fatal("未知预设不该命中")
	}
	for _, name := range Families() {
		m, ok := FamilyModel(name, "x")
		if !ok || m.Backend != name || m.Billing != BillingMetered {
			t.Fatalf("厂商面基线 %s = %+v / %v", name, m, ok)
		}
		models = append(models, m)
	}
	types := map[string]bool{ParamEnum: true, ParamInteger: true, ParamBoolean: true, ParamStrings: true, ParamString: true}
	for _, m := range models {
		if m.Kind != store.ModelKindImage && m.Kind != store.ModelKindVideo {
			t.Fatalf("模型 %s 的种类 = %q", m.ID, m.Kind)
		}
		if _, ok := m.Operation(store.MediaOpGenerate); !ok {
			t.Fatalf("模型 %s 没有 generate 操作", m.ID)
		}
		for _, op := range m.Operations {
			if op.PromptOptionalWith == nil || op.Params == nil || op.Inputs == nil {
				t.Fatalf("模型 %s 操作 %s 有 nil 切片（JSON 会出 null）: %+v", m.ID, op.Name, op)
			}
			if op.Label == "" {
				t.Fatalf("模型 %s 操作 %s 没有标签", m.ID, op.Name)
			}
			for _, in := range op.Inputs {
				if in.Formats == nil || in.Max < 1 || in.MaxBytes < 1 || in.Label == "" {
					t.Fatalf("模型 %s 操作 %s 的输入 %+v 不完整", m.ID, op.Name, in)
				}
				if got, ok := op.Input(in.Role); !ok || got.Role != in.Role {
					t.Fatalf("Input(%s) 没有命中", in.Role)
				}
			}
			for _, role := range op.PromptOptionalWith {
				if _, ok := op.Input(role); !ok {
					t.Fatalf("模型 %s 操作 %s 的提示词可省条件 %s 不是它接受的角色", m.ID, op.Name, role)
				}
			}
			for _, p := range op.Params {
				if !types[p.Type] || p.Label == "" {
					t.Fatalf("模型 %s 参数 %+v 的类型或标签不对", m.ID, p)
				}
				if got, ok := op.Param(p.Name); !ok || got.Name != p.Name {
					t.Fatalf("Param(%s) 没有命中", p.Name)
				}
				switch p.Type {
				case ParamEnum:
					if len(p.Values) == 0 {
						t.Fatalf("模型 %s 枚举参数 %s 没有取值", m.ID, p.Name)
					}
				case ParamInteger:
					if p.Min >= p.Max {
						t.Fatalf("模型 %s 整数参数 %s 的区间 %d–%d 无效", m.ID, p.Name, p.Min, p.Max)
					}
				case ParamStrings:
					if p.MaxItems < 1 {
						t.Fatalf("模型 %s 字符串表参数 %s 没有上限", m.ID, p.Name)
					}
				case ParamString:
					if p.MaxLength < 1 {
						t.Fatalf("模型 %s 字符串参数 %s 没有长度上限", m.ID, p.Name)
					}
				}
			}
		}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("能力表 JSON 里有 null: %s", raw)
	}
}
