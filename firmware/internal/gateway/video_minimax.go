// video_minimax.go 是 MiniMax 视频协议面（H3 / 海螺 03 的 /v2 接口）的适配器。契约出处是
// docs-upstream/minimax-h3-video.md，任务参数细节以它为准；老 /v1 系
// （Hailuo-2.3 等）与之完全不兼容，明确不在范围内。2026-08-09 厂商官方接口
// 改版起，这套 /v2 路径加上 MiniMax 协议面的首段 /minimax **就是设备的客户端
// 入口**（video.go；出站不带 /minimax），适配器只做旁路认知：路径拼装、受理/
// 查询/取消响应的观测解析、计费特征提取。
//
// 形态要点：创建 POST /v2/video_generation（Context-IR 变体
// POST /v2/h3_context_ir，见 minimaxContextIRPath）只回 {"task_id":"…"}；
// 查询 GET /v2/query/video_generation/{id} 的响应包在 {"task":{…}} 信封里——
// status、content.url、usage、任务级 code/message 都在 task 对象内，拆包后再
// 映射。usage 两种形态都原文入库不换算：视频是秒/张形（{total_seconds,
// input_seconds,output_seconds,input_image_count}），Context-IR 是 token 形
// （prompt_tokens/completion_tokens，iteration-9 按此识别）。**两层错误都判**
// （文档 §11.1）：HTTP 层是标准状态码 + OpenAI 风格 error 对象（一律原样
// 透传）；任务层是 HTTP 200 里的 task.status=failed + task.code/message——
// 审核拒绝（1026 类）走的就是这层，观测把 code/message 载进任务行记账，
// 只判 HTTP 层会把生成类 API 最高频的失败原因漏成「看起来还在跑」。
//
// 状态词汇 queued/running/succeeded/failed/cancelled 直通入库，没有 expired：
// minimax 任务的 expired 只由「厂商记录已查不到」（超 7 天窗口）的设备侧
// 归一产生。取消/删除共用 DELETE /v2/video_generation/{id}（queued → 取消 /
// 终态 → 删记录 / running → 厂商 4xx 拒绝，原样透出），响应体
// {"task_id","action","status"} 由 parseCancelResponse 旁路判 action。
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

// minimaxContextIRPath 是 Context-IR（提示词增强）任务的创建路径：与视频
// 生成同一受理/查询/取消机制，只是提交端点不同、不需要 resolution。
const minimaxContextIRPath = "/v2/h3_context_ir"

// minimaxVideoAdapter 实现 videoAdapter，无状态（路径与解析都是纯函数）。
type minimaxVideoAdapter struct{}

func (minimaxVideoAdapter) protocol() string   { return config.ProtocolMinimaxVideo }
func (minimaxVideoAdapter) submitPath() string { return "/v2/video_generation" }

func (minimaxVideoAdapter) queryPath(vendorTaskID string) string {
	return "/v2/query/video_generation/" + url.PathEscape(vendorTaskID)
}

func (minimaxVideoAdapter) cancelPath(vendorTaskID string) string {
	return "/v2/video_generation/" + url.PathEscape(vendorTaskID)
}

// parseSubmitResponse：受理响应只有 {"task_id":…}（文档 §3.7）。文档示例是
// 字符串，但同平台的 file_id 走数字——两种标量都收，数字保原字面（15 位 id
// 经 float64 会丢精度，UseNumber 挡住）。
func (minimaxVideoAdapter) parseSubmitResponse(body []byte) (string, error) {
	var v struct {
		TaskID any `json:"task_id"`
	}
	if err := unmarshalUseNumber(body, &v); err != nil {
		return "", fmt.Errorf("受理响应不是 JSON: %w", err)
	}
	id := jsonScalarLiteral(v.TaskID)
	if id == "" {
		return "", errors.New("受理响应缺任务 id")
	}
	return id, nil
}

// parseTaskResponse：拆 {"task":{…}} 信封。status 直通；任务级 code/message
// 仅在 failed 时载入观测（文档口径：任务级失败才带它们；succeeded 若携带
// 回声字段也不当错误外泄）；usage 存原文字节（RawMessage 不经 map 往返）；
// content.url 是产物 URL，minimax 无尾帧概念，LastFrameURL 恒空。
func (minimaxVideoAdapter) parseTaskResponse(body []byte) (taskObservation, error) {
	var v struct {
		Task *struct {
			Status  string          `json:"status"`
			Code    any             `json:"code"`
			Message string          `json:"message"`
			Usage   json.RawMessage `json:"usage"`
			Content struct {
				URL string `json:"url"`
			} `json:"content"`
		} `json:"task"`
	}
	if err := unmarshalUseNumber(body, &v); err != nil {
		return taskObservation{}, fmt.Errorf("任务响应不是 JSON: %w", err)
	}
	if v.Task == nil || v.Task.Status == "" {
		return taskObservation{}, errors.New("任务响应缺 task.status")
	}
	obs := taskObservation{
		Status:   v.Task.Status,
		VideoURL: v.Task.Content.URL,
	}
	if v.Task.Status == aigcStatusFailed {
		// 审核拒绝形如 code=1026、message="video description contains
		// sensitive content"——原样透传（code 数字/字符串两种形态都见于文档）。
		obs.ErrorCode, obs.ErrorMessage = jsonScalarLiteral(v.Task.Code), v.Task.Message
	}
	if usage := string(v.Task.Usage); usage != "" && usage != "null" {
		obs.UsageJSON = usage
	}
	return obs, nil
}

// billingFacts：resolution/duration 是 minimax 的必填顶层参数，照字面入库；
// has_video_input 按 content[] 是否含 video_url 判定——参考视频按「输入秒数
// × 输出档单价」真金白银计费（文档 §9），iteration-9 必须知道。
// generate_audio 恒 true：H3 原生出声，没有可关的请求参数（「全模型一致才
// 代填缺省」的规则适用）；service_tier 恒空——minimax 无此概念，如实记无。
func (minimaxVideoAdapter) billingFacts(payload map[string]any) aigcBillingFacts {
	res, _ := payload["resolution"].(string)
	return aigcBillingFacts{
		generateAudio: true,
		reqResolution: res,
		reqDuration:   jsonNumberLiteral(payload["duration"]),
		hasVideoInput: contentHasVideoInput(payload),
	}
}

// parseCancelResponse 旁路判取消（2xx）响应：action=cancelled 表示任务被本次
// 操作取消（queued → cancelled）；action=deleted 是终态删记录，行不动。解析
// 不动按「没取消」处理——观测是旁路，宁少写不错写。
func (minimaxVideoAdapter) parseCancelResponse(body []byte) bool {
	var v struct {
		Action string `json:"action"`
	}
	if err := unmarshalUseNumber(body, &v); err != nil {
		return false
	}
	return v.Action == aigcStatusCancelled
}

// rewriteTaskModel 把查询响应 task.model 的来源侧回显改写回客户端可见名
// （chat 侧 model 回写同一口径：来源侧模型 ID 属于上游账户差异，不外显）。
// payload 是已解出的响应对象；缺 task/model 或已一致时不动。
func (minimaxVideoAdapter) rewriteTaskModel(payload map[string]any, model string) bool {
	t, ok := payload["task"].(map[string]any)
	if !ok {
		return false
	}
	cur, ok := t["model"].(string)
	if !ok || cur == model {
		return false
	}
	t["model"] = model
	return true
}

// unmarshalUseNumber 以 UseNumber 反序列化（数字落成 json.Number 保原字面，
// 不经 float64）。
func unmarshalUseNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// jsonNumberLiteral 把请求体里的数值字段还原为字面量文本（duration: -1 存
// "-1"）；非数值（缺失/字符串形）返回空串，如实记「请求没给」。
func jsonNumberLiteral(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return ""
}

// jsonScalarLiteral 把 JSON 标量还原为字面量文本：字符串原样、数字保原字面；
// 其余（缺失/null/复合值）返回空串。
func jsonScalarLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}
