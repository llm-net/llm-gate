package officialsite

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
)

const (
	AppsPath       = "/apps/"
	AppsPageMarker = `name="llmgate-apps"`
)

var ErrAppsPath = errors.New("推荐应用透传只接受 /apps/ 前缀内的干净路径")

var appsForwardedHeaders = [...]string{"Accept", "If-None-Match", "If-Modified-Since"}

type AppsRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

func (c *Client) OpenApps(ctx context.Context, req AppsRequest) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("推荐应用透传不接受 %s", req.Method)
	}
	if !validAppsPath(req.Path) {
		return nil, ErrAppsPath
	}
	target := c.baseURL + req.Path
	if req.Query != "" {
		target += "?" + req.Query
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, target, nil)
	if err != nil {
		return nil, errors.New("构造官网透传请求失败")
	}
	for _, k := range appsForwardedHeaders {
		if v := req.Header.Get(k); v != "" {
			out.Header.Set(k, v)
		}
	}
	// net/http otherwise injects Go-http-client/1.1. The website only needs a
	// static-file request and must not receive the customer's browser UA.
	out.Header.Set("User-Agent", "")
	resp, err := c.pass.Do(out)
	if err != nil {
		return nil, fmt.Errorf("连接 LLM Gate官网失败: %w", err)
	}
	return resp, nil
}

func validAppsPath(p string) bool {
	if !strings.HasPrefix(p, AppsPath) {
		return false
	}
	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned == p
}
