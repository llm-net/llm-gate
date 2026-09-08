package admin

// API密钥 管理端点：GET/POST /admin/v1/keys、PATCH/DELETE /admin/v1/keys/{id}、
// POST /admin/v1/keys/{id}/plaintext、POST /admin/v1/keys/{id}/metered-allowance。
//
// 设备只有一个管理员，密钥因此没有「属主」这一维——一台设备一串密钥，签发、
// 复制、改标签、限额、启停、删除都在同一组端点上（0019 之前分成「管理端」与「本人
// 自助」两组，那个区分随用户概念一起退场）。
//
// 明文纪律（2026-08-12 产品决定放宽，AGENTS.md 同步开例外）：明文以设备密钥
// 封存入库（store 层，AAD 钉摘要），只经两个响应外流——创建响应与复制端点；
// 列表接口只回 plaintext_available 布尔，既不回明文也不回摘要；日志与审计
// detail 照旧永不含明文。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Key 变更审计事件（entity 形如 key:7；detail 只含 label/display_prefix
// 等非敏感字段，明文与摘要一律不进审计）。
const (
	EventKeyCreate  = "key.create"
	EventKeyDisable = "key.disable"
	EventKeyEnable  = "key.enable"
	EventKeyDelete  = "key.delete"
	// EventKeyReveal 记经复制端点解封明文（凭据被读取要留痕；detail 与其他
	// 事件同规——只有 label 与前缀，永不含明文）。
	EventKeyReveal = "key.reveal"
	// EventKeyLimitsUpdate 记密钥级预算与 RPM 变更（钱的口子改没改，翻审计时
	// 要一眼看得见）。
	EventKeyLimitsUpdate = "key.limits_update"
	// EventKeyLabelUpdate 记展示标签变更；标签不参与鉴权或计量归属。
	EventKeyLabelUpdate = "key.label_update"
	// EventKeyMeteredAllowanceAdjust 记管理员增减按量额度。金额是配置，不是
	// 凭据，可在审计 detail 中记录有符号增量与调整后的剩余。
	EventKeyMeteredAllowanceAdjust = "key.metered_allowance_adjust"
)

// Key 明文形态：`sk_` + 43 位 base62（62^43 ≈ 2^256）；display_prefix 取
// 明文前 12 位（含 sk_），display_last4 取末 4 位。数据面按整串摘要鉴权，
// 与前缀无关，因此从配置导入的、别的形状的密钥同样有效。
const (
	keyPlaintextPrefix  = "sk_"
	keyRandLen          = 43
	keyDisplayPrefixLen = 12
	base62Alphabet      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// labelMaxRunes 是 label 的长度上限（展示用自由文本，防超长灌入）。
	labelMaxRunes = 128
)

func entityKey(id int64) string { return fmt.Sprintf("key:%d", id) }

// pathID 解析路径参数 {id}；非正整数视为目标不存在。
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// newKeyPlaintext 生成 Key 明文。拒绝采样：62 不整除 256，直接取模有偏，
// 只接受 < 248（= 62×4）的随机字节。
func newKeyPlaintext() (string, error) {
	out := make([]byte, 0, keyRandLen)
	var buf [64]byte
	for len(out) < keyRandLen {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("生成 Key: %w", err)
		}
		for _, b := range buf {
			if b >= 248 {
				continue
			}
			out = append(out, base62Alphabet[int(b)%62])
			if len(out) == keyRandLen {
				break
			}
		}
	}
	return keyPlaintextPrefix + string(out), nil
}

// keyDigest 返回明文的 SHA-256 十六进制摘要（库中唯一形态；数据面
// LookupKeyByDigest 以同一算法点查）。
func keyDigest(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// keyJSON 是 Key 在管理 API 里的对外形态：不含明文，也不含摘要
// （摘要无展示必要，不给拿库外比对的材料）。LastUsedAt 零值省略 = 从未使用。
type keyJSON struct {
	ID            int64  `json:"id"`
	Label         string `json:"label"`
	DisplayPrefix string `json:"display_prefix"`
	DisplayLast4  string `json:"display_last4"`
	Disabled      bool   `json:"disabled"`
	// PlaintextAvailable 表示库里封存有明文、可自助复制；false = 签发于
	// 0012 之前，界面对这些行不给复制按钮（明文物理上不可恢复，只能重签）。
	PlaintextAvailable bool `json:"plaintext_available"`
	// 密钥级限额（微元 / 每分钟请求数，null = 不限）。
	BudgetDayMicro   *int64 `json:"budget_day_micro"`
	BudgetWeekMicro  *int64 `json:"budget_week_micro"`
	BudgetMonthMicro *int64 `json:"budget_month_micro"`
	RPMLimit         *int64 `json:"rpm_limit"`
	// MeteredAllowanceMicro 是预算耗尽后继续使用的按量额度即时剩余。数据库
	// 值会减去计量器尚未批量落库的待扣，避免界面最多五分钟虚高。
	MeteredAllowanceMicro int64     `json:"metered_allowance_micro"`
	CreatedAt             time.Time `json:"created_at"`
	LastUsedAt            time.Time `json:"last_used_at,omitzero"`
	// Spend 是这把密钥当前自然日/周/月的已消费额（整数微元）。它与同一结构里
	// 的 budget_*_micro 成对读：一个是花了多少，一个是能花多少——密钥列表不摆
	// 已消费额，等于让管理员盯着一排永远不动的上限。
	//
	// 恒为**当前**窗口，与「用量」页的任何区间参数无关：按别的区间重算出来的
	// 数字配不上额度，只会让人以为自己还剩很多。
	//
	// 读数来自计量器的内存计数器（预算准入判的就是它），不查库、不触发上游，
	// 每把密钥一次 map 查找。**计量未装配时整块字段缺席而不是填 0**：没有计量
	// 就没有「花了多少」这回事，填 0 会把「没接线」读成「没花钱」（同用量端点
	// 宁可 503 也不回空报表的口径）。
	Spend *keySpendJSON `json:"spend,omitempty"`
}

// keySpendJSON 是一把密钥当前三个预算窗口的已消费额（整数微元）。
type keySpendJSON struct {
	DayMicro   int64 `json:"day_micro"`
	WeekMicro  int64 `json:"week_micro"`
	MonthMicro int64 `json:"month_micro"`
}

func toKeyJSON(k *store.APIKey) keyJSON {
	return keyJSON{
		ID:                    k.ID,
		Label:                 k.Label,
		DisplayPrefix:         k.DisplayPrefix,
		DisplayLast4:          k.DisplayLast4,
		Disabled:              k.Disabled,
		PlaintextAvailable:    k.PlaintextAvailable,
		BudgetDayMicro:        k.BudgetDayMicro,
		BudgetWeekMicro:       k.BudgetWeekMicro,
		BudgetMonthMicro:      k.BudgetMonthMicro,
		RPMLimit:              k.RPMLimit,
		MeteredAllowanceMicro: k.MeteredAllowanceMicro,
		CreatedAt:             k.CreatedAt,
		LastUsedAt:            k.LastUsedAt,
	}
}

// withSpend 给一行挂上当前窗口的已消费额；计量未装配时原样返回（字段缺席）。
func (s *Server) withSpend(kj keyJSON) keyJSON {
	if s.usage == nil {
		return kj
	}
	sp := s.usage.Spend(kj.ID)
	kj.Spend = &keySpendJSON{DayMicro: sp.DayMicro, WeekMicro: sp.WeekMicro, MonthMicro: sp.MonthMicro}
	kj.MeteredAllowanceMicro -= sp.MeteredAllowancePendingMicro
	if kj.MeteredAllowanceMicro < 0 {
		kj.MeteredAllowanceMicro = 0
	}
	return kj
}

// keyCreatedResponse 是签发响应。Plaintext 当场可复制；此后随时可经复制端点
// 重新取回（0012 起明文封存入库）。
type keyCreatedResponse struct {
	Key       keyJSON `json:"key"`
	Plaintext string  `json:"plaintext"`
}

// keyResponse 包装单密钥响应。
type keyResponse struct {
	Key keyJSON `json:"key"`
}

// validateLabel 校验展示用自由文本长度（防超长灌入）。
func (s *Server) validateLabel(w http.ResponseWriter, label string) bool {
	if utf8.RuneCountInString(label) > labelMaxRunes {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("label 最长 %d 个字符", labelMaxRunes))
		return false
	}
	return true
}

// issueKey 生成明文、落摘要 + 封存明文行并写审计（明文纪律只在这一处成立）。
func (s *Server) issueKey(ctx context.Context, label, ip string) (*store.APIKey, string, error) {
	plaintext, err := newKeyPlaintext()
	if err != nil {
		return nil, "", err
	}
	k, err := s.st.CreateAPIKey(ctx, label, keyDigest(plaintext),
		plaintext[:keyDisplayPrefixLen], plaintext[len(plaintext)-4:], plaintext)
	if err != nil {
		return nil, "", err
	}
	s.audit(ctx, store.AuditEvent{
		Event:    EventKeyCreate,
		Entity:   entityKey(k.ID),
		Detail:   fmt.Sprintf("label=%s display_prefix=%s", k.Label, k.DisplayPrefix),
		RemoteIP: ip,
	})
	return k, plaintext, nil
}

// handleListKeys 列出全部 Key，永不含明文与摘要。
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListAPIKeys(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := struct {
		Keys []keyJSON `json:"keys"`
	}{Keys: make([]keyJSON, 0, len(keys))}
	for i := range keys {
		out.Keys = append(out.Keys, s.withSpend(toKeyJSON(&keys[i])))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateKey 签发一把 Key。明文在本响应当场可复制，此后随时可经
// POST /admin/v1/keys/{id}/plaintext 重新取回。
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.validateLabel(w, req.Label) {
		return
	}
	k, plaintext, err := s.issueKey(r.Context(), req.Label, remoteIP(r))
	if err != nil {
		// 摘要唯一冲突在 256 位随机下不可能发生，一律按内部错误处理。
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, keyCreatedResponse{Key: toKeyJSON(k), Plaintext: plaintext})
}

// handleDeleteKey 删除一把 Key（不可撤销：封存明文随行删除，这把 Key 再无
// 恢复可能，误删只能重签一把新的）。删除后摘要点查落空，数据面下一个请求
// 即 401。
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	k, err := s.st.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	if err := s.st.DeleteAPIKey(r.Context(), k.ID); err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventKeyDelete,
		Entity:   entityKey(k.ID),
		Detail:   fmt.Sprintf("label=%s display_prefix=%s", k.Label, k.DisplayPrefix),
		RemoteIP: remoteIP(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleRevealKey 解封 Key 明文（自助复制；2026-08-12 产品决定，明文外流仅有
// 的第二个响应）。禁用的 Key 照样可复制——禁用挡的是数据面，不是管理员看自己
// 的凭据。无封存明文（签发于 0012 之前）与解封失败都是 409：状态不是故障，
// 出路都是重签一把。成功写 key.reveal 审计（凭据被读取要留痕，detail 永不
// 含明文）。
func (s *Server) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	k, err := s.st.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	plaintext, err := s.st.GetAPIKeyPlaintext(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrKeyPlaintextMissing):
			writeError(w, http.StatusConflict, "plaintext_unavailable",
				"该 Key 签发于旧版本，明文未留存，无法复制；请新建一把替换")
		case errors.Is(err, store.ErrKeyPlaintextUnreadable):
			writeError(w, http.StatusConflict, "plaintext_unreadable",
				"明文解密失败（设备密钥被替换或密文损坏），无法复制；请新建一把替换")
		default:
			s.writeKeyError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventKeyReveal,
		Entity:   entityKey(k.ID),
		Detail:   fmt.Sprintf("label=%s display_prefix=%s", k.Label, k.DisplayPrefix),
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Plaintext string `json:"plaintext"`
	}{plaintext})
}

// writeKeyError 把 Key 写入路径的 store 错误映射为统一错误体。
func (s *Server) writeKeyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	s.internalError(w, r, err)
}

// handlePatchKey 更新 Key 展示标签、禁用位与四项限额（日/周/月预算、RPM）。
// 决策 5（数据面无缓存层）保证禁用位与限额对 /v1/* 即时生效——它们随下一个
// 请求的那一次摘要点查带回；标签只影响管理界面展示。
func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	var req struct {
		Label    *string `json:"label"`
		Disabled *bool   `json:"disabled"`
		// 四项限额：不传 = 不改，null = 清除（不限），数字 = 额度 / 次数。
		BudgetDayMicro   json.RawMessage `json:"budget_day_micro"`
		BudgetWeekMicro  json.RawMessage `json:"budget_week_micro"`
		BudgetMonthMicro json.RawMessage `json:"budget_month_micro"`
		RPMLimit         json.RawMessage `json:"rpm_limit"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Label == nil && req.Disabled == nil && len(req.BudgetDayMicro) == 0 && len(req.BudgetWeekMicro) == 0 &&
		len(req.BudgetMonthMicro) == 0 && len(req.RPMLimit) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request",
			"至少要带 label / disabled / budget_day_micro / budget_week_micro / budget_month_micro / rpm_limit 之一")
		return
	}
	if req.Label != nil && !s.validateLabel(w, *req.Label) {
		return
	}
	// 四项限额先全部校验：任何一项不合法都不该留下半个已生效的改动。
	budgetDay, dayGiven, err := parseLimit(req.BudgetDayMicro, "budget_day_micro")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	budgetWeek, weekGiven, err := parseLimit(req.BudgetWeekMicro, "budget_week_micro")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	budgetMonth, monthGiven, err := parseLimit(req.BudgetMonthMicro, "budget_month_micro")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	rpm, rpmGiven, err := parseLimit(req.RPMLimit, "rpm_limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	target, err := s.st.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	// 展示标签：空串 = 清除；幂等无变更不写库、不写审计。
	if req.Label != nil && *req.Label != target.Label {
		oldLabel := target.Label
		if err := s.st.SetAPIKeyLabel(r.Context(), id, *req.Label); err != nil {
			s.writeKeyError(w, r, err)
			return
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:    EventKeyLabelUpdate,
			Entity:   entityKey(id),
			Detail:   fmt.Sprintf("old_label=%q new_label=%q display_prefix=%s", oldLabel, *req.Label, target.DisplayPrefix),
			RemoteIP: remoteIP(r),
		})
		target.Label = *req.Label
	}
	// 四列一并写（存储接口如此）：本次没传的那些沿用现值。
	if dayGiven || weekGiven || monthGiven || rpmGiven {
		day := pickLimit(budgetDay, dayGiven, target.BudgetDayMicro)
		week := pickLimit(budgetWeek, weekGiven, target.BudgetWeekMicro)
		month := pickLimit(budgetMonth, monthGiven, target.BudgetMonthMicro)
		limit := pickLimit(rpm, rpmGiven, target.RPMLimit)
		if !sameLimit(day, target.BudgetDayMicro) || !sameLimit(week, target.BudgetWeekMicro) ||
			!sameLimit(month, target.BudgetMonthMicro) || !sameLimit(limit, target.RPMLimit) {
			if err := s.st.SetAPIKeyLimits(r.Context(), id, day, week, month, limit); err != nil {
				s.writeKeyError(w, r, err)
				return
			}
			s.audit(r.Context(), store.AuditEvent{
				Event:  EventKeyLimitsUpdate,
				Entity: entityKey(id),
				Detail: fmt.Sprintf("label=%s display_prefix=%s budget_day_micro=%s budget_week_micro=%s budget_month_micro=%s rpm_limit=%s",
					target.Label, target.DisplayPrefix,
					limitText(day), limitText(week), limitText(month), limitText(limit)),
				RemoteIP: remoteIP(r),
			})
		}
		target.BudgetDayMicro, target.BudgetWeekMicro, target.BudgetMonthMicro, target.RPMLimit = day, week, month, limit
	}
	// 禁用位：幂等无变更就不写库、不写审计（限额可能已经改过了，所以这里
	// 只跳过这一步而不是整个请求提前返回）。
	if req.Disabled != nil && *req.Disabled != target.Disabled {
		if err := s.st.SetAPIKeyDisabled(r.Context(), id, *req.Disabled); err != nil {
			s.writeKeyError(w, r, err)
			return
		}
		event := EventKeyEnable
		if *req.Disabled {
			event = EventKeyDisable
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:    event,
			Entity:   entityKey(id),
			Detail:   fmt.Sprintf("label=%s display_prefix=%s", target.Label, target.DisplayPrefix),
			RemoteIP: remoteIP(r),
		})
		target.Disabled = *req.Disabled
	}
	writeJSON(w, http.StatusOK, keyResponse{Key: s.withSpend(toKeyJSON(target))})
}

// handleAdjustKeyMeteredAllowance 增减单把密钥的按量额度。端点只收增量，避免
// 与计量器同时扣减时发生“读旧值再覆盖新值”的丢更新；负数超过当前剩余钳到 0。
func (s *Server) handleAdjustKeyMeteredAllowance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	var req struct {
		DeltaMicro json.RawMessage `json:"delta_micro"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	delta, err := parseMeteredAllowanceDelta(req.DeltaMicro)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_delta", err.Error())
		return
	}
	target, err := s.st.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	remaining, err := s.st.AdjustAPIKeyMeteredAllowance(r.Context(), id, delta)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	target.MeteredAllowanceMicro = remaining
	out := s.withSpend(toKeyJSON(target))
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventKeyMeteredAllowanceAdjust,
		Entity: entityKey(id),
		Detail: fmt.Sprintf("label=%s display_prefix=%s delta_micro=%+d metered_allowance_micro=%d",
			target.Label, target.DisplayPrefix, delta, out.MeteredAllowanceMicro),
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, keyResponse{Key: out})
}
