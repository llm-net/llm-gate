// keyauth_internal_test.go 用注入时钟直测 StoreKeyAuthorizer 的节流窗口：
// 60s 内合并写、超窗再写（HTTP 侧的 60s 内单次落库由 server_test.go 的
// TestLastUsedThrottle 覆盖），以及 Authorize 的返回值契约。
package gateway

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestStoreKeyAuthorizerTouchWindow(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const plaintext = "sk_throttle_internal_0123456789"
	k, err := st.CreateAPIKey(t.Context(), "t", keyDigest(plaintext), "sk_throttle_", "6789", plaintext)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	a := NewStoreKeyAuthorizer(st, logging.New(io.Discard, slog.LevelDebug))
	base := time.Now()
	a.now = func() time.Time { return base }

	lastUsed := func() time.Time {
		t.Helper()
		keys, err := st.ListAPIKeys(t.Context())
		if err != nil || len(keys) != 1 {
			t.Fatalf("ListAPIKeys: %v（%d 行）", err, len(keys))
		}
		return keys[0].LastUsedAt
	}

	ident, ok := a.Authorize(t.Context(), plaintext)
	if !ok || ident.KeyID != k.ID || ident.KeyDisplay != "sk_throttle_…6789" {
		t.Fatalf("Authorize = (%+v, %v)，期望 (key %d, true)", ident, ok, k.ID)
	}
	t1 := lastUsed()
	if t1.IsZero() {
		t.Fatal("首次 Authorize 应写 last_used_at")
	}

	// 窗口内（时钟不动）：不再落库。5ms 睡眠保证若误写会产生可区分的毫秒时间戳。
	time.Sleep(5 * time.Millisecond)
	if _, ok := a.Authorize(t.Context(), plaintext); !ok {
		t.Fatal("窗口内 Authorize 应放行")
	}
	if t2 := lastUsed(); !t2.Equal(t1) {
		t.Errorf("60s 窗口内重复落库: %v → %v", t1, t2)
	}

	// 超窗（前进 61s）：应再次落库。
	a.now = func() time.Time { return base.Add(61 * time.Second) }
	if _, ok := a.Authorize(t.Context(), plaintext); !ok {
		t.Fatal("超窗 Authorize 应放行")
	}
	if t3 := lastUsed(); !t3.After(t1) {
		t.Errorf("超过 60s 后应重新落库: %v → %v", t1, t3)
	}

	// 未知 Key：拒绝且不 panic。
	if _, ok := a.Authorize(t.Context(), "sk_no_such_key_000000000000"); ok {
		t.Error("未知 Key 不应放行")
	}
}

// TestStoreKeyAuthorizerAcceptsImportedKey 钉住前缀只影响新签发：从配置导入的、
// 别的形状的密钥照样按整串明文摘要鉴权，不做迁移或重写。
func TestStoreKeyAuthorizerAcceptsImportedKey(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const plaintext = "imported_existing_key_0123456789abcdef"
	k, err := st.CreateAPIKey(t.Context(), "existing", keyDigest(plaintext), "imported_exi", "cdef", plaintext)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	a := NewStoreKeyAuthorizer(st, logging.New(io.Discard, slog.LevelDebug))
	ident, ok := a.Authorize(t.Context(), plaintext)
	if !ok || ident.KeyID != k.ID || ident.KeyDisplay != "imported_exi…cdef" {
		t.Fatalf("Authorize = (%+v, %v)，期望导入的现有 Key 继续有效", ident, ok)
	}
}
