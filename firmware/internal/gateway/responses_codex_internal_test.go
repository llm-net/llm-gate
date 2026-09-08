// responses_codex_internal_test.go 直测 Codex 订阅分支的输入过滤（包内）：
// 目录面合成 id 的形状与过滤前缀必须一致，过滤只认设备自造且不可回放的
// reasoning item，没有命中时 input 原值不换。走 HTTP 的端到端在
// responses_codex_test.go。
package gateway

import (
	"strings"
	"testing"
)

func TestCatalogReasoningIDPrefixMatchesItemID(t *testing.T) {
	respID := "resp_" + newRequestID()
	if id := itemID(respID, "rs", 0); !strings.HasPrefix(id, catalogReasoningIDPrefix) {
		t.Fatalf("目录面合成的 reasoning id %q 不带前缀 %q：订阅分支的过滤会静默失效", id, catalogReasoningIDPrefix)
	}
	for _, kind := range []string{"msg", "fc"} {
		if id := itemID(respID, kind, 1); strings.HasPrefix(id, catalogReasoningIDPrefix) {
			t.Fatalf("%s 类 item 的 id %q 不该命中 reasoning 前缀", kind, id)
		}
	}
}

func TestDropCatalogReasoningItems(t *testing.T) {
	device := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "reasoning", "summary": []any{}}
	}
	cases := []struct {
		name        string
		input       any // nil 表示不带 input
		wantDropped int
		wantLen     int
	}{
		{"缺 input", nil, 0, 0},
		{"input 是字符串", "hi", 0, 0},
		{"空数组", []any{}, 0, 0},
		{"只有设备条目", []any{device("rs_resp_a_0")}, 1, 0},
		{"混合且保序", []any{"junk", device("rs_resp_a_0"), map[string]any{"type": "message"}, device("rs_resp_b_3")}, 2, 2},
		{"带 encrypted_content 的不摘", []any{map[string]any{"id": "rs_resp_a_0", "type": "reasoning", "encrypted_content": "x"}}, 0, 1},
		{"OpenAI 自己的 reasoning 不摘", []any{map[string]any{"id": "rs_abc", "type": "reasoning", "summary": []any{}}}, 0, 1},
		{"非 reasoning 类型即使带前缀也不摘", []any{map[string]any{"id": "rs_resp_a_0", "type": "message"}}, 0, 1},
		{"id 不是字符串", []any{map[string]any{"id": 1, "type": "reasoning"}}, 0, 1},
		{"没有 id", []any{map[string]any{"type": "reasoning", "summary": []any{}}}, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"model": "m"}
			if tc.input != nil {
				payload["input"] = tc.input
			}
			orig := payload["input"]
			if got := dropCatalogReasoningItems(payload); got != tc.wantDropped {
				t.Fatalf("dropped = %d，期望 %d", got, tc.wantDropped)
			}
			in, isSlice := orig.([]any)
			if tc.wantDropped == 0 {
				// 没命中：input 原值不换——切片连头都是同一个，非切片按值相等。
				switch {
				case isSlice:
					after, _ := payload["input"].([]any)
					if len(after) != len(in) || (len(in) > 0 && &after[0] != &in[0]) {
						t.Fatalf("没有命中却换了 input：%v → %v", in, after)
					}
				case payload["input"] != orig:
					t.Fatalf("没有命中却换了 input：%v → %v", orig, payload["input"])
				}
				return
			}
			after, _ := payload["input"].([]any)
			if len(after) != tc.wantLen {
				t.Fatalf("剩余条数 = %d，期望 %d：%v", len(after), tc.wantLen, after)
			}
			// 保序：剩下的就是原序列去掉命中项。
			j := 0
			for _, raw := range in {
				if isCatalogReasoningItem(raw) {
					continue
				}
				if j >= len(after) || !sameAny(after[j], raw) {
					t.Fatalf("剩余条目乱序或被改：%v", after)
				}
				j++
			}
		})
	}
}

// sameAny 比较两个 input 条目是否是同一个值（map 比引用，标量比值）。
func sameAny(a, b any) bool {
	if am, ok := a.(map[string]any); ok {
		bm, ok := b.(map[string]any)
		return ok && len(am) == len(bm) && (len(am) == 0 || func() bool {
			for k := range am {
				if _, ok := bm[k]; !ok {
					return false
				}
			}
			return true
		}())
	}
	return a == b
}
