package admin

// 上游账户管理端点：GET/POST /admin/v1/upstreams、
// PATCH/DELETE /admin/v1/upstreams/{id}（iteration-5 决策 8）。
//
// 凭证纪律（架构 §12 + §15.1）：api_key 明文只在**请求**里出现一次，经
// store 的设备密钥封存成密文入库；任何响应只回 api_key_last4，审计 detail 与
// 日志全链路无明文——换 Key 只留 upstream.rotate_key 事件与上游名。
// PATCH 的 api_key 留空表示"不动"，故没有清空凭证的路径（要清空就删了重建）。
//
// 形态校验与 YAML 导入段同口径：type 只取固件稳定适配器；数据目录平台另带
// catalog_id，并只能复用两种通用兼容适配器。mock 与 compat 必须有 base_url、
// 非 mock 建行必须有 api_key。缺
// base_url 的无内置端点行一个入口协议都不服务（upstream.Account.Endpoint），
// 挂着它的模型会被 /v1/models 列出却在 chat 与 messages 双双 404——这个口径
// 缺口在写入侧堵住。type 建后不可改（换类型即换端点表与凭证语义，删了重建
// 更清楚）。
//
// base_url 只有 mock 与通用 compat 允许管理员写任意地址。产品上游的
// base_url 一旦可写，就能把
// deepseek 上游指向任意主机，而数据面会照常把**解密后的**上游 Key 以
// Authorization 头发过去——于是"管理员也读不回 Key"这条设计承诺（决策 2）
// 被一个写接口绕开了。已有 base_url 的历史行（YAML seed 建的）不受影响，
// 仍可清空。
//
// type minimax（迭代 8）是受限例外：它的国内/国际双站点经 base_url 表达，
// 但可写值**只在内置双值里选**（upstream.MinimaxSiteCN/Intl，空 = 国内
// 缺省）——白名单不破坏上面的安全论证，Key 只可能发往这两个官方站点。
//
// 两种 compat 是自由地址的例外，但守着同一条论证的
// 等价形式：「封存的 Key 绝不发往管理员没为它录过 Key 的主机」。建行时
// 地址与 Key 一起录入；改址（PATCH base_url 为不同值）必须**同请求重新录入
// api_key**（否则 400 base_url_requires_key），且两者经
// store.UpdateUpstreamAddressAndKey 一条 UPDATE 原子落库——发往新主机的
// 永远是管理员刚为它输入的 Key，封存的旧 Key 拿不出来也发不出去。
// 数据目录新增的 compat 平台必须给固定 HTTPS 地址；创建账号时 catalog_id、
// billing_mode 与地址同 Key 一起快照，之后不能改址。YAML seed 的先例同理：
// 写得出 base_url 的人本来就握着明文 Key。

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// 上游变更审计事件（entity 形如 upstream:3；detail 只含名称/类型/base_url
// 等非敏感字段，api_key 明文与密文一律不进审计）。
const (
	EventUpstreamCreate    = "upstream.create"
	EventUpstreamUpdate    = "upstream.update"
	EventUpstreamRotateKey = "upstream.rotate_key"
	EventUpstreamEnable    = "upstream.enable"
	EventUpstreamDisable   = "upstream.disable"
	EventUpstreamDelete    = "upstream.delete"
)

// catalogNameMaxRunes 是上游账户名与模型名的长度上限。模型名同时是客户端
// 请求体里的 model 值，故与用户名同规则：非空、无空白与控制字符。
const catalogNameMaxRunes = 128

// 上游凭证的形态区间。下限取 8 是为了让 last4 有意义：store.last4 对不足 8 字节
// 的明文返回空串，而空 last4 在管理台是"设备密钥与密文不匹配、请重新录入"的
// 信号——放进一个 3 字符的 Key 会让这两种状态在界面上无法区分。
const (
	apiKeyMinRunes = 8
	apiKeyMaxRunes = 512
)

// baseURLNotAllowedMsg 解释为什么产品上游不给写 base_url（见文件头注释）。
const baseURLNotAllowedMsg = "该平台端点固定，不能在账号中配置 base_url"

// minimaxSiteMsg 解释 minimax 站点字段的取值边界（见文件头注释）。
const minimaxSiteMsg = "type minimax 的 base_url 是站点选择，只能留空（国内站）或取 " +
	upstream.MinimaxSiteCN + "（国内）/ " + upstream.MinimaxSiteIntl + "（国际）之一"

// minimaxSiteAllowed 判定 minimax 上游的 base_url 是否在站点白名单内
// （空 = 国内缺省）。两个站点的账号与 Key 互相独立、余额不互通，选错站
// 上游会回 401——但那是凭证问题，不是这里要拦的改址风险。
func minimaxSiteAllowed(baseURL string) bool {
	return baseURL == "" || baseURL == upstream.MinimaxSiteCN || baseURL == upstream.MinimaxSiteIntl
}

func entityUpstream(id int64) string { return fmt.Sprintf("upstream:%d", id) }

// validateCatalogName 校验上游账户名 / 模型名的形态。上游/模型共用一套规则。
func validateCatalogName(name string) error {
	if name == "" {
		return errors.New("不能为空")
	}
	if utf8.RuneCountInString(name) > catalogNameMaxRunes {
		return fmt.Errorf("长度须不超过 %d 个字符", catalogNameMaxRunes)
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("不得包含空白或控制字符")
		}
	}
	return nil
}

// validateBaseURL 校验 base_url 形态（空串表示"走内置端点表"，由调用方按
// type 决定是否允许为空）。
//
// 额外拒绝 URL 里的 user:password——base_url 会原样出现在管理 API 响应、
// 审计 detail 与 UI 列表里，一旦有人把凭证塞进 userinfo，它就绕过了
// "凭证只走 api_key 列、只回 last4" 的整条脱敏链路（§15.1）。上游鉴权一律
// 走 api_key 字段，userinfo 在这里没有合法用途。
func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("不是合法的 http(s) URL")
	}
	if u.User != nil {
		return errors.New("不得在 URL 里内嵌用户名密码，上游 Key 请填 api_key 字段")
	}
	return nil
}

// validateAPIKey 校验上游凭证的形态。错误信息只描述规则，绝不回显 Key 本身
// （§15.1）。空白与控制字符除了必然是误粘贴，还会让出站请求在 Go 的
// Transport 里以"invalid header field value"失败——写入侧拦掉比运行时才炸好。
func validateAPIKey(key string) error {
	if n := utf8.RuneCountInString(key); n < apiKeyMinRunes || n > apiKeyMaxRunes {
		return fmt.Errorf("长度须为 %d–%d 个字符", apiKeyMinRunes, apiKeyMaxRunes)
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("不得包含空白或控制字符")
		}
	}
	return nil
}

// upstreamJSON 是上游在管理 API 里的对外形态：只带凭证末 4 位，永不含明文
// 或密文（§15.1）。APIKeyLast4 为空有三种含义——无凭证的 mock、凭证过短、
// 或设备密钥与密文不匹配（需在本页重新录入 Key）。
type upstreamJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	CatalogID     string `json:"catalog_id"`
	PlatformLabel string `json:"platform_label"`
	BillingMode   string `json:"billing_mode"`
	APIKeyLast4   string `json:"api_key_last4"`
	BaseURL       string `json:"base_url"`
	Disabled      bool   `json:"disabled"`
	// EgressMode 是该账号的出站方式覆盖（inherit|direct|proxy，internal/egress）。
	EgressMode string `json:"egress_mode"`
	// BalanceSupported 表示该类型支持平台余额查询（特化平台能力，服务端
	// upstream.SupportsBalance 是唯一权威——UI 按它显隐按钮，不自维护类型表）。
	BalanceSupported bool      `json:"balance_supported"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toUpstreamJSON(u *store.Upstream, doc platformcatalog.Doc) upstreamJSON {
	label := catalogPlatformLabel(doc, u.CatalogID, u.Type)
	return upstreamJSON{
		ID:               u.ID,
		Name:             u.Name,
		Type:             u.Type,
		CatalogID:        u.CatalogID,
		PlatformLabel:    label,
		BillingMode:      u.BillingMode,
		APIKeyLast4:      u.APIKeyLast4,
		BaseURL:          u.BaseURL,
		Disabled:         u.Disabled,
		EgressMode:       egressModeOf(u.EgressMode),
		BalanceSupported: upstream.SupportsBalance(u.Type),
		CreatedAt:        u.CreatedAt,
		UpdatedAt:        u.UpdatedAt,
	}
}

func catalogPlatformLabel(doc platformcatalog.Doc, catalogID, adapter string) string {
	if p, ok := doc.PlatformByID(catalogID, adapter); ok && p.Vendor != "" {
		return p.Vendor
	}
	if catalogID != "" {
		return catalogID
	}
	return adapter
}

type platformProfileJSON struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Vendor        string   `json:"vendor"`
	BaseURL       string   `json:"base_url,omitempty"`
	CustomBaseURL bool     `json:"custom_base_url"`
	BillingMode   string   `json:"billing_mode"`
	SuggestedName string   `json:"suggested_name"`
	Entries       string   `json:"entries"`
	Protocols     []string `json:"protocols"`
	Ability       string   `json:"ability"`
}

func (s *Server) handleListUpstreamPlatforms(w http.ResponseWriter, r *http.Request) {
	doc, _ := s.effectivePlatformModels(r.Context())
	out := struct {
		Platforms []platformProfileJSON `json:"platforms"`
	}{Platforms: make([]platformProfileJSON, 0, len(doc.Platforms))}
	for _, p := range doc.Platforms {
		baseURL := p.BaseURL
		if p.CustomBaseURL && baseURL == "" {
			// 自填地址平台要求创建时录入地址；这里只算协议能力，不发请求。
			baseURL = "https://api.example.com"
		}
		protocols := []string{}
		for _, kind := range []string{store.ModelKindText, store.ModelKindVideo, store.ModelKindImage} {
			protocols = append(protocols, servableProtocols(kind, p.Type, baseURL)...)
		}
		out.Platforms = append(out.Platforms, platformProfileJSON{
			ID: p.ID, Type: p.Type, Vendor: p.Vendor, BaseURL: p.BaseURL,
			CustomBaseURL: p.CustomBaseURL, BillingMode: p.BillingMode,
			SuggestedName: p.SuggestedName, Entries: p.Entries, Protocols: protocols, Ability: p.Ability,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// upstreamResponse 包装单上游响应。
type upstreamResponse struct {
	Upstream upstreamJSON `json:"upstream"`
}

// handleListUpstreams 列出全部上游（含已停用的），按 id 升序。
func (s *Server) handleListUpstreams(w http.ResponseWriter, r *http.Request) {
	ups, err := s.st.ListUpstreams(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := struct {
		Upstreams []upstreamJSON `json:"upstreams"`
	}{Upstreams: make([]upstreamJSON, 0, len(ups))}
	doc, _ := s.effectivePlatformModels(r.Context())
	for i := range ups {
		out.Upstreams = append(out.Upstreams, toUpstreamJSON(&ups[i], doc))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateUpstream 建上游账户。类型联动校验（见文件头注释）：
// mock 须 base_url，非 mock 须 api_key 且不得带 base_url。
func (s *Server) handleCreateUpstream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		CatalogID string `json:"catalog_id"`
		APIKey    string `json:"api_key"`
		BaseURL   string `json:"base_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateCatalogName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "上游名称不合法："+err.Error())
		return
	}
	billingMode := ""
	if req.CatalogID != "" && req.CatalogID != req.Type {
		doc, _ := s.effectivePlatformModels(r.Context())
		profile, ok := doc.PlatformByID(req.CatalogID, req.Type)
		if !ok || profile.ID == profile.Type {
			writeError(w, http.StatusBadRequest, "invalid_catalog_platform", "平台目录里没有这个可数据接入的平台")
			return
		}
		if req.Type != "" && req.Type != profile.Type {
			writeError(w, http.StatusBadRequest, "catalog_platform_mismatch", "平台身份与协议适配器不匹配")
			return
		}
		req.Type = profile.Type
		req.BaseURL = profile.BaseURL
		billingMode = profile.BillingMode
	}
	switch req.Type {
	case config.UpstreamDeepseek, config.UpstreamArk, config.UpstreamArkPlan, config.UpstreamQwenPlan, config.UpstreamOpenCodeGo:
		if req.APIKey == "" {
			writeError(w, http.StatusBadRequest, "api_key_required",
				fmt.Sprintf("type %s 必须提供 api_key", req.Type))
			return
		}
		if req.BaseURL != "" {
			writeError(w, http.StatusBadRequest, "base_url_not_allowed", baseURLNotAllowedMsg)
			return
		}
	case config.UpstreamMinimax:
		if req.APIKey == "" {
			writeError(w, http.StatusBadRequest, "api_key_required",
				fmt.Sprintf("type %s 必须提供 api_key", req.Type))
			return
		}
		// 站点白名单（见文件头注释）：base_url 只在内置双值里选，空 = 国内。
		if !minimaxSiteAllowed(req.BaseURL) {
			writeError(w, http.StatusBadRequest, "invalid_base_url", minimaxSiteMsg)
			return
		}
	case config.UpstreamOpenAICompat, config.UpstreamAnthropicCompat:
		// 通用适配两样都要：地址不内置，Key 与地址一起录入（改址规则见
		// 文件头注释——发往任何主机的 Key 都是管理员亲手为它输入的）。
		if req.APIKey == "" {
			writeError(w, http.StatusBadRequest, "api_key_required",
				fmt.Sprintf("type %s 必须提供 api_key（无鉴权的兼容服务可填任意占位串）", req.Type))
			return
		}
		if req.BaseURL == "" {
			writeError(w, http.StatusBadRequest, "base_url_required",
				fmt.Sprintf("type %s 必须提供 base_url（兼容服务的端点根，通常以 /v1 结尾）", req.Type))
			return
		}
	case config.UpstreamMock:
		if req.BaseURL == "" {
			writeError(w, http.StatusBadRequest, "base_url_required",
				"type mock 必须提供 base_url（指向本地 mock 上游），否则两个入口都无法服务")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_type",
			fmt.Sprintf("未知上游类型 %q（可选 %s|%s|%s|%s|%s|%s|%s|%s|%s）", req.Type,
				config.UpstreamDeepseek, config.UpstreamArk, config.UpstreamArkPlan,
				config.UpstreamQwenPlan, config.UpstreamOpenCodeGo, config.UpstreamMinimax, config.UpstreamOpenAICompat, config.UpstreamAnthropicCompat,
				config.UpstreamMock))
		return
	}
	if err := validateBaseURL(req.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_base_url", "base_url "+err.Error())
		return
	}
	if req.APIKey != "" {
		if err := validateAPIKey(req.APIKey); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_api_key", "api_key 不合法："+err.Error())
			return
		}
	}

	var u *store.Upstream
	var err error
	if req.CatalogID != "" && req.CatalogID != req.Type {
		u, err = s.st.CreateCatalogUpstream(r.Context(), req.Name, req.Type, req.CatalogID, billingMode, req.APIKey, req.BaseURL)
	} else {
		u, err = s.st.CreateUpstream(r.Context(), req.Name, req.Type, req.APIKey, req.BaseURL)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "upstream_name_taken", "上游名称已存在")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventUpstreamCreate,
		Entity: entityUpstream(u.ID), Detail: upstreamDetail(u),
		RemoteIP: remoteIP(r),
	})
	doc, _ := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusCreated, upstreamResponse{Upstream: toUpstreamJSON(u, doc)})
}

// handlePatchUpstream 改上游：name / base_url / disabled / 换 api_key
// （留空不动）。type 只接受与现值相同的回传，改动即 400 type_immutable。
//
// 校验全部先于写入；三个子动作（改名或改 base_url、换 Key、启停）按序独立
// 落库，各记一条审计。store 不暴露事务，故这里**不是原子的**：中途失败会留下
// 已生效的前几步（都已进审计），客户端重试整个 PATCH 即可收敛——所有子动作
// 都是幂等赋值。
func (s *Server) handlePatchUpstream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "上游不存在")
		return
	}
	var req struct {
		Name       *string `json:"name"`
		Type       string  `json:"type"` // 仅允许回传现值（见 type_immutable）
		APIKey     *string `json:"api_key"`
		BaseURL    *string `json:"base_url"`
		Disabled   *bool   `json:"disabled"`
		EgressMode *string `json:"egress_mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := s.st.GetUpstreamByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "上游不存在")
		return
	}
	var egressMode egress.Mode
	if req.EgressMode != nil {
		egressMode, err = egress.ParseMode(*req.EgressMode)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_egress_mode", "出站方式不合法："+err.Error())
			return
		}
		if egressMode == egress.ModeProxy && !s.egress.Status().Configured {
			writeError(w, http.StatusBadRequest, "proxy_unconfigured",
				"尚未配置出站代理：先在「网络/域名/代理」页填写代理端点，再把账号设为经代理")
			return
		}
	}
	if req.Type != "" && req.Type != current.Type {
		writeError(w, http.StatusBadRequest, "type_immutable",
			"上游类型不可修改，请删除后按新类型重建")
		return
	}
	name, baseURL := current.Name, current.BaseURL
	if req.Name != nil {
		if err := validateCatalogName(*req.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_name", "上游名称不合法："+err.Error())
			return
		}
		name = *req.Name
	}
	if req.BaseURL != nil {
		if err := validateBaseURL(*req.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_base_url", "base_url "+err.Error())
			return
		}
		// 只拦"把上游指向新地址"这个动作本身：历史行（YAML seed 建的）带着
		// base_url 仍可被改名/启停，也可以清空。mock 收任意合法地址；minimax
		// 的地址是站点选择，只在内置双值里换（清空 = 回国内缺省）；
		// openai_compat 收任意合法地址，但改址须同请求重录 Key（见下）。
		if *req.BaseURL != "" {
			switch current.Type {
			case config.UpstreamMock, config.UpstreamOpenAICompat, config.UpstreamAnthropicCompat:
			case config.UpstreamMinimax:
				if !minimaxSiteAllowed(*req.BaseURL) {
					writeError(w, http.StatusBadRequest, "invalid_base_url", minimaxSiteMsg)
					return
				}
			default:
				writeError(w, http.StatusBadRequest, "base_url_not_allowed", baseURLNotAllowedMsg)
				return
			}
		}
		baseURL = *req.BaseURL
	}
	switch {
	case current.Type == config.UpstreamMock && baseURL == "":
		writeError(w, http.StatusBadRequest, "base_url_required",
			"type mock 必须提供 base_url（指向本地 mock 上游），否则两个入口都无法服务")
		return
	case (current.Type == config.UpstreamOpenAICompat || current.Type == config.UpstreamAnthropicCompat) && baseURL == "":
		writeError(w, http.StatusBadRequest, "base_url_required",
			"兼容适配器必须提供 base_url（服务的端点根），否则入口无法服务")
		return
	}
	keyProvided := req.APIKey != nil && *req.APIKey != ""
	if keyProvided {
		if err := validateAPIKey(*req.APIKey); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_api_key", "api_key 不合法："+err.Error())
			return
		}
	}
	// openai_compat 改址必须同请求重录 Key，且两者原子落库：数据面发往新主机
	// 的永远是管理员刚输入的 Key，封存的旧 Key 绝不会被一次改址带去新地址
	// （文件头注释的安全论证；拆成两步写会留下「旧 Key 配新地址」的中间态）。
	compatType := current.Type == config.UpstreamOpenAICompat || current.Type == config.UpstreamAnthropicCompat
	repointing := compatType && baseURL != current.BaseURL
	if repointing && current.CatalogID != "" && current.CatalogID != current.Type {
		writeError(w, http.StatusBadRequest, "catalog_endpoint_immutable",
			"目录平台的端点是录入 Key 时确认的快照；换地址请删除账号后按新平台重建")
		return
	}
	if repointing && !keyProvided {
		writeError(w, http.StatusBadRequest, "base_url_requires_key",
			"修改兼容上游的 base_url 时必须同时重新输入 api_key：封存的旧 Key 不能被发往新地址")
		return
	}

	if name != current.Name || baseURL != current.BaseURL {
		var err error
		if repointing {
			err = s.st.UpdateUpstreamAddressAndKey(r.Context(), id, name, baseURL, *req.APIKey)
		} else {
			err = s.st.UpdateUpstream(r.Context(), id, name, baseURL)
		}
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeError(w, http.StatusConflict, "upstream_name_taken", "上游名称已存在")
			} else {
				s.writeCatalogError(w, r, err, "上游不存在")
			}
			return
		}
		detail := fmt.Sprintf("name=%s type=%s base_url=%s", name, current.Type, baseURL)
		if name != current.Name {
			detail += " renamed_from=" + current.Name
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventUpstreamUpdate,
			Entity: entityUpstream(id), Detail: detail, RemoteIP: remoteIP(r),
		})
	}
	// api_key 留空 = 不动（没有清空凭证的路径，见文件头注释）。改址路径的
	// Key 已随上面那条 UPDATE 原子落库，这里只补审计，不再二次写。
	if keyProvided {
		if !repointing {
			if err := s.st.SetUpstreamKey(r.Context(), id, *req.APIKey); err != nil {
				s.writeCatalogError(w, r, err, "上游不存在")
				return
			}
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventUpstreamRotateKey,
			Entity: entityUpstream(id), Detail: fmt.Sprintf("name=%s type=%s", name, current.Type),
			RemoteIP: remoteIP(r),
		})
	}
	if req.Disabled != nil && *req.Disabled != current.Disabled {
		if err := s.st.SetUpstreamDisabled(r.Context(), id, *req.Disabled); err != nil {
			s.writeCatalogError(w, r, err, "上游不存在")
			return
		}
		event := EventUpstreamEnable
		if *req.Disabled {
			event = EventUpstreamDisable
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:  event,
			Entity: entityUpstream(id), Detail: fmt.Sprintf("name=%s type=%s", name, current.Type),
			RemoteIP: remoteIP(r),
		})
	}
	if req.EgressMode != nil && string(egressMode) != egressModeOf(current.EgressMode) {
		if err := s.st.SetUpstreamEgressMode(r.Context(), id, string(egressMode)); err != nil {
			s.writeCatalogError(w, r, err, "上游不存在")
			return
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventUpstreamUpdate,
			Entity: entityUpstream(id), Detail: fmt.Sprintf("name=%s type=%s egress_mode=%s", name, current.Type, egressMode),
			RemoteIP: remoteIP(r),
		})
	}

	updated, err := s.st.GetUpstreamByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "上游不存在")
		return
	}
	doc, _ := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusOK, upstreamResponse{Upstream: toUpstreamJSON(updated, doc)})
}

// egressModeOf 把库里的 egress_mode 归一成 API 值（历史空串按 inherit）。
func egressModeOf(mode string) string {
	if m, err := egress.ParseMode(mode); err == nil {
		return string(m)
	}
	return string(egress.ModeInherit)
}

// handleDeleteUpstream 删上游。仍被模型来源引用时外键拒绝 → 409
// upstream_in_use（提示先删或改挂这些来源）。
func (s *Server) handleDeleteUpstream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "上游不存在")
		return
	}
	current, err := s.st.GetUpstreamByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "上游不存在")
		return
	}
	if err := s.st.DeleteUpstream(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "upstream_in_use",
				"该上游仍被模型来源引用，请先删除这些来源或把它们改挂到其他上游")
		} else {
			s.writeCatalogError(w, r, err, "上游不存在")
		}
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventUpstreamDelete,
		Entity: entityUpstream(id), Detail: upstreamDetail(current),
		RemoteIP: remoteIP(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// upstreamDetail 组装上游审计 detail：只含名称/类型/base_url（均非敏感）。
func upstreamDetail(u *store.Upstream) string {
	return fmt.Sprintf("name=%s type=%s base_url=%s", u.Name, u.Type, u.BaseURL)
}

// writeCatalogError 把 store 的哨兵错误映射为统一错误体：ErrNotFound → 404
// （notFoundMsg 指明是哪种实体），其余按内部错误处理。
func (s *Server) writeCatalogError(w http.ResponseWriter, r *http.Request, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", notFoundMsg)
		return
	}
	if errors.Is(err, store.ErrInvalidPricing) {
		// 目录价形态不合法。本层的校验（parseModelPricing）本该先挡住它，
		// 两层判定万一分叉也必须是 400 而不是 500——「服务内部错误」会让
		// 管理员去查设备，而实际上只是他刚才填错了一个字段。
		writeError(w, http.StatusBadRequest, "invalid_pricing", "目录价不合法："+err.Error())
		return
	}
	s.internalError(w, r, err)
}
