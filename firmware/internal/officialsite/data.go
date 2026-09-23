package officialsite

// Public data-update file. It is served by the static website and fetched
// anonymously; ETag makes the hourly check cheap when nothing changed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

var ErrNotModified = errors.New("官网文件未变化（304）")

// ModelCatalogPath 是模型目录数据文件（平台、模型、价格合一）的官网路径。
const ModelCatalogPath = "/updates/data/model-catalog.json"

// ModelCatalogFile 是取回的一份模型目录数据：原文、ETag 与解析结果。
type ModelCatalogFile struct {
	URL  string
	Raw  []byte
	ETag string
	Doc  platformcatalog.Doc
}

func (c *Client) ModelCatalogURL() string { return c.baseURL + ModelCatalogPath }

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

// FetchModelCatalog 取回模型目录数据文件并做结构校验（platformcatalog.Parse）。
// ifNoneMatch 非空时做条件 GET，官网没变返回 ErrNotModified。
func (c *Client) FetchModelCatalog(ctx context.Context, ifNoneMatch string) (*ModelCatalogFile, error) {
	url, raw, etag, err := c.fetchJSON(ctx, ModelCatalogPath, ifNoneMatch, platformcatalog.MaxBytes, "模型目录文件")
	if err != nil {
		return nil, err
	}
	doc, err := platformcatalog.Parse(raw)
	if err != nil {
		return nil, errors.New("官网返回的不是模型目录文件（" + err.Error() + "）")
	}
	return &ModelCatalogFile{URL: url, Raw: raw, ETag: etag, Doc: doc}, nil
}
