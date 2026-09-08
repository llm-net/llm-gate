package store

// 单值配置（settings 表）与其密封变体的验收（迭代 10）。
//
// 密封那一套的三条性质是这里要钉住的：往返可读、**库里存的不是明文**、
// 密文钉在自己的键上（AAD）。第二条是它存在的全部理由——单独外流的 llmgate.db
// 副本里不该有密封配置的明文。

import (
	"context"
	"strings"
	"testing"
)

// TestLegacyCloudAndExternalSettingsAreDropped：存量库升级后清除不再使用的官网
// 上报配置与外部转发地址，仍在使用的本地目录数据保持原样。
func TestLegacyCloudAndExternalSettingsAreDropped(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 17)
	for key, value := range map[string]string{
		"cloud_enabled":    "true",
		"cloud_serial":     "FAKE-SERIAL",
		"cloud_secret":     "sealed-fake-secret",
		"external_url":     "https://box.example.com",
		"official_pricing": "{}",
		"platform_models":  "{}",
	} {
		if _, err := db.Exec(
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			key, value, legacyTS,
		); err != nil {
			t.Fatalf("写存量设置 %s: %v", key, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量库: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("迁移存量库: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, key := range []string{"cloud_enabled", "cloud_serial", "cloud_secret", "external_url"} {
		if got, err := s.GetSetting(ctx, key); err != nil || got != "" {
			t.Errorf("旧设置 %s 迁移后 = %q (err=%v)，期望删除", key, got, err)
		}
	}
	for key, want := range map[string]string{
		"official_pricing": "{}",
		"platform_models":  "{}",
	} {
		if got, err := s.GetSetting(ctx, key); err != nil || got != want {
			t.Errorf("本地设置 %s 迁移后 = %q (err=%v)，期望 %q", key, got, err, want)
		}
	}
	for _, version := range []int{18, 20} {
		var applied int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("schema_migrations 缺版本 %d (n=%d, err=%v)", version, applied, err)
		}
	}
}

func TestSettingRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	// 没设过 = 空串，不是 ErrNotFound（调用方把两者当同一状态）。
	if got, err := s.GetSetting(ctx, "never_set"); err != nil || got != "" {
		t.Fatalf("未设置的键 = %q (err=%v)，期望空串", got, err)
	}
	if err := s.SetSetting(ctx, "feature_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSetting(ctx, "feature_enabled"); got != "true" {
		t.Errorf("读回 %q，期望 true", got)
	}
	// 覆盖写。
	if err := s.SetSetting(ctx, "feature_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSetting(ctx, "feature_enabled"); got != "false" {
		t.Errorf("覆盖后读回 %q，期望 false", got)
	}
}

// TestSealedSettingIsCiphertextAtRest：密封配置往返可读，而库里那一行不含明文。
func TestSealedSettingIsCiphertextAtRest(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	const secret = "sealed-setting-secret-not-a-real-one"

	if err := s.SetSealedSetting(ctx, "integration_secret", secret); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSealedSetting(ctx, "integration_secret")
	if err != nil {
		t.Fatalf("GetSealedSetting: %v", err)
	}
	if got != secret {
		t.Errorf("往返得到 %q，期望原值", got)
	}

	// 底层那一行是密文：明文一个字节都不在库里。
	raw, err := s.GetSetting(ctx, "integration_secret")
	if err != nil {
		t.Fatal(err)
	}
	if raw == secret || strings.Contains(raw, secret) {
		t.Errorf("库里存的是明文: %q", raw)
	}
	if raw == "" {
		t.Error("密文列为空")
	}

	// 空值合法，且解出空值（等同「清空」）。
	if err := s.SetSealedSetting(ctx, "integration_secret", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSealedSetting(ctx, "integration_secret"); err != nil || got != "" {
		t.Errorf("清空后 = %q (err=%v)", got, err)
	}
}

// TestSealedSettingAADBindsKey：密文钉在自己的键上——把它抄到别的键，解出来的
// 不是「那个键的值」，而是一个错误。AAD 的全部意义就在这条。
func TestSealedSettingAADBindsKey(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	const secret = "sealed-setting-secret-not-a-real-one"

	if err := s.SetSealedSetting(ctx, "integration_secret", secret); err != nil {
		t.Fatal(err)
	}
	sealed, _ := s.GetSetting(ctx, "integration_secret")
	if err := s.SetSetting(ctx, "other_secret", sealed); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSealedSetting(ctx, "other_secret")
	if err == nil {
		t.Fatalf("换键仍解出了 %q", got)
	}
	// 错误可读、点名键与设备密钥文件，且不回显密文与明文。
	msg := err.Error()
	if !strings.Contains(msg, "other_secret") || !strings.Contains(msg, DeviceKeyFileName) {
		t.Errorf("错误未指明键与补救对象: %v", err)
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, sealed) {
		t.Errorf("错误泄露了密文或明文: %v", err)
	}
}

// TestSealedSettingRejectsCorruption：库里那一行被改坏时报错而不是返回空串。
// 「没设过」与「读不出来」的处置完全不同（前者是正常空值，后者要重新写入）。
func TestSealedSettingRejectsCorruption(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	if err := s.SetSetting(ctx, "integration_secret", "not-base64-@@@"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSealedSetting(ctx, "integration_secret"); err == nil {
		t.Error("坏密文应报错")
	}
}
