// Package cursorauth 只负责 Cursor 订阅凭据在设备侧的存储形态。它刻意不实现
// 任何登录协议：管理员在 cursor.com/dashboard 自行签发 API Key 粘贴进管理台，
// 设备只做校验、封存与出站换发（exchange_user_api_key 的注入点在网关，本包
// 不出网）。整体仿 internal/claudeauth 的 setup-token 纪律。
//
// 明文是高价值凭据。调用方绝不能以通用格式化打印 Credential，也不得把它的
// JSON 放进任何 API 响应或日志（§15.1）。
package cursorauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxAPIKeyBytes = 16 << 10

// Credential 是封在 agent_accounts.auth_json_sealed（provider=cursor）里的
// 规范明文对象。字段不导出：误用 json.Marshal 或 fmt 格式化都吐不出 Key。
type Credential struct {
	apiKey string
}

type wireCredential struct {
	APIKey string `json:"api_key"`
}

// FromAPIKey 校验管理员从 Cursor Dashboard 复制来的一把 API Key。刻意不钉
// 前缀：前缀不是文档化的兼容契约，可能独立于固件变化。
func FromAPIKey(raw string) (*Credential, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return nil, errors.New("Cursor API Key 不能为空")
	}
	if len(key) > maxAPIKeyBytes {
		return nil, fmt.Errorf("Cursor API Key 过长（最多 %d 字节）", maxAPIKeyBytes)
	}
	if !utf8.ValidString(key) {
		return nil, errors.New("Cursor API Key 不是有效文本")
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, errors.New("Cursor API Key 中间不得包含空白或控制字符")
		}
	}
	return &Credential{apiKey: key}, nil
}

// Parse 解码 store 解封后的规范明文对象。未知字段一律拒绝：损坏或手改过的
// 行不能悄悄长出第二份事实来源。
func Parse(blob string) (*Credential, error) {
	dec := json.NewDecoder(strings.NewReader(blob))
	dec.DisallowUnknownFields()
	var wire wireCredential
	if err := dec.Decode(&wire); err != nil {
		return nil, errors.New("Cursor 订阅凭据格式不正确")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("Cursor 订阅凭据含多余内容")
	}
	cred, err := FromAPIKey(wire.APIKey)
	if err != nil {
		return nil, errors.New("Cursor 订阅凭据格式不正确")
	}
	return cred, nil
}

// JSON 返回交给 store 封存的规范明文表示。
func (c *Credential) JSON() (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("Cursor API Key 不能为空")
	}
	b, err := json.Marshal(wireCredential{APIKey: c.apiKey})
	if err != nil {
		return "", errors.New("编码 Cursor 订阅凭据失败")
	}
	return string(b), nil
}

// Token 只把明文交给出站换发请求的窄路径（exchange_user_api_key 的
// Authorization 头构造）。
func (c *Credential) Token() string {
	if c == nil {
		return ""
	}
	return c.apiKey
}

// String / GoString / LogValue 让顺手的诊断打印吐不出明文——GoString 单独
// 存在是因为 %#v 走 fmt.GoStringer 而不是 Stringer，缺了它 %#v 会按 Go 语法
// 连未导出字段一起打印。值接收者是有意的：方法同时进 Credential 与
// *Credential 的方法集，解引用或按值内嵌的凭据也走遮蔽——fmt 拿到的是
// 不可寻址的值，够不着指针接收者方法。
func (Credential) String() string { return "CursorCredential([REDACTED])" }

func (Credential) GoString() string { return "CursorCredential([REDACTED])" }

func (Credential) LogValue() slog.Value {
	return slog.GroupValue(slog.String("credential", "[REDACTED]"))
}
