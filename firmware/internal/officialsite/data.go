package officialsite

// Public data-update files. Both are served by the static website and fetched
// anonymously; ETag makes the hourly check cheap when nothing changed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

var ErrNotModified = errors.New("官网文件未变化（304）")

const (
	OfficialPricingPath = "/updates/data/official-pricing.json"
	PlatformModelsPath  = "/updates/data/platform-models.json"

	OfficialPricingSchema = "llmgate.official-pricing/v1"

	// OfficialPricingMaxBytes / OfficialPricingMaxModels 是价目文件的下载上限与
	// 条目上限（发布侧的校验工具用同一对数字）。
	OfficialPricingMaxBytes  = 256 << 10
	OfficialPricingMaxModels = 500
)

type OfficialPricing struct {
	Schema    string                 `json:"schema"`
	Version   int64                  `json:"version"`
	UpdatedAt string                 `json:"updated_at"`
	Currency  string                 `json:"currency"`
	Unit      string                 `json:"unit"`
	Models    []OfficialPricingModel `json:"models"`
}

type OfficialPricingModel struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Vendor  string          `json:"vendor"`
	Agent   string          `json:"agent"`
	Pricing json.RawMessage `json:"pricing"`
	// Schedule 是可选的分时段价（形态见 usage.ParseSchedule）。设备当前只把
	// Pricing（标准价）同步进模型目录，这一段原样落库、不参与记账；形态合法性
	// 由发布侧的校验工具（tools/catalogcheck）把守，取回时不据此拒收文件。
	Schedule  json.RawMessage `json:"schedule,omitempty"`
	Source    string          `json:"source"`
	CheckedAt string          `json:"checked_at"`
	Note      string          `json:"note"`
}

type OfficialPricingFile struct {
	URL  string
	Raw  []byte
	ETag string
	Doc  OfficialPricing
}

type PlatformModelsFile struct {
	URL  string
	Raw  []byte
	ETag string
	Doc  platformcatalog.Doc
}

func (c *Client) OfficialPricingURL() string { return c.baseURL + OfficialPricingPath }
func (c *Client) PlatformModelsURL() string  { return c.baseURL + PlatformModelsPath }

func (c *Client) fetchJSON(ctx context.Context, path, ifNoneMatch string, limit int64, what string) (string, []byte, string, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return url, nil, "", errors.New("构造官网请求失败")
	}
	req.Header.Set("Accept", "application/json")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.json.Do(req)
	if err != nil {
		return url, nil, "", transportError(err)
	}
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		if etag == "" {
			etag = ifNoneMatch
		}
		return url, nil, etag, ErrNotModified
	}
	if resp.StatusCode/100 != 2 {
		return url, nil, "", &HTTPError{Status: resp.StatusCode, Path: path}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return url, nil, "", errors.New("读取" + what + "失败")
	}
	if int64(len(raw)) > limit {
		return url, nil, "", fmt.Errorf("%s超过 %d 字节上限", what, limit)
	}
	return url, raw, etag, nil
}

func (c *Client) FetchOfficialPricing(ctx context.Context, ifNoneMatch string) (*OfficialPricingFile, error) {
	url, raw, etag, err := c.fetchJSON(ctx, OfficialPricingPath, ifNoneMatch, OfficialPricingMaxBytes, "官方价格文件")
	if err != nil {
		return nil, err
	}
	var doc OfficialPricing
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("官网返回的不是官方价格文件（内容不是合法 JSON）")
	}
	if doc.Schema != OfficialPricingSchema {
		return nil, fmt.Errorf("官方价格文件形态不符：期望 %q，实际 %q", OfficialPricingSchema, capRunes(doc.Schema, 64))
	}
	if len(doc.Models) == 0 {
		return nil, errors.New("官方价格文件里一条模型价目都没有")
	}
	if len(doc.Models) > OfficialPricingMaxModels {
		return nil, fmt.Errorf("官方价格文件的条目数超过上限 %d", OfficialPricingMaxModels)
	}
	return &OfficialPricingFile{URL: url, Raw: raw, ETag: etag, Doc: doc}, nil
}

func (c *Client) FetchPlatformModels(ctx context.Context, ifNoneMatch string) (*PlatformModelsFile, error) {
	url, raw, etag, err := c.fetchJSON(ctx, PlatformModelsPath, ifNoneMatch, platformcatalog.MaxBytes, "平台模型文件")
	if err != nil {
		return nil, err
	}
	doc, err := platformcatalog.Parse(raw)
	if err != nil {
		return nil, errors.New("官网返回的不是平台模型文件（" + err.Error() + "）")
	}
	return &PlatformModelsFile{URL: url, Raw: raw, ETag: etag, Doc: doc}, nil
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
