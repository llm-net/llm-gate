// cli.go 把官方 Codex CLI 的公开安装物从 OpenAI 固定来源白名单透传到本设备。
//
// 受限现场的电脑只需访问 LLM Gate Box：接入脚本从
//
//	GET /codex-helper/cli/install.sh|install.ps1
//	GET /codex-helper/cli/channels/latest
//	GET /codex-helper/cli/releases/<版本>/<公开安装物>
//
// 取官方安装脚本、版本元数据、SHA-256 清单与当前平台包。盒子分别去
// chatgpt.com/codex 与 releases.openai.com/codex 拉取，不内嵌、不落盘、
// 不改制品字节。接入脚本只把官方安装脚本里的 release 根替换成设备路径，
// 校验与原子安装仍由官方脚本完成。
//
// 这不是开放代理：路径必须匹配下面的版本与制品白名单，非法路径在出站前
// 就 404。无会话公共 GET|HEAD，与 install.sh 同性质；内容是全体设备相同的
// 公开安装物，不含用户数据与凭据。§15.1：响应正文不进日志。
package codexhelper

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
	DefaultCLIInstallerBase = "https://chatgpt.com/codex"
	DefaultCLIReleasesBase  = "https://releases.openai.com/codex"

	cliConnectTimeout      = 10 * time.Second
	cliTLSHandshakeTimeout = 10 * time.Second
	cliHeaderTimeout       = 30 * time.Second
	// 官方 standalone package 是百 MB 级；15 分钟覆盖板子 Wi-Fi 慢链路。
	cliBodyTimeout = 15 * time.Minute
	// 给当前平台包留增长余量，同时阻止把盒子当任意大文件跳板。
	cliMaxBytes    = 512 << 20
	cliMaxInflight = 4
)

var (
	cliVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-alpha(?:\.[0-9]+){0,2}|-beta(?:\.[0-9]+)?)?$`)
	// 官方当前优先发布 codex-package；legacy 平台 npm 包仍在官方安装脚本的
	// 兼容分支中，所以白名单照收。文件名不得含斜杠，版本目录另行校验。
	cliPackageRe = regexp.MustCompile(`^codex-package-[A-Za-z0-9._-]+\.(?:tar\.gz|zip)$`)
	cliLegacyRe  = regexp.MustCompile(`^codex-npm-(?:darwin|linux|win32)-(?:arm64|x64)-[0-9A-Za-z._-]+\.tgz$`)
)

// ValidCLIPath 报告 path 是否是官方安装流程会读取的固定路径。
func ValidCLIPath(path string) bool {
	if path == "install.sh" || path == "install.ps1" || path == "channels/latest" {
		return true
	}
	if path == "" || len(path) > 240 {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] != "releases" || !cliVersionRe.MatchString(parts[1]) {
		return false
	}
	asset := parts[2]
	return asset == "release.json" || asset == "codex-package_SHA256SUMS" ||
		cliPackageRe.MatchString(asset) || cliLegacyRe.MatchString(asset)
}

var cliForwardedRespHeaders = [...]string{
	"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
	"ETag", "Last-Modified", "Cache-Control",
}

// CLIProxy 是 /codex-helper/cli/{path...} 的透传 handler。零值不可用。
type CLIProxy struct {
	log       *slog.Logger
	hc        *http.Client
	installer string
	releases  string
	sem       chan struct{}

	mu     sync.Mutex
	failed bool
}

func NewCLIProxy(logger *slog.Logger, opts ...Option) *CLIProxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &CLIProxy{
		log: logger,

		installer: DefaultCLIInstallerBase,
		releases:  DefaultCLIReleasesBase,
		sem:       make(chan struct{}, cliMaxInflight),
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

// SetBases 替换两类固定来源，只给测试用。空串表示该类来源不可用。
func (p *CLIProxy) SetBases(installer, releases string) {
	p.installer = strings.TrimRight(strings.TrimSpace(installer), "/")
	p.releases = strings.TrimRight(strings.TrimSpace(releases), "/")
}

func newCLIHTTPClient(router egress.Router) *http.Client {
	return &http.Client{
		// 官方 chatgpt.com 安装入口精确跳到 releases.openai.com 的
		// 同名脚本；只放行这一跳。其余 3xx 不跟，避免请求越过
		// 本文件的两个官方基址白名单。
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
	if req == nil || len(via) != 1 || via[0] == nil {
		return false
	}
	from, to := via[0].URL, req.URL
	if from == nil || to == nil || from.Scheme != "https" || from.Host != "chatgpt.com" ||
		to.Scheme != "https" || to.Host != "releases.openai.com" || from.RawQuery != "" || to.RawQuery != "" {
		return false
	}
	return (from.Path == "/codex/install.sh" && to.Path == "/codex/install.sh") ||
		(from.Path == "/codex/install.ps1" && to.Path == "/codex/install.ps1")
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
		p.note(fmt.Errorf("拉官方 Codex CLI %s: %w", path, err))
		status := http.StatusBadGateway
		if cliIsTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "暂时无法从官方源取回 Codex CLI", status)
		return
	}
	defer resp.Body.Close()

	if !cliOKStatus(resp.StatusCode) {
		p.note(fmt.Errorf("官方 Codex CLI %s 答 HTTP %d", path, resp.StatusCode))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	if resp.ContentLength > cliMaxBytes {
		p.note(fmt.Errorf("官方 Codex CLI %s 超过 %d 字节", path, cliMaxBytes))
		http.Error(w, "官方 Codex CLI 超出本设备允许的大小", http.StatusBadGateway)
		return
	}

	p.note(nil)
	for _, k := range cliForwardedRespHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
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

func (p *CLIProxy) open(ctx context.Context, method, path, rng string) (*http.Response, error) {
	base := p.releases
	if path == "install.sh" || path == "install.ps1" {
		base = p.installer
	}
	if base == "" {
		return nil, errors.New("未配置官方 CLI 来源")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	if rng != "" {
		req.Header.Set("Range", rng)
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
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// note 只记录成功/失败翻转；err 不含响应正文。
func (p *CLIProxy) note(err error) {
	p.mu.Lock()
	flip := p.failed != (err != nil)
	p.failed = err != nil
	p.mu.Unlock()
	if !flip {
		return
	}
	if err != nil {
		p.log.Warn("官方 Codex CLI 透传失败", "err", err.Error())
		return
	}
	p.log.Info("官方 Codex CLI 透传已恢复")
}
