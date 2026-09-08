package store

// 设备密钥与上游凭证封存（架构 §12）。
//
// <data_dir>/device-key 是 32 字节 crypto/rand 随机串，权限 0600，首次 Open
// 时生成；upstreams.api_key_sealed 存 AES-256-GCM 密文（随机 nonce 前置，
// 整体 base64）。价值边界写明白：单独外流的 llmgate.db 副本不含上游 Key 明文；
// 密钥与库同目录，同时拿到两者仍可解密——这个局限是已知并接受的（决策 2），
// 加密备份迭代复用同一套密钥体系。
//
// 用设备密钥封存的密文共有四类，各自钉不同的 AAD：upstreams.api_key_sealed
// （AAD 恒 nil——那批密文迭代 5 就已入库，改 AAD 等于让所有已存的 Key 立刻读不
// 出来）、密封的 settings 单值（AAD = setting 键名）、agent_accounts.auth_json_sealed（AAD = "agent:<provider>"，
// Codex/Grok 存 auth.json，Claude 存规范 setup-token 包装；见 agents.go），以及 api_keys.plaintext_sealed（AAD =
// "apikey:<key_digest>"，逐行钉死在自己的摘要上，密文挪行解不开；属主自助复制
// 端点用，见 repo.go 的 GetAPIKeyPlaintext）。加第五处之前先回来改这一段。
//
// §15.1 纪律：明文只出现在 CreateUpstream/SetUpstreamKey 的入参与
// ResolveModelRoute 的返回值里（Agents 凭据同理，只在 GetAgentCredential 的
// 返回值里；客户端 API密钥同理，只在 CreateAPIKey 的入参与 GetAPIKeyPlaintext
// 的返回值里）；本文件的错误信息不含密钥物料、密文内容或明文片段，可安全落日志。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DeviceKeyFileName 是数据目录下的设备密钥文件名；板上即 /var/lib/llmgate/device-key。
const DeviceKeyFileName = "device-key"

// deviceKeySize 是设备密钥字节数：32 字节 → AES-256。
const deviceKeySize = 32

// loadOrCreateDeviceKey 读取 dataDir 下的设备密钥，不存在则生成。
// 生成用 O_EXCL，两个进程并发首启时只有一个能建、另一个转为读取已生成的
// 密钥；写入后 fsync——设备密钥一旦丢失，已入库的上游 Key 全部不可恢复。
func loadOrCreateDeviceKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, DeviceKeyFileName)

	key, err := readDeviceKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	key = make([]byte, deviceKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成设备密钥: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 与另一个进程竞态：它已经写好了，改读它的。
			return readDeviceKey(path)
		}
		return nil, fmt.Errorf("创建设备密钥: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, fmt.Errorf("写入设备密钥: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("同步设备密钥: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("关闭设备密钥: %w", err)
	}
	return key, nil
}

// readDeviceKey 读取并校验既有设备密钥；文件不存在时原样返回 fs.ErrNotExist
// 供调用方转入生成分支。已存在的文件同样收紧到 0600（chmod 幂等）。
func readDeviceKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("读取设备密钥: %w", err)
	}
	if len(key) != deviceKeySize {
		return nil, fmt.Errorf("设备密钥 %s 为 %d 字节，期望 %d：文件已损坏；"+
			"恢复备份，或删除后重建（重建后已入库的上游 Key 需在管理台重新录入）",
			path, len(key), deviceKeySize)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("收紧设备密钥权限: %w", err)
	}
	return key, nil
}

// newAEAD 用设备密钥构造 AES-256-GCM。
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化设备密钥密码器: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM: %w", err)
	}
	return aead, nil
}

// seal 把明文封存为入库文本：base64(nonce || GCM 密文)。空明文封存为空串。
//
// aad 是附加认证数据：它不进密文，但解开时必须逐字节相同。给单值配置传
// **setting 键名**，密文就钉死在那一个键上——把某项密文抄进另一个
// 键（或反过来）都会解不开，而不是悄悄当成那个键的值。上游凭证传 nil：那批
// 密文在迭代 5 就已入库，改 AAD 等于让所有已存的 Key 立刻读不出来。
func (s *Store) seal(plaintext string, aad []byte) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成 nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), aad)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// open 解出入库文本对应的明文；空串解出空串。三类失败各返回一个哨兵化的
// 原因，由调用方拼上自己的补救文案（同一个坏设备密钥，对上游 Key 与对云凭据
// 意味着两件完全不同的补救动作）。
func (s *Store) open(sealed string, aad []byte) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", errSealBase64
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) <= nonceSize {
		return "", errSealTooShort
	}
	plaintext, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], aad)
	if err != nil {
		return "", errSealMismatch
	}
	return string(plaintext), nil
}

// 解封失败的三种原因（内部哨兵，不外泄给调用方）。
var (
	errSealBase64   = errors.New("密文 base64 解码失败")
	errSealTooShort = errors.New("密文长度不足")
	errSealMismatch = errors.New("密文与设备密钥不匹配（设备密钥被替换或密文损坏）")
)

// sealKey 把上游凭证明文封存为入库文本。
// 空明文封存为空串——mock 之类无凭证的上游 api_key_sealed 就是空列。
func (s *Store) sealKey(plaintext string) (string, error) { return s.seal(plaintext, nil) }

// openKey 解出入库文本对应的上游凭证明文；空串解出空串。
// 密钥被替换或密文损坏时返回可读错误（不 panic、不回显密文），指明补救动作。
func (s *Store) openKey(sealed string) (string, error) {
	plaintext, err := s.open(sealed, nil)
	if err != nil {
		if errors.Is(err, errSealMismatch) {
			return "", fmt.Errorf("上游凭证解密失败：%s 与密文不匹配（设备密钥被替换或密文损坏），请在管理台重新录入该上游的 Key",
				DeviceKeyFileName)
		}
		return "", fmt.Errorf("上游凭证%s：库中密文已损坏，请在管理台重新录入该上游的 Key", err)
	}
	return plaintext, nil
}

// last4 取凭证末 4 位作展示片段。短于 8 字节的凭证无法在"至少遮住一半"的
// 前提下展示，一律留空（与 internal/gateway 的 displayParts 同一尺度）。
func last4(plaintext string) string {
	if len(plaintext) < 8 {
		return ""
	}
	return plaintext[len(plaintext)-4:]
}
