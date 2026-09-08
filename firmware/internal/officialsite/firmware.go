package officialsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

const (
	FirmwareIndexPath   = "/updates/firmware/stable.json"
	FirmwareSchema      = "llmgate.firmware-index/v1"
	firmwareIndexCap    = 256 << 10
	firmwareDownloadCap = 256 << 20

	// DefaultFirmwarePlatform 是索引条目省略 platform 时的含义：linux/arm64 是最早的
	// 交付平台，没有平台字段的条目一律按它理解，老索引与老固件因此互不误读。
	DefaultFirmwarePlatform = "linux-arm64"
)

// FirmwareRelease 是官网固件索引里的一个发布项。
//
// 索引把两个交付平台的制品放在同一份文件里，靠 platform（`<GOOS>-<GOARCH>`：
// linux-arm64 / linux-amd64）分开；设备只取与本机平台相同的条目。同一版本的两个
// 平台条目要 arm64 在前：不认 platform 字段的固件按文件顺序取「最新」，这样它拿到的
// 仍是 arm64 制品（就算拿错，fwimage 的机器类型闸也会把它拦下）。
// hardwareModels 非空表示只面向这些型号；通用主机（型号识别不出）只接受不限型号的条目。
type FirmwareRelease struct {
	Version        string   `json:"version"`
	Channel        string   `json:"channel"`
	Platform       string   `json:"platform,omitempty"`
	HardwareModels []string `json:"hardwareModels,omitempty"`
	ArtifactURL    string   `json:"artifactUrl"`
	ArtifactSHA256 string   `json:"artifactSha256"`
	SizeBytes      int64    `json:"sizeBytes"`
	ReleaseNotes   string   `json:"releaseNotes"`
	MinVersion     string   `json:"minVersion"`
	PublishedAt    string   `json:"publishedAt"`
}

type FirmwareIndex struct {
	Schema    string            `json:"schema"`
	Channel   string            `json:"channel"`
	UpdatedAt string            `json:"updatedAt"`
	Releases  []FirmwareRelease `json:"releases"`
}

type FirmwareCheckResult struct {
	Current          string           `json:"current"`
	Latest           *FirmwareRelease `json:"latest"`
	Newest           *FirmwareRelease `json:"newest"`
	UpgradeAvailable bool             `json:"upgradeAvailable"`
}

func (c *Client) FirmwareCheck(ctx context.Context) (*FirmwareCheckResult, error) {
	_, raw, _, err := c.fetchJSON(ctx, FirmwareIndexPath, "", firmwareIndexCap, "固件版本清单")
	if err != nil {
		return nil, err
	}
	var index FirmwareIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, errors.New("官网返回的不是固件版本清单（内容不是合法 JSON）")
	}
	if index.Schema != FirmwareSchema || index.Channel != "stable" {
		return nil, errors.New("官网固件版本清单形态不符")
	}
	model := ""
	if c.model != nil {
		model, err = c.model.Model()
		if err != nil {
			return nil, fmt.Errorf("读取板卡型号: %w", err)
		}
	}
	platform := c.platform
	if platform == "" {
		platform = HostPlatform()
	}
	rows := make([]FirmwareRelease, 0, len(index.Releases))
	for _, rel := range index.Releases {
		if rel.Channel != "" && rel.Channel != "stable" {
			continue
		}
		if releasePlatform(rel) != platform {
			continue
		}
		if !releaseSupports(rel, model) {
			continue
		}
		if rel.Version == "" || rel.ArtifactURL == "" || !isSHA256Hex(rel.ArtifactSHA256) || rel.SizeBytes <= 0 {
			continue
		}
		rows = append(rows, rel)
	}
	sort.SliceStable(rows, func(i, j int) bool { return compareVersion(rows[i].Version, rows[j].Version) > 0 })
	result := &FirmwareCheckResult{Current: buildinfo.Version}
	if len(rows) > 0 {
		result.Newest = copyRelease(rows[0])
	}
	for i := range rows {
		rel := &rows[i]
		if compareVersion(rel.Version, buildinfo.Version) <= 0 {
			continue
		}
		if rel.MinVersion != "" && compareVersion(buildinfo.Version, rel.MinVersion) < 0 {
			continue
		}
		result.Latest = copyRelease(*rel)
		result.UpgradeAvailable = true
		break
	}
	return result, nil
}

func copyRelease(in FirmwareRelease) *FirmwareRelease {
	out := in
	out.HardwareModels = append([]string(nil), in.HardwareModels...)
	return &out
}

// HostPlatform 是本机的固件平台标识（`<GOOS>-<GOARCH>`），与索引条目的 platform 同一口径。
func HostPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// WithPlatform 覆盖用来筛索引的平台标识（测试与工具用；生产恒按本机）。
func WithPlatform(platform string) Option {
	return func(c *Client) { c.platform = strings.TrimSpace(platform) }
}

// releasePlatform 取条目的平台，省略即 [DefaultFirmwarePlatform]。
func releasePlatform(rel FirmwareRelease) string {
	if p := strings.TrimSpace(rel.Platform); p != "" {
		return p
	}
	return DefaultFirmwarePlatform
}

// releaseSupports 按型号筛：条目不限型号即通用；限了型号就要本机型号在列——识别不出
// 型号的通用主机拿不到限型号的条目，而不是反过来什么都收。
func releaseSupports(rel FirmwareRelease, model string) bool {
	if len(rel.HardwareModels) == 0 {
		return true
	}
	if model == "" {
		return false
	}
	for _, candidate := range rel.HardwareModels {
		if candidate == model {
			return true
		}
	}
	return false
}

func (c *Client) FetchFirmware(ctx context.Context, artifactURL string, w io.Writer) (string, int64, error) {
	target, err := c.resolveArtifactURL(artifactURL)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", 0, errors.New("构造固件下载请求失败")
	}
	client := &http.Client{Transport: c.siteTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, &HTTPError{Status: resp.StatusCode, Path: req.URL.Path}
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hasher), io.LimitReader(resp.Body, firmwareDownloadCap+1))
	if err != nil {
		return "", 0, fmt.Errorf("下载固件包中断: %w", err)
	}
	if n > firmwareDownloadCap {
		return "", 0, fmt.Errorf("固件包超过 %d 字节上限", int64(firmwareDownloadCap))
	}
	if n == 0 {
		return "", 0, errors.New("固件包为空")
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

func (c *Client) resolveArtifactURL(artifactURL string) (string, error) {
	artifactURL = strings.TrimSpace(artifactURL)
	if artifactURL == "" {
		return "", errors.New("固件发布项缺少下载地址")
	}
	if strings.HasPrefix(artifactURL, "/") {
		return c.baseURL + artifactURL, nil
	}
	p, err := url.Parse(artifactURL)
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
		return "", errors.New("固件下载地址不是合法的 HTTPS URL")
	}
	return artifactURL, nil
}

// compareVersion 比较两个固件版本名称：a<b 回 -1、相等回 0、a>b 回 1。
//
// 版本名称形如 2608221732-2d81（YYMMDDHHMM-提交短哈希末 4 位，脏树再缀 -d，见
// firmware/Makefile）。**只有连字号前那个十位时间戳参与比较**——它精确到分钟、
// 单调递增，天生就是排序键；后面的哈希只是给人分辨同一分钟里的不同提交，两串
// 哈希之间没有先后可言。同一时间戳的两个包因此判为相等，互不构成升级。
//
// 首段仍按点分整数逐段比，是为了让清单里手写的 `minVersion` 也能写成 vX.Y.Z：
// 十位时间戳比任何点分整数都大，所以那种写法只当「不设下限」。代价是点分号的
// 预发布序（v1.0.0-rc1 与 v1.0.0）不分先后，本产品不产出那种版本名称。
func compareVersion(a, b string) int {
	aCore, bCore := versionCore(a), versionCore(b)
	for i := 0; i < len(aCore) || i < len(bCore); i++ {
		av, bv := 0, 0
		if i < len(aCore) {
			av = aCore[i]
		}
		if i < len(bCore) {
			bv = bCore[i]
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

// versionCore 取版本名称里参与比较的那一段：首个连字号之前、按点分开的整数。
// 遇到不是整数的段即停（`rc1` 这类后缀），已取到的段就是全部比较依据。
// gate 侧 gateReleaseStamp 是同一口径的另一份实现，两边必须一起改。
func versionCore(v string) []int {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V"))
	head, _, _ := strings.Cut(v, "-")
	var nums []int
	for _, seg := range strings.Split(head, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(seg))
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	return nums
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
