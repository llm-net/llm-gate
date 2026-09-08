package admin

// 「API密钥接入 → 添加模型」（2026-08-14，模型接入页改版：模型不再从模型列自己
// 「新建」，而是从**承载它的那个账号**加进来）：
//
//	GET  /admin/v1/upstreams/{id}/models   本账号平台的可选清单 + 已添加/占用标记
//	POST /admin/v1/upstreams/{id}/models   幂等添加所选（或自定义的）模型
//
// 为什么把入口挪到账号上：配一个模型的动线本来就是「录账号 → 这家平台有什么
// 模型 → 挂上去」。旧的「新建模型」只建一个没有来源的空行——建完必然还要再点
// 一次「添加上游」，中间那一步（没挂来源的模型进不了任何候选、调用一律 404）
// 是纯粹的中间态。从账号加，两步合成一步，且**平台是已知的**：协议面不必让
// 操作者选，由这条账号服务的那一个直接定下来（下面 resolveFamily）。
//
// 清单从哪来：internal/platformcatalog 的「平台模型信息」文件——固件内嵌一份
// 基线，LLM Gate官网有同一份可匿名更新（catalogsync.go）。清单只是**选单**：
// 每条都还要过一遍与手工建模同一条的校验，且对话框恒提供「自定义」，厂商上新
// 而文件还没更新时照样录得进去（产品要求，2026-08-14）。
//
// 添加一个模型 = 两步幂等写入：模型行（缺则建、
// 未声明族则认领）→ 来源行（priority 按上游计费模式取 100/200）。两步不在一个
// 事务里：各自幂等，中途失败重发一次 POST 即补齐，不会留下按错账的中间态——
// 没挂上来源的模型行进不了任何候选。

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// maxAddModels 是一次 POST 能添加的条数上限。清单本身有条目上限，这里挡的是
// 手写请求——每条都要写两次库并留两条审计。
const maxAddModels = 50

// 一条清单项为什么加不了（机读值，zh-CN 文案在管理台一侧）。空 = 可以加。
const (
	blockKindMismatch   = "kind_mismatch"   // 同名模型行是另一种种类
	blockFamilyMismatch = "family_mismatch" // 同名模型行声明了另一个协议面
)

// upstreamModelJSON 是「添加模型」对话框里的一行。
type upstreamModelJSON struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Family          string `json:"family,omitempty"` // AIGC 的协议面声明；text 恒空
	UpstreamModelID string `json:"upstream_model_id,omitempty"`
	Note            string `json:"note,omitempty" i18n:"text"`
	// Added：目录里已有同名行且**已挂在本账号上**（对话框置灰勾选）。
	Added bool `json:"added,omitempty"`
	// InCatalog：目录里已有同名行但没挂本账号——加它 = 给既有模型多挂一条
	// 来源（对话框据此把文案从「新建」改成「挂到这个账号」）。
	InCatalog bool `json:"in_catalog,omitempty"`
	// Blocked：加不了的原因（见 block* 常量）；常态缺席。
	Blocked string `json:"blocked,omitempty"`
}

// upstreamKindJSON 是这条账号能承载的一种模型种类。Family 是该种类下这家平台
// 走的协议面（AIGC 才有；text 恒空）——「自定义」表单据此不必让操作者选。
type upstreamKindJSON struct {
	Kind   string `json:"kind"`
	Family string `json:"family,omitempty"`
}

// upstreamCatalogJSON 是清单本身的出处读数：这份选单从哪来、有多新、这个平台
// 收录没有。Listed=false 不是错误——那是「这家平台的官方型号表还没核对」，
// 对话框只提供「自定义」。
type upstreamCatalogJSON struct {
	Origin    string `json:"origin"` // builtin | synced
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Listed    bool   `json:"listed"`
	Vendor    string `json:"vendor,omitempty"`
	Note      string `json:"note,omitempty" i18n:"text"`
	Source    string `json:"source,omitempty"` // 当次核对的官方页面
	CheckedAt string `json:"checked_at,omitempty"`
}

type upstreamModelsResponse struct {
	UpstreamID   int64               `json:"upstream_id"`
	UpstreamName string              `json:"upstream_name"`
	UpstreamType string              `json:"upstream_type"`
	Priority     int64               `json:"priority"` // 本次添加会用的来源优先级
	Kinds        []upstreamKindJSON  `json:"kinds"`
	Catalog      upstreamCatalogJSON `json:"catalog"`
	Models       []upstreamModelJSON `json:"models"`
	// 本次 POST 的产出（GET 恒 0）：新建了几个模型行、新挂了几条来源。
	// 两个数分开报——新建模型与给既有模型多挂一条来源，对管理员是两回事。
	Created int `json:"created"`
	Added   int `json:"added"`
}

// upstreamForModels 取路径上的账号并挡掉不该从这条入口操作的类型。
// 失败时已写响应。
func (s *Server) upstreamForModels(w http.ResponseWriter, r *http.Request) (*store.Upstream, bool) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "上游账号不存在")
		return nil, false
	}
	up, err := s.st.GetUpstreamByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "上游账号不存在")
		} else {
			s.internalError(w, r, err)
		}
		return nil, false
	}
	return up, true
}

// servableKinds 列出这条账号能承载的模型种类与各自的协议面，次序恒为
// 文本 → 视频 → 图像（对话框的排列）。判据是内置端点表那一份
// （servableProtocols），不另抄一张平台能力表。
func servableKinds(up *store.Upstream) []upstreamKindJSON {
	out := make([]upstreamKindJSON, 0, 3)
	for _, kind := range []string{store.ModelKindText, store.ModelKindVideo, store.ModelKindImage} {
		protos := servableProtocols(kind, up.Type, up.BaseURL)
		if len(protos) == 0 {
			continue
		}
		row := upstreamKindJSON{Kind: kind}
		if kind != store.ModelKindText {
			// 每个上游类型在一个 kind 圈内至多服务一个协议面（内置端点表如此）。
			row.Family = protos[0]
		}
		out = append(out, row)
	}
	return out
}

// resolveFamily 定下一条待添加模型的协议面：**以这条账号服务的那一个为准**。
// 请求里带了就必须与它相等（挡住"清单文件写错"与手写请求）；text 恒空。
// 返回的 err 已是可直接回给操作者的文案。
func resolveFamily(up *store.Upstream, kind, want string) (string, error) {
	if kind == store.ModelKindText {
		if want != "" {
			return "", errors.New("文本模型不声明协议面")
		}
		if len(servableProtocols(kind, up.Type, up.BaseURL)) == 0 {
			return "", errors.New("该账号不服务文本入口")
		}
		return "", nil
	}
	protos := servableProtocols(kind, up.Type, up.BaseURL)
	if len(protos) == 0 {
		return "", fmt.Errorf("该账号不服务%s模型", kindText(kind))
	}
	fam := protos[0]
	if want != "" && want != fam {
		return "", fmt.Errorf("协议面与该账号不符（这条账号走 %s）", fam)
	}
	return fam, nil
}

// kindText 是错误文案里的种类说法（用户可见文案的词汇同管理台 kindLabel）。
func kindText(kind string) string {
	switch kind {
	case store.ModelKindVideo:
		return "视频"
	case store.ModelKindImage:
		return "图像"
	default:
		return "文本"
	}
}

// catalogEntries 把平台模型信息里本平台的条目，按当前目录状态标注成对话框要的
// 清单。形态不合设备词汇的条目（名字不合规、种类不认识、协议面与本账号对不上）
// **整条丢掉**：它们提交上来必然 400，摆在清单里只会引人去撞。
func (s *Server) catalogEntries(doc platformcatalog.Doc, up *store.Upstream, entries []platformcatalog.Model, models []store.ModelWithSources) []upstreamModelJSON {
	byName := make(map[string]*store.ModelWithSources, len(models))
	for i := range models {
		byName[models[i].Name] = &models[i]
	}
	out := make([]upstreamModelJSON, 0, len(entries))
	for _, e := range entries {
		if len(sourceProtocols(doc, e.Kind, e.Name, e.UpstreamModelID, up.Type, up.CatalogID, up.BaseURL)) == 0 {
			continue
		}
		if validateCatalogName(e.Name) != nil {
			continue
		}
		if e.UpstreamModelID != "" && validateCatalogName(e.UpstreamModelID) != nil {
			continue
		}
		switch e.Kind {
		case store.ModelKindText, store.ModelKindVideo, store.ModelKindImage:
		default:
			continue
		}
		family, err := resolveFamily(up, e.Kind, e.Family)
		if err != nil {
			continue
		}
		row := upstreamModelJSON{
			Name:            e.Name,
			Kind:            e.Kind,
			Family:          family,
			UpstreamModelID: e.UpstreamModelID,
			Note:            clipDisplay(e.Note),
		}
		if m, ok := byName[e.Name]; ok {
			row.InCatalog = true
			switch {
			case m.Kind != e.Kind:
				row.Blocked = blockKindMismatch
			case m.Family != "" && m.Family != family:
				row.Blocked = blockFamilyMismatch
			default:
				for _, src := range m.Sources {
					if src.UpstreamID == up.ID {
						row.Added = true
						break
					}
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// upstreamModelsSnapshot 组装 GET 与 POST 共用的响应（POST 回添加后的最新
// 清单，前端原地重绘，不必再发一次 GET）。
func (s *Server) upstreamModelsSnapshot(w http.ResponseWriter, r *http.Request, up *store.Upstream) (upstreamModelsResponse, bool) {
	doc, status := s.effectivePlatformModels(r.Context())
	resp := upstreamModelsResponse{
		UpstreamID:   up.ID,
		UpstreamName: up.Name,
		UpstreamType: up.Type,
		Priority:     defaultPriorityForUpstream(up),
		Kinds:        servableKinds(up),
		Catalog: upstreamCatalogJSON{
			Origin:    status.Origin,
			Version:   status.Version,
			UpdatedAt: status.UpdatedAt,
		},
		Models: []upstreamModelJSON{},
	}
	models, err := s.st.ListModelsWithSources(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return resp, false
	}
	if p, ok := doc.PlatformByID(up.CatalogID, up.Type); ok {
		resp.Catalog.Listed = true
		resp.Catalog.Vendor = clipDisplay(p.Vendor)
		resp.Catalog.Note = clipDisplay(p.Note)
		resp.Catalog.Source = clipDisplay(p.Source)
		resp.Catalog.CheckedAt = clipDisplay(p.CheckedAt)
		resp.Models = s.catalogEntries(doc, up, p.Models, models)
	}
	return resp, true
}

// handleListUpstreamModels 是 GET /admin/v1/upstreams/{id}/models（只读）。
func (s *Server) handleListUpstreamModels(w http.ResponseWriter, r *http.Request) {
	up, ok := s.upstreamForModels(w, r)
	if !ok {
		return
	}
	resp, ok := s.upstreamModelsSnapshot(w, r, up)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddUpstreamModels 是 POST /admin/v1/upstreams/{id}/models：
// body {"models":[{"name","kind","family","upstream_model_id"}]}，幂等添加。
//
// 名字**不限于清单**：自定义是产品要求（厂商上新而清单还没更新时照样录得进去），
// 所以这里的裁决者是与手工建模同一条的校验路径，不是那份文件。
func (s *Server) handleAddUpstreamModels(w http.ResponseWriter, r *http.Request) {
	up, ok := s.upstreamForModels(w, r)
	if !ok {
		return
	}
	var req struct {
		Models []struct {
			Name            string `json:"name"`
			Kind            string `json:"kind"`
			Family          string `json:"family"`
			UpstreamModelID string `json:"upstream_model_id"`
		} `json:"models"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Models) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "至少选择一个模型")
		return
	}
	if len(req.Models) > maxAddModels {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("一次最多添加 %d 个模型", maxAddModels))
		return
	}
	// 先整批校验再写：一半写进去一半报错的结果最难向操作者交代（模型列会长出
	// 几行、对话框却红着），而校验不碰库、代价可以忽略。同名重复项在这里合并
	// ——同一个名字提交两次，第二次本就是幂等空转。
	type pick struct {
		name, kind, family, upstreamModelID string
	}
	picks := make([]pick, 0, len(req.Models))
	seen := make(map[string]bool, len(req.Models))
	doc, _ := s.effectivePlatformModels(r.Context())
	for _, m := range req.Models {
		if err := validateCatalogName(m.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_name",
				fmt.Sprintf("模型名 %q 不合法：%s", clipPricingField(m.Name), err.Error()))
			return
		}
		if m.UpstreamModelID != "" {
			if err := validateCatalogName(m.UpstreamModelID); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_upstream_model_id",
					fmt.Sprintf("模型 %q 的来源侧模型 ID 不合法：%s", m.Name, err.Error()))
				return
			}
		}
		switch m.Kind {
		case store.ModelKindText, store.ModelKindVideo, store.ModelKindImage:
		default:
			writeError(w, http.StatusBadRequest, "invalid_kind",
				fmt.Sprintf("模型 %q 的种类 %q 未知（可选 %s|%s|%s）", m.Name, clipPricingField(m.Kind),
					store.ModelKindText, store.ModelKindVideo, store.ModelKindImage))
			return
		}
		family, err := resolveFamily(up, m.Kind, m.Family)
		if err != nil {
			writeError(w, http.StatusBadRequest, "source_kind_unservable",
				fmt.Sprintf("模型 %q 加不到这个账号上：%s", m.Name, err.Error()))
			return
		}
		if seen[m.Name] {
			continue
		}
		if len(sourceProtocols(doc, m.Kind, m.Name, m.UpstreamModelID, up.Type, up.CatalogID, up.BaseURL)) == 0 {
			writeError(w, http.StatusBadRequest, "source_protocol_unservable",
				fmt.Sprintf("模型 %q 的上游协议当前无法由固件承载，请选择其他模型", m.Name))
			return
		}
		seen[m.Name] = true
		picks = append(picks, pick{name: m.Name, kind: m.Kind, family: family, upstreamModelID: m.UpstreamModelID})
	}

	created, added := 0, 0
	for _, p := range picks {
		ok, newModel, newSource := s.addUpstreamModel(w, r, up, p.name, p.kind, p.family, p.upstreamModelID)
		if !ok {
			return // addUpstreamModel 已写响应
		}
		if newModel {
			created++
		}
		if newSource {
			added++
		}
	}
	resp, ok := s.upstreamModelsSnapshot(w, r, up)
	if !ok {
		return
	}
	resp.Created, resp.Added = created, added
	s.log.Info("账号添加模型完成", "upstream", up.Name, "type", up.Type,
		"picked", len(picks), "created", created, "added", added)
	writeJSON(w, http.StatusOK, resp)
}

// addUpstreamModel 幂等添加一个模型：模型行缺则建、未声明族则认领、来源行缺
// 则挂。返回 (是否成功, 是否新建了模型行, 是否新挂了来源)；失败时已写响应。
func (s *Server) addUpstreamModel(w http.ResponseWriter, r *http.Request, up *store.Upstream, name, kind, family, upstreamModelID string) (ok, createdModel, createdSource bool) {
	m, err := s.st.GetModelByName(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		m, err = s.st.CreateModel(r.Context(), name, kind, "")
		if err != nil {
			if errors.Is(err, store.ErrConflict) { // 并发建行：转认领路径
				return s.addUpstreamModel(w, r, up, name, kind, family, upstreamModelID)
			}
			s.internalError(w, r, err)
			return false, false, false
		}
		createdModel = true
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventModelCreate,
			Entity: entityModel(m.ID),
			Detail: fmt.Sprintf("name=%s kind=%s family=%s entries=%s pricing=%s upstream=%s",
				m.Name, m.Kind, familyAudit(family), entriesLabel(true, true, true), pricingAudit(""), up.Name),
			RemoteIP: remoteIP(r),
		})
	case err != nil:
		s.internalError(w, r, err)
		return false, false, false
	default:
		// 同名行已在：种类不同、已声明的异族都不能认领——那是另一个模型，
		// 覆盖等于偷走别人的名字。
		if m.Kind != kind {
			writeError(w, http.StatusConflict, "model_kind_conflict",
				fmt.Sprintf("模型名 %q 已被一个%s模型占用，请换个名字或先处理那一行", name, kindText(m.Kind)))
			return false, false, false
		}
		if m.Family != "" && m.Family != family {
			writeError(w, http.StatusConflict, "source_family_mismatch",
				fmt.Sprintf("模型 %q 声明的协议面与这个账号走的不同——模型的全部上游必须服务同一个协议面", name))
			return false, false, false
		}
	}
	// 未声明族的存量行（或刚建的行）在这里钉族；并发钉成异值收敛为族冲突。
	if family != "" && m.Family != family {
		if err := s.st.SetModelFamilyIfUnset(r.Context(), m.ID, family); err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeError(w, http.StatusConflict, "source_family_mismatch",
					fmt.Sprintf("模型 %q 声明的协议面与这个账号走的不同——模型的全部上游必须服务同一个协议面", name))
			} else {
				s.internalError(w, r, err)
			}
			return false, false, false
		}
	}
	priority := defaultPriorityForUpstream(up)
	src, err := s.st.CreateModelSource(r.Context(), m.ID, up.ID, upstreamModelID, priority)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return true, createdModel, false // 已挂过（UNIQUE(model, upstream)）：幂等成功
		}
		s.internalError(w, r, err)
		return false, false, false
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventSourceCreate,
		Entity: entitySource(src.ID),
		Detail: fmt.Sprintf("model=%s kind=%s upstream=%s type=%s upstream_model_id=%s priority=%d",
			m.Name, m.Kind, up.Name, up.Type, src.UpstreamModelID, src.Priority),
		RemoteIP: remoteIP(r),
	})
	return true, createdModel, true
}

// defaultPriorityForType 按上游的计费模式给出新来源的缺省优先级：订阅型
// 先用满（100），按量与泛型兜底排后（200）。同表在管理台 api.ts 的
// defaultPriorityFor —— 两处必须同值，否则「界面显示的默认值」与「直接调
// API 得到的默认值」会不一样。
func defaultPriorityForUpstream(up *store.Upstream) int64 {
	switch up.BillingMode {
	case platformcatalog.BillingSubscription:
		return defaultSourcePriority
	case platformcatalog.BillingUsage:
		return usageSourcePriority
	default:
		if up.Type == config.UpstreamMock {
			return defaultSourcePriority
		}
		return usageSourcePriority
	}
}
