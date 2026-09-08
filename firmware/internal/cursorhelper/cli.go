// cli.go 把官方 Cursor CLI（cursor-agent）的公开安装物从 Cursor 固定来源
// 白名单透传到本设备。
//
// 受限现场的电脑只需访问 LLM Gate Box：接入流程从
//
//	GET /cursor-helper/cli/install.sh|install.ps1
//	GET /cursor-helper/cli/lab/<版本>/<os>/<arch>/agent-cli-package.tar.gz|zip
//
// 取官方安装脚本与当前平台整树归档。脚本取自 cursor.com/install——官方给
// bash 与 PowerShell 的是同一个 URL，靠固定 query ?win32=true 区分，映射钉
// 死在本文件、盒子路径不带 query；归档取自 downloads.cursor.com/lab，逐版本
// 目录原样透传。盒子不内嵌、不落盘、不改制品字节。官方没有独立版本清单
// 端点、制品也没有 SHA-256 清单：版本钉在脚本正文里，gate 侧解析脚本取版本
// 并以 --version 自检（同 Grok 安装链的完整性水位）。
//
// 这不是开放代理：路径必须匹配下面的版本与平台白名单，非法路径在出站前
// 就 404；两个官方来源实测都不用 3xx，重定向一律不跟。无会话公共 GET|HEAD，
// 与 install.sh 同性质；内容是全体设备相同的公开安装物，不含用户数据与
// 凭据。§15.1：响应正文不进日志。
package cursorhelper

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
	// DefaultCLIInstallerBase 是官方安装脚本的完整 URL（单文件，不是目录）；
	// install.ps1 在它之上追加固定 query ?win32=true。
	DefaultCLIInstallerBase = "https://cursor.com/install"
	// DefaultCLILabBase 是逐版本整树归档的目录根 /lab/<版本>/<os>/<arch>/。
	DefaultCLILabBase = "https://downloads.cursor.com/lab"

	cliConnectTimeout      = 10 * time.Second
	cliTLSHandshakeTimeout = 10 * time.Second
	cliHeaderTimeout       = 30 * time.Second
	// 整树归档压缩 ~84MB；60 分钟覆盖板子 Wi-Fi 慢链路。
	cliBodyTimeout = 60 * time.Minute
	cliMaxBytes    = 512 << 20
	cliMaxInflight = 4
)

var (
	// 官方版本形如 2026.08.11-e8db854：日期 + 5–12 位小写短 hex。
	cliVersionRE = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{5,12}$`)
	// 扩展名绑定 os：linux/darwin 发 tar.gz，windows 发 zip；arch 另行校验。
	cliOSAssets = map[string]string{
		"linux":   "agent-cli-package.tar.gz",
		"darwin":  "agent-cli-package.tar.gz",
		"windows": "agent-cli-package.zip",
	}
)

// ValidCLIPath 报告 path 是否是官方安装流程会读取的固定路径：两个安装脚本，
// 或 lab/<版本>/<os>/<arch>/ 下当前平台的整树归档。
func ValidCLIPath(path string) bool {
	if path == "install.sh" || path == "install.ps1" {
		return true
	}
	if path == "" || len(path) > 120 {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "lab" || !cliVersionRE.MatchString(parts[1]) {
		return false
	}
	if parts[3] != "x64" && parts[3] != "arm64" {
		return false
	}
	asset, ok := cliOSAssets[parts[2]]
	return ok && parts[4] == asset
}

var cliForwardedRespHeaders = [...]string{
	"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
	"ETag", "Last-Modified", "Cache-Control",
}

// CLIProxy 是 /cursor-helper/cli/{path...} 的透传 handler。零值不可用。
type CLIProxy struct {
	log       *slog.Logger
	hc        *http.Client
	installer string
	lab       string
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
		lab:       DefaultCLILabBase,
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
func (p *CLIProxy) SetBases(installer, lab string) {
	p.installer = strings.TrimRight(strings.TrimSpace(installer), "/")
	p.lab = strings.TrimRight(strings.TrimSpace(lab), "/")
}

func newCLIHTTPClient(router egress.Router) *http.Client {
	return &http.Client{
		// 两个官方来源实测都不用 3xx；一律不跟，避免请求越过固定基址白名单。
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
		p.note(fmt.Errorf("拉官方 Cursor CLI %s: %w", path, err))
		status := http.StatusBadGateway
		if cliIsTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "暂时无法从官方源取回 Cursor CLI", status)
		return
	}
	defer resp.Body.Close()

	if !cliOKStatus(resp.StatusCode) {
		p.note(fmt.Errorf("官方 Cursor CLI %s 答 HTTP %d", path, resp.StatusCode))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	if resp.ContentLength > cliMaxBytes {
		p.note(fmt.Errorf("官方 Cursor CLI %s 超过 %d 字节", path, cliMaxBytes))
		http.Error(w, "官方 Cursor CLI 超出本设备允许的大小", http.StatusBadGateway)
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
	target := ""
	switch path {
	case "install.sh":
		target = p.installer
	case "install.ps1":
		// 官方 PowerShell 脚本 = 同一 URL 加固定 query。映射只在这里发生：
		// 白名单只看盒子路径，客户端自带的 query 到不了上游。
		if p.installer != "" {
			target = p.installer + "?win32=true"
		}
	default:
		// ValidCLIPath 保证此处恒为 lab/<版本>/<os>/<arch>/<归档>。
		if p.lab != "" {
			target = p.lab + "/" + strings.TrimPrefix(path, "lab/")
		}
	}
	if target == "" {
		return nil, errors.New("未配置官方 CLI 来源")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
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
		p.log.Warn("官方 Cursor CLI 透传失败", "err", err.Error())
		return
	}
	p.log.Info("官方 Cursor CLI 透传已恢复")
}
