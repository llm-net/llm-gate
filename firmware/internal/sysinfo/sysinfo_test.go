package sysinfo

// 夹具驱动的采集器测试：临时目录里搭一套仿真开发板（Allwinner A733，
// 6×A55 + 2×A76）的 /proc 与 /sys，注入假时钟与假 sleep，逐项验证解析、
// 差值计算与采样节奏（缓存命中、基点复用、过期重采）。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fixture 是一套可改写的 /proc、/sys 夹具与被注入的采集器。
type fixture struct {
	t       *testing.T
	proc    string
	sys     string
	c       *Collector
	nowAt   time.Time
	sleeps  []time.Duration
	onSleep func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:     t,
		proc:  filepath.Join(root, "proc"),
		sys:   filepath.Join(root, "sys"),
		nowAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
	f.c = &Collector{
		procRoot: f.proc,
		sysRoot:  f.sys,
		diskPath: root, // 真实 statfs，测试只断言非零
		window:   defaultWindow,
		cacheTTL: defaultCacheTTL,
		reuseMax: defaultReuseMax,
		now:      func() time.Time { return f.nowAt },
		sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			f.nowAt = f.nowAt.Add(d)
			if f.onSleep != nil {
				f.onSleep()
			}
			return nil
		},
	}
	return f
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeProc(name, content string) { f.write(filepath.Join(f.proc, name), content) }
func (f *fixture) writeSys(name, content string)  { f.write(filepath.Join(f.sys, name), content) }

// installBoard 摆出仿真板的静态拓扑与即时读数。
func (f *fixture) installBoard() {
	cpuinfo := ""
	for i := 0; i < 8; i++ {
		part := "0xd05"
		if i >= 6 {
			part = "0xd0b"
		}
		cpuinfo += "processor\t: " + itoa(i) + "\nCPU implementer\t: 0x41\nCPU part\t: " + part + "\n\n"
	}
	f.writeProc("cpuinfo", cpuinfo)

	f.writeSys("devices/system/cpu/cpufreq/policy0/related_cpus", "0 1 2 3 4 5\n")
	f.writeSys("devices/system/cpu/cpufreq/policy0/cpuinfo_max_freq", "1794000\n")
	f.writeSys("devices/system/cpu/cpufreq/policy0/scaling_cur_freq", "1196000\n")
	f.writeSys("devices/system/cpu/cpufreq/policy6/related_cpus", "6 7\n")
	f.writeSys("devices/system/cpu/cpufreq/policy6/cpuinfo_max_freq", "2002000\n")
	f.writeSys("devices/system/cpu/cpufreq/policy6/scaling_cur_freq", "780000\n")
	for i := 0; i < 6; i++ {
		f.writeSys("devices/system/cpu/cpu"+itoa(i)+"/cpu_capacity", "385\n")
	}
	f.writeSys("devices/system/cpu/cpu6/cpu_capacity", "1024\n")
	f.writeSys("devices/system/cpu/cpu7/cpu_capacity", "1024\n")

	// 真实网卡有 device 链接（测试用目录代替），lo/sit0 没有。
	f.writeSys("class/net/wlan0/device/uevent", "")
	f.writeSys("class/net/wlan0/operstate", "up\n")
	f.writeSys("class/net/eth0/device/uevent", "")
	f.writeSys("class/net/eth0/operstate", "down\n")
	f.writeSys("class/net/lo/operstate", "unknown\n")
	f.writeSys("class/net/sit0/operstate", "down\n")

	zones := []struct{ typ, temp string }{
		{"cpul_thermal_zone", "54312"}, {"cpub_thermal_zone", "54560"},
		{"cpul_idle_zone", "54560"}, {"cpub_idle_zone", "54312"},
		{"gpu_thermal_zone", "54002"}, {"npu_thermal_zone", "54126"},
		{"ddr_thermal_zone", "55304"}, {"skin_zone", "34862"},
	}
	for i, z := range zones {
		f.writeSys("class/thermal/thermal_zone"+itoa(i)+"/type", z.typ+"\n")
		f.writeSys("class/thermal/thermal_zone"+itoa(i)+"/temp", z.temp+"\n")
	}

	f.writeProc("meminfo",
		"MemTotal:        4001548 kB\nMemFree:         3539920 kB\nMemAvailable:    3722704 kB\n"+
			"Buffers:           34184 kB\nCached:          180052 kB\nSwapTotal:       2000772 kB\n"+
			"SwapFree:        2000672 kB\n")
	f.writeProc("loadavg", "0.66 0.25 0.10 2/180 9162\n")
	f.writeProc("uptime", "217836.37 1738413.88\n")
}

// 三个计数状态：A 起点，B 是 A 之后 500ms，C 是 B 之后 3s。数值手工挑成
// 整齐的期望占用率（小核 10%、大核 90/20%……），iowait 计入 total 不计 busy。
func (f *fixture) writeCountersA() {
	stat := "cpu  800 0 200 8900 100 0 0 0 0 0\n"
	for i := 0; i < 6; i++ {
		stat += "cpu" + itoa(i) + " 100 0 0 900 0 0 0 0 0 0\n"
	}
	stat += "cpu6 500 0 0 500 0 0 0 0 0 0\ncpu7 0 0 0 1000 0 0 0 0 0 0\n"
	stat += "intr 1 2 3\nctxt 42\n"
	f.writeProc("stat", stat)
	f.writeNet(1_000_000, 500_000)
}

func (f *fixture) writeCountersB() {
	stat := "cpu  900 0 250 9700 150 0 0 0 0 0\n"
	for i := 0; i < 6; i++ {
		stat += "cpu" + itoa(i) + " 110 0 0 990 0 0 0 0 0 0\n"
	}
	stat += "cpu6 590 0 0 510 0 0 0 0 0 0\ncpu7 20 0 0 1080 0 0 0 0 0 0\n"
	f.writeProc("stat", stat)
	f.writeNet(1_050_000, 510_000)
}

func (f *fixture) writeCountersC() {
	stat := "cpu  1200 0 400 12100 300 0 0 0 0 0\n"
	for i := 0; i < 6; i++ {
		stat += "cpu" + itoa(i) + " 140 0 0 1560 0 0 0 0 0 0\n"
	}
	stat += "cpu6 890 0 0 810 0 0 0 0 0 0\ncpu7 80 0 0 1620 0 0 0 0 0 0\n"
	f.writeProc("stat", stat)
	f.writeNet(1_350_000, 540_000)
}

func (f *fixture) writeNet(wlanRx, wlanTx uint64) {
	f.writeProc("net/dev",
		"Inter-|   Receive                                                |  Transmit\n"+
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
			"    lo:  172813    1130    0    0    0     0          0         0   172813    1130    0    0    0     0       0          0\n"+
			"  sit0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"+
			"  eth0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"+
			" wlan0: "+utoa(wlanRx)+"  163642    0 58124    0     0          0         0  "+utoa(wlanTx)+"   22467    0    0    0     0       0          0\n")
}

func itoa(i int) string    { return strconv.Itoa(i) }
func utoa(u uint64) string { return strconv.FormatUint(u, 10) }

func TestSnapshotBoardFixture(t *testing.T) {
	f := newFixture(t)
	f.installBoard()
	f.writeCountersA()
	f.onSleep = f.writeCountersB // 采样窗内计数前进到 B

	snap, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] != defaultWindow {
		t.Fatalf("首采应现场等一个 %v 窗口，实际 %v", defaultWindow, f.sleeps)
	}
	if snap.WindowMS != 500 {
		t.Errorf("WindowMS = %d，期望 500", snap.WindowMS)
	}
	if snap.UptimeSeconds != 217836 {
		t.Errorf("UptimeSeconds = %d", snap.UptimeSeconds)
	}
	if len(snap.LoadAvg) != 3 || snap.LoadAvg[0] != 0.66 || snap.LoadAvg[2] != 0.10 {
		t.Errorf("LoadAvg = %v", snap.LoadAvg)
	}

	cpu := snap.CPU
	if cpu == nil {
		t.Fatal("CPU 节缺失")
	}
	// 整机：Δtotal=1000，Δidle=800，Δiowait=50 → busy 150 → 15%。
	if cpu.OverallPercent != 15.0 {
		t.Errorf("OverallPercent = %v，期望 15.0", cpu.OverallPercent)
	}
	if len(cpu.Clusters) != 2 {
		t.Fatalf("簇数 = %d，期望 2（大核/小核）", len(cpu.Clusters))
	}
	big, little := cpu.Clusters[0], cpu.Clusters[1]
	if big.Label != "大核" || big.CoreModel != "Cortex-A76" || big.MaxFreqMHz != 2002 || big.CurFreqMHz != 780 {
		t.Errorf("大核簇 = %+v", big)
	}
	if len(big.Cores) != 2 || big.Cores[0].CPU != 6 || big.Cores[0].Percent != 90.0 || big.Cores[1].Percent != 20.0 {
		t.Errorf("大核逐核 = %+v", big.Cores)
	}
	if big.Percent != 55.0 {
		t.Errorf("大核簇占用 = %v，期望 55.0", big.Percent)
	}
	if little.Label != "小核" || little.CoreModel != "Cortex-A55" || little.MaxFreqMHz != 1794 || little.CurFreqMHz != 1196 {
		t.Errorf("小核簇 = %+v", little)
	}
	if little.Percent != 10.0 || len(little.Cores) != 6 || little.Cores[0].CPU != 0 || little.Cores[0].Percent != 10.0 {
		t.Errorf("小核占用 = %v %+v", little.Percent, little.Cores)
	}

	mem := snap.Memory
	if mem == nil {
		t.Fatal("内存节缺失")
	}
	if mem.TotalBytes != 4001548*1024 || mem.UsedBytes != (4001548-3722704)*1024 ||
		mem.BuffCacheBytes != (34184+180052)*1024 ||
		mem.SwapTotalBytes != 2000772*1024 || mem.SwapUsedBytes != 100*1024 {
		t.Errorf("内存 = %+v", mem)
	}

	// 网卡：只剩 eth0/wlan0（lo、sit0 无 device 链接被滤掉），按名排序。
	if len(snap.Network) != 2 {
		t.Fatalf("网卡数 = %d：%+v", len(snap.Network), snap.Network)
	}
	eth, wlan := snap.Network[0], snap.Network[1]
	if eth.Name != "eth0" || eth.Up || eth.RxBytesPerSec != 0 {
		t.Errorf("eth0 = %+v", eth)
	}
	// Δrx=50000/0.5s=100000，Δtx=10000/0.5s=20000。
	if wlan.Name != "wlan0" || !wlan.Up || wlan.RxBytesPerSec != 100000 || wlan.TxBytesPerSec != 20000 ||
		wlan.RxTotalBytes != 1_050_000 {
		t.Errorf("wlan0 = %+v", wlan)
	}

	wantTemps := []TempZone{
		{Label: "CPU 小核", Celsius: 54.3}, {Label: "CPU 大核", Celsius: 54.6}, {Label: "GPU", Celsius: 54.0},
		{Label: "NPU", Celsius: 54.1}, {Label: "DDR", Celsius: 55.3}, {Label: "机身表面", Celsius: 34.9, Surface: true},
	}
	if len(snap.Temperatures) != len(wantTemps) {
		t.Fatalf("温度路数 = %d：%+v", len(snap.Temperatures), snap.Temperatures)
	}
	for i, want := range wantTemps {
		if snap.Temperatures[i] != want {
			t.Errorf("温度[%d] = %+v，期望 %+v", i, snap.Temperatures[i], want)
		}
	}

	if snap.Disk == nil || snap.Disk.TotalBytes == 0 {
		t.Errorf("磁盘节 = %+v", snap.Disk)
	}
}

func TestSnapshotCacheAndBaseReuse(t *testing.T) {
	f := newFixture(t)
	f.installBoard()
	f.writeCountersA()
	f.onSleep = f.writeCountersB

	first, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 缓存窗口内重复调用：同一份快照，不再采样。
	cached, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cached != first {
		t.Error("cacheTTL 内应返回同一份快照")
	}

	// 3s 后：缓存过期，但上份计数仍新鲜 → 复用为基点，不等采样窗。
	f.nowAt = f.nowAt.Add(3 * time.Second)
	f.writeCountersC()
	third, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sleeps) != 1 {
		t.Fatalf("复用基点不应再等窗口，sleep 记录 = %v", f.sleeps)
	}
	if third.WindowMS != 3000 {
		t.Errorf("WindowMS = %d，期望 3000", third.WindowMS)
	}
	// B→C：大核 cpu6 Δtotal=600 Δbusy=300 → 50%；wlan0 Δrx=300000/3s=100000。
	if third.CPU.Clusters[0].Cores[0].Percent != 50.0 {
		t.Errorf("复用基点后大核 cpu6 = %v", third.CPU.Clusters[0].Cores[0].Percent)
	}
	if third.Network[1].RxBytesPerSec != 100000 || third.Network[1].TxBytesPerSec != 10000 {
		t.Errorf("复用基点后 wlan0 速率 = %+v", third.Network[1])
	}

	// 120s 后（> reuseMax 90s）：上份计数过老，重新现场采样。
	f.nowAt = f.nowAt.Add(120 * time.Second)
	f.onSleep = nil // 计数不动 → 各占用率归零，但不许出错
	fourth, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sleeps) != 2 {
		t.Fatalf("基点过期应重新等窗口，sleep 记录 = %v", f.sleeps)
	}
	if fourth.CPU.OverallPercent != 0 {
		t.Errorf("零差值占用 = %v，期望 0", fourth.CPU.OverallPercent)
	}
}

// TestSnapshotEmptyPlatform：没有 /proc、/sys 的平台（darwin 开发机）——
// 快照只剩时间戳与磁盘，不出错、不白等采样窗。
func TestSnapshotEmptyPlatform(t *testing.T) {
	f := newFixture(t)
	snap, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sleeps) != 0 {
		t.Errorf("空平台不应等采样窗：%v", f.sleeps)
	}
	if snap.CPU != nil || snap.Memory != nil || snap.Network != nil || snap.Temperatures != nil {
		t.Errorf("空平台应整节省略：%+v", snap)
	}
	if snap.UptimeSeconds != 0 || snap.LoadAvg != nil {
		t.Errorf("空平台 uptime/load 应缺省：%+v", snap)
	}
}

// TestFallbackSingleCluster：无 cpufreq 的平台（x86 开发机、精简内核）整机
// 归入一个「CPU」簇。
func TestFallbackSingleCluster(t *testing.T) {
	f := newFixture(t)
	f.writeProc("stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\n")
	f.onSleep = func() {
		f.writeProc("stat", "cpu  150 0 150 900 0 0 0 0 0 0\ncpu0 75 0 75 450 0 0 0 0 0 0\ncpu1 75 0 75 450 0 0 0 0 0 0\n")
	}
	snap, err := f.c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cpu := snap.CPU
	if cpu == nil || len(cpu.Clusters) != 1 {
		t.Fatalf("CPU = %+v", cpu)
	}
	cl := cpu.Clusters[0]
	if cl.Label != "CPU" || cl.CoreModel != "" || cl.MaxFreqMHz != 0 || len(cl.Cores) != 2 {
		t.Errorf("兜底簇 = %+v", cl)
	}
	if cl.Percent != 50.0 { // Δtotal=200，Δbusy=100
		t.Errorf("兜底簇占用 = %v", cl.Percent)
	}
}

func TestRateWraparound(t *testing.T) {
	if got := ratePerSec(1000, 500, 1); got != 0 {
		t.Errorf("计数回绕速率 = %v，期望 0", got)
	}
	if got := busyPercent(cpuTicks{idle: 100}, cpuTicks{idle: 100}); got != 0 {
		t.Errorf("零差值占用 = %v，期望 0", got)
	}
}

func TestClusterLabel(t *testing.T) {
	cases := []struct {
		i, n int
		want string
	}{
		{0, 1, "CPU"}, {0, 2, "大核"}, {1, 2, "小核"},
		{0, 3, "大核"}, {1, 3, "中核"}, {2, 3, "小核"}, {3, 4, "簇 4"},
	}
	for _, c := range cases {
		if got := clusterLabel(c.i, c.n); got != c.want {
			t.Errorf("clusterLabel(%d,%d) = %q，期望 %q", c.i, c.n, got, c.want)
		}
	}
}

// 四款在手板卡的温度区命名都要认得出来：A733 走 cpub/cpul_thermal_zone，
// RK3576 走 bigcore/little-core/gate-thermal，树莓派 5 只有一个 cpu-thermal，
// H618 有 cpu/gpu/ddr/ve-thermal 四个。认不出的类型仍原样透出。
func TestThermalLabel(t *testing.T) {
	cases := []struct{ zoneType, want string }{
		// Allwinner A733（cubie-a7a）
		{"cpub_thermal_zone", "CPU 大核"},
		{"cpul_thermal_zone", "CPU 小核"},
		{"gpu_thermal_zone", "GPU"},
		// Rockchip RK3576（rk3576-evb1）
		{"bigcore-thermal", "CPU 大核"},
		{"little-core-thermal", "CPU 小核"},
		{"gate-thermal", "SoC"},
		{"ddr-thermal", "DDR"},
		{"npu-thermal", "NPU"},
		{"gpu-thermal", "GPU"},
		// 博通 BCM2712（bcm2712-pi5）：整块板只有这一个 zone
		{"cpu-thermal", "CPU"},
		// Allwinner H618 主线内核（h618-x98h）：cpu/gpu/ddr 三个上面已覆盖，
		// ve-thermal 是这块板带进来的新命名（视频引擎）
		{"ve-thermal", "视频引擎"},
		// "ve" 的前缀匹配不许殃及无辜：这两个不是视频引擎
		{"device-thermal", "device-thermal"},
		{"reserved_zone", "reserved_zone"},
		// 认不出的原样透出
		{"vendor_zone_7", "vendor_zone_7"},
	}
	for _, c := range cases {
		if got := thermalLabel(c.zoneType); got != c.want {
			t.Errorf("thermalLabel(%q) = %q，期望 %q", c.zoneType, got, c.want)
		}
	}
}
