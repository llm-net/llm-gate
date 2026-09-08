// redact_test.go 是 iteration-1 Phase 2 的可执行验收：
// 断言经 internal/logging 的日志输出在任何级别（含 debug）都不含
// API密钥 明文、Authorization 头、请求体内容，只含 body_len /
// body_sha256_8 / content_type 等元数据字段（§15.1 脱敏硬规则）。
package logging_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
)

// 模拟一次真实请求携带的敏感材料。
const (
	testAPIKey      = "sk_supersecret_0123456789abcdef"
	testUpstreamKey = "sk-upstream-fedcba9876543210xyz"
)

var (
	testAuthHeader = "Bearer " + testAPIKey
	// 模拟提示词请求体：内容在任何日志级别都不得出现。
	testBody = []byte(`{"model":"chat-fast","messages":[{"role":"user","content":"绝密提示词：给我讲一个秘密"}]}`)
)

func sha8(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// emitTypicalRecords 模拟网关按硬规则落访问日志：涉及 Key 一律 RedactKey，
// 涉及 body 一律 BodyMeta，绝不把明文传入 logger。
func emitTypicalRecords(logger *slog.Logger) {
	logger.Debug("incoming request",
		"method", "POST",
		"path", "/v1/chat/completions",
		"api_key", logging.RedactKey(testAPIKey),
		logging.BodyMeta(testBody, "application/json"),
	)
	logger.Info("upstream request",
		"upstream", "mock-main",
		"upstream_key", logging.RedactKey(testUpstreamKey),
		logging.BodyMeta(testBody, "application/json"),
	)
	logger.Error("upstream error",
		"status", 502,
		"api_key", logging.RedactKey(testAPIKey),
		logging.BodyMeta(testBody, "application/json"),
	)
}

// assertNoSecrets 断言原始日志输出不含任何敏感明文。
func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	// body 若被塞进 JSON 字段，引号会被转义成 \"，原文包含检查匹配不到；
	// 因此同时断言 JSON 转义后的形态（去掉 Marshal 加的首尾引号）。
	escapedBody, err := json.Marshal(string(testBody))
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"本地 API密钥 明文":       testAPIKey,
		"Authorization 头":   testAuthHeader,
		"上游 Key 明文":         testUpstreamKey,
		"请求体全文":             string(testBody),
		"请求体全文（JSON 转义后）":   string(escapedBody[1 : len(escapedBody)-1]),
		"提示词内容":             "绝密提示词",
		"提示词片段":             "给我讲一个秘密",
		"API密钥 前缀第 5 位起片段":  testAPIKey[4:12],
		"上游 Key 前缀第 5 位起片段": testUpstreamKey[4:12],
	}
	for label, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("日志输出泄露%s %q:\n%s", label, s, out)
		}
	}
}

// TestLogOutputRedaction_AllLevels 覆盖 info 与 debug 两档 logger：
// debug 级别不得放宽脱敏（§15.1 第 4 条）。
func TestLogOutputRedaction_AllLevels(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var buf bytes.Buffer
			logger := logging.New(&buf, level)
			emitTypicalRecords(logger)

			out := buf.String()
			if out == "" {
				t.Fatal("日志无输出")
			}
			assertNoSecrets(t, out)

			// 每行必须是 JSON，且含且仅含元数据形式的 body 信息。
			lines := nonEmptyLines(out)
			wantLines := 3
			if level == slog.LevelInfo {
				wantLines = 2 // debug 行被级别过滤
			}
			if len(lines) != wantLines {
				t.Fatalf("期望 %d 行日志，得到 %d 行:\n%s", wantLines, len(lines), out)
			}
			for _, line := range lines {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("日志行不是 JSON: %v\n%s", err, line)
				}
				assertBodyMetaFields(t, rec)
			}
		})
	}
}

// assertBodyMetaFields 断言记录含元数据字段且取值正确，同时不存在任何 body 内容字段。
func assertBodyMetaFields(t *testing.T, rec map[string]any) {
	t.Helper()
	if got, want := rec["body_len"], float64(len(testBody)); got != want {
		t.Errorf("body_len = %v，期望 %v", got, want)
	}
	if got, want := rec["body_sha256_8"], sha8(testBody); got != want {
		t.Errorf("body_sha256_8 = %v，期望 %v", got, want)
	}
	if got, want := rec["content_type"], "application/json"; got != want {
		t.Errorf("content_type = %v，期望 %v", got, want)
	}
	if _, ok := rec["body"]; ok {
		t.Error("日志记录不得含 body 字段")
	}
}

// TestRedactKey 验证 RedactKey 的输出形态：至多保留 4 位前缀 + SHA-256 摘要前 8 位，
// 可稳定标识 Key 但不可逆推。
func TestRedactKey(t *testing.T) {
	key := testAPIKey
	r := logging.RedactKey(key)

	if strings.Contains(r, key) {
		t.Fatalf("RedactKey 输出含完整 Key: %q", r)
	}
	if !strings.HasPrefix(r, key[:4]) {
		t.Errorf("RedactKey 应保留 4 位前缀便于人工比对，got %q", r)
	}
	if strings.Contains(r, key[:5]) {
		t.Errorf("RedactKey 泄露超过 4 位明文前缀: %q", r)
	}
	if want := sha8([]byte(key)); !strings.HasSuffix(r, want) {
		t.Errorf("RedactKey 摘要后缀应为 SHA-256 前 8 位十六进制 %q，got %q", want, r)
	}
	if r2 := logging.RedactKey(key); r2 != r {
		t.Errorf("RedactKey 必须确定性: %q != %q", r, r2)
	}
	if other := logging.RedactKey(testUpstreamKey); other == r {
		t.Errorf("不同 Key 的 RedactKey 不应相同: %q", r)
	}

	// 短 Key 不保留前缀，避免泄露大半内容。
	short := "abc1234"
	rs := logging.RedactKey(short)
	if strings.Contains(rs, "abc") {
		t.Errorf("短 Key 不得保留明文前缀: %q", rs)
	}

	// 空 Key 返回固定占位，不参与摘要比对。
	if re := logging.RedactKey(""); re != "(empty)" {
		t.Errorf("空 Key 应返回 (empty)，got %q", re)
	}
}

// TestParseLevel 验证 log_level 字符串解析。
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"", slog.LevelInfo, true}, // 缺省 info
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"INFO", slog.LevelInfo, true}, // 大小写不敏感
		{"verbose", 0, false},
	}
	for _, c := range cases {
		got, err := logging.ParseLevel(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseLevel(%q) = %v, %v；期望 %v", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseLevel(%q) 应报错", c.in)
		}
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
