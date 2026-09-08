package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DeviceNameSetting  = "device_name"
	DeviceNameMaxLen   = 32
	deviceNameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var ErrInvalidDeviceName = errors.New("设备名须为 1–32 个字符，不能包含控制字符或不可见格式字符")

// DeviceName 返回持久化的本地显示名称。缺失或空白时生成一次，条件写入避免
// 两个初始化读数互相覆盖，或覆盖管理员刚保存的名称。它不是设备身份或系统主机名。
func (s *Store) DeviceName(ctx context.Context) (string, error) {
	name, err := s.GetSetting(ctx, DeviceNameSetting)
	if err != nil || strings.TrimSpace(name) != "" {
		return name, err
	}
	var generated [6]byte
	for i := range generated {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(deviceNameAlphabet))))
		if err != nil {
			return "", fmt.Errorf("生成设备名: %w", err)
		}
		generated[i] = deviceNameAlphabet[n.Int64()]
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		WHERE settings.value = ?`, DeviceNameSetting, string(generated[:]), fmtTime(time.Now()), name)
	if err != nil {
		return "", fmt.Errorf("初始化设备名: %w", err)
	}
	return s.GetSetting(ctx, DeviceNameSetting)
}

func (s *Store) SetDeviceName(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > DeviceNameMaxLen {
		return "", ErrInvalidDeviceName
	}
	for _, char := range name {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) || unicode.Is(unicode.Zl, char) || unicode.Is(unicode.Zp, char) {
			return "", ErrInvalidDeviceName
		}
	}
	if err := s.SetSetting(ctx, DeviceNameSetting, name); err != nil {
		return "", err
	}
	return name, nil
}
