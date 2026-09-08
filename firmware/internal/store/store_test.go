package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mustOpen 在临时目录建库并注册清理，返回可用 Store。
func mustOpen(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// mustKey 签一把 Key（明文按摘要派生，封存往返可核对），失败即终止测试。
func mustKey(t *testing.T, s *Store, digest string) *APIKey {
	t.Helper()
	k, err := s.CreateAPIKey(context.Background(), "test-label", digest, "sk_prefix12", "wxyz", "plain-"+digest)
	if err != nil {
		t.Fatalf("CreateAPIKey(digest=%s): %v", digest, err)
	}
	return k
}

// 迁移从零建库幂等：重复 Open 不改变 schema_migrations 的任何一行；
// 库文件 0600；PRAGMA journal_mode=WAL、foreign_keys=ON、busy_timeout=5000 生效。
func TestOpenMigratesIdempotently(t *testing.T) {
	s, dir := mustOpen(t)

	dbPath := filepath.Join(dir, "llmgate.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("库文件不存在: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("库文件权限 = %o, 期望 0600", got)
	}

	readMigrations := func(s *Store) map[int64]string {
		t.Helper()
		rows, err := s.db.Query(`SELECT version, applied_at FROM schema_migrations`)
		if err != nil {
			t.Fatalf("查询 schema_migrations: %v", err)
		}
		defer rows.Close()
		got := map[int64]string{}
		for rows.Next() {
			var v int64
			var at string
			if err := rows.Scan(&v, &at); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if _, err := time.Parse(timeLayout, at); err != nil {
				t.Errorf("applied_at %q 非法: %v", at, err)
			}
			got[v] = at
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return got
	}

	first := readMigrations(s)
	if _, ok := first[1]; !ok {
		t.Fatalf("schema_migrations 缺少版本 1, got %v", first)
	}

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q (err=%v), 期望 wal", mode, err)
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d (err=%v), 期望 1", fk, err)
	}
	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
		t.Errorf("busy_timeout = %d (err=%v), 期望 5000", busy, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重复 Open: %v", err)
	}
	defer s2.Close()
	second := readMigrations(s2)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("重复 Open 变更了 schema_migrations: %v → %v", first, second)
	}
}

// Open 不创建父目录：目录不存在必须报错（部署职责边界）。
func TestOpenRequiresExistingDataDir(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Fatal("父目录不存在时 Open 应报错")
	}
}

// 外键约束生效：孤儿 model_source 插入报错。
// （api_keys / sessions 的外键随 users 表在 0019 一起退场，模型来源是现在
// 库里仅剩的外键面。）
func TestForeignKeyEnforced(t *testing.T) {
	s, _ := mustOpen(t)
	m := mustModel(t, s, "fk-probe")
	if _, err := s.CreateModelSource(context.Background(), m.ID, 9999, "up-model", 100); err == nil {
		t.Fatal("孤儿 model_source（upstream_id 不存在）插入应报错")
	}
}

// 0021 在现有 api_keys 表上只追加按量额度列：存量密钥与限额原样保留，
// 新列从 0 起步，迁移后可立即走鉴权与管理调整。
func TestMigration0021PreservesExistingAPIKeys(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 20)
	if _, err := db.Exec(`
		INSERT INTO api_keys (
			id, label, key_digest, display_prefix, display_last4, disabled, created_at,
			budget_day_micro, budget_week_micro, budget_month_micro, rpm_limit, plaintext_sealed
		) VALUES (7, 'existing', 'digest-existing', 'sk_existing', 'ting', 0, ?, 100, 200, 300, 4, '')`,
		legacyTS); err != nil {
		t.Fatalf("写入 0020 存量密钥: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭 0020 存量库: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("迁移存量库: %v", err)
	}
	defer s.Close()
	k, err := s.GetAPIKeyByID(context.Background(), 7)
	if err != nil {
		t.Fatalf("读取迁移后密钥: %v", err)
	}
	if k.Label != "existing" || k.MeteredAllowanceMicro != 0 ||
		k.BudgetDayMicro == nil || *k.BudgetDayMicro != 100 || k.RPMLimit == nil || *k.RPMLimit != 4 {
		t.Fatalf("迁移后密钥失真: %+v", k)
	}
	if remaining, err := s.AdjustAPIKeyMeteredAllowance(context.Background(), 7, 500); err != nil || remaining != 500 {
		t.Fatalf("迁移后调整按量额度 = (%d, %v)，期望 500", remaining, err)
	}
	var applied int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 21`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("schema_migrations 缺版本 21 (n=%d, err=%v)", applied, err)
	}
}

// api_keys CRUD 往返：创建/列表/禁用/last_used_at；摘要重复返回 ErrConflict。
func TestAPIKeyCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	k1 := mustKey(t, s, "digest-1")
	mustKey(t, s, "digest-2")

	if k1.Label != "test-label" || k1.DisplayPrefix != "sk_prefix12" || k1.DisplayLast4 != "wxyz" || k1.Disabled || !k1.LastUsedAt.IsZero() {
		t.Errorf("CreateAPIKey 返回不完整: %+v", k1)
	}
	if _, err := s.CreateAPIKey(ctx, "dup", "digest-1", "p", "l4", "plain-dup"); !errors.Is(err, ErrConflict) {
		t.Errorf("摘要重复应返回 ErrConflict, got %v", err)
	}

	all, err := s.ListAPIKeys(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAPIKeys = %d 把 (err=%v), 期望 2", len(all), err)
	}

	if err := s.SetAPIKeyDisabled(ctx, k1.ID, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	if got, _ := s.ListAPIKeys(ctx); len(got) != 2 || !got[0].Disabled || got[1].Disabled {
		t.Error("禁用后只有 k1 的 Disabled 应为 true")
	}
	if err := s.SetAPIKeyLabel(ctx, k1.ID, "renamed"); err != nil {
		t.Fatalf("SetAPIKeyLabel: %v", err)
	}
	if got, err := s.GetAPIKeyByID(ctx, k1.ID); err != nil || got.Label != "renamed" {
		t.Fatalf("更新标签后读回 = %+v (err=%v)", got, err)
	}
	if err := s.SetAPIKeyLabel(ctx, k1.ID, ""); err != nil {
		t.Fatalf("清除标签: %v", err)
	}
	if got, err := s.GetAPIKeyByID(ctx, k1.ID); err != nil || got.Label != "" {
		t.Fatalf("清除标签后读回 = %+v (err=%v)", got, err)
	}
	if err := s.SetAPIKeyLabel(ctx, 9999, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("更新不存在 Key 标签应返回 ErrNotFound, got %v", err)
	}
	if err := s.SetAPIKeyDisabled(ctx, 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("禁用不存在 Key 应返回 ErrNotFound, got %v", err)
	}

	if err := s.TouchKeyLastUsed(ctx, k1.ID); err != nil {
		t.Fatalf("TouchKeyLastUsed: %v", err)
	}
	if got, _ := s.ListAPIKeys(ctx); len(got) != 2 || got[0].LastUsedAt.IsZero() {
		t.Error("TouchKeyLastUsed 后 LastUsedAt 应非零")
	}

	one, err := s.GetAPIKeyByID(ctx, k1.ID)
	if err != nil || one.KeyDigest != "digest-1" || !one.Disabled {
		t.Fatalf("GetAPIKeyByID 往返不一致 (err=%v): %+v", err, one)
	}
	if _, err := s.GetAPIKeyByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 id 应返回 ErrNotFound, got %v", err)
	}

	if err := s.DeleteAPIKey(ctx, k1.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, err := s.LookupKeyByDigest(ctx, "digest-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后摘要点查应落空, got %v", err)
	}
	if err := s.DeleteAPIKey(ctx, k1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound, got %v", err)
	}
}

// 封存明文四态（0012）：往返一致 / 旧行未留存 / 密文损坏 / 密文挪行解不开
// （AAD 钉摘要的绊线）。列表与点查只带 PlaintextAvailable 布尔，不带密文。
func TestAPIKeyPlaintextRoundtrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	k1 := mustKey(t, s, "digest-r1")
	k2 := mustKey(t, s, "digest-r2")

	// 往返：创建返回、列表与点查都报可复制，解封回到同一明文。
	if !k1.PlaintextAvailable {
		t.Error("CreateAPIKey 返回的 PlaintextAvailable 应为 true")
	}
	if got, err := s.GetAPIKeyByID(ctx, k1.ID); err != nil || !got.PlaintextAvailable {
		t.Errorf("GetAPIKeyByID PlaintextAvailable = %+v (err=%v), 期望 true", got, err)
	}
	if plain, err := s.GetAPIKeyPlaintext(ctx, k1.ID); err != nil || plain != "plain-digest-r1" {
		t.Errorf("GetAPIKeyPlaintext = %q (err=%v), 期望 plain-digest-r1", plain, err)
	}
	if _, err := s.GetAPIKeyPlaintext(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 id 应返回 ErrNotFound, got %v", err)
	}

	// 旧行（0012 之前签发）：plaintext_sealed 恒空串 = 未留存。
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET plaintext_sealed = '' WHERE id = ?`, k1.ID); err != nil {
		t.Fatalf("清空封存明文: %v", err)
	}
	if _, err := s.GetAPIKeyPlaintext(ctx, k1.ID); !errors.Is(err, ErrKeyPlaintextMissing) {
		t.Errorf("旧行应返回 ErrKeyPlaintextMissing, got %v", err)
	}
	if got, err := s.GetAPIKeyByID(ctx, k1.ID); err != nil || got.PlaintextAvailable {
		t.Errorf("旧行 PlaintextAvailable 应为 false (err=%v): %+v", err, got)
	}
	if all, err := s.ListAPIKeys(ctx); err != nil || len(all) != 2 ||
		all[0].PlaintextAvailable || !all[1].PlaintextAvailable {
		t.Errorf("列表 PlaintextAvailable 应为 [false true] (err=%v): %+v", err, all)
	}

	// 密文损坏：解不开返回 ErrKeyPlaintextUnreadable，不 panic、不回密文。
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET plaintext_sealed = 'not!base64' WHERE id = ?`, k1.ID); err != nil {
		t.Fatalf("写入坏密文: %v", err)
	}
	if _, err := s.GetAPIKeyPlaintext(ctx, k1.ID); !errors.Is(err, ErrKeyPlaintextUnreadable) {
		t.Errorf("坏密文应返回 ErrKeyPlaintextUnreadable, got %v", err)
	}

	// 密文挪行：k2 的合法密文抄给 k1（摘要不同），AAD 不匹配必须解不开。
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET plaintext_sealed =
		(SELECT plaintext_sealed FROM api_keys WHERE id = ?) WHERE id = ?`, k2.ID, k1.ID); err != nil {
		t.Fatalf("挪动密文: %v", err)
	}
	if _, err := s.GetAPIKeyPlaintext(ctx, k1.ID); !errors.Is(err, ErrKeyPlaintextUnreadable) {
		t.Errorf("挪行密文应返回 ErrKeyPlaintextUnreadable, got %v", err)
	}
	// 原行不受影响。
	if plain, err := s.GetAPIKeyPlaintext(ctx, k2.ID); err != nil || plain != "plain-digest-r2" {
		t.Errorf("k2 明文应完好 = %q (err=%v)", plain, err)
	}
}

// LookupKeyByDigest 三态：命中 / 未命中 / 已禁用。限额与按量额度随同一次点查带回。
func TestLookupKeyByDigestThreeStates(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	k := mustKey(t, s, "digest-live")

	// 命中。
	got, err := s.LookupKeyByDigest(ctx, "digest-live")
	if err != nil {
		t.Fatalf("LookupKeyByDigest 命中态: %v", err)
	}
	if got.KeyID != k.ID || got.KeyDisplay != "sk_prefix12…wxyz" || got.KeyDisabled {
		t.Errorf("命中态字段不对: %+v", got)
	}
	if got.KeyBudgetDayMicro != nil || got.KeyRPMLimit != nil || got.KeyMeteredAllowanceMicro != 0 {
		t.Errorf("新签密钥应无限额: %+v", got)
	}

	// 未命中。
	if _, err := s.LookupKeyByDigest(ctx, "digest-miss"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未命中应返回 ErrNotFound, got %v", err)
	}

	// 已禁用：仍返回行，KeyDisabled=true（是否 401 由调用方裁决）。
	if err := s.SetAPIKeyDisabled(ctx, k.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, err = s.LookupKeyByDigest(ctx, "digest-live"); err != nil || !got.KeyDisabled {
		t.Errorf("禁用态 = %+v (err=%v), 期望 KeyDisabled=true", got, err)
	}

	// 限额随点查带回（同一条 SQL，准入因此零额外查询）。
	day, rpm := int64(1_000_000), int64(30)
	if err := s.SetAPIKeyLimits(ctx, k.ID, &day, nil, nil, &rpm); err != nil {
		t.Fatal(err)
	}
	got, err = s.LookupKeyByDigest(ctx, "digest-live")
	if err != nil || got.KeyBudgetDayMicro == nil || *got.KeyBudgetDayMicro != day ||
		got.KeyBudgetWeekMicro != nil || got.KeyRPMLimit == nil || *got.KeyRPMLimit != rpm {
		t.Errorf("限额未随点查带回 = %+v (err=%v)", got, err)
	}
}

// 按量额度以 api_keys 列为唯一权威：管理员增减与计量批量扣减都原子作用在
// 剩余额上，扣穿钳 0，删除后的密钥在批量扣减中静默跳过。
func TestAPIKeyMeteredAllowanceAdjustAndDrain(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	k := mustKey(t, s, "digest-allowance")

	remaining, err := s.AdjustAPIKeyMeteredAllowance(ctx, k.ID, 5_000_000)
	if err != nil || remaining != 5_000_000 {
		t.Fatalf("增加按量额度 = (%d, %v)，期望 5000000", remaining, err)
	}
	got, err := s.GetAPIKeyByID(ctx, k.ID)
	if err != nil || got.MeteredAllowanceMicro != 5_000_000 {
		t.Fatalf("按量额度读回 = %+v (err=%v)", got, err)
	}
	auth, err := s.LookupKeyByDigest(ctx, "digest-allowance")
	if err != nil || auth.KeyMeteredAllowanceMicro != 5_000_000 {
		t.Fatalf("鉴权热路径未带回按量额度 = %+v (err=%v)", auth, err)
	}

	if err := s.DrainAPIKeyMeteredAllowances(ctx, map[int64]int64{k.ID: 2_000_000, 9999: 1}); err != nil {
		t.Fatalf("批量扣减: %v", err)
	}
	if got, _ = s.GetAPIKeyByID(ctx, k.ID); got.MeteredAllowanceMicro != 3_000_000 {
		t.Errorf("扣减后剩余 = %d，期望 3000000", got.MeteredAllowanceMicro)
	}
	if err := s.DrainAPIKeyMeteredAllowances(ctx, map[int64]int64{k.ID: 9_000_000}); err != nil {
		t.Fatalf("扣穿: %v", err)
	}
	if got, _ = s.GetAPIKeyByID(ctx, k.ID); got.MeteredAllowanceMicro != 0 {
		t.Errorf("扣穿后应钳 0，实际 %d", got.MeteredAllowanceMicro)
	}
	if remaining, err = s.AdjustAPIKeyMeteredAllowance(ctx, k.ID, -1); err != nil || remaining != 0 {
		t.Errorf("从 0 继续扣减应仍为 0 = (%d, %v)", remaining, err)
	}
	if remaining, err = s.AdjustAPIKeyMeteredAllowance(ctx, k.ID, math.MaxInt64); err != nil || remaining != math.MaxInt64 {
		t.Fatalf("增加到 MaxInt64 = (%d, %v)", remaining, err)
	}
	if remaining, err = s.AdjustAPIKeyMeteredAllowance(ctx, k.ID, 1); err != nil || remaining != math.MaxInt64 {
		t.Errorf("正向上溢应钳 MaxInt64 = (%d, %v)", remaining, err)
	}
	if remaining, err = s.AdjustAPIKeyMeteredAllowance(ctx, k.ID, math.MinInt64); err != nil || remaining != 0 {
		t.Errorf("负向扣穿应钳 0 = (%d, %v)", remaining, err)
	}
	if _, err := s.AdjustAPIKeyMeteredAllowance(ctx, 9999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("调整不存在密钥应返回 ErrNotFound，实际 %v", err)
	}
	if err := s.DrainAPIKeyMeteredAllowances(ctx, nil); err != nil {
		t.Errorf("空批应为空操作: %v", err)
	}
}

// sessions CRUD 往返：创建/按摘要取/删单条/清空/删过期。
func TestSessionCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	sess, err := s.CreateSession(ctx, "tok-1", future, "192.168.50.5")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.RemoteIP != "192.168.50.5" || sess.CreatedAt.IsZero() {
		t.Errorf("CreateSession 返回不完整: %+v", sess)
	}
	// 过期时间毫秒精度往返。
	if got := sess.ExpiresAt.Sub(future).Abs(); got > time.Millisecond {
		t.Errorf("ExpiresAt 偏差 %v, 期望 ≤1ms", got)
	}
	if _, err := s.CreateSession(ctx, "tok-1", future, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("会话摘要重复应返回 ErrConflict, got %v", err)
	}

	got, err := s.GetSessionByTokenDigest(ctx, "tok-1")
	if err != nil || got.ID != sess.ID {
		t.Fatalf("GetSessionByTokenDigest = %+v (err=%v)", got, err)
	}
	if _, err := s.GetSessionByTokenDigest(ctx, "tok-miss"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知会话应返回 ErrNotFound, got %v", err)
	}

	if err := s.DeleteSession(ctx, "tok-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSessionByTokenDigest(ctx, "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Error("删除后会话应不可见")
	}
	if err := s.DeleteSession(ctx, "tok-1"); err != nil {
		t.Errorf("重复删除应幂等无错, got %v", err)
	}

	// 改密踢会话场景的底座：清空全部会话（口令换了，所有浏览器都得重验）。
	for _, tok := range []string{"tok-a1", "tok-a2", "tok-b1"} {
		if _, err := s.CreateSession(ctx, tok, future, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAllSessions(ctx); err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}
	for _, tok := range []string{"tok-a1", "tok-a2", "tok-b1"} {
		if _, err := s.GetSessionByTokenDigest(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteAllSessions 后 %s 仍在", tok)
		}
	}

	// 过期清理：只删已过期的。
	if _, err := s.CreateSession(ctx, "tok-old", past, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "tok-live", future, ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredSessions(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d (err=%v), 期望 1", n, err)
	}
	if _, err := s.GetSessionByTokenDigest(ctx, "tok-old"); !errors.Is(err, ErrNotFound) {
		t.Error("过期会话应被删除")
	}
	if _, err := s.GetSessionByTokenDigest(ctx, "tok-live"); err != nil {
		t.Errorf("未过期会话不应被删除, got %v", err)
	}
}

// 审计仅追加：AppendAudit 落行可查；At 零值自动补当前时间。
func TestAuditAppend(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	if err := s.AppendAudit(ctx, AuditEvent{
		Event:    "login.success",
		Entity:   "device:password",
		Detail:   "test-detail",
		RemoteIP: "192.168.50.5",
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if err := s.AppendAudit(ctx, AuditEvent{Event: "login.failure", RemoteIP: "192.168.50.6"}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	rows, err := s.db.Query(`SELECT at, event, entity, detail, remote_ip FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 audit_events: %v", err)
	}
	defer rows.Close()
	type row struct {
		at, event, entity, detail, ip string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.at, &r.event, &r.entity, &r.detail, &r.ip); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("审计行数 = %d, 期望 2", len(got))
	}
	if got[0].event != "login.success" || got[0].entity != "device:password" ||
		got[0].detail != "test-detail" || got[0].ip != "192.168.50.5" {
		t.Errorf("第 1 行审计不对: %+v", got[0])
	}
	if got[1].event != "login.failure" || got[1].entity != "" {
		t.Errorf("第 2 行审计不对: %+v", got[1])
	}
	for _, r := range got {
		if _, err := time.Parse(timeLayout, r.at); err != nil {
			t.Errorf("审计 at %q 非法: %v", r.at, err)
		}
	}
}

// audit_events 不再有 actor 列（0019：设备只有一个操作者，「谁做的」恒等于
// 同一个答案）。这是一条绊线——顺手把它加回来就会在这里断。
func TestAuditHasNoActorColumns(t *testing.T) {
	s, _ := mustOpen(t)
	rows, err := s.db.Query(`PRAGMA table_info(audit_events)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(name, "actor") {
			t.Errorf("audit_events 不该再有 actor 列，发现 %q", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// 审计 API 形状约束：Store 上除 AppendAudit 外不得有任何含 "Audit" 的方法
// （无更新/删除路径——追加式审计的可执行验收）。
func TestAuditAPIIsAppendOnly(t *testing.T) {
	typ := reflect.TypeOf(&Store{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if strings.Contains(name, "Audit") && name != "AppendAudit" {
			t.Errorf("审计必须只追加：不允许存在方法 %s", name)
		}
	}
}

// ---- iteration-5 Phase 1：上游/模型/来源三表 ----

// upstreamTypeMock 等常量刻意在测试里重述而不 import config：store 不依赖
// config，两处枚举同步靠 0002 迁移的 CHECK 约束注释约定。
const (
	upstreamTypeDeepseek = "deepseek"
	upstreamTypeArk      = "ark"
	upstreamTypeArkPlan  = "ark_plan"
	upstreamTypeQwenPlan = "qwen_plan"
	upstreamTypeOpenCode = "opencode_go"
	upstreamTypeMock     = "mock"
)

// mustUpstream 建一个上游，失败即终止测试。
func mustUpstream(t *testing.T, s *Store, name, typ, apiKey, baseURL string) *Upstream {
	t.Helper()
	u, err := s.CreateUpstream(context.Background(), name, typ, apiKey, baseURL)
	if err != nil {
		t.Fatalf("CreateUpstream(%s): %v", name, err)
	}
	return u
}

// mustModel 建一个文本模型（既有测试的缺省 kind），失败即终止测试。
func mustModel(t *testing.T, s *Store, name string) *Model {
	t.Helper()
	m, err := s.CreateModel(context.Background(), name, ModelKindText, "")
	if err != nil {
		t.Fatalf("CreateModel(%s): %v", name, err)
	}
	return m
}

// mustSource 给模型挂一条来源，失败即终止测试。
func mustSource(t *testing.T, s *Store, modelID, upstreamID int64, upstreamModelID string, priority int64) *ModelSource {
	t.Helper()
	src, err := s.CreateModelSource(context.Background(), modelID, upstreamID, upstreamModelID, priority)
	if err != nil {
		t.Fatalf("CreateModelSource(model=%d upstream=%d): %v", modelID, upstreamID, err)
	}
	return src
}

// upstreams CRUD 往返：创建/列表/按 id 取/改名与 base_url/启停/换 Key/删除；
// 重名与非法 type 被拒；管理视图只暴露末 4 位。
func TestUpstreamCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-deepseek-plaintext-0001", "")
	if ds.ID <= 0 || ds.Type != upstreamTypeDeepseek || ds.Disabled || ds.CreatedAt.IsZero() {
		t.Errorf("CreateUpstream 返回不完整: %+v", ds)
	}
	if ds.APIKeyLast4 != "0001" {
		t.Errorf("APIKeyLast4 = %q, 期望 0001", ds.APIKeyLast4)
	}
	mock := mustUpstream(t, s, "mock", upstreamTypeMock, "", "http://127.0.0.1:18080/v1")
	if mock.APIKeyLast4 != "" {
		t.Errorf("无凭证上游的 last4 应为空, got %q", mock.APIKeyLast4)
	}

	if _, err := s.CreateUpstream(ctx, "ds", upstreamTypeArk, "k", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("重名上游应返回 ErrConflict, got %v", err)
	}
	if _, err := s.CreateUpstream(ctx, "bad", "openai", "k", ""); err == nil {
		t.Error("非法 type 应被 CHECK 约束拒绝")
	}

	ups, err := s.ListUpstreams(ctx)
	if err != nil || len(ups) != 2 {
		t.Fatalf("ListUpstreams = %d 个 (err=%v), 期望 2", len(ups), err)
	}
	if ups[0].Name != "ds" || ups[1].Name != "mock" {
		t.Errorf("ListUpstreams 应按 id 升序, got %s/%s", ups[0].Name, ups[1].Name)
	}
	if ups[1].BaseURL != "http://127.0.0.1:18080/v1" {
		t.Errorf("base_url 往返不一致: %q", ups[1].BaseURL)
	}

	got, err := s.GetUpstreamByID(ctx, ds.ID)
	if err != nil || got.Name != "ds" || got.APIKeyLast4 != "0001" {
		t.Fatalf("GetUpstreamByID = %+v (err=%v)", got, err)
	}
	if _, err := s.GetUpstreamByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知上游应返回 ErrNotFound, got %v", err)
	}

	if err := s.UpdateUpstream(ctx, ds.ID, "ds-main", "http://override"); err != nil {
		t.Fatalf("UpdateUpstream: %v", err)
	}
	if got, _ = s.GetUpstreamByID(ctx, ds.ID); got.Name != "ds-main" || got.BaseURL != "http://override" {
		t.Errorf("改名/改 base_url 未生效: %+v", got)
	}
	if got.APIKeyLast4 != "0001" {
		t.Error("UpdateUpstream 不应动凭证")
	}
	if err := s.UpdateUpstream(ctx, mock.ID, "ds-main", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("改成已存在的名字应返回 ErrConflict, got %v", err)
	}
	if err := s.UpdateUpstream(ctx, 9999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在上游应返回 ErrNotFound, got %v", err)
	}

	if err := s.SetUpstreamDisabled(ctx, ds.ID, true); err != nil {
		t.Fatalf("SetUpstreamDisabled: %v", err)
	}
	if got, _ = s.GetUpstreamByID(ctx, ds.ID); !got.Disabled {
		t.Error("停用后 Disabled 应为 true")
	}
	if err := s.SetUpstreamDisabled(ctx, 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("停用不存在上游应返回 ErrNotFound, got %v", err)
	}

	if err := s.SetUpstreamKey(ctx, ds.ID, "sk-deepseek-rotated-9876"); err != nil {
		t.Fatalf("SetUpstreamKey: %v", err)
	}
	if got, _ = s.GetUpstreamByID(ctx, ds.ID); got.APIKeyLast4 != "9876" {
		t.Errorf("换 Key 后 last4 = %q, 期望 9876", got.APIKeyLast4)
	}
	if err := s.SetUpstreamKey(ctx, 9999, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("给不存在上游换 Key 应返回 ErrNotFound, got %v", err)
	}

	if err := s.DeleteUpstream(ctx, mock.ID); err != nil {
		t.Fatalf("DeleteUpstream: %v", err)
	}
	if _, err := s.GetUpstreamByID(ctx, mock.ID); !errors.Is(err, ErrNotFound) {
		t.Error("删除后上游应不可见")
	}
	if err := s.DeleteUpstream(ctx, mock.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound, got %v", err)
	}
}

// models / model_sources CRUD 往返 + 唯一约束 + 级联删 + 被引用上游禁止删。
func TestModelAndSourceCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-ds-plaintext-1111", "")
	mock := mustUpstream(t, s, "mock", upstreamTypeMock, "", "http://127.0.0.1:18080/v1")

	m := mustModel(t, s, "deepseek-chat")
	if m.ID <= 0 || m.Disabled || m.CreatedAt.IsZero() {
		t.Errorf("CreateModel 返回不完整: %+v", m)
	}
	if _, err := s.CreateModel(ctx, "deepseek-chat", ModelKindText, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("重名模型应返回 ErrConflict, got %v", err)
	}
	if got, err := s.GetModelByID(ctx, m.ID); err != nil || got.Name != "deepseek-chat" {
		t.Fatalf("GetModelByID = %+v (err=%v)", got, err)
	}
	if _, err := s.GetModelByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知模型应返回 ErrNotFound, got %v", err)
	}

	src1 := mustSource(t, s, m.ID, ds.ID, "", 100)
	if src1.UpstreamModelID != "" || src1.Priority != 100 || src1.Disabled {
		t.Errorf("CreateModelSource 返回不完整: %+v", src1)
	}
	src2 := mustSource(t, s, m.ID, mock.ID, "mock-chat", 200)

	// UNIQUE(model_id, upstream_id)：同一模型不得在同一上游挂两条来源。
	if _, err := s.CreateModelSource(ctx, m.ID, ds.ID, "other", 300); !errors.Is(err, ErrConflict) {
		t.Errorf("(model, upstream) 重复应返回 ErrConflict, got %v", err)
	}
	// 外键：引用不存在的模型/上游同样是 ErrConflict（mapErr 映射 FK 违反）。
	if _, err := s.CreateModelSource(ctx, 9999, ds.ID, "", 100); !errors.Is(err, ErrConflict) {
		t.Errorf("引用不存在的模型应返回 ErrConflict, got %v", err)
	}
	if _, err := s.CreateModelSource(ctx, m.ID, 9999, "", 100); !errors.Is(err, ErrConflict) {
		t.Errorf("引用不存在的上游应返回 ErrConflict, got %v", err)
	}

	if got, err := s.GetModelSourceByID(ctx, src2.ID); err != nil || got.UpstreamModelID != "mock-chat" || got.Priority != 200 {
		t.Fatalf("GetModelSourceByID = %+v (err=%v)", got, err)
	}
	if _, err := s.GetModelSourceByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知来源应返回 ErrNotFound, got %v", err)
	}

	if err := s.UpdateModelSource(ctx, src1.ID, "deepseek-chat-v2", 50); err != nil {
		t.Fatalf("UpdateModelSource: %v", err)
	}
	if got, _ := s.GetModelSourceByID(ctx, src1.ID); got.UpstreamModelID != "deepseek-chat-v2" || got.Priority != 50 {
		t.Errorf("改来源未生效: %+v", got)
	}
	if err := s.UpdateModelSource(ctx, 9999, "x", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在来源应返回 ErrNotFound, got %v", err)
	}

	if err := s.SetModelSourceDisabled(ctx, src2.ID, true); err != nil {
		t.Fatalf("SetModelSourceDisabled: %v", err)
	}
	if got, _ := s.GetModelSourceByID(ctx, src2.ID); !got.Disabled {
		t.Error("停用来源后 Disabled 应为 true")
	}
	if err := s.SetModelSourceDisabled(ctx, 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("停用不存在来源应返回 ErrNotFound, got %v", err)
	}

	if err := s.SetModelDisabled(ctx, m.ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}
	if got, _ := s.GetModelByID(ctx, m.ID); !got.Disabled {
		t.Error("停用模型后 Disabled 应为 true")
	}
	if err := s.SetModelDisabled(ctx, 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("停用不存在模型应返回 ErrNotFound, got %v", err)
	}

	if err := s.RenameModel(ctx, m.ID, "deepseek-chat-renamed"); err != nil {
		t.Fatalf("RenameModel: %v", err)
	}
	if got, _ := s.GetModelByID(ctx, m.ID); got.Name != "deepseek-chat-renamed" {
		t.Error("改名未生效")
	}
	other := mustModel(t, s, "other-model")
	if err := s.RenameModel(ctx, other.ID, "deepseek-chat-renamed"); !errors.Is(err, ErrConflict) {
		t.Errorf("改成已存在的模型名应返回 ErrConflict, got %v", err)
	}
	if err := s.RenameModel(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在模型应返回 ErrNotFound, got %v", err)
	}

	// 被来源引用的上游不得删（外键 RESTRICT）→ ErrConflict + 可读原因。
	err := s.DeleteUpstream(ctx, ds.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("删被引用的上游应返回 ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "仍被模型来源引用") {
		t.Errorf("被引用上游的删除错误应可读, got %q", err.Error())
	}

	// 删来源后即可删上游。
	if err := s.DeleteModelSource(ctx, src1.ID); err != nil {
		t.Fatalf("DeleteModelSource: %v", err)
	}
	if _, err := s.GetModelSourceByID(ctx, src1.ID); !errors.Is(err, ErrNotFound) {
		t.Error("删除后来源应不可见")
	}
	if err := s.DeleteModelSource(ctx, src1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删来源应返回 ErrNotFound, got %v", err)
	}
	if err := s.DeleteUpstream(ctx, ds.ID); err != nil {
		t.Fatalf("来源已删，上游应可删: %v", err)
	}

	// 删模型级联删其全部来源（src2 仍挂在 mock 上）。
	if err := s.DeleteModel(ctx, m.ID); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := s.GetModelSourceByID(ctx, src2.ID); !errors.Is(err, ErrNotFound) {
		t.Error("删模型应级联删掉它的来源")
	}
	if err := s.DeleteModel(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删模型应返回 ErrNotFound, got %v", err)
	}
	// 级联删掉引用后，mock 上游可删——反证级联真的落到了库里。
	if err := s.DeleteUpstream(ctx, mock.ID); err != nil {
		t.Fatalf("级联删来源后 mock 上游应可删: %v", err)
	}
}

// ResolveModelRoute：按 (priority, 订阅先行, id) 排序（本例无订阅型上游，
// 即退化为 (priority, id)；订阅裁决位见 TestSourceOrderSubscriptionFirst）、
// 带齐来源/上游全字段与三层 disabled 位、upstream_model_id 空串解析为
// 模型名、模型不存在 → ErrNotFound。
func TestResolveModelRoute(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const dsKey = "sk-ds-route-plaintext-2222"
	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, dsKey, "")
	mock := mustUpstream(t, s, "mock", upstreamTypeMock, "", "http://127.0.0.1:18080/v1")
	ark := mustUpstream(t, s, "ark", upstreamTypeArk, "sk-ark-route-plaintext-3333", "")

	m := mustModel(t, s, "deepseek-chat")
	// 刻意乱序创建：低优先级先建，高优先级后建，验证排序不是靠 id。
	srcMock := mustSource(t, s, m.ID, mock.ID, "mock-chat", 200)
	srcDS := mustSource(t, s, m.ID, ds.ID, "", 100)
	// 同优先级（200）的第二条：与 srcMock 同级，按 id（创建序）排在其后。
	srcArk := mustSource(t, s, m.ID, ark.ID, "ark-chat", 200)

	route, err := s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if route.Model.ID != m.ID || route.Model.Name != "deepseek-chat" || route.Model.Disabled {
		t.Errorf("路由模型行不对: %+v", route.Model)
	}
	if len(route.Candidates) != 3 {
		t.Fatalf("候选数 = %d, 期望 3", len(route.Candidates))
	}
	wantOrder := []int64{srcDS.ID, srcMock.ID, srcArk.ID}
	for i, want := range wantOrder {
		if route.Candidates[i].SourceID != want {
			t.Errorf("候选[%d].SourceID = %d, 期望 %d（(priority, id) 序）", i, route.Candidates[i].SourceID, want)
		}
	}

	// 第一候选：来源侧 ID 为空 → 解析成模型名；上游全字段齐备，凭证已解密。
	c0 := route.Candidates[0]
	if c0.Priority != 100 || c0.UpstreamModelID != "deepseek-chat" {
		t.Errorf("候选[0] = %+v, 期望 priority=100、upstream_model_id 解析为模型名", c0)
	}
	if c0.Upstream.ID != ds.ID || c0.Upstream.Name != "ds" || c0.Upstream.Type != upstreamTypeDeepseek {
		t.Errorf("候选[0].Upstream 字段不全: %+v", c0.Upstream)
	}
	if c0.Upstream.APIKey != dsKey {
		t.Errorf("候选[0] 凭证未解密回明文: %q", c0.Upstream.APIKey)
	}
	if c0.Upstream.BaseURL != "" || c0.SourceDisabled || c0.Upstream.Disabled {
		t.Errorf("候选[0] 初始态不对: %+v", c0)
	}
	// 第二候选：显式来源侧 ID 原样带出，base_url 覆盖带出。
	if c1 := route.Candidates[1]; c1.UpstreamModelID != "mock-chat" || c1.Upstream.BaseURL != "http://127.0.0.1:18080/v1" {
		t.Errorf("候选[1] = %+v, 期望带出显式 upstream_model_id 与 base_url", c1)
	}

	// 三层 disabled 位都要如实带出（由调用方裁决口径，store 不做过滤）。
	if err := s.SetModelSourceDisabled(ctx, srcMock.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpstreamDisabled(ctx, ark.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelDisabled(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}
	route, err = s.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute（停用后）: %v", err)
	}
	if !route.Model.Disabled {
		t.Error("模型 disabled 位未带出")
	}
	if len(route.Candidates) != 3 {
		t.Fatalf("停用不应过滤候选，候选数 = %d, 期望 3", len(route.Candidates))
	}
	if route.Candidates[0].SourceDisabled || route.Candidates[0].Upstream.Disabled {
		t.Error("未停用的候选不应带 disabled 位")
	}
	if !route.Candidates[1].SourceDisabled {
		t.Error("来源 disabled 位未带出")
	}
	if !route.Candidates[2].Upstream.Disabled {
		t.Error("上游 disabled 位未带出")
	}

	// 模型存在但没挂来源：返回模型行 + 空候选（不是 ErrNotFound）。
	bare := mustModel(t, s, "bare-model")
	route, err = s.ResolveModelRoute(ctx, "bare-model")
	if err != nil {
		t.Fatalf("无来源模型应正常返回: %v", err)
	}
	if route.Model.ID != bare.ID || len(route.Candidates) != 0 {
		t.Errorf("无来源模型的路由 = %+v, 期望空候选", route)
	}

	if _, err := s.ResolveModelRoute(ctx, "no-such-model"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知模型应返回 ErrNotFound, got %v", err)
	}
}

// 计费模式裁决位（billingRankSQL）：同优先级下订阅套餐型上游（ark_plan）
// 先于按量付费，且只在优先级相同时生效——更小的显式优先级永远压过计费模式。
// ResolveModelRoute 与 ListModelsWithSources 必须同一口径，界面顺序即调度顺序。
func TestSourceOrderSubscriptionFirst(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-ds-billing-plaintext-1111", "")
	ark := mustUpstream(t, s, "ark", upstreamTypeArk, "sk-ark-billing-plaintext-2222", "")
	plan := mustUpstream(t, s, "ark-plan", upstreamTypeArkPlan, "sk-plan-billing-plaintext-3333", "")
	ds2 := mustUpstream(t, s, "ds-2", upstreamTypeDeepseek, "sk-ds2-billing-plaintext-4444", "")

	m := mustModel(t, s, "billing-model")
	// 同优先级 100：按量的 ds、ark 先建（id 更小），订阅的 plan 最后建。
	srcDS := mustSource(t, s, m.ID, ds.ID, "", 100)
	srcArk := mustSource(t, s, m.ID, ark.ID, "ark-chat", 100)
	srcPlan := mustSource(t, s, m.ID, plan.ID, "plan-chat", 100)
	// 显式更小的优先级（50，按量）应仍排在订阅 100 之前。
	srcTop := mustSource(t, s, m.ID, ds2.ID, "", 50)

	wantOrder := []int64{srcTop.ID, srcPlan.ID, srcDS.ID, srcArk.ID}

	route, err := s.ResolveModelRoute(ctx, "billing-model")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != len(wantOrder) {
		t.Fatalf("候选数 = %d, 期望 %d", len(route.Candidates), len(wantOrder))
	}
	for i, want := range wantOrder {
		if route.Candidates[i].SourceID != want {
			t.Errorf("候选[%d].SourceID = %d, 期望 %d（(priority, 订阅先行, id) 序）",
				i, route.Candidates[i].SourceID, want)
		}
	}

	models, err := s.ListModelsWithSources(ctx)
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(models) != 1 || len(models[0].Sources) != len(wantOrder) {
		t.Fatalf("嵌套列表形状不对: %+v", models)
	}
	for i, want := range wantOrder {
		if models[0].Sources[i].ID != want {
			t.Errorf("管理列表来源[%d].ID = %d, 期望 %d（与路由同口径）",
				i, models[0].Sources[i].ID, want)
		}
	}
}

// ResolveModelRoute：停用候选的凭证不参与解密——设备密钥换过之后，一行"停用
// 且密文解不开"的历史上游不得把整条路由拖成错误，同库的启用候选照常可用。
func TestResolveModelRouteSkipsDisabledCredentials(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const liveKey = "sk-live-plaintext-7777"
	stale := mustUpstream(t, s, "stale", upstreamTypeDeepseek, "sk-stale-plaintext-8888", "")
	live := mustUpstream(t, s, "live", upstreamTypeDeepseek, liveKey, "")
	m := mustModel(t, s, "two-source-model")
	staleSrc := mustSource(t, s, m.ID, stale.ID, "", 100)
	mustSource(t, s, m.ID, live.ID, "", 200)

	// 把 stale 上游的密文改成解不开的形态（等价"用旧设备密钥封存的历史行"），
	// 再按运维惯例停用它、并给 live 重录 Key。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upstreams SET api_key_sealed = ? WHERE id = ?`, "bm90LWEtdmFsaWQtY2lwaGVy", stale.ID); err != nil {
		t.Fatalf("注入损坏密文: %v", err)
	}
	if _, err := s.ResolveModelRoute(ctx, "two-source-model"); err == nil {
		t.Fatal("启用状态下解不开的凭证应硬报错（口径不变）")
	}
	if err := s.SetUpstreamDisabled(ctx, stale.ID, true); err != nil {
		t.Fatal(err)
	}

	route, err := s.ResolveModelRoute(ctx, "two-source-model")
	if err != nil {
		t.Fatalf("停用损坏行后路由应可用: %v", err)
	}
	if len(route.Candidates) != 2 {
		t.Fatalf("候选数 = %d, 期望 2（停用不过滤）", len(route.Candidates))
	}
	if !route.Candidates[0].Upstream.Disabled || route.Candidates[0].Upstream.APIKey != "" {
		t.Errorf("停用候选不应解密凭证: %+v", route.Candidates[0])
	}
	if route.Candidates[1].Upstream.APIKey != liveKey {
		t.Errorf("启用候选凭证 = %q, 期望解密回明文", route.Candidates[1].Upstream.APIKey)
	}

	// 来源侧停用同理（上游启用、来源停用）。
	if err := s.SetUpstreamDisabled(ctx, stale.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelSourceDisabled(ctx, staleSrc.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModelRoute(ctx, "two-source-model"); err != nil {
		t.Errorf("停用来源后路由应可用: %v", err)
	}
}

// GetSourceRoute（管理面「测试来源」的探测视图）：按来源主键取模型 + 来源 +
// 上游并解密凭证；无视三层启停位但原样带回启停事实；留空来源侧 ID 解析为
// 模型名；密文解不开报 ErrKeyUnreadable；未命中报 ErrNotFound。
func TestGetSourceRoute(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const key = "sk-source-route-9012"
	up := mustUpstream(t, s, "route-up", upstreamTypeDeepseek, key, "")
	m := mustModel(t, s, "route-model")
	src := mustSource(t, s, m.ID, up.ID, "", 100)

	route, err := s.GetSourceRoute(ctx, src.ID)
	if err != nil {
		t.Fatalf("GetSourceRoute: %v", err)
	}
	if route.Model.ID != m.ID || route.Model.Name != "route-model" {
		t.Errorf("模型行异常: %+v", route.Model)
	}
	c := route.Candidate
	if c.SourceID != src.ID || c.Priority != 100 || c.UpstreamModelID != "route-model" {
		t.Errorf("来源行异常（留空应解析为模型名）: %+v", c)
	}
	if c.Upstream.Name != "route-up" || c.Upstream.APIKey != key {
		t.Errorf("上游行异常（应带解密明文）: %+v", c.Upstream)
	}

	// 三层全停用照样可取，启停事实原样带回。
	if err := s.SetModelDisabled(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelSourceDisabled(ctx, src.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpstreamDisabled(ctx, up.ID, true); err != nil {
		t.Fatal(err)
	}
	route, err = s.GetSourceRoute(ctx, src.ID)
	if err != nil {
		t.Fatalf("停用后 GetSourceRoute: %v", err)
	}
	if !route.Model.Disabled || !route.Candidate.SourceDisabled || !route.Candidate.Upstream.Disabled {
		t.Errorf("启停位应原样带回: %+v", route.Candidate)
	}
	if route.Candidate.Upstream.APIKey != key {
		t.Errorf("停用行也要解密（测试要拿真凭证）: %q", route.Candidate.Upstream.APIKey)
	}

	// 密文损坏 → ErrKeyUnreadable（与 ResolveModelRoute 的静默留空刻意不同）。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upstreams SET api_key_sealed = ? WHERE id = ?`, "bm90LWEtdmFsaWQtY2lwaGVy", up.ID); err != nil {
		t.Fatalf("注入损坏密文: %v", err)
	}
	if _, err := s.GetSourceRoute(ctx, src.ID); !errors.Is(err, ErrKeyUnreadable) {
		t.Errorf("损坏密文应报 ErrKeyUnreadable, got %v", err)
	}

	if _, err := s.GetSourceRoute(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知来源应报 ErrNotFound, got %v", err)
	}
}

// ListServableModels 口径：模型/来源/上游任一停用即不列；无来源不列；
// 按原始名字典序。
func TestListServableModels(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	up := mustUpstream(t, s, "ds", upstreamTypeDeepseek, "sk-ds-plaintext-4444", "")
	up2 := mustUpstream(t, s, "mock", upstreamTypeMock, "", "http://127.0.0.1:18080/v1")

	// 名字刻意逆字典序创建，验证输出按名排序而非按 id。
	zeta := mustModel(t, s, "zeta-model")
	mustSource(t, s, zeta.ID, up.ID, "", 100)
	alpha := mustModel(t, s, "alpha-model")
	mustSource(t, s, alpha.ID, up.ID, "", 100)
	// 无任何来源 → 不可服务。
	mustModel(t, s, "bare-model")
	// 模型自身停用 → 不可服务。
	offModel := mustModel(t, s, "off-model")
	mustSource(t, s, offModel.ID, up.ID, "", 100)
	if err := s.SetModelDisabled(ctx, offModel.ID, true); err != nil {
		t.Fatal(err)
	}
	// 唯一来源停用 → 不可服务。
	offSource := mustModel(t, s, "off-source-model")
	src := mustSource(t, s, offSource.ID, up.ID, "", 100)
	if err := s.SetModelSourceDisabled(ctx, src.ID, true); err != nil {
		t.Fatal(err)
	}
	// 唯一来源的上游停用 → 不可服务。
	offUpstream := mustModel(t, s, "off-upstream-model")
	mustSource(t, s, offUpstream.ID, up2.ID, "", 100)
	if err := s.SetUpstreamDisabled(ctx, up2.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListServableModels(ctx)
	if err != nil {
		t.Fatalf("ListServableModels: %v", err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if len(names) != 2 || names[0] != "alpha-model" || names[1] != "zeta-model" {
		t.Fatalf("可服务模型 = %v, 期望 [alpha-model zeta-model]（字典序）", names)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("/v1/models 的 created 取模型 created_at，不应为零值")
	}

	// 多来源模型：只要还有一条启用来源挂在启用上游上，就仍然可服务。
	multi := mustModel(t, s, "multi-model")
	mustSource(t, s, multi.ID, up2.ID, "", 100) // up2 已停用
	if got, _ = s.ListServableModels(ctx); len(got) != 2 {
		t.Fatalf("只有停用上游来源的模型不应可服务, got %d 个", len(got))
	}
	mustSource(t, s, multi.ID, up.ID, "", 200)
	if got, _ = s.ListServableModels(ctx); len(got) != 3 {
		t.Fatalf("补一条可用来源后应可服务, got %d 个", len(got))
	}

	// ListServableModelSources 是同一口径的摊开版（把「有没有可用来源」变成
	// 「有哪些」）：模型集合必须与上面恒等——使用API页的可用模型清单靠这一点
	// 与数据面 /v1/models 对齐，两边一旦漂移，页面就会教人去调一个 404 的名字。
	srcs, err := s.ListServableModelSources(ctx)
	if err != nil {
		t.Fatalf("ListServableModelSources: %v", err)
	}
	var flat []string
	for _, r := range srcs {
		if len(flat) == 0 || flat[len(flat)-1] != r.ModelName {
			flat = append(flat, r.ModelName)
		}
		if r.UpstreamType == "" {
			t.Errorf("模型 %s 的来源缺上游 type（调用方据它算入口协议）", r.ModelName)
		}
		// ModelID 是读数层套 Key 的 API模型范围用的目录行 id，必须与模型行一致。
		if r.ModelName == "multi-model" && r.ModelID != multi.ID {
			t.Errorf("模型 %s 的 ModelID = %d，期望 %d", r.ModelName, r.ModelID, multi.ID)
		}
		if r.ModelID == 0 {
			t.Errorf("模型 %s 的来源缺 ModelID", r.ModelName)
		}
	}
	if len(flat) != len(got) {
		t.Fatalf("摊开后的模型集合 = %v，期望与 ListServableModels 的 %d 个恒等", flat, len(got))
	}
	for i, m := range got {
		if flat[i] != m.Name {
			t.Errorf("第 %d 个模型 = %q，期望 %q（两条查询同序同集合）", i, flat[i], m.Name)
		}
	}

	// base_url 要如实带出：mock 型上游靠它才有入口，调用方算协议时要用。
	// 同时验证一个模型的多条可用来源各占一行。
	if err := s.SetUpstreamDisabled(ctx, up2.ID, false); err != nil {
		t.Fatal(err)
	}
	srcs, err = s.ListServableModelSources(ctx)
	if err != nil {
		t.Fatalf("ListServableModelSources: %v", err)
	}
	var multiRows []ServableModelSource
	for _, r := range srcs {
		if r.ModelName == "multi-model" {
			multiRows = append(multiRows, r)
		}
	}
	if len(multiRows) != 2 {
		t.Fatalf("multi-model 的可用来源 = %d 行，期望 2（两个上游都启用了）", len(multiRows))
	}
	var sawBaseURL bool
	for _, r := range multiRows {
		if r.UpstreamType == upstreamTypeMock {
			sawBaseURL = r.UpstreamBaseURL == "http://127.0.0.1:18080/v1"
		}
	}
	if !sawBaseURL {
		t.Errorf("mock 来源的 base_url 未带出：%+v", multiRows)
	}
}

// TestServableModelsTextOnly 钉死迭代 8 的文本口径：加了视频/图片模型之后
// /v1/models 与使用API页的两条 servable 读数**集合不变**。关键陷阱是挂在 ark
// 上游的视频模型——ark 服务 openai_chat，若谓词只按协议过滤而不按 kind 过滤，
// 它就会漏进文本读数；所以谓词必须显式钉 kind=text，本测试是那道绊线。
func TestServableModelsTextOnly(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ark := mustUpstream(t, s, "ark-main", "ark", "sk-ark-plaintext-2222", "")
	mm := mustUpstream(t, s, "minimax-main", "minimax", "sk-minimax-plaintext-7777", "")
	textModel := mustModel(t, s, "deepseek-chat")
	mustSource(t, s, textModel.ID, ark.ID, "", 100)

	snapshot := func() []string {
		t.Helper()
		got, err := s.ListServableModels(ctx)
		if err != nil {
			t.Fatalf("ListServableModels: %v", err)
		}
		var names []string
		for _, m := range got {
			names = append(names, m.Name)
		}
		rows, err := s.ListServableModelSources(ctx)
		if err != nil {
			t.Fatalf("ListServableModelSources: %v", err)
		}
		var flat []string
		for _, r := range rows {
			if len(flat) == 0 || flat[len(flat)-1] != r.ModelName {
				flat = append(flat, r.ModelName)
			}
		}
		if strings.Join(flat, ",") != strings.Join(names, ",") {
			t.Fatalf("两条 servable 读数集合漂移：models=%v sources=%v", names, flat)
		}
		return names
	}

	before := snapshot()
	if strings.Join(before, ",") != "deepseek-chat" {
		t.Fatalf("基线 = %v，期望 [deepseek-chat]", before)
	}

	// 视频模型挂 ark（陷阱）与 minimax 来源，图片模型挂 ark——三条来源全部
	// 启用、上游全部启用，仍不得进入文本读数。
	video, err := s.CreateModel(ctx, "doubao-seedance-2-0-mini", ModelKindVideo, "")
	if err != nil {
		t.Fatalf("CreateModel(video): %v", err)
	}
	mustSource(t, s, video.ID, ark.ID, "doubao-seedance-2-0-mini-260615", 100)
	mustSource(t, s, video.ID, mm.ID, "MiniMax-H3", 200)
	image, err := s.CreateModel(ctx, "doubao-seedream", ModelKindImage, "")
	if err != nil {
		t.Fatalf("CreateModel(image): %v", err)
	}
	mustSource(t, s, image.ID, ark.ID, "", 100)

	after := snapshot()
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("加了视频/图片模型后 /v1/models 集合变了：%v → %v", before, after)
	}
	// ResolveModelRoute 不设 kind 过滤（路由视图带 kind 事实，闸门在 gateway
	// 按入口裁决）：视频模型仍可按名解析，供视频入口选路。
	route, err := s.ResolveModelRoute(ctx, "doubao-seedance-2-0-mini")
	if err != nil || route.Model.Kind != ModelKindVideo || len(route.Candidates) != 2 {
		t.Fatalf("视频模型的路由视图 = %+v (err=%v)，期望 kind=video 且 2 候选", route, err)
	}
}

// TestListServableAIGCSources：视频/图片读数与文本读数互为反选——同一启用
// 谓词、kind 显式 IN (video,image)；文本模型不进、三层任一停用即出、字段只有
// 模型名/kind/上游 type/base_url（上游名与凭据不出成员可读链路）。
func TestListServableAIGCSources(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	ark := mustUpstream(t, s, "ark-main", upstreamTypeArk, "sk-ark-plaintext-2222", "")
	mm := mustUpstream(t, s, "minimax-main", "minimax", "sk-minimax-plaintext-7777", "")
	ds := mustUpstream(t, s, "ds-main", upstreamTypeDeepseek, "sk-ds-plaintext-3333", "")
	textModel := mustModel(t, s, "deepseek-chat")
	mustSource(t, s, textModel.ID, ds.ID, "", 100)

	video, err := s.CreateModel(ctx, "doubao-seedance-2-0-mini", ModelKindVideo, "")
	if err != nil {
		t.Fatalf("CreateModel(video): %v", err)
	}
	mustSource(t, s, video.ID, ark.ID, "doubao-seedance-2-0-mini-260615", 100)
	mmSrc := mustSource(t, s, video.ID, mm.ID, "MiniMax-H3", 200)
	image, err := s.CreateModel(ctx, "doubao-seedream", ModelKindImage, "")
	if err != nil {
		t.Fatalf("CreateModel(image): %v", err)
	}
	mustSource(t, s, image.ID, ark.ID, "", 100)
	// 无来源的视频模型不出现（与 /v1/models 排除无来源模型同一口径）。
	if _, err := s.CreateModel(ctx, "bare-video", ModelKindVideo, ""); err != nil {
		t.Fatalf("CreateModel(bare-video): %v", err)
	}

	rows, err := s.ListServableAIGCSources(ctx)
	if err != nil {
		t.Fatalf("ListServableAIGCSources: %v", err)
	}
	var flat []string
	for _, r := range rows {
		flat = append(flat, r.ModelName+"/"+r.Kind+"/"+r.UpstreamType)
		if r.ModelID == 0 {
			t.Errorf("模型 %s 的来源缺 ModelID（读数层套 Key 范围要用）", r.ModelName)
		}
	}
	want := "doubao-seedance-2-0-mini/video/ark,doubao-seedance-2-0-mini/video/minimax,doubao-seedream/image/ark"
	if strings.Join(flat, ",") != want {
		t.Fatalf("读数 = %v，期望 %s", flat, want)
	}

	// 停用一条来源即从读数消失；停用模型则整组消失。
	if err := s.SetModelSourceDisabled(ctx, mmSrc.ID, true); err != nil {
		t.Fatalf("SetModelSourceDisabled: %v", err)
	}
	if err := s.SetModelDisabled(ctx, image.ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}
	rows, err = s.ListServableAIGCSources(ctx)
	if err != nil {
		t.Fatalf("ListServableAIGCSources(after disable): %v", err)
	}
	if len(rows) != 1 || rows[0].ModelName != "doubao-seedance-2-0-mini" || rows[0].UpstreamType != upstreamTypeArk {
		t.Fatalf("停用后读数 = %+v，期望只剩 seedance@ark", rows)
	}
}

// ListModelsWithSources：模型字典序 + 来源 (priority, 订阅先行, id) 序 +
// 上游展示字段；无来源的模型也出现；管理视图不含任何凭证明文。
func TestListModelsWithSources(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	const dsKey = "sk-ds-nested-plaintext-5555"
	ds := mustUpstream(t, s, "ds", upstreamTypeDeepseek, dsKey, "")
	mock := mustUpstream(t, s, "mock", upstreamTypeMock, "", "http://127.0.0.1:18080/v1")

	zeta := mustModel(t, s, "zeta-model")
	alpha := mustModel(t, s, "alpha-model")
	mustModel(t, s, "bare-model")
	srcLow := mustSource(t, s, alpha.ID, mock.ID, "mock-chat", 200)
	srcHigh := mustSource(t, s, alpha.ID, ds.ID, "", 100)
	mustSource(t, s, zeta.ID, ds.ID, "deepseek-reasoner", 100)
	if err := s.SetUpstreamDisabled(ctx, mock.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListModelsWithSources(ctx)
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("模型数 = %d, 期望 3（含无来源的）", len(got))
	}
	if got[0].Name != "alpha-model" || got[1].Name != "bare-model" || got[2].Name != "zeta-model" {
		t.Fatalf("模型未按字典序: %s/%s/%s", got[0].Name, got[1].Name, got[2].Name)
	}
	if len(got[1].Sources) != 0 {
		t.Errorf("无来源模型的 Sources 应为空, got %d 条", len(got[1].Sources))
	}
	if len(got[0].Sources) != 2 {
		t.Fatalf("alpha-model 来源数 = %d, 期望 2", len(got[0].Sources))
	}
	if got[0].Sources[0].ID != srcHigh.ID || got[0].Sources[1].ID != srcLow.ID {
		t.Error("来源未按 (priority, id) 排序")
	}
	first := got[0].Sources[0]
	if first.ModelID != alpha.ID || first.UpstreamID != ds.ID || first.Priority != 100 {
		t.Errorf("来源字段不全: %+v", first.ModelSource)
	}
	// 管理视图保留原始空串（UI 用模型名做占位符），不做路由侧的解析。
	if first.UpstreamModelID != "" {
		t.Errorf("管理视图应保留空的 upstream_model_id, got %q", first.UpstreamModelID)
	}
	if first.UpstreamName != "ds" || first.UpstreamType != upstreamTypeDeepseek || first.UpstreamDisabled {
		t.Errorf("来源未带出上游展示字段: %+v", first)
	}
	if second := got[0].Sources[1]; !second.UpstreamDisabled || second.UpstreamName != "mock" {
		t.Errorf("上游停用位未带出: %+v", second)
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Error("来源时间戳未解析")
	}

	// §15.1：管理视图的整棵结构里不得出现凭证明文。
	if dump := fmt.Sprintf("%+v", got); strings.Contains(dump, dsKey) {
		t.Error("ListModelsWithSources 结果含凭证明文")
	}
}

// 设备密钥与凭证封存：device-key 生成为 32B/0600 并跨重开复用；
// 凭证明文加解密往返；数据目录下的原始字节（含 WAL）里没有明文。
func TestUpstreamKeySealing(t *testing.T) {
	s, dir := mustOpen(t)
	ctx := context.Background()

	keyPath := filepath.Join(dir, DeviceKeyFileName)
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("设备密钥未生成: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("设备密钥权限 = %o, 期望 0600", got)
	}
	if fi.Size() != deviceKeySize {
		t.Errorf("设备密钥长度 = %d, 期望 %d", fi.Size(), deviceKeySize)
	}
	firstKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	const plaintext = "sk-NEVER-IN-DB-abcdefghijklmnopqrstuvwxyz-7777"
	up := mustUpstream(t, s, "ds", upstreamTypeDeepseek, plaintext, "")
	m := mustModel(t, s, "deepseek-chat")
	mustSource(t, s, m.ID, up.ID, "", 100)

	// 密文列本身必须与明文不同（真的加过密，不是原样存）。
	var sealed string
	if err := s.db.QueryRow(`SELECT api_key_sealed FROM upstreams WHERE id = ?`, up.ID).Scan(&sealed); err != nil {
		t.Fatalf("读 api_key_sealed: %v", err)
	}
	if sealed == "" || strings.Contains(sealed, plaintext) {
		t.Fatalf("api_key_sealed 未加密: %q", sealed)
	}

	// 关库让 WAL 落盘/清理，再扫数据目录下的全部文件。
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
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读 %s: %v", e.Name(), err)
		}
		if bytes.Contains(raw, []byte(plaintext)) {
			t.Errorf("%s 的原始字节含上游凭证明文（§15.1 违规）", e.Name())
		}
	}

	// 重开：复用同一把设备密钥，密文解得回明文（加解密往返）。
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	secondKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstKey, secondKey) {
		t.Error("重开不应重新生成设备密钥")
	}
	route, err := s2.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveModelRoute: %v", err)
	}
	if len(route.Candidates) != 1 || route.Candidates[0].Upstream.APIKey != plaintext {
		t.Fatalf("凭证未解回明文: %+v", route.Candidates)
	}
	if got, err := s2.GetUpstreamByID(ctx, up.ID); err != nil || got.APIKeyLast4 != "7777" {
		t.Errorf("管理视图 last4 = %q (err=%v), 期望 7777", got.APIKeyLast4, err)
	}

	// 每次封存用新 nonce：同一明文两次入库的密文不同。
	up2 := mustUpstream(t, s2, "ds2", upstreamTypeDeepseek, plaintext, "")
	var sealed2 string
	if err := s2.db.QueryRow(`SELECT api_key_sealed FROM upstreams WHERE id = ?`, up2.ID).Scan(&sealed2); err != nil {
		t.Fatal(err)
	}
	if sealed2 == sealed {
		t.Error("相同明文两次封存应因随机 nonce 得到不同密文")
	}
}

// 设备密钥异常：长度不对拒绝启动；密钥被替换后解密报可读错误而不是 panic，
// 管理视图退化为空 last4 但仍可列出（可恢复：管理台重新录入 Key）。
func TestDeviceKeyFailureModes(t *testing.T) {
	s, dir := mustOpen(t)
	ctx := context.Background()

	const plaintext = "sk-rotate-victim-plaintext-8888"
	up := mustUpstream(t, s, "ds", upstreamTypeDeepseek, plaintext, "")
	m := mustModel(t, s, "deepseek-chat")
	mustSource(t, s, m.ID, up.ID, "", 100)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, DeviceKeyFileName)

	// 长度不对 → Open 失败，错误可读且指明补救动作。
	if err := os.WriteFile(keyPath, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil {
		t.Fatal("设备密钥长度非法时 Open 应失败")
	}
	if !strings.Contains(err.Error(), DeviceKeyFileName) || !strings.Contains(err.Error(), "已损坏") {
		t.Errorf("设备密钥长度错误信息不够可读: %q", err.Error())
	}

	// 换一把等长的密钥：Open 成功，但既有密文解不开。
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x5a}, deviceKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("等长新密钥应能启动: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	// 路由侧：报可读错误（不 panic、不静默跳过），且不回显密文与明文。
	_, err = s2.ResolveModelRoute(ctx, "deepseek-chat")
	if err == nil {
		t.Fatal("密钥被替换后路由解密应报错")
	}
	if !strings.Contains(err.Error(), "解密失败") || !strings.Contains(err.Error(), "重新录入") {
		t.Errorf("解密错误不够可读: %q", err.Error())
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Error("解密错误信息含凭证明文（§15.1 违规）")
	}

	// 管理侧：列表仍可读，last4 退化为空（提示需重新录入），不整张表打不开。
	ups, err := s2.ListUpstreams(ctx)
	if err != nil {
		t.Fatalf("密钥被替换后上游列表仍应可读: %v", err)
	}
	if len(ups) != 1 || ups[0].APIKeyLast4 != "" {
		t.Errorf("解不开的凭证 last4 应为空: %+v", ups)
	}

	// 重新录入即恢复：新密钥封存的凭证解得回来。
	const rotated = "sk-rotated-plaintext-9999"
	if err := s2.SetUpstreamKey(ctx, up.ID, rotated); err != nil {
		t.Fatalf("SetUpstreamKey: %v", err)
	}
	route, err := s2.ResolveModelRoute(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("重新录入后路由应恢复: %v", err)
	}
	if len(route.Candidates) != 1 || route.Candidates[0].Upstream.APIKey != rotated {
		t.Fatalf("重新录入后凭证不对: %+v", route.Candidates)
	}
}

// 封存/解出的边界：空明文往返空串；密文被截断/非 base64 时报可读错误。
func TestSealRoundTripEdgeCases(t *testing.T) {
	s, _ := mustOpen(t)

	if sealed, err := s.sealKey(""); err != nil || sealed != "" {
		t.Errorf("空明文应封存为空串, got %q (err=%v)", sealed, err)
	}
	if plaintext, err := s.openKey(""); err != nil || plaintext != "" {
		t.Errorf("空密文应解出空串, got %q (err=%v)", plaintext, err)
	}

	const secret = "sk-roundtrip-plaintext-0000"
	sealed, err := s.sealKey(secret)
	if err != nil {
		t.Fatalf("sealKey: %v", err)
	}
	if got, err := s.openKey(sealed); err != nil || got != secret {
		t.Fatalf("往返不一致: %q (err=%v)", got, err)
	}

	if _, err := s.openKey("这不是 base64"); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Errorf("非 base64 密文应报可读错误, got %v", err)
	}
	if _, err := s.openKey("YWJj"); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Errorf("过短密文应报可读错误, got %v", err)
	}
	if _, err := s.openKey(sealed[:len(sealed)-8] + "AAAAAAAA"); err == nil {
		t.Error("被篡改的密文应解密失败（GCM 认证）")
	}
}

// TestModelEntrySwitches（0013 调用入口开关）：默认双开；SetModelEntries 写入
// 即读出；两条 servable 读数排除双关模型且集合保持恒等，来源读数带回开关。
func TestModelEntrySwitches(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	up := mustUpstream(t, s, "ds-main", "deepseek", "sk-ds-plaintext-1111", "")
	m := mustModel(t, s, "entry-model")
	mustSource(t, s, m.ID, up.ID, "", 100)

	if !m.EntryOpenAI || !m.EntryAnthropic {
		t.Fatalf("新模型入口开关应默认双开: %+v", m)
	}
	if err := s.SetModelEntries(ctx, m.ID, false, false, true); err != nil {
		t.Fatalf("SetModelEntries: %v", err)
	}
	got, err := s.GetModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetModelByID: %v", err)
	}
	if got.EntryOpenAI || !got.EntryAnthropic {
		t.Fatalf("开关未写入: openai=%v anthropic=%v", got.EntryOpenAI, got.EntryAnthropic)
	}

	// 单关照列，且来源读数带回开关事实。
	rows, err := s.ListServableModelSources(ctx)
	if err != nil {
		t.Fatalf("ListServableModelSources: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ModelName == "entry-model" {
			found = true
			if r.EntryOpenAI || !r.EntryAnthropic {
				t.Fatalf("来源读数开关不符: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("单关模型应仍在 servable 读数里: %+v", rows)
	}

	// 双关等同停用：两条读数一起消失（谓词逐字相同，集合恒等）。
	if err := s.SetModelEntries(ctx, m.ID, false, false, false); err != nil {
		t.Fatalf("SetModelEntries: %v", err)
	}
	models, err := s.ListServableModels(ctx)
	if err != nil {
		t.Fatalf("ListServableModels: %v", err)
	}
	for _, sm := range models {
		if sm.Name == "entry-model" {
			t.Fatalf("双关模型不该出现在 ListServableModels: %+v", models)
		}
	}
	rows, err = s.ListServableModelSources(ctx)
	if err != nil {
		t.Fatalf("ListServableModelSources: %v", err)
	}
	for _, r := range rows {
		if r.ModelName == "entry-model" {
			t.Fatalf("双关模型不该出现在 ListServableModelSources: %+v", rows)
		}
	}
	if err := s.SetModelEntries(ctx, 99999, true, true, true); err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的模型应回 ErrNotFound，得到 %v", err)
	}
}
