package claudeauth

import (
	"fmt"
	"strings"
	"testing"
)

const fakeSetupToken = "fake-claude-setup-credential-never-real-0123456789"

func TestSetupTokenCanonicalRoundTrip(t *testing.T) {
	cred, err := FromSetupToken(" \n" + fakeSetupToken + "\r\n")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob, `"setup_token"`) {
		t.Fatalf("canonical blob 缺字段: %q", blob)
	}
	got, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token() != fakeSetupToken {
		t.Fatal("setup-token 往返不一致")
	}
}

func TestSetupTokenRejectsUnsafeShapesWithoutEcho(t *testing.T) {
	for _, input := range []string{"", "fake token", "fake\ntoken", string([]byte{0xff})} {
		_, err := FromSetupToken(input)
		if err == nil {
			t.Fatalf("FromSetupToken(%q) 应失败", input)
		}
		if input != "" && strings.Contains(err.Error(), input) {
			t.Fatalf("错误回显凭据: %v", err)
		}
	}
	if _, err := Parse(`{"setup_token":"` + fakeSetupToken + `","future":true}`); err == nil {
		t.Fatal("未知凭据字段应拒绝")
	}
}

// TestCredentialSelfRedaction：顺手的诊断打印（String、fmt 通用格式化、slog）
// 一律只见 [REDACTED]，见不到 setup-token 本身；空凭据编码不出规范包装。
func TestCredentialSelfRedaction(t *testing.T) {
	cred, err := FromSetupToken(fakeSetupToken)
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
		if strings.Contains(rendered, fakeSetupToken) {
			t.Errorf("%s 泄露了 setup-token: %q", name, rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("%s 未标注遮蔽: %q", name, rendered)
		}
	}
	if _, err := (&Credential{}).JSON(); err == nil {
		t.Error("空凭据 JSON() 应失败")
	}
}
