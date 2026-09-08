// netconfig 的可执行验收：terse 解析（转义/列表键）、IPv4 校验规则、以及
// 「延迟应用 → 待确认 → 确认落盘 / 逾期回滚」状态机的全部走向。nmcli 以
// 脚本化 Runner 假冒（板上真实输出形状），定时器用毫秒级时序真跑。
package netconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNM 假冒板上的 nmcli：记录每次调用，按参数形状回放真实输出。
type fakeNM struct {
	mu    sync.Mutex
	calls [][]string

	failDeviceModify     bool
	failConnectionModify bool
	failReapply          bool
	failConnectionUp     bool
}

func (f *fakeNM) runner(ctx context.Context, args ...string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "device status"):
		return "wlan0:wifi:connected:UUID-1:zcloud\n" +
			"lo:loopback:connected (externally):UUID-LO:lo\n" +
			"p2p-dev-wlan0:wifi-p2p:disconnected::\n" +
			"eth0:ethernet:unavailable::\n" +
			"wlan1:wifi:unavailable::\n" +
			"sit0:iptunnel:unmanaged::\n", "", nil
	case strings.Contains(joined, "device show wlan0"):
		return "GENERAL.HWADDR:9C\\:04\\:B6\\:84\\:3D\\:72\n" +
			"IP4.ADDRESS[1]:192.168.50.101/24\n" +
			"IP4.GATEWAY:192.168.50.1\n" +
			"IP4.DNS[1]:192.168.53.1\n" +
			"IP4.ROUTE[1]:dst = 192.168.50.0/24, nh = 0.0.0.0, mt = 600\n" +
			"IP4.ROUTE[2]:dst = 0.0.0.0/0, nh = 192.168.50.1, mt = 100\n", "", nil
	case strings.Contains(joined, "device show eth0"):
		return "GENERAL.HWADDR:AA\\:BB\\:CC\\:DD\\:EE\\:FF\nIP4.GATEWAY:\n", "", nil
	case strings.Contains(joined, "device show wlan1"):
		return "GENERAL.HWADDR:AA\\:BB\\:CC\\:DD\\:EE\\:01\n", "", nil
	// 非活动 profile 列表（板上 netplan 形状：eth0 一个精确绑定 + 一个通配，
	// 无非活动 wifi profile）。
	case strings.Contains(joined, "UUID,TYPE,ACTIVE connection show"):
		return "UUID-1:802-11-wireless:yes\n" +
			"UUID-LO:loopback:yes\n" +
			"UUID-ETH-WILD:802-3-ethernet:no\n" +
			"UUID-ETH-EXACT:802-3-ethernet:no\n", "", nil
	case strings.Contains(joined, "connection show uuid UUID-1"):
		return "connection.id:zcloud\nipv4.method:manual\n" +
			"ipv4.addresses:192.168.50.101/24\nipv4.gateway:192.168.50.1\nipv4.dns:192.168.53.1\n", "", nil
	case strings.Contains(joined, "connection show uuid UUID-ETH-EXACT"):
		return "connection.id:netplan-eth0\nconnection.interface-name:eth0\n" +
			"connection.autoconnect-priority:0\nipv4.method:auto\n", "", nil
	case strings.Contains(joined, "connection show uuid UUID-ETH-WILD"):
		return "connection.id:netplan-all-eth-interfaces\nconnection.interface-name:\n" +
			"connection.autoconnect-priority:0\nipv4.method:auto\n", "", nil
	case strings.HasPrefix(joined, "device modify "):
		if f.failDeviceModify {
			return "", "Error: Reapplying connection to device 'wlan0' failed: not authorized", errors.New("exit status 4")
		}
		return "", "", nil
	case strings.HasPrefix(joined, "connection modify "):
		if f.failConnectionModify {
			return "", "Error: Failed to modify connection 'zcloud': Insufficient privileges", errors.New("exit status 1")
		}
		return "", "", nil
	case strings.HasPrefix(joined, "device reapply "):
		if f.failReapply {
			return "", "Error: Reapplying connection to device 'wlan0' failed", errors.New("exit status 4")
		}
		return "", "", nil
	case strings.Contains(joined, "connection up"):
		if f.failConnectionUp {
			return "", "Error: Connection activation failed", errors.New("exit status 4")
		}
		return "", "", nil
	}
	return "", "", fmt.Errorf("fakeNM: 未编排的调用 %q", joined)
}

// hasCall 报告是否出现过「以这些词开头（忽略 -t/-f/-w 选项）」的调用。
func (f *fakeNM) hasCall(prefix ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		var words []string
		for i := 0; i < len(call); i++ {
			switch call[i] {
			case "-t":
			case "-f", "-w":
				i++ // 连值一起跳过
			default:
				words = append(words, call[i])
			}
		}
		if len(words) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if words[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

type auditRec struct {
	event, detail string
}

type auditLog struct {
	mu   sync.Mutex
	recs []auditRec
}

func (a *auditLog) fn(event, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, auditRec{event, detail})
}

func (a *auditLog) has(event string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.recs {
		if r.event == event {
			return true
		}
	}
	return false
}

func newTestManager(t *testing.T, f *fakeNM) (*Manager, *auditLog) {
	t.Helper()
	al := &auditLog{}
	m := NewManager(slog.New(slog.DiscardHandler), al.fn,
		WithRunner(f.runner),
		// 应用延迟/确认窗口压到毫秒级真跑定时器。
		WithTimings(20*time.Millisecond, 120*time.Millisecond))
	return m, al
}

func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", msg)
}


func testChange() Change {
	return Change{Device: "wlan0", Method: "manual",
		Address: "192.168.50.102", Prefix: 24, Gateway: "192.168.50.1", DNS: []string{"192.168.53.1"}}
}

// ---- 解析 ----

func TestSplitTerseEscapes(t *testing.T) {
	got := splitTerse(`GENERAL.HWADDR:9C\:04\:B6\\x`)
	if len(got) != 2 || got[0] != "GENERAL.HWADDR" || got[1] != `9C:04:B6\x` {
		t.Fatalf("splitTerse 转义解析错误：%#v", got)
	}
}

func TestParseShowKVListKeys(t *testing.T) {
	kv := parseShowKV("IP4.ADDRESS[1]:10.0.0.2/24\nIP4.ADDRESS[2]:10.0.0.3/24\nIP4.GATEWAY:--\n")
	if got := kv["IP4.ADDRESS"]; len(got) != 2 || got[0] != "10.0.0.2/24" || got[1] != "10.0.0.3/24" {
		t.Fatalf("列表键聚合错误：%#v", kv)
	}
	if _, ok := kv["IP4.GATEWAY"]; ok {
		t.Fatalf("未设置值（--）应跳过：%#v", kv)
	}
}

// ---- 读 ----

func TestStatusReadsInterfaces(t *testing.T) {
	f := &fakeNM{}
	m, _ := newTestManager(t, f)
	st := m.Status(context.Background())
	if !st.Supported {
		t.Fatalf("supported=false：%s", st.Reason)
	}
	if len(st.Interfaces) != 3 {
		t.Fatalf("应只收 wifi/ethernet 三块物理网卡，得到 %d 块", len(st.Interfaces))
	}
	w := st.Interfaces[0]
	if w.Device != "wlan0" || !w.Connected || w.MAC != "9C:04:B6:84:3D:72" {
		t.Fatalf("wlan0 读数错误：%+v", w)
	}
	if w.Runtime == nil || len(w.Runtime.Addresses) != 1 || w.Runtime.Addresses[0] != "192.168.50.101/24" {
		t.Fatalf("wlan0 运行时读数错误：%+v", w.Runtime)
	}
	if w.Connection == nil || w.Connection.ID != "zcloud" || w.Connection.IPv4.Method != "manual" ||
		w.Connection.IPv4.Gateway != "192.168.50.1" {
		t.Fatalf("wlan0 连接配置错误：%+v", w.Connection)
	}
	if !w.DefaultRoute {
		t.Fatalf("wlan0 持有 0.0.0.0/0，应标默认出口：%+v", w)
	}
	e := st.Interfaces[1]
	if e.Device != "eth0" || e.Connected || e.Runtime != nil || e.DefaultRoute {
		t.Fatalf("eth0（未插线）读数错误：%+v", e)
	}
	if e.MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("eth0 MAC 错误：%q", e.MAC)
	}
	// 未插线网卡应带出离线直写的候选 profile：interface-name 精确匹配优先。
	if e.Connection == nil || e.Connection.ID != "netplan-eth0" || e.Connection.UUID != "UUID-ETH-EXACT" {
		t.Fatalf("eth0 候选 profile 错误：%+v", e.Connection)
	}
	// 没有任何候选 profile 的未连接网卡：Connection 为空。
	if w1 := st.Interfaces[2]; w1.Device != "wlan1" || w1.Connection != nil {
		t.Fatalf("wlan1（无候选 profile）读数错误：%+v", w1)
	}
}

func TestMarkEntry(t *testing.T) {
	ifaces := []Interface{
		{Device: "wlan0", Runtime: &IPv4Config{Addresses: []string{"192.168.50.101/24"}}},
		{Device: "eth0"},
	}
	MarkEntry(ifaces, netip.MustParseAddr("192.168.50.101"))
	if !ifaces[0].Entry || ifaces[1].Entry {
		t.Fatalf("按本地地址标注入口错误：%+v", ifaces)
	}
	// 回环/隧道进来：不命中任何网卡，也不误标。
	fresh := []Interface{{Device: "wlan0", Runtime: &IPv4Config{Addresses: []string{"192.168.50.101/24"}}}}
	MarkEntry(fresh, netip.MustParseAddr("127.0.0.1"))
	if fresh[0].Entry {
		t.Fatalf("回环地址不应命中：%+v", fresh)
	}
}

func TestStatusUnsupported(t *testing.T) {
	m := NewManager(slog.New(slog.DiscardHandler), nil, WithRunner(
		func(ctx context.Context, args ...string) (string, string, error) {
			return "", "", exec.ErrNotFound
		}))
	st := m.Status(context.Background())
	if st.Supported || st.Reason == "" {
		t.Fatalf("nmcli 不存在应降级为 supported=false + reason：%+v", st)
	}
}

// ---- 校验 ----

func TestValidateChange(t *testing.T) {
	base := testChange()
	cases := []struct {
		name    string
		mutate  func(*Change)
		wantErr string // 空 = 应通过；否则错误文案须含此子串
	}{
		{"静态合法", func(c *Change) {}, ""},
		{"网关可留空", func(c *Change) { c.Gateway = "" }, ""},
		{"DNS 可留空", func(c *Change) { c.DNS = nil }, ""},
		{"DHCP 合法", func(c *Change) {
			*c = Change{Device: "wlan0", Method: "auto"}
		}, ""},
		{"DHCP 带静态参数", func(c *Change) { c.Method = "auto" }, "不能填写静态地址参数"},
		{"方式非法", func(c *Change) { c.Method = "static" }, "获取方式"},
		{"地址非法", func(c *Change) { c.Address = "10.0.51" }, "不是合法的 IPv4"},
		{"地址回环", func(c *Change) { c.Address = "127.0.0.2" }, "回环"},
		{"地址链路本地", func(c *Change) { c.Address = "169.254.1.2" }, "链路本地"},
		{"前缀过大", func(c *Change) { c.Prefix = 31 }, "前缀长度"},
		{"前缀过小", func(c *Change) { c.Prefix = 4 }, "前缀长度"},
		{"网络地址", func(c *Change) { c.Address = "192.168.50.0" }, "网络地址"},
		{"广播地址", func(c *Change) { c.Address = "192.168.50.255" }, "广播地址"},
		{"网关跨网段", func(c *Change) { c.Gateway = "10.0.52.1" }, "不在网段"},
		{"网关即地址", func(c *Change) { c.Gateway = "192.168.50.102" }, "网关不能与 IP 地址相同"},
		{"DNS 过多", func(c *Change) { c.DNS = []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} }, "最多 3 个"},
		{"DNS 非法", func(c *Change) { c.DNS = []string{"dns.example"} }, "不是合法的 IPv4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := base
			ch.DNS = append([]string(nil), base.DNS...)
			tc.mutate(&ch)
			cfg, err := validateChange(&ch)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过，得到错误：%v", err)
				}
				if ch.Method == "manual" && (len(cfg.Addresses) != 1 || !strings.Contains(cfg.Addresses[0], "/")) {
					t.Fatalf("归一化配置错误：%+v", cfg)
				}
				return
			}
			if err == nil {
				t.Fatalf("应拒绝（%s），却通过了：%+v", tc.wantErr, cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误文案 %q 不含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateDNSDedupe(t *testing.T) {
	ch := testChange()
	ch.DNS = []string{"192.168.53.1", "192.168.53.1", "1.1.1.1"}
	cfg, err := validateChange(&ch)
	if err != nil {
		t.Fatalf("应通过：%v", err)
	}
	if len(cfg.DNS) != 2 {
		t.Fatalf("DNS 应去重保序：%v", cfg.DNS)
	}
}

// ---- 状态机 ----

func TestSubmitRejections(t *testing.T) {
	f := &fakeNM{}
	m, _ := newTestManager(t, f)
	ctx := context.Background()

	ch := testChange()
	ch.Device = "enp999"
	if _, err := m.Submit(ctx, ch); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("未知网卡应拒绝，得到 %v", err)
	}
	ch = testChange()
	ch.Device = "wlan1"
	if _, err := m.Submit(ctx, ch); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("无候选 profile 的未激活网卡应拒绝，得到 %v", err)
	}
	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("首次提交应通过：%v", err)
	}
	if _, err := m.Submit(ctx, testChange()); !errors.Is(err, ErrChangePending) {
		t.Fatalf("已有进行中的变更应拒绝，得到 %v", err)
	}
	// 有变更进行中时，离线直写同样拒绝：整页同一时刻只有一项变更。
	ch = testChange()
	ch.Device = "eth0"
	if _, err := m.Submit(ctx, ch); !errors.Is(err, ErrChangePending) {
		t.Fatalf("进行中时离线直写应拒绝，得到 %v", err)
	}
}

// ---- 离线直写（未连接网卡） ----

func TestOfflinePersist(t *testing.T) {
	f := &fakeNM{}
	m, _ := newTestManager(t, f)
	ctx := context.Background()
	ch := Change{Device: "eth0", Method: "manual",
		Address: "192.168.8.20", Prefix: 24, Gateway: "192.168.8.1"}
	out, err := m.Submit(ctx, ch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if out.Pending != nil || out.Persisted == nil {
		t.Fatalf("未连接网卡应走离线直写：%+v", out)
	}
	if out.Persisted.Device != "eth0" || out.Persisted.ConnectionID != "netplan-eth0" ||
		out.Persisted.New.Addresses[0] != "192.168.8.20/24" {
		t.Fatalf("离线直写结果错误：%+v", out.Persisted)
	}
	if !f.hasCall("connection", "modify", "uuid", "UUID-ETH-EXACT",
		"ipv4.method", "manual", "ipv4.addresses", "192.168.8.20/24") {
		t.Fatalf("离线直写应 connection modify 候选 profile，调用序列：%v", f.calls)
	}
	if f.hasCall("device", "modify") {
		t.Fatalf("离线直写不得动运行时（device modify）：%v", f.calls)
	}
	if st := m.Status(ctx); st.Pending != nil || st.Last != nil {
		t.Fatalf("离线直写不进 pending 状态机：pending=%+v last=%+v", st.Pending, st.Last)
	}
	// 离线直写后仍可提交在线变更。
	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("离线直写后在线提交应通过：%v", err)
	}
}

func TestOfflinePersistFailure(t *testing.T) {
	f := &fakeNM{failConnectionModify: true}
	m, _ := newTestManager(t, f)
	ch := Change{Device: "eth0", Method: "auto"}
	if _, err := m.Submit(context.Background(), ch); !errors.Is(err, ErrOfflineWriteFailed) {
		t.Fatalf("离线直写失败应报 ErrOfflineWriteFailed，得到 %v", err)
	}
}

func TestConfirmFlow(t *testing.T) {
	f := &fakeNM{}
	m, al := newTestManager(t, f)
	ctx := context.Background()

	out, err := m.Submit(ctx, testChange())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	p := out.Pending
	if p == nil || out.Persisted != nil {
		t.Fatalf("已连接网卡应走确认状态机：%+v", out)
	}
	if p.Phase != PhaseScheduled || p.Old.Summary() != "192.168.50.101/24 gw 192.168.50.1 dns 192.168.53.1" {
		t.Fatalf("登记快照错误：%+v", p)
	}
	// 应用前确认：拒绝。
	if _, err := m.Confirm(); !errors.Is(err, ErrNotApplied) {
		t.Fatalf("应用前确认应拒绝，得到 %v", err)
	}
	// **等的是审计而不只是相位**：applyPending 先置 PhaseAwaitingConfirm 并解锁，
	// 之后才补 system.network_apply 审计（审计回调会写库，刻意不在锁内做）。
	// 只等相位翻转的话，这一等与那一写没有 happens-before 关系，断言审计就是
	// 一个约 1/3 概率的偶发失败。审计到位 ⇒ 相位必然早已翻转，一个条件够了。
	waitFor(t, 2*time.Second, "进入待确认并补上应用审计", func() bool {
		st := m.Status(ctx)
		return st.Pending != nil && st.Pending.Phase == PhaseAwaitingConfirm && al.has("system.network_apply")
	})
	if !f.hasCall("device", "modify", "wlan0", "ipv4.method", "manual", "ipv4.addresses", "192.168.50.102/24") {
		t.Fatalf("应用应走 device modify（运行时），调用序列：%v", f.calls)
	}
	if _, err := m.Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !f.hasCall("connection", "modify", "uuid", "UUID-1", "ipv4.method", "manual") {
		t.Fatalf("确认应写配置文件（connection modify），调用序列：%v", f.calls)
	}
	st := m.Status(ctx)
	if st.Pending != nil || st.Last == nil || st.Last.Event != ResultConfirmed || !st.Last.OK {
		t.Fatalf("确认后状态错误：pending=%+v last=%+v", st.Pending, st.Last)
	}
	// 原确认窗口耗尽后不得再发生回滚。
	time.Sleep(150 * time.Millisecond)
	if f.hasCall("device", "reapply") {
		t.Fatalf("确认后不应回滚，调用序列：%v", f.calls)
	}
}

func TestRollbackOnTimeout(t *testing.T) {
	f := &fakeNM{}
	m, al := newTestManager(t, f)
	ctx := context.Background()

	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, 2*time.Second, "回滚完成", func() bool {
		st := m.Status(ctx)
		return st.Pending == nil && st.Last != nil
	})
	st := m.Status(ctx)
	if st.Last.Event != ResultRolledBack || !st.Last.OK {
		t.Fatalf("超时应回滚：%+v", st.Last)
	}
	if !f.hasCall("device", "reapply", "wlan0") {
		t.Fatalf("回滚应重放配置文件（device reapply），调用序列：%v", f.calls)
	}
	if !al.has("system.network_rollback") {
		t.Fatal("回滚应补审计 system.network_rollback")
	}
	// 回滚后允许再次提交。
	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("回滚后重新提交应通过：%v", err)
	}
}

func TestRollbackFallsBackToConnectionUp(t *testing.T) {
	f := &fakeNM{failReapply: true}
	m, _ := newTestManager(t, f)
	ctx := context.Background()

	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, 2*time.Second, "回滚完成", func() bool {
		return m.Status(ctx).Pending == nil
	})
	if !f.hasCall("connection", "up", "uuid", "UUID-1") {
		t.Fatalf("reapply 失败应回退 connection up，调用序列：%v", f.calls)
	}
	if st := m.Status(ctx); st.Last.Event != ResultRolledBack || !st.Last.OK {
		t.Fatalf("回退路径也算回滚成功：%+v", st.Last)
	}
}

func TestApplyFailureEndsRound(t *testing.T) {
	f := &fakeNM{failDeviceModify: true}
	m, al := newTestManager(t, f)
	ctx := context.Background()

	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, 2*time.Second, "应用失败终局", func() bool {
		st := m.Status(ctx)
		return st.Pending == nil && st.Last != nil
	})
	st := m.Status(ctx)
	if st.Last.Event != ResultApplyFailed || st.Last.OK {
		t.Fatalf("应用失败终局错误：%+v", st.Last)
	}
	if !al.has("system.network_apply") {
		t.Fatal("失败也要补审计 system.network_apply")
	}
	// 失败后可立即重新提交。
	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("失败后重新提交应通过：%v", err)
	}
}

func TestConfirmPersistFailureKeepsPending(t *testing.T) {
	f := &fakeNM{failConnectionModify: true}
	m, _ := newTestManager(t, f)
	ctx := context.Background()

	if _, err := m.Submit(ctx, testChange()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, 2*time.Second, "进入待确认", func() bool {
		st := m.Status(ctx)
		return st.Pending != nil && st.Pending.Phase == PhaseAwaitingConfirm
	})
	if _, err := m.Confirm(); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("落盘失败应报 ErrPersistFailed，得到 %v", err)
	}
	if st := m.Status(ctx); st.Pending == nil {
		t.Fatal("落盘失败后应保持待确认（可重试，逾期回滚）")
	}
	// 落盘一直失败 → 最终仍会回滚兜底。
	waitFor(t, 2*time.Second, "兜底回滚", func() bool {
		return m.Status(ctx).Pending == nil
	})
	if st := m.Status(ctx); st.Last.Event != ResultRolledBack {
		t.Fatalf("兜底应回滚：%+v", st.Last)
	}
}

func TestNoPendingConfirm(t *testing.T) {
	f := &fakeNM{}
	m, _ := newTestManager(t, f)
	if _, err := m.Confirm(); !errors.Is(err, ErrNoPending) {
		t.Fatalf("无变更确认应拒绝，得到 %v", err)
	}
}
