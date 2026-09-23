package store

// credentials 仓储：「智能体 → 凭证管理」交给智能体使用的第三方凭证。约定同 repo.go：
// context 化、走 prepared statement、未命中 → ErrNotFound、唯一性冲突 → ErrConflict、
// 时间入库经 fmtTime。
//
// 一行是一份凭证：类型（kind，目前只有 git）、所属站点（host，git 只认 github.com /
// gitee.com）、那边的账号（username）与令牌 / 口令。令牌经设备密钥封存入库（AAD 钉本行
// id，见 seal.go），行里只留末 4 位作辨认；明文只在 [Store.CreateCredential] /
// [Store.UpdateCredential] 的入参与 [Store.GetCredentialSecret] 的返回值里出现（§15.1），
// 列表与单行读数（[Credential]）没有它。
//
// 入参校验失败返回包着 [ErrInvalidCredential] 的错误，管理面据此答 400；错误文本不含令牌。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// credentials.kind 的封闭词汇表（与 0050 迁移的 CHECK 同步维护）。
const (
	// CredentialKindGit：git 托管站点的 HTTPS 凭证（账号 + 令牌 / 口令）。
	CredentialKindGit = "git"
)

// ValidCredentialKind 报告 kind 是否在词汇表里。
func ValidCredentialKind(kind string) bool { return kind == CredentialKindGit }

// GitCredentialHosts 是 git 凭证可选的托管站点（封闭词汇表，界面下拉即此表）。
var GitCredentialHosts = []string{"github.com", "gitee.com"}

// ValidGitCredentialHost 报告 host 是否在 git 托管站点词汇表里。
func ValidGitCredentialHost(host string) bool {
	for _, h := range GitCredentialHosts {
		if h == host {
			return true
		}
	}
	return false
}

// ErrInvalidCredential 是凭证入参不合法的哨兵；具体原因在包着它的错误文本里。
var ErrInvalidCredential = errors.New("凭证无效")

// 入参尺寸上限。
const (
	credentialNameMaxRunes = 64
	credentialUserMaxRunes = 128
	credentialSecretMax    = 4096
)

// credentialColumns 是 SELECT 列表（与 scanCredential 的扫描顺序一一对应）。密文刻意
// 不在其中：它不进 Credential 结构体，只由 GetCredentialSecret 单独点查。
const credentialColumns = `id, kind, name, host, username, secret_hint, created_at, updated_at`

// Credential 是 credentials 表的一行公开面：没有令牌。
type Credential struct {
	ID       string
	Kind     string
	Name     string
	Host     string
	Username string
	// SecretHint 是令牌末 4 位（短于 8 字节的令牌留空），只作辨认。
	SecretHint string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CredentialSecret 是一份凭证连同解封后的令牌。它持有凭据物料，因此自遮蔽：
// String / GoString / LogValue / MarshalJSON 都是值接收者、只吐公开面，一次顺手的
// %+v、slog.Any 或 json.Marshal 印不出令牌（§15.1）；令牌只经 Plaintext 取。
type CredentialSecret struct {
	Credential
	secret string
}

// Plaintext 是令牌明文。
func (c CredentialSecret) Plaintext() string { return c.secret }

func (c CredentialSecret) String() string {
	return "store.CredentialSecret(" + c.Kind + " " + c.Username + "@" + c.Host + ")"
}
func (c CredentialSecret) GoString() string { return c.String() }
func (c CredentialSecret) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", c.ID), slog.String("kind", c.Kind),
		slog.String("host", c.Host), slog.String("username", c.Username))
}
func (c CredentialSecret) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Host     string `json:"host"`
		Username string `json:"username"`
	}{c.ID, c.Kind, c.Host, c.Username})
}

// NewCredential 是 [Store.CreateCredential] 的入参。
type NewCredential struct {
	Kind     string
	Name     string
	Host     string
	Username string
	// Secret 是令牌 / 口令明文，必填；只在这个入参里出现一次。
	Secret string
	// CreatedAt 零值取当前时间。
	CreatedAt time.Time
}

// CredentialPatch 是 [Store.UpdateCredential] 的入参：nil 字段保持原值。Secret 指向空串
// 与 nil 同义（界面「留空保持不变」），类型不可改。
type CredentialPatch struct {
	Name     *string
	Host     *string
	Username *string
	Secret   *string
}

// CreateCredential 建一行凭证并返回公开面。同一类型 + 站点 + 账号重复即 ErrConflict。
func (s *Store) CreateCredential(ctx context.Context, nc NewCredential) (*Credential, error) {
	if !ValidCredentialKind(nc.Kind) {
		return nil, fmt.Errorf("%w: 不支持的凭证类型", ErrInvalidCredential)
	}
	name, err := normalizeCredentialName(nc.Name)
	if err != nil {
		return nil, err
	}
	host, err := normalizeCredentialHost(nc.Kind, nc.Host)
	if err != nil {
		return nil, err
	}
	user, err := normalizeCredentialUser(nc.Username)
	if err != nil {
		return nil, err
	}
	if err := checkCredentialSecret(nc.Secret); err != nil {
		return nil, err
	}
	at := nc.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := NewULID(at)
	sealed, err := s.seal(nc.Secret, credentialAAD(id))
	if err != nil {
		return nil, fmt.Errorf("添加凭证: 封存令牌: %w", err)
	}
	ts := fmtTime(at)
	if _, err := s.stmtCreateCredential.ExecContext(ctx,
		id, nc.Kind, name, host, user, sealed, last4(nc.Secret), ts, ts); err != nil {
		return nil, fmt.Errorf("添加凭证: %w", mapErr(err))
	}
	return s.GetCredential(ctx, id)
}

// ListCredentials 按创建顺序列出全部凭证（ULID 字典序即时间序）。
func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.stmtListCredentials.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出凭证: %w", err)
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("列出凭证: %w", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出凭证: %w", err)
	}
	return out, nil
}

// GetCredential 按 id 取一行公开面；不存在返回 ErrNotFound。
func (s *Store) GetCredential(ctx context.Context, id string) (*Credential, error) {
	c, err := scanCredential(s.stmtGetCredential.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取凭证 %s: %w", id, err)
	}
	return c, nil
}

// GetCredentialSecret 取一行连同解封后的令牌；不存在返回 ErrNotFound。解不开（设备密钥
// 被替换或密文损坏）返回可读错误，指明补救动作；错误文本不含密文。
func (s *Store) GetCredentialSecret(ctx context.Context, id string) (CredentialSecret, error) {
	c, err := s.GetCredential(ctx, id)
	if err != nil {
		return CredentialSecret{}, err
	}
	var sealed string
	if err := s.stmtGetCredentialSecret.QueryRowContext(ctx, id).Scan(&sealed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CredentialSecret{}, ErrNotFound
		}
		return CredentialSecret{}, fmt.Errorf("读取凭证 %s: %w", id, err)
	}
	secret, err := s.open(sealed, credentialAAD(id))
	if err != nil {
		return CredentialSecret{}, fmt.Errorf("凭证 %s 解封失败：%w（%s 被替换或密文损坏，请在凭证管理里重新填写令牌）",
			id, err, DeviceKeyFileName)
	}
	return CredentialSecret{Credential: *c, secret: secret}, nil
}

// UpdateCredential 按 patch 改名称、站点、账号与令牌（nil 或指向空串的 Secret 保持原令牌）；
// 不存在返回 ErrNotFound，改后与别的行撞上唯一约束返回 ErrConflict。
func (s *Store) UpdateCredential(ctx context.Context, id string, p CredentialPatch) (*Credential, error) {
	cur, err := s.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	name, host, user := cur.Name, cur.Host, cur.Username
	if p.Name != nil {
		if name, err = normalizeCredentialName(*p.Name); err != nil {
			return nil, err
		}
	}
	if p.Host != nil {
		if host, err = normalizeCredentialHost(cur.Kind, *p.Host); err != nil {
			return nil, err
		}
	}
	if p.Username != nil {
		if user, err = normalizeCredentialUser(*p.Username); err != nil {
			return nil, err
		}
	}
	ts := fmtTime(time.Now())
	var res sql.Result
	if p.Secret != nil && *p.Secret != "" {
		if err := checkCredentialSecret(*p.Secret); err != nil {
			return nil, err
		}
		sealed, err := s.seal(*p.Secret, credentialAAD(id))
		if err != nil {
			return nil, fmt.Errorf("修改凭证 %s: 封存令牌: %w", id, err)
		}
		res, err = s.stmtUpdateCredentialWithSecret.ExecContext(ctx, name, host, user, sealed, last4(*p.Secret), ts, id)
		if err != nil {
			return nil, fmt.Errorf("修改凭证 %s: %w", id, mapErr(err))
		}
	} else {
		res, err = s.stmtUpdateCredential.ExecContext(ctx, name, host, user, ts, id)
		if err != nil {
			return nil, fmt.Errorf("修改凭证 %s: %w", id, mapErr(err))
		}
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("修改凭证 %s: %w", id, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetCredential(ctx, id)
}

// DeleteCredential 删一行；不存在返回 ErrNotFound。
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	res, err := s.stmtDeleteCredential.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("删除凭证 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删除凭证 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// credentialAAD 是 credentials.secret_sealed 的附加认证数据：钉本行 id。
func credentialAAD(id string) []byte { return []byte("credential:" + id) }

func normalizeCredentialName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > credentialNameMaxRunes {
		return "", fmt.Errorf("%w: 名称不能超过 %d 个字符", ErrInvalidCredential, credentialNameMaxRunes)
	}
	if hasControl(name) {
		return "", fmt.Errorf("%w: 名称不能含控制字符", ErrInvalidCredential)
	}
	return name, nil
}

func normalizeCredentialHost(kind, host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if kind == CredentialKindGit && !ValidGitCredentialHost(host) {
		return "", fmt.Errorf("%w: 站点只能是 %s", ErrInvalidCredential, strings.Join(GitCredentialHosts, " / "))
	}
	return host, nil
}

func normalizeCredentialUser(user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", fmt.Errorf("%w: 用户名不能为空", ErrInvalidCredential)
	}
	if utf8.RuneCountInString(user) > credentialUserMaxRunes {
		return "", fmt.Errorf("%w: 用户名不能超过 %d 个字符", ErrInvalidCredential, credentialUserMaxRunes)
	}
	if strings.IndexFunc(user, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("%w: 用户名不能含空白或控制字符", ErrInvalidCredential)
	}
	return user, nil
}

func checkCredentialSecret(secret string) error {
	if secret == "" {
		return fmt.Errorf("%w: 令牌不能为空", ErrInvalidCredential)
	}
	if len(secret) > credentialSecretMax {
		return fmt.Errorf("%w: 令牌不能超过 %d 字节", ErrInvalidCredential, credentialSecretMax)
	}
	if hasControl(secret) {
		return fmt.Errorf("%w: 令牌不能含换行或控制字符", ErrInvalidCredential)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func scanCredential(row interface{ Scan(...any) error }) (*Credential, error) {
	var (
		c         Credential
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.Host, &c.Username, &c.SecretHint, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("解析创建时刻: %w", err)
	}
	c.CreatedAt = t
	if t, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("解析更新时刻: %w", err)
	}
	c.UpdatedAt = t
	return &c, nil
}
