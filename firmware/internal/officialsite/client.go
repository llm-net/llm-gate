// Package officialsite reads the public, immutable-or-versioned files that
// LLM Gate devices consume from the LLM Gate website. The website is a
// static Cloudflare deployment: there is no device identity, registration,
// credential, heartbeat, or writable API on this connection.
package officialsite

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

const DefaultBaseURL = "https://llm.net"

const (
	connectTimeout        = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 15 * time.Second
)

// ModelSource reports this board's technical model identifier. It contains no
// device identity and is used only to select a compatible firmware release
// from the public manifest. An empty model means a generic host (no board
// profile): only releases without a hardwareModels restriction apply.
type ModelSource interface {
	Model() (string, error)
}

// FixedModel 是一个恒定的 ModelSource：gatewayd 启动时识别一次型号（识别不出即
// 空串 = 通用主机），之后固件检查不再碰硬件。
type FixedModel string

// Model 返回固定的型号代号。
func (m FixedModel) Model() (string, error) { return string(m), nil }

// Client is a read-only client for the public website. It is safe for
// concurrent use after construction.
type Client struct {
	baseURL string
	model   ModelSource
	// platform 是筛固件索引用的平台标识；空即本机（HostPlatform）。
	platform string
	json     *http.Client
	pass     *http.Client
	// artifactTLS 只给组件制品直下的独立客户端用：生产恒 nil（系统信任链），
	// 测试注入本地 httptest 证书。不是「关闭校验」的口子——它只能加信任根。
	artifactTLS *tls.Config
	// egress 是出站策略（internal/egress）：官网自身的 JSON / 透传 / 固件下载走
	// official_site 分类，第三方组件制品走 component_artifacts。nil 恒直连。
	egress egress.Router
}

// Option 调整 Client 装配。
type Option func(*Client)

// WithEgress 接入出站策略；生产装配恒传 gatewayd 唯一的 *egress.Manager。
func WithEgress(r egress.Router) Option { return func(c *Client) { c.egress = r } }

func NewClient(baseURL string, model ModelSource, opts ...Option) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{baseURL: baseURL, model: model}
	for _, o := range opts {
		o(c)
	}
	c.json = c.newJSONClient()
	c.pass = c.newPassthroughClient()
	return c
}

func (c *Client) BaseURL() string { return c.baseURL }

// HTTPError means the website answered with a non-success status. Response
// bodies are deliberately excluded: update files are untrusted remote content
// and must never be copied into logs.
type HTTPError struct {
	Status int
	Path   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("LLM Gate官网返回 HTTP %d（%s）", e.Status, e.Path)
}

func transportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("连接 LLM Gate官网超时")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("连接 LLM Gate官网超时")
	}
	return errors.New("无法连接 LLM Gate官网")
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
}

// siteTransport 是官网自身流量（official_site 分类）的 RoundTripper。
func (c *Client) siteTransport() http.RoundTripper {
	return egress.TransportFor(c.egress, egress.ScopeOfficialSite, newTransport())
}

func (c *Client) newJSONClient() *http.Client {
	return &http.Client{Transport: c.siteTransport(), Timeout: 20 * time.Second}
}

func (c *Client) newPassthroughClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     c.siteTransport(),
	}
}
