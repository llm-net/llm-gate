package store

// agent_accounts（Agents / Codex 订阅代理凭据）的可执行验收（迭代 11 Phase 1）。
//
// 四条性质是这里要钉住的，每一条都对应一种真会发生的事故：
//
//   - 整份 auth.json 密封 round-trip 逐字节还原——句柄少一个字节就是一次
//     "刷新永远 401、还查不出为什么"。
//   - 库里、乃至整个数据目录的原始字节里没有明文（§15.1 的落点）。
//   - AAD 钉 provider：codex 的密文当别的 provider 解会**失败**，而不是悄悄
//     取到值——将来接第二个 provider 时，这是唯一能挡住串号的东西。
//   - 坏 device-key 下解封给可读错误，且**两侧都不回显**（明文与密文都不进
//     错误串，否则错误一落日志就是 §15.1 违规）。
//
// 本文件里的 token 全是假的：任何形如真实凭据的串都不许进仓库。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAuthJSON 是一份形态逼真、内容全假的 auth.json（codex login 的产物形态）。
// 故意带缩进与嵌套：密封走的是字节通道，round-trip 必须逐字节相等。
const fakeAuthJSON = `{
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": "fake.id.token-NOT-REAL",
    "access_token": "fake-access-token-NOT-REAL-0123456789",
    "refresh_token": "fake-refresh-token-NOT-REAL-9876543210",
    "account_id": "acct_fake_0001"
  },
  "last_refresh": "2026-08-11T00:00:00.000Z"
}`

// mustAgent 连一份订阅账号，失败即终止测试。
func mustAgent(t *testing.T, s *Store, provider, authJSON string) *AgentAccount {
	t.Helper()
	acct, err := s.UpsertAgentAccount(context.Background(), NewAgentAccount{
		Provider:     provider,
		Label:        "订阅一号",
		AccountID:    "acct_fake_0001",
		DefaultModel: "gpt-5-codex",
		AuthJSON:     authJSON,
	})
	if err != nil {
		t.Fatalf("UpsertAgentAccount(%s): %v", provider, err)
	}
	return acct
}

// TestAgentAuthJSONSealRoundTrip：整份 auth.json 密封往返逐字节还原；库里那一列
// 是密文；管理视图（列表/点查）一个字节的密文都不带。
func TestAgentAuthJSONSealRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	acct := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	if acct.Status != AgentStatusActive {
		t.Errorf("新连接的账号状态 = %q, 期望 %q", acct.Status, AgentStatusActive)
	}
	if !acct.LastRefreshAt.IsZero() {
		t.Errorf("刚连上不该有刷新时间: %v", acct.LastRefreshAt)
	}
	if acct.AuthJSONSealed != "" {
		t.Error("管理视图（Upsert 返回值）不该带密文")
	}

	got, authJSON, err := s.GetAgentCredential(ctx, AgentProviderCodex)
	if err != nil {
		t.Fatalf("GetAgentCredential: %v", err)
	}
	if authJSON != fakeAuthJSON {
		t.Errorf("auth.json 往返不一致:\n得到 %q\n期望 %q", authJSON, fakeAuthJSON)
	}
	if got.ID != acct.ID || got.AccountID != "acct_fake_0001" || got.DefaultModel != "gpt-5-codex" {
		t.Errorf("取令牌视图字段不符: %+v", struct {
			ID                    int64
			AccountID, DefaultMdl string
		}{got.ID, got.AccountID, got.DefaultModel})
	}

	// 密文列真的是密文（不是原样存），且随机 nonce 使同一明文两次封存不同。
	var sealed string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE id = ?`, acct.ID).Scan(&sealed); err != nil {
		t.Fatalf("读 auth_json_sealed: %v", err)
	}
	if sealed == "" || strings.Contains(sealed, "refresh_token") || strings.Contains(sealed, fakeAuthJSON) {
		t.Fatalf("auth_json_sealed 未加密: %q", sealed)
	}
	if err := s.SetAgentAuthJSON(ctx, acct.ID, AgentProviderCodex, fakeAuthJSON); err != nil {
		t.Fatalf("SetAgentAuthJSON: %v", err)
	}
	var sealed2 string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE id = ?`, acct.ID).Scan(&sealed2); err != nil {
		t.Fatal(err)
	}
	if sealed2 == sealed {
		t.Error("相同明文两次封存应因随机 nonce 得到不同密文")
	}

	// 刷新回写盖 last_refresh_at，状态不动。
	after, err := s.GetAgentAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAgentAccount: %v", err)
	}
	if after.LastRefreshAt.IsZero() {
		t.Error("刷新回写后 last_refresh_at 仍为空")
	}
	if after.Status != AgentStatusActive {
		t.Errorf("刷新不该改状态，得到 %q", after.Status)
	}
	if after.AuthJSONSealed != "" {
		t.Error("GetAgentAccount 是管理视图，不该带密文")
	}

	// 列表同样是管理视图：无密文、无 auth.json。
	list, err := s.ListAgentAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAgentAccounts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列表 %d 行, 期望 1", len(list))
	}
	if list[0].AuthJSONSealed != "" {
		t.Error("ListAgentAccounts 带出了密文")
	}
	if list[0].Label != "订阅一号" || list[0].Provider != AgentProviderCodex {
		t.Errorf("列表字段不符: %q/%q", list[0].Label, list[0].Provider)
	}
}

// TestAgentAuthJSONNotOnDisk：数据目录下任何文件（含 WAL）的原始字节里都没有
// auth.json 的明文片段。单独外流的 llmgate.db 副本不该含订阅句柄（§15.1）。
func TestAgentAuthJSONNotOnDisk(t *testing.T) {
	s, dir := mustOpen(t)

	mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("数据目录文件数 = %d, 期望至少 llmgate.db 与 device-key", len(entries))
	}
	// 逐个 token 查，比整份 blob 查严：整份 blob 只要有一个字节不同就"通过"了。
	needles := []string{
		"fake-access-token-NOT-REAL-0123456789",
		"fake-refresh-token-NOT-REAL-9876543210",
		"fake.id.token-NOT-REAL",
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读 %s: %v", e.Name(), err)
		}
		for _, needle := range needles {
			if bytes.Contains(raw, []byte(needle)) {
				t.Errorf("%s 的原始字节含 auth.json 明文片段（§15.1 违规）", e.Name())
			}
		}
	}
}

// TestAgentSealAADBindsProvider：密文钉在自己的 provider 上——把 codex 那份密文
// 搬到另一个 provider 的行上，解出来的是错误而不是"那个 provider 的凭据"。
// 这条是将来接第二个 provider 时唯一能挡住串号的东西。
func TestAgentSealAADBindsProvider(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	acct := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	var sealed string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE id = ?`, acct.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}

	// 直接把密文塞进另一个 provider 的行（模拟串号/误抄）。
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_accounts
		(provider, label, account_id, default_model, auth_json_sealed, status, created_at, updated_at)
		VALUES ('claude', '', '', '', ?, 'active', '2026-08-11T00:00:00.000Z', '2026-08-11T00:00:00.000Z')`,
		sealed); err != nil {
		t.Fatal(err)
	}

	// 失败时只印 id/provider：这是唯一带密文的视图，%+v 会把密文与明文一起
	// 印进测试输出（agents.go 里那条"本结构不得以 %+v 打印"的规矩从这里就得守）。
	got, authJSON, err := s.GetAgentCredential(ctx, "claude")
	if err == nil {
		t.Fatalf("换 provider 仍解出了凭据: id=%d provider=%s, 明文 %d 字节",
			got.ID, got.Provider, len(authJSON))
	}
	if !errors.Is(err, ErrAgentAuthUnreadable) {
		t.Errorf("期望 ErrAgentAuthUnreadable, 得到 %v", err)
	}
	assertNoAgentSecretLeak(t, err, sealed)

	// 原 provider 照旧解得开（AAD 没把好行也一起挡掉）。
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCodex); err != nil || plain != fakeAuthJSON {
		t.Errorf("原 provider 应仍可解: err=%v", err)
	}

	// 反向：SetAgentAuthJSON 的 provider 进了 WHERE，写不到别人的行上。
	err = s.SetAgentAuthJSON(ctx, acct.ID, "claude", fakeAuthJSON)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("provider 不符的凭据回写应 ErrNotFound, 得到 %v", err)
	}
}

// TestAgentAccountUpsertSingleRow：同一 provider 重复连接是**覆盖而非增行**
// （单账户语义）。覆盖分两组：label/default_model 空即保持（重登不该把管理员
// 设过的默认模型悄悄抹掉），而 account_id 跟着凭据无条件走（陈旧的 account_id
// 配新账号的 token 是一台注入了错 ChatGPT-Account-ID 的代理）。
// 另一个 provider 是另一行。
func TestAgentAccountUpsertSingleRow(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	first := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)

	// 重新登录：只带新凭据，其余字段留空。
	const rotated = `{"tokens":{"access_token":"fake-access-2","refresh_token":"fake-refresh-2"}}`
	second, err := s.UpsertAgentAccount(ctx, NewAgentAccount{
		Provider: AgentProviderCodex,
		AuthJSON: rotated,
	})
	if err != nil {
		t.Fatalf("重复连接: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("重复连接插出了新行: id %d → %d", first.ID, second.ID)
	}
	if second.Label != "订阅一号" || second.DefaultModel != "gpt-5-codex" {
		t.Errorf("空值覆盖抹掉了既有展示字段: label=%q default_model=%q",
			second.Label, second.DefaultModel)
	}
	// account_id 与凭据同进同出：换了一份 token 而新的 claim 解不出来，
	// 宁可空着，也不能把上一个账号的 id 留下来配这份新 token。
	if second.AccountID != "" {
		t.Errorf("account_id 未随凭据一起换掉，仍是 %q", second.AccountID)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCodex); err != nil || plain != rotated {
		t.Errorf("凭据未被新一代覆盖: %q (err=%v)", plain, err)
	}

	// 显式带值的重连覆盖展示字段与 account_id，并把状态拉回 active。
	if err := s.SetAgentStatus(ctx, first.ID, AgentStatusAuthExpired); err != nil {
		t.Fatal(err)
	}
	third, err := s.UpsertAgentAccount(ctx, NewAgentAccount{
		Provider:     AgentProviderCodex,
		Label:        "订阅二号",
		AccountID:    "acct_fake_0002",
		DefaultModel: "gpt-5-codex-mini",
		AuthJSON:     rotated,
	})
	if err != nil {
		t.Fatalf("重新登录: %v", err)
	}
	if third.Label != "订阅二号" || third.DefaultModel != "gpt-5-codex-mini" || third.AccountID != "acct_fake_0002" {
		t.Errorf("显式值未覆盖: label=%q default_model=%q account_id=%q",
			third.Label, third.DefaultModel, third.AccountID)
	}
	if third.Status != AgentStatusActive {
		t.Errorf("重新登录后状态 = %q, 期望回到 %q", third.Status, AgentStatusActive)
	}

	// 空凭据不许落库：一行的存在就意味着句柄在盒子手里。既不许建行，
	// 也不许把在跑的那份刷成空（那不是"清空"，是把句柄弄丢）。
	if _, err := s.UpsertAgentAccount(ctx, NewAgentAccount{Provider: "empty-provider"}); err == nil {
		t.Error("空凭据不应建出账号行")
	}
	if err := s.SetAgentAuthJSON(ctx, first.ID, AgentProviderCodex, ""); err == nil {
		t.Error("空凭据不应覆盖在跑的句柄")
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCodex); err != nil || plain != rotated {
		t.Errorf("被拒的空写入不该动到既有凭据: %q (err=%v)", plain, err)
	}
	// 读侧也守同一条不变式：手改过的库（或将来某个忘了封存那一步的写入方）
	// 留下一行空密文时，返回的必须是错误而不是"一份成功的空凭据"——后者会把
	// 故障推迟到解析 auth.json 或一次没有 Authorization 的上游请求上。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE agent_accounts SET auth_json_sealed = '' WHERE provider = ?`,
		AgentProviderCodex); err != nil {
		t.Fatal(err)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCodex); !errors.Is(err, ErrAgentAuthUnreadable) {
		t.Errorf("空密文应回 ErrAgentAuthUnreadable, 得到 %d 字节明文 (err=%v)", len(plain), err)
	}
	// 复原，后面的断言继续用这一行。
	if err := s.SetAgentAuthJSON(ctx, first.ID, AgentProviderCodex, rotated); err != nil {
		t.Fatal(err)
	}

	// 另一个 provider 是另一行；codex 仍只有一行。
	if _, err := s.UpsertAgentAccount(ctx, NewAgentAccount{
		Provider: "claude", AuthJSON: fakeAuthJSON,
	}); err != nil {
		t.Fatalf("另一 provider: %v", err)
	}
	list, err := s.ListAgentAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("列表 %d 行, 期望 2（每 provider 一行）", len(list))
	}
}

// TestAgentAccountUpdateClears（迭代 11 Phase 4）：UpdateAgentAccount 与
// UpsertAgentAccount 的空值语义**恰好相反**，这正是它存在的理由——Upsert 那条
// 路上空串恒等于「别动」（重登不抹管理员的配置），于是"把名字清空"在整个系统里
// 无从表达。这条路是显式赋值：空串就是清空。
//
// 顺带钉住它**不碰凭据与状态**：改个名不是重新登录，也不是启停。
func TestAgentAccountUpdateClears(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	acct := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	if err := s.SetAgentStatus(ctx, acct.ID, AgentStatusDisabled); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateAgentAccount(ctx, acct.ID, "", ""); err != nil {
		t.Fatalf("清空: %v", err)
	}
	got, err := s.GetAgentAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "" || got.DefaultModel != "" {
		t.Errorf("空串未清空: label=%q default_model=%q", got.Label, got.DefaultModel)
	}
	if got.Status != AgentStatusDisabled {
		t.Errorf("改名动了状态: %q", got.Status)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCodex); err != nil || plain != fakeAuthJSON {
		t.Errorf("改名动了凭据 (err=%v)", err)
	}

	if err := s.UpdateAgentAccount(ctx, acct.ID, "新名", "gpt-5-codex-mini"); err != nil {
		t.Fatalf("赋值: %v", err)
	}
	got, err = s.GetAgentAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "新名" || got.DefaultModel != "gpt-5-codex-mini" {
		t.Errorf("赋值未生效: label=%q default_model=%q", got.Label, got.DefaultModel)
	}

	if err := s.UpdateAgentAccount(ctx, acct.ID+999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的行应回 ErrNotFound, 得到 %v", err)
	}
}

// TestAgentAccountStatusAndDelete：状态只收封闭词汇表里的三值；删除后点查与
// 取令牌都回 ErrNotFound（代理据此回 agent_not_configured）。
func TestAgentAccountStatusAndDelete(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	acct := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)

	for _, status := range []string{AgentStatusDisabled, AgentStatusAuthExpired, AgentStatusActive} {
		if err := s.SetAgentStatus(ctx, acct.ID, status); err != nil {
			t.Fatalf("SetAgentStatus(%s): %v", status, err)
		}
		got, err := s.GetAgentAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("GetAgentAccount: %v", err)
		}
		if got.Status != status {
			t.Fatalf("状态 = %q, 期望 %q", got.Status, status)
		}
	}
	if err := s.SetAgentStatus(ctx, acct.ID, "expired"); err == nil {
		t.Error("词汇表外的状态应被拒")
	}
	if err := s.SetAgentStatus(ctx, acct.ID+999, AgentStatusDisabled); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的行应 ErrNotFound, 得到 %v", err)
	}

	// 停用的行照样取得到（状态分岔归调用方，不在 SQL 里滤）。
	if _, _, err := s.GetAgentCredential(ctx, AgentProviderCodex); err != nil {
		t.Fatalf("停用/失效的行仍应可读: %v", err)
	}

	if err := s.DeleteAgentAccount(ctx, acct.ID); err != nil {
		t.Fatalf("DeleteAgentAccount: %v", err)
	}
	if _, err := s.GetAgentAccount(ctx, acct.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后点查应 ErrNotFound, 得到 %v", err)
	}
	if _, _, err := s.GetAgentCredential(ctx, AgentProviderCodex); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后取凭据应 ErrNotFound, 得到 %v", err)
	}
	if err := s.DeleteAgentAccount(ctx, acct.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应 ErrNotFound, 得到 %v", err)
	}
}

// TestAgentAccountBadDeviceKey：设备密钥被换掉后，取凭据报可读错误而不是 panic
// 或空串，且错误里两侧都不回显；管理视图不受影响——那一行的状态仍该看得见，
// 管理员才知道要去重新连接（同上游 Key 列表视图留空 last4 的先例）。
func TestAgentAccountBadDeviceKey(t *testing.T) {
	s, dir := mustOpen(t)

	acct := mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	var sealed string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE id = ?`, acct.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 换一把等长的密钥：库照开，既有密文解不开。
	if err := os.WriteFile(filepath.Join(dir, DeviceKeyFileName), bytes.Repeat([]byte{0x5a}, deviceKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("换密钥后 Open 应成功: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	ctx := context.Background()

	got, authJSON, err := s2.GetAgentCredential(ctx, AgentProviderCodex)
	if err == nil {
		t.Fatalf("坏设备密钥仍解出了凭据: id=%d provider=%s, 明文 %d 字节",
			got.ID, got.Provider, len(authJSON))
	}
	if !errors.Is(err, ErrAgentAuthUnreadable) {
		t.Errorf("期望 ErrAgentAuthUnreadable, 得到 %v", err)
	}
	if !strings.Contains(err.Error(), DeviceKeyFileName) || !strings.Contains(err.Error(), "重新连接") {
		t.Errorf("错误未指明补救对象与动作: %v", err)
	}
	assertNoAgentSecretLeak(t, err, sealed)

	// 管理视图照常：这一行还在，状态还看得见。
	list, err := s2.ListAgentAccounts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("坏密钥不该影响管理视图: %d 行 (err=%v)", len(list), err)
	}
}

// fakeCursorKey / fakeCursorAuthJSON 是 cursor 行的假凭据（cursorauth 的规范
// 包装形态：整把 Dashboard API Key 收在 api_key 一个字段里）。
const (
	fakeCursorKey      = "fake-cursor-dashboard-api-key-NOT-REAL-0123456789"
	fakeCursorAuthJSON = `{"api_key":"` + fakeCursorKey + `"}`
)

// 存量库可能保存过 Cursor default_model；迁移必须把这个无执行语义的字段清空，
// 其余账号配置与凭据保持不动。
func TestCursorModelFreeMigration(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	acct, err := s.UpsertAgentAccount(ctx, NewAgentAccount{
		Provider: AgentProviderCursor,
		Label:    "Cursor 订阅",
		AuthJSON: fakeCursorAuthJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_accounts SET default_model = 'legacy-cursor-model' WHERE id = ?`, acct.ID); err != nil {
		t.Fatal(err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sqlText string
	for _, m := range migs {
		if m.version == 27 {
			sqlText = m.sql
			break
		}
	}
	if sqlText == "" {
		t.Fatal("缺少 0027 Cursor 模型清理迁移")
	}
	if _, err := s.db.ExecContext(ctx, sqlText); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAgentAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultModel != "" || got.Label != "Cursor 订阅" {
		t.Fatalf("迁移后 label=%q default_model=%q", got.Label, got.DefaultModel)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCursor); err != nil || plain != fakeCursorAuthJSON {
		t.Fatalf("迁移改坏凭据: err=%v", err)
	}
}

// TestAgentCursorSealAADAndSingleRow（迭代 3 Phase 1）：cursor 作为第四个
// provider 复用 agent_accounts 的两道既有防线，逐条对它复验——AAD 钉
// provider（cursor 的密文搬进别的 provider 行、别家的密文冒充 cursor，都解出
// 错误而不是值），以及 UNIQUE(provider) 的单账户覆盖语义。Cursor 的
// default_model 恒归零，因为透明代理没有模型配置语义。
func TestAgentCursorSealAADAndSingleRow(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	acct, err := s.UpsertAgentAccount(ctx, NewAgentAccount{
		Provider:     AgentProviderCursor,
		Label:        "Cursor 订阅",
		DefaultModel: "composer-1",
		AuthJSON:     fakeCursorAuthJSON,
	})
	if err != nil {
		t.Fatalf("UpsertAgentAccount(cursor): %v", err)
	}
	if acct.DefaultModel != "" {
		t.Fatalf("cursor default_model = %q，期望恒为空", acct.DefaultModel)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCursor); err != nil || plain != fakeCursorAuthJSON {
		t.Fatalf("cursor 凭据往返不一致 (err=%v)", err)
	}
	var sealedCursor string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE id = ?`, acct.ID).Scan(&sealedCursor); err != nil {
		t.Fatal(err)
	}
	if sealedCursor == "" || strings.Contains(sealedCursor, fakeCursorKey) {
		t.Fatalf("cursor 的 auth_json_sealed 未加密")
	}

	// cursor 的密文当 claude 的解：必须失败，而不是被当成 claude 凭据用。
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_accounts
		(provider, label, account_id, default_model, auth_json_sealed, status, created_at, updated_at)
		VALUES ('claude', '', '', '', ?, 'active', '2026-08-11T00:00:00.000Z', '2026-08-11T00:00:00.000Z')`,
		sealedCursor); err != nil {
		t.Fatal(err)
	}
	got, plain, err := s.GetAgentCredential(ctx, AgentProviderClaude)
	if err == nil {
		t.Fatalf("cursor 密文按 claude 解出了凭据: id=%d, 明文 %d 字节", got.ID, len(plain))
	}
	if !errors.Is(err, ErrAgentAuthUnreadable) {
		t.Errorf("期望 ErrAgentAuthUnreadable, 得到 %v", err)
	}
	assertNoAgentSecretLeak(t, err, sealedCursor)
	if strings.Contains(err.Error(), fakeCursorKey) {
		t.Errorf("错误回显了 cursor API Key: %v", err)
	}

	// 反向：codex 的密文塞进 cursor 行同样解不出。
	mustAgent(t, s, AgentProviderCodex, fakeAuthJSON)
	var sealedCodex string
	if err := s.db.QueryRow(`SELECT auth_json_sealed FROM agent_accounts WHERE provider = ?`, AgentProviderCodex).Scan(&sealedCodex); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_accounts SET auth_json_sealed = ? WHERE provider = ?`,
		sealedCodex, AgentProviderCursor); err != nil {
		t.Fatal(err)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCursor); !errors.Is(err, ErrAgentAuthUnreadable) {
		t.Errorf("codex 密文冒充 cursor 应解不开, 得到 %d 字节明文 (err=%v)", len(plain), err)
	}

	// 单账户语义：重复连接 cursor 是覆盖而非增行；空 label 保持，default_model 恒空。
	const rotated = `{"api_key":"fake-cursor-dashboard-api-key-NOT-REAL-rotated"}`
	second, err := s.UpsertAgentAccount(ctx, NewAgentAccount{Provider: AgentProviderCursor, AuthJSON: rotated})
	if err != nil {
		t.Fatalf("重复连接: %v", err)
	}
	if second.ID != acct.ID {
		t.Errorf("重复连接插出了新行: id %d → %d", acct.ID, second.ID)
	}
	if second.Label != "Cursor 订阅" || second.DefaultModel != "" {
		t.Errorf("重复连接后的字段不符: label=%q default_model=%q", second.Label, second.DefaultModel)
	}
	if _, plain, err := s.GetAgentCredential(ctx, AgentProviderCursor); err != nil || plain != rotated {
		t.Errorf("凭据未被新一代覆盖 (err=%v)", err)
	}
	list, err := s.ListAgentAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("列表 %d 行, 期望 3（cursor/claude/codex 各一行）", len(list))
	}
}

// assertNoAgentSecretLeak 断言错误串既不含 auth.json 明文片段也不含密文——
// 错误会被落日志，回显任一侧都是 §15.1 违规。
func assertNoAgentSecretLeak(t *testing.T, err error, sealed string) {
	t.Helper()
	msg := err.Error()
	for _, needle := range []string{
		"fake-access-token-NOT-REAL-0123456789",
		"fake-refresh-token-NOT-REAL-9876543210",
		"fake.id.token-NOT-REAL",
		"refresh_token",
	} {
		if strings.Contains(msg, needle) {
			t.Errorf("错误回显了 auth.json 明文片段 %q: %v", needle, err)
		}
	}
	if sealed != "" && strings.Contains(msg, sealed) {
		t.Errorf("错误回显了密文: %v", err)
	}
}
