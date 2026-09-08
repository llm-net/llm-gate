package mihomo

// 受限内核配置：由固件按节点列表生成完整 YAML，不接受任何管理员或订阅给的顶层键。
// 只开 127.0.0.1 上的 SOCKS 端口；无 LAN 入站、TUN/redir/tproxy、DNS 劫持、external
// controller、脚本或文件引用；规则只有一条 MATCH，选路交给 select 组：管理员选定节点时它
// 排在第一位（select 组缺省取第一个），否则第一位是 url-test 自动选择组。

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	// GroupSelect / GroupAuto 是本包生成的两个策略组名。
	GroupSelect = "LLM Gate"
	GroupAuto   = "自动选择"
	// autoTestURL 是 url-test 组的延迟测试地址（Clash 生态缺省，返回 204、无正文）。
	autoTestURL = "https://www.gstatic.com/generate_204"
)

// ConfigOptions 是生成配置的输入。
type ConfigOptions struct {
	Port int
	// Nodes 是放行的节点；Selected 是管理员选定的节点名（空 = 自动选择）。
	Nodes    []Node
	Selected string
}

// BuildConfig 生成内核配置 YAML。
func BuildConfig(o ConfigOptions) ([]byte, error) {
	if o.Port < 1 || o.Port > 65535 {
		return nil, errors.New("SOCKS 端口不合法")
	}
	if len(o.Nodes) == 0 {
		return nil, errors.New("没有可用节点")
	}
	names := make([]string, 0, len(o.Nodes))
	proxies := make([]map[string]any, 0, len(o.Nodes))
	seen := map[string]bool{}
	for _, n := range o.Nodes {
		name := n.Name
		// 节点名不能与策略组重名。
		for name == GroupSelect || name == GroupAuto || seen[name] {
			name += " ·"
		}
		seen[name] = true
		raw := make(map[string]any, len(n.Raw))
		for k, v := range n.Raw {
			raw[k] = v
		}
		raw["name"] = name
		names = append(names, name)
		proxies = append(proxies, raw)
	}
	selectList := make([]string, 0, len(names)+1)
	if o.Selected != "" && seen[o.Selected] {
		selectList = append(selectList, o.Selected, GroupAuto)
		for _, n := range names {
			if n != o.Selected {
				selectList = append(selectList, n)
			}
		}
	} else {
		selectList = append(selectList, GroupAuto)
		selectList = append(selectList, names...)
	}
	doc := configDoc{
		SocksPort:   o.Port,
		BindAddress: "127.0.0.1",
		AllowLAN:    false,
		Mode:        "rule",
		LogLevel:    "warning",
		IPv6:        true,
		FindProcess: "off",
		Profile:     profileDoc{StoreSelected: false, StoreFakeIP: false},
		Proxies:     proxies,
		ProxyGroups: []map[string]any{
			{"name": GroupSelect, "type": "select", "proxies": selectList},
			{"name": GroupAuto, "type": "url-test", "url": autoTestURL, "interval": 300, "tolerance": 50, "proxies": names},
		},
		Rules: []string{"MATCH," + GroupSelect},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("生成内核配置失败: %w", err)
	}
	return out, nil
}

type configDoc struct {
	SocksPort   int              `yaml:"socks-port"`
	BindAddress string           `yaml:"bind-address"`
	AllowLAN    bool             `yaml:"allow-lan"`
	Mode        string           `yaml:"mode"`
	LogLevel    string           `yaml:"log-level"`
	IPv6        bool             `yaml:"ipv6"`
	FindProcess string           `yaml:"find-process-mode"`
	Profile     profileDoc       `yaml:"profile"`
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []map[string]any `yaml:"proxy-groups"`
	Rules       []string         `yaml:"rules"`
}

type profileDoc struct {
	StoreSelected bool `yaml:"store-selected"`
	StoreFakeIP   bool `yaml:"store-fake-ip"`
}
