package admin

// usage.go 是「用量」页的读数端点：GET /admin/v1/usage。
//
// 设备只有一个管理员，账也就只有一本——按密钥/模型/上游/入口/kind 五个维度
// 分解 + 按日序列 + 全量明细环，一次请求取齐。密钥级额度不在这里回：它是密钥
// 自己的属性，跟着 GET /admin/v1/keys 一起给（见 keys.go 的 keyJSON），
// 为一份额度在这里多发一次读数只会重复同一份事实。
//
// 零轮询：一次进页、一次手动刷新、切一次区间各一个请求，服务端不推、客户端
// 不轮（与「设备状态」页同一口径）。读数全部来自内存 + 已落盘的小时聚合，
// 不触发任何上游调用。
//
// §15.1：响应里只有计数、金额、时长与标识（模型名、上游账户名、密钥展示串）。
// 提示词、模型响应、密钥明文没有任何一条路径能到这里——它们从来没进过
// internal/usage 的任何结构体。

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// UsageReader 是用量读数出口（生产实现是 *usage.Meter，装配在 cmd/llmgate）。
// 只留读数端点真正用得着的方法：区间折算与读数都归计量层，管理面不自己
// 定义「今天」——预算窗口的本地时区口径只该有一份。
type UsageReader interface {
	// RangeWindow 把区间关键词折成 [from, to)；ok=false 表示关键词不认识。
	RangeWindow(name string) (from, to time.Time, ok bool)
	// Report 汇总区间用量。
	Report(ctx context.Context, from, to time.Time) (*usage.Report, error)
	// ReportForKey 汇总区间内单把 API 密钥的用量。
	ReportForKey(ctx context.Context, from, to time.Time, keyID int64) (*usage.Report, error)
	// Spend 读一把密钥当前的预算已用额（本地自然日/自然周/自然月）。
	Spend(keyID int64) usage.Spend
}

// SetUsageReader 注入用量读数出口（装配期一次性，同 SetTLSProbe）。
// 不注入时用量端点答 503：没有计量就没有数可读，编个空报表比说实话更糟。
func (s *Server) SetUsageReader(u UsageReader) {
	s.usage = u
	if meter, ok := u.(interface{ SetCursorPriceSource(usage.CursorPriceSource) }); ok {
		meter.SetCursorPriceSource(s)
	}
}

// usageResponse 是端点的响应形：Report 内嵌展开（from/to/total/by_*/days/
// recent 平铺在顶层）。
type usageResponse struct {
	// Range 回显本次生效的区间关键词（缺省 today）：前端切区间时用它对齐
	// 自己的选中态，不必自己记住发出去的是什么。
	Range string `json:"range"`
	// KeyID 回显当前统计对象；缺省表示设备全量。
	KeyID int64 `json:"key_id,omitempty"`
	usage.Report
}

// handleUsage 是用量读数：区间报表 + 五个维度分解 + 明细环。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage_unavailable", "用量计量未启用")
		return
	}
	name := r.URL.Query().Get("range")
	if name == "" {
		name = usage.RangeToday
	}
	from, to, ok := s.usage.RangeWindow(name)
	if !ok {
		// 不认识的关键词一律 400，**不静默退回缺省区间**：那会让人以为屏幕上
		// 的数字是自己要的那一段。
		writeError(w, http.StatusBadRequest, "invalid_range", "range 只能是 today、month、7d 或 30d")
		return
	}
	var keyID int64
	if raw := r.URL.Query().Get("key_id"); raw != "" {
		var err error
		keyID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || keyID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_key_id", "key_id 必须为正整数")
			return
		}
	}
	var rep *usage.Report
	var err error
	if keyID == 0 {
		rep, err = s.usage.Report(r.Context(), from, to)
	} else {
		rep, err = s.usage.ReportForKey(r.Context(), from, to, keyID)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, usageResponse{Range: name, KeyID: keyID, Report: *rep})
}
