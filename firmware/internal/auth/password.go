package auth

// Argon2id 密码哈希（架构 §12：管理员口令 Argon2id 存储）。
//
//   - 生成参数取 OWASP 推荐：m=19456 KiB（19 MiB）、t=2、p=1，盐 16B、key 32B。
//   - 入库为 PHC 格式串 `$argon2id$v=19$m=...,t=...,p=...$salt$hash`
//     （base64 无填充）。verify 按串内参数重算——未来调参后旧串仍可验证。
//   - 密码策略：长度 ≥8 且 ≤128（按 Unicode 字符计），无复杂度规则。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// 生成新哈希用的缺省参数（OWASP Password Storage Cheat Sheet 推荐档）。
const (
	argonMemoryKiB uint32 = 19456
	argonTime      uint32 = 2
	argonThreads   uint8  = 1
	argonSaltLen          = 16
	argonKeyLen    uint32 = 32
)

// 密码策略边界。下限与出厂默认口令 [DefaultPassword] 同宽——策略比默认值
// 严会造出一个「设备自带的口令按自己的规矩是非法的」的怪状态。
const (
	PasswordMinRunes = 8
	PasswordMaxRunes = 128
)

// 解析 PHC 串时的参数上限：防止畸形/被改写的库行造成内存或 CPU 放大。
// 上限覆盖可预见的调参空间（OWASP 各推荐档均远低于此）。
const (
	phcMaxMemoryKiB = 262144 // 256 MiB
	phcMaxTime      = 16
	phcMaxThreads   = 8
	phcMinSaltLen   = 8
	phcMaxSaltLen   = 64
	phcMinKeyLen    = 16
	phcMaxKeyLen    = 128
)

// ValidatePassword 校验密码策略（长度按 Unicode 字符计）。
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < PasswordMinRunes || n > PasswordMaxRunes {
		return fmt.Errorf("%w：长度须为 %d–%d 个字符", ErrPasswordPolicy, PasswordMinRunes, PasswordMaxRunes)
	}
	return nil
}

// HashPassword 以缺省参数生成 Argon2id PHC 串。策略校验（ValidatePassword）
// 由调用方在入口处完成，本函数只负责哈希。
func HashPassword(password string) (string, error) {
	return hashWithParams(password, argonMemoryKiB, argonTime, argonThreads)
}

// hashWithParams 以给定参数生成 PHC 串（测试用它验证 verify 的参数兼容性）。
func hashWithParams(password string, memoryKiB, timeCost uint32, threads uint8) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, timeCost, memoryKiB, threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, timeCost, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 按 phc 串内的参数重算并常数时间比较。
// 返回 (false, err) 表示串本身非法（服务端数据问题）；(false, nil) 表示密码不符。
func VerifyPassword(phc, password string) (bool, error) {
	p, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), p.salt, p.time, p.memoryKiB, p.threads, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(got, p.key) == 1, nil
}

// phcParams 是从 PHC 串解析出的验证参数。
type phcParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	salt      []byte
	key       []byte
}

// errMalformedPHC 统一畸形串错误。不回显串内容——哈希串虽非明文，
// 也没有出现在错误信息与日志里的理由。
var errMalformedPHC = errors.New("auth: 密码哈希串非法或不受支持")

// parsePHC 严格解析 `$argon2id$v=19$m=..,t=..,p=..$salt$hash`（base64 无填充，
// 参数按 m,t,p 顺序），并对参数与长度做上限校验。
func parsePHC(s string) (*phcParams, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" {
		return nil, errMalformedPHC
	}
	if parts[1] != "argon2id" {
		return nil, fmt.Errorf("%w（算法须为 argon2id）", errMalformedPHC)
	}
	vs, ok := strings.CutPrefix(parts[2], "v=")
	if !ok {
		return nil, errMalformedPHC
	}
	v, err := strconv.Atoi(vs)
	if err != nil || v != argon2.Version {
		return nil, fmt.Errorf("%w（argon2 版本须为 %d）", errMalformedPHC, argon2.Version)
	}

	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return nil, errMalformedPHC
	}
	m, err1 := cutUint(fields[0], "m=")
	t, err2 := cutUint(fields[1], "t=")
	p, err3 := cutUint(fields[2], "p=")
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, errMalformedPHC
	}
	if p < 1 || p > phcMaxThreads || t < 1 || t > phcMaxTime ||
		m < 8*p || m > phcMaxMemoryKiB {
		return nil, fmt.Errorf("%w（参数越界）", errMalformedPHC)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < phcMinSaltLen || len(salt) > phcMaxSaltLen {
		return nil, fmt.Errorf("%w（盐非法）", errMalformedPHC)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < phcMinKeyLen || len(key) > phcMaxKeyLen {
		return nil, fmt.Errorf("%w（哈希段非法）", errMalformedPHC)
	}
	return &phcParams{
		memoryKiB: uint32(m),
		time:      uint32(t),
		threads:   uint8(p),
		salt:      salt,
		key:       key,
	}, nil
}

// cutUint 解析形如 "m=19456" 的字段。
func cutUint(field, prefix string) (uint64, error) {
	v, ok := strings.CutPrefix(field, prefix)
	if !ok {
		return 0, errMalformedPHC
	}
	return strconv.ParseUint(v, 10, 32)
}
