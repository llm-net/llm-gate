package gateway

// keyauth.go 承载数据面客户端 Key 鉴权的 SQLite 侧（iteration-4 Phase 4）：
// KeyAuthorizer 接口由 withAuth 消费；StoreKeyAuthorizer 以 SHA-256 摘要
// 点查 store（设计决策 5：无缓存层，禁用/启用即时生效），并对 last_used_at
// 做 60s 内存节流合并写；YAML api_keys 由 ImportConfigKeys 在启动时幂等
// 导入（设计决策 7：导入后 SQLite 是唯一事实源）。
//
// §15.1 纪律：本文件的日志与错误信息不含 Key 明文——指认条目一律经
// logging.RedactKey，导入结果只记条数。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// ClientIdentity 是数据面鉴权通过后的调用方身份快照，自 iteration-9 起就是
// store.KeyAuth 整行：KeyID 是任务面（aigc_tasks）的归属判据，
// KeyID/KeyDisplay 同时是账本的密钥维度与访问日志的调用方标识，四项限额
// （nil = 不限）与按量额度剩余供预算准入**就地**判——鉴权那一次点查已经
// 把它们带回来了，准入不必再查一次库。
//
// KeyDisabled 在这里恒为 false（为真就不会放行），保留只是因为整行嵌入比
// 逐字段抄写少一处漏抄的可能。
type ClientIdentity struct {
	store.KeyAuth
}

// KeyAuthorizer 校验数据面客户端 Key。ok=false 统一表示拒绝——未知 Key、
// Key 被禁用、存储故障，调用方一律回 401 不区分原因（§11.1 不给探测面）；
// 此时 ident 为零值。
type KeyAuthorizer interface {
	Authorize(ctx context.Context, key string) (ident ClientIdentity, ok bool)
	// AuthorizeDigest 供设备签发的代理凭证回查原客户端 Key。调用方只可
	// 传本设备按同一 keyDigest 规则生成并签名保护的摘要，不能把客户端输入
	// 当作摘要直接透传。
	AuthorizeDigest(ctx context.Context, digest string) (ident ClientIdentity, ok bool)
}

// touchInterval 是 last_used_at 的合并写窗口：同一 Key 两次落库间隔不小于
// 此值，认证热路径不因写放大受累。
const touchInterval = 60 * time.Second

// StoreKeyAuthorizer 是 KeyAuthorizer 的 SQLite 实现：对明文取 SHA-256
// 摘要点查（与管理面签发同一算法），每请求直查库、无缓存层（决策 5——
// 换取禁用即时生效与零失效一致性问题；板上规模点查 µs 级）。
type StoreKeyAuthorizer struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time // 节流判据时钟；测试注入，生产恒 time.Now

	mu        sync.Mutex
	lastTouch map[int64]time.Time // keyID → 上次 last_used_at 落库时间
}

// NewStoreKeyAuthorizer 装配 SQLite Key 鉴权器；st 与管理面共享同一库。
func NewStoreKeyAuthorizer(st *store.Store, logger *slog.Logger) *StoreKeyAuthorizer {
	return &StoreKeyAuthorizer{st: st, log: logger, now: time.Now, lastTouch: map[int64]time.Time{}}
}

// Authorize 实现 KeyAuthorizer：命中且 Key 未禁用才放行，随后节流刷新
// last_used_at。存储故障按拒绝处理并记 Error 日志（store 错误文本只含
// 约束/列名，Key 指认经 RedactKey，无明文）。
func (a *StoreKeyAuthorizer) Authorize(ctx context.Context, key string) (ClientIdentity, bool) {
	return a.authorizeDigest(ctx, keyDigest(key), logging.RedactKey(key))
}

// AuthorizeDigest 实现设备签发代理凭证的摘要回查。它与明文入口走同一条
// SQLite 点查及 last_used_at 节流路径，因此禁用、删除和限额修改仍在下一个
// 请求即时生效。
func (a *StoreKeyAuthorizer) AuthorizeDigest(ctx context.Context, digest string) (ClientIdentity, bool) {
	return a.authorizeDigest(ctx, digest, "device-signed-token")
}

func (a *StoreKeyAuthorizer) authorizeDigest(ctx context.Context, digest, logRef string) (ClientIdentity, bool) {
	ka, err := a.st.LookupKeyByDigest(ctx, digest)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.log.Error("Key 鉴权存储查询失败",
				"api_key", logRef,
				"err", err.Error(),
			)
		}
		return ClientIdentity{}, false
	}
	if ka.KeyDisabled {
		return ClientIdentity{}, false
	}
	a.touch(ctx, ka.KeyID)
	return ClientIdentity{KeyAuth: *ka}, true
}

// touch 节流刷新 last_used_at：距同一 Key 上次落库不足 touchInterval 则
// 跳过；先在锁内登记再于锁外写库（DB IO 不占锁）。写失败只记 Warn——
// 最近使用时间是尽力而为的元数据，不影响放行。
func (a *StoreKeyAuthorizer) touch(ctx context.Context, keyID int64) {
	now := a.now()
	a.mu.Lock()
	if last, ok := a.lastTouch[keyID]; ok && now.Sub(last) < touchInterval {
		a.mu.Unlock()
		return
	}
	a.lastTouch[keyID] = now
	a.mu.Unlock()
	if err := a.st.TouchKeyLastUsed(ctx, keyID); err != nil {
		a.log.Warn("刷新 Key last_used_at 失败", "key_id", keyID, "err", err.Error())
	}
}

// keyDigest 返回 Key 明文的 SHA-256 十六进制摘要——库中 key_digest 的唯一
// 形态，与管理面签发（internal/admin）同一算法。
func keyDigest(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// importLabel 标记启动导入的 Key 行来源（正式 Key 经管理界面签发）。
const importLabel = "imported-from-config"

// ImportConfigKeys 把 YAML api_keys 幂等导入 SQLite。逐条按摘要查重：
// 摘要已存在（含已禁用——禁用状态不被导入复活）整条跳过；缺失则建行。
// 返回新建与跳过条数；日志只记条数不记 Key 物料（§15.1）。
func ImportConfigKeys(ctx context.Context, st *store.Store, keys []config.APIKey, logger *slog.Logger) (imported, skipped int, err error) {
	for i, k := range keys {
		digest := keyDigest(k.Key)
		if _, err := st.LookupKeyByDigest(ctx, digest); err == nil {
			skipped++
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return imported, skipped, fmt.Errorf("导入 api_keys[%d]（key %s）: %w", i, logging.RedactKey(k.Key), err)
		}
		prefix, last4 := displayParts(k.Key)
		// 明文一并封存（0012）：YAML 作者本就持有这份明文，管理台照样可自助复制。
		if _, err := st.CreateAPIKey(ctx, importLabel, digest, prefix, last4, k.Key); err != nil {
			if errors.Is(err, store.ErrConflict) {
				skipped++ // 摘要已被并发落库（防御分支）：视同已存在
				continue
			}
			return imported, skipped, fmt.Errorf("导入 api_keys[%d]（key %s）: %w", i, logging.RedactKey(k.Key), err)
		}
		imported++
	}
	if len(keys) > 0 {
		logger.Info("YAML api_keys 已导入 SQLite",
			"imported", imported, "skipped", skipped, "total", len(keys))
	}
	return imported, skipped, nil
}

// displayParts 从导入 Key 明文取展示片段。长 Key（≥24 字节，至少 8 字节仍
// 被遮蔽）对齐管理面签发的形态：前 12 位 + 末 4 位；较短 Key 收窄到
// logging.RedactKey 同款尺度（≥8 字节仅 4 位前缀）；更短或含非可打印
// ASCII 的 Key 无法在不泄露明文的前提下展示，一律留空。
func displayParts(key string) (prefix, last4 string) {
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return "", ""
		}
	}
	switch {
	case len(key) >= 24:
		return key[:12], key[len(key)-4:]
	case len(key) >= 8:
		return key[:4], ""
	default:
		return "", ""
	}
}
