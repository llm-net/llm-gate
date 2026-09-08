package admin

// Agent 订阅模型的收敛器：**订阅接入的模型不是管理员一条条加进来的，是模型
// 目录数据里写好的**——连上哪份订阅，就把那份订阅的文本模型读出来显示，
// 管理台一概不可编辑。
//
// 数据流一句话：
//
//	模型目录数据 platform-models.json 的 agents 段（生效那份：内嵌基线，
//	  或版本号更大的同步副本）  ×  agent_accounts 里在场的订阅
//	  =  设备目录里那批 Agent 模型行（本文件把两边收敛成一致）
//
// 为什么仍然落成 models 行，而不是每次现算：数据面按**名字点查目录**借价
// （applyAgentModelPricing），落成行意味着那条路一个字节都不用改；开发工具
// 策略的订阅模型可见集（devtoolpolicy）、`GET /agents/v1/models`、用量页的
// 模型维度也都是同一份目录的读数，行在库里它们自然一致。**但这些行是派生
// 数据**：本文件是它们唯一的写点，管理面对它们整组只读（守卫见 models.go）。
//
// 四处触发，都是「状态变了就收敛一次」，不设定时器（零轮询铁律）：
//
//	连接订阅      connectAgentAccount（各 provider 的共同收尾）
//	撤掉订阅      handleDeleteAgentAccount
//	同步模型目录  handleSyncCatalog——厂商上新/下架在这一刻生效
//	进程启动      cmd/llmgate 装配处——固件升级带来新基线时开机即对齐
//
// 三个动作，只碰**本收敛器拥有的行**（判据见 agentModelOwned）：
//
//	建  目录里有、库里没有 → 建文本模型行并录官方名义价
//	补  行在但被停用、或还没录上价 → 恢复启用、按价目文件补价
//	删  订阅撤了、或厂商把型号下架了 → 删掉（这就是「不再显示」的实现）
//
// 三条安全边界，改之前先读：
//
//  1. **管理员亲手挂了 API 上游的同名行不归我们**，一根手指都不碰——那是他的
//     API 模型（比如把 grok-4.6 同时接到某个按量平台上），照旧在 API密钥接入标签页
//     按常规渲染、照旧可编辑。
//  2. **回收只按账本，不按目录**（settingAgentCatalogModels）：目录与价目都是
//     "读不到即降级"的外部文件，让降级后的空索引参与回收判据，一次读失败就会
//     表现成"整批模型行被清空"。账本记的是"我建/认领过谁"，读不到时它是空集，
//     那一轮一行都不认领——降级只会少删，绝不会多删。读订阅列表失败则整次
//     收敛就地返回，一行都不动。
//  3. **价只填不洗**：价目文件里有价才写，没有（没同步过、这次没取到、厂商下架了
//     这一条）一律保留现状。一次失败的价目同步不该把设备上已有的名义单价抹平。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// settingAgentCatalogModels 是收敛器的**账本**：它建/认领过的那些模型名
// （小写，JSON 数组，与两份目录数据文件同住 settings 表）。
//
// 为什么需要一个账本，而不是每次拿目录现算：厂商在新一版目录里撤掉一个型号
// 之后，那个名字**同时**从目录里消失了——只按目录判"这行是不是我的"，正好在
// 该回收它的那一刻认不出它，于是留下一行谁也删不掉的僵尸。账本记的是"我建过
// 谁"，撤型号、撤订阅、改名三种情况因此都收敛得掉。
//
// 账本丢了（库被换过、老固件升上来时还没有这一项）不是故障：下一次收敛会把
// 目录里还在、订阅还连着的那些行**重新认领**记进来，只有"目录已撤 + 账本已丢"
// 的行会留下——那正是保守的方向（宁可留一行也不误删管理员的行）。
const settingAgentCatalogModels = "agent_catalog_models"

// agentCatalogIndex 建「模型名（小写）→ 订阅 provider」索引，来源只有一处：
// 生效目录的 agents 段。三个消费方——模型视图的 agent 注记（管理台据此把行
// 归到订阅接入标签页并渲染订阅侧调用入口）、通用建模面的名字守卫、收敛器的
// want 集。
//
// **只认目录、不认价目文件**：价目文件的 agent 标记只回答"这个名字按多少钱
// 记名义金额"，不回答"这台设备上有没有这个模型"——后者由目录 agents 段与
// 在场订阅共同决定。
//
// 不分大小写按名匹配；重复条目属发布错误，取第一条（同 Doc.Platform 的口径）。
// 读不到、解不开一律降级为空索引：注记是展示层事实，绝不把模型列表拖成 500；
// 回收侧的降级处置见文件头第 2 条。
func (s *Server) agentCatalogIndex(ctx context.Context) map[string]string {
	idx := make(map[string]string)
	doc, _ := s.effectivePlatformModels(ctx)
	for _, a := range doc.Agents {
		if a.Provider == store.AgentProviderCursor {
			continue // Cursor 只从平台目录读取计价，不创建共享 API 模型行。
		}
		if !recognizedAgentProvider(a.Provider) {
			continue // 将来的新 provider：旧固件渲染不出它的调用入口，索性不认
		}
		for _, m := range a.Models {
			if !agentCatalogModelOK(m) {
				continue
			}
			key := strings.ToLower(m.Name)
			if _, dup := idx[key]; dup {
				continue
			}
			idx[key] = a.Provider
		}
	}
	return idx
}

// ownedAgentModelNames 读账本。读不到、解不开都回空集（附 Warn）：空账本只会
// 让这一轮少回收几行，而把读失败当成"什么都不是我的"正是安全的方向。
func (s *Server) ownedAgentModelNames(ctx context.Context) map[string]bool {
	raw, err := s.st.GetSetting(ctx, settingAgentCatalogModels)
	if err != nil {
		s.log.Warn("读取 Agent 订阅模型账本失败", "err", err)
		return nil
	}
	if raw == "" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		s.log.Warn("Agent 订阅模型账本解析失败", "err", err)
		return nil
	}
	owned := make(map[string]bool, len(names))
	for _, n := range names {
		owned[strings.ToLower(n)] = true
	}
	return owned
}

// saveOwnedAgentModelNames 落账本（排序后写，好让同一份状态每次都得到同一段
// 文本——库文件的无谓改动会白白吃掉 SD 卡的写次数）。
func (s *Server) saveOwnedAgentModelNames(ctx context.Context, owned map[string]bool) {
	names := make([]string, 0, len(owned))
	for n := range owned {
		names = append(names, n)
	}
	sort.Strings(names)
	raw, err := json.Marshal(names)
	if err != nil { // []string 必然可序列化；防御分支
		s.log.Warn("Agent 订阅模型账本序列化失败", "err", err)
		return
	}
	if err := s.st.SetSetting(ctx, settingAgentCatalogModels, string(raw)); err != nil {
		s.log.Warn("Agent 订阅模型账本落库失败", "err", err)
	}
}

// agentCatalogModelOK 判定目录 agents 段里的一条是否设备收得下：名字过建模那条
// 校验（与手工建模同一份定义），且是文本——agents 段只收文本模型，其余种类
// 一律当发布错误跳过。
func agentCatalogModelOK(m platformcatalog.AgentModel) bool {
	return validateCatalogName(m.Name) == nil && m.Kind == store.ModelKindText
}

// agentModelOwned 判断一条已在库的模型行是不是本收敛器拥有的（= 撤订阅或
// 厂商下架时该由我们回收、管理面对它只读）：账本记过的文本行，且**一条来源
// 都没挂**——挂了就是管理员把它接到 API 上游上了，那是他的行（文件头第 1 条）。
func agentModelOwned(m *store.ModelWithSources, owned map[string]bool) bool {
	return m.Kind == store.ModelKindText && len(m.Sources) == 0 && owned[strings.ToLower(m.Name)]
}

// agentCatalogManagedModel 是管理面只读守卫的判据（这批行整组只读：不改名、
// 不录价、不启停、不删除）：这一行是不是模型目录数据管的 Agent 订阅模型。
//
// 先按账本对名字，对上了再取一次来源（挂了 API 上游的同名行不归我们，照旧
// 可编辑）。这个顺序是有意的——绝大多数 PATCH/DELETE 打的是普通 API 模型，
// 不该为它们各多读一次 settings。
func (s *Server) agentCatalogManagedModel(ctx context.Context, m *store.Model) (bool, error) {
	if m.Kind != store.ModelKindText || !s.ownedAgentModelNames(ctx)[strings.ToLower(m.Name)] {
		return false, nil
	}
	full, err := s.st.GetModelWithSources(ctx, m.ID)
	if err != nil {
		return false, err
	}
	return len(full.Sources) == 0, nil
}

// agentCatalogManagedError 是这批行被写入面拦下时的统一答复：入口不是
// 「添加模型」，而是"根本没有入口"。
func agentCatalogManagedError(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "model_agent_managed",
		"该模型由模型目录数据定义、随订阅自动读出，管理台不提供编辑入口："+
			"要增减型号请更新模型目录数据，要让它下架请撤掉对应的 Agent 订阅")
}

// wantAgentModel 是「目录说这台设备此刻该有的一行」。
type wantAgentModel struct {
	Provider string
	Name     string
}

// agentModelSyncResult 是一次收敛的账：建了几行、补了几行（补价或恢复启用）、
// 回收了几行。同步模型目录的响应把它捎给界面。
type agentModelSyncResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
}

func (r agentModelSyncResult) touched() bool {
	return r.Created > 0 || r.Updated > 0 || r.Removed > 0
}

// agentModelActor 是收敛时写审计用的触发者。启动那一次没有人（Trigger=boot）。
type agentModelActor struct {
	Trigger string // boot | agent_connect | agent_delete | catalog_sync
	By      string // 来路（手动 / 自动检查），开机收敛为空
	IP      string
}

// SyncAgentModels 是装配处（cmd/llmgate）在启动时的一次收敛：固件升级换来
// 新的内嵌基线、或者上一次收敛中途失败时，开机就对齐。失败只记日志——设备照常
// 起来，下一次触发会再收敛一次。
func (s *Server) SyncAgentModels(ctx context.Context) {
	res, err := s.syncAgentModels(ctx, agentModelActor{Trigger: "boot"})
	if err != nil {
		s.log.Warn("Agent 订阅模型收敛失败，下次触发时重试", "err", err.Error())
		return
	}
	if res.touched() {
		s.log.Info("Agent 订阅模型已按模型目录数据收敛",
			"created", res.Created, "updated", res.Updated, "removed", res.Removed)
	}
}

// runAgentModelSync 跑一次收敛并把失败压成一条日志（下面两个入口共用）。
// 失败不改变调用方的结果（订阅确实连上了 / 撤掉了 / 文件确实同步了）。
func (s *Server) runAgentModelSync(ctx context.Context, a agentModelActor) agentModelSyncResult {
	res, err := s.syncAgentModels(ctx, a)
	if err != nil {
		s.log.Warn("Agent 订阅模型收敛失败，下次触发时重试", "trigger", a.Trigger, "err", err.Error())
	}
	return res
}

// syncAgentModelsAfter 是订阅连接/撤销两个处理器的收尾调用：**在写响应之前**跑，
// 好让管理台那一次刷新读到的就是收敛后的模型列表。
func (s *Server) syncAgentModelsAfter(r *http.Request, trigger string) agentModelSyncResult {
	return s.runAgentModelSync(r.Context(), agentModelActor{
		Trigger: trigger, By: catalogByManual, IP: remoteIP(r),
	})
}

// syncAgentModelsFor 是官网数据升级路径的收尾调用（手动与自动共用）：来路
// 由 catalogActor 带过来，by= 段因此分得出「手动」和「自动检查」。
func (s *Server) syncAgentModelsFor(ctx context.Context, actor catalogActor, trigger string) agentModelSyncResult {
	return s.runAgentModelSync(ctx, agentModelActor{Trigger: trigger, By: actor.by(), IP: actor.IP})
}

// syncAgentModels 收敛一次：回收 → 建/补 → 落账本。
//
// 回收与建/补分开走而不是一遍到底，是为了"改名"这种目录改动能收敛：旧名先
// 回收、新名再建，两者在同一次里都做完。中途失败就地返回——已经做完的那些
// 都是好的（每一步各自幂等），下一次触发接着收敛。
func (s *Server) syncAgentModels(ctx context.Context, actor agentModelActor) (agentModelSyncResult, error) {
	var res agentModelSyncResult
	doc, _ := s.effectivePlatformModels(ctx)
	accounts, err := s.st.ListAgentAccounts(ctx)
	if err != nil {
		return res, err
	}
	// 「连接了订阅」= agent_accounts 里有这个 provider 的行，状态不限：
	// auth_expired 是"该重新登录了"、disabled 是"管理员先停一下"，模型清单
	// 都该照旧摆在那儿（数据面自会用 409 说真话）。
	connected := make(map[string]bool, len(accounts))
	for i := range accounts {
		connected[accounts[i].Provider] = true
	}
	want, order := s.wantedAgentModels(doc, connected)
	// owned 是收敛前的账本；mine 是收敛后的账本（建成的 + 认领的 + 留下的），
	// 收尾时整份写回。
	owned := s.ownedAgentModelNames(ctx)
	mine := make(map[string]bool, len(want))
	models, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return res, err
	}
	byName := make(map[string]*store.ModelWithSources, len(models))
	for i := range models {
		byName[strings.ToLower(models[i].Name)] = &models[i]
	}

	// 一、回收：目录不再要、而我们拥有的行。
	for i := range models {
		m := &models[i]
		key := strings.ToLower(m.Name)
		if _, keep := want[key]; keep {
			continue
		}
		if !agentModelOwned(m, owned) {
			continue
		}
		if err := s.st.DeleteModel(ctx, m.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // 并发删掉了，正是我们想要的结果
			}
			return res, fmt.Errorf("回收 Agent 模型行 %q: %w", m.Name, err)
		}
		delete(byName, key)
		res.Removed++
		s.auditAgentModel(ctx, actor, EventModelDelete, entityModel(m.ID),
			fmt.Sprintf("name=%s kind=%s", m.Name, m.Kind))
	}

	// 二、建 / 补。价目按已存文件取（没有就先建未定价的行——那行仍要显示，
	// 只是名义金额记 0；下次同步到价会补上）。
	pricing := s.storedPricingByName(ctx)
	for _, key := range order {
		w := want[key]
		m := byName[key]
		if m != nil && m.Kind != store.ModelKindText {
			// 名字被一个种类不同的模型占着：不抢名字，也不改人家的行。
			s.log.Warn("模型目录数据里的订阅模型与本地同名行种类不符，已跳过",
				"provider", w.Provider, "model", w.Name, "local_kind", m.Kind)
			continue
		}
		if m != nil && len(m.Sources) > 0 {
			// 管理员挂了 API 上游的行：那是他的（文件头第 1 条）。也不记进
			// 账本——账本记的是"我建/认领过谁"。
			continue
		}
		// 走到这里的行要么马上由我们建出来，要么是我们**认领**的（目录点过名、
		// 一条来源都没挂）：账本自此记着它，将来厂商撤型号或管理员撤订阅时
		// 回收得掉。
		mine[key] = true
		created, updated, err := s.ensureAgentTextModel(ctx, actor, w, m, pricing[key].Pricing)
		if err != nil {
			return res, err
		}
		res.Created += boolCount(created)
		res.Updated += boolCount(updated)
	}

	// 三、落账本。写在最后：中途返回的那些路径宁可留着旧账本——旧账本至多让
	// 下一轮多认领一次（幂等），而半份新账本会让没写进去的行从此认不出来。
	if !sameNameSet(owned, mine) {
		s.saveOwnedAgentModelNames(ctx, mine)
	}
	return res, nil
}

// sameNameSet 判断账本有没有变，省掉"每次收敛都写一次 settings"的无谓写盘
// （SD 卡写放大是这台设备上真实的成本，同 usage 只落小时聚合的理由）。
func sameNameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// wantedAgentModels 按「生效目录 × 在场订阅」算出这台设备此刻该有的那批行。
// order 保持文件序（provider 的次序、每份订阅内部的次序），好让建行的次序、
// 审计的次序与文件一致，读起来是同一份清单。
func (s *Server) wantedAgentModels(doc platformcatalog.Doc, connected map[string]bool) (map[string]wantAgentModel, []string) {
	want := make(map[string]wantAgentModel)
	order := make([]string, 0)
	for _, a := range doc.Agents {
		if a.Provider == store.AgentProviderCursor {
			continue // Cursor 同名型号独立计价，不归共享模型行的收敛器管理。
		}
		if !connected[a.Provider] || !recognizedAgentProvider(a.Provider) {
			continue
		}
		for _, m := range a.Models {
			if !agentCatalogModelOK(m) {
				// 文件发布错误：名字不合目录规范、或不是文本模型。跳过这一条，
				// 其余照建（一条写错不该让整份清单都上不去）。
				s.log.Warn("模型目录数据里的订阅模型条目不合形态，已跳过",
					"provider", a.Provider, "model", clipDisplay(m.Name), "kind", m.Kind)
				continue
			}
			key := strings.ToLower(m.Name)
			if _, dup := want[key]; dup {
				continue
			}
			want[key] = wantAgentModel{Provider: a.Provider, Name: m.Name}
			order = append(order, key)
		}
	}
	return want, order
}

// ensureAgentTextModel 收敛一条订阅文本计价行：缺则建、停用则恢复、没价而
// 文件里有价则补。**不改名、不清价**（文件头第 3 条）。
func (s *Server) ensureAgentTextModel(ctx context.Context, actor agentModelActor,
	w wantAgentModel, m *store.ModelWithSources, priceRaw json.RawMessage) (created, updated bool, err error) {

	pricing, _, perr := parseModelPricing(store.ModelKindText, priceRaw)
	if perr != nil {
		// 价目文件里那条不合形态：建行照建（未定价），别让一条坏价目把模型
		// 从界面上抹掉。同步端点那一侧会把这条报成 invalid_pricing。
		s.log.Warn("官方价目文件里的订阅模型价不合形态，本行按未定价处理",
			"model", w.Name, "err", perr.Error())
		pricing = ""
	}
	if m == nil {
		row, cerr := s.st.CreateModel(ctx, w.Name, store.ModelKindText, pricing)
		if cerr != nil {
			if errors.Is(cerr, store.ErrConflict) {
				return false, false, nil // 并发建好了：下次收敛再补它的价
			}
			return false, false, fmt.Errorf("建 Agent 文本模型行 %q: %w", w.Name, cerr)
		}
		s.auditAgentModel(ctx, actor, EventModelCreate, entityModel(row.ID),
			fmt.Sprintf("name=%s kind=%s agent=%s pricing=%s", row.Name, row.Kind, w.Provider, pricingAudit(pricing)))
		return true, false, nil
	}
	if pricing != "" && pricing != m.Pricing {
		if err := s.st.SetModelPricing(ctx, m.ID, pricing); err != nil {
			return false, false, fmt.Errorf("补 Agent 文本模型价 %q: %w", w.Name, err)
		}
		s.auditAgentModel(ctx, actor, EventModelPricing, entityModel(m.ID),
			fmt.Sprintf("name=%s kind=%s agent=%s pricing=%s", m.Name, m.Kind, w.Provider, pricingAudit(pricing)))
		updated = true
	}
	if enabled, err := s.enableAgentModelRow(ctx, actor, m); err != nil {
		return false, updated, err
	} else if enabled {
		updated = true
	}
	return false, updated, nil
}

// enableAgentModelRow 把停用位收拾干净：这批行整组只读、管理台不提供启停，
// 界面上一个关不掉的开关若真处在关闭态（升级前被停过、或库被手工改过），
// 就成了一条"看得见、说不清、改不动"的死行。
func (s *Server) enableAgentModelRow(ctx context.Context, actor agentModelActor, m *store.ModelWithSources) (bool, error) {
	if !m.Disabled {
		return false, nil
	}
	if err := s.st.SetModelDisabled(ctx, m.ID, false); err != nil {
		return false, fmt.Errorf("恢复 Agent 模型启用位 %q: %w", m.Name, err)
	}
	s.auditAgentModel(ctx, actor, EventModelEnable, entityModel(m.ID),
		fmt.Sprintf("name=%s kind=%s", m.Name, m.Kind))
	return true, nil
}

// auditAgentModel 记一条收敛审计。detail 末尾恒带
// `source=agent_catalog trigger=…`（必要时再加 by=），好让「这行怎么突然多
// 出来/没了」一眼追得到源头——**改模型行的决定来自目录数据，不是谁点的**，
// 哪怕这一次是被某个管理员动作触发的。
func (s *Server) auditAgentModel(ctx context.Context, actor agentModelActor, event, entity, detail string) {
	full := detail + " source=agent_catalog trigger=" + actor.Trigger
	if actor.By != "" {
		full += " by=" + actor.By
	}
	s.audit(ctx, store.AuditEvent{
		Event: event, Entity: entity, Detail: full, RemoteIP: actor.IP,
	})
}

// boolCount 把"做没做"折成计数，省掉三处 if。
func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
