package updated

// UDS 客户端：gatewayd 侧唯一的引擎入口。拨不通 socket 统一归为
// ErrUnavailable——dev 机、没装 llmgate-updated 单元的老部署都会落在这一态，
// 管理面据此答 503 engine_unavailable 并指向手工替换的老路
// （docs-board/deployment.md）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrUnavailable 表示引擎 socket 不可达（未部署/未启动）。
var ErrUnavailable = errors.New("升级引擎不可用")

// Client 经 UDS 调用引擎。零值不可用，经 NewClient 构造；并发安全。
type Client struct {
	socket string
	hc     *http.Client
	// slow 给组件安装/回退与 connector 启停用：它们同步跑完文件拷贝、自述版本
	// 核对与就绪窗口，十秒预算不够。
	slow *http.Client
}

// NewClient 构造引擎客户端。socket 为空取包缺省。
func NewClient(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	sock := socket
	transport := func() *http.Transport {
		return &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "unix", sock)
			},
		}
	}
	return &Client{
		socket: sock,
		hc: &http.Client{
			Transport: transport(),
			// install/rollback 的受理是即答的（执行异步），预算给足即可。
			Timeout: 10 * time.Second,
		},
		slow: &http.Client{Transport: transport(), Timeout: 120 * time.Second},
	}
}

// Socket 返回客户端指向的 socket 路径（读数展示用）。
func (c *Client) Socket() string { return c.socket }

// Status 取引擎快照；socket 不可达返回 ErrUnavailable。
func (c *Client) Status(ctx context.Context) (*Status, error) {
	return c.do(ctx, http.MethodGet, "/status", nil)
}

// Install 请求安装一个已 staging 的固件包（受理即返回，结果落在引擎状态）。
func (c *Client) Install(ctx context.Context, req InstallRequest) (*Status, error) {
	return c.do(ctx, http.MethodPost, "/install", req)
}

// Rollback 请求回退到上一版本。
func (c *Client) Rollback(ctx context.Context) (*Status, error) {
	return c.do(ctx, http.MethodPost, "/rollback", nil)
}

func (c *Client) do(ctx context.Context, method, path string, payload any) (*Status, error) {
	var st Status
	if err := c.doJSON(ctx, c.hc, method, path, payload, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ---- 组件与 Tunnel ----

// ComponentStatus 取一个组件的 slot 读数。
func (c *Client) ComponentStatus(ctx context.Context, name string) (*ComponentStatus, error) {
	var st ComponentStatus
	if err := c.doJSON(ctx, c.hc, http.MethodGet, "/components/"+name, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ComponentInstall 请求把一个已 staging 的组件制品装进非活动 slot（同步等待
// 安装、自述版本核对与 connector 就绪）。ErrInstallNotReady 表示装上了但新版本
// 没就绪、已切回旧版。
func (c *Client) ComponentInstall(ctx context.Context, name string, req ComponentInstallRequest) (*ComponentStatus, error) {
	var st ComponentStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/components/"+name+"/install", req, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ComponentRollback 把组件切回上一个 slot。
func (c *Client) ComponentRollback(ctx context.Context, name string) (*ComponentStatus, error) {
	var st ComponentStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/components/"+name+"/rollback", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ComponentRemove 卸载组件（先停 connector）。
func (c *Client) ComponentRemove(ctx context.Context, name string) (*ComponentStatus, error) {
	var st ComponentStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/components/"+name+"/remove", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TunnelStatus 取 connector unit 读数。
func (c *Client) TunnelStatus(ctx context.Context) (*TunnelStatus, error) {
	var st TunnelStatus
	if err := c.doJSON(ctx, c.hc, http.MethodGet, "/tunnel", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TunnelStart 把 token 交给引擎写运行期文件并启动 connector。token 只在这一次
// 请求体里出现。
func (c *Client) TunnelStart(ctx context.Context, token string) (*TunnelStatus, error) {
	var st TunnelStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/tunnel/start", tunnelStartWire{Token: token}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TunnelStop 停掉 connector 并清除运行期 token。
func (c *Client) TunnelStop(ctx context.Context) (*TunnelStatus, error) {
	var st TunnelStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/tunnel/stop", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ProxyStatus 取板上代理内核 unit 的读数。
func (c *Client) ProxyStatus(ctx context.Context) (*ProxyStatus, error) {
	var st ProxyStatus
	if err := c.doJSON(ctx, c.hc, http.MethodGet, "/proxy-core", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ProxyStart 把生成好的内核配置交给引擎写运行期文件并启动（或按需重启）内核，
// 同步等就绪。配置正文只在这一次请求体里出现。
func (c *Client) ProxyStart(ctx context.Context, config string) (*ProxyStatus, error) {
	var st ProxyStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/proxy-core/start", proxyStartWire{Config: config}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ProxyStop 停掉内核并清除运行期配置。
func (c *Client) ProxyStop(ctx context.Context) (*ProxyStatus, error) {
	var st ProxyStatus
	if err := c.doJSON(ctx, c.slow, http.MethodPost, "/proxy-core/stop", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// EngineError 是引擎按状态码拒绝时带回的错误：Status 供调用方区分 404/409/400。
type EngineError struct {
	Status  int
	Message string
}

func (e *EngineError) Error() string { return e.Message }

func (c *Client) doJSON(ctx context.Context, hc *http.Client, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("序列化引擎请求: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	// host 段只为凑出合法 URL；拨号恒走 unix socket。
	req, err := http.NewRequestWithContext(ctx, method, "http://updated"+path, body)
	if err != nil {
		return fmt.Errorf("构造引擎请求: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w（%s）", ErrUnavailable, c.socket)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取引擎应答: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var we wireError
		if json.Unmarshal(raw, &we) == nil && we.Error != "" {
			if resp.StatusCode == http.StatusConflict && path == "/install" || resp.StatusCode == http.StatusConflict && path == "/rollback" {
				return fmt.Errorf("%w: %s", ErrBusy, we.Error)
			}
			return &EngineError{Status: resp.StatusCode, Message: we.Error}
		}
		return fmt.Errorf("引擎返回 HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("引擎应答形状不符: %w", err)
	}
	return nil
}
