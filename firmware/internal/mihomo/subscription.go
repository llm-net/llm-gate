package mihomo

// Clash 订阅：管理员填一个 https:// 订阅地址（机场给的那条），固件按 Clash 内核的习惯
// 带 clash 系 User-Agent 拉取，只认 YAML 顶层 `proxies` 里的节点条目——机场下发的
// proxy-groups / rules / dns / tun 一概不用（本包生成自己的受限配置）。节点条目原样交给内核
// （它们是机场数据，字段随协议不同），只剔除会碰系统网络栈的键并做形态检查。
//
// 订阅 URL 与节点条目是凭据（node 的 password / uuid …）：URL 密封存 settings，节点落在
// 数据目录 0600 文件，两者都不进日志、审计与 API 响应；API 只回节点名与类型。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// subscriptionCap 是订阅正文上限（几百个节点的完整 Clash 配置通常 < 1 MiB）。
	subscriptionCap = 8 << 20
	// maxNodes 是保留的节点数上限。
	maxNodes         = 500
	maxNodeNameRunes = 96
)

// supportedTypes 是本包放行的出站协议（Mihomo 1.19 支持且不需要额外权限的）。
var supportedTypes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "tuic": true, "wireguard": true, "anytls": true,
	"snell": true, "mieru": true, "ssh": true, "socks5": true, "http": true,
}

// strippedKeys 是节点条目里不放行的键：绑定网卡与路由标记会碰系统网络栈（且内核没有
// CAP_NET_ADMIN，只会失败）。
var strippedKeys = []string{"interface-name", "routing-mark"}

// Node 是一个放行的节点：Name / Type 可以进 API 与日志，Raw 是交给内核的完整条目（含凭据）。
type Node struct {
	Name string
	Type string
	Raw  map[string]any
}

// Parsed 是一次订阅解析的结果。
type Parsed struct {
	Nodes []Node
	// Dropped 是被剔除的条目数（协议不支持、形态不对、超过上限），DroppedTypes 按协议计数。
	Dropped      int
	DroppedTypes map[string]int
}

// ErrNotClashConfig 表示订阅正文不是 Clash 配置（没有 proxies）。
var ErrNotClashConfig = errors.New("订阅内容不是 Clash 配置：没有 proxies 节点列表（请确认这是 Clash / Mihomo 格式的订阅地址）")

// ParseSubscription 解析订阅正文。
func ParseSubscription(raw []byte) (*Parsed, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, ErrNotClashConfig
	}
	if len(doc.Proxies) == 0 {
		return nil, ErrNotClashConfig
	}
	out := &Parsed{DroppedTypes: map[string]int{}}
	names := map[string]int{}
	for _, entry := range doc.Proxies {
		typ, _ := entry["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		if !supportedTypes[typ] {
			out.Dropped++
			if typ == "" {
				typ = "?"
			}
			out.DroppedTypes[typ]++
			continue
		}
		name := cleanName(entry["name"])
		if name == "" || !validServer(entry["server"]) || !validPort(entry["port"]) {
			out.Dropped++
			out.DroppedTypes[typ]++
			continue
		}
		if len(out.Nodes) >= maxNodes {
			out.Dropped++
			out.DroppedTypes[typ]++
			continue
		}
		raw := make(map[string]any, len(entry))
		for k, v := range entry {
			raw[k] = v
		}
		for _, k := range strippedKeys {
			delete(raw, k)
		}
		raw["type"] = typ
		// 重名节点加序号：内核要求节点名唯一，选择器也按名字找。
		if n := names[name]; n > 0 {
			name = fmt.Sprintf("%s #%d", name, n+1)
		}
		names[cleanName(entry["name"])]++
		raw["name"] = name
		out.Nodes = append(out.Nodes, Node{Name: name, Type: typ, Raw: raw})
	}
	if len(out.Nodes) == 0 {
		return nil, fmt.Errorf("订阅里没有可用节点（%d 条被剔除：%s）", out.Dropped, describeDropped(out.DroppedTypes))
	}
	return out, nil
}

func describeDropped(m map[string]int) string {
	if len(m) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func cleanName(v any) string {
	s, _ := v.(string)
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if runes := []rune(s); len(runes) > maxNodeNameRunes {
		s = string(runes[:maxNodeNameRunes])
	}
	return s
}

func validServer(v any) bool {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 253 || strings.ContainsAny(s, " /\\@?#") {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return true
}

func validPort(v any) bool {
	_, ok := portOf(v)
	return ok
}

// portOf 从节点条目的 port 值取端口号（YAML 解析后可能是数字或字符串）。
func portOf(v any) (int, bool) {
	var port int
	switch p := v.(type) {
	case int:
		port = p
	case int64:
		port = int(p)
	case float64:
		port = int(p)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0, false
		}
		port = n
	default:
		return 0, false
	}
	if port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// ValidateSubscriptionURL 校验订阅地址：https、有主机名、无 userinfo。查询串允许（机场常用
// token 参数）。
func ValidateSubscriptionURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("订阅地址不能为空")
	}
	if len(raw) > 2048 {
		return "", errors.New("订阅地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("订阅地址不是合法的网址")
	}
	if u.Scheme != "https" {
		return "", errors.New("订阅地址必须是 https://（http 会让订阅令牌明文经过网络）")
	}
	if u.Hostname() == "" {
		return "", errors.New("订阅地址缺少主机名")
	}
	if u.User != nil {
		return "", errors.New("订阅地址不能包含用户名或口令")
	}
	if u.Fragment != "" {
		u.Fragment = ""
	}
	return u.String(), nil
}

// userAgent 是拉取订阅时的 UA：机场按它决定下发 Clash YAML 还是其他格式。
const userAgent = "clash.meta/1.19.30 (llmgate)"

// UserInfo 是机场在 subscription-userinfo 头里给的用量（字节 / 到期 Unix 秒）；不是凭据。
type UserInfo struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
}

// FetchResult 是一次拉取的结果。
type FetchResult struct {
	Parsed   *Parsed
	Raw      []byte
	UserInfo *UserInfo
}

// fetchSubscription 拉取并解析订阅。hc 由装配方给（出站策略 proxy_subscription 分类）。
// 错误文本不含订阅地址。
func fetchSubscription(ctx context.Context, hc *http.Client, subURL string) (*FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subURL, nil)
	if err != nil {
		return nil, errors.New("构造订阅请求失败")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := hc.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, errors.New("拉取订阅超时")
		}
		return nil, errors.New("无法连接订阅服务器")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("订阅服务器返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionCap+1))
	if err != nil {
		return nil, errors.New("读取订阅内容失败")
	}
	if len(raw) > subscriptionCap {
		return nil, errors.New("订阅内容超过长度上限")
	}
	parsed, err := ParseSubscription(raw)
	if err != nil {
		return nil, err
	}
	return &FetchResult{Parsed: parsed, Raw: raw, UserInfo: parseUserInfo(resp.Header.Get("subscription-userinfo"))}, nil
}

// parseUserInfo 解析 `upload=…; download=…; total=…; expire=…`。
func parseUserInfo(h string) *UserInfo {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	info := &UserInfo{}
	found := false
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		found = true
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload = n
		case "download":
			info.Download = n
		case "total":
			info.Total = n
		case "expire":
			info.Expire = n
		}
	}
	if !found {
		return nil
	}
	return info
}

// marshalNodes 把节点列表编成落盘 / 生成配置用的 YAML（{proxies: [...]}）。
func marshalNodes(nodes []Node) ([]byte, error) {
	raws := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		raws = append(raws, n.Raw)
	}
	return yaml.Marshal(map[string]any{"proxies": raws})
}
