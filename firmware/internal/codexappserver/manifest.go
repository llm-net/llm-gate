// Package codexappserver 管理板上可选的 Codex App Server 组件：OpenAI Codex 官方 release 里
// 独立发布的 `codex-app-server-package-*` tar.gz 包（Rust 静态二进制加它的运行伴侣）。链路与
// cloudflared / Mihomo 同源——官网签名清单 → 官方精确 asset → 长度/摘要/解包上限/ELF/自述
// 版本校验 → 升级引擎装进 A/B 槽 → 可回退、可卸载。
//
// 包布局（`codex-package.json` 的 layoutVersion 1）：
//
//	bin/codex-app-server           入口可执行文件（清单 unpackedSha256 / unpackedSizeBytes 说的就是它）
//	bin/codex-code-mode-host       工具执行宿主；app-server 按自身所在目录找它，缺了模型就无法执行任何命令
//	codex-package.json             包自述（layoutVersion / version / entrypoint / resourcesDir / pathDir）
//	codex-path/rg                  加进沙箱 PATH 的工具
//	codex-resources/bwrap          捆绑的 bubblewrap（系统没装时用它建沙箱）
//	codex-resources/zsh/bin/zsh    捆绑的 zsh
//
// 只装 `bin/codex-app-server` 一个文件的组件是残缺的：它能登录、能对话，但每次工具调用都
// 因找不到同目录的 `codex-code-mode-host` 而失败。所以设备整包安装，槽位就是这棵目录树。
//
// 边界：本包管组件的安装、升级、回退与卸载，以及按需拉起实例的运行期入口（runtime.go：
// Launch / Initialize / Close 与无凭据自检）。没有 systemd unit、没有常驻进程与就绪探针；
// 卸载的唯一守卫是本进程有运行中的实例。固件与官网都不携带该可执行文件。§15.1：清单与
// 制品都是公开物；运行期只按字节把调用方给的 auth.json 写进实例目录并在结束时覆写删除，
// 不解析、不记录。契约见 docs-dev/firmware-codex-app-server.md。
package codexappserver

// 签名组件清单：官网 /updates/components/codex-app-server/stable.json 及其离线签名 .sig。
// 形态与 Mihomo 同源（llmgate.component-index/v1），差别只在：制品是 tar.gz 目录包
//（`packaging: "tar.gz"`，清单声明入口文件解包后的长度与摘要）、官方来源是 releases.openai.com
// 或 openai/codex 的 GitHub release、许可证 Apache-2.0。

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
	ComponentName   = "codex-app-server"
	// ComponentManagerVersion 是本固件对该组件的管理器协议版本。版本 1 只装单个
	// `codex-app-server` 文件（不可用，见包注释）；版本 2 整包安装 `codex-app-server-package-*`。
	// 清单条目标 `minComponentManager: 2`，旧固件对它只会标 needs_firmware，不会装成残缺件。
	ComponentManagerVersion = 2

	// EntrypointPath 是包内入口可执行文件的相对路径，也是引擎在槽位里的落位。
	EntrypointPath = "bin/codex-app-server"
	// PackageMetaFile 是包自述文件（包根）。
	PackageMetaFile = "codex-package.json"
	// packageLayoutVersion 是本固件认识的包布局版本。
	packageLayoutVersion = 1

	// maxPackageTotalBytes / maxPackageFiles 是整包解开后的总量上限（0.154 解包约 235–270 MiB、
	// 6 个文件）。
	maxPackageTotalBytes = 768 << 20
	maxPackageFiles      = 64

	// 官方两处来源：OpenAI 自有 release 桶与 openai/codex 的 GitHub release（tag rust-v<版本>）。
	officialReleasesHost = "releases.openai.com"
	officialReleasesPath = "/codex/releases/"
	officialGitHubHost   = "github.com"
	officialGitHubPath   = "/openai/codex/releases/download/rust-v"

	// maxArtifactBytes / maxUnpackedBytes 是任何条目允许声明的上限（0.154 整包压缩约 94–100 MiB、
	// 入口文件解包约 169–195 MiB）。
	maxArtifactBytes = 256 << 20
	maxUnpackedBytes = 512 << 20
)

// packageTopDirs 是包根下允许出现的目录；除此之外包根只允许 PackageMetaFile 一个文件。
var packageTopDirs = map[string]bool{"bin": true, "codex-path": true, "codex-resources": true}

// artifactRedirectHosts 是官方 release 直下允许经过的主机。
var artifactRedirectHosts = []string{
	officialReleasesHost,
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

// Release 是清单里的一个精确版本条目。ArtifactSHA256 / SizeBytes 说的是官方 tar.gz 包；
// UnpackedSHA256 / UnpackedSizeBytes 说的是包里的入口文件 bin/codex-app-server（引擎复核它）。
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
		return rejected("解包长度越界")
	}
	if r.Packaging != "tar.gz" {
		return rejected("打包形态不符（只接受 tar.gz）")
	}
	if r.License != "Apache-2.0" {
		return rejected("许可证声明不符")
	}
	if r.MinComponentManager <= 0 {
		return rejected("minComponentManager 缺失")
	}
	return checkOfficialURL(r.ArtifactURL, r.Version, r.Platform)
}

// AssetName 是官方 release 里该平台整包 asset 去掉 .tar.gz 后的名字：
// codex-app-server-package-<aarch64|x86_64>-unknown-linux-musl。同名的单文件 asset
// `codex-app-server-<arch>-unknown-linux-musl.tar.gz` 与 `.zst` 都不接受。
func AssetName(platform string) string {
	arch := "x86_64"
	if platform == "linux-arm64" {
		arch = "aarch64"
	}
	return "codex-app-server-package-" + arch + "-unknown-linux-musl"
}

// checkOfficialURL 只放行官方 release 的精确 asset，两种地址二选一：
//
//	https://releases.openai.com/codex/releases/<版本>/<AssetName>.tar.gz
//	https://github.com/openai/codex/releases/download/rust-v<版本>/<AssetName>.tar.gz
func checkOfficialURL(raw, version, platform string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return rejected("制品地址不是官方 release 地址")
	}
	asset := AssetName(platform) + ".tar.gz"
	switch u.Host {
	case officialReleasesHost:
		if u.Path == officialReleasesPath+version+"/"+asset {
			return nil
		}
	case officialGitHubHost:
		if u.Path == officialGitHubPath+version+"/"+asset {
			return nil
		}
	default:
		return rejected("制品地址不是官方 release 地址")
	}
	return rejected("制品地址与版本/平台不一致")
}

// validVersion 接受 Codex 的 x.y.z 三段数字版本（清单里不带 rust-v 前缀）。
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

// findByDigest 按官方 tar.gz 包的摘要在本平台条目里找一条（手动上传的制品必须命中清单）。
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
