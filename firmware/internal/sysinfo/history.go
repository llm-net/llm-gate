package sysinfo

// 设备状态历史（Recorder）：为「保留几天趋势」而生，设计围绕 SD 卡的写入
// 磨损与空间占用（这块板的上一张卡就是写坏的）：
//
//   - 采样：每 interval（60s）读一次计数，与上一次做差得到该分钟的均值点。
//     只读十几个 /proc、/sys 虚拟小文件，不 exec、不阻塞（无采样窗）。
//   - 分钟层：最近 ringN（360 = 6h）个分钟点只存内存（约百余 KB），
//     进程退出即弃——高频层绝不落盘。
//   - 归档层：每 bucketSpan（10 分钟）把桶内分钟点聚合成一条 avg+max 记录
//     （峰值不被平均抹掉），追加写入 <data_dir>/history/YYYY-MM-DD.jsonl。
//     一天 144 行、每行数百字节：逻辑写 ≈ 90 KB/天，7 天全量 < 1 MB。
//   - 落盘方式：O_APPEND 顺序追加、不调 fsync（内核自行合并回写，掉电最多
//     丢一桶监控数据），无改写、无重命名——这是对 flash 最温和的写法。
//   - 保留：按日文件整只删除，天数来自 config history_days（0 = 连采样带
//     记录整个关闭，回到「零后台成本」）。
//   - 协同：每次采样把原始计数喂给 Collector 作差值基点（offerBase），
//     即时页冷打开不再等 500ms 采样窗。
//
// 读路径（管理员点开历史面板）才读文件，平时只有追加；单行损坏（掉电截断）
// 解析跳过，不传染整个文件。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultHistoryInterval 是分钟层采样周期。
	DefaultHistoryInterval = time.Minute
	// historyRingN 是分钟层容量：6 小时。
	historyRingN = 360
	// historyBucketSpan 是归档聚合窗口。
	historyBucketSpan = 10 * time.Minute
	// historyFileMax 是单个日文件的兜底上限（正常一天 ~90 KB，超限说明
	// 出了 bug，宁可停写也不吃满卡）。
	historyFileMax = 1 << 20
	// historyDirName 位于 data_dir 下。
	historyDirName = "history"
	dayLayout      = "2006-01-02"
)

// Metric 是一段时间内某读数的均值与峰值。分钟点两者相等（一分钟就是一个
// 差值）；归档点由桶内分钟点聚合而来。
type Metric struct {
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// LabeledMetric 是带名字的读数（CPU 簇、温度区）。Surface 只对温度区有意义：
// 机身表面那一路为 true（与 TempZone.Surface 同源），Label 会按语言本地化，
// 界面靠它排除机身表面。
type LabeledMetric struct {
	Label   string `json:"label" i18n:"text"`
	Surface bool   `json:"surface,omitempty"`
	Metric
}

// NetMetric 是一块网卡的收发速率（字节/秒）。
type NetMetric struct {
	Name  string  `json:"name"`
	RxAvg float64 `json:"rx_avg"`
	RxMax float64 `json:"rx_max"`
	TxAvg float64 `json:"tx_avg"`
	TxMax float64 `json:"tx_max"`
}

// HistoryPoint 是历史序列的一个点：分钟层与归档层同构（内存占用类指标记
// 百分比，绝对量即时页有）。T 是归档点所在窗口的起点。各节读不到即缺省，
// 与 Snapshot 同一容错口径。
type HistoryPoint struct {
	T        time.Time       `json:"t"`
	CPU      *Metric         `json:"cpu,omitempty"`
	Clusters []LabeledMetric `json:"clusters,omitempty"`
	Memory   *Metric         `json:"memory_percent,omitempty"`
	Swap     *Metric         `json:"swap_percent,omitempty"`
	Net      []NetMetric     `json:"net,omitempty"`
	Temps    []LabeledMetric `json:"temps,omitempty"`
	Disk     *Metric         `json:"disk_percent,omitempty"`
}

// empty 报告该点没有任何读数（无 /proc 的开发机）：既不进环也不落盘。
func (p *HistoryPoint) empty() bool {
	return p.CPU == nil && p.Memory == nil && len(p.Net) == 0 &&
		len(p.Temps) == 0 && p.Disk == nil
}

// HistoryResult 是历史查询响应体。
type HistoryResult struct {
	Enabled         bool           `json:"enabled"`
	Range           string         `json:"range"`
	IntervalSeconds int            `json:"interval_seconds"`
	RetentionDays   int            `json:"retention_days"`
	Points          []HistoryPoint `json:"points"`
}

// Recorder 是后台历史记录器。零值不可用，必须经 NewRecorder 构造；
// days <= 0 时完全停摆（Run 直接返回，不采样、不写盘）。
type Recorder struct {
	c        *Collector
	dir      string
	days     int
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	prev      *rawSample
	ring      []HistoryPoint // 分钟层，头旧尾新
	bucket    []HistoryPoint // 当前归档桶
	bucketKey time.Time      // 桶窗口起点（UTC，Truncate(bucketSpan)）

	lastCleanupDay string
	persistFailed  bool // 写失败只告警一次，恢复时再报一次
}

// NewRecorder 构造历史记录器；dataDir 与 store 同目录，days 为保留天数
// （0 = 关闭）。
func NewRecorder(c *Collector, dataDir string, days int, logger *slog.Logger) *Recorder {
	return &Recorder{
		c:        c,
		dir:      filepath.Join(dataDir, historyDirName),
		days:     days,
		log:      logger.With("srv", "sysinfo"),
		interval: DefaultHistoryInterval,
		now:      time.Now,
	}
}

// Enabled 报告历史记录是否开启。
func (r *Recorder) Enabled() bool { return r.days > 0 }

// Run 驱动采样循环直到 ctx 取消；退出前把未满窗口的桶落盘。关闭态直接
// 返回——不留任何后台成本。
func (r *Recorder) Run(ctx context.Context) {
	if !r.Enabled() {
		r.log.Info("设备状态历史已关闭（history_days: 0），不启动采样")
		return
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		// 目录建不出来就只剩内存分钟层；不拖垮 gatewayd。
		r.log.Warn("历史目录创建失败，仅保留内存层", "dir", r.dir, "err", err.Error())
	}
	r.cleanup(r.now())
	r.log.Info("设备状态历史采样已启动",
		"interval", r.interval.String(), "retention_days", r.days, "dir", r.dir)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushPending()
			r.log.Info("设备状态历史采样已停止")
			return
		case <-t.C:
			r.tick()
		}
	}
}

// tick 采一个分钟点：与上一次计数做差 → 入分钟环 → 跨过 10 分钟窗口边界
// 时把上一桶聚合落盘。文件 IO 在锁外。
func (r *Recorder) tick() {
	cur := r.c.readRaw()
	// 顺手喂即时页：管理员冷打开「设备状态」时有 ≤1 分钟龄的基点可用，
	// 不必现场等采样窗。
	r.c.offerBase(cur)

	r.mu.Lock()
	prev := r.prev
	r.prev = cur
	if prev == nil || !cur.at.After(prev.at) {
		r.mu.Unlock()
		return
	}
	point := snapshotToPoint(r.c.build(prev, cur))
	if point == nil {
		r.mu.Unlock()
		return
	}
	// 分钟环：满了滑动，不增长。
	if len(r.ring) >= historyRingN {
		copy(r.ring, r.ring[1:])
		r.ring[len(r.ring)-1] = *point
	} else {
		r.ring = append(r.ring, *point)
	}
	// 归档桶按墙钟窗口划分（错过若干 tick 也不会错位）。
	key := point.T.UTC().Truncate(historyBucketSpan)
	var flushKey time.Time
	var flush *HistoryPoint
	if r.bucketKey.IsZero() {
		r.bucketKey = key
	} else if !key.Equal(r.bucketKey) {
		flush = aggregatePoints(r.bucketKey, r.bucket)
		flushKey = r.bucketKey
		r.bucket = r.bucket[:0]
		r.bucketKey = key
	}
	r.bucket = append(r.bucket, *point)
	r.mu.Unlock()

	if flush != nil {
		r.persist(flushKey, flush)
	}
}

// flushPending 把当前未满窗口的桶聚合落盘（优雅停机路径；掉电则丢弃，
// 这是「不 fsync」的约定成本）。
func (r *Recorder) flushPending() {
	r.mu.Lock()
	var flush *HistoryPoint
	var key time.Time
	if len(r.bucket) > 0 {
		flush = aggregatePoints(r.bucketKey, r.bucket)
		key = r.bucketKey
		r.bucket = r.bucket[:0]
		r.bucketKey = time.Time{}
	}
	r.mu.Unlock()
	if flush != nil {
		r.persist(key, flush)
	}
}

// persist 追加一行归档点到 key 所在日期的文件；跨天时先做保留清理。
func (r *Recorder) persist(key time.Time, p *HistoryPoint) {
	if !r.Enabled() {
		return
	}
	day := key.UTC().Format(dayLayout)
	if day != r.lastCleanupDay {
		r.cleanup(key)
		r.lastCleanupDay = day
	}
	path := filepath.Join(r.dir, day+".jsonl")
	if st, err := os.Stat(path); err == nil && st.Size() > historyFileMax {
		r.warnPersist(fmt.Errorf("日文件超过 %d 字节上限，停止追加（正常一天约 90 KB，超限属缺陷）", historyFileMax))
		return
	}
	line, err := json.Marshal(p)
	if err != nil {
		r.warnPersist(err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.warnPersist(err)
		return
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		r.warnPersist(werr)
		return
	}
	if r.persistFailed {
		r.persistFailed = false
		r.log.Info("历史归档写入已恢复", "dir", r.dir)
	}
}

// warnPersist 写失败告警（只报状态翻转，不每 10 分钟刷一条）。
func (r *Recorder) warnPersist(err error) {
	if r.persistFailed {
		return
	}
	r.persistFailed = true
	r.log.Warn("历史归档写入失败，内存分钟层不受影响", "err", err.Error())
}

// cleanup 删除超出保留天数的日文件。文件名即日期，比较不解析内容；
// 认不出名字的文件不碰。
func (r *Recorder) cleanup(now time.Time) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	oldest := now.UTC().AddDate(0, 0, -r.days).Format(dayLayout)
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok {
			continue
		}
		if _, err := time.Parse(dayLayout, day); err != nil {
			continue
		}
		if day < oldest {
			if err := os.Remove(filepath.Join(r.dir, e.Name())); err != nil {
				r.log.Warn("历史过期文件删除失败", "file", e.Name(), "err", err.Error())
			} else {
				r.log.Info("历史过期文件已删除", "file", e.Name())
			}
		}
	}
}

// History 返回一段历史序列。"6h" 取内存分钟层；"7d" 读日文件并附上尚未
// 落盘的当前桶（视图新鲜到分钟）。只在管理员查询时才有读 IO。
// Points 恒为数组（空也序列化成 []，不是 null）——前端按数组遍历。
func (r *Recorder) History(rng string) HistoryResult {
	res := HistoryResult{
		Enabled: r.Enabled(), Range: rng, RetentionDays: r.days,
		Points: []HistoryPoint{},
	}
	if !r.Enabled() {
		return res
	}
	now := r.now()
	switch rng {
	case "6h":
		res.IntervalSeconds = int(r.interval / time.Second)
		cutoff := now.Add(-6 * time.Hour)
		r.mu.Lock()
		for _, p := range r.ring {
			if p.T.After(cutoff) {
				res.Points = append(res.Points, p)
			}
		}
		r.mu.Unlock()
	case "7d":
		res.IntervalSeconds = int(historyBucketSpan / time.Second)
		cutoff := now.AddDate(0, 0, -r.days)
		res.Points = append(res.Points, r.loadFiles(cutoff, now)...)
		r.mu.Lock()
		if len(r.bucket) > 0 {
			if p := aggregatePoints(r.bucketKey, r.bucket); p != nil {
				res.Points = append(res.Points, *p)
			}
		}
		r.mu.Unlock()
	}
	return res
}

// loadFiles 读保留窗口内的日文件。损坏行（掉电截断、并发追加的半行）跳过。
// 逐行扫描而不是整读后 Split：7 天档案有几 MB，整读 + string 转换 + 行切片
// 头是三份瞬时拷贝，这台板子的内存犯不上（json.Unmarshal 不留存入参切片）。
func (r *Recorder) loadFiles(cutoff, now time.Time) []HistoryPoint {
	var out []HistoryPoint
	for day := cutoff.UTC().Truncate(24 * time.Hour); !day.After(now.UTC()); day = day.AddDate(0, 0, 1) {
		f, err := os.Open(filepath.Join(r.dir, day.Format(dayLayout)+".jsonl"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		// 一个历史点行只有几百字节；1 MiB 上限只为让「病态长行中止本文件
		// 扫描」在实践中不可能发生（与损坏行跳过同一容错方向）。
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var p HistoryPoint
			if err := json.Unmarshal(line, &p); err != nil || p.T.IsZero() {
				continue
			}
			// 没有 surface 字段的旧点按源文案补上标记。
			for i := range p.Temps {
				if p.Temps[i].Label == surfaceLabel {
					p.Temps[i].Surface = true
				}
			}
			if p.T.After(cutoff) {
				out = append(out, p)
			}
		}
		f.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

// snapshotToPoint 把一份快照压成历史点（分钟层：avg == max）。
func snapshotToPoint(s *Snapshot) *HistoryPoint {
	p := &HistoryPoint{T: s.SampledAt}
	if s.CPU != nil {
		p.CPU = &Metric{Avg: s.CPU.OverallPercent, Max: s.CPU.OverallPercent}
		for _, cl := range s.CPU.Clusters {
			p.Clusters = append(p.Clusters, LabeledMetric{
				Label: cl.Label, Metric: Metric{Avg: cl.Percent, Max: cl.Percent},
			})
		}
	}
	if m := s.Memory; m != nil && m.TotalBytes > 0 {
		mp := round1(100 * float64(m.UsedBytes) / float64(m.TotalBytes))
		p.Memory = &Metric{Avg: mp, Max: mp}
		if m.SwapTotalBytes > 0 {
			sp := round1(100 * float64(m.SwapUsedBytes) / float64(m.SwapTotalBytes))
			p.Swap = &Metric{Avg: sp, Max: sp}
		}
	}
	for _, ni := range s.Network {
		p.Net = append(p.Net, NetMetric{
			Name:  ni.Name,
			RxAvg: ni.RxBytesPerSec, RxMax: ni.RxBytesPerSec,
			TxAvg: ni.TxBytesPerSec, TxMax: ni.TxBytesPerSec,
		})
	}
	for _, t := range s.Temperatures {
		p.Temps = append(p.Temps, LabeledMetric{
			Label: t.Label, Surface: t.Surface, Metric: Metric{Avg: t.Celsius, Max: t.Celsius},
		})
	}
	if d := s.Disk; d != nil && d.TotalBytes > 0 {
		dp := round1(100 * float64(d.UsedBytes) / float64(d.TotalBytes))
		p.Disk = &Metric{Avg: dp, Max: dp}
	}
	if p.empty() {
		return nil
	}
	return p
}

// aggregatePoints 把一桶分钟点聚合为一个归档点（均值取等权平均，峰值取
// 最大）。簇/温度按 Label、网卡按 Name 对齐；某分钟缺某读数就按出现次数
// 平均，不掺零。
func aggregatePoints(key time.Time, points []HistoryPoint) *HistoryPoint {
	if len(points) == 0 {
		return nil
	}
	agg := &HistoryPoint{T: key}
	agg.CPU = mergeMetric(points, func(p *HistoryPoint) *Metric { return p.CPU })
	agg.Memory = mergeMetric(points, func(p *HistoryPoint) *Metric { return p.Memory })
	agg.Swap = mergeMetric(points, func(p *HistoryPoint) *Metric { return p.Swap })
	agg.Disk = mergeMetric(points, func(p *HistoryPoint) *Metric { return p.Disk })
	agg.Clusters = mergeLabeled(points, func(p *HistoryPoint) []LabeledMetric { return p.Clusters })
	agg.Temps = mergeLabeled(points, func(p *HistoryPoint) []LabeledMetric { return p.Temps })
	agg.Net = mergeNet(points)
	if agg.empty() {
		return nil
	}
	return agg
}

func mergeMetric(points []HistoryPoint, pick func(*HistoryPoint) *Metric) *Metric {
	var sum, max float64
	n := 0
	for i := range points {
		m := pick(&points[i])
		if m == nil {
			continue
		}
		sum += m.Avg
		if n == 0 || m.Max > max {
			max = m.Max
		}
		n++
	}
	if n == 0 {
		return nil
	}
	return &Metric{Avg: round1(sum / float64(n)), Max: round1(max)}
}

// mergeLabeled 按 Label 聚合，输出顺序取首次出现顺序（与采样顺序一致）。
func mergeLabeled(points []HistoryPoint, pick func(*HistoryPoint) []LabeledMetric) []LabeledMetric {
	type acc struct {
		sum, max float64
		n        int
		surface  bool
	}
	var order []string
	accs := make(map[string]*acc)
	for i := range points {
		for _, m := range pick(&points[i]) {
			a := accs[m.Label]
			if a == nil {
				a = &acc{}
				accs[m.Label] = a
				order = append(order, m.Label)
			}
			a.surface = a.surface || m.Surface
			a.sum += m.Avg
			if a.n == 0 || m.Max > a.max {
				a.max = m.Max
			}
			a.n++
		}
	}
	out := make([]LabeledMetric, 0, len(order))
	for _, label := range order {
		a := accs[label]
		out = append(out, LabeledMetric{
			Label:   label,
			Surface: a.surface,
			Metric:  Metric{Avg: round1(a.sum / float64(a.n)), Max: round1(a.max)},
		})
	}
	return out
}

func mergeNet(points []HistoryPoint) []NetMetric {
	type acc struct {
		rxSum, rxMax, txSum, txMax float64
		n                          int
	}
	var order []string
	accs := make(map[string]*acc)
	for i := range points {
		for _, m := range points[i].Net {
			a := accs[m.Name]
			if a == nil {
				a = &acc{}
				accs[m.Name] = a
				order = append(order, m.Name)
			}
			a.rxSum += m.RxAvg
			a.txSum += m.TxAvg
			if a.n == 0 || m.RxMax > a.rxMax {
				a.rxMax = m.RxMax
			}
			if a.n == 0 || m.TxMax > a.txMax {
				a.txMax = m.TxMax
			}
			a.n++
		}
	}
	out := make([]NetMetric, 0, len(order))
	for _, name := range order {
		a := accs[name]
		out = append(out, NetMetric{
			Name:  name,
			RxAvg: math.Round(a.rxSum / float64(a.n)), RxMax: a.rxMax,
			TxAvg: math.Round(a.txSum / float64(a.n)), TxMax: a.txMax,
		})
	}
	return out
}
