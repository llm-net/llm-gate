package admin

// 设备状态端点（仅 admin，withSession 默认拒绝已覆盖）：GET /admin/v1/system
// 返回一份 CPU（分大小核）/内存/网络/温度/存储 快照。
//
// 资源约定（与管理 UI 的「零轮询」规则配套）：采集完全按需——页面不开就
// 没有任何采集发生；sysinfo.Collector 自带 2s 结果缓存与上次计数复用，连点
// 「刷新」也只等于一次采样。只读端点，不写审计。

import (
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/sysinfo"
)

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := s.sys.Snapshot(r.Context())
	if err != nil {
		// Snapshot 唯一的错误源是采样窗内 ctx 被取消（客户端已断开）：
		// 响应写不出去，也不值一条 internal 错误日志。
		if r.Context().Err() != nil {
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		System *sysinfo.Snapshot `json:"system"`
		// FirmwareVersion 是固件唯一的版本名称（buildinfo，llmgate --version
		// 的那份），设备状态页的「固件版本」读它。产品里没有第二个版本号可回
		// （docs/firmware-update.md「版本纪律」）。
		FirmwareVersion string `json:"firmwareVersion"`
		// HardwareModel 与登录页、顶栏同源，来自 boardinfo 的本机型号档案。
		HardwareModel string `json:"hardwareModel"`
	}{System: snap, FirmwareVersion: buildinfo.Version, HardwareModel: s.hardwareModel})
}

// handleSystemHistory 返回历史序列：range=6h 取内存分钟层，range=7d 取
// 归档层（跨越整个保留窗口，名字是习惯叫法，实际天数看 retention_days）。
// 缺省 6h；其余值 400。
func (s *Server) handleSystemHistory(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "6h"
	}
	if rng != "6h" && rng != "7d" {
		writeError(w, http.StatusBadRequest, "bad_request", "range 须为 6h 或 7d")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		History sysinfo.HistoryResult `json:"history"`
	}{History: s.rec.History(rng)})
}
