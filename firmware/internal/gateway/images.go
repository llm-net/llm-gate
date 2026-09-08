// images.go 实现图片入口（2026-08-10 接入，方舟 Seedream）。与视频任务面不同，
// 出图是**同步**调用——没有任务 id、没有任务行、没有查询与取消，整条路复用
// chat 侧的选路透传核心 forward()（proxy.go）：
//
//	POST /ark/api/v3/images/generations     创建并同步返回图像
//
// 首段 `/ark` 是火山方舟协议面的路径段（server.go 的路由块），之后就是方舟站点根
// 之后的官方那一段（客户端把厂商 SDK 的 base_url 换成「设备地址 + /ark」即可），
// 与视频面 /ark/api/v3/contents/generations/tasks 同段；出站不带 /ark。
//
//   - kind=image 选路 + 单协议 ark_image（内置端点表里方舟按量与订阅**都**
//     服务它，端点根各自不同）。kind 闸门双向：图片模型进文本/视频入口、
//     文本/视频模型进本入口，一律 404 model_not_found，不解释内部原因；
//   - 请求体字节保真透传，只改 model 为来源侧 ID。Seedream 与 OpenAI images
//     形的参数差异（image 参考图单值/数组、sequential_image_generation 组图、
//     无 n/quality/style）一概不翻译不校验——参数值词汇表随模型走；
//   - 响应侧沿 chat 口径：2xx 顶层 model 回写为客户端请求名（官方响应带顶层
//     model），错误体绝不改写；stream:true 的 SSE（image_generation.* 事件）
//     走 forward 的通用逐行转发，带 model 的 data 行同样回写；
//   - 观察器把厂商 usage 原文、出图张数与请求 size 参数记进 reqInfo 内存
//     点位（账单事实；同步调用无任务行，不建新表），由 metering.go 的收尾
//     记账消费——非流式与流式两种形态都覆盖。
//
// 真值形态（2026-08-10 用真实订阅 Key 对 doubao-seedream-5.0-lite 实调一次，
// 覆盖此前只据文档推定的部分）：
//
//	POST {ark}/images/generations，鉴权 Authorization: Bearer 同 chat；
//	请求必填 model + prompt，可选 size（"2k"/"3k"/"4k" 或 "宽x高"；5.0 lite
//	的下限是 3686400 像素 = 1920×1920，给小了 400 InvalidParameter）、
//	image（URL/base64 data URI，单值或数组）、sequential_image_generation
//	(+_options.max_images)、stream（缺省 false）、response_format（url 缺省 |
//	b64_json）、watermark、seed、output_format 等；非流式 200 响应
//	{"model":"doubao-seedream-5.0-lite","created":…,
//	 "data":[{"url":…,"size":"2048x2048"}],
//	 "usage":{"generated_images":1,"output_tokens":16384,"total_tokens":16384}}
//	——顶层 model 在，chat 侧回写语义直接适用；usage 的两个键与
//	usage.FieldArkImageEach / FieldArkImageToken 的计价量逐字对应。
//	组图部分失败挂在 data[].error、HTTP 仍 200（透传语义下无需特判）；
//	请求级错误是 OpenAI 形 {error:{code,message}}。
//	产物 URL 是 24h 时效的厂商签名地址：同步调用即拿即下，时效由客户端消化，
//	本入口**不**代理下载也不合成设备地址。
//
// §15.1：本文件绝不把请求/响应 body 传入 logger；usage/size 只进 reqInfo
// 内存点位，刻意不进访问日志（访问日志对 body 只记长度/哈希/类型）。
package gateway

import (
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// arkImagesPath 是方舟出图相对端点根的官方路径（端点根已含 /api/v3 或
// /api/plan/v3，见 internal/upstream 的内置端点表）。
const arkImagesPath = "/images/generations"

// handleArkImagesGenerations 是 POST /ark/api/v3/images/generations 的入口
// （已过认证中间件）。
func (s *Server) handleArkImagesGenerations(w http.ResponseWriter, r *http.Request) {
	errStyle := entryErrorStyle(r)
	// 图片体与视频体同一形态风险（base64 素材内嵌可到几十 MB）：同一上限
	// 硬拦（超限 413），解码直读 body 流式进行（entry.go）。
	r.Body = http.MaxBytesReader(w, r.Body, videoSubmitBodyLimit)
	payload, model, ok := decodeEntryPayload(w, r, errStyle)
	if !ok {
		return
	}
	// 自此本请求要入账（iteration-9）。图片**不估算**：厂商 usage 拿不到就
	// 记 0 元 + estimated 标 + 告警（recordUsage），所以这里不挂估算器。
	info := beginEntry(r, usage.EntryImage, model)
	// 预算准入（iteration-9）：鉴权之后、选路之前，与其余消费入口同一位置。
	if !s.admit(w, r, errStyle) {
		return
	}
	// kind=image 选路：种类不符与不可服务同回 404（闸门双向）。图片种类没有
	// protocol_mismatch 口径——resolveRoute 的互指提示只在文本种类内生效，
	// 本入口的 status 只会是 OK / ModelNotFound / StoreError。
	cands, status := s.resolveRoute(r.Context(), model, store.ModelKindImage, config.ProtocolArkImage)
	switch status {
	case routeModelNotFound, routeProtocolMismatch: // 后者按口径不可达，防御合并
		errStyle(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
		return
	case routeStoreError:
		errStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	s.forward(w, r, cands, forwardSpec{
		protocol: config.ProtocolArkImage,
		path:     arkImagesPath,
		model:    model,
		body: func(upstreamModelID string) ([]byte, error) {
			payload["model"] = upstreamModelID
			return encodeJSON(payload)
		},
		errStyle: errStyle,
		observe:  imageUsageObserver(info, payload),
	})
}

// imageUsageObserver 构造图片入口的响应观察器：厂商 usage 原文、出图张数与
// 请求 size 参数记入 reqInfo（账单事实内存记录点位，iteration-9 的计价输入）。
// 只在 2xx 载荷上被调用（forwardSpec.observe 的契约），解析不出对象的响应
// 根本到不了这里——观测是旁路，绝不影响透传，也绝不落日志。
//
// 两种响应形态同一个闭包收：
//
//	非流式  顶层 {data:[…], usage:{…}}——张数 = data[] 长度（含逐图 error
//	        条目：组图部分失败照样计费，张数语义以 usage.generated_images 为准）；
//	流式    image_generation.* 事件——usage 挂在 completed 事件上，
//	        partial_succeeded 每条是一张图。**这一路不覆盖就是整条流式出图漏账**
//	        （chat 的 usage chunk 与它形态不同，共用不了）。
//
// 取值一律"取到就用、取不到就当没有"，绝不假设结构：非流式形态已由真值验证，
// 流式形态仍只据文档（本机没有触发过 stream:true 的真调用）。
func imageUsageObserver(info *reqInfo, payload map[string]any) func(map[string]any) {
	reqSize, _ := payload["size"].(string) // 提交时即固化，闭包先取——payload 的 model 键随尝试改写
	return func(m map[string]any) {
		info.imageReqSize = reqSize
		if u := jsonObjectText(m["usage"]); u != "" {
			info.imageUsage = u
		}
		if data, ok := m["data"].([]any); ok {
			info.imageCount = len(data)
		} else if jsonString(m["type"]) == imageEventPartialOK {
			info.imageCount++
		}
	}
}

// imageEventPartialOK 是流式出图里"又出好一张"的事件名（方舟
// image_generation.* 事件族）。
const imageEventPartialOK = "image_generation.partial_succeeded"
