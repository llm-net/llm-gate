package gateway

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestReplaceInvalidToolImages(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			input := `{"input":[{"type":"` + kind + `","call_id":"call_preview","status":"completed","output":[
				{"type":"input_text","text":"saved media/original.png"},
				{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U=","detail":"high"},
				{"type":"input_image","image_url":"data:image/png;base64,aW...[truncated]...==","detail":"high"},
				{"type":"input_image","image_url":"data:image/jpeg;base64,"},
				{"type":"input_image","image_url":"https://example.com/image.png"},
				{"type":"input_text","text":"after"}]}]}`
			var payload map[string]any
			if err := json.Unmarshal([]byte(input), &payload); err != nil {
				t.Fatal(err)
			}
			if n := replaceInvalidToolImages(payload); n != 2 {
				t.Fatalf("replaced = %d, want 2", n)
			}
			item := payload["input"].([]any)[0].(map[string]any)
			output := item["output"].([]any)
			if item["call_id"] != "call_preview" || item["status"] != "completed" || len(output) != 6 {
				t.Fatal("tool identity or output ordering changed")
			}
			for _, i := range []int{2, 3} {
				part := output[i].(map[string]any)
				if part["type"] != "input_text" || len(part) != 2 || !strings.Contains(part["text"].(string), "Read the original image again") {
					t.Fatal("invalid image must become an explicit reread notice")
				}
			}
			var original map[string]any
			_ = json.Unmarshal([]byte(input), &original)
			before := original["input"].([]any)[0].(map[string]any)["output"].([]any)
			for _, i := range []int{0, 1, 4, 5} {
				if !reflect.DeepEqual(output[i], before[i]) {
					t.Fatalf("unaffected content at %d changed", i)
				}
			}
			if n := replaceInvalidToolImages(payload); n != 0 {
				t.Fatal("replacement is not idempotent")
			}
		})
	}
}

func TestToolImageGuardLeavesOtherInputsUntouched(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"input":"hello"}`, `{"input":[null,42,"text"]}`,
		`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%%%"}]}]}`,
		`{"input":[{"type":"unknown","output":[{"type":"input_image","image_url":"data:image/png;base64,%%%"}]}]}`,
		`{"input":[{"type":"function_call_output","output":"data:image/png;base64,%%%"}]}`,
		`{"input":[{"type":"function_call_output","output":[null,42,{"type":"input_image","file_id":"file_1"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`,
	} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(payload)
		if n := replaceInvalidToolImages(payload); n != 0 {
			t.Fatal("unrelated input was replaced")
		}
		after, _ := json.Marshal(payload)
		if string(before) != string(after) {
			t.Fatal("unrelated payload changed")
		}
	}
}

func TestValidInlineToolImage(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		valid     bool
	}{
		{"padded", "data:image/png;base64,aW1hZ2U=", true},
		{"unpadded", "data:image/jpeg;base64,aW1hZ2U", true},
		{"webp", "data:image/webp;base64,aW1hZ2U=", true},
		{"mime parameters", "data:image/png;charset=binary;base64,aW1hZ2U=", true},
		{"newlines", "data:image/png;base64,aW1h\r\nZ2U=", true},
		{"large", "data:image/png;base64," + strings.Repeat("YWJj", 1<<18), true},
		{"truncated", "data:image/png;base64,a", false},
		{"placeholder", "data:image/png;base64,[omitted]", false},
		{"empty", "data:image/png;base64,", false},
		{"whitespace only", "data:image/png;base64,\r\n", false},
		{"invalid alphabet", "data:image/png;base64,%%%", false},
		{"bad padding", "data:image/png;base64,aW1hZ2U===", false},
		{"not image mime", "data:text/plain;base64,aW1hZ2U=", false},
		{"missing mime subtype", "data:image/;base64,aW1hZ2U=", false},
		{"missing marker", "data:image/png,aW1hZ2U=", false},
		{"missing comma", "data:image/png;base64", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validInlineToolImage(tc.url); got != tc.valid {
				t.Fatalf("valid = %v, want %v", got, tc.valid)
			}
		})
	}
}

// toolImageHistory 造一段 Codex 回放形态的会话历史：一条带图片附件的用户消息，随后
// 每张图一对 function_call / function_call_output（说明文字 + 内联图片），外加一条
// 远程 URL 的工具图片。sizes 是各张内联图片的 data URL 字节数。
func toolImageHistory(sizes ...int) map[string]any {
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("VVNFUg", 1<<16)},
	}}}
	for i, n := range sizes {
		id := "call_" + strconv.Itoa(i)
		input = append(input,
			map[string]any{"type": "function_call", "call_id": id, "name": "view_image", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": id, "output": []any{
				map[string]any{"type": "input_text", "text": "media/frame-" + strconv.Itoa(i) + ".png"},
				map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64," + strings.Repeat("A", n-len("data:image/jpeg;base64,")), "detail": "high"},
			}})
	}
	input = append(input, map[string]any{"type": "custom_tool_call_output", "call_id": "call_remote", "output": []any{
		map[string]any{"type": "input_image", "image_url": "https://example.com/remote.png"},
	}})
	return map[string]any{"model": "gpt-test", "input": input}
}

func encodedLen(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	body, err := encodeJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(body))
}

// omittedToolImages 返回被换成省略说明的工具图片序号（按 toolImageHistory 的第几张）。
func omittedToolImages(payload map[string]any) []int {
	var got []int
	n := 0
	for _, raw := range payload["input"].([]any) {
		item := raw.(map[string]any)
		if item["type"] != "function_call_output" {
			continue
		}
		if part := item["output"].([]any)[1].(map[string]any); part["text"] == omittedToolImageNotice {
			got = append(got, n)
		}
		n++
	}
	return got
}

func repeatSize(n, size int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = size
	}
	return s
}

func TestOmitEarlyToolImagesFitsBudget(t *testing.T) {
	const budget = agentsResponsesBodyLimit
	small := toolImageHistory(repeatSize(8, 300<<10)...)
	before, _ := json.Marshal(small)
	if fitted, n := omitEarlyToolImages(small, encodedLen(t, small), budget); n != 0 || fitted != int64(len(before))+1 {
		t.Fatalf("under budget: omitted %d, fitted %d", n, fitted)
	}
	if after, _ := json.Marshal(small); string(after) != string(before) {
		t.Fatal("payload under budget must stay untouched")
	}

	payload := toolImageHistory(repeatSize(40, 330<<10)...)
	size := encodedLen(t, payload)
	original, _ := json.Marshal(payload)
	fitted, n := omitEarlyToolImages(payload, size, budget)
	actual := encodedLen(t, payload)
	if fitted > budget || actual > budget {
		t.Fatalf("still over budget: estimated %d, actual %d", fitted, actual)
	}
	if diff := fitted - actual; diff < -int64(n)*64 || diff > int64(n)*64 {
		t.Fatalf("size estimate %d drifted from encoded %d", fitted, actual)
	}
	want := make([]int, n)
	for i := range want {
		want[i] = i
	}
	if got := omittedToolImages(payload); n == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("omitted images = %v, want the oldest %d", got, n)
	}
	// 省略量按步长取整，省略后留出至少一个步长以内的余量。
	if saved := size - fitted; saved < size-budget || saved > size-budget+toolImageOmitStep+330<<10 {
		t.Fatalf("saved %d bytes for an excess of %d", saved, size-budget)
	}

	var orig map[string]any
	_ = json.Unmarshal(original, &orig)
	items, origItems := payload["input"].([]any), orig["input"].([]any)
	for i, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "function_call_output" && item["output"].([]any)[1].(map[string]any)["text"] == omittedToolImageNotice {
			if item["call_id"] != origItems[i].(map[string]any)["call_id"] || !reflect.DeepEqual(item["output"].([]any)[0], origItems[i].(map[string]any)["output"].([]any)[0]) {
				t.Fatalf("item %d: tool identity or caption changed", i)
			}
			continue
		}
		if !reflect.DeepEqual(any(item), origItems[i]) {
			t.Fatalf("item %d (%v) must stay untouched", i, item["type"])
		}
	}
}

// 历史只追加时，省略集合按步长跳变：没跨过步长边界的新请求省略同一批图片（前缀不变），
// 跨过之后在原集合上继续往后省略。
func TestOmitEarlyToolImagesStableAcrossGrowth(t *testing.T) {
	const budget = agentsResponsesBodyLimit
	omit := func(sizes []int) []int {
		p := toolImageHistory(sizes...)
		if fitted, _ := omitEarlyToolImages(p, encodedLen(t, p), budget); fitted > budget {
			t.Fatalf("%d images did not fit", len(sizes))
		}
		return omittedToolImages(p)
	}
	sizes := repeatSize(30, 330<<10)
	first := omit(sizes)
	if len(first) == 0 {
		t.Fatal("expected omissions")
	}
	changes := 0
	prev := first
	for range 20 {
		sizes = append(sizes, 330<<10)
		got := omit(sizes)
		if !reflect.DeepEqual(got, prev) {
			if len(got) <= len(prev) || !reflect.DeepEqual(got[:len(prev)], prev) {
				t.Fatalf("omission set %v does not extend %v", got, prev)
			}
			changes++
		}
		prev = got
	}
	// 20 × 330 KiB ≈ 6.4 MiB 的增长，2 MiB 步长下只该跳变三四次。
	if changes == 0 || changes > 4 {
		t.Fatalf("omission set changed %d times over 20 appended images", changes)
	}
}

func TestOmitEarlyToolImagesKeepsLatestImage(t *testing.T) {
	const budget = agentsResponsesBodyLimit
	// 两张大图：只能省略较早那张，最新的一张恒保留。
	payload := toolImageHistory(5<<20, 5<<20)
	fitted, n := omitEarlyToolImages(payload, encodedLen(t, payload), budget)
	if n != 1 || fitted > budget || !reflect.DeepEqual(omittedToolImages(payload), []int{0}) {
		t.Fatalf("omitted %d (%v), fitted %d", n, omittedToolImages(payload), fitted)
	}
	// 只剩一张图、文字本身超限：让不出空间，由调用方答 413。
	payload = toolImageHistory(9 << 20)
	if fitted, n := omitEarlyToolImages(payload, encodedLen(t, payload), budget); n != 0 || fitted <= budget {
		t.Fatalf("single oversized image: omitted %d, fitted %d", n, fitted)
	}
}
