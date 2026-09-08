// cursorauth 的可执行验收（迭代 3 Phase 1）：API Key 校验、规范包装往返、
// 严格解析，以及 §15.1 自遮蔽（String/LogValue/通用格式化都吐不出明文）。
// 夹具全部是假凭据（仓库凭据纪律）。
package cursorauth

import (
	"fmt"
	"strings"
	"testing"
)

const fakeAPIKey = "fake-cursor-dashboard-api-key-never-real-0123456789"

func TestAPIKeyCanonicalRoundTrip(t *testing.T) {
	cred, err := FromAPIKey(" \n" + fakeAPIKey + "\r\n")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob, `"api_key"`) {
		t.Fatalf("canonical blob 缺字段: %q", blob)
	}
	got, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token() != fakeAPIKey {
		t.Fatal("API Key 往返不一致")
	}
}

func TestAPIKeyRejectsUnsafeShapesWithoutEcho(t *testing.T) {
	overlong := strings.Repeat("a", 16<<10+1)
	for _, input := range []string{"", "fake key", "fake\nkey", "fake\tkey", string([]byte{0xff}), overlong} {
		_, err := FromAPIKey(input)
		if err == nil {
			t.Fatalf("FromAPIKey(%.20q…) 应失败", input)
		}
		if input != "" && strings.Contains(err.Error(), input) {
			t.Fatalf("错误回显凭据: %v", err)
		}
	}
	for _, blob := range []string{
		`{"api_key":"` + fakeAPIKey + `","future":true}`, // 未知字段
		`{"api_key":"` + fakeAPIKey + `"}{}`,             // 多余内容
		`{"api_key":""}`,                                 // 空 Key
		`{}`,
		`not-json`,
	} {
		if _, err := Parse(blob); err == nil {
			t.Fatalf("Parse(%q) 应拒绝", blob)
		}
	}
}

// TestCredentialSelfRedaction：顺手的诊断打印（String、fmt 通用格式化、slog）
// 一律只见 [REDACTED]，见不到 Key 本身；空凭据编码不出规范包装。
func TestCredentialSelfRedaction(t *testing.T) {
	cred, err := FromAPIKey(fakeAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, rendered := range map[string]string{
		"String()": cred.String(),
		"%v":       fmt.Sprintf("%v", cred),
		"%+v":      fmt.Sprintf("%+v", cred),
		"%#v":      fmt.Sprintf("%#v", cred), // Go 语法验证走 GoStringer，不走 Stringer
		// 解引用后的值形态：值接收者三件套要求 Credential 与 *Credential 都被挡。
		"%v值":      fmt.Sprintf("%v", *cred),
		"%+v值":     fmt.Sprintf("%+v", *cred),
		"%#v值":     fmt.Sprintf("%#v", *cred),
		"LogValue": cred.LogValue().String(),
	} {
		if strings.Contains(rendered, fakeAPIKey) {
			t.Errorf("%s 泄露了 API Key: %q", name, rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("%s 未标注遮蔽: %q", name, rendered)
		}
	}
	if _, err := (&Credential{}).JSON(); err == nil {
		t.Error("空凭据 JSON() 应失败")
	}
}
