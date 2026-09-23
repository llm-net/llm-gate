package mediagen

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Inputs 是按角色给出的媒体输入，值是 data URI（data:image/…;base64,… /
// data:video/mp4;base64,…）或 https 地址。角色是稳定的，固定成结构；后端把角色折成各平台的
// 形状。只在内存里经过（Submission → Request → 平台请求体），不落任务行、不进日志（§15.1）。
type Inputs struct {
	FirstFrame      string   `json:"first_frame"`
	LastFrame       string   `json:"last_frame"`
	ReferenceImages []string `json:"reference_images"`
	SourceVideo     string   `json:"source_video"`
}

// Shape 是输入形态快照：各角色带了几个。
func (in Inputs) Shape() map[string]int {
	out := map[string]int{}
	if in.FirstFrame != "" {
		out[store.MediaRoleFirstFrame] = 1
	}
	if in.LastFrame != "" {
		out[store.MediaRoleLastFrame] = 1
	}
	if n := len(in.ReferenceImages); n > 0 {
		out[store.MediaRoleReferenceImages] = n
	}
	if in.SourceVideo != "" {
		out[store.MediaRoleSourceVideo] = 1
	}
	return out
}

// Params 是按能力表校验过的生成参数：enum / string 为 string，integer 为 int64，boolean 为
// bool，strings 为 []string。键名即各模型的参数名，后端把它们放到各平台的位置。
type Params map[string]any

// String 取字符串参数；没带为空串。
func (p Params) String(name string) string { s, _ := p[name].(string); return s }

// Int 取整数参数；ok 为假表示没带。
func (p Params) Int(name string) (int64, bool) { n, ok := p[name].(int64); return n, ok }

// Bool 取布尔参数；nil 表示没带（平台取缺省）。
func (p Params) Bool(name string) *bool {
	if b, ok := p[name].(bool); ok {
		return &b
	}
	return nil
}

// Strings 取字符串表参数。
func (p Params) Strings(name string) []string { s, _ := p[name].([]string); return s }

// Submission 是一次受理的输入。Model 决定后端，没有 provider 字段；Operation 缺省 generate。
// KeyID / KeyDisplay 是这次生成挂靠的客户端 Key 与其展示串快照——每个调用方都必须给一把
// Key，授权、准入与用量都算它的。Origin / Owner 由调用方填（页面 page、创作工作空间
// studio、gate media cli）。
type Submission struct {
	Model     string
	Operation string
	Prompt    string
	Inputs    Inputs
	Params    map[string]any
	Count     int

	KeyID      int64
	KeyDisplay string
	Origin     string
	Owner      string
}

// SubmissionBody 是提交端点（管理面 /admin/v1/media/jobs 与 /gate-helper/v1/media/jobs）
// 共用的请求体形状。
type SubmissionBody struct {
	// KeyID 是管理面提交时选定的归属密钥行 id（必填）；凭 Key 自证的端点由处理器按认证
	// 身份覆盖，收到什么都不作数。
	KeyID     int64          `json:"key_id"`
	Model     string         `json:"model"`
	Operation string         `json:"operation"`
	Prompt    string         `json:"prompt"`
	Inputs    Inputs         `json:"inputs"`
	Params    map[string]any `json:"params"`
	Count     int            `json:"count"`
}

// SubmitBodyLimit 是提交体的字节上限：首帧、尾帧、参考图与源视频都可能以 data URI 内嵌。
const SubmitBodyLimit = 80 << 20

// RunningPerKey 是一把 Key 同时未到终态的任务上限，也是一次提交的候选数上限：一次生成占
// 平台一两分钟，谁都不该把设备后台塞满。各调用方在受理前按它判，全部来源合计。
const RunningPerKey = 3

// DecodeSubmission 读取提交端点的请求体；超限或不是合法 JSON 时返回错误（处理器答 400）。
// 数字按 json.Number 解，整数参数不经 float64。
func DecodeSubmission(w http.ResponseWriter, r *http.Request) (Submission, error) {
	var body SubmissionBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, SubmitBodyLimit))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return Submission{}, err
	}
	return Submission{KeyID: body.KeyID, Model: body.Model, Operation: body.Operation, Prompt: body.Prompt,
		Inputs: body.Inputs, Params: body.Params, Count: body.Count}, nil
}

// InvalidError 是提交参数不合法（调用方答 400），Msg 是给人看的中文原因。
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// NormalizeHead 规整模型名、操作与候选数（能力表查到之前就能做的那一半）。
func NormalizeHead(in Submission) (Submission, error) {
	in.Model = strings.TrimSpace(in.Model)
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.Operation = strings.ToLower(strings.TrimSpace(in.Operation))
	if in.Operation == "" {
		in.Operation = store.MediaOpGenerate
	}
	if in.Model == "" {
		return in, invalid("请选择模型")
	}
	if in.Count == 0 {
		in.Count = 1
	}
	if in.Count < 1 || in.Count > RunningPerKey {
		return in, invalid("候选数须在 1–%d 之间", RunningPerKey)
	}
	return in, nil
}

// Normalize 按能力表规整并校验一次提交；不合法返回 *InvalidError。返回规整后的提交与
// 类型化的参数。
func Normalize(m Model, in Submission) (Submission, Params, error) {
	in, err := NormalizeHead(in)
	if err != nil {
		return in, nil, err
	}
	op, ok := m.Operation(in.Operation)
	if !ok {
		names := make([]string, 0, len(m.Operations))
		for _, o := range m.Operations {
			names = append(names, o.Name)
		}
		return in, nil, invalid("模型 %s 不支持操作 %s（可用：%s）", m.ID, in.Operation, strings.Join(names, "、"))
	}
	if in.Inputs, err = normalizeInputs(op, in.Inputs); err != nil {
		return in, nil, err
	}
	params, err := normalizeParams(op, in.Params)
	if err != nil {
		return in, nil, err
	}
	if in.Prompt == "" && !promptOptional(op, in.Inputs) {
		return in, nil, invalid("提示词不能为空")
	}
	return in, params, nil
}

// promptOptional：带了 PromptOptionalWith 里的角色之一时，提示词可省略（平台由画面驱动）。
func promptOptional(op Operation, in Inputs) bool {
	shape := in.Shape()
	for _, role := range op.PromptOptionalWith {
		if shape[role] > 0 {
			return true
		}
	}
	return false
}

// normalizeInputs 按操作的输入上限校验媒体输入：未列的角色不接受，个数、单值字节与格式按表，
// 必填角色必须给。
func normalizeInputs(op Operation, in Inputs) (Inputs, error) {
	in.FirstFrame = strings.TrimSpace(in.FirstFrame)
	in.LastFrame = strings.TrimSpace(in.LastFrame)
	in.SourceVideo = strings.TrimSpace(in.SourceVideo)
	var refs []string
	for _, ref := range in.ReferenceImages {
		if ref = strings.TrimSpace(ref); ref != "" {
			refs = append(refs, ref)
		}
	}
	in.ReferenceImages = refs
	values := map[string][]string{
		store.MediaRoleFirstFrame:      one(in.FirstFrame),
		store.MediaRoleLastFrame:       one(in.LastFrame),
		store.MediaRoleReferenceImages: in.ReferenceImages,
		store.MediaRoleSourceVideo:     one(in.SourceVideo),
	}
	for _, role := range roleOrder {
		vs := values[role]
		spec, ok := op.Input(role)
		if !ok {
			if len(vs) > 0 {
				return in, invalid("%s操作不接受%s", op.Label, roleLabel(role))
			}
			continue
		}
		if spec.Required && len(vs) == 0 {
			return in, invalid("%s操作需要%s", op.Label, spec.Label)
		}
		if len(vs) > spec.Max {
			return in, invalid("%s最多 %d 个", spec.Label, spec.Max)
		}
		for _, v := range vs {
			if len(v) > spec.MaxBytes {
				return in, invalid("单个%s不能超过 %d MiB", spec.Label, spec.MaxBytes>>20)
			}
			format, ok := inputFormat(v, role == store.MediaRoleSourceVideo)
			if !ok {
				return in, invalid("%s必须是 https 地址或 base64 data URI", spec.Label)
			}
			if format != "" && !oneOf(format, spec.Formats) {
				return in, invalid("%s只接受 %s", spec.Label, strings.ToUpper(strings.Join(spec.Formats, "、")))
			}
		}
	}
	return in, nil
}

var roleOrder = [...]string{store.MediaRoleFirstFrame, store.MediaRoleLastFrame, store.MediaRoleReferenceImages, store.MediaRoleSourceVideo}

func roleLabel(role string) string {
	switch role {
	case store.MediaRoleFirstFrame:
		return "首帧"
	case store.MediaRoleLastFrame:
		return "尾帧"
	case store.MediaRoleReferenceImages:
		return "参考图"
	}
	return "源视频"
}

func one(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// normalizeParams 按操作的参数表校验 params：表里没有的键一律拒绝；枚举不分大小写、落成表里
// 的写法；整数按区间；字符串表的元素须是 [a-z0-9_-]{1,64}（单个字符串当一项）；必填参数
// 必须给。null 与空串当没带。
func normalizeParams(op Operation, raw map[string]any) (Params, error) {
	out := Params{}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		v := raw[name]
		spec, ok := op.Param(name)
		if !ok {
			return nil, invalid("%s操作不接受参数 %s", op.Label, name)
		}
		if v == nil {
			continue
		}
		switch spec.Type {
		case ParamEnum:
			s, ok := v.(string)
			if !ok {
				return nil, invalid("参数 %s 须是字符串", name)
			}
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if strings.EqualFold(s, "jpg") {
				s = "jpeg" // 常见别名
			}
			canon := ""
			for _, allowed := range spec.Values {
				if strings.EqualFold(allowed, s) {
					canon = allowed
				}
			}
			if canon == "" {
				return nil, invalid("参数 %s 不支持取值 %s（可用：%s）", name, s, strings.Join(spec.Values, "、"))
			}
			out[name] = canon
		case ParamString:
			s, ok := v.(string)
			if !ok {
				return nil, invalid("参数 %s 须是字符串", name)
			}
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if len(s) > spec.MaxLength || strings.ContainsAny(s, "\r\n\t") {
				return nil, invalid("参数 %s 过长或含非法字符", name)
			}
			out[name] = s
		case ParamInteger:
			n, ok := toInt(v)
			if !ok {
				return nil, invalid("参数 %s 须是整数", name)
			}
			if n < int64(spec.Min) || n > int64(spec.Max) {
				return nil, invalid("参数 %s 须在 %d–%d 之间", name, spec.Min, spec.Max)
			}
			out[name] = n
		case ParamBoolean:
			b, ok := v.(bool)
			if !ok {
				return nil, invalid("参数 %s 须是 true 或 false", name)
			}
			out[name] = b
		case ParamStrings:
			var items []string
			switch vv := v.(type) {
			case string:
				items = []string{vv}
			case []any:
				for _, it := range vv {
					s, ok := it.(string)
					if !ok {
						return nil, invalid("参数 %s 须是字符串表", name)
					}
					items = append(items, s)
				}
			case []string:
				items = vv
			default:
				return nil, invalid("参数 %s 须是字符串表", name)
			}
			var clean []string
			for _, s := range items {
				if s = strings.ToLower(strings.TrimSpace(s)); s == "" {
					continue
				}
				if !validToken(s) {
					return nil, invalid("参数 %s 的取值只能是字母、数字、连字符与下划线", name)
				}
				clean = append(clean, s)
			}
			if len(clean) > spec.MaxItems {
				return nil, invalid("参数 %s 最多 %d 项", name, spec.MaxItems)
			}
			if len(clean) > 0 {
				out[name] = clean
			}
		}
	}
	for _, spec := range op.Params {
		if _, ok := out[spec.Name]; spec.Required && !ok {
			return nil, invalid("缺少参数 %s（%s）", spec.Name, spec.Label)
		}
	}
	return out, nil
}

// toInt 把 JSON 数字（json.Number / float64）或进程内调用方给的整数收成 int64；带小数的不收。
func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// snapshot 把类型化参数折成任务行的快照（JSON 可编码的值）。
func (p Params) snapshot() map[string]any {
	if len(p) == 0 {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// validToken：[a-z0-9_-]{1,64}。
func validToken(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// inputFormat 接受 https 地址或 data:<image|video>/<格式>;base64,<正文> 形式的 data URI，返回
// 小写格式名（jpg 归一成 jpeg；https 地址给不出格式，返回空串）。
func inputFormat(v string, video bool) (string, bool) {
	if isHTTPSURL(v) {
		return "", true
	}
	prefix := "data:image/"
	if video {
		prefix = "data:video/"
	}
	rest, ok := strings.CutPrefix(v, prefix)
	if !ok {
		return "", false
	}
	mediaType, payload, ok := strings.Cut(rest, ";base64,")
	if !ok || payload == "" || !validBase64Body(payload) {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType == "jpg" {
		mediaType = "jpeg"
	}
	if !validToken(mediaType) {
		return "", false
	}
	return mediaType, true
}

func isHTTPSURL(v string) bool {
	if !strings.HasPrefix(v, "https://") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Host != "" && !strings.ContainsAny(v, " \t\r\n")
}

func validBase64Body(payload string) bool {
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}
