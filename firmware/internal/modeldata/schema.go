package modeldata

// 数据仓库文件的**读取子集**：只声明合成要用到的字段，其余（证据、审阅状态、
// 套餐、上下文长度……）原样跳过。字段名与 schemas/*.schema.json 逐字对齐。

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Provider 是 providers/<id>/provider.json。
type Provider struct {
	ID          string           `json:"id"`
	DisplayName string           `json:"display_name"`
	Sources     []ProviderSource `json:"sources"`
}

// ProviderSource 是一条官方资料入口；模型 verification.source_ids 引用它的 ID。
type ProviderSource struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Purpose string `json:"purpose"`
}

// Catalog 是 providers/<id>/offerings/<offering>/catalog.json。
type Catalog struct {
	SchemaVersion int     `json:"schema_version"`
	ProviderID    string  `json:"provider_id"`
	ID            string  `json:"id"`
	DisplayName   string  `json:"display_name"`
	Kind          string  `json:"kind"` // pay_as_you_go | api_subscription | tool_subscription
	Market        string  `json:"market"`
	Models        []Model `json:"models"`
}

// Model 是产品内的一条模型。
type Model struct {
	ID              string       `json:"id"`
	RequestID       *string      `json:"request_id"`
	DisplayName     string       `json:"display_name"`
	Modalities      []string     `json:"modalities"`
	Availability    string       `json:"availability"`
	AliasOf         *string      `json:"alias_of"`
	Verification    Verification `json:"verification"`
	UsagePrices     Prices       `json:"usage_prices"`
	ReferencePrices Prices       `json:"reference_prices"`
	Capabilities    Capabilities `json:"capabilities"`
}

// Verification 是核对记录里合成要看的三个字段。
type Verification struct {
	Status    string   `json:"status"`
	CheckedAt *string  `json:"checked_at"`
	SourceIDs []string `json:"source_ids"`
}

// Prices 是一组价格（usage_prices / reference_prices）。
type Prices struct {
	Status string `json:"status"` // published | unknown | not_applicable
	Rules  []Rule `json:"rules"`
}

// Rule 是价格规则；当前发布每组最多一条。
type Rule struct {
	Currency     string       `json:"currency"`
	Rates        []Rate       `json:"rates"`
	TimePricing  *TimePricing `json:"time_pricing"`
	Verification Verification `json:"verification"`
}

// Rate 是一个计费分量。
type Rate struct {
	Meter    string    `json:"meter"`
	Amount   string    `json:"amount"`
	Per      int64     `json:"per"`
	Unit     string    `json:"unit"`
	Quantity *Quantity `json:"quantity"`
}

// Quantity 是数量口径；合成只看免费量。
type Quantity struct {
	IncludedUnits *string `json:"included_units"`
}

// TimePricing 是规则内的循环分时价（catalog schema_version=2）。
type TimePricing struct {
	Timezone string `json:"timezone"`
	Bands    []Band `json:"bands"`
}

// Band 是一个时段档：days 为 ISO 星期（1–7），hours 为 "HH:MM-HH:MM"；
// 两者都缺即无条件兜底档。按序首个匹配者胜。
type Band struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Days  []int    `json:"days"`
	Hours []string `json:"hours"`
	Rates []Rate   `json:"rates"`
}

// Capabilities 读取协议面与模型声明的输入模态。
type Capabilities struct {
	Interfaces      []string `json:"interfaces"`
	InputModalities []string `json:"input_modalities"`
}

// Input 是一次合成的全部输入：数据仓库某个 tag 下的服务商与产品。
type Input struct {
	Tag       Tag
	Commit    string
	Providers map[string]Provider
	Catalogs  []Catalog // 按 provider_id、offering id 排序
}

// Load 从数据仓库读取 ref（通常是 tag 名）下的全部服务商与产品。
func Load(repo Repo, ref string) (Input, error) {
	tag, ok := ParseTag(ref)
	if !ok {
		return Input{}, fmt.Errorf("%q 不是 data-YYYY.MM.DD.N 形式的正式数据版本", ref)
	}
	commit, err := repo.Commit(ref)
	if err != nil {
		return Input{}, err
	}
	files, err := repo.ListFiles(ref, "providers")
	if err != nil {
		return Input{}, err
	}
	in := Input{Tag: tag, Commit: commit, Providers: map[string]Provider{}}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "/provider.json") && strings.Count(f, "/") == 2:
			raw, err := repo.ReadFile(ref, f)
			if err != nil {
				return Input{}, err
			}
			var p Provider
			if err := json.Unmarshal(raw, &p); err != nil {
				return Input{}, fmt.Errorf("%s: %v", f, err)
			}
			if p.ID == "" {
				return Input{}, fmt.Errorf("%s: 缺 id", f)
			}
			in.Providers[p.ID] = p
		case path.Base(f) == "catalog.json" && strings.Contains(f, "/offerings/"):
			raw, err := repo.ReadFile(ref, f)
			if err != nil {
				return Input{}, err
			}
			var c Catalog
			if err := json.Unmarshal(raw, &c); err != nil {
				return Input{}, fmt.Errorf("%s: %v", f, err)
			}
			if c.ProviderID == "" || c.ID == "" {
				return Input{}, fmt.Errorf("%s: 缺 provider_id / id", f)
			}
			in.Catalogs = append(in.Catalogs, c)
		}
	}
	sort.Slice(in.Catalogs, func(i, j int) bool {
		a, b := in.Catalogs[i], in.Catalogs[j]
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		return a.ID < b.ID
	})
	for _, c := range in.Catalogs {
		if _, ok := in.Providers[c.ProviderID]; !ok {
			return Input{}, fmt.Errorf("产品 %s/%s 没有对应的 provider.json", c.ProviderID, c.ID)
		}
	}
	return in, nil
}

// sourceURL 按 verification.source_ids 找官方页面；找不到时按用途回退到服务商
// 登记的入口（pricing → models → 任意）。
func (p Provider) sourceURL(v Verification, purposes ...string) string {
	byID := make(map[string]string, len(p.Sources))
	for _, s := range p.Sources {
		byID[s.ID] = s.URL
	}
	for _, id := range v.SourceIDs {
		if u := byID[id]; u != "" {
			return u
		}
	}
	for _, purpose := range purposes {
		for _, s := range p.Sources {
			if s.Purpose == purpose && s.URL != "" {
				return s.URL
			}
		}
	}
	if len(p.Sources) > 0 {
		return p.Sources[0].URL
	}
	return ""
}

// checkedDate 把 RFC 3339 的核对时刻裁成 YYYY-MM-DD；缺席返回空串。
func checkedDate(v Verification) string {
	if v.CheckedAt == nil || len(*v.CheckedAt) < 10 {
		return ""
	}
	return (*v.CheckedAt)[:10]
}
