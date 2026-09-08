// Package sysinfo 按需采集设备运行状态（CPU 分大小核、内存、网络、温度、
// 存储），供管理面「设备状态」页读数。
//
// 资源纪律（本包的存在理由之一是省资源，与管理 UI 的「零轮询」约定配套）：
//   - 无后台 goroutine、无定时器：不打开页面就一个字节都不读；
//   - 快照带短缓存（cacheTTL）：多名管理员同时刷新也只采样一次；
//   - CPU 占用与网速是两个时间点的计数差：上一次调用留下的计数在 reuseMax
//     内直接当基点（刷新即时返回），过期才现场等一个 window 采样窗；
//   - 单次采样只读十几个内核虚拟小文件（/proc、/sys），不 exec 外部命令；
//   - 拓扑（簇划分、核型号、最高频率）开机后不变，进程内只解析一次。
//
// 容错口径：读不到的项从快照里省略（对应 JSON 字段缺省），不视为错误——
// 开发机 darwin 没有 /proc，快照就只剩采样时间戳；错误只在 ctx 取消时返回。
package sysinfo

import (
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

// 采样节奏参数。cacheTTL 必须小于 reuseMax，否则缓存过期后上一份计数也一定
// 过期，「刷新即时返回」永远走不到。
const (
	defaultWindow   = 500 * time.Millisecond // 无历史基点时的现场采样窗
	defaultCacheTTL = 2 * time.Second        // 快照结果缓存：连点刷新只算一次
	// defaultReuseMax 盖过 Recorder 的采样周期（60s）：历史记录器开着时
	// 每分钟经 offerBase 喂来新基点，即时页永远有 ≤1 分钟龄的基点可用，
	// 冷打开也不必等采样窗；≤90s 的差值窗口作「当前占用」仍然诚实。
	defaultReuseMax = 90 * time.Second
)

// Snapshot 是一次设备运行状态读数。除 SampledAt/WindowMS 外的所有节均为
// 「读得到才有」：字段为 nil/空即该平台不提供（darwin 开发机、精简内核等）。
type Snapshot struct {
	SampledAt time.Time `json:"sampled_at"`
	// WindowMS 是本次差值计算的实际窗口毫秒数（现场采样约为 window，
	// 复用上次计数时为距上次采样的间隔）。
	WindowMS      int64      `json:"window_ms"`
	UptimeSeconds int64      `json:"uptime_seconds,omitempty"`
	LoadAvg       []float64  `json:"load_avg,omitempty"` // 1/5/15 分钟
	CPU           *CPUStatus `json:"cpu,omitempty"`
	Memory        *MemStatus `json:"memory,omitempty"`
	Network       []NetIface `json:"network,omitempty"`
	Temperatures  []TempZone `json:"temperatures,omitempty"`
	Disk          *DiskUsage `json:"disk,omitempty"`
}

// CPUStatus 是整机与分簇的 CPU 占用。占用率口径：busy = total−idle−iowait，
// 百分比按两采样点的 tick 差计算，保留一位小数。
type CPUStatus struct {
	OverallPercent float64   `json:"overall_percent"`
	Clusters       []Cluster `json:"clusters"`
}

// Cluster 是一个 cpufreq 簇（big.LITTLE 的大核/小核各为一簇；无 cpufreq 的
// 平台整机归入一簇）。Label 是用户可见名（大核/小核/CPU…），CoreModel 是
// 核心型号（如 Cortex-A76，识别不出则缺省）。
type Cluster struct {
	Label      string  `json:"label" i18n:"text"`
	CoreModel  string  `json:"core_model,omitempty"`
	Percent    float64 `json:"percent"`
	CurFreqMHz int64   `json:"cur_freq_mhz,omitempty"`
	MaxFreqMHz int64   `json:"max_freq_mhz,omitempty"`
	Cores      []Core  `json:"cores"`
}

// Core 是单个逻辑 CPU 的占用。
type Core struct {
	CPU     int     `json:"cpu"`
	Percent float64 `json:"percent"`
}

// MemStatus 是内存与交换区用量（字节）。Used = Total−Available（内核对
// 「还能分配多少」的口径），BuffCache = Buffers+Cached，可回收、不算占用。
type MemStatus struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	BuffCacheBytes uint64 `json:"buff_cache_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes"`
}

// NetIface 是一块物理网卡的状态与速率。只收录 /sys/class/net/<名>/device
// 存在的接口（真实硬件），lo/sit0 之类的内核虚拟设备不出现。
type NetIface struct {
	Name          string  `json:"name"`
	Up            bool    `json:"up"`
	RxBytesPerSec float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec"`
	RxTotalBytes  uint64  `json:"rx_total_bytes"`
	TxTotalBytes  uint64  `json:"tx_total_bytes"`
}

// TempZone 是一路温度读数；Label 已按本机常见 zone 类型译好（CPU 大核/
// CPU 小核/GPU/NPU/DDR/机身表面），认不出的类型原样透出。Surface 标记机身表面
// 那一路：Label 回给界面时会按语言本地化，界面要「排除机身表面」只能认这个
// 与语言无关的标记，不能比对文案。
type TempZone struct {
	Label   string  `json:"label" i18n:"text"`
	Celsius float64 `json:"celsius"`
	Surface bool    `json:"surface,omitempty"`
}

// DiskUsage 是一个文件系统的容量用量（字节）。Used = Total−Free（含 root
// 预留块，与 df 的 Used 同口径）；Avail 是非特权进程还能写入的量。
type DiskUsage struct {
	Mount      string `json:"mount"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	AvailBytes uint64 `json:"avail_bytes"`
}

// Collector 按需采集快照，可并发调用（内部串行，后到者直接吃缓存）。
// 零值不可用，必须经 NewCollector 构造。
type Collector struct {
	procRoot string // "/proc"；测试注入夹具目录
	sysRoot  string // "/sys"
	diskPath string // statfs 目标；板上根分区即整张 SD 卡
	window   time.Duration
	cacheTTL time.Duration
	reuseMax time.Duration
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error

	mu       sync.Mutex
	topoOnce sync.Once
	topo     []clusterMeta
	prev     *rawSample // 上一次采样的原始计数，作下次差值基点
	last     *Snapshot
	lastAt   time.Time
}

// NewCollector 构造读取真实 /proc、/sys 的采集器。
func NewCollector() *Collector {
	return &Collector{
		procRoot: "/proc",
		sysRoot:  "/sys",
		diskPath: "/",
		window:   defaultWindow,
		cacheTTL: defaultCacheTTL,
		reuseMax: defaultReuseMax,
		now:      time.Now,
		sleep:    ctxSleep,
	}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// offerBase 把一份外部采到的原始计数（Recorder 每分钟的采样）存作差值
// 基点。只在比现有基点新时替换；Snapshot 随后照常按 reuseMax 判断可用性。
func (c *Collector) offerBase(s *rawSample) {
	c.mu.Lock()
	if c.prev == nil || s.at.After(c.prev.at) {
		c.prev = s
	}
	c.mu.Unlock()
}

// Snapshot 返回当前设备状态。缓存命中直接返回上一份；无可复用计数基点时
// 会阻塞约 window 采一次样。唯一的错误来源是 ctx 在采样窗内被取消。
func (c *Collector) Snapshot(ctx context.Context) (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.last != nil && c.now().Sub(c.lastAt) < c.cacheTTL {
		return c.last, nil
	}

	// 差值基点：优先复用上次采样的计数；太老（占用率被摊平成长期均值）或
	// 没有则现场读一份、等一个采样窗再读第二份。基点全空（无 /proc 的平台）
	// 时没有可差的计数，窗口白等，直接跳过。
	base := c.prev
	if base != nil && c.now().Sub(base.at) > c.reuseMax {
		base = nil
	}
	if base == nil {
		first := c.readRaw()
		if len(first.perCPU) > 0 || len(first.net) > 0 {
			if err := c.sleep(ctx, c.window); err != nil {
				return nil, err
			}
		}
		base = first
	}
	cur := c.readRaw()
	c.prev = cur

	snap := c.build(base, cur)
	c.last = snap
	c.lastAt = cur.at
	return snap, nil
}

// build 由基点与当前计数装配快照，并补齐即时读数（内存/温度/频率/负载/
// 运行时长/磁盘）。Snapshot 与 Recorder.tick 都走这里，簇拓扑在首次装配时
// 解析一次。
func (c *Collector) build(base, cur *rawSample) *Snapshot {
	c.topoOnce.Do(func() { c.topo = c.readTopology() })
	snap := &Snapshot{SampledAt: cur.at}
	elapsed := cur.at.Sub(base.at)
	snap.WindowMS = elapsed.Milliseconds()

	snap.UptimeSeconds = c.readUptime()
	snap.LoadAvg = c.readLoadAvg()
	snap.CPU = c.buildCPU(base, cur)
	snap.Memory = c.readMeminfo()
	snap.Network = c.buildNet(base, cur, elapsed)
	snap.Temperatures = c.readThermal()
	snap.Disk = readDisk(c.diskPath)
	return snap
}

// buildCPU 计算整机与分簇占用。当前采样没有 per-CPU 计数（无 /proc 平台）
// 即整节省略。不在任何 cpufreq 簇里的 CPU 兜底归入一个无频率信息的「CPU」簇
// （x86 开发机、精简内核）。
func (c *Collector) buildCPU(base, cur *rawSample) *CPUStatus {
	if len(cur.perCPU) == 0 {
		return nil
	}
	st := &CPUStatus{OverallPercent: busyPercent(base.agg, cur.agg)}
	seen := make(map[int]bool)
	for _, meta := range c.topo {
		cl := Cluster{
			Label:      meta.label,
			CoreModel:  meta.coreModel,
			MaxFreqMHz: meta.maxFreqKHz / 1000,
			CurFreqMHz: c.readCurFreqMHz(meta),
		}
		var baseSum, curSum cpuTicks
		for _, id := range meta.cpus {
			seen[id] = true
			bt, ct := base.perCPU[id], cur.perCPU[id]
			baseSum, curSum = baseSum.add(bt), curSum.add(ct)
			cl.Cores = append(cl.Cores, Core{CPU: id, Percent: busyPercent(bt, ct)})
		}
		cl.Percent = busyPercent(baseSum, curSum)
		st.Clusters = append(st.Clusters, cl)
	}
	var restIDs []int
	for id := range cur.perCPU {
		if !seen[id] {
			restIDs = append(restIDs, id)
		}
	}
	if len(restIDs) > 0 {
		slices.Sort(restIDs)
		label := "CPU"
		if len(st.Clusters) > 0 {
			label = "其他核"
		}
		cl := Cluster{Label: label}
		var baseSum, curSum cpuTicks
		for _, id := range restIDs {
			bt, ct := base.perCPU[id], cur.perCPU[id]
			baseSum, curSum = baseSum.add(bt), curSum.add(ct)
			cl.Cores = append(cl.Cores, Core{CPU: id, Percent: busyPercent(bt, ct)})
		}
		cl.Percent = busyPercent(baseSum, curSum)
		st.Clusters = append(st.Clusters, cl)
	}
	return st
}

// buildNet 计算网卡速率。计数回绕（网卡重置/驱动重载）按 0 处理，不产生
// 荒谬的负速率。
func (c *Collector) buildNet(base, cur *rawSample, elapsed time.Duration) []NetIface {
	if len(cur.net) == 0 {
		return nil
	}
	names := make([]string, 0, len(cur.net))
	for name := range cur.net {
		names = append(names, name)
	}
	slices.Sort(names)
	secs := elapsed.Seconds()
	out := make([]NetIface, 0, len(names))
	for _, name := range names {
		nc := cur.net[name]
		iface := NetIface{
			Name:         name,
			Up:           c.readOperstateUp(name),
			RxTotalBytes: nc.rx,
			TxTotalBytes: nc.tx,
		}
		if bc, ok := base.net[name]; ok && secs > 0 {
			iface.RxBytesPerSec = ratePerSec(bc.rx, nc.rx, secs)
			iface.TxBytesPerSec = ratePerSec(bc.tx, nc.tx, secs)
		}
		out = append(out, iface)
	}
	return out
}

// busyPercent 按两点 tick 差算占用率（一位小数，钳在 0–100）。
func busyPercent(a, b cpuTicks) float64 {
	total := b.total() - a.total()
	if total <= 0 {
		return 0
	}
	busy := total - (b.idle - a.idle) - (b.iowait - a.iowait)
	return round1(math.Min(100, math.Max(0, 100*float64(busy)/float64(total))))
}

func ratePerSec(from, to uint64, secs float64) float64 {
	if to < from { // 计数回绕
		return 0
	}
	return math.Round(float64(to-from) / secs)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
