// Package opencodehelper exposes a fixed-path proxy for public OpenCode CLI
// release metadata and archives. Client computers fetch only through their
// configured LLM Gate; the device fetches the same bytes from the official
// anomalyco/opencode GitHub release and never stores them.
package opencodehelper

import (
	"context"
	"errors"
	"fmt"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCLIAPIBase      = "https://api.github.com/repos/anomalyco/opencode/releases"
	DefaultCLIReleasesBase = "https://github.com/anomalyco/opencode/releases/download"

	cliConnectTimeout      = 10 * time.Second
	cliTLSHandshakeTimeout = 10 * time.Second
	cliHeaderTimeout       = 30 * time.Second
	cliBodyTimeout         = 60 * time.Minute
	cliMaxBytes            = 512 << 20
	cliMaxInflight         = 4
)

var (
	cliVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z._-]+)?$`)
	cliAssets    = []*regexp.Regexp{
		regexp.MustCompile(`^opencode-linux-arm64(?:-musl)?\.tar\.gz$`),
		regexp.MustCompile(`^opencode-linux-x64(?:-baseline)?(?:-musl)?\.tar\.gz$`),
		regexp.MustCompile(`^opencode-darwin-arm64\.zip$`),
		regexp.MustCompile(`^opencode-darwin-x64(?:-baseline)?\.zip$`),
		regexp.MustCompile(`^opencode-windows-arm64\.zip$`),
		regexp.MustCompile(`^opencode-windows-x64(?:-baseline)?\.zip$`),
	}
)

// ValidCLIPath accepts only the latest stable release document and CLI archives
// for the six gate client platforms. Desktop packages and arbitrary release
// assets are deliberately outside this proxy.
func ValidCLIPath(path string) bool {
	if path == "latest" {
		return true
	}
	if path == "" || len(path) > 180 {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] != "releases" || !cliVersionRE.MatchString(parts[1]) {
		return false
	}
	for _, re := range cliAssets {
		if re.MatchString(parts[2]) {
			return true
		}
	}
	return false
}

var cliForwardedRespHeaders = [...]string{
	"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
	"ETag", "Last-Modified", "Cache-Control",
}

// CLIProxy serves /opencode-helper/cli/{path...}. Its zero value is not usable.
type CLIProxy struct {
	log      *slog.Logger
	hc       *http.Client
	api      string
	releases string
	sem      chan struct{}

	mu     sync.Mutex
	failed bool
}

func NewCLIProxy(logger *slog.Logger, opts ...Option) *CLIProxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &CLIProxy{
		log: logger, api: DefaultCLIAPIBase,
		releases: DefaultCLIReleasesBase, sem: make(chan struct{}, cliMaxInflight),
	}
	var router egress.Router
	for _, o := range opts {
		o(&router)
	}
	p.hc = newCLIHTTPClient(router)
	return p
}

// Option 调整装配。
type Option func(*egress.Router)

// WithEgress 接入出站策略（internal/egress，cli_artifacts 分类）；nil 恒直连。
func WithEgress(r egress.Router) Option { return func(dst *egress.Router) { *dst = r } }

// SetBases replaces the official metadata and archive origins for tests.
func (p *CLIProxy) SetBases(api, releases string) {
	p.api = strings.TrimRight(strings.TrimSpace(api), "/")
	p.releases = strings.TrimRight(strings.TrimSpace(releases), "/")
}

func newCLIHTTPClient(router egress.Router) *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if cliAllowedRedirect(req, via) {
				return nil
			}
			return http.ErrUseLastResponse
		},
		Transport: egress.TransportFor(router, egress.ScopeCLIArtifacts, &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: cliConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   cliTLSHandshakeTimeout,
			ResponseHeaderTimeout: cliHeaderTimeout,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
		}),
	}
}

func cliAllowedRedirect(req *http.Request, via []*http.Request) bool {
	if req == nil || len(via) != 1 || via[0] == nil || req.URL == nil || via[0].URL == nil {
		return false
	}
	from, to := via[0].URL, req.URL
	return from.Scheme == "https" && from.Host == "github.com" && from.RawQuery == "" &&
		strings.HasPrefix(from.Path, "/anomalyco/opencode/releases/download/v") &&
		to.Scheme == "https" && to.Host == "release-assets.githubusercontent.com" &&
		strings.HasPrefix(to.Path, "/github-production-release-asset/")
}

func (p *CLIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.PathValue("path")
	if !ValidCLIPath(path) {
		http.NotFound(w, r)
		return
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-r.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cliBodyTimeout)
	defer cancel()
	resp, err := p.open(ctx, r.Method, path, r.Header.Get("Range"))
	if err != nil {
		p.note(fmt.Errorf("拉官方 OpenCode CLI %s: %w", path, err))
		status := http.StatusBadGateway
		if cliIsTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "暂时无法从官方源取回 OpenCode CLI", status)
		return
	}
	defer resp.Body.Close()

	if !cliOKStatus(resp.StatusCode) {
		p.note(fmt.Errorf("官方 OpenCode CLI %s 答 HTTP %d", path, resp.StatusCode))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	if resp.ContentLength > cliMaxBytes {
		p.note(fmt.Errorf("官方 OpenCode CLI %s 超过 %d 字节", path, cliMaxBytes))
		http.Error(w, "官方 OpenCode CLI 超出本设备允许的大小", http.StatusBadGateway)
		return
	}

	p.note(nil)
	for _, key := range cliForwardedRespHeaders {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	n, _ := io.Copy(&cliFlushWriter{w: w}, io.LimitReader(resp.Body, cliMaxBytes+1))
	if n > cliMaxBytes {
		panic(http.ErrAbortHandler)
	}
}

func (p *CLIProxy) open(ctx context.Context, method, path, byteRange string) (*http.Response, error) {
	base, suffix := p.api, "/latest"
	if path != "latest" {
		parts := strings.Split(path, "/")
		base = p.releases
		suffix = "/v" + parts[1] + "/" + parts[2]
	}
	if base == "" {
		return nil, errors.New("未配置官方 CLI 来源")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+suffix, nil)
	if err != nil {
		return nil, err
	}
	if path == "latest" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	return p.hc.Do(req)
}

func cliOKStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusPartialContent
}

type cliFlushWriter struct {
	w http.ResponseWriter
	n int
}

func (f *cliFlushWriter) Write(body []byte) (int, error) {
	n, err := f.w.Write(body)
	f.n += n
	if f.n >= 1<<20 {
		_ = http.NewResponseController(f.w).Flush()
		f.n = 0
	}
	return n, err
}

func cliIsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// note records only failure-state transitions; release bodies never enter logs.
func (p *CLIProxy) note(err error) {
	p.mu.Lock()
	flip := p.failed != (err != nil)
	p.failed = err != nil
	p.mu.Unlock()
	if !flip {
		return
	}
	if err != nil {
		p.log.Warn("官方 OpenCode CLI 透传失败", "err", err.Error())
		return
	}
	p.log.Info("官方 OpenCode CLI 透传已恢复")
}
