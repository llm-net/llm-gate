// Package mcodehelper exposes a fixed-path proxy for public MiniMax Code release
// installers and the pinned Node.js runtime. Clients fall back to their
// configured device; it streams the original bytes without storing them.
package mcodehelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

const (
	DefaultCLIReleasesBase = "https://filecdn.minimax.chat/public"
	DefaultNodeBase        = "https://nodejs.org/dist"
	NodeVersion            = "22.23.2"

	cliConnectTimeout      = 10 * time.Second
	cliTLSHandshakeTimeout = 10 * time.Second
	cliHeaderTimeout       = 30 * time.Second
	cliBodyTimeout         = 60 * time.Minute
	cliMaxBytes            = 128 << 20
	cliMaxInflight         = 4
)

// ValidCLIPath permits the official bootstrap and the pinned Node runtime.
// This is not a general registry or URL proxy.
func ValidCLIPath(path string) bool {
	if path == "install.sh" || path == "install.ps1" {
		return true
	}
	asset, ok := strings.CutPrefix(path, "node/v"+NodeVersion+"/")
	if !ok {
		return false
	}
	if asset == "SHASUMS256.txt" {
		return true
	}
	for _, platform := range []string{"linux-x64.tar.gz", "linux-arm64.tar.gz", "darwin-x64.tar.gz", "darwin-arm64.tar.gz", "win-x64.zip", "win-arm64.zip"} {
		if asset == "node-v"+NodeVersion+"-"+platform {
			return true
		}
	}
	return false
}

var cliForwardedRespHeaders = [...]string{
	"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
	"ETag", "Last-Modified", "Cache-Control",
}

// CLIProxy serves /mcode-helper/cli/{path...}. Its zero value is not usable.
type CLIProxy struct {
	log      *slog.Logger
	hc       *http.Client
	base     string
	nodeBase string
	sem      chan struct{}

	mu     sync.Mutex
	failed bool
}

func NewCLIProxy(logger *slog.Logger, opts ...Option) *CLIProxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &CLIProxy{
		log: logger, base: DefaultCLIReleasesBase, nodeBase: DefaultNodeBase,
		sem: make(chan struct{}, cliMaxInflight),
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

// SetBase replaces the fixed release origin for tests. Production never calls it.
func (p *CLIProxy) SetBase(base string) {
	p.base = strings.TrimRight(strings.TrimSpace(base), "/")
	p.nodeBase = p.base
}

func newCLIHTTPClient(router egress.Router) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
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
		p.note(fmt.Errorf("拉官方 MiniMax Code CLI %s: %w", path, err))
		status := http.StatusBadGateway
		if cliIsTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "暂时无法从官方源取回 MiniMax Code CLI", status)
		return
	}
	defer resp.Body.Close()

	if !cliOKStatus(resp.StatusCode) {
		p.note(fmt.Errorf("官方 MiniMax Code CLI %s 答 HTTP %d", path, resp.StatusCode))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	if resp.ContentLength > cliMaxBytes {
		p.note(fmt.Errorf("官方 MiniMax Code CLI %s 超过 %d 字节", path, cliMaxBytes))
		http.Error(w, "官方 MiniMax Code CLI 超出本设备允许的大小", http.StatusBadGateway)
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
	base := p.base
	if rel, ok := strings.CutPrefix(path, "node/"); ok {
		base, path = p.nodeBase, rel
	}
	if base == "" {
		return nil, errors.New("未配置官方 CLI 来源")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/"+path, nil)
	if err != nil {
		return nil, err
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

func (f *cliFlushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
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
		p.log.Warn("官方 MiniMax Code CLI 透传失败", "err", err.Error())
		return
	}
	p.log.Info("官方 MiniMax Code CLI 透传已恢复")
}
