package store

// credentials 仓储验收：
//
//   - 建行 / 列表 / 点查 / 改 / 删的基本形状与 ErrNotFound / ErrConflict；
//   - 令牌只经 GetCredentialSecret 解封取回，公开面与列表里没有它；
//   - 密文钉本行 id：把 A 行的密文抄进 B 行解不开，而不是悄悄当成 B 的令牌；
//   - 入参校验（类型、站点词汇表、用户名、令牌）返回 ErrInvalidCredential；
//   - CredentialSecret 自遮蔽：%v / %+v / %#v、slog、json.Marshal 都印不出令牌（§15.1）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const testGitToken = "ghp_TESTTOKEN0123456789abcdef"

func newGitCredential(t *testing.T, s *Store, host, user string) *Credential {
	t.Helper()
	c, err := s.CreateCredential(context.Background(), NewCredential{
		Kind: CredentialKindGit, Name: "工作账号", Host: host, Username: user, Secret: testGitToken,
	})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return c
}

func TestCredentialLifecycle(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	list, err := s.ListCredentials(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("初始清单 = %+v（err=%v）", list, err)
	}

	c := newGitCredential(t, s, "github.com", "octocat")
	if len(c.ID) != 26 || c.Kind != CredentialKindGit || c.Host != "github.com" || c.Username != "octocat" || c.Name != "工作账号" {
		t.Fatalf("新行 = %+v", c)
	}
	if c.SecretHint != testGitToken[len(testGitToken)-4:] {
		t.Fatalf("末 4 位 = %q", c.SecretHint)
	}
	if c.CreatedAt.IsZero() || !c.UpdatedAt.Equal(c.CreatedAt) {
		t.Fatalf("时刻 = %v / %v", c.CreatedAt, c.UpdatedAt)
	}

	// 同一类型 + 站点 + 账号只留一份；换站点就是另一份。
	if _, err := s.CreateCredential(ctx, NewCredential{Kind: CredentialKindGit, Host: "GitHub.com ", Username: "octocat", Secret: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复应冲突：%v", err)
	}
	c2 := newGitCredential(t, s, "gitee.com", "octocat")

	list, err = s.ListCredentials(ctx)
	if err != nil || len(list) != 2 || list[0].ID != c.ID || list[1].ID != c2.ID {
		t.Fatalf("清单 = %+v（err=%v）", list, err)
	}

	// 令牌只经 GetCredentialSecret 取回。
	sec, err := s.GetCredentialSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCredentialSecret: %v", err)
	}
	if sec.Plaintext() != testGitToken || sec.ID != c.ID || sec.Username != "octocat" {
		t.Fatalf("解封 = %+v / %q", sec.Credential, sec.Plaintext())
	}

	// 只改名称与账号，令牌不动（Secret 指向空串 = 保持）。
	empty := ""
	name := "  备用账号 "
	user := "octocat-2"
	u, err := s.UpdateCredential(ctx, c.ID, CredentialPatch{Name: &name, Username: &user, Secret: &empty})
	if err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	if u.Name != "备用账号" || u.Username != "octocat-2" || u.Host != "github.com" || u.SecretHint != c.SecretHint {
		t.Fatalf("改后 = %+v", u)
	}
	if sec, err = s.GetCredentialSecret(ctx, c.ID); err != nil || sec.Plaintext() != testGitToken {
		t.Fatalf("不改令牌时令牌变了：%q（err=%v）", sec.Plaintext(), err)
	}
	// 换令牌：密文与末 4 位一起换。
	next := "glpat-NEWTOKEN9876543210"
	host := "gitee.com"
	if _, err = s.UpdateCredential(ctx, c.ID, CredentialPatch{Secret: &next, Host: &host}); err != nil {
		t.Fatalf("换令牌: %v", err)
	}
	u, _ = s.GetCredential(ctx, c.ID)
	if u.Host != "gitee.com" || u.SecretHint != next[len(next)-4:] {
		t.Fatalf("换令牌后 = %+v", u)
	}
	if sec, err = s.GetCredentialSecret(ctx, c.ID); err != nil || sec.Plaintext() != next {
		t.Fatalf("换令牌后解封 = %q（err=%v）", sec.Plaintext(), err)
	}
	// 改成与另一行同站点同账号 → 冲突。
	clash := "octocat"
	if _, err = s.UpdateCredential(ctx, c.ID, CredentialPatch{Username: &clash}); !errors.Is(err, ErrConflict) {
		t.Fatalf("改成重复应冲突：%v", err)
	}

	if err = s.DeleteCredential(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if err = s.DeleteCredential(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("二次删除应 ErrNotFound：%v", err)
	}
	if _, err = s.GetCredential(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后点查应 ErrNotFound：%v", err)
	}
	if _, err = s.GetCredentialSecret(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后解封应 ErrNotFound：%v", err)
	}
	if _, err = s.UpdateCredential(ctx, c.ID, CredentialPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后修改应 ErrNotFound：%v", err)
	}
}

func TestCredentialValidation(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	long := strings.Repeat("a", credentialUserMaxRunes+1)
	for name, bad := range map[string]NewCredential{
		"类型为空":    {Host: "github.com", Username: "u", Secret: "x"},
		"类型不认识":   {Kind: "ssh", Host: "github.com", Username: "u", Secret: "x"},
		"站点不在词汇表": {Kind: CredentialKindGit, Host: "gitlab.com", Username: "u", Secret: "x"},
		"站点为空":    {Kind: CredentialKindGit, Username: "u", Secret: "x"},
		"用户名为空":   {Kind: CredentialKindGit, Host: "github.com", Username: " ", Secret: "x"},
		"用户名带空白":  {Kind: CredentialKindGit, Host: "github.com", Username: "a b", Secret: "x"},
		"用户名过长":   {Kind: CredentialKindGit, Host: "github.com", Username: long, Secret: "x"},
		"令牌为空":    {Kind: CredentialKindGit, Host: "github.com", Username: "u"},
		"令牌带换行":   {Kind: CredentialKindGit, Host: "github.com", Username: "u", Secret: "a\nb"},
		"令牌过长":    {Kind: CredentialKindGit, Host: "github.com", Username: "u", Secret: strings.Repeat("x", credentialSecretMax+1)},
		"名称过长":    {Kind: CredentialKindGit, Host: "github.com", Username: "u", Secret: "x", Name: strings.Repeat("名", credentialNameMaxRunes+1)},
	} {
		_, err := s.CreateCredential(ctx, bad)
		if !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("%s：应 ErrInvalidCredential，得 %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), bad.Secret) && bad.Secret != "" && bad.Secret != "x" {
			t.Errorf("%s：错误文本回显了令牌", name)
		}
	}
	if list, _ := s.ListCredentials(ctx); len(list) != 0 {
		t.Fatalf("被拒的入参落库了：%+v", list)
	}
	// 站点大小写与首尾空白归一。
	c, err := s.CreateCredential(ctx, NewCredential{Kind: CredentialKindGit, Host: " Gitee.COM ", Username: " u ", Secret: "x"})
	if err != nil || c.Host != "gitee.com" || c.Username != "u" {
		t.Fatalf("归一 = %+v（err=%v）", c, err)
	}
	// 修改时同样校验。
	badHost := "bitbucket.org"
	if _, err := s.UpdateCredential(ctx, c.ID, CredentialPatch{Host: &badHost}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("改成不认识的站点应被拒：%v", err)
	}
	badUser := ""
	if _, err := s.UpdateCredential(ctx, c.ID, CredentialPatch{Username: &badUser}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("改成空用户名应被拒：%v", err)
	}
}

// 密文钉本行 id：把 A 的密文抄进 B，B 解不开。
func TestCredentialSecretPinnedToRow(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	a := newGitCredential(t, s, "github.com", "a")
	b := newGitCredential(t, s, "github.com", "b")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET secret_sealed = (SELECT secret_sealed FROM credentials WHERE id = ?) WHERE id = ?`, a.ID, b.ID); err != nil {
		t.Fatalf("搬密文: %v", err)
	}
	if sec, err := s.GetCredentialSecret(ctx, a.ID); err != nil || sec.Plaintext() != testGitToken {
		t.Fatalf("A 自己的密文应照常解开：%v", err)
	}
	_, err := s.GetCredentialSecret(ctx, b.ID)
	if err == nil {
		t.Fatal("搬来的密文不该解得开")
	}
	if strings.Contains(err.Error(), testGitToken) {
		t.Fatal("错误文本回显了令牌")
	}
}

// CredentialSecret 自遮蔽：值与指针的 %v / %+v / %#v、slog、json.Marshal 都印不出令牌。
func TestCredentialSecretRedacted(t *testing.T) {
	s, _ := mustOpen(t)
	c := newGitCredential(t, s, "github.com", "octocat")
	sec, err := s.GetCredentialSecret(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("GetCredentialSecret: %v", err)
	}
	if sec.Plaintext() != testGitToken {
		t.Fatalf("Plaintext = %q", sec.Plaintext())
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("cred", "value", sec, "ptr", &sec, slog.Any("any", sec))
	outputs := map[string]string{
		"%v":       fmt.Sprintf("%v", sec),
		"%+v":      fmt.Sprintf("%+v", sec),
		"%#v":      fmt.Sprintf("%#v", sec),
		"%v ptr":   fmt.Sprintf("%v", &sec),
		"%+v ptr":  fmt.Sprintf("%+v", &sec),
		"%#v ptr":  fmt.Sprintf("%#v", &sec),
		"%s":       fmt.Sprintf("%s", sec),
		"slog":     buf.String(),
		"embedded": fmt.Sprintf("%+v", struct{ Inner CredentialSecret }{sec}),
	}
	if raw, err := json.Marshal(sec); err != nil {
		t.Fatalf("json.Marshal: %v", err)
	} else {
		outputs["json"] = string(raw)
	}
	if raw, err := json.Marshal(&sec); err != nil {
		t.Fatalf("json.Marshal ptr: %v", err)
	} else {
		outputs["json ptr"] = string(raw)
	}
	for name, out := range outputs {
		if strings.Contains(out, testGitToken) {
			t.Errorf("%s 泄漏了令牌：%s", name, out)
		}
		if !strings.Contains(out, "octocat") {
			t.Errorf("%s 连公开面都没有：%s", name, out)
		}
	}
}
