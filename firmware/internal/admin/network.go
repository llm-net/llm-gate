package admin

// 设备设置——网卡配置端点（仅 admin，withSession 默认拒绝已覆盖）：
//   GET  /admin/v1/system/network          读物理网卡与连接的 IPv4 配置
//   PUT  /admin/v1/system/network          提交修改（已连接：延迟应用 + 确认
//                                          窗口；未连接：直接写配置文件）
//   POST /admin/v1/system/network/confirm  确认保留（写入配置文件）
//
// 状态机、时序与回滚语义都在 internal/netconfig（先改运行时、确认才落盘、
// 逾期重放配置文件回滚；未连接网卡走离线直写旁路）；本文件只做入参解码、
// 错误映射、审计，以及在读响应上标注「当前入口」网卡。资源约定与设备状态
// 页同款：读端点按需执行几条 nmcli（毫秒级），无轮询无缓存。
//
// 审计：提交与确认在此处记（带 RemoteIP）；应用/回滚发生在定时器里，由
// netconfig 经 gatewayd 注入的回调补记（归属到提交人，无 RemoteIP 可记）。
// detail 只含网卡名、连接名与 IP 参数摘要——无凭证物料（§15.1）。

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/llm-net/llm-gate/firmware/internal/netconfig"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名（system.* 与设备状态同域：主体是这台设备本身）。
const (
	EventNetworkUpdate  = "system.network_update"
	EventNetworkConfirm = "system.network_confirm"
)

func (s *Server) handleNetworkStatus(w http.ResponseWriter, r *http.Request) {
	st := s.net.Status(r.Context())
	// 标注「当前入口」：请求经由的监听侧本地地址落在哪块网卡上。经回环隧道
	// 或反代进来命不中，界面按入口未知保守处理。
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if tcp, ok := la.(*net.TCPAddr); ok {
			if a, ok := netip.AddrFromSlice(tcp.IP); ok {
				netconfig.MarkEntry(st.Interfaces, a)
			}
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Network *netconfig.Status `json:"network"`
	}{Network: st})
}

// networkChangeReq 是 PUT 的请求体。auto（DHCP）时静态字段必须留空。
type networkChangeReq struct {
	Device  string   `json:"device"`
	Method  string   `json:"method"`
	Address string   `json:"address"`
	Prefix  int      `json:"prefix"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

func (s *Server) handleNetworkUpdate(w http.ResponseWriter, r *http.Request) {
	var req networkChangeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.net.Submit(r.Context(), netconfig.Change{
		Device:  req.Device,
		Method:  req.Method,
		Address: req.Address,
		Prefix:  req.Prefix,
		Gateway: req.Gateway,
		DNS:     req.DNS,
	})
	if err != nil {
		s.writeNetworkError(w, r, err)
		return
	}
	var detail string
	if p := out.Persisted; p != nil {
		detail = fmt.Sprintf("%s（%s）：%s → %s（未连接，已直接写入配置文件）",
			p.Device, p.ConnectionID, p.Old.Summary(), p.New.Summary())
	} else {
		p := out.Pending
		detail = fmt.Sprintf("%s（%s）：%s → %s", p.Device, p.ConnectionID,
			p.Old.Summary(), p.New.Summary())
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventNetworkUpdate,
		Entity:   "system:network",
		Detail:   detail,
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNetworkConfirm(w http.ResponseWriter, r *http.Request) {
	// 空 body 也要按统一约定解码（拒绝未知字段、消费掉 body）。
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	confirmed, err := s.net.Confirm()
	if err != nil {
		s.writeNetworkError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventNetworkConfirm,
		Entity: "system:network",
		Detail: fmt.Sprintf("%s（%s）：确认保留 %s", confirmed.Device, confirmed.ConnectionID,
			confirmed.New.Summary()),
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// writeNetworkError 把 netconfig 的哨兵/校验错误映射为统一错误体。
func (s *Server) writeNetworkError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *netconfig.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, "invalid_network_config", verr.Error())
	case errors.Is(err, netconfig.ErrUnsupported):
		writeError(w, http.StatusConflict, "network_unsupported",
			"此运行环境不支持网络配置（仅在设备上可用）")
	case errors.Is(err, netconfig.ErrChangePending):
		writeError(w, http.StatusConflict, "network_change_pending",
			"已有一项网络变更进行中：请先确认保留，或等待其自动回滚后再试")
	case errors.Is(err, netconfig.ErrUnknownDevice):
		writeError(w, http.StatusNotFound, "device_not_found", "网卡不存在")
	case errors.Is(err, netconfig.ErrNotEditable):
		writeError(w, http.StatusConflict, "interface_not_editable",
			"该网卡没有可修改的连接配置，接入网络后再试")
	case errors.Is(err, netconfig.ErrOfflineWriteFailed):
		writeError(w, http.StatusInternalServerError, "offline_write_failed",
			"写入连接配置文件失败，请重试")
	case errors.Is(err, netconfig.ErrNoPending):
		writeError(w, http.StatusConflict, "no_pending_change",
			"没有待确认的网络变更（可能已确认、已回滚或服务已重启）")
	case errors.Is(err, netconfig.ErrNotApplied):
		writeError(w, http.StatusConflict, "not_applied_yet",
			"新配置尚未应用完成，请稍候几秒再确认")
	case errors.Is(err, netconfig.ErrPersistFailed):
		writeError(w, http.StatusInternalServerError, "persist_failed",
			"新配置已生效但写入配置文件失败，请重试确认；逾期未确认将自动回滚")
	default:
		s.internalError(w, r, err)
	}
}
