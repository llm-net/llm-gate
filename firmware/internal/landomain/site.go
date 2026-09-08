package landomain

// LLM Gate官网的设备侧接口客户端：设备关联（匿名发起、轮询取令牌）与持设备令牌的
// 内网域名接口（申领托管域名 / 登记自有域名、改解析地址、DNS 检查、释放、提交 CSR、轮询证书订单）。
//
// 这是设备与官网之间唯一带凭据的连接：凭据只有一把由官网账号持有人确认后签发的
// 设备令牌（dvt_…），作用域仅限 /api/device/*，不代表设备身份，也不能被官网用来
// 控制设备。§15.1：令牌只进 Authorization 头，不进日志、错误信息或审计；响应体
// 只解析成结构体，不落日志。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

const (
	siteConnectTimeout        = 5 * time.Second
	siteTLSHandshakeTimeout   = 5 * time.Second
	siteResponseHeaderTimeout = 30 * time.Second
	siteOverallTimeout        = 40 * time.Second
	siteBodyCap               = 256 << 10
)

// SiteError 是官网按统一错误体 {"error":{"code","message"}} 拒绝请求。Message 是官网
// 写给管理员看的中文，可直接展示。
type SiteError struct {
	Status  int
	Code    string
	Message string
}

func (e *SiteError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("LLM Gate官网返回 HTTP %d（%s）", e.Status, e.Code)
}

// IsSiteError 报告 err 是否为官网的业务拒绝，并返回它。
func IsSiteError(err error) (*SiteError, bool) {
	var se *SiteError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// SiteClient 是官网设备侧接口的 HTTP 客户端。
type SiteClient struct {
	baseURL string
	http    *http.Client
	egress  egress.Router
}

// SiteOption 调整 SiteClient 装配。
type SiteOption func(*SiteClient)

// WithSiteEgress 接入出站策略（internal/egress，official_site 分类）；nil 恒直连。
func WithSiteEgress(r egress.Router) SiteOption { return func(c *SiteClient) { c.egress = r } }

// NewSiteClient 构造客户端；baseURL 为空取官网缺省。
func NewSiteClient(baseURL string, opts ...SiteOption) *SiteClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://llm.net"
	}
	c := &SiteClient{baseURL: baseURL}
	for _, o := range opts {
		o(c)
	}
	base := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: siteConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   siteTLSHandshakeTimeout,
		ResponseHeaderTimeout: siteResponseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	c.http = &http.Client{
		Timeout:   siteOverallTimeout,
		Transport: egress.TransportFor(c.egress, egress.ScopeOfficialSite, base),
	}
	return c
}

func (c *SiteClient) BaseURL() string { return c.baseURL }

// LinkStartRequest 是设备发起关联时自报的展示信息（不含设备身份）。
type LinkStartRequest struct {
	Model           string `json:"model,omitempty"`
	Name            string `json:"name,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
}

type LinkStartResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type LinkPollResponse struct {
	Status   string `json:"status"` // pending | approved | denied | expired | consumed
	Token    string `json:"token,omitempty"`
	LinkID   string `json:"linkId,omitempty"`
	Interval int    `json:"interval"`
	Account  struct {
		DisplayName string `json:"displayName"`
	} `json:"account"`
}

// String / GoString / LogValue：轮询应答里带设备令牌，任何格式化输出都不能把它带出去。
func (r LinkPollResponse) String() string {
	return fmt.Sprintf("LinkPollResponse{status=%s token=%s}", r.Status, redactToken(r.Token))
}
func (r LinkPollResponse) GoString() string { return r.String() }

func redactToken(token string) string {
	if token == "" {
		return "<none>"
	}
	return "<redacted>"
}

type LanDomainConfig struct {
	Enabled bool   `json:"enabled"`
	Suffix  string `json:"suffix"`
	DNS     bool   `json:"dns"`
	// OwnDomain：官网能为设备管理员自己的域名签证书；AcmeDelegateZone 是委托名所在子域（acme.llm.net）。
	OwnDomain        bool   `json:"ownDomain"`
	AcmeDelegateZone string `json:"acmeDelegateZone"`
	Authorities      struct {
		Google      bool `json:"google"`
		LetsEncrypt bool `json:"letsencrypt"`
	} `json:"authorities"`
}

type DomainInfo struct {
	ID string `json:"id"`
	// Kind：managed（<label>.llm.net，Label 非空）/ custom（自有域名，AcmeDelegate 非空）。
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	Hostname     string `json:"hostname"`
	TargetIP     string `json:"targetIp"`
	AcmeDelegate string `json:"acmeDelegate"`
	Certificate  *struct {
		CA       string `json:"ca"`
		NotAfter int64  `json:"notAfter"`
		IssuedAt int64  `json:"issuedAt"`
	} `json:"certificate"`
}

type LinkStatusResponse struct {
	Link struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Account struct {
			DisplayName string `json:"displayName"`
		} `json:"account"`
	} `json:"link"`
	Domain    *DomainInfo     `json:"domain"`
	LanDomain LanDomainConfig `json:"lanDomain"`
}

// DNSCheck 是官网用公共解析器核对自有域名三条记录的结果（只读）。各 Status：
// a / challenge 取 ok | mismatch | missing | error；caa 取 ok | blocked | none | error。
type DNSCheck struct {
	Hostname     string `json:"hostname"`
	AcmeDelegate string `json:"acmeDelegate"`
	TargetIP     string `json:"targetIp"`
	A            struct {
		Status    string   `json:"status"`
		Addresses []string `json:"addresses"`
	} `json:"a"`
	Challenge struct {
		Status string `json:"status"`
		Target string `json:"target"`
	} `json:"challenge"`
	CAA struct {
		Status    string   `json:"status"`
		FoundAt   string   `json:"foundAt"`
		Records   []string `json:"records"`
		Permitted []string `json:"permitted"`
	} `json:"caa"`
	// Ready：签发的前置条件（委托 CNAME 就位且 CAA 不拦）都满足；A 记录只影响能不能访问。
	Ready     bool  `json:"ready"`
	CheckedAt int64 `json:"checkedAt"`
}

type OrderInfo struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	CA       string `json:"ca"`
	Status   string `json:"status"` // pending | authorizing | challenging | finalizing | issued | failed
	// ChallengeTriggered：challenging 阶段已把挑战交给 CA（之前是在等 TXT 传播到公共解析器）。
	ChallengeTriggered bool   `json:"challengeTriggered"`
	Error              string `json:"error"`
	NotAfter           int64  `json:"notAfter"`
	RetryAfter         int    `json:"retryAfter"`
	CertificatePEM     string `json:"certificatePem"`
}

// Terminal 报告订单是否已到终态。
func (o *OrderInfo) Terminal() bool { return o.Status == "issued" || o.Status == "failed" }

func (c *SiteClient) do(ctx context.Context, method, path, token string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return errors.New("构造官网请求失败")
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return errors.New("构造官网请求失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "llmgate/"+buildinfo.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, siteBodyCap))
	if err != nil {
		return errors.New("读取官网响应失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		return &SiteError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("官网响应不是预期的 JSON")
	}
	return nil
}

func transportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("连接 LLM Gate官网超时")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("连接 LLM Gate官网超时")
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("无法连接 LLM Gate官网")
}

func (c *SiteClient) LinkStart(ctx context.Context, req LinkStartRequest) (*LinkStartResponse, error) {
	var out LinkStartResponse
	if err := c.do(ctx, http.MethodPost, "/api/device/link/start", "", req, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, errors.New("官网未返回关联码")
	}
	return &out, nil
}

func (c *SiteClient) LinkPoll(ctx context.Context, deviceCode string) (*LinkPollResponse, error) {
	var out LinkPollResponse
	body := struct {
		DeviceCode string `json:"deviceCode"`
	}{DeviceCode: deviceCode}
	if err := c.do(ctx, http.MethodPost, "/api/device/link/poll", "", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *SiteClient) LinkStatus(ctx context.Context, token string) (*LinkStatusResponse, error) {
	var out LinkStatusResponse
	if err := c.do(ctx, http.MethodGet, "/api/device/link", token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *SiteClient) Unlink(ctx context.Context, token string) error {
	return c.do(ctx, http.MethodPost, "/api/device/link/unlink", token, struct{}{}, nil)
}

func (c *SiteClient) Claim(ctx context.Context, token, label, targetIP string) (*DomainInfo, error) {
	var out struct {
		Domain DomainInfo `json:"domain"`
	}
	body := struct {
		Label    string `json:"label"`
		TargetIP string `json:"targetIp"`
	}{Label: label, TargetIP: targetIP}
	if err := c.do(ctx, http.MethodPost, "/api/device/domain/claim", token, body, &out); err != nil {
		return nil, err
	}
	if out.Domain.Hostname == "" {
		return nil, errors.New("官网未返回域名")
	}
	return &out.Domain, nil
}

// RegisterCustom 登记自有域名：官网只落库并分配 DNS-01 委托名，不写解析记录。
func (c *SiteClient) RegisterCustom(ctx context.Context, token, hostname, targetIP string) (*DomainInfo, error) {
	var out struct {
		Domain DomainInfo `json:"domain"`
	}
	body := struct {
		Hostname string `json:"hostname"`
		TargetIP string `json:"targetIp"`
	}{Hostname: hostname, TargetIP: targetIP}
	if err := c.do(ctx, http.MethodPost, "/api/device/domain/custom", token, body, &out); err != nil {
		return nil, err
	}
	if out.Domain.Hostname == "" {
		return nil, errors.New("官网未返回域名")
	}
	return &out.Domain, nil
}

// DNSCheck 请官网核对自有域名的公开 DNS 记录；targetIP 是设备期望 A 记录指到的地址。
func (c *SiteClient) DNSCheck(ctx context.Context, token, targetIP string) (*DNSCheck, error) {
	var out struct {
		Check *DNSCheck `json:"check"`
	}
	body := struct {
		TargetIP string `json:"targetIp,omitempty"`
	}{TargetIP: targetIP}
	if err := c.do(ctx, http.MethodPost, "/api/device/domain/dns-check", token, body, &out); err != nil {
		return nil, err
	}
	if out.Check == nil {
		return nil, errors.New("官网未返回 DNS 检查结果")
	}
	return out.Check, nil
}

func (c *SiteClient) SetTarget(ctx context.Context, token, targetIP string) (*DomainInfo, error) {
	var out struct {
		Domain DomainInfo `json:"domain"`
	}
	body := struct {
		TargetIP string `json:"targetIp"`
	}{TargetIP: targetIP}
	if err := c.do(ctx, http.MethodPut, "/api/device/domain/target", token, body, &out); err != nil {
		return nil, err
	}
	return &out.Domain, nil
}

func (c *SiteClient) Release(ctx context.Context, token string) error {
	return c.do(ctx, http.MethodPost, "/api/device/domain/release", token, struct{}{}, nil)
}

func (c *SiteClient) OrderCertificate(ctx context.Context, token, csrPEM string) (*OrderInfo, error) {
	var out struct {
		Order OrderInfo `json:"order"`
	}
	body := struct {
		CSRPEM string `json:"csrPem"`
	}{CSRPEM: csrPEM}
	if err := c.do(ctx, http.MethodPost, "/api/device/domain/certificate", token, body, &out); err != nil {
		return nil, err
	}
	if out.Order.ID == "" {
		return nil, errors.New("官网未返回证书订单")
	}
	return &out.Order, nil
}

func (c *SiteClient) CertificateStatus(ctx context.Context, token string) (*OrderInfo, error) {
	var out struct {
		Order *OrderInfo `json:"order"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/device/domain/certificate", token, nil, &out); err != nil {
		return nil, err
	}
	if out.Order == nil {
		return nil, errors.New("官网没有这台设备的证书订单")
	}
	return out.Order, nil
}
