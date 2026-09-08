package admin

// 设备铭牌端点：GET /admin/v1/version（**免会话**，见 middleware.go 的
// requiresSession 白名单）。
//
// 固件只有一个版本名称（形如 2608221732-2d81，见 firmware/Makefile 与
// internal/buildinfo）；设备型号来自 boardinfo 的本机型号档案。免会话是因为
// 登录页的规格铭牌要印这两项——登录页在会话之前，取不到其他管理读数。
//
// 型号只是公开的兼容代号，不是设备身份；序列号、SID、MAC、口令状态一概不在
// 这里。只读端点，不写审计。

import (
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Version       string `json:"version"`
		HardwareModel string `json:"hardware_model"`
	}{Version: buildinfo.Version, HardwareModel: s.hardwareModel})
}
