package sysinfo

// /proc 与 /sys 的读取与解析。统一容错口径：文件缺失、格式意外的行一律
// 跳过，绝不因单项读不到而放弃整份快照。所有路径经 procRoot/sysRoot 拼接，
// 测试用临时目录夹具驱动全部解析逻辑。

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuTicks 是 /proc/stat 一行的 jiffies 计数（guest 列已计入 user/nice，
// 不重复累加）。
type cpuTicks struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTicks) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

func (t cpuTicks) add(o cpuTicks) cpuTicks {
	return cpuTicks{
		user: t.user + o.user, nice: t.nice + o.nice,
		system: t.system + o.system, idle: t.idle + o.idle,
		iowait: t.iowait + o.iowait, irq: t.irq + o.irq,
		softirq: t.softirq + o.softirq, steal: t.steal + o.steal,
	}
}

// netCounters 是一块网卡的累计收发字节数。
type netCounters struct {
	rx, tx uint64
}

// rawSample 是一次原始计数采样：CPU tick 与网卡字节数，差值算占用与速率。
type rawSample struct {
	at     time.Time
	agg    cpuTicks
	perCPU map[int]cpuTicks
	net    map[string]netCounters
}

// clusterMeta 是一个 cpufreq 簇的静态拓扑（开机后不变，进程内解析一次）。
type clusterMeta struct {
	label      string
	coreModel  string
	cpus       []int
	policyDir  string // 读 scaling_cur_freq 用；无 cpufreq 的兜底簇为空
	maxFreqKHz int64
	capacity   int64 // /sys cpu_capacity；大小核排序依据，缺省 0
}

// readRaw 读一份原始计数。
func (c *Collector) readRaw() *rawSample {
	s := &rawSample{
		at:     c.now(),
		perCPU: make(map[int]cpuTicks),
		net:    make(map[string]netCounters),
	}
	c.readProcStat(s)
	c.readNetDev(s)
	return s
}

// readProcStat 解析 /proc/stat 的 cpu 行（首行整机汇总 + 每核一行）。
func (c *Collector) readProcStat(s *rawSample) {
	data, err := os.ReadFile(filepath.Join(c.procRoot, "stat"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		var t cpuTicks
		for i, dst := range []*uint64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal} {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				continue
			}
			*dst = v
		}
		if fields[0] == "cpu" {
			s.agg = t
		} else if id, err := strconv.Atoi(fields[0][3:]); err == nil {
			s.perCPU[id] = t
		}
	}
}

// readNetDev 解析 /proc/net/dev，只收 /sys/class/net/<名>/device 存在的
// 真实硬件网卡（lo、sit0 等内核虚拟设备没有 device 链接）。
func (c *Collector) readNetDev(s *rawSample) {
	data, err := os.ReadFile(filepath.Join(c.procRoot, "net", "dev"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue // 两行表头没有冒号
		}
		name = strings.TrimSpace(name)
		if name == "" || !c.isHardwareIface(name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		s.net[name] = netCounters{rx: rx, tx: tx}
	}
}

func (c *Collector) isHardwareIface(name string) bool {
	_, err := os.Stat(filepath.Join(c.sysRoot, "class", "net", name, "device"))
	return err == nil
}

func (c *Collector) readOperstateUp(name string) bool {
	return readTrimmed(filepath.Join(c.sysRoot, "class", "net", name, "operstate")) == "up"
}

// readTopology 解析 CPU 簇拓扑：cpufreq policy 划簇，cpu_capacity（缺省则
// 最高频率）定大小，/proc/cpuinfo 的 CPU part 认核型号。按大→小排序后命名：
// 一簇叫 CPU，两簇大核/小核，三簇再插中核，更多按序号兜底。
func (c *Collector) readTopology() []clusterMeta {
	parts := c.readCPUParts()
	freqRoot := filepath.Join(c.sysRoot, "devices", "system", "cpu", "cpufreq")
	entries, _ := os.ReadDir(freqRoot)
	var clusters []clusterMeta
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "policy") {
			continue
		}
		dir := filepath.Join(freqRoot, e.Name())
		cpus := parseIntList(readTrimmed(filepath.Join(dir, "related_cpus")))
		if len(cpus) == 0 {
			continue
		}
		meta := clusterMeta{
			cpus:       cpus, // 已升序（parseIntList 排序）
			policyDir:  dir,
			maxFreqKHz: parseInt64(readTrimmed(filepath.Join(dir, "cpuinfo_max_freq"))),
			capacity:   c.readCapacity(cpus[0]),
			coreModel:  armPartName(parts[cpus[0]]),
		}
		clusters = append(clusters, meta)
	}
	// 大核在前：容量优先（同容量比最高频率），再按首核编号保证稳定。
	sort.Slice(clusters, func(i, j int) bool {
		a, b := clusters[i], clusters[j]
		if a.capacity != b.capacity {
			return a.capacity > b.capacity
		}
		if a.maxFreqKHz != b.maxFreqKHz {
			return a.maxFreqKHz > b.maxFreqKHz
		}
		return a.cpus[0] < b.cpus[0]
	})
	for i := range clusters {
		clusters[i].label = clusterLabel(i, len(clusters))
	}
	return clusters
}

// clusterLabel 给第 i 个簇（大→小序）命名。
func clusterLabel(i, n int) string {
	switch {
	case n == 1:
		return "CPU"
	case n == 2:
		return [2]string{"大核", "小核"}[i]
	case n == 3:
		return [3]string{"大核", "中核", "小核"}[i]
	default:
		return "簇 " + strconv.Itoa(i+1)
	}
}

func (c *Collector) readCapacity(cpu int) int64 {
	return parseInt64(readTrimmed(filepath.Join(c.sysRoot, "devices", "system", "cpu",
		"cpu"+strconv.Itoa(cpu), "cpu_capacity")))
}

func (c *Collector) readCurFreqMHz(meta clusterMeta) int64 {
	if meta.policyDir == "" {
		return 0
	}
	return parseInt64(readTrimmed(filepath.Join(meta.policyDir, "scaling_cur_freq"))) / 1000
}

// readCPUParts 从 /proc/cpuinfo 取每个逻辑 CPU 的 Arm part 号（x86 没有该
// 字段，得到空表）。
func (c *Collector) readCPUParts() map[int]string {
	parts := make(map[int]string)
	data, err := os.ReadFile(filepath.Join(c.procRoot, "cpuinfo"))
	if err != nil {
		return parts
	}
	cur := -1
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "processor":
			if id, err := strconv.Atoi(val); err == nil {
				cur = id
			} else {
				cur = -1
			}
		case "CPU part":
			if cur >= 0 {
				parts[cur] = strings.ToLower(val)
			}
		}
	}
	return parts
}

// armPartName 把 Arm 的 CPU part 号译成核型号；认不出返回空（快照里省略，
// 不猜）。
func armPartName(part string) string {
	names := map[string]string{
		"0xd03": "Cortex-A53", "0xd04": "Cortex-A35", "0xd05": "Cortex-A55",
		"0xd07": "Cortex-A57", "0xd08": "Cortex-A72", "0xd09": "Cortex-A73",
		"0xd0a": "Cortex-A75", "0xd0b": "Cortex-A76", "0xd0d": "Cortex-A77",
		"0xd41": "Cortex-A78", "0xd44": "Cortex-X1", "0xd46": "Cortex-A510",
		"0xd47": "Cortex-A710", "0xd48": "Cortex-X2", "0xd4d": "Cortex-A715",
		"0xd4e": "Cortex-X3", "0xd80": "Cortex-A520", "0xd81": "Cortex-A720",
		"0xd82": "Cortex-X4",
	}
	return names[part]
}

// readMeminfo 解析 /proc/meminfo（kB 计），MemTotal 读不到视为整节缺失。
func (c *Collector) readMeminfo() *MemStatus {
	data, err := os.ReadFile(filepath.Join(c.procRoot, "meminfo"))
	if err != nil {
		return nil
	}
	kv := make(map[string]uint64, 8)
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			kv[strings.TrimSpace(key)] = n * 1024
		}
	}
	total, ok := kv["MemTotal"]
	if !ok || total == 0 {
		return nil
	}
	m := &MemStatus{
		TotalBytes:     total,
		AvailableBytes: kv["MemAvailable"],
		BuffCacheBytes: kv["Buffers"] + kv["Cached"],
		SwapTotalBytes: kv["SwapTotal"],
	}
	if m.AvailableBytes <= total {
		m.UsedBytes = total - m.AvailableBytes
	}
	if free := kv["SwapFree"]; free <= m.SwapTotalBytes {
		m.SwapUsedBytes = m.SwapTotalBytes - free
	}
	return m
}

// readThermal 读 /sys/class/thermal 各 zone。*_idle_zone 是策略镜像不是
// 独立传感器，跳过；毫摄氏度换算后超出 −40…150 ℃ 的读数视为传感器无效。
func (c *Collector) readThermal() []TempZone {
	root := filepath.Join(c.sysRoot, "class", "thermal")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type zone struct {
		idx int
		t   TempZone
	}
	var zones []zone
	for _, e := range entries {
		idxStr, ok := strings.CutPrefix(e.Name(), "thermal_zone")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		zoneType := readTrimmed(filepath.Join(dir, "type"))
		if zoneType == "" || strings.Contains(zoneType, "idle") {
			continue
		}
		milli := readTrimmed(filepath.Join(dir, "temp"))
		mv, err := strconv.ParseInt(milli, 10, 64)
		if err != nil {
			continue
		}
		celsius := float64(mv) / 1000
		if celsius < -40 || celsius > 150 {
			continue
		}
		label := thermalLabel(zoneType)
		zones = append(zones, zone{idx: idx, t: TempZone{
			Label:   label,
			Surface: label == surfaceLabel,
			Celsius: round1(celsius),
		}})
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].idx < zones[j].idx })
	out := make([]TempZone, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.t)
	}
	return out
}

// surfaceLabel 是机身表面那一路的源文案；TempZone.Surface 与历史点的 Surface
// 都由它推出，界面不再比对文案。
const surfaceLabel = "机身表面"

// thermalLabel 把 zone 类型译成读数名。对齐四款在手板卡的命名
//（Allwinner A733 的 cpub/cpul_thermal_zone、Rockchip RK3576 的
// bigcore/little-core/gate-thermal、树莓派 5 唯一那个 cpu-thermal、
// 主线内核下 Allwinner H618 的 cpu/gpu/ddr/ve-thermal 四个），
// 同时兜住常见内核惯例；认不出的类型原样透出，宁可生硬不可撒谎。
func thermalLabel(zoneType string) string {
	t := strings.ToLower(zoneType)
	switch {
	case strings.Contains(t, "cpub"), strings.Contains(t, "bigcore"), strings.Contains(t, "big-core"):
		return "CPU 大核"
	case strings.Contains(t, "cpul"), strings.Contains(t, "littlecore"), strings.Contains(t, "little-core"):
		return "CPU 小核"
	case strings.Contains(t, "cpu"):
		return "CPU"
	case strings.Contains(t, "gpu"):
		return "GPU"
	case strings.Contains(t, "npu"):
		return "NPU"
	case strings.Contains(t, "ddr"), strings.Contains(t, "dram"):
		return "DDR"
	// 全志的视频引擎（Video Engine）。前缀匹配而不是 Contains("ve")——后者会
	// 顺手吃掉 "device"、"reserved" 这类串，把无关 zone 全标成视频引擎。
	case strings.HasPrefix(t, "ve-"), strings.HasPrefix(t, "ve_"), t == "ve":
		return "视频引擎"
	case strings.Contains(t, "skin"), strings.Contains(t, "shell"):
		return surfaceLabel
	case strings.Contains(t, "gate"):
		return "SoC"
	default:
		return zoneType
	}
}

func (c *Collector) readUptime() int64 {
	fields := strings.Fields(readTrimmed(filepath.Join(c.procRoot, "uptime")))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return int64(secs)
}

func (c *Collector) readLoadAvg() []float64 {
	fields := strings.Fields(readTrimmed(filepath.Join(c.procRoot, "loadavg")))
	if len(fields) < 3 {
		return nil
	}
	out := make([]float64, 3)
	for i := range 3 {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil
		}
		out[i] = v
	}
	return out
}

// readDisk 用 Statfs 读文件系统容量（linux 与 darwin 字段同名、宽度不同，
// 显式转换两边都能编译）。失败（路径不存在等）即整节缺失。
func readDisk(path string) *DiskUsage {
	if path == "" {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	if total == 0 {
		return nil
	}
	free := st.Bfree * bsize
	d := &DiskUsage{
		Mount:      path,
		TotalBytes: total,
		AvailBytes: st.Bavail * bsize,
	}
	if free <= total {
		d.UsedBytes = total - free
	}
	return d
}

// ---- 小工具 ----

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseIntList 解析空格分隔的整数表并升序排序（related_cpus）。
func parseIntList(s string) []int {
	var out []int
	for _, f := range strings.Fields(s) {
		if v, err := strconv.Atoi(f); err == nil {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}
