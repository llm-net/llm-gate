package gateway

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestCursorCorrelationIsolationAndExpiry(t *testing.T) {
	c := new(cursorRequestModels)
	id := "fake-subscription-handle"
	digest := sha256.Sum256([]byte(id))
	c.put(1, id, "model-a")
	c.put(2, id, "model-b")
	if c.take(3, digest) != "" || c.take(1, digest) != "model-a" || c.take(2, digest) != "model-b" || c.take(1, digest) != "" {
		t.Fatal("cross-key or duplicate model correlation")
	}
	c.put(1, id, "model-a")
	c.put(1, id, "model-b")
	if c.take(1, digest) != "" {
		t.Fatal("conflicting model accepted")
	}
	c.put(1, id, "model-a")
	key := cursorRequestKey{1, digest}
	v := c.entries[key]
	v.at = time.Now().Add(-cursorPendingTTL - time.Second)
	c.entries[key] = v
	if c.take(1, digest) != "" {
		t.Fatal("expired handle accepted")
	}
}
