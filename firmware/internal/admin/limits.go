package admin

// limits.go 是预算与限额字段的收发口径（iteration-9 Phase 4）：用户与密钥的
// 日/月预算、密钥的 RPM 上限。三条口径贯穿本文件：
//
//   - **传输单位是整数微元**（1 元 = 10⁶ 微元）。元↔微元的换算在前端做，
//     线上恒为整数——金额禁止浮点（根 AGENTS.md「金额」硬约束），而 JSON
//     数字一旦被 float64 接住，就不再保证是原来那个整数。
//   - **「不传」「传 null」「传数字」是三件不同的事**：不传 = 本次不改这一项，
//     null = 清除限额（回到不限），数字 = 设成这个值。Go 的 *int64 分不开前
//     两者（都解成 nil），所以入参收 json.RawMessage 自己判。
//   - **0 是一个合法限额**（零额度 = 一律拒绝），与 null（不限）泾渭分明；
//     负数拒绝。这条与 store 的 `nil = 不限` 与 usage 的
//     `已用 >= 限额即拒` 是同一套语义，别在任何一层把 0 当成"没设置"。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// parseLimit 解析一个可清除的限额入参。三态：
//
//	present=false            字段缺席，本次不改
//	present=true,  v == nil  显式 null，清除限额（不限）
//	present=true,  v != nil  设成 *v
//
// name 只用于错误文案，是本文件写死的字段名，不是调用方给的内容。
func parseLimit(raw json.RawMessage, name string) (v *int64, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	// 解进 any 而不是 json.Number：后者的底层类型是 string，会把带引号的
	// "100" 也照收（同 store.validatePricing 的理由）。any 目标下字符串解成
	// string、数字才解成 json.Number，两者泾渭分明。
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, false, limitErr(name)
	}
	if decoded == nil {
		return nil, true, nil // 显式 null = 清除
	}
	num, ok := decoded.(json.Number)
	if !ok {
		return nil, false, limitErr(name)
	}
	n, err := num.Int64()
	if err != nil {
		return nil, false, limitErr(name)
	}
	if n < 0 {
		return nil, false, fmt.Errorf("%s 不得为负（0 表示零额度，不限请传 null）", name)
	}
	return &n, true, nil
}

func limitErr(name string) error {
	return fmt.Errorf("%s 须为非负整数，或 null（表示不限）", name)
}

// parseMeteredAllowanceDelta 解析按量额度的增减金额：必填、**非零**整数微元
// （正 = 增加、负 = 扣减）。与 parseLimit 同样经 json.Number 收紧——带引号
// 的数字、小数与越界值一律拒收，金额在线上恒为整数。
func parseMeteredAllowanceDelta(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("缺少 delta_micro（整数微元：正数增加、负数扣减）")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return 0, fmt.Errorf("delta_micro 须为非零整数微元（正数增加、负数扣减）")
	}
	num, ok := decoded.(json.Number)
	if !ok {
		return 0, fmt.Errorf("delta_micro 须为非零整数微元（正数增加、负数扣减）")
	}
	n, err := num.Int64()
	if err != nil {
		return 0, fmt.Errorf("delta_micro 须为非零整数微元（小数、科学计数与越界值不收）")
	}
	if n == 0 {
		return 0, fmt.Errorf("delta_micro 不能为 0（正数增加、负数扣减）")
	}
	return n, nil
}

// pickLimit 在「本次没传」时沿用当前值——两列/三列一并写的存储接口
// （SetUserBudgets / SetAPIKeyLimits）要求每次都给全，缺的那一项必须是现值
// 而不是零值，否则改一项会把另一项静默清掉。
func pickLimit(v *int64, present bool, current *int64) *int64 {
	if present {
		return v
	}
	return current
}

// sameLimit 判两个限额是否相同（含 nil = 不限）。用于「无变更就不写库、
// 不写审计」——幂等 PATCH 不该在审计里堆出一串什么都没改的行。
func sameLimit(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// limitText 把一个限额渲染进审计 detail（nil = 不限）。金额与次数都是运营
// 参数，不是凭证也不是内容，如实记录。
func limitText(v *int64) string {
	if v == nil {
		return "不限"
	}
	return strconv.FormatInt(*v, 10)
}
