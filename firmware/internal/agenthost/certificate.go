package agenthost

// 设备访问证书：一把 ed25519 SSH 密钥对，设备拿它免密登录已纳管的主机。
//
// 私钥用设备密钥封存在 settings（AAD = 键名，见 store.SetSealedSetting），
// **永不出本包**：不进 API 响应、不进日志、不进审计 detail，也不落成文件。
// 公钥是公开值，管理员可以下载成 .pub 手工装到主机上。
//
// 上一把证书（公钥 + 私钥）保留一份：轮换证书时新公钥还没装到主机上，只能用
// 上一把登录进去换。某台主机当时不在线、错过了这次轮换，之后「补发证书」仍然
// 能用上一把连上，不必再要一次口令。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"
)

// settings 键名。公开面（公钥、生成时刻）是明文单值；两把私钥封存。
const (
	SettingPublicKey     = "agent_host.cert_public"
	SettingCreatedAt     = "agent_host.cert_created_at"
	SettingPrevPublicKey = "agent_host.cert_public_prev"

	settingPrivateKey     = "agent_host.cert_private"
	settingPrevPrivateKey = "agent_host.cert_private_prev"
)

// keyComment 是写进 authorized_keys 的注释：主机上的人一眼看得出这行是谁装的。
const keyComment = "llmgate-agent"

// Certificate 是访问证书的公开面。私钥不在其中，也没有任何字段能导出它。
type Certificate struct {
	// PublicKey 是 authorized_keys 单行（含 keyComment 注释）。
	PublicKey string `json:"public_key"`
	// Fingerprint 是 SHA256:… 形状的公钥指纹，与 ssh-keygen -lf 一致。
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"key_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// certKey 是证书的进程内形态：公钥行 + 私钥。它持有密钥物料，因此自遮蔽
// （String / GoString / LogValue 都是值接收者，指针也拿得到），一次顺手的
// %+v 或 slog.Any 印不出私钥（§15.1）。
type certKey struct {
	public     string
	privatePEM string
	signer     ssh.Signer
	createdAt  time.Time
}

// String / GoString / LogValue：只吐公开面（指纹），不吐私钥。
func (k certKey) String() string   { return "agenthost.certKey(" + k.safeFingerprint() + ")" }
func (k certKey) GoString() string { return k.String() }
func (k certKey) LogValue() slog.Value {
	return slog.GroupValue(slog.String("fingerprint", k.safeFingerprint()))
}

func (k certKey) safeFingerprint() string {
	if k.signer == nil {
		return "unset"
	}
	return ssh.FingerprintSHA256(k.signer.PublicKey())
}

// newCertKey 生成一把 ed25519 密钥对。
func newCertKey(now time.Time) (*certKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成访问证书: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("生成访问证书: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("生成访问证书: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, keyComment)
	if err != nil {
		return nil, fmt.Errorf("生成访问证书: %w", err)
	}
	return &certKey{
		public:     authorizedLine(sshPub) + " " + keyComment,
		privatePEM: string(pem.EncodeToMemory(block)),
		signer:     signer,
		createdAt:  now,
	}, nil
}

// describe 把进程内形态折成公开面。
func (k *certKey) describe() *Certificate {
	return &Certificate{
		PublicKey:   k.public,
		Fingerprint: ssh.FingerprintSHA256(k.signer.PublicKey()),
		KeyType:     k.signer.PublicKey().Type(),
		CreatedAt:   k.createdAt,
	}
}

// fingerprint 是这把证书公钥的 SHA256 指纹（主机行里记的就是它）。
func (k *certKey) fingerprint() string { return ssh.FingerprintSHA256(k.signer.PublicKey()) }

// loadCert 读当前证书；没生成过返回 (nil, nil)。
func (m *Manager) loadCert(ctx context.Context) (*certKey, error) {
	return m.loadCertAt(ctx, SettingPublicKey, settingPrivateKey, SettingCreatedAt)
}

// loadPrevCert 读上一把证书；没有返回 (nil, nil)。
func (m *Manager) loadPrevCert(ctx context.Context) (*certKey, error) {
	return m.loadCertAt(ctx, SettingPrevPublicKey, settingPrevPrivateKey, "")
}

func (m *Manager) loadCertAt(ctx context.Context, pubKey, privKey, createdKey string) (*certKey, error) {
	public, err := m.st.GetSetting(ctx, pubKey)
	if err != nil {
		return nil, err
	}
	privatePEM, err := m.st.GetSealedSetting(ctx, privKey)
	if err != nil {
		return nil, err
	}
	if public == "" || privatePEM == "" {
		return nil, nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		// 只说「读不出来」：错误里不带私钥物料，也不带密文（§15.1）。
		return nil, &Error{Code: CodeCertificateUnreadable, Msg: "设备上封存的访问证书私钥读不出来（设备密钥被替换或密文损坏），请重新生成证书并重新纳管各主机。"}
	}
	k := &certKey{public: public, privatePEM: privatePEM, signer: signer}
	if createdKey != "" {
		if raw, err := m.st.GetSetting(ctx, createdKey); err == nil && raw != "" {
			if at, err := time.Parse(time.RFC3339, raw); err == nil {
				k.createdAt = at
			}
		}
	}
	return k, nil
}

// saveCert 落一把新证书：当前槽写新的，上一槽写传入的旧证书（keepPrev 为 nil
// 表示清空上一槽）。两个槽必须同时生效——只落一半会得到「私钥是新的、公钥还是
// 旧的」这种谁也登不上的状态，所以整组走一次事务。
func (m *Manager) saveCert(ctx context.Context, next *certKey, keepPrev *certKey) error {
	plain := map[string]string{
		SettingPublicKey:     next.public,
		SettingCreatedAt:     next.createdAt.UTC().Format(time.RFC3339),
		SettingPrevPublicKey: "",
	}
	sealed := map[string]string{
		settingPrivateKey:     next.privatePEM,
		settingPrevPrivateKey: "",
	}
	if keepPrev != nil {
		plain[SettingPrevPublicKey] = keepPrev.public
		sealed[settingPrevPrivateKey] = keepPrev.privatePEM
	}
	return m.st.SetSettingsAtomic(ctx, plain, sealed)
}
