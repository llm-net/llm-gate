package store

import (
	"regexp"
	"testing"
	"time"
)

func TestNewULID(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewULID(at)
		if !shape.MatchString(id) {
			t.Fatalf("ULID %q 不是 26 位 Crockford base32", id)
		}
		if seen[id] {
			t.Fatalf("ULID 重复：%q", id)
		}
		seen[id] = true
		got, ok := ULIDTime(id)
		if !ok || !got.Equal(at) {
			t.Fatalf("ULIDTime(%q) = %v/%v，期望 %v", id, got, ok, at)
		}
	}
	// 时间戳在前：晚一毫秒的 ID 字典序恒在后。
	if a, b := NewULID(at), NewULID(at.Add(time.Millisecond)); a >= b {
		t.Fatalf("字典序不随时间递增：%q >= %q", a, b)
	}
	for _, bad := range []string{"", "preview-1", "0123456789ABCDEFGHJKMNPQRI", "01ARZ3NDEKTSV4RRFFQ69G5FA"} {
		if _, ok := ULIDTime(bad); ok {
			t.Fatalf("%q 不该被认成 ULID", bad)
		}
	}
	if _, ok := ULIDTime("01arz3ndektsv4rrffq69g5fav"); !ok {
		t.Fatal("小写 ULID 应当可解析")
	}
}
