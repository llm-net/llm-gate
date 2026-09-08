// limits_test.go 是 iteration-9 Phase 4 的可执行验收（管理写面这一半）：
// 密钥的预算与 RPM 三态入参（不传 / null / 数字），以及模型目录价按 kind
// 校验形态字段集。
//
// 复用 server_test.go 的 env/do 装置（同属 admin_test 包）。
package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// patchKey 发一次 PATCH 并回状态码与响应体。
func (e *env) patchKey(cookie string, id int64, body string) *http.Response {
	e.t.Helper()
	return e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", id), cookie, body)
}

func decodeKey(t *testing.T, resp *http.Response) keyDTO {
	t.Helper()
	var out struct {
		Key keyDTO `json:"key"`
	}
	decodeInto(t, resp, &out)
	return out.Key
}

// 非法限额一律 400 invalid_limit，且**不留下半个已生效的改动**。
func TestLimitValidation(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()
	k, _ := e.createKey(admin, "k1")

	bad := []struct {
		name string
		body string
	}{
		{"负数", `{"budget_day_micro":-1}`},
		{"小数", `{"budget_day_micro":1.5}`},
		{"字符串", `{"budget_day_micro":"100"}`},
		{"对象", `{"budget_day_micro":{"a":1}}`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			// 同一请求里带一个合法的禁用位改动：400 之后它也不该生效。
			body := `{"disabled":true,` + strings.TrimPrefix(tc.body, "{")
			resp := e.patchKey(admin, k.ID, body)
			wantStatus(t, resp, http.StatusBadRequest)
			if code := errCode(t, resp); code != "invalid_limit" {
				t.Errorf("错误码 = %q，期望 invalid_limit", code)
			}
		})
	}
	// 校验失败没有留下半个改动。
	resp := e.do("GET", "/admin/v1/keys", admin, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	decodeInto(t, resp, &list)
	for _, got := range list.Keys {
		if got.ID == k.ID {
			if got.Disabled {
				t.Error("400 之后禁用位也被改了")
			}
			if got.BudgetDayMicro != nil {
				t.Errorf("400 之后预算也落库了: %v", got.BudgetDayMicro)
			}
		}
	}
	// 负的 RPM 同样拒绝。
	resp = e.patchKey(admin, k.ID, `{"rpm_limit":-5}`)
	wantStatus(t, resp, http.StatusBadRequest)
}

// 密钥的三项限额与禁用位可以同请求提交；只带限额也合法（老守卫要求
// disabled 必填，现在放宽）。
func TestKeyLimitsPatch(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()
	k, _ := e.createKey(admin, "k1")

	if k.BudgetDayMicro != nil || k.RPMLimit != nil {
		t.Fatalf("新签发的 Key 应无限额: %+v", k)
	}
	// 只带限额（不带 disabled）。
	resp := e.patchKey(admin, k.ID, `{"budget_day_micro":1000000,"rpm_limit":60}`)
	wantStatus(t, resp, http.StatusOK)
	got := decodeKey(t, resp)
	if got.BudgetDayMicro == nil || *got.BudgetDayMicro != 1_000_000 ||
		got.RPMLimit == nil || *got.RPMLimit != 60 {
		t.Fatalf("限额未落库: %+v", got)
	}
	if got.BudgetMonthMicro != nil {
		t.Errorf("没传的月额应保持不限，实际 %v", got.BudgetMonthMicro)
	}

	// 限额 + 禁用位同请求：两件事都要生效（禁用位的幂等短路不能吞掉限额）。
	resp = e.patchKey(admin, k.ID, `{"disabled":true,"rpm_limit":null}`)
	wantStatus(t, resp, http.StatusOK)
	got = decodeKey(t, resp)
	if !got.Disabled {
		t.Error("disabled 未生效")
	}
	if got.RPMLimit != nil {
		t.Errorf("rpm_limit 的 null 未清除，实际 %v", got.RPMLimit)
	}
	if got.BudgetDayMicro == nil || *got.BudgetDayMicro != 1_000_000 {
		t.Errorf("没传的日额被清掉了: %v", got.BudgetDayMicro)
	}

	// 一个字段都不带仍是 400。
	resp = e.patchKey(admin, k.ID, `{}`)
	wantStatus(t, resp, http.StatusBadRequest)

	// 审计留痕：限额变更单列一个事件。
	assertAuditHas(t, e, "key.limits_update")
}

// 标签可在签发后修改或清空；它只影响管理展示，不改变 Key。超长标签与同请求
// 携带的其他改动要一起拒绝，避免留下半个已生效的 PATCH。
func TestKeyLabelPatch(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()
	k, _ := e.createKey(admin, "旧标签")

	resp := e.patchKey(admin, k.ID, `{"label":"开发机"}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.Label != "开发机" {
		t.Fatalf("更新后标签 = %q，期望 开发机", got.Label)
	}
	resp = e.patchKey(admin, k.ID, `{"label":""}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.Label != "" {
		t.Fatalf("清除后标签 = %q，期望空串", got.Label)
	}

	tooLong := strings.Repeat("界", 129)
	resp = e.patchKey(admin, k.ID, fmt.Sprintf(`{"label":%q,"disabled":true}`, tooLong))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "bad_request" {
		t.Errorf("超长标签错误码 = %q，期望 bad_request", code)
	}
	resp = e.do("GET", "/admin/v1/keys", admin, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	decodeInto(t, resp, &list)
	if len(list.Keys) != 1 || list.Keys[0].Label != "" || list.Keys[0].Disabled {
		t.Fatalf("非法 PATCH 后 Key 被部分修改: %+v", list.Keys)
	}

	wantStatus(t, e.patchKey(admin, 9999, `{"label":"missing"}`), http.StatusNotFound)
	wantStatus(t, e.patchKey(admin, k.ID, `{"label":null}`), http.StatusBadRequest)
	assertAuditHas(t, e, "key.label_update")
}

// 按量额度走单独的增量端点：正数增加、负数扣减、扣穿钳 0；0 / 缺失 /
// 非整数拒绝。它不混进 PATCH，避免并发计量扣减被绝对值覆盖。
func TestKeyMeteredAllowanceAdjust(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()
	k, plaintext := e.createKey(admin, "allowance")
	adjust := func(id int64, body string) *http.Response {
		return e.do("POST", fmt.Sprintf("/admin/v1/keys/%d/metered-allowance", id), admin, body)
	}

	if k.MeteredAllowanceMicro != 0 {
		t.Fatalf("新 Key 按量额度应为 0，实际 %d", k.MeteredAllowanceMicro)
	}
	resp := adjust(k.ID, `{"delta_micro":3000000}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.MeteredAllowanceMicro != 3_000_000 {
		t.Fatalf("增加后剩余 = %d，期望 3000000", got.MeteredAllowanceMicro)
	}
	resp = adjust(k.ID, `{"delta_micro":-1000000}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.MeteredAllowanceMicro != 2_000_000 {
		t.Fatalf("扣减后剩余 = %d，期望 2000000", got.MeteredAllowanceMicro)
	}
	resp = adjust(k.ID, `{"delta_micro":-9000000}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.MeteredAllowanceMicro != 0 {
		t.Fatalf("扣穿后应钳 0，实际 %d", got.MeteredAllowanceMicro)
	}

	// 数据库还没到五分钟冲刷点时，列表与调整响应都要扣掉计量器内存待扣，
	// 不能短暂展示数据库里的旧剩余。
	wantStatus(t, adjust(k.ID, `{"delta_micro":5000000}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", k.ID), admin,
		`{"budget_day_micro":0}`), http.StatusOK)
	ka, err := e.st.LookupKeyByDigest(context.Background(), digestOf(plaintext))
	if err != nil {
		t.Fatalf("LookupKeyByDigest: %v", err)
	}
	if d := e.meter.Admit(*ka); !d.Allowed {
		t.Fatalf("零预算有按量额度应放行: %+v", d)
	}
	e.spend(k.ID, k.DisplayPrefix+"…"+k.DisplayLast4, 500_000, 0) // 1 元待扣
	resp = e.do("GET", "/admin/v1/keys", admin, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	decodeInto(t, resp, &list)
	if len(list.Keys) != 1 || list.Keys[0].MeteredAllowanceMicro != 4_000_000 {
		t.Fatalf("列表即时按量额度 = %+v，期望 4000000", list.Keys)
	}
	resp = adjust(k.ID, `{"delta_micro":1000000}`)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeKey(t, resp); got.MeteredAllowanceMicro != 5_000_000 {
		t.Fatalf("有待扣时增加后的即时剩余 = %d，期望 5000000", got.MeteredAllowanceMicro)
	}

	for _, body := range []string{
		`{"delta_micro":0}`, `{}`, `{"delta_micro":1.5}`,
		`{"delta_micro":"1"}`, `{"delta_micro":null}`,
	} {
		resp = adjust(k.ID, body)
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != "invalid_delta" {
			t.Errorf("body=%s 错误码 = %q，期望 invalid_delta", body, code)
		}
	}
	wantStatus(t, adjust(9999, `{"delta_micro":1}`), http.StatusNotFound)
	assertAuditHas(t, e, "key.metered_allowance_adjust")
}

// ---- 模型目录价 ----

// 目录价按 kind 校验形态字段集，未知字段拒绝；null / {} = 未定价。
func TestModelPricing(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()

	// 建模时一并录价。
	m := e.createModelBody(admin,
		`{"name":"priced-text","kind":"text","pricing":{"in":3000000,"out":12000000}}`)
	if len(m.Pricing) != 2 || m.Pricing["in"] != 3_000_000 || m.Pricing["out"] != 12_000_000 {
		t.Fatalf("建模时的目录价未落库: %+v", m.Pricing)
	}

	// PATCH 覆盖整张表。
	resp := e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), admin,
		`{"pricing":{"in":1,"out":2,"cache_read":0}}`)
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Model modelDTO `json:"model"`
	}
	decodeInto(t, resp, &out)
	if len(out.Model.Pricing) != 3 || out.Model.Pricing["cache_read"] != 0 {
		t.Fatalf("改价未生效: %+v", out.Model.Pricing)
	}

	// null 清为未定价；{} 同义。
	for _, body := range []string{`{"pricing":null}`, `{"pricing":{}}`} {
		resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), admin, body)
		wantStatus(t, resp, http.StatusOK)
		decodeInto(t, resp, &out)
		if len(out.Model.Pricing) != 0 {
			t.Errorf("%s 应清为未定价，实际 %+v", body, out.Model.Pricing)
		}
		// 复位，好让下一轮的"清除"是真的有东西可清（in/out 必须成对）。
		wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), admin,
			`{"pricing":{"in":1,"out":1}}`), http.StatusOK)
	}

	// 未定价模型的 pricing 恒为 null（前端据此挂警示徽章）。
	plain := e.createModel(admin, "unpriced")
	if plain.Pricing != nil {
		t.Errorf("未定价模型的 pricing 应为 null，实际 %+v", plain.Pricing)
	}
}

// 形态字段集按 kind 圈定：串了种类的字段一律 400 invalid_pricing。
func TestModelPricingKindFieldSet(t *testing.T) {
	e := newEnv(t)
	admin := e.rootSession()

	bad := []struct {
		name, kind, pricing string
	}{
		{"文本模型收视频字段", "text", `{"ark_video_token":1}`},
		{"视频模型收文本字段", "video", `{"in":1}`},
		{"图片模型收秒价", "image", `{"minimax_video_sec_2k":1}`},
		{"拼错的字段名", "text", `{"input":1}`},
		{"负价", "text", `{"in":-1}`},
		{"小数价", "text", `{"in":1.5}`},
		{"字符串价", "text", `{"in":"1"}`},
		{"越界价", "text", `{"in":1000000000001,"out":1}`},
		// 成对字段只填一半：另一档会被静默按 0 元或按邻档计费，而模型仍显示
		// 为「已定价」——外审点名的那类静默错账，挡在写入侧。
		{"文本只填输入价", "text", `{"in":1}`},
		{"文本只填输出价", "text", `{"out":1}`},
		{"Seedance 只填一档", "video", `{"ark_video_token":1}`},
		{"Context-IR 只填输入价", "video", `{"minimax_context_ir_in":1}`},
	}
	for i, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do("POST", "/admin/v1/models", admin,
				fmt.Sprintf(`{"name":"bad-%d","kind":%q,"pricing":%s}`, i, tc.kind, tc.pricing))
			wantStatus(t, resp, http.StatusBadRequest)
			if code := errCode(t, resp); code != "invalid_pricing" {
				t.Errorf("错误码 = %q，期望 invalid_pricing", code)
			}
		})
	}

	// 每个 kind 的合法字段集都收得下（含 Phase 4 新补的 minimax token 计费档）。
	ok := []struct{ kind, pricing string }{
		{"text", `{"in":1,"out":2,"cache_read":3}`},
		// cache_read 可选（缺省按 in 计）；minimax 秒价两档互为回退，
		// 单配一档是明写的设计，不受成对规则约束。
		{"text", `{"in":1,"out":2}`},
		{"video", `{"minimax_video_sec_2k":4}`},
		{"image", `{"ark_image_each":1}`},
		{"video", `{"ark_video_token":1,"ark_video_token_ref":2,"minimax_video_sec_768p":3,` +
			`"minimax_video_sec_2k":4,"minimax_video_image_extra":5,` +
			`"minimax_context_ir_in":6,"minimax_context_ir_out":7}`},
		{"image", `{"ark_image_each":1,"ark_image_token":2}`},
	}
	for i, tc := range ok {
		resp := e.do("POST", "/admin/v1/models", admin,
			fmt.Sprintf(`{"name":"good-%d","kind":%q,"pricing":%s}`, i, tc.kind, tc.pricing))
		wantStatus(t, resp, http.StatusCreated)
	}

	// 改价留审计（价格摘要是运营参数，如实记）。
	assertAuditHas(t, e, "model.pricing", func() {
		m := e.createModel(admin, "audited")
		wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), admin,
			`{"pricing":{"in":42,"out":84}}`), http.StatusOK)
	})
}

// ---- 小工具 ----

// assertAuditHas 断言审计表里出现过某个事件；可选的 setup 先跑一遍触发它。
// 直接以只读 SQL 读库——store 依约不提供审计读接口（同 TestAuditTrail）。
func assertAuditHas(t *testing.T, e *env, event string, setup ...func()) {
	t.Helper()
	for _, fn := range setup {
		fn()
	}
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event = ?`, event).Scan(&n); err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	if n == 0 {
		t.Errorf("审计表里没有 %s 事件", event)
	}
}
