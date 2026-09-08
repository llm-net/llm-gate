package officialsite

// 可选第三方组件（当前只有 cloudflared）的两样官网只读物：
//
//   - 签名组件清单 /updates/components/<组件>/stable.json 及其离线签名 stable.json.sig。
//     官网只发布这份小型元数据，**不镜像 executable**；验签、版本防回退与条目筛选
//     在 internal/cloudflared 里做，本文件只负责匿名取回原始字节。
//   - 官方制品直下：清单指定的精确 URL（Cloudflare 官方 GitHub release）。GitHub 的
//     release 资产恒经一次 302 到 *.githubusercontent.com，所以这里带一条**逐跳校验**
//     的重定向策略：只许 HTTPS、只许清单允许的主机、跳数封顶。长度按清单精确上限
//     截断，摘要流式计算，正文一个字节都不进日志。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// ComponentIndexPathFmt 是签名组件清单的路径模板（%s 为组件名）。
	ComponentIndexPathFmt = "/updates/components/%s/stable.json"
	// ComponentSignatureSuffix 是清单离线签名文件的后缀。
	ComponentSignatureSuffix = ".sig"

	componentIndexCap     = 256 << 10
	componentSignatureCap = 4 << 10
	// componentMaxRedirects 是制品直下允许的最多跳数（GitHub 实际是 1 跳）。
	componentMaxRedirects = 4
)

// ComponentIndexPath 返回某组件清单在官网上的路径。
func ComponentIndexPath(component string) string {
	return fmt.Sprintf(ComponentIndexPathFmt, component)
}

// FetchComponentIndex 匿名取回组件清单与其签名的原始字节。两份都是必需的：
// 没有签名的清单不构成允许决策，调用方一律拒绝。
func (c *Client) FetchComponentIndex(ctx context.Context, component string) (index, signature []byte, err error) {
	path := ComponentIndexPath(component)
	_, index, _, err = c.fetchJSON(ctx, path, "", componentIndexCap, "组件清单")
	if err != nil {
		return nil, nil, err
	}
	_, signature, _, err = c.fetchJSON(ctx, path+ComponentSignatureSuffix, "", componentSignatureCap, "组件清单签名")
	if err != nil {
		return nil, nil, err
	}
	return index, signature, nil
}

// ArtifactPolicy 是一次制品直下的边界：允许的主机集合（首跳与每一跳重定向都要
// 命中）与精确字节上限。
type ArtifactPolicy struct {
	AllowedHosts []string
	MaxBytes     int64
	// allowTestHosts 是测试专用钩子：本地 httptest TLS 服务绕不开显式端口，而生产
	// 策略拒绝任何带端口的地址。只有包内测试能设置它（host:port 精确匹配）。
	allowTestHosts []string
}

func (p ArtifactPolicy) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, h := range p.AllowedHosts {
		if strings.ToLower(h) == host {
			return true
		}
	}
	return false
}

// ErrArtifactPolicy 表示制品地址或重定向落在允许边界之外。
var ErrArtifactPolicy = errors.New("组件制品地址不在允许边界内")

// FetchComponentArtifact 按策略下载一个官方制品：首跳与每一跳都必须是允许主机上
// 的 HTTPS，跳数封顶；正文流式写入 w 并计算 SHA-256，超过 MaxBytes 立即失败。
// 返回摘要十六进制与实际字节数。
func (c *Client) FetchComponentArtifact(ctx context.Context, artifactURL string, pol ArtifactPolicy, w io.Writer) (string, int64, error) {
	if err := checkArtifactURL(artifactURL, pol); err != nil {
		return "", 0, err
	}
	if pol.MaxBytes <= 0 {
		return "", 0, errors.New("组件制品缺少字节上限")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return "", 0, errors.New("构造组件下载请求失败")
	}
	tr := newTransport()
	if c.artifactTLS != nil {
		tr.TLSClientConfig = c.artifactTLS
	}
	// 第三方制品走 component_artifacts 分类：首次装本地代理内核时 127.0.0.1 上还没有
	// 代理，管理员可以让这一类保持直连、或经已配置的外部 SOCKS5。
	client := &http.Client{
		Transport: egress.TransportFor(c.egress, egress.ScopeComponentArtifacts, tr),
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= componentMaxRedirects {
				return fmt.Errorf("%w：重定向次数超过 %d", ErrArtifactPolicy, componentMaxRedirects)
			}
			if err := checkArtifactURL(next.URL.String(), pol); err != nil {
				return err
			}
			// 跨主机跳转不带任何来源头（本来也没有凭据，这里只是把边界说死）。
			next.Header.Del("Authorization")
			next.Header.Del("Cookie")
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && errors.Is(ue.Err, ErrArtifactPolicy) {
			return "", 0, ue.Err
		}
		return "", 0, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, &HTTPError{Status: resp.StatusCode, Path: req.URL.Path}
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hasher), io.LimitReader(resp.Body, pol.MaxBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("下载组件制品中断: %w", err)
	}
	if n > pol.MaxBytes {
		return "", 0, fmt.Errorf("组件制品超过 %d 字节上限", pol.MaxBytes)
	}
	if n == 0 {
		return "", 0, errors.New("组件制品为空")
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// checkArtifactURL 校验一个制品地址（首跳或重定向目标）：HTTPS、无 userinfo、
// 主机在允许集合内、不带显式端口。
func checkArtifactURL(raw string, pol ArtifactPolicy) error {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
		return fmt.Errorf("%w：必须是不带账号的 HTTPS 地址", ErrArtifactPolicy)
	}
	for _, h := range pol.allowTestHosts {
		if p.Host == h {
			return nil
		}
	}
	if !pol.hostAllowed(p.Hostname()) || p.Port() != "" {
		return fmt.Errorf("%w：主机 %s 不在允许集合内", ErrArtifactPolicy, p.Hostname())
	}
	return nil
}
