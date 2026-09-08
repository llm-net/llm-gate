// video_ark.go 是方舟（Volcengine Ark）Seedance 系的视频任务适配器。契约
// 出处是 docs-upstream/ark-seedance-video.md，任务参数细节以它为准。
// 2026-08-10 按「AIGC 恒转发源头厂商官方接口」的口径重新接入：这套
// /contents/generations/tasks 路径**原样就是设备的客户端入口**（挂在火山方舟
// 协议面 /ark/api/v3 之下，见 server.go；出站不带 /ark），适配器只做旁路认知
// ——路径拼装、受理/查询/取消响应的观测解析、计费特征提取、model 回显改写。
//
// 形态要点：创建 POST /contents/generations/tasks 只回 {"id":"cgt-…"}；查询
// GET …/tasks/{id} 回 {model, status, content{video_url,last_frame_url},
// usage, error, duration|frames, …}——duration 与 frames 只会返回其一（请求
// 给了 frames 就回 frames），本适配器对两种形态都容忍：观测只取 status/
// error/usage/产物 URL，其余字段一概不解析不搬运。**Seedance 2.5 的部分报错
// 是异步的**（任务类型判定后才返回）：创建 200 不代表参数合法，错误经查询
// 响应的 error 字段透出——观测把 error 原样带回正是为此。取消与删除共用
// DELETE …/tasks/{id}（queued → 取消 / 终态 → 删记录 / running → 厂商 4xx
// 拒绝，原样透出）。
//
// 状态词汇 queued/running/succeeded/failed/cancelled/expired 与设备内部词汇
// 逐字一致（方舟自己就有 expired——任务超时；设备的「厂商已查不到记录」归一
// 也用这个词，两者对记账的处置相同：终态 + 停止回查）。
//
// 与 minimax 族的差别恰好是「同一个任务面、两种厂商形态」的示例：受理响应的
// id 键（id vs task_id）、查询响应有无 {"task":…} 信封、产物 URL 的键名、
// 取消响应的形态全都不同——videoAdapter 接口存在的理由就是把这些差异关在
// 适配器里，video.go 的提交/查询/取消/清算流程一字不改地复用。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

// arkVideoAdapter 实现 videoAdapter，无状态（路径与解析都是纯函数）。
type arkVideoAdapter struct{}

func (arkVideoAdapter) protocol() string   { return config.ProtocolArkVideo }
func (arkVideoAdapter) submitPath() string { return arkVideoTasksPath }

func (arkVideoAdapter) queryPath(vendorTaskID string) string {
	return arkVideoTasksPath + "/" + url.PathEscape(vendorTaskID)
}

func (arkVideoAdapter) cancelPath(vendorTaskID string) string {
	return arkVideoTasksPath + "/" + url.PathEscape(vendorTaskID)
}

// arkVideoTasksPath 是方舟视频任务族相对端点根的官方路径（端点根已含
// /api/v3 或 /api/plan/v3，见 internal/upstream 的内置端点表）。
const arkVideoTasksPath = "/contents/generations/tasks"

// parseSubmitResponse：受理响应只有任务 id（文档 §4.6）。
func (arkVideoAdapter) parseSubmitResponse(body []byte) (string, error) {
	var v struct {
		ID any `json:"id"`
	}
	if err := unmarshalUseNumber(body, &v); err != nil {
		return "", fmt.Errorf("受理响应不是 JSON: %w", err)
	}
	id := jsonScalarLiteral(v.ID)
	if id == "" {
		return "", errors.New("受理响应缺任务 id")
	}
	return id, nil
}

// parseTaskResponse：status 词汇直通，error 原样带回，usage 存原文字节
// （RawMessage，不经 map 往返——iteration-9 计价读的就是这份原文）。
func (arkVideoAdapter) parseTaskResponse(body []byte) (taskObservation, error) {
	var v struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Usage   json.RawMessage `json:"usage"`
		Content struct {
			VideoURL     string `json:"video_url"`
			LastFrameURL string `json:"last_frame_url"`
		} `json:"content"`
	}
	if err := unmarshalUseNumber(body, &v); err != nil {
		return taskObservation{}, fmt.Errorf("任务响应不是 JSON: %w", err)
	}
	if v.Status == "" {
		return taskObservation{}, errors.New("任务响应缺 status")
	}
	obs := taskObservation{
		Status:       v.Status,
		VideoURL:     v.Content.VideoURL,
		LastFrameURL: v.Content.LastFrameURL,
	}
	if v.Error != nil {
		obs.ErrorCode, obs.ErrorMessage = v.Error.Code, v.Error.Message
	}
	if usage := string(v.Usage); usage != "" && usage != "null" {
		obs.UsageJSON = usage
	}
	return obs, nil
}

// parseCancelResponse：方舟的 DELETE **无返回参数**（文档 §7），所以响应体
// 里没有「本次到底是取消了还是删了记录」的判据。取消前 video.go 已经对非
// 终态行做过一次设备自己的回查，那一次的观测才是账单事实；这里恒回 false，
// 让状态由回查而不是由一个空响应体推定——宁少写不错写（与 minimax 适配器
// 解析不动时同一处置）。代价是「queued 任务被取消」要等下一次查询或懒对账
// 才落成 cancelled，而那两条路都不会漏掉它。
func (arkVideoAdapter) parseCancelResponse([]byte) bool { return false }

// rewriteTaskModel 把查询响应顶层 model 的来源侧回显改写回客户端可见名
// （chat 侧 model 回写同一口径）。方舟回的是「模型名称-版本」，即便来源侧
// 模型 ID 留空、与模型名同名，回显里也会多出版本后缀——这道改写因此不是
// 只为多来源准备的，单来源同样会命中。
func (arkVideoAdapter) rewriteTaskModel(payload map[string]any, model string) bool {
	cur, ok := payload["model"].(string)
	if !ok || cur == model {
		return false
	}
	payload["model"] = model
	return true
}

// billingFacts 提取提交时即固化的计费特征。缺省值只在**全模型一致**时代填
// （generate_audio 缺省 true、service_tier 缺省 default——方舟文档口径）；
// 随模型而变的 resolution/duration 缺省存空串，如实记录「未指定」——猜错的
// 缺省比空白更误导 iteration-9 的计价。has_video_input 按 content[] 是否含
// video_url 元素判定（Seedance 分档判据：参考视频输入走更高的那档单价）。
func (arkVideoAdapter) billingFacts(payload map[string]any) aigcBillingFacts {
	facts := aigcBillingFacts{generateAudio: true, serviceTier: "default"}
	if v, ok := payload["generate_audio"].(bool); ok {
		facts.generateAudio = v
	}
	if v, ok := payload["service_tier"].(string); ok && v != "" {
		facts.serviceTier = v
	}
	if v, ok := payload["resolution"].(string); ok {
		facts.reqResolution = v
	}
	facts.reqDuration = jsonNumberLiteral(payload["duration"])
	facts.hasVideoInput = contentHasVideoInput(payload)
	return facts
}
