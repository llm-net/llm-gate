// responses_tool_images.go 处理 Responses 会话历史里工具结果携带的内联图片：替换
// 无效图片（replaceInvalidToolImages，仅 Codex 订阅），以及请求体超过转发上限时省略
// 较早的图片（omitEarlyToolImages，Agent 面三个 Responses 入口）。
package gateway

import (
	"encoding/base64"
	"io"
	"mime"
	"strings"
)

// replaceInvalidToolImages prevents a malformed inline image in Codex tool
// history from rejecting the whole turn. Keep the result and its call ID, and
// tell the model to read the source again instead of claiming it saw the image.
// User attachments, remote URLs and unknown item shapes remain upstream-owned.
// Neither the image nor adjacent tool text may be logged (§15.1).
func replaceInvalidToolImages(payload map[string]any) int {
	items, _ := payload["input"].([]any)
	replaced := 0
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
			continue
		}
		output, _ := item["output"].([]any)
		for i, raw := range output {
			part, _ := raw.(map[string]any)
			url, ok := part["image_url"].(string)
			if part["type"] != "input_image" || !ok || !strings.HasPrefix(url, "data:") || validInlineToolImage(url) {
				continue
			}
			output[i] = map[string]any{
				"type": "input_text",
				"text": "[This tool image could not be delivered because its inline image data is invalid. Read the original image again with an image tool if needed; do not infer its contents from this result. Do not repeat media generation merely to recover this preview.]",
			}
			replaced++
		}
	}
	return replaced
}

func validInlineToolImage(url string) bool {
	header, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || data == "" || !strings.HasSuffix(header, ";base64") {
		return false
	}
	typ, _, err := mime.ParseMediaType(strings.TrimSuffix(header, ";base64"))
	if err != nil || !strings.HasPrefix(typ, "image/") || len(typ) == len("image/") {
		return false
	}
	// Stream validation keeps memory bounded even with many large history images.
	// Accept both padded and unpadded standard Base64, without rewriting either.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		n, err := io.Copy(io.Discard, base64.NewDecoder(enc, strings.NewReader(data)))
		if err == nil && n > 0 {
			return true
		}
	}
	return false
}

// toolImageOmitStep 是省略较早工具图片的粒度：超出转发上限的字节数向上取整到它的
// 整数倍再省略。会话历史只追加不改写，省略哪几张只取决于取整后的量，所以相邻多次
// 请求省略的是同一批图片、转发内容的前缀不变（上游提示缓存按前缀命中），历史每再长
// 这么多字节才多省略一批。
const toolImageOmitStep = 2 << 20

const omittedToolImageNotice = "[An earlier tool image was omitted from this request to keep it within the device's request size limit. View the file again if you still need it; do not infer its contents from this note.]"

// omitEarlyToolImages 把原始体 size 字节、超过 budget 的 Responses 请求收进 budget：
// 从最早的工具结果起，把 function_call_output / custom_tool_call_output 的 output[]
// 里内联（data:）的 input_image 换成文字说明，最后一张工具图片恒保留（模型刚要来看
// 的就是它）。工具调用 ID、相邻文字、用户附件、远程 URL 与未知结构不动。返回省略后
// 的估算字节数与省略张数；估算值仍超过 budget 表示图片让不出足够空间。图片与相邻
// 文字都不进日志（§15.1）。
func omitEarlyToolImages(payload map[string]any, size, budget int64) (int64, int) {
	if size <= budget {
		return size, 0
	}
	type toolImage struct {
		output []any
		index  int
		bytes  int64
	}
	var images []toolImage
	items, _ := payload["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
			continue
		}
		output, _ := item["output"].([]any)
		for i, raw := range output {
			part, _ := raw.(map[string]any)
			if url, ok := part["image_url"].(string); ok && part["type"] == "input_image" && strings.HasPrefix(url, "data:") {
				images = append(images, toolImage{output: output, index: i, bytes: int64(len(url))})
			}
		}
	}
	if len(images) > 0 {
		images = images[:len(images)-1]
	}
	need := (size - budget + toolImageOmitStep - 1) / toolImageOmitStep * toolImageOmitStep
	var saved int64
	omitted := 0
	for _, img := range images {
		if saved >= need {
			break
		}
		img.output[img.index] = map[string]any{"type": "input_text", "text": omittedToolImageNotice}
		saved += img.bytes - int64(len(omittedToolImageNotice))
		omitted++
	}
	return size - saved, omitted
}
