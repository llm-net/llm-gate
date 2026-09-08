// Package config 加载并校验 gatewayd 的 YAML 配置。
//
// upstreams / logical_models / api_keys 三段都已降级为**启动导入表**
// （iteration-4 api_keys、iteration-5 前两段）：整段可选，启动时幂等导入
// SQLite，此后 SQLite 是唯一事实源，正式管理经管理界面。YAML 再编辑只影响
// 尚未导入的新条目——已存在的行不会被 YAML 覆盖，也不会被 YAML 复活。
// 本包只做条目形态校验（枚举、必填、引用完整性），语义归属由导入器裁决
// （见 internal/gateway/bootstrap.go）。
//
// 校验错误面向管理员：可读、指明字段，且绝不包含敏感值——凡需在错误信息里
// 指认某个 Key，一律经 logging.RedactKey。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
)

// 缺省值。listen 默认仅回环；板上部署显式改为内网地址。
const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultDataDir  = "./data" // 板上部署约定 /var/lib/llmgate，由部署配置显式给出
	DefaultLogLevel = "info"
)

// binding 协议：决定客户端入口与上游端点——openai_chat ↔ POST /v1/chat/completions，
// anthropic_messages ↔ POST /v1/messages（含 count_tokens）。入口与协议不匹配时
// 网关明确拒绝，不做跨协议转换（MVP §6）。
//
// 视频 / 图像等厂商协议面：每个协议面就是一家源头厂商的一种官方接口，设备按
// 官方路径与报文**纯转发**，不设归一信封——所以协议标识按「厂商_模态」命名且
// **客户端可见**（客户端必须按该厂商的官方文档构造请求）。客户端路径 =
// `/<厂商段>` + 厂商站点根之后的原样那一段：设备只有一个主机名，厂商面按路径
// 首段分开（ProtocolFace*，gateway/server.go 的路由块），首段之后方法、路径尾段、
// 查询串、报文与任务 id 与厂商官方文档逐字一致；设备→厂商那一跳不带首段。
// 当前三个：
//
//	minimax_video  MiniMax 视频（/minimax 段）：POST /minimax/v2/video_generation、
//	               GET /minimax/v2/query/video_generation/{id}、DELETE 同路径、
//	               POST /minimax/v2/h3_context_ir
//	ark_video      火山方舟 视频（/ark 段，Seedance）：POST /ark/api/v3/contents/
//	               generations/tasks、GET/DELETE 同路径 /{id}
//	ark_image      火山方舟 图像（/ark 段，Seedream）：POST /ark/api/v3/images/generations
//
// 把厂商 SDK 的 base_url 换成「设备地址 + /<厂商段>」就能直接用。
// 一个 AIGC 模型的全部来源必须同一协议面（管理 API 写入时校验）；文本入口
// 使用 OpenAI Chat、OpenAI Responses、Anthropic Messages 三个协议面，互不越界。
const (
	ProtocolOpenAIChat        = "openai_chat"
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolArkVideo          = "ark_video"
	ProtocolArkImage          = "ark_image"
	ProtocolMinimaxVideo      = "minimax_video"
)

// 厂商协议面的路径首段（段名即厂商 slug）。数据面把 `/<段>/` 整棵子树交给该
// 厂商的路由块，网关自产错误按首段选该厂商的错误形（gateway.entryErrorStyle）。
const (
	ProtocolFaceArk     = "ark"
	ProtocolFaceMinimax = "minimax"
)

// ProtocolFace 返回 AIGC 协议标识所属的厂商路径首段；文本协议与未知值返回空串。
func ProtocolFace(protocol string) string {
	switch protocol {
	case ProtocolArkVideo, ProtocolArkImage:
		return ProtocolFaceArk
	case ProtocolMinimaxVideo:
		return ProtocolFaceMinimax
	}
	return ""
}

// 上游类型。deepseek/ark/ark_plan/qwen_plan/opencode_go/minimax 走内置 (type, 协议) 二维
// 端点表（internal/upstream）；mock 指向本地 mock 上游，必须显式给 base_url。
// ark（方舟按量）与 ark_plan（方舟 Agent Plan 套餐）的 Key 双向隔离
// （iteration-2 实测），是两种独立的上游类型。qwen_plan（2026-08-09）是阿里云
// 百炼的通义千问 Token Plan 订阅套餐，与 ark_plan 同为「预付额度先用满」的
// 订阅型，文本双入口（两个协议的端点根不同段，见内置端点表）。opencode_go 是
// OpenCode Go 包月套餐（opencode.ai/zen/go）：同一把 sk- Key、Chat 与 Messages 同一
// 端点根，与 ark_plan / qwen_plan 同为订阅型；Zen 按量余额是另一套权益，不在此类型。minimax
// （迭代 8）只服务 minimax_video；其国内/国际双站点经受限的 base_url 选择，
// 见 internal/upstream 的 MinimaxSite* 常量。openai_compat 是通用 OpenAI 兼容
// 适配：无内置端点，必须显式给 base_url 与 api_key，只服务 openai_chat（泛型行
// 说不清自己「该被当哪家厂商适配」，所以 anthropic 与方舟 / MiniMax 的厂商
// 协议面都不在列）——特化平台（余额查询等平台能力）之外的兜底接入，永远排在
// 类型清单末尾。
const (
	UpstreamDeepseek        = "deepseek"
	UpstreamArk             = "ark"
	UpstreamArkPlan         = "ark_plan"
	UpstreamQwenPlan        = "qwen_plan"
	UpstreamOpenCodeGo      = "opencode_go"
	UpstreamMinimax         = "minimax"
	UpstreamOpenAICompat    = "openai_compat"
	UpstreamAnthropicCompat = "anthropic_compat"
	UpstreamMock            = "mock"
)

// 上游 HTTP 客户端分层超时缺省值（internal/upstream 消费）。
// response_header 须覆盖非流式 chat 的上游整个生成期（上游生成完才回头部）；
// overall 是单次请求整体硬上限，SSE 长流同样受它约束。
const (
	DefaultConnectTimeout        = 10 * time.Second
	DefaultTLSHandshakeTimeout   = 10 * time.Second
	DefaultResponseHeaderTimeout = 5 * time.Minute
	DefaultOverallTimeout        = 10 * time.Minute
)

// 设备状态历史保留天数（history_days）的缺省与上限。
const (
	DefaultHistoryDays = 7
	MaxHistoryDays     = 60
)

// 用量小时聚合（usage_hourly）的保留天数（usage_days）的缺省、上下限。
//
// 与 history_days 有两处**刻意的不同**，都不是笔误：
//
//   - **没有 0 这个关闭档**。计量不是可选的监控功能，而是预算准入、用量页与
//     未定价警示的共同地基；给它一个总开关等于给自己一把脚枪。
//   - **下限是 31 天而不是 1**。预算窗口含「本地自然月」，gatewayd 启动时
//     从 usage_hourly 播种当月已用额；保留期短于一个最长自然月，播出来的
//     月度消费就是残缺的——月预算会在每次重启后**缩水**（表现为「本该拦住的
//     请求重启后又放行了」），而且只会变松不会变紧，现场极难察觉。
const (
	DefaultUsageDays = 90
	MinUsageDays     = 31
	MaxUsageDays     = 366
)

// DefaultOfficialSiteBaseURL 是设备匿名读取公开升级文件与推荐应用页的官网根地址。
// 生产站点的公开升级面是 Cloudflare 静态部署；本字段只为本地联调与镜像站保留覆盖能力。
const DefaultOfficialSiteBaseURL = "https://llm.net"

// Config 是 gatewayd 的顶层配置。
type Config struct {
	// Listen 是明文监听地址：数据面 /v1/* 与管理控制面 /admin/*（含管理
	// 界面）同端口，按路径前缀分域（2026-08-06 决策：单端口取代 §13.10 的
	// 独立管理监听器；认证中间件仍相互独立）。可选的 TLS 监听与它共享同一个
	// handler，不会形成第二套管理面。
	Listen   string `yaml:"listen"`
	DataDir  string `yaml:"data_dir"`
	LogLevel string `yaml:"log_level"`
	// TLS 是客户自管的本地 HTTPS 入口。整段缺省时不开 HTTPS；启用时监听地址、
	// 证书与私钥文件必须一次配齐。私钥只由 net/tls 在本机读取，不经管理 API、
	// 日志或官网。
	TLS TLS `yaml:"tls"`
	// HistoryDays 是「设备状态」历史的保留天数（internal/sysinfo.Recorder）：
	// 缺省 DefaultHistoryDays；0 关闭整个后台采样与归档；上限
	// MaxHistoryDays（日文件很小，上限防的是配置手滑）。
	HistoryDays *int `yaml:"history_days"`
	// UsageDays 是用量小时聚合的保留天数（internal/usage.Meter）：缺省
	// DefaultUsageDays，取值须在 MinUsageDays–MaxUsageDays 之间。**没有
	// 「0 = 关闭」这一档**，下限也不是 1——理由见常量组的注释。
	UsageDays *int `yaml:"usage_days"`
	// UpdateSocket 是升级引擎（llmgate updated）的 UDS 地址；留空取内置缺省
	// /run/llmgate-updated/updated.sock（updated.DefaultSocket——缺省值在那边，
	// 本包不 import 它）。只有 dev 联调需要覆盖；socket 不可达时管理台的
	// 安装/回退如实降级（docs/firmware-update.md）。
	UpdateSocket string `yaml:"update_socket"`

	// OfficialSite 是 LLM Gate官网的只读入口。整段可选；设备不携带身份或
	// 凭据，只匿名读取数据升级、固件升级和推荐应用静态文件。
	OfficialSite OfficialSite `yaml:"official_site"`

	// Upstreams / LogicalModels / APIKeys 都是可选的启动导入表（见包文档）。
	Upstreams        []Upstream         `yaml:"upstreams"`
	UpstreamTimeouts UpstreamTimeouts   `yaml:"upstream_timeouts"`
	LogicalModels    map[string]Binding `yaml:"logical_models"`
	APIKeys          []APIKey           `yaml:"api_keys"`
}

// TLS 是本地静态证书监听配置：客户可用自己的域名与证书，或在网络边界以 HTTPS
// 直通/回源本监听。它与管理台「内网域名」（internal/landomain：经 LLM Gate官网申领
// <label>.llm.net 或登记自有域名并自动签发证书，监听由 settings 决定）是两条独立入口。
type TLS struct {
	Listen   string `yaml:"listen"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Enabled 报告 TLS 三元组是否已配置。配置加载路径会拒绝不完整三元组；这个
// 方法也供测试手工构造 Config 的装配路径使用。
func (t TLS) Enabled() bool {
	return t.Listen != "" && t.CertFile != "" && t.KeyFile != ""
}

// OfficialSite 是官网静态文件入口配置。
type OfficialSite struct {
	BaseURL string `yaml:"base_url"`
}

func (c OfficialSite) EffectiveBaseURL() string {
	if c.BaseURL == "" {
		return DefaultOfficialSiteBaseURL
	}
	return c.BaseURL
}

func (c OfficialSite) LogValue() slog.Value {
	return slog.GroupValue(slog.String("base_url", c.EffectiveBaseURL()))
}

// UpstreamTimeouts 是到上游 HTTP 客户端的分层超时；零值字段取包缺省
// （见 Default*Timeout）。各层的接线位置见 internal/upstream 包文档。
type UpstreamTimeouts struct {
	Connect        Duration `yaml:"connect"`         // TCP 建连
	TLSHandshake   Duration `yaml:"tls_handshake"`   // TLS 握手
	ResponseHeader Duration `yaml:"response_header"` // 发出请求到收到响应头
	Overall        Duration `yaml:"overall"`         // 单次请求整体上限（含响应体转发完毕）
}

// Normalized 返回补齐缺省值后的副本：未配置（零值）字段取包缺省。
// 绕过 Load 手工构造 Config 的路径（如测试）也应经由它取得可用超时。
func (t UpstreamTimeouts) Normalized() UpstreamTimeouts {
	if t.Connect <= 0 {
		t.Connect = Duration(DefaultConnectTimeout)
	}
	if t.TLSHandshake <= 0 {
		t.TLSHandshake = Duration(DefaultTLSHandshakeTimeout)
	}
	if t.ResponseHeader <= 0 {
		t.ResponseHeader = Duration(DefaultResponseHeaderTimeout)
	}
	if t.Overall <= 0 {
		t.Overall = Duration(DefaultOverallTimeout)
	}
	return t
}

// HistoryDaysOrDefault 返回设备状态历史的保留天数：未配置取缺省，0 表示
// 关闭。绕过 Load 手工构造 Config 的路径（测试）同样经它取值。
func (c *Config) HistoryDaysOrDefault() int {
	if c.HistoryDays == nil {
		return DefaultHistoryDays
	}
	return *c.HistoryDays
}

// UsageDaysOrDefault 返回用量聚合的保留天数：未配置取缺省。绕过 Load 手工
// 构造 Config 的路径（测试）同样经它取值——那条路径没经过 validate，所以
// 这里对越界值再兜一次底，绝不把小于下限的保留期交给 Meter（那会让月预算
// 播种残缺，见常量组注释）。
func (c *Config) UsageDaysOrDefault() int {
	if c.UsageDays == nil || *c.UsageDays < MinUsageDays {
		return DefaultUsageDays
	}
	if *c.UsageDays > MaxUsageDays {
		return MaxUsageDays
	}
	return *c.UsageDays
}

// Duration 是 YAML 配置里的时长字段，取 Go time.ParseDuration 语法的
// 字符串（如 "10s"、"5m"），必须大于 0。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。错误文本不回显原始值——
// 与本包其余校验错误一致，靠 yaml 附带的行号定位问题字段。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return errors.New(`时长须为字符串，Go 时长语法（如 "10s"、"5m"）`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return errors.New(`时长格式非法（Go 时长语法，如 "10s"、"5m"）`)
	}
	if v <= 0 {
		return errors.New("时长必须大于 0")
	}
	*d = Duration(v)
	return nil
}

// Upstream 是上游账户导入表的一项：启动时按 name 幂等导入 SQLite——
// 同名上游已存在即整条跳过（改 Key/base_url、启停只经管理界面，YAML 再编辑
// 不生效）。api_key 明文经设备密钥封存后入库（架构 §12）。整段可选。
type Upstream struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"` // deepseek | ark | ark_plan | qwen_plan | opencode_go | minimax | openai_compat | mock
	APIKey string `yaml:"api_key"`
	// BaseURL 仅 dev 覆盖用（如指向 mock 上游）；产品默认走内置端点表。
	BaseURL string `yaml:"base_url"`
}

// Binding 是模型目录导入表的一项，键为历史遗留的逻辑名。导入时自动转换为
// 「原始名模型 + 来源」：模型名取 UpstreamModelID（不再是键名），同名跨上游
// 合并为多来源，(模型, 上游) 已存在即跳过。Protocol 仍受校验但导入器忽略
// ——协议由来源上游的 type 在运行时决定（iteration-5 决策 4/6）。整段可选。
type Binding struct {
	Upstream        string `yaml:"upstream"`
	UpstreamModelID string `yaml:"upstream_model_id"`
	Protocol        string `yaml:"protocol"` // openai_chat（缺省）| anthropic_messages；导入时忽略
}

// APIKey 是客户端 Key 过渡导入表的一项：启动时按摘要幂等导入 SQLite
// （已存在摘要整条跳过，禁用状态不被复活），此后 SQLite 是唯一事实源；
// 正式 Key 经管理界面签发。整段可选。
//
// 设备没有「用户」这个概念（0019 起），所以条目只有 key 一个字段。**升级
// 上来的配置文件必须删掉旧的 `user:` 行**：解码开着 KnownFields，多余的键
// 会让 gatewayd 启动即退出——宁可在启动时说清楚，也不静默忽略一个作者以为
// 还在起作用的字段。
type APIKey struct {
	Key string `yaml:"key"`
}

// Load 读取并校验 path 处的 YAML 配置。任何错误都应让 gatewayd 启动即退出；
// 返回的错误信息可直接展示给管理员，不含敏感值。
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("未指定配置文件，用法：llmgate gatewayd --config <path>")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // 未知字段视为错误，尽早暴露拼写问题
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("配置文件 %s 为空", path)
		}
		// yaml 类型错误会把文件中的标量值原样带进错误文本（api_key 等敏感值
		// 可能在内），必须脱敏后返回，且不 %w 包装原错误以免明文经 Unwrap 泄出。
		return nil, fmt.Errorf("解析配置文件 %s: %s", path, sanitizeYAMLError(err))
	}
	// 单文档约束：出现 --- 分隔的多文档时，后续文档会被静默丢弃，
	// 对声明「哪些 Key 有效」的安全配置不可接受，直接报错。
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("配置文件 %s 含多个 YAML 文档（--- 分隔），仅支持单文档", path)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	// 未配置的超时字段补缺省；已配置字段由 Duration.UnmarshalYAML 保证 > 0。
	c.UpstreamTimeouts = c.UpstreamTimeouts.Normalized()
	for name, b := range c.LogicalModels {
		if b.Protocol == "" {
			b.Protocol = ProtocolOpenAIChat
			c.LogicalModels[name] = b
		}
	}
}

// yamlQuoted 匹配 yaml 错误文本里反引号包裹的片段——yaml.v3 的类型错误
// 用它回显文件中的原始值。
var yamlQuoted = regexp.MustCompile("`[^`]*`")

// sanitizeYAMLError 把 yaml 解析/类型错误变为可安全展示的文本：
// 隐去所有回显的原始值，保留行号与类型信息供管理员定位。
func sanitizeYAMLError(err error) string {
	return yamlQuoted.ReplaceAllString(err.Error(), "[值已隐去]")
}

func (c *Config) validate() error {
	if err := validateListenAddr("listen", c.Listen); err != nil {
		return err
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	if _, err := logging.ParseLevel(c.LogLevel); err != nil {
		return err
	}
	if c.HistoryDays != nil && (*c.HistoryDays < 0 || *c.HistoryDays > MaxHistoryDays) {
		return fmt.Errorf("history_days 须在 0–%d 之间（0 = 关闭历史记录）", MaxHistoryDays)
	}
	if c.UsageDays != nil && (*c.UsageDays < MinUsageDays || *c.UsageDays > MaxUsageDays) {
		return fmt.Errorf("usage_days 须在 %d–%d 之间（计量没有关闭档；下限护住「本地自然月」预算窗口的播种）",
			MinUsageDays, MaxUsageDays)
	}

	if err := c.validateOfficialSite(); err != nil {
		return err
	}

	names, err := c.validateUpstreams()
	if err != nil {
		return err
	}
	if err := c.validateLogicalModels(names); err != nil {
		return err
	}
	return c.validateAPIKeys()
}

func (c *Config) validateTLS() error {
	t := c.TLS
	configured := t.Listen != "" || t.CertFile != "" || t.KeyFile != ""
	if !configured {
		return nil
	}
	if t.Listen == "" || t.CertFile == "" || t.KeyFile == "" {
		return errors.New("tls.listen、tls.cert_file 与 tls.key_file 必须同时配置")
	}
	if err := validateListenAddr("tls.listen", t.Listen); err != nil {
		return err
	}
	if t.Listen == c.Listen {
		return errors.New("tls.listen 不能与明文 listen 相同")
	}
	return nil
}

// validateListenAddr 校验监听地址为合法 host:port 且端口在 1–65535。
func validateListenAddr(field, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q 不是合法的 host:port", field, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s %q 端口非法（需 1–65535）", field, addr)
	}
	return nil
}

func (c *Config) validateOfficialSite() error {
	baseURL := c.OfficialSite.BaseURL
	if baseURL == "" {
		return nil
	}
	p, err := url.Parse(baseURL)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil ||
		p.Opaque != "" || (p.Path != "" && p.Path != "/") || p.RawPath != "" || p.RawQuery != "" ||
		p.ForceQuery || p.Fragment != "" {
		return fmt.Errorf("official_site.base_url %q 不是合法的 http(s) URL", baseURL)
	}
	return nil
}

// validateUpstreams 校验上游导入表的条目形态并返回其 name 集合
// （validateLogicalModels 据此校验 binding 的引用完整性）。整段可选：
// 缺省即不导入，上游全部在管理界面维护。
func (c *Config) validateUpstreams() (map[string]struct{}, error) {
	names := make(map[string]struct{}, len(c.Upstreams))
	for i, u := range c.Upstreams {
		if u.Name == "" {
			return nil, fmt.Errorf("upstreams[%d]: name 不能为空", i)
		}
		if _, dup := names[u.Name]; dup {
			return nil, fmt.Errorf("upstream name %q 重复", u.Name)
		}
		names[u.Name] = struct{}{}

		switch u.Type {
		case UpstreamDeepseek, UpstreamArk, UpstreamArkPlan, UpstreamQwenPlan, UpstreamOpenCodeGo, UpstreamMinimax:
			if u.APIKey == "" {
				return nil, fmt.Errorf("upstream %q: type %s 必须配置 api_key（真实 Key 写入 gitignored 的 *.local.yaml）", u.Name, u.Type)
			}
		case UpstreamOpenAICompat, UpstreamAnthropicCompat:
			// 通用适配两样都要：地址不内置，凭证也照常要发（无 Key 的兼容
			// 服务在 Key 里填任意占位串即可，形态校验只在管理 API 侧）。
			if u.APIKey == "" {
				return nil, fmt.Errorf("upstream %q: type %s 必须配置 api_key（真实 Key 写入 gitignored 的 *.local.yaml）", u.Name, u.Type)
			}
			if u.BaseURL == "" {
				return nil, fmt.Errorf("upstream %q: type %s 必须配置 base_url（兼容服务的端点根，通常以 /v1 结尾）", u.Name, u.Type)
			}
		case UpstreamMock:
			if u.BaseURL == "" {
				return nil, fmt.Errorf("upstream %q: type mock 必须配置 base_url（指向本地 mock 上游）", u.Name)
			}
		default:
			return nil, fmt.Errorf("upstream %q: 未知 type %q（可选 %s|%s|%s|%s|%s|%s|%s|%s|%s）", u.Name, u.Type, UpstreamDeepseek, UpstreamArk, UpstreamArkPlan, UpstreamQwenPlan, UpstreamOpenCodeGo, UpstreamMinimax, UpstreamOpenAICompat, UpstreamAnthropicCompat, UpstreamMock)
		}

		if u.BaseURL != "" {
			p, err := url.Parse(u.BaseURL)
			if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
				return nil, fmt.Errorf("upstream %q: base_url %q 不是合法的 http(s) URL", u.Name, u.BaseURL)
			}
		}
	}
	return names, nil
}

// validateLogicalModels 校验模型导入表的条目形态。整段可选：缺省即不导入，
// 模型与来源全部在管理界面维护。protocol 仍校验取值（历史字段，写错了要报
// 出来），但导入器忽略它——运行时按来源上游的 type 决定可服务的入口协议
// （iteration-5 决策 4），故不再有 (ark, anthropic_messages) 的交叉校验。
func (c *Config) validateLogicalModels(upstreams map[string]struct{}) error {
	// 排序遍历，保证多处错误时报出的那一条确定。
	names := make([]string, 0, len(c.LogicalModels))
	for name := range c.LogicalModels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := c.LogicalModels[name]
		if name == "" {
			return errors.New("logical_models: 模型名不能为空")
		}
		if b.Upstream == "" {
			return fmt.Errorf("logical_models.%s: upstream 不能为空", name)
		}
		if _, defined := upstreams[b.Upstream]; !defined {
			return fmt.Errorf("logical_models.%s: upstream %q 未在 upstreams 中定义", name, b.Upstream)
		}
		if b.UpstreamModelID == "" {
			return fmt.Errorf("logical_models.%s: upstream_model_id 不能为空", name)
		}
		switch b.Protocol {
		case ProtocolOpenAIChat, ProtocolAnthropicMessages:
		default:
			return fmt.Errorf("logical_models.%s: protocol %q 不支持（可选 %s|%s）", name, b.Protocol, ProtocolOpenAIChat, ProtocolAnthropicMessages)
		}
	}
	return nil
}

// validateAPIKeys 校验过渡导入表的条目形态。段本身可选（iteration-4 起
// 客户端 Key 存 SQLite、经管理界面签发，缺省即不导入）；给出的条目仍须
// key 非空且不重复。
func (c *Config) validateAPIKeys() error {
	seen := make(map[string]int, len(c.APIKeys))
	for i, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d]: key 不能为空", i)
		}
		if j, dup := seen[k.Key]; dup {
			return fmt.Errorf("api_keys[%d] 与 api_keys[%d] 的 key 重复（%s）", i, j, logging.RedactKey(k.Key))
		}
		seen[k.Key] = i
	}
	return nil
}
