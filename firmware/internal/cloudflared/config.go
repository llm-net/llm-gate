package cloudflared

// 设置项与输入校验（docs-dev/firmware-cloudflare-tunnel.md §6.1）。运行时事实
// 存 SQLite settings，token 用现有 device-key 密封（AAD 钉在键名上）。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// settings 键名。
const (
	settingHostname         = "cloudflare.hostname"
	settingExposure         = "cloudflare.exposure"
	settingAutoUpdate       = "cloudflare.auto_update"
	settingEnabled          = "cloudflare.enabled"
	settingTokenSealed      = "cloudflare.token_sealed"
	settingManifestRevision = "cloudflare.manifest_revision"
	settingManifestSHA256   = "cloudflare.manifest_sha256"
)

// Config 是不含秘密的 Tunnel 配置。
type Config struct {
	Hostname   string            `json:"hostname"`
	Exposure   tunnelctx.Profile `json:"exposure"`
	AutoUpdate bool              `json:"auto_update"`
}

// policy 把配置折成 listener 策略快照。
func (c Config) policy() tunnelctx.Policy {
	return tunnelctx.Policy{Hostname: c.Hostname, Profile: c.Exposure}
}

// ExternalURL 是 Cloudflare 模式下对外公布的基址：恒为 https://<hostname>，
// 不允许 path、port、query、fragment 或 userinfo。
func (c Config) ExternalURL() string {
	if c.Hostname == "" {
		return ""
	}
	return "https://" + c.Hostname
}

// 输入校验的哨兵错误（管理面映射为 400 invalid_cloudflare_config）。
var ErrInvalidConfig = errors.New("cloudflare 配置无效")

func invalid(msg string) error { return fmt.Errorf("%w：%s", ErrInvalidConfig, msg) }

// reservedTLDs 是不可能属于用户 Cloudflare zone 的顶级域（RFC 6761/6762/7686/8375）。
var reservedTLDs = map[string]bool{
	"local": true, "localhost": true, "internal": true, "test": true, "example": true,
	"invalid": true, "onion": true, "arpa": true, "home": true, "lan": true, "corp": true,
}

// NormalizeHostname 校验并规范化 public hostname：小写 ASCII/Punycode FQDN，
// 拒绝 wildcard、IP、单 label、保留域、URL 字符、尾点与非 ASCII。
func NormalizeHostname(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" {
		return "", invalid("请填写 public hostname，例如 box.example.com")
	}
	if len(h) > 253 {
		return "", invalid("hostname 过长（上限 253 字符）")
	}
	if strings.ContainsAny(h, "/:@?#*[] \t\\") {
		return "", invalid("只填主机名，不要带协议、路径、端口、账号或通配符")
	}
	if strings.HasSuffix(h, ".") {
		return "", invalid("不要带尾点")
	}
	for _, c := range h {
		if c > 127 {
			return "", invalid("请使用 Punycode（xn--）形态的 ASCII 域名")
		}
	}
	if net.ParseIP(h) != nil {
		return "", invalid("不能是 IP 地址，必须是你 Cloudflare zone 下的域名")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", invalid("需要完整域名（至少两段，如 box.example.com）")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", invalid("域名段长度须为 1–63 字符")
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", invalid("域名段不能以连字符开头或结尾")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", invalid("域名只能包含字母、数字与连字符")
			}
		}
	}
	tld := labels[len(labels)-1]
	allDigits := true
	for _, c := range tld {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return "", invalid("顶级域不能全是数字")
	}
	if reservedTLDs[tld] {
		return "", invalid("." + tld + " 是保留域，不能作为公网 hostname")
	}
	return h, nil
}

// ErrInvalidToken 表示粘贴的不是 Cloudflare Tunnel token。
var ErrInvalidToken = errors.New("tunnel token 无效")

// maxTokenLen 是 token 的长度上限（实际约 200 字节）。
const maxTokenLen = 8192

// ValidateToken 校验 remotely-managed tunnel token 的形态：base64 编码的 JSON，
// 含账号 a、tunnel t 与秘密 s。界面只接收原始 token；粘贴整条安装命令一律拒绝。
// 返回值只在调用方封存与交给引擎时使用，绝不进入日志、审计或响应。
func ValidateToken(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", fmt.Errorf("%w：请粘贴 Cloudflare 给出的 tunnel token", ErrInvalidToken)
	}
	if strings.ContainsAny(t, " \t\r\n") || strings.Contains(strings.ToLower(t), "cloudflared") {
		return "", fmt.Errorf("%w：只粘贴 token 本身（以 eyJ 开头的一串），不要粘贴整条安装命令", ErrInvalidToken)
	}
	if len(t) > maxTokenLen {
		return "", fmt.Errorf("%w：过长", ErrInvalidToken)
	}
	var decoded []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err = enc.DecodeString(t)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w：不是 base64 形态", ErrInvalidToken)
	}
	var body struct {
		A string `json:"a"`
		T string `json:"t"`
		S string `json:"s"`
	}
	if json.Unmarshal(decoded, &body) != nil || body.A == "" || body.T == "" || body.S == "" {
		return "", fmt.Errorf("%w：内容不是 Cloudflare Tunnel token", ErrInvalidToken)
	}
	return t, nil
}
