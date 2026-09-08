// Package mihomo 管理板上可选的代理内核组件（docs-dev/firmware-egress-proxy.md §8）：
// 官网签名清单 → Mihomo 官方 GitHub release 精确 asset（gzip）→ 长度/摘要/解压上限/ELF/自述
// 版本校验 → 升级引擎装进 A/B 槽 → 专用用户的 systemd unit；订阅只取 `proxies` 节点，由本包
// 生成受限配置（只绑 127.0.0.1 的 SOCKS 端口，无 LAN 入站、TUN、controller、DNS 劫持），
// 内核起来后把出站代理 profile 指向 127.0.0.1:<端口>。
//
// 边界：llmgate 不 import / link Mihomo，只经标准 SOCKS5 通信；固件包与官网都不携带 Mihomo
// 可执行文件；订阅 URL、节点凭据是上游凭据（device-key 密封、不进日志/审计/API 响应）；
// 内核失败只让选择了经代理的流量失败关闭，网关与管理台照常。
package mihomo

// 签名组件清单：官网 /updates/components/mihomo/stable.json 及其离线签名 .sig。形态与
// cloudflared 同源（llmgate.component-index/v1），差别只在：制品是 gzip 包（清单多声明
// 解压后长度与摘要）、官方来源是 MetaCubeX/mihomo 的 release、版本是三段数字。

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	IndexSchema     = "llmgate.component-index/v1"
	SignatureSchema = "llmgate.component-signature/v1"
	ComponentName   = "mihomo"
	// ComponentManagerVersion 是本固件对 Mihomo 组件的管理器协议版本。
	ComponentManagerVersion = 1

	officialHost       = "github.com"
	officialPathPrefix = "/MetaCubeX/mihomo/releases/download/"
	// maxArtifactBytes / maxUnpackedBytes 是任何条目允许声明的上限（v1.19 压缩约 17 MiB、
	// 解压约 47 MiB）。
	maxArtifactBytes = 96 << 20
	maxUnpackedBytes = 192 << 20
)

// artifactRedirectHosts 是官方 release 直下允许经过的主机。
var artifactRedirectHosts = []string{
	"github.com",
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",
	"github-releases.githubusercontent.com",
}

// Index 是清单本体。
type Index struct {
	Schema    string    `json:"schema"`
	Component string    `json:"component"`
	Channel   string    `json:"channel"`
	Revision  int64     `json:"revision"`
	UpdatedAt string    `json:"updatedAt"`
	Releases  []Release `json:"releases"`
}

// Release 是清单里的一个精确版本条目。ArtifactSHA256 / SizeBytes 说的是官方 gzip 包；
// UnpackedSHA256 / UnpackedSizeBytes 说的是解压出的 ELF（引擎装的就是它）。
type Release struct {
	Version             string `json:"version"`
	Platform            string `json:"platform"`
	ArtifactURL         string `json:"artifactUrl"`
	ArtifactSHA256      string `json:"artifactSha256"`
	SizeBytes           int64  `json:"sizeBytes"`
	Packaging           string `json:"packaging"`
	UnpackedSHA256      string `json:"unpackedSha256"`
	UnpackedSizeBytes   int64  `json:"unpackedSizeBytes"`
	License             string `json:"license"`
	SourceURL           string `json:"sourceUrl,omitempty"`
	LicenseURL          string `json:"licenseUrl,omitempty"`
	MinComponentManager int    `json:"minComponentManager"`
	AllowInstall        bool   `json:"allowInstall"`
	AllowUpdate         bool   `json:"allowUpdate"`
	Blocked             bool   `json:"blocked"`
	Note                string `json:"note,omitempty"`
	PublishedAt         string `json:"publishedAt,omitempty"`
}

type signatureFile struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"keyId"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

// ManifestError 是清单被拒的原因（面向管理员的一句话，不含清单内容）。
type ManifestError struct{ Reason string }

func (e *ManifestError) Error() string { return "组件清单被拒绝：" + e.Reason }

func rejected(reason string) error { return &ManifestError{Reason: reason} }

// VerifiedIndex 是验签通过的清单及其原始字节。
type VerifiedIndex struct {
	Index  Index
	Raw    []byte
	Sig    []byte
	SHA256 string
	KeyID  string
}

// ParseIndex 验签并解析清单。
func ParseIndex(raw, sig []byte, keys map[string]ed25519.PublicKey) (*VerifiedIndex, error) {
	var sf signatureFile
	if err := json.Unmarshal(sig, &sf); err != nil {
		return nil, rejected("签名文件不是合法 JSON")
	}
	if sf.Schema != SignatureSchema || sf.Algorithm != "ed25519" {
		return nil, rejected("签名文件形态不符")
	}
	pub, ok := keys[sf.KeyID]
	if !ok {
		return nil, rejected("签名密钥不在固件信任表内（可能需要升级固件）")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || !ed25519.Verify(pub, raw, sigBytes) {
		return nil, rejected("签名校验失败")
	}
	var idx Index
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil {
		return nil, rejected("清单不是合法 JSON 或含未知字段")
	}
	if err := idx.validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &VerifiedIndex{Index: idx, Raw: raw, Sig: sig, SHA256: hex.EncodeToString(sum[:]), KeyID: sf.KeyID}, nil
}

func (idx *Index) validate() error {
	if idx.Schema != IndexSchema {
		return rejected("schema 不符")
	}
	if idx.Component != ComponentName || idx.Channel != "stable" {
		return rejected("组件或通道不符")
	}
	if idx.Revision <= 0 {
		return rejected("revision 必须为正整数")
	}
	seen := map[string]bool{}
	for i := range idx.Releases {
		rel := &idx.Releases[i]
		if err := rel.validate(); err != nil {
			return err
		}
		key := rel.Platform + "/" + rel.Version
		if seen[key] {
			return rejected("同一平台重复声明版本 " + rel.Version)
		}
		seen[key] = true
	}
	return nil
}

func (r *Release) validate() error {
	if !validVersion(r.Version) {
		return rejected("版本号形态不符")
	}
	if !validPlatform(r.Platform) {
		return rejected("平台标识不符")
	}
	if !isSHA256Hex(r.ArtifactSHA256) || !isSHA256Hex(r.UnpackedSHA256) {
		return rejected("制品摘要不是小写 SHA-256")
	}
	if r.SizeBytes <= 0 || r.SizeBytes > maxArtifactBytes {
		return rejected("制品长度越界")
	}
	if r.UnpackedSizeBytes <= 0 || r.UnpackedSizeBytes > maxUnpackedBytes {
		return rejected("解压长度越界")
	}
	if r.Packaging != "gzip" {
		return rejected("打包形态不符（只接受 gzip）")
	}
	if r.License != "GPL-3.0" {
		return rejected("许可证声明不符")
	}
	if r.MinComponentManager <= 0 {
		return rejected("minComponentManager 缺失")
	}
	return checkOfficialURL(r.ArtifactURL, r.Version, r.Platform)
}

// checkOfficialURL 只放行 Mihomo 官方仓库 release 的精确 asset：
// https://github.com/MetaCubeX/mihomo/releases/download/v<版本>/mihomo-<asset 平台>-v<版本>.gz。
// asset 平台见 [assetPlatforms]。
func checkOfficialURL(raw, version, platform string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != officialHost || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return rejected("制品地址不是官方 release 地址")
	}
	for _, asset := range assetPlatforms(platform) {
		if u.Path == officialPathPrefix+"v"+version+"/mihomo-"+asset+"-v"+version+".gz" {
			return nil
		}
	}
	return rejected("制品地址与版本/平台不一致")
}

// assetPlatforms 把清单平台映到官方 release 里允许的 asset 平台段。
//
// linux-amd64 官方出两种：`linux-amd64` 按 GOAMD64=v3 编译（要 AVX2 等扩展），
// `linux-amd64-compatible` 是 v1 基线。云主机的实例规格与虚拟 CPU 型号不保证暴露
// v3 指令集，跑不了的内核在自检那步就 SIGILL；清单可以二选一，缺省发 compatible。
func assetPlatforms(platform string) []string {
	if platform == "linux-amd64" {
		return []string{"linux-amd64-compatible", "linux-amd64"}
	}
	return []string{platform}
}

// validVersion 接受 Mihomo 的 x.y.z 三段数字版本（清单里不带 v 前缀）。
func validVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 6 {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func validPlatform(p string) bool {
	switch p {
	case "linux-arm64", "linux-amd64":
		return true
	}
	return false
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func compareVersion(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// HostPlatform 是本机的清单平台标识。
func HostPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Advisory 是按本机平台与已装版本筛出的安装建议。
type Advisory struct {
	Revision         int64     `json:"revision"`
	Source           string    `json:"source"`
	CheckedAt        time.Time `json:"checked_at,omitempty"`
	Latest           *Release  `json:"latest,omitempty"`
	Newest           *Release  `json:"newest,omitempty"`
	InstalledBlocked bool      `json:"installed_blocked"`
	NeedsFirmware    bool      `json:"needs_firmware,omitempty"`
}

func (idx *Index) selectRelease(platform, installed string) Advisory {
	adv := Advisory{Revision: idx.Revision}
	var newest, latest *Release
	for i := range idx.Releases {
		rel := &idx.Releases[i]
		if rel.Platform != platform {
			continue
		}
		if installed != "" && rel.Version == installed && rel.Blocked {
			adv.InstalledBlocked = true
		}
		if newest == nil || compareVersion(rel.Version, newest.Version) > 0 {
			newest = rel
		}
		if rel.Blocked {
			continue
		}
		if rel.MinComponentManager > ComponentManagerVersion {
			if installed == "" || compareVersion(rel.Version, installed) > 0 {
				adv.NeedsFirmware = true
			}
			continue
		}
		if installed == "" {
			if !rel.AllowInstall {
				continue
			}
		} else if !rel.AllowUpdate || compareVersion(rel.Version, installed) <= 0 {
			continue
		}
		if latest == nil || compareVersion(rel.Version, latest.Version) > 0 {
			latest = rel
		}
	}
	if newest != nil {
		n := *newest
		adv.Newest = &n
	}
	if latest != nil {
		l := *latest
		adv.Latest = &l
		adv.NeedsFirmware = false
	}
	return adv
}

// findByDigest 按官方 gzip 包的摘要在本平台条目里找一条（手动上传的制品必须命中清单）。
func (idx *Index) findByDigest(platform, sum string) *Release {
	for i := range idx.Releases {
		rel := &idx.Releases[i]
		if rel.Platform == platform && rel.ArtifactSHA256 == sum {
			r := *rel
			return &r
		}
	}
	return nil
}

// ErrManifestStale 表示新清单的 revision 低于设备已接受的，或同 revision 换了内容。
var ErrManifestStale = errors.New("组件清单被拒绝：revision 回退或同 revision 内容变化")

func checkRevision(v *VerifiedIndex, storedRevision int64, storedSHA string) error {
	switch {
	case v.Index.Revision < storedRevision:
		return ErrManifestStale
	case v.Index.Revision == storedRevision && storedSHA != "" && v.SHA256 != storedSHA:
		return ErrManifestStale
	}
	return nil
}

func describeRelease(r *Release) string {
	if r == nil {
		return "无"
	}
	return fmt.Sprintf("%s（%s，%d 字节）", r.Version, r.Platform, r.SizeBytes)
}
