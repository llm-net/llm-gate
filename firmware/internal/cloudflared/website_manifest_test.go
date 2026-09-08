package cloudflared_test

import (
	"os"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
)

// TestWebsiteManifestVerifies 钉住官网伺服的那份 cloudflared 清单：用固件内嵌公钥能验签、
// 逐条过形状校验——改了清单没重签，这里先红。
func TestWebsiteManifestVerifies(t *testing.T) {
	raw, err := os.ReadFile("../../../website/public/updates/components/cloudflared/stable.json")
	if err != nil {
		t.Skip("website manifest not present")
	}
	sig, err := os.ReadFile("../../../website/public/updates/components/cloudflared/stable.json.sig")
	if err != nil {
		t.Fatal(err)
	}
	v, err := cloudflared.ParseIndex(raw, sig, cloudflared.TrustedKeys())
	if err != nil {
		t.Fatalf("官网清单验签/校验失败: %v", err)
	}
	if len(v.Index.Releases) == 0 || v.Index.Releases[0].Platform != "linux-arm64" {
		t.Fatalf("首条不是 linux-arm64: %+v", v.Index.Releases)
	}
}
