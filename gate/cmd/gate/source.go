package main

// source.go 定义开发工具安装物的取回顺序：先直连该工具的官方地址，失败再改经
// 盒子的 /<tool>-helper/cli/* 白名单透传。
//
// 两边给的是同一份公开字节：盒子那头的 helper 本来就是按固定映射去官方源
// 拉，这里把同一张映射表搬到客户端（与固件 internal/*helper 的 Default* 基址
// 和 open() 逐条对应），使用者电脑能出网时不必让每个字节都绕经设备出网口；
// 受限现场官方地址不可达时，只为第一次请求付一次连接超时，之后整次安装都
// 走盒子。校验（大小、SHA-256、自检版本）不区分来源，一律照做。

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 各工具官方安装物的固定基址。var 仅供测试改成假上游或清空；清空即该官方
// 来源不可用，直接经盒子。
var (
	codexOfficialReleasesBase    = "https://releases.openai.com/codex"
	grokOfficialPrimaryBase      = "https://x.ai/cli"
	grokOfficialFallbackBase     = "https://storage.googleapis.com/grok-build-public-artifacts/cli"
	claudeOfficialReleasesBase   = "https://downloads.claude.ai/claude-code-releases"
	cursorOfficialInstallerURL   = "https://cursor.com/install"
	cursorOfficialLabBase        = "https://downloads.cursor.com/lab"
	openCodeOfficialAPIBase      = "https://api.github.com/repos/anomalyco/opencode/releases"
	openCodeOfficialReleasesBase = "https://github.com/anomalyco/opencode/releases/download"
	mcodeOfficialBase            = "https://filecdn.minimax.chat/public"
	mcodeNodeOfficialBase        = "https://nodejs.org/dist"
)

func (a *app) mcodeOrigins() *originChain {
	client := *a.hc
	client.Timeout = 30 * time.Minute
	installer, node := underBase(mcodeOfficialBase), underBase(mcodeNodeOfficialBase)
	return a.newOriginChain("MiniMax Code", "/mcode-helper/cli", &client, artifactOrigin{
		label: "官方源", url: func(rel string) string {
			if path, ok := strings.CutPrefix(rel, "node/"); ok {
				if node != nil {
					return node(path)
				}
			} else if installer != nil {
				return installer(rel)
			}
			return ""
		},
		client: officialHTTPClient(30*time.Minute, nil),
	})
}

const (
	officialConnectTimeout = 10 * time.Second
	officialHeaderTimeout  = 30 * time.Second
)

// artifactOrigin 是一处安装物来源。
type artifactOrigin struct {
	// label 是提示里的人读名："官方源" 或 "设备"。
	label string
	// url 把工具相对路径（恒与盒子 helper 白名单同一路径）映射为完整 URL；
	// 该来源不提供此路径时返回空串。
	url func(rel string) string
	// header 可选：官方 API 要求的固定请求头。
	header func(rel string, h http.Header)
	client *http.Client
}

// originChain 是一次安装内的来源顺序：前面是官方地址，最后恒是设备。某个来源
// 一旦失败，链就前进到下一个且不再回头——同一次安装里的后续元数据与制品都
// 从新来源取。零值不可用。
type originChain struct {
	a       *app
	tool    string
	origins []artifactOrigin
	cur     int
}

// newOriginChain 拼出 tool 的来源链：official 按顺序在前，设备 helper 在后。
func (a *app) newOriginChain(tool, devicePath string, deviceClient *http.Client, official ...artifactOrigin) *originChain {
	origins := make([]artifactOrigin, 0, len(official)+1)
	for _, o := range official {
		if o.url != nil {
			origins = append(origins, o)
		}
	}
	base := a.cfg.BaseURL + devicePath + "/"
	origins = append(origins, artifactOrigin{
		label:  "设备",
		url:    func(rel string) string { return base + rel },
		client: deviceClient,
	})
	return &originChain{a: a, tool: tool, origins: origins}
}

// label 报告下一次请求会先试的来源，供安装叙述用。
func (c *originChain) label() string { return c.origins[c.cur].label }

// advance 记下当前来源失败并前进；已在最后一个来源时返回 false。
func (c *originChain) advance(rel string, err error) bool {
	if c.cur >= len(c.origins)-1 {
		return false
	}
	failed := c.origins[c.cur]
	c.cur++
	if c.a != nil && c.a.err != nil {
		fmt.Fprintf(c.a.err, "%s %s不可用（%v），改经%s取回 %s。\n",
			c.tool, failed.label, err, c.origins[c.cur].label, rel)
	}
	return true
}

// get 取一份小体量元数据（版本指针、manifest、清单）。
func (c *originChain) get(rel string, maxBytes int64) ([]byte, error) {
	for {
		origin := c.origins[c.cur]
		url := origin.url(rel)
		var body []byte
		var err error
		if url == "" {
			err = errors.New("该来源不提供此路径")
		} else {
			body, err = getPublicURL(origin.client, url, rel, origin.header, maxBytes)
		}
		if err == nil {
			return body, nil
		}
		if !c.advance(rel, err) {
			return nil, err
		}
	}
}

// fetch 经 fetch.go 的续传下载器取大制品。续传件按工具与相对路径定名而不是
// 按 URL：官方源半途掐断后改经设备，已经收下的字节照样接着用。
func (c *originChain) fetch(rel string, f artifactFetch) (string, error) {
	f.key = c.tool + "/" + rel
	for {
		origin := c.origins[c.cur]
		f.url = origin.url(rel)
		f.client = origin.client
		var path string
		var err error
		if f.url == "" {
			err = errors.New("该来源不提供此路径")
		} else {
			path, err = c.a.fetchArtifact(f)
		}
		if err == nil {
			return path, nil
		}
		if !c.advance(rel, err) {
			return "", err
		}
	}
}

// getPublicURL 是 get 的一次请求：只认 200，正文限长。
func getPublicURL(client *http.Client, url, rel string, header func(string, http.Header), maxBytes int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if header != nil {
		header(rel, req.Header)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", rel, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, errors.New("下载内容超过大小限制")
	}
	return body, err
}

// officialHTTPClient 是直连官方地址的客户端：系统信任链、环境代理、短连接与
// 首字节时限（官方地址不可达时要尽快让链前进到设备），正文时限由调用方按
// 制品体量给。redirect 为 nil 时一律不跟 3xx，同固件各 helper 的取舍。
func officialHTTPClient(timeout time.Duration, redirect func(req *http.Request, via []*http.Request) bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if redirect != nil && redirect(req, via) {
				return nil
			}
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: officialConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   officialConnectTimeout,
			ResponseHeaderTimeout: officialHeaderTimeout,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// underBase 把 rel 挂到 base 下；base 为空表示该官方来源被禁用。
func underBase(base string) func(string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	return func(rel string) string { return base + "/" + rel }
}

// ---------- 各工具的官方来源（与固件 internal/*helper 的 open() 逐条对应） ----------

// codexOrigins：channels/latest 与 releases/<v>/<asset> 都在 releases.openai.com/codex
// 下；gate 不取官方安装脚本，chatgpt.com 那条映射不需要。
func (a *app) codexOrigins() *originChain {
	return a.newOriginChain("Codex", codexCLIBasePath, a.codexHTTPClient(), artifactOrigin{
		label:  "官方源",
		url:    underBase(codexOfficialReleasesBase),
		client: officialHTTPClient(codexCLITimeout, nil),
	})
}

// grokOrigins：官方安装脚本的两级来源——x.ai 在前，GCS 回落在后——再到设备。
func (a *app) grokOrigins() *originChain {
	client := officialHTTPClient(grokCLITimeout, nil)
	return a.newOriginChain("Grok", grokCLIBasePath, a.grokHTTPClient(),
		artifactOrigin{label: "官方源", url: underBase(grokOfficialPrimaryBase), client: client},
		artifactOrigin{label: "官方回落源", url: underBase(grokOfficialFallbackBase), client: client},
	)
}

func (a *app) claudeOrigins() *originChain {
	return a.newOriginChain("Claude Code", claudeCLIBasePath, a.claudeHTTPClient(), artifactOrigin{
		label:  "官方源",
		url:    underBase(claudeOfficialReleasesBase),
		client: officialHTTPClient(claudeCLITimeout, nil),
	})
}

// cursorOrigins：install.sh 是 cursor.com/install 这一个文件（PowerShell 版加
// ?win32=true，gate 不用）；lab/<版本>/<os>/<arch>/<归档> 挂在 downloads.cursor.com/lab。
func (a *app) cursorOrigins() *originChain {
	installer := strings.TrimSpace(cursorOfficialInstallerURL)
	lab := underBase(cursorOfficialLabBase)
	var url func(string) string
	if installer != "" || lab != nil {
		url = func(rel string) string {
			switch {
			case rel == "install.sh":
				return installer
			case rel == "install.ps1":
				if installer == "" {
					return ""
				}
				return installer + "?win32=true"
			case lab != nil && strings.HasPrefix(rel, "lab/"):
				return lab(strings.TrimPrefix(rel, "lab/"))
			}
			return ""
		}
	}
	return a.newOriginChain("Cursor CLI", cursorCLIBasePath, a.cursorHTTPClient(), artifactOrigin{
		label:  "官方源",
		url:    url,
		client: officialHTTPClient(cursorCLITimeout, nil),
	})
}

// openCodeOrigins：latest 走 GitHub REST API（带固定 Accept 与版本头），归档走
// github.com/…/releases/download/v<版本>/<归档>，它会 302 到 githubusercontent.com
// 的制品域，只跟这一跳。
func (a *app) openCodeOrigins() *originChain {
	api := underBase(openCodeOfficialAPIBase)
	releases := underBase(openCodeOfficialReleasesBase)
	var url func(string) string
	if api != nil || releases != nil {
		url = func(rel string) string {
			if rel == "latest" {
				if api == nil {
					return ""
				}
				return api("latest")
			}
			parts := strings.Split(rel, "/")
			if releases == nil || len(parts) != 3 || parts[0] != "releases" {
				return ""
			}
			return releases("v" + parts[1] + "/" + parts[2])
		}
	}
	return a.newOriginChain("OpenCode", openCodeCLIBasePath, a.openCodeHTTPClient(), artifactOrigin{
		label: "官方源",
		url:   url,
		header: func(rel string, h http.Header) {
			if rel == "latest" {
				h.Set("Accept", "application/vnd.github+json")
				h.Set("X-GitHub-Api-Version", "2022-11-28")
			}
		},
		client: officialHTTPClient(openCodeCLITimeout, openCodeOfficialRedirect),
	})
}

// openCodeOfficialRedirect 只放行 GitHub release 附件到制品域的那一跳。
func openCodeOfficialRedirect(req *http.Request, via []*http.Request) bool {
	if req == nil || len(via) != 1 || via[0] == nil || req.URL == nil || via[0].URL == nil {
		return false
	}
	from, to := via[0].URL, req.URL
	return from.Scheme == "https" && from.Host == "github.com" &&
		strings.HasPrefix(from.Path, "/anomalyco/opencode/releases/download/v") &&
		to.Scheme == "https" && strings.HasSuffix(to.Host, ".githubusercontent.com")
}
