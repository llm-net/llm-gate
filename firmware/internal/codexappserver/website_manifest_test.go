package codexappserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
)

const websiteManifestPath = "../../../website/public/updates/components/codex-app-server/stable.json"

// TestWebsiteManifestVerifies 用内嵌公钥验官网清单的签名。签名文件由维护者用
// `tools/componentsign` 生成；没有 .sig 时跳过并说明，内容合法性由下一个测试保证。
func TestWebsiteManifestVerifies(t *testing.T) {
	raw, err := os.ReadFile(websiteManifestPath)
	if err != nil {
		t.Skip("website manifest not present")
	}
	sig, err := os.ReadFile(websiteManifestPath + ".sig")
	if err != nil {
		t.Skip("官网清单尚未签名（缺 stable.json.sig）：需维护者用 tools/componentsign 与 .component-signing-key 签名后提交")
	}
	v, err := ParseIndex(raw, sig, cloudflared.TrustedKeys())
	if err != nil {
		t.Fatalf("官网清单验签/校验失败: %v", err)
	}
	if v.Index.Releases[0].Version != "0.154.0" {
		t.Fatalf("版本不对: %+v", v.Index.Releases[0])
	}
}

// TestWebsiteManifestContent 用一次性密钥签官网清单正文，确认内容本身通过全部策略校验，
// 两个平台各有一条可安装的 0.154.0。
func TestWebsiteManifestContent(t *testing.T) {
	raw, err := os.ReadFile(websiteManifestPath)
	if err != nil {
		t.Skip("website manifest not present")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := json.Marshal(signatureFile{Schema: SignatureSchema, KeyID: "throwaway", Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))})
	v, err := ParseIndex(raw, sig, map[string]ed25519.PublicKey{"throwaway": pub})
	if err != nil {
		t.Fatalf("官网清单内容不合法: %v", err)
	}
	if v.Index.Revision != 2 {
		t.Fatalf("revision 不对: %d", v.Index.Revision)
	}
	for _, platform := range []string{"linux-arm64", "linux-amd64"} {
		adv := v.Index.selectRelease(platform, "")
		if adv.Latest == nil || adv.Latest.Version != "0.154.0" || adv.Latest.Packaging != "tar.gz" {
			t.Fatalf("%s 没有可安装的 0.154.0: %+v", platform, adv)
		}
		if !strings.Contains(adv.Latest.ArtifactURL, "/codex-app-server-package-") || adv.Latest.MinComponentManager != ComponentManagerVersion {
			t.Fatalf("%s 应指向整包 asset 并要求管理器版本 %d: %+v", platform, ComponentManagerVersion, adv.Latest)
		}
		if adv.Latest.UnpackedSizeBytes <= adv.Latest.SizeBytes {
			t.Fatalf("%s 解包长度应大于压缩包: %+v", platform, adv.Latest)
		}
	}
}
