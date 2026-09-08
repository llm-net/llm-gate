package mihomo_test

import (
	"os"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
	"github.com/llm-net/llm-gate/firmware/internal/mihomo"
)

func TestWebsiteManifestVerifies(t *testing.T) {
	raw, err := os.ReadFile("../../../website/public/updates/components/mihomo/stable.json")
	if err != nil {
		t.Skip("website manifest not present")
	}
	sig, err := os.ReadFile("../../../website/public/updates/components/mihomo/stable.json.sig")
	if err != nil {
		t.Fatal(err)
	}
	v, err := mihomo.ParseIndex(raw, sig, cloudflared.TrustedKeys())
	if err != nil {
		t.Fatalf("官网清单验签/校验失败: %v", err)
	}
	if v.Index.Releases[0].Version != "1.19.30" {
		t.Fatalf("版本不对: %+v", v.Index.Releases[0])
	}
}
