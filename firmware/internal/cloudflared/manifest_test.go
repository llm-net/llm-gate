package cloudflared

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// signer 是测试专用的清单签发器：自造一对密钥，模拟 tools/componentsign 的输出。
type signer struct {
	id   string
	priv ed25519.PrivateKey
	keys map[string]ed25519.PublicKey
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{id: "test-key", priv: priv, keys: map[string]ed25519.PublicKey{"test-key": pub}}
}

func (s *signer) sign(raw []byte) []byte {
	sig, _ := json.Marshal(signatureFile{
		Schema: SignatureSchema, KeyID: s.id, Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, raw)),
	})
	return sig
}

const testSHA = "7747d94570fb390cf47dcb4f9555c193c6355cda9793f0d878d9049e5d6a7790"

func sampleIndex(revision int64, rels ...Release) []byte {
	raw, _ := json.Marshal(Index{
		Schema: IndexSchema, Component: ComponentName, Channel: "stable", Revision: revision,
		UpdatedAt: "2026-08-26T00:00:00Z", Releases: rels,
	})
	return raw
}

func rel(version, platform string, sha string, size int64) Release {
	return Release{
		Version: version, Platform: platform,
		ArtifactURL:    "https://github.com/cloudflare/cloudflared/releases/download/" + version + "/cloudflared-" + platform,
		ArtifactSHA256: sha, SizeBytes: size, License: "Apache-2.0", MinComponentManager: 1,
		AllowInstall: true, AllowUpdate: true,
	}
}

func TestParseIndexVerifiesSignatureAndShape(t *testing.T) {
	s := newSigner(t)
	raw := sampleIndex(3, rel("2026.8.2", "linux-arm64", testSHA, 37404344))
	v, err := ParseIndex(raw, s.sign(raw), s.keys)
	if err != nil {
		t.Fatalf("合法清单应通过: %v", err)
	}
	if v.Index.Revision != 3 || len(v.Index.Releases) != 1 || v.KeyID != "test-key" || len(v.SHA256) != 64 {
		t.Fatalf("解析结果不对: %+v", v)
	}

	other := newSigner(t)
	if _, err := ParseIndex(raw, other.sign(raw), s.keys); err == nil {
		t.Fatal("未知密钥签名应被拒")
	}
	tampered := []byte(strings.Replace(string(raw), "2026.8.2", "2026.8.3", 1))
	if _, err := ParseIndex(tampered, s.sign(raw), s.keys); err == nil {
		t.Fatal("篡改内容应验签失败")
	}
	if _, err := ParseIndex(raw, []byte(`{"schema":"x"}`), s.keys); err == nil {
		t.Fatal("签名文件形态不符应被拒")
	}
	if _, err := ParseIndex(raw, s.sign(raw), nil); err == nil {
		t.Fatal("没有信任表应被拒")
	}

	bad := []Release{
		func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 1); r.ArtifactURL = "https://mirror.example/cloudflared-linux-arm64"; return r }(),
		func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 1); r.ArtifactURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64"; return r }(),
		func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 1); r.ArtifactURL = "http://github.com/cloudflare/cloudflared/releases/download/2026.8.2/cloudflared-linux-arm64"; return r }(),
		func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 1); r.ArtifactURL = "https://github.com/cloudflare/cloudflared/releases/download/2026.8.3/cloudflared-linux-arm64"; return r }(),
		rel("v2026.8.2", "linux-arm64", testSHA, 1),
		rel("2026.8.2", "linux-mips", testSHA, 1),
		rel("2026.8.2", "linux-arm64", strings.ToUpper(testSHA), 1),
		rel("2026.8.2", "linux-arm64", testSHA, 0),
		rel("2026.8.2", "linux-arm64", testSHA, maxArtifactBytes+1),
		func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 1); r.MinComponentManager = 0; return r }(),
	}
	for i, b := range bad {
		raw := sampleIndex(1, b)
		if _, err := ParseIndex(raw, s.sign(raw), s.keys); err == nil {
			t.Errorf("非法条目 #%d 应被拒", i)
		} else {
			var me *ManifestError
			if !errors.As(err, &me) {
				t.Errorf("非法条目 #%d 应是 ManifestError: %v", i, err)
			}
		}
	}
	dup := sampleIndex(1, rel("2026.8.2", "linux-arm64", testSHA, 1), rel("2026.8.2", "linux-arm64", testSHA, 2))
	if _, err := ParseIndex(dup, s.sign(dup), s.keys); err == nil {
		t.Error("同平台重复版本应被拒")
	}
	unknownField := []byte(strings.Replace(string(raw), `"schema"`, `"script":"rm -rf /","schema"`, 1))
	if _, err := ParseIndex(unknownField, s.sign(unknownField), s.keys); err == nil {
		t.Error("未知字段应被拒（清单不能下发代码或脚本）")
	}
	wrongSchema := sampleIndex(1)
	wrongSchema = []byte(strings.Replace(string(wrongSchema), IndexSchema, "llmgate.component-index/v2", 1))
	if _, err := ParseIndex(wrongSchema, s.sign(wrongSchema), s.keys); err == nil {
		t.Error("未知 schema 应被拒")
	}
}

func TestCheckRevisionAntiRollback(t *testing.T) {
	s := newSigner(t)
	raw := sampleIndex(5)
	v, _ := ParseIndex(raw, s.sign(raw), s.keys)
	if err := checkRevision(v, 0, ""); err != nil {
		t.Fatalf("首次接受: %v", err)
	}
	if err := checkRevision(v, 5, v.SHA256); err != nil {
		t.Fatalf("同 revision 同内容应通过: %v", err)
	}
	if err := checkRevision(v, 5, "deadbeef"); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("同 revision 换内容应拒: %v", err)
	}
	if err := checkRevision(v, 6, ""); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("revision 回退应拒: %v", err)
	}
}

func TestSelectRelease(t *testing.T) {
	idx := Index{Releases: []Release{
		rel("2026.8.2", "linux-arm64", testSHA, 10),
		func() Release { r := rel("2026.9.1", "linux-arm64", testSHA, 11); r.Blocked = true; return r }(),
		func() Release { r := rel("2026.10.1", "linux-arm64", testSHA, 12); r.MinComponentManager = 99; return r }(),
		rel("2026.7.1", "linux-amd64", testSHA, 13),
	}}
	adv := idx.selectRelease("linux-arm64", "")
	if adv.Latest == nil || adv.Latest.Version != "2026.8.2" {
		t.Fatalf("未安装时应选 2026.8.2: %+v", adv)
	}
	if adv.Newest == nil || adv.Newest.Version != "2026.10.1" || adv.NeedsFirmware {
		t.Fatalf("Newest/NeedsFirmware 不对: %+v", adv)
	}
	adv = idx.selectRelease("linux-arm64", "2026.8.2")
	if adv.Latest != nil {
		t.Fatalf("阻断与需升级固件的条目不该被选: %+v", adv.Latest)
	}
	if !adv.NeedsFirmware {
		t.Fatal("更高版本要求更高管理器时应提示升级固件")
	}
	adv = idx.selectRelease("linux-arm64", "2026.9.1")
	if !adv.InstalledBlocked {
		t.Fatal("已装版本被阻断应标出")
	}
	adv = idx.selectRelease("linux-amd64", "2026.7.1")
	if adv.Latest != nil || adv.Newest == nil || adv.Newest.Version != "2026.7.1" {
		t.Fatalf("已是最新时不该有 Latest: %+v", adv)
	}
	noInstall := Index{Releases: []Release{func() Release { r := rel("2026.8.2", "linux-arm64", testSHA, 10); r.AllowInstall = false; return r }()}}
	if adv := noInstall.selectRelease("linux-arm64", ""); adv.Latest != nil {
		t.Fatal("allowInstall=false 不能新装")
	}
	if adv := noInstall.selectRelease("linux-arm64", "2026.1.1"); adv.Latest == nil {
		t.Fatal("allowUpdate=true 应允许更新")
	}
	if compareVersion("2026.8.2", "2026.10.1") >= 0 || compareVersion("2026.8.2", "2026.8.2") != 0 || compareVersion("2027.1.0", "2026.12.9") <= 0 {
		t.Fatal("compareVersion 不对")
	}
}

func TestNormalizeHostname(t *testing.T) {
	ok := map[string]string{
		"box.example.com":     "box.example.com",
		" Box.Example.COM ":   "box.example.com",
		"xn--fsq.example.com": "xn--fsq.example.com",
		"a-b.c1.io":           "a-b.c1.io",
		"api.box.example.com": "api.box.example.com",
	}
	for in, want := range ok {
		got, err := NormalizeHostname(in)
		if err != nil || got != want {
			t.Errorf("%q → %q, %v；期望 %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "box", "*.example.com", "https://box.example.com", "box.example.com/", "box.example.com:443",
		"user@box.example.com", "box.example.com.", "192.0.2.1", "[2001:db8::1]", "box.local", "box.example.test",
		"box.home", "盒子.example.com", "-box.example.com", "box-.example.com", "box.example.123",
		strings.Repeat("a", 64) + ".example.com", "a." + strings.Repeat("b.", 130) + "com",
	}
	for _, in := range bad {
		if _, err := NormalizeHostname(in); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%q 应被拒: %v", in, err)
		}
	}
}

func sampleToken() string {
	return base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"0d6d3e0c-1c2b-4c5d-8e9f-0a1b2c3d4e5f","s":"secret-bytes"}`))
}

// sampleToken2 是轮换用的第二把 token（不同秘密）。
func sampleToken2() string {
	return base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"0d6d3e0c-1c2b-4c5d-8e9f-0a1b2c3d4e5f","s":"rotated-secret-bytes"}`))
}

func TestValidateToken(t *testing.T) {
	tok := sampleToken()
	if got, err := ValidateToken("  " + tok + "\n"); err != nil || got != tok {
		t.Fatalf("合法 token 应通过: %v", err)
	}
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"a":"1","t":"2","s":"3"}`))
	if _, err := ValidateToken(raw); err != nil {
		t.Fatalf("raw url base64 也应通过: %v", err)
	}
	bad := []string{
		"", "cloudflared service install " + tok, "sudo cloudflared tunnel run --token " + tok,
		"not base64 !!!", base64.StdEncoding.EncodeToString([]byte(`{"a":"x"}`)),
		base64.StdEncoding.EncodeToString([]byte(`hello`)), strings.Repeat("A", maxTokenLen+4),
	}
	for _, in := range bad {
		if _, err := ValidateToken(in); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%.30q 应被拒: %v", in, err)
		}
	}
}
