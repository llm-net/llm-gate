// Package netconfig 实现「设备设置」页的网络配置查看与修改：exec nmcli 走
// NetworkManager D-Bus 读物理网卡与其活动连接的 IPv4 配置，并按一台无屏
// 设备该有的方式改 IP——配错地址等于把设备丢在客户内网里找不回来，所以：
//
//  1. 提交后先延迟约 2 秒再动网（让 HTTP 响应先离开设备）；
//  2. 应用走 `nmcli device modify`（Device.Reapply）：只改**运行时**，
//     不写连接配置文件，Wi-Fi 关联不掉线，地址原位切换；
//  3. 管理员在新地址登录并「确认保留」后，才 `nmcli connection modify`
//     把新配置写入配置文件（持久化）；
//  4. 确认窗口内没等到确认，就 `nmcli device reapply` 重放**没动过的**
//     配置文件自动回滚。窗口内断电重启同样回到旧配置——持久层从未变过。
//
// 以上确认状态机只保护**已连接网卡**的运行时修改。未连接的网卡（未插线、
// 未关联）走旁路：直接 `nmcli connection modify` 写配置文件——不动运行时、
// 现网无感、下次接入时生效，因此不需要延迟与确认窗口。写入目标是该卡的
// 候选 profile（connection.interface-name 精确匹配优先于未绑定网卡的通配
// profile，再比 autoconnect-priority），读接口把它放在 Connection 字段，
// 界面能看到将写入哪个连接。
//
// 权限前提：gatewayd 以服务用户 llmgate 运行且 NoNewPrivileges=yes（不能
// sudo），修改类 D-Bus 调用靠板上 polkit 本地授权放行（deploy/
// 50-llmgate-network.pkla：settings.modify.system + network-control；读取本就
// 无需授权）。运行环境没有 nmcli / NetworkManager 时整体报告「不支持」，
// 由界面降级展示（x86-64 云主机多用 netplan / cloud-init，与 darwin 开发机、CI 同走这条路）。
//
// 待确认状态只在内存里：gatewayd 重启丢弃 pending 与定时器，此时运行时
// 配置保持已应用的新值直到下一次网卡重激活/重启回落到配置文件。接受这个
// 边角（窗口只有几分钟，与部署重启相撞的概率可忽略；两种落点都是管理员
// 明确提交过的配置）。
//
// §15.1 纪律：绝不带 --show-secrets 调 nmcli，Wi-Fi PSK 等秘密永不进程内；
// nmcli 的 stderr 只含设备/连接名与英文错误短语，无凭证，截断后可入日志
// 与审计 detail。
package netconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 状态机时序（导出到 GET 响应，界面文案与倒计时以服务端值为准）。
const (
	// DefaultApplyDelay 提交到动网的延迟：让 PUT 响应先写回客户端。
	DefaultApplyDelay = 2 * time.Second
	// DefaultConfirmWindow 应用成功到自动回滚的确认窗口：够管理员在新
	// 地址重新登录并点确认，又不至于让一次误配把设备晾太久。
	DefaultConfirmWindow = 3 * time.Minute
)

// 每类 nmcli 调用的兜底超时（正常都在百毫秒级返回）。
const (
	readTimeout    = 10 * time.Second
	applyTimeout   = 60 * time.Second // device modify：DHCP 取址可能要几秒
	persistTimeout = 20 * time.Second
	revertTimeout  = 60 * time.Second
)

// Pending.Phase 的取值。
const (
	PhaseScheduled       = "scheduled"        // 已提交，尚未动网
	PhaseAwaitingConfirm = "awaiting_confirm" // 运行时已生效，等确认或回滚
	phaseConfirming      = "confirming"       // 确认落盘进行中（内部态）
)

// LastResult.Event 的取值。
const (
	ResultConfirmed    = "confirmed"     // 已确认并写入配置文件
	ResultRolledBack   = "rolled_back"   // 窗口超时，已重放配置文件回滚
	ResultApplyFailed  = "apply_failed"  // 运行时应用失败，配置未变
	ResultRevertFailed = "revert_failed" // 回滚也失败（等 NM 自愈或人工介入）
)

// 哨兵错误：由管理面 handler 映射为统一错误体。
var (
	ErrUnsupported        = errors.New("netconfig: 运行环境不支持网络配置")
	ErrChangePending      = errors.New("netconfig: 已有变更进行中")
	ErrUnknownDevice      = errors.New("netconfig: 网卡不存在")
	ErrNotEditable        = errors.New("netconfig: 网卡当前不可配置")
	ErrNoPending          = errors.New("netconfig: 没有待确认的变更")
	ErrNotApplied         = errors.New("netconfig: 变更尚未应用")
	ErrPersistFailed      = errors.New("netconfig: 写入配置文件失败")
	ErrOfflineWriteFailed = errors.New("netconfig: 离线写入连接配置文件失败")
)

// ValidationError 是入参校验错误；Error() 即 zh-CN 展示文案。
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func vErr(format string, a ...any) *ValidationError {
	return &ValidationError{msg: fmt.Sprintf(format, a...)}
}

// IPv4Config 一份 IPv4 配置。连接（配置文件）视角与运行时视角共用：运行时
// 视角没有 method（omitempty 缺省）。
type IPv4Config struct {
	Method    string   `json:"method,omitempty"` // manual | auto | 其他 NM 值原样
	Addresses []string `json:"addresses,omitempty"`
	Gateway   string   `json:"gateway,omitempty"`
	DNS       []string `json:"dns,omitempty"`
}

// Summary 是审计 detail 与界面提示用的单行摘要（无敏感值）。
func (c IPv4Config) Summary() string {
	if c.Method == "auto" {
		return "auto (DHCP)"
	}
	parts := []string{}
	if c.Method != "" && c.Method != "manual" {
		parts = append(parts, c.Method)
	}
	if len(c.Addresses) > 0 {
		parts = append(parts, strings.Join(c.Addresses, "+"))
	}
	if c.Gateway != "" {
		parts = append(parts, "gw "+c.Gateway)
	}
	if len(c.DNS) > 0 {
		parts = append(parts, "dns "+strings.Join(c.DNS, ","))
	}
	if len(parts) == 0 {
		return "（空）"
	}
	return strings.Join(parts, " ")
}

// Connection 网卡当前绑定的 NM 连接（配置文件视角）。
type Connection struct {
	ID   string     `json:"id"`
	UUID string     `json:"uuid"`
	IPv4 IPv4Config `json:"ipv4"`
}

// Interface 一块物理网卡的读数。
type Interface struct {
	Device       string `json:"device"`
	Type         string `json:"type"` // wifi | ethernet
	MAC          string `json:"mac,omitempty"`
	State        string `json:"state"` // NM 状态词原样（connected/unavailable/…）
	Connected    bool   `json:"connected"`
	DefaultRoute bool   `json:"default_route,omitempty"` // 运行时持有 0.0.0.0/0（多卡在线时的默认出口）
	Entry        bool   `json:"entry,omitempty"`         // 本次管理台请求经由的网卡（MarkEntry 标注）
	// Connection 是修改将写入的连接：已连接时为活动连接；未连接时为离线直写
	// 的候选 profile（选择规则见包注释）。为空表示这块卡没有可修改的连接。
	Connection *Connection `json:"connection,omitempty"`
	Runtime    *IPv4Config `json:"runtime,omitempty"` // 已连接时的实际生效值
}

// Change 一次修改请求（handler 解码后的入参）。
type Change struct {
	Device  string
	Method  string // manual | auto
	Address string // manual：点分 IPv4
	Prefix  int    // manual：8–30
	Gateway string // manual 可空
	DNS     []string
}

// Outcome 一次 Submit 的走向，两个字段恰好一个非空：已连接网卡走确认状态机
// （Pending）；未连接网卡直接写入连接配置文件（Persisted）。
type Outcome struct {
	Pending   *Pending   `json:"pending,omitempty"`
	Persisted *Persisted `json:"persisted,omitempty"`
}

// Persisted 一次离线直写的结果：已落盘，接入时生效，无确认窗口。
type Persisted struct {
	Device       string     `json:"device"`
	ConnectionID string     `json:"connection_id"`
	Old          IPv4Config `json:"old"`
	New          IPv4Config `json:"new"`
}

// Pending 一次进行中的变更；ConnectionUUID 不下发。
type Pending struct {
	Device         string     `json:"device"`
	ConnectionID   string     `json:"connection_id"`
	ConnectionUUID string     `json:"-"`
	Phase          string     `json:"phase"`
	Old            IPv4Config `json:"old"`
	New            IPv4Config `json:"new"`
	AppliesAt      time.Time  `json:"applies_at"`
	Deadline       time.Time  `json:"confirm_deadline,omitzero"` // 应用成功后才有
}

// LastResult 上一次变更的终局（内存态，重启即失；只为界面把「后来发生了
// 什么」讲完——回滚与失败若不留痕，管理员回到旧地址只会看到配置无事发生）。
type LastResult struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"`
	OK     bool      `json:"ok"`
	Device string    `json:"device"`
	Detail string    `json:"detail" i18n:"text"`
}

// Status 是 GET /admin/v1/system/network 的主体。
type Status struct {
	Supported            bool        `json:"supported"`
	Reason               string      `json:"reason,omitempty" i18n:"text"` // supported=false 的说明
	Interfaces           []Interface `json:"interfaces,omitempty"`
	Pending              *Pending    `json:"pending,omitempty"`
	Last                 *LastResult `json:"last,omitempty"`
	ConfirmWindowSeconds int         `json:"confirm_window_seconds"`
	ApplyDelaySeconds    int         `json:"apply_delay_seconds"`
}

// Runner 执行一次 nmcli；测试注入脚本化实现。返回 stdout/stderr 原文。
type Runner func(ctx context.Context, args ...string) (stdout, stderr string, err error)

// AuditFn 接收异步阶段（应用/回滚）的审计事件；由 gatewayd 接到 store。
type AuditFn func(event, detail string)

// Manager 持有状态机；并发安全。一台设备同一时刻只允许一项网络变更。
type Manager struct {
	log           *slog.Logger
	run           Runner
	audit         AuditFn
	applyDelay    time.Duration
	confirmWindow time.Duration

	mu            sync.Mutex
	gen           int64 // 每次 Submit 递增；定时器回调据此丢弃过期工作
	pending       *Pending
	last          *LastResult
	rollbackTimer *time.Timer
}

// Option 调整 Manager 构造（测试注入 Runner 与时序）。
type Option func(*Manager)

// WithRunner 替换 nmcli 执行器。
func WithRunner(r Runner) Option { return func(m *Manager) { m.run = r } }

// WithTimings 替换应用延迟与确认窗口。
func WithTimings(applyDelay, confirmWindow time.Duration) Option {
	return func(m *Manager) {
		m.applyDelay = applyDelay
		m.confirmWindow = confirmWindow
	}
}

// NewManager 构造网络配置管理器。audit 为 nil 时异步审计静默丢弃（测试）。
func NewManager(logger *slog.Logger, audit AuditFn, opts ...Option) *Manager {
	m := &Manager{
		log:           logger.With("mod", "netconfig"),
		run:           execNmcli,
		audit:         audit,
		applyDelay:    DefaultApplyDelay,
		confirmWindow: DefaultConfirmWindow,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// execNmcli 是缺省 Runner。LC_ALL=C 固定输出词形（终态词与错误短语不随
// 系统语言变），terse 值解析才可靠。
func execNmcli(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (m *Manager) runCmd(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.run(ctx, args...)
}

// ---- 读 ----

// Status 汇总当前网络读数与状态机快照。读失败不报错：降级为
// supported=false + 原因，界面照常渲染。
func (m *Manager) Status(ctx context.Context) *Status {
	st := &Status{
		ConfirmWindowSeconds: int(m.confirmWindow / time.Second),
		ApplyDelaySeconds:    int(m.applyDelay / time.Second),
	}
	ifaces, err := m.readInterfaces(ctx)
	if err != nil {
		st.Reason = unsupportedReason(err)
	} else {
		st.Supported = true
		st.Interfaces = ifaces
	}
	m.mu.Lock()
	if m.pending != nil {
		cp := *m.pending
		// 内部态不外露：落盘进行中在界面上仍是「待确认」。
		if cp.Phase == phaseConfirming {
			cp.Phase = PhaseAwaitingConfirm
		}
		st.Pending = &cp
	}
	if m.last != nil {
		cp := *m.last
		st.Last = &cp
	}
	m.mu.Unlock()
	return st
}

// devRow 是 `nmcli device status` 的一行。
type devRow struct {
	device, typ, state, conUUID, conName string
}

func (m *Manager) readInterfaces(ctx context.Context) ([]Interface, error) {
	out, stderr, err := m.runCmd(ctx, readTimeout,
		"-t", "-f", "DEVICE,TYPE,STATE,CON-UUID,CONNECTION", "device", "status")
	if err != nil {
		return nil, cmdError("device status", err, stderr)
	}
	var ifaces []Interface
	// 非活动 profile 列表按需拉一次，多块未连接网卡共享。
	var profiles []profileInfo
	profilesLoaded := false
	for _, row := range parseDeviceStatus(out) {
		if row.typ != "wifi" && row.typ != "ethernet" {
			continue
		}
		iface := Interface{
			Device:    row.device,
			Type:      row.typ,
			State:     row.state,
			Connected: row.state == "connected",
		}
		// 每块网卡的读数尽力而为：单卡读失败只缺那一节，不拖垮整页。
		if kv, err := m.showKV(ctx, "device", "show", row.device,
			"GENERAL.HWADDR,IP4.ADDRESS,IP4.GATEWAY,IP4.DNS,IP4.ROUTE"); err == nil {
			iface.MAC = firstVal(kv["GENERAL.HWADDR"])
			if iface.Connected {
				iface.Runtime = &IPv4Config{
					Addresses: kv["IP4.ADDRESS"],
					Gateway:   firstVal(kv["IP4.GATEWAY"]),
					DNS:       kv["IP4.DNS"],
				}
				iface.DefaultRoute = hasDefaultRoute(kv["IP4.ROUTE"])
			}
		} else {
			m.log.Warn("读网卡状态失败", "device", row.device, "error", err.Error())
		}
		switch {
		case row.conUUID != "":
			if conn, err := m.readConnection(ctx, row.conUUID); err == nil {
				iface.Connection = conn
			} else {
				m.log.Warn("读连接配置失败", "device", row.device, "error", err.Error())
			}
		case !iface.Connected:
			// 未连接且无活动连接：找离线直写的候选 profile。
			if !profilesLoaded {
				profilesLoaded = true
				var perr error
				if profiles, perr = m.readInactiveProfiles(ctx); perr != nil {
					m.log.Warn("读连接列表失败", "error", perr.Error())
				}
			}
			iface.Connection = pickCandidate(profiles, row.device, row.typ)
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces, nil
}

// hasDefaultRoute 报告设备的 IP4.ROUTE 读数里是否有默认路由。行形如
// "dst = 0.0.0.0/0, nh = 192.168.50.1, mt = 100"。
func hasDefaultRoute(routes []string) bool {
	for _, r := range routes {
		if strings.HasPrefix(r, "dst = 0.0.0.0/0") {
			return true
		}
	}
	return false
}

func (m *Manager) readConnection(ctx context.Context, uuid string) (*Connection, error) {
	kv, err := m.showKV(ctx, "connection", "show", "uuid", uuid,
		"connection.id,ipv4.method,ipv4.addresses,ipv4.gateway,ipv4.dns")
	if err != nil {
		return nil, err
	}
	return &Connection{
		ID:   firstVal(kv["connection.id"]),
		UUID: uuid,
		IPv4: IPv4Config{
			Method:    firstVal(kv["ipv4.method"]),
			Addresses: splitListVal(firstVal(kv["ipv4.addresses"])),
			Gateway:   firstVal(kv["ipv4.gateway"]),
			DNS:       splitListVal(firstVal(kv["ipv4.dns"])),
		},
	}, nil
}

// profileInfo 一个非活动连接 profile，附选候选所需的元数据。
type profileInfo struct {
	conn      Connection
	nmType    string // 802-3-ethernet | 802-11-wireless
	ifaceName string // connection.interface-name（空 = 不绑定网卡的通配）
	priority  int    // connection.autoconnect-priority
}

// readInactiveProfiles 拉取全部非活动的有线/无线连接 profile。单个 profile
// 读失败只丢那一个，不拖垮整页。
func (m *Manager) readInactiveProfiles(ctx context.Context) ([]profileInfo, error) {
	out, stderr, err := m.runCmd(ctx, readTimeout, "-t", "-f", "UUID,TYPE,ACTIVE", "connection", "show")
	if err != nil {
		return nil, cmdError("connection show", err, stderr)
	}
	var profiles []profileInfo
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 3 {
			continue
		}
		uuid, typ, active := f[0], f[1], f[2]
		if active == "yes" || (typ != "802-3-ethernet" && typ != "802-11-wireless") {
			continue
		}
		kv, err := m.showKV(ctx, "connection", "show", "uuid", uuid,
			"connection.id,connection.interface-name,connection.autoconnect-priority,"+
				"ipv4.method,ipv4.addresses,ipv4.gateway,ipv4.dns")
		if err != nil {
			m.log.Warn("读连接配置失败", "uuid", uuid, "error", err.Error())
			continue
		}
		prio, _ := strconv.Atoi(firstVal(kv["connection.autoconnect-priority"]))
		profiles = append(profiles, profileInfo{
			conn: Connection{
				ID:   firstVal(kv["connection.id"]),
				UUID: uuid,
				IPv4: IPv4Config{
					Method:    firstVal(kv["ipv4.method"]),
					Addresses: splitListVal(firstVal(kv["ipv4.addresses"])),
					Gateway:   firstVal(kv["ipv4.gateway"]),
					DNS:       splitListVal(firstVal(kv["ipv4.dns"])),
				},
			},
			nmType:    typ,
			ifaceName: firstVal(kv["connection.interface-name"]),
			priority:  prio,
		})
	}
	return profiles, nil
}

// nmConnType 设备类型 → 连接列表 TYPE 列的 NM 连接类型。
func nmConnType(devType string) string {
	if devType == "wifi" {
		return "802-11-wireless"
	}
	return "802-3-ethernet"
}

// pickCandidate 为未连接网卡选离线直写的目标 profile：类型匹配且绑定本卡或
// 通配；interface-name 精确匹配优先于通配，再比 autoconnect-priority（高者
// 先），同分按连接名钉死。结果要可预期：界面上会显示将写入哪个连接。
func pickCandidate(profiles []profileInfo, device, devType string) *Connection {
	want := nmConnType(devType)
	var best *profileInfo
	better := func(p, q *profileInfo) bool {
		pe, qe := p.ifaceName == device, q.ifaceName == device
		if pe != qe {
			return pe
		}
		if p.priority != q.priority {
			return p.priority > q.priority
		}
		return p.conn.ID < q.conn.ID
	}
	for i := range profiles {
		p := &profiles[i]
		if p.nmType != want || (p.ifaceName != "" && p.ifaceName != device) {
			continue
		}
		if best == nil || better(p, best) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	cp := best.conn
	return &cp
}

// MarkEntry 标注「当前入口」：管理台请求经由的本地地址与哪块已连接网卡的
// 运行时地址相等，就标哪块。经回环/隧道/反代进来时无命中，界面按入口未知
// 保守处理（保留强警告）。
func MarkEntry(ifaces []Interface, local netip.Addr) {
	local = local.Unmap()
	for i := range ifaces {
		rt := ifaces[i].Runtime
		if rt == nil {
			continue
		}
		for _, cidr := range rt.Addresses {
			ipStr, _, _ := strings.Cut(cidr, "/")
			if a, err := netip.ParseAddr(ipStr); err == nil && a.Unmap() == local {
				ifaces[i].Entry = true
				break
			}
		}
	}
}

// showKV 跑一条 `nmcli -t -f <fields> <verb…>` 并解析成 键→值列表。
// fields 放参数末尾是为了让调用点读起来先动词后字段。
func (m *Manager) showKV(ctx context.Context, verb, sub string, rest ...string) (map[string][]string, error) {
	fields := rest[len(rest)-1]
	target := rest[:len(rest)-1]
	args := append([]string{"-t", "-f", fields, verb, sub}, target...)
	out, stderr, err := m.runCmd(ctx, readTimeout, args...)
	if err != nil {
		return nil, cmdError(verb+" "+sub, err, stderr)
	}
	return parseShowKV(out), nil
}

// ---- 改 ----

// Submit 校验一次变更并按目标网卡状态分流：已连接网卡登记 Pending（applyDelay
// 后由定时器动网，走确认状态机）；未连接网卡直接写配置文件（Persisted）。
// 返回的快照含变更前配置，handler 据此写审计 detail。
func (m *Manager) Submit(ctx context.Context, ch Change) (*Outcome, error) {
	next, verr := validateChange(&ch)
	if verr != nil {
		return nil, verr
	}
	m.mu.Lock()
	if m.pending != nil {
		m.mu.Unlock()
		return nil, ErrChangePending
	}
	m.mu.Unlock()

	// NM 侧核对（不持锁：读要跑几条子进程）。
	ifaces, err := m.readInterfaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w：%s", ErrUnsupported, unsupportedReason(err))
	}
	var target *Interface
	for i := range ifaces {
		if ifaces[i].Device == ch.Device {
			target = &ifaces[i]
			break
		}
	}
	if target == nil {
		return nil, ErrUnknownDevice
	}
	if target.Connection == nil {
		return nil, ErrNotEditable
	}
	if !target.Connected {
		return m.persistOffline(target, next)
	}

	p := &Pending{
		Device:         ch.Device,
		ConnectionID:   target.Connection.ID,
		ConnectionUUID: target.Connection.UUID,
		Phase:          PhaseScheduled,
		Old:            target.Connection.IPv4,
		New:            *next,
		AppliesAt:      time.Now().Add(m.applyDelay),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil { // 双检：读 NM 期间别的请求先登记了
		return nil, ErrChangePending
	}
	m.gen++
	gen := m.gen
	m.pending = p
	m.last = nil // 新一轮开始，上一轮的终局播报退场
	time.AfterFunc(m.applyDelay, func() { m.applyPending(gen) })
	m.log.Info("网络变更已登记",
		"device", p.Device, "connection", p.ConnectionID,
		"old", p.Old.Summary(), "new", p.New.Summary())
	cp := *p
	return &Outcome{Pending: &cp}, nil
}

// persistOffline 未连接网卡的旁路：对候选 profile 直接 connection modify
// 落盘。不进 pending 状态机——不动运行时、现网无感，接入时生效，断电也不丢。
func (m *Manager) persistOffline(target *Interface, next *IPv4Config) (*Outcome, error) {
	args := append([]string{"connection", "modify", "uuid", target.Connection.UUID}, propertyArgs(*next)...)
	// 与 Confirm 同理用背景 ctx：请求中途断开不该中止落盘。
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	_, stderr, err := m.run(ctx, args...)
	cancel()
	if err != nil {
		reason := shortReason(err, stderr)
		m.log.Error("离线网卡配置写入失败",
			"device", target.Device, "connection", target.Connection.ID, "error", reason)
		return nil, fmt.Errorf("%w：%s", ErrOfflineWriteFailed, reason)
	}
	m.log.Info("离线网卡配置已写入配置文件",
		"device", target.Device, "connection", target.Connection.ID, "new", next.Summary())
	return &Outcome{Persisted: &Persisted{
		Device:       target.Device,
		ConnectionID: target.Connection.ID,
		Old:          target.Connection.IPv4,
		New:          *next,
	}}, nil
}

// applyPending 定时器回调：把 pending 的新配置应用到运行时。
func (m *Manager) applyPending(gen int64) {
	m.mu.Lock()
	p := m.pending
	if p == nil || m.gen != gen || p.Phase != PhaseScheduled {
		m.mu.Unlock()
		return
	}
	args := append([]string{"device", "modify", p.Device}, propertyArgs(p.New)...)
	m.mu.Unlock()

	// 定时器脱离了请求生命周期，用背景 ctx + 兜底超时。
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	_, stderr, err := m.run(ctx, args...)
	cancel()

	m.mu.Lock()
	if m.pending != p || m.gen != gen {
		m.mu.Unlock()
		return
	}
	if err != nil {
		// device modify 原子失败：运行时未变，本轮直接终局。
		detail := fmt.Sprintf("%s %s 应用失败：%s", p.Device, p.New.Summary(), shortReason(err, stderr))
		m.pending = nil
		m.last = &LastResult{At: time.Now(), Event: ResultApplyFailed, OK: false, Device: p.Device, Detail: detail}
		m.mu.Unlock()
		m.log.Error("网络配置应用失败", "device", p.Device, "error", shortReason(err, stderr))
		m.auditEvent("system.network_apply", detail)
		return
	}
	p.Phase = PhaseAwaitingConfirm
	p.Deadline = time.Now().Add(m.confirmWindow)
	m.rollbackTimer = time.AfterFunc(m.confirmWindow, func() { m.rollbackPending(gen) })
	m.mu.Unlock()
	m.log.Info("网络配置已应用（运行时，待确认）",
		"device", p.Device, "new", p.New.Summary(), "window", m.confirmWindow.String())
	m.auditEvent("system.network_apply",
		fmt.Sprintf("%s %s → %s 已生效，待确认", p.Device, p.Old.Summary(), p.New.Summary()))
}

// rollbackPending 确认窗口超时回调：重放未改动的配置文件。
func (m *Manager) rollbackPending(gen int64) {
	m.mu.Lock()
	p := m.pending
	if p == nil || m.gen != gen || p.Phase != PhaseAwaitingConfirm {
		m.mu.Unlock()
		return
	}
	device, uuid := p.Device, p.ConnectionUUID
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), revertTimeout)
	_, stderr, err := m.run(ctx, "device", "reapply", device)
	cancel()
	if err != nil {
		// reapply 失败（比如应用的新配置把网卡带进了坏状态）：整连接重激活。
		ctx2, cancel2 := context.WithTimeout(context.Background(), revertTimeout)
		_, stderr2, err2 := m.run(ctx2, "-w", "30", "connection", "up", "uuid", uuid)
		cancel2()
		if err2 != nil {
			stderr = stderr + " / " + stderr2
			err = err2
		} else {
			err = nil
		}
	}

	m.mu.Lock()
	if m.pending != p || m.gen != gen {
		m.mu.Unlock()
		return
	}
	m.pending = nil
	var res LastResult
	if err != nil {
		// 回滚也失败：不再重试（连接配置文件没动过，NM 的 autoconnect 会
		// 在网卡下次掉线时用旧配置自愈）。
		res = LastResult{At: time.Now(), Event: ResultRevertFailed, OK: false, Device: device,
			Detail: fmt.Sprintf("%s 确认超时，且回滚失败：%s", device, shortReason(err, stderr))}
	} else {
		res = LastResult{At: time.Now(), Event: ResultRolledBack, OK: true, Device: device,
			Detail: fmt.Sprintf("%s 确认超时，已回滚为 %s", device, p.Old.Summary())}
	}
	m.last = &res
	m.mu.Unlock()
	if err != nil {
		m.log.Error("网络配置回滚失败", "device", device, "error", shortReason(err, stderr))
	} else {
		m.log.Info("网络配置确认超时，已回滚", "device", device, "old", p.Old.Summary())
	}
	m.auditEvent("system.network_rollback", res.Detail)
}

// Confirm 把已生效的新配置写入连接配置文件并结束本轮。失败时保持待确认
// 状态与回滚定时器（重新武装到原 deadline），调用方可重试。
func (m *Manager) Confirm() (*Pending, error) {
	m.mu.Lock()
	p := m.pending
	if p == nil {
		m.mu.Unlock()
		return nil, ErrNoPending
	}
	if p.Phase == PhaseScheduled {
		m.mu.Unlock()
		return nil, ErrNotApplied
	}
	if p.Phase == phaseConfirming {
		m.mu.Unlock()
		return nil, ErrNotApplied // 并发确认：前一次落盘还在路上
	}
	// 先解除回滚定时器再落盘；Stop 失败 = 回滚回调已经跑起来了，本轮已定局。
	if m.rollbackTimer != nil && !m.rollbackTimer.Stop() {
		m.mu.Unlock()
		return nil, ErrNoPending
	}
	p.Phase = phaseConfirming
	gen := m.gen
	args := append([]string{"connection", "modify", "uuid", p.ConnectionUUID}, propertyArgs(p.New)...)
	m.mu.Unlock()

	// 落盘用背景 ctx：请求中途断开不该留下「运行时新、文件旧」的半台账。
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	_, stderr, err := m.run(ctx, args...)
	cancel()

	m.mu.Lock()
	if m.pending != p || m.gen != gen {
		m.mu.Unlock()
		return nil, ErrNoPending
	}
	if err != nil {
		p.Phase = PhaseAwaitingConfirm
		// 给重试留口气，别落盘一失败立刻回滚；但宽限不超过一个确认窗口
		//（也让毫秒级窗口的测试时序成立）。
		remain := time.Until(p.Deadline)
		if grace := min(5*time.Second, m.confirmWindow); remain < grace {
			remain = grace
		}
		m.rollbackTimer = time.AfterFunc(remain, func() { m.rollbackPending(gen) })
		m.mu.Unlock()
		reason := shortReason(err, stderr)
		m.log.Error("网络配置写入配置文件失败", "device", p.Device, "error", reason)
		return nil, fmt.Errorf("%w：%s", ErrPersistFailed, reason)
	}
	m.pending = nil
	m.rollbackTimer = nil
	m.last = &LastResult{At: time.Now(), Event: ResultConfirmed, OK: true, Device: p.Device,
		Detail: fmt.Sprintf("%s %s → %s 已确认并写入配置文件", p.Device, p.Old.Summary(), p.New.Summary())}
	cp := *p
	m.mu.Unlock()
	m.log.Info("网络配置已确认持久化", "device", cp.Device, "new", cp.New.Summary())
	return &cp, nil
}

func (m *Manager) auditEvent(event, detail string) {
	if m.audit != nil {
		m.audit(event, detail)
	}
}

// ---- nmcli 参数与输出 ----

// propertyArgs 生成 device modify / connection modify 共用的属性参数。
// auto 时清空三项静态值（空串 = 清除）。只动 ipv4.*，IPv6 与 Wi-Fi 段一概
// 不碰。
func propertyArgs(cfg IPv4Config) []string {
	if cfg.Method == "auto" {
		return []string{"ipv4.method", "auto", "ipv4.addresses", "", "ipv4.gateway", "", "ipv4.dns", ""}
	}
	return []string{
		"ipv4.method", "manual",
		"ipv4.addresses", strings.Join(cfg.Addresses, ","),
		"ipv4.gateway", cfg.Gateway,
		"ipv4.dns", strings.Join(cfg.DNS, ","),
	}
}

// parseDeviceStatus 解析 `nmcli -t -f DEVICE,TYPE,STATE,CON-UUID,CONNECTION
// device status`。列数不足的行丢弃（防御：nmcli 版本差异）。
func parseDeviceStatus(out string) []devRow {
	var rows []devRow
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 5 {
			continue
		}
		rows = append(rows, devRow{device: f[0], typ: f[1], state: f[2], conUUID: f[3], conName: f[4]})
	}
	return rows
}

// parseShowKV 解析 `nmcli -t -f … <show>` 的 键:值 行。列表键（如
// IP4.ADDRESS[1]）剥掉下标聚成切片；值为空或 "--"（未设置）跳过。
func parseShowKV(out string) map[string][]string {
	kv := make(map[string][]string)
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 2 {
			continue
		}
		key := f[0]
		if i := strings.IndexByte(key, '['); i > 0 {
			key = key[:i]
		}
		// 值本身含冒号时（如 HWADDR）会被切开，拼回去。
		val := strings.Join(f[1:], ":")
		if val == "" || val == "--" {
			continue
		}
		kv[key] = append(kv[key], val)
	}
	return kv
}

// splitTerse 按 nmcli terse 规则切一行：`:` 分列，`\:` 与 `\\` 是转义。
func splitTerse(line string) []string {
	var fields []string
	var b strings.Builder
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ':':
			fields = append(fields, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	fields = append(fields, b.String())
	return fields
}

// splitListVal 把连接属性里的列表值（"10.0.0.1/24, 10.0.0.2/24" 或逗号无
// 空格）切成切片；空值与 "--" 给空。
func splitListVal(v string) []string {
	if v == "" || v == "--" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstVal(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	v := vs[0]
	if v == "--" {
		return ""
	}
	return v
}

func cmdError(op string, err error, stderr string) error {
	return fmt.Errorf("nmcli %s: %s", op, shortReason(err, stderr))
}

// shortReason 取 stderr 首个非空行（否则 err 文本），截到 200 字符。nmcli
// 错误只含设备/连接名与英文短语，无凭证（§15.1 前提，见包注释）。
func shortReason(err error, stderr string) string {
	msg := ""
	for _, line := range strings.Split(stderr, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			msg = l
			break
		}
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return msg
}

// unsupportedReason 把「环境不支持」的底因转成可展示说明。
func unsupportedReason(err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "此运行环境没有 NetworkManager（nmcli 不存在）；云主机与不用 NetworkManager 的系统请用系统自带的网络工具配置地址"
	}
	return "无法读取网络状态：" + err.Error()
}

// ---- 校验 ----

// validateChange 做纯语法/语义校验并归一出目标配置；不触网。
func validateChange(ch *Change) (*IPv4Config, *ValidationError) {
	if ch.Device == "" {
		return nil, vErr("必须指定网卡")
	}
	switch ch.Method {
	case "auto":
		if ch.Address != "" || ch.Prefix != 0 || ch.Gateway != "" || len(ch.DNS) != 0 {
			return nil, vErr("自动获取（DHCP）时不能填写静态地址参数")
		}
		return &IPv4Config{Method: "auto"}, nil
	case "manual":
		// 走下面的静态校验。
	default:
		return nil, vErr("获取方式须为 manual（静态）或 auto（DHCP）")
	}

	addr, verr := parseUnicast4(ch.Address, "IP 地址")
	if verr != nil {
		return nil, verr
	}
	if ch.Prefix < 8 || ch.Prefix > 30 {
		return nil, vErr("前缀长度须在 8–30 之间（如 24 对应掩码 255.255.255.0）")
	}
	prefix := netip.PrefixFrom(addr, ch.Prefix).Masked()
	network := prefix.Addr()
	bcast := broadcastOf(prefix)
	if addr == network {
		return nil, vErr("IP 地址 %s 是网段 %s 的网络地址，不能分配给设备", ch.Address, prefix)
	}
	if addr == bcast {
		return nil, vErr("IP 地址 %s 是网段 %s 的广播地址，不能分配给设备", ch.Address, prefix)
	}

	cfg := &IPv4Config{
		Method:    "manual",
		Addresses: []string{fmt.Sprintf("%s/%d", addr, ch.Prefix)},
	}
	if ch.Gateway != "" {
		gw, verr := parseUnicast4(ch.Gateway, "网关")
		if verr != nil {
			return nil, verr
		}
		if gw == addr {
			return nil, vErr("网关不能与 IP 地址相同")
		}
		if !prefix.Contains(gw) || gw == network || gw == bcast {
			return nil, vErr("网关 %s 不在网段 %s 内", ch.Gateway, prefix)
		}
		cfg.Gateway = gw.String()
	}
	if len(ch.DNS) > 3 {
		return nil, vErr("DNS 服务器最多 3 个")
	}
	seen := map[netip.Addr]bool{}
	for _, d := range ch.DNS {
		if strings.TrimSpace(d) == "" {
			continue
		}
		ip, verr := parseUnicast4(strings.TrimSpace(d), "DNS 服务器")
		if verr != nil {
			return nil, verr
		}
		if seen[ip] {
			continue
		}
		seen[ip] = true
		cfg.DNS = append(cfg.DNS, ip.String())
	}
	return cfg, nil
}

// parseUnicast4 解析点分 IPv4 并排除不能作为主机/网关/DNS 的地址类别。
func parseUnicast4(s, what string) (netip.Addr, *ValidationError) {
	addr, err := netip.ParseAddr(s)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, vErr("%s %q 不是合法的 IPv4 地址", what, s)
	}
	switch {
	case addr.IsUnspecified():
		return netip.Addr{}, vErr("%s 不能是 0.0.0.0", what)
	case addr.IsLoopback():
		return netip.Addr{}, vErr("%s 不能是回环地址（127.0.0.0/8）", what)
	case addr.IsMulticast():
		return netip.Addr{}, vErr("%s 不能是组播地址", what)
	case addr.IsLinkLocalUnicast():
		return netip.Addr{}, vErr("%s 不能是链路本地地址（169.254.0.0/16）", what)
	case addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return netip.Addr{}, vErr("%s 不能是 255.255.255.255", what)
	}
	return addr, nil
}

// broadcastOf 算 IPv4 网段的广播地址（主机位全 1）。
func broadcastOf(p netip.Prefix) netip.Addr {
	a4 := p.Addr().As4()
	bits := p.Bits()
	for i := 0; i < 4; i++ {
		for b := 0; b < 8; b++ {
			if i*8+b >= bits {
				a4[i] |= 1 << (7 - b)
			}
		}
	}
	return netip.AddrFrom4(a4)
}
