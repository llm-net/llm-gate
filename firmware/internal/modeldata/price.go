package modeldata

// 原币种十进制 → 设备价目字段（整数微元）的换算。
//
// 设备价目字段的单位：token 类是「微元 / 百万 token」，按张 / 按秒类是「微元 / 单位」。
// 数据仓库的 rate 是 amount（原币种十进制字符串）/ per（单位数）。全程 big.Rat，
// 四舍五入到整数微元；不出现浮点。美元按固定汇率折算（与旧价目文件同一口径），
// 汇率随文件 source 段一起发布。

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// fieldRule 说明一个 meter 落到哪个设备字段、按什么单位换算。
type fieldRule struct {
	Field      string
	PerMillion bool // true：字段单位是「/ 百万」；false：「/ 单位」
}

// meterFields：（kind, 厂商协议面）→ meter → 设备字段。协议面为空串即文本。
var meterFields = map[string]map[string]fieldRule{
	"text": {
		"input_uncached_tokens": {usage.FieldIn, true},
		"input_cached_tokens":   {usage.FieldCacheRead, true},
		"output_tokens":         {usage.FieldOut, true},
		"cache_write_5m_tokens": {usage.FieldCacheWrite, true},
	},
	config.ProtocolArkVideo: {
		"video_output_tokens": {usage.FieldArkVideoToken, true},
	},
	config.ProtocolArkImage: {
		"generated_images":    {usage.FieldArkImageEach, false},
		"image_output_tokens": {usage.FieldArkImageToken, true},
	},
	config.ProtocolMinimaxVideo: {
		"video_output_seconds": {usage.FieldMinimaxSec768p, false},
		"input_images":         {usage.FieldMinimaxImageExtra, false},
	},
}

// ignoredMeters 是有意不进设备价目的分量（设备的计价形态装不下或按同价折算）。
// 报告里不再逐条点名。
var ignoredMeters = map[string]bool{
	"cache_write_1h_tokens": true, // 设备只建模 5 分钟缓存写入
	"video_input_seconds":   true, // MiniMax 输入秒按输出档单价计（固件口径），不单列
}

var (
	million = big.NewRat(1_000_000, 1)
	half    = big.NewRat(1, 2)
)

// fx 是汇率表：币种 → 折人民币的比率。CNY 恒 1。
type fx map[string]*big.Rat

func newFX(usdCNY string) (fx, error) {
	usd, ok := new(big.Rat).SetString(usdCNY)
	if !ok || usd.Sign() <= 0 {
		return nil, fmt.Errorf("汇率 %q 不是正的十进制数", usdCNY)
	}
	return fx{"CNY": big.NewRat(1, 1), "USD": usd}, nil
}

// toMicro 把一条 rate 换算成整数微元；perMillion 为真时按「/ 百万」单位。
func (f fx) toMicro(r Rate, currency string, perMillion bool) (int64, error) {
	rate, ok := f[currency]
	if !ok {
		return 0, fmt.Errorf("币种 %s 没有汇率", currency)
	}
	amount, ok := new(big.Rat).SetString(r.Amount)
	if !ok || amount.Sign() < 0 {
		return 0, fmt.Errorf("金额 %q 不是非负十进制数", r.Amount)
	}
	if r.Per <= 0 {
		return 0, fmt.Errorf("per %d 不是正整数", r.Per)
	}
	v := new(big.Rat).Mul(amount, rate)
	v.Mul(v, million) // 元 → 微元
	if perMillion {
		v.Mul(v, million) // / per 单位 → / 百万单位
	}
	v.Quo(v, big.NewRat(r.Per, 1))
	// 四舍五入到整数。
	v.Add(v, half)
	n := new(big.Int).Quo(v.Num(), v.Denom())
	if !n.IsInt64() || n.Int64() > store.MaxPricingMicro {
		return 0, fmt.Errorf("金额 %s %s / %d 换算后超出设备上限", r.Amount, currency, r.Per)
	}
	return n.Int64(), nil
}

// convertRates 把一组分量换成设备价目表。返回：字段表、被丢弃的 meter 名、错误。
// 没有一个分量落到设备字段时返回空表（调用方视为「未定价」）。
func (f fx) convertRates(kind, face, currency string, rates []Rate) (map[string]int64, []string, error) {
	table := meterFields[face]
	if kind == store.ModelKindText {
		table = meterFields["text"]
	}
	if table == nil {
		return nil, nil, fmt.Errorf("%s 模型的协议面 %q 没有价目字段映射", kind, face)
	}
	out := map[string]int64{}
	var dropped []string
	for _, r := range rates {
		rule, ok := table[r.Meter]
		if !ok {
			if !ignoredMeters[r.Meter] {
				dropped = append(dropped, r.Meter)
			}
			continue
		}
		micro, err := f.toMicro(r, currency, rule.PerMillion)
		if err != nil {
			return nil, nil, fmt.Errorf("%s：%v", r.Meter, err)
		}
		if _, dup := out[rule.Field]; dup {
			return nil, nil, fmt.Errorf("%s 重复落到字段 %s", r.Meter, rule.Field)
		}
		out[rule.Field] = micro
	}
	if len(out) == 0 {
		return nil, dropped, nil
	}
	// 成对字段：数据仓库每模型只有一个基准（不分含 / 不含参考视频输入），设备的
	// Seedance 两档按同一基准投影。
	if v, ok := out[usage.FieldArkVideoToken]; ok {
		if _, has := out[usage.FieldArkVideoTokenRef]; !has {
			out[usage.FieldArkVideoTokenRef] = v
		}
	}
	if err := usage.CheckPricingPairs(usage.Pricing(out)); err != nil {
		return nil, nil, err
	}
	sort.Strings(dropped)
	return out, dropped, nil
}

// pricingSource 挑一个模型用于估价的价格组：按量产品优先 usage_prices；订阅产品
// 只有 reference_prices（数据仓库口径：订阅统一按按量参考价记名义金额）。
// 两组都不是 published 时返回 nil。
func pricingSource(m Model) *Rule {
	for _, p := range []Prices{m.UsagePrices, m.ReferencePrices} {
		if p.Status == "published" && len(p.Rules) > 0 {
			r := p.Rules[0]
			return &r
		}
	}
	return nil
}

// kindOf 把 modalities 折成设备的模型种类；设备不计价的模态返回空串。
func kindOf(modalities []string) string {
	if len(modalities) != 1 {
		return ""
	}
	switch modalities[0] {
	case "text":
		return store.ModelKindText
	case "video":
		return store.ModelKindVideo
	case "image":
		return store.ModelKindImage
	}
	return ""
}

// familyOf 在模型协议面与平台承载面的交集里找唯一的 AIGC 厂商面。
func familyOf(kind string, interfaces, platformProtocols []string) (string, error) {
	var found []string
	for _, p := range platformProtocols {
		if !isAIGCFace(p) || !contains(interfaces, p) {
			continue
		}
		if kindOfFace(p) == kind {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", errors.New("没有平台承载的厂商协议面")
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("同时匹配多个厂商协议面：%s", strings.Join(found, "、"))
}

func isAIGCFace(p string) bool {
	return p == config.ProtocolArkVideo || p == config.ProtocolArkImage || p == config.ProtocolMinimaxVideo
}

func kindOfFace(p string) string {
	switch p {
	case config.ProtocolArkVideo, config.ProtocolMinimaxVideo:
		return store.ModelKindVideo
	case config.ProtocolArkImage:
		return store.ModelKindImage
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// intersect 按 order 的顺序取两者交集。
func intersect(order, set []string) []string {
	var out []string
	for _, p := range order {
		if contains(set, p) {
			out = append(out, p)
		}
	}
	return out
}
