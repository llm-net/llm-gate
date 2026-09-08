// cli.go 把官方 Grok CLI 的公开安装物从 x.ai 白名单透传到本设备。
//
// 受限现场的电脑够不着 x.ai / GCS，但盒子出网（Grok 订阅已经走这条出网）。
// 接入脚本本机没有 grok 时，从
//
//	GET /grok-helper/cli/{stable|alpha|enterprise}
//	GET /grok-helper/cli/grok-<版本>-<os>-<arch>[.exe]
//
// 取通道指针和对应平台二进制；盒子按同样的相对路径去拉
// https://x.ai/cli/…，失败再试 GCS 回落。设备不内嵌、不落盘、不改字节，
// 也不是开放代理——名字对不上白名单的请求根本不出站。
//
// 无会话公共 GET|HEAD，与 install.sh 同性质：转的是全体设备同一份的公开
// 安装物，不含用户数据与上游凭据。§15.1：响应正文不进日志（access log
// 只记路径 / 状态 / 字节）；上游失败只在成功↔失败翻转时各记一条。
package grokhelper

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
	// DefaultCLIPrimaryBase 是官方安装脚本的第一来源（Cloudflare 前置的 x.ai）。
	DefaultCLIPrimaryBase = "https://x.ai/cli"
	// DefaultCLIFallbackBase 是官方安装脚本在 x.ai 不可达时的 GCS 回落。
	DefaultCLIFallbackBase = "https://storage.googleapis.com/grok-build-public-artifacts/cli"

	cliConnectTimeout      = 10 * time.Second
	cliTLSHandshakeTimeout = 10 * time.Second
	cliHeaderTimeout       = 30 * time.Second
	// cliBodyTimeout 覆盖整次出站（含 160MB 级二进制体）。官方 linux-x86_64
	// 约 159 MiB，板子 Wi-Fi 出网按几分钟计；15 分钟是上限不是目标。
	cliBodyTimeout = 15 * time.Minute
	// cliMaxBytes 是单件上限。当前最大官方件约 159 MiB，512 MiB 留增长余量；
	// 超了按坏件掐掉，避免把盒子当任意大文件跳板。
	cliMaxBytes = 512 << 20
	// cliMaxInflight 限制同时在飞的上游拉取。一件就要占满板子出网，
	// 4 够两个人交错安装；满了排队等，不是拒绝。
	cliMaxInflight = 4
)

// 官方通道指针文件名；与 install.sh 里 GROK_CHANNEL 三值对齐。
var cliChannels = map[string]struct{}{
	"stable":     {},
	"alpha":      {},
	"enterprise": {},
}

// grok-<X.Y.Z[-suffix]>-<os>-<arch>[.exe] ；.exe 只许配 windows。
var cliArtifactRe = regexp.MustCompile(`^grok-[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9._]+)?-(linux|macos|windows)-(x86_64|aarch64)(\.exe)?$`)

// ValidCLIName 报告 name 是否是允许出站的通道指针或官方制品名。
func ValidCLIName(name string) bool {
	if name == "" || len(name) > 96 {
		return false
	}
	if _, ok := cliChannels[name]; ok {
		return true
	}
	m := cliArtifactRe.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	// m[1]=os m[3]=.exe 或空
	if m[3] == ".exe" && m[1] != "windows" {
		return false
	}
	return true
}

// 上游响应里唯一会拷回客户端的头。Set-Cookie（Cloudflare 的 __cf_bm）等一律不过。
var cliForwardedRespHeaders = [...]string{
	"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
	"ETag", "Last-Modified", "Cache-Control",
}

// CLIProxy 是 /grok-helper/cli/{name} 的透传 handler。零值不可用。
type CLIProxy struct {
	log      *slog.Logger
	hc       *http.Client
	primary  string
	fallback string
	sem      chan struct{}

	mu     sync.Mutex
	failed bool
}

// NewCLIProxy 构造官方 CLI 安装物透传。logger 只收连接层与路径，不收正文。
func NewCLIProxy(logger *slog.Logger, opts ...Option) *CLIProxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &CLIProxy{
		log:      logger,
		primary:  DefaultCLIPrimaryBase,
		fallback: DefaultCLIFallbackBase,
		sem:      make(chan struct{}, cliMaxInflight),
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

// SetBases 替换出站基址，只给测试用（指向 httptest）。空串表示这一侧不试。
func (p *CLIProxy) SetBases(primary, fallback string) {
	p.primary = strings.TrimRight(strings.TrimSpace(primary), "/")
	p.fallback = strings.TrimRight(strings.TrimSpace(fallback), "/")
}

func newCLIHTTPClient(router egress.Router) *http.Client {
	return &http.Client{
		// 不跟随重定向：3xx 当这一侧失败、改试回落。跟过去可能把请求送到
		// 白名单外的主机。整体 Timeout 不设——体传输用请求 context 的 15 分钟。
		CheckRedirect: func(*http.Request, []*http.Request) error {
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

// ServeHTTP 处理 GET|HEAD /grok-helper/cli/{name}。
func (p *CLIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.PathValue("name")
	if !ValidCLIName(name) {
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

	resp, err := p.open(ctx, r.Method, name, r.Header.Get("Range"))
	if err != nil {
		p.note(fmt.Errorf("拉官方 grok CLI %s: %w", name, err))
		status := http.StatusBadGateway
		if cliIsTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "暂时无法从官方源取回 grok CLI", status)
		return
	}
	defer resp.Body.Close()

	if !cliOKStatus(resp.StatusCode) {
		// 两侧都不是 2xx/206：把最终状态空体答回（404 就是「这个平台还没有」），
		// 不把上游 HTML/JSON 错误页转给 curl|sh。
		p.note(fmt.Errorf("官方 grok CLI %s 答 HTTP %d", name, resp.StatusCode))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}

	if resp.ContentLength > cliMaxBytes {
		p.note(fmt.Errorf("官方 grok CLI %s 超过 %d 字节", name, cliMaxBytes))
		http.Error(w, "官方 grok CLI 超出本设备允许的大小", http.StatusBadGateway)
		return
	}

	p.note(nil)
	h := w.Header()
	for _, k := range cliForwardedRespHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	n, _ := io.Copy(&cliFlushWriter{w: w}, io.LimitReader(resp.Body, cliMaxBytes+1))
	if n > cliMaxBytes {
		// 头已经出去了，只能掐断连接：net/http 对 ErrAbortHandler 静默关连接，
		// 客户端拿到的是一次失败的下载而不是一份被截半的文件。
		panic(http.ErrAbortHandler)
	}
}

func cliOKStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusPartialContent
}

// cliFlushWriter 每写出约 1 MiB 刷一次，避免 160 MB 二进制在盒子里攒满再吐。
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

func (p *CLIProxy) open(ctx context.Context, method, name, rng string) (*http.Response, error) {
	bases := make([]string, 0, 2)
	if p.primary != "" {
		bases = append(bases, p.primary)
	}
	if p.fallback != "" {
		bases = append(bases, p.fallback)
	}
	var last *http.Response
	var firstErr error
	for i, base := range bases {
		resp, err := p.get(ctx, method, base, name, rng)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if cliOKStatus(resp.StatusCode) {
			if last != nil {
				_ = last.Body.Close()
			}
			return resp, nil
		}
		if last != nil {
			_ = last.Body.Close()
		}
		last = resp
		if firstErr == nil {
			firstErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if i == len(bases)-1 {
			return last, nil
		}
	}
	if last != nil {
		return last, nil
	}
	if firstErr == nil {
		firstErr = errors.New("未配置官方 CLI 来源")
	}
	return nil, firstErr
}

func (p *CLIProxy) get(ctx context.Context, method, base, name, rng string) (*http.Response, error) {
	u := strings.TrimRight(base, "/") + "/" + name
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return p.hc.Do(req)
}

func cliIsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// note 记下这一次透传的成败：只在翻转时各记一条，不因客户反复安装而刷屏。
// err 里只有连接层信息与路径，没有响应内容。
func (p *CLIProxy) note(err error) {
	p.mu.Lock()
	flip := p.failed != (err != nil)
	p.failed = err != nil
	p.mu.Unlock()
	if !flip {
		return
	}
	if err != nil {
		p.log.Warn("官方 grok CLI 透传失败", "err", err.Error())
		return
	}
	p.log.Info("官方 grok CLI 透传已恢复")
}
