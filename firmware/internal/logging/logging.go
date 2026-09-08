// Package logging 是 llmgate 全部运行日志的唯一出口，落实根 AGENTS.md「日志与
// 脱敏」（即 §15.1）硬规则（违反即安全事故）：
//
//   - 提示词、模型响应、上传媒体内容不得写入日志——任何级别，包括 debug 与 trace。
//   - 上游密钥、本地 API密钥 明文、备份口令、会话 Cookie 不得写入日志。
//   - 请求体与响应体只记录长度、内容哈希前缀、content-type，不记录内容。
//   - debug 模式不放宽以上规则。
//
// 硬规则对调用方的约束：任何调用方不得把请求/响应 body 或凭证明文传入
// logger——需要标识某个 Key 时必须用 [RedactKey]，需要记录 body 元信息时
// 必须用 [BodyMeta]（整段可得）或 [BodyMeter]（流式路径）。本包只提供脱敏
// 后的落日志途径，不提供任何「原样记录内容」的接口；排障采样等受控例外
// 未来单独设计，不经由本包放宽。
package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"strings"
)

// New 返回把 JSON 行写入 w 的 logger，低于 level 的记录被丢弃。
// 输出格式固定为 slog JSON（time/level/msg + 调用方属性）。
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// ParseLevel 解析配置 log_level 字符串（大小写不敏感）。
// 支持 debug|info|warn|error；空串按 info 处理。
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("未知 log_level %q（可选 debug|info|warn|error）", s)
	}
}

// RedactKey 把密钥/API密钥 变为可安全写入日志与错误信息的标识：
// 至多保留 4 位 ASCII 前缀，加 SHA-256 摘要前 8 位十六进制后缀，
// 形如 "sk_a…3f9c01ab"。前缀便于人工比对，摘要保证同一 Key 标识稳定
// 且不可逆推。短于 8 字节的 Key 不保留前缀；空值返回 "(empty)"。
func RedactKey(key string) string {
	if key == "" {
		return "(empty)"
	}
	sum := sha256.Sum256([]byte(key))
	digest := hex.EncodeToString(sum[:4])
	prefix := ""
	if len(key) >= 8 && isPrintableASCII(key[:4]) {
		prefix = key[:4]
	}
	return prefix + "…" + digest
}

// BodyMeta 返回请求/响应 body 的可落日志元数据，以内联组并入日志记录，
// 字段为 body_len（字节数）、body_sha256_8（SHA-256 前 8 位十六进制）、
// content_type。body 内容本身绝不进入返回值。
//
// 同一条记录只放一个内联 BodyMeta，否则 JSON 字段名冲突；需要同时记录
// 请求与响应两侧元数据时，用命名组隔开：
//
//	logger.Info("access",
//	    slog.Group("req", logging.BodyMeta(reqBody, reqCT)),
//	    slog.Group("resp", logging.BodyMeta(respBody, respCT)))
func BodyMeta(body []byte, contentType string) slog.Attr {
	sum := sha256.Sum256(body)
	return bodyAttr(int64(len(body)), sum[:4], contentType)
}

// BodyMeter 增量累计一侧 body 的长度与 SHA-256，供无法整段缓冲 body 的
// 路径（访问日志、SSE 流式转发）产出与 [BodyMeta] 同契约的日志元数据：
// 把经过的字节喂给 Write，结束后用 [BodyMeter.Attr] 取属性组。
// body 内容只进哈希，不被留存。非并发安全，单请求内单 goroutine 使用。
type BodyMeter struct {
	n int64
	h hash.Hash
}

// NewBodyMeter 返回归零的 BodyMeter。
func NewBodyMeter() *BodyMeter { return &BodyMeter{h: sha256.New()} }

// Write 实现 io.Writer：只累计长度与哈希，永不失败。
func (m *BodyMeter) Write(p []byte) (int, error) {
	m.n += int64(len(p))
	m.h.Write(p)
	return len(p), nil
}

// Attr 返回与 [BodyMeta] 相同字段（body_len/body_sha256_8/content_type）
// 的内联属性组；同一条记录记两侧时同样需用命名组包裹（见 BodyMeta 文档）。
func (m *BodyMeter) Attr(contentType string) slog.Attr {
	return bodyAttr(m.n, m.h.Sum(nil)[:4], contentType)
}

// bodyAttr 是 BodyMeta/BodyMeter 共用的属性组构造：字段名与语义在此收敛。
func bodyAttr(n int64, sha4 []byte, contentType string) slog.Attr {
	return slog.Attr{
		// 空 Key 组：slog 各 handler 会把组内属性内联到当前层级。
		Key: "",
		Value: slog.GroupValue(
			slog.Int64("body_len", n),
			slog.String("body_sha256_8", hex.EncodeToString(sha4)),
			slog.String("content_type", contentType),
		),
	}
}

// isPrintableASCII 报告 s 是否全部由可打印 ASCII 组成——
// RedactKey 只在此前提下保留前缀，避免按字节截断多字节字符。
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
