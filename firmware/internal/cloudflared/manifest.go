package cloudflared

// 签名组件清单（docs-dev/firmware-cloudflare-tunnel.md §7.1）：官网
// /updates/components/cloudflared/stable.json 及其离线签名 stable.json.sig。
//
// 设备只在验签通过、schema/组件/通道相符、revision 不回退（同 revision 不换内容）
// 之后才把它当作允许决策；条目再按平台、最低组件管理器版本、阻断标记与
// 新装/更新许可筛选。清单不能下发代码、脚本、任意 URL 或镜像地址——制品地址
// 必须是 Cloudflare 官方仓库 release 的精确 asset。

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
	// IndexSchema 是组件清单的 schema 标识。
	IndexSchema = "llmgate.component-index/v1"
	// SignatureSchema 是清单签名文件的 schema 标识（与 tools/componentsign 同值）。
	SignatureSchema = "llmgate.component-signature/v1"
	// ComponentName 是本包管理的唯一组件。
	ComponentName = "cloudflared"
	// ComponentManagerVersion 是本固件组件管理器的协议版本；清单条目的
	// minComponentManager 大于它时该条目不可安装，提示先升级固件。
	ComponentManagerVersion = 1

	// 官方制品来源：只认 Cloudflare 官方仓库 release 的精确 asset。
	officialHost       = "github.com"
	officialPathPrefix = "/cloudflare/cloudflared/releases/download/"
	// maxArtifactBytes 是任何条目允许声明的字节上限（cloudflared 约 37 MiB）。
	maxArtifactBytes = 128 << 20
)

// artifactRedirectHosts 是官方 release 直下允许经过的主机：GitHub 的 release
// 资产恒经一次 302 到 *.githubusercontent.com。
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

// Release 是清单里的一个精确版本条目。
type Release struct {
	Version             string `json:"version"`
	Platform            string `json:"platform"`
	ArtifactURL         string `json:"artifactUrl"`
	ArtifactSHA256      string `json:"artifactSha256"`
	SizeBytes           int64  `json:"sizeBytes"`
	License             string `json:"license"`
	SourceURL           string `json:"sourceUrl,omitempty"`
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
type ManifestError struct {
	Reason string
}

func (e *ManifestError) Error() string { return "组件清单被拒绝：" + e.Reason }

func rejected(reason string) error { return &ManifestError{Reason: reason} }

// VerifiedIndex 是验签通过的清单及其原始字节（落盘与摘要都用原始字节，不重排）。
type VerifiedIndex struct {
	Index  Index
	Raw    []byte
	Sig    []byte
	SHA256 string
	KeyID  string
}

// ParseIndex 验签并解析清单。keys 为空或签名不认识一律拒绝；解析后再逐条校验。
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
	if !isSHA256Hex(r.ArtifactSHA256) {
		return rejected("制品摘要不是小写 SHA-256")
	}
	if r.SizeBytes <= 0 || r.SizeBytes > maxArtifactBytes {
		return rejected("制品长度越界")
	}
	if r.MinComponentManager <= 0 {
		return rejected("minComponentManager 缺失")
	}
	if err := checkOfficialURL(r.ArtifactURL, r.Version, r.Platform); err != nil {
		return err
	}
	return nil
}

// checkOfficialURL 只放行 Cloudflare 官方仓库 release 的精确 asset：
// https://github.com/cloudflare/cloudflared/releases/download/<版本>/cloudflared-<平台>。
func checkOfficialURL(raw, version, platform string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != officialHost || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return rejected("制品地址不是官方 release 地址")
	}
	want := officialPathPrefix + version + "/cloudflared-" + platform
	if u.Path != want {
		return rejected("制品地址与版本/平台不一致")
	}
	return nil
}

// validVersion 接受 cloudflared 的 YYYY.M.P 三段数字版本。
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

// compareVersion 比较两个 YYYY.M.P 版本：a<b 回 -1、相等 0、a>b 1。形态由
// validVersion 保证；非法串按 0 段处理。
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

// HostPlatform 是本机的清单平台标识（linux-arm64 / linux-amd64）。
func HostPlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Advisory 是按本机平台与已装版本筛出的安装建议。
type Advisory struct {
	// Revision 是当前生效清单的 revision。
	Revision int64 `json:"revision"`
	// Source 是清单来源：website | upload。
	Source string `json:"source"`
	// CheckedAt 是清单被接受的时刻（内存态；重启后取落盘清单时为零值缺省）。
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// Latest 是此刻可以安装/更新到的条目；nil = 没有。
	Latest *Release `json:"latest,omitempty"`
	// Newest 是清单里本平台最新的条目（可能因阻断或许可不可装），供界面展示。
	Newest *Release `json:"newest,omitempty"`
	// InstalledBlocked 为真表示已装版本被清单标为阻断：界面持续显示安全警告。
	InstalledBlocked bool `json:"installed_blocked"`
	// NeedsFirmware 为真表示最新条目要求更高的组件管理器版本（先升级固件）。
	NeedsFirmware bool `json:"needs_firmware,omitempty"`
}

// selectRelease 按平台/已装版本筛选：不阻断、管理器版本够、已装为空要求
// allowInstall、否则要求 allowUpdate 且更新；多个候选取最高版本。
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
		} else {
			if !rel.AllowUpdate || compareVersion(rel.Version, installed) <= 0 {
				continue
			}
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
		// 有可装条目就不必再提示升级固件——那是更高版本的事。
		adv.NeedsFirmware = false
	}
	return adv
}

// findByDigest 在本平台条目里按摘要找一条（手动上传的制品必须命中清单）。
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

// checkRevision 做防回退：storedRevision 是设备已接受的 revision，storedSHA 是其
// 原始字节摘要。
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
