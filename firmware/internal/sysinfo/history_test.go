package sysinfo

// Recorder 测试：复用 sysinfo_test 的板卡夹具，手工驱动 tick（假时钟逐分钟
// 前进、计数按公式演进），验证分钟环、10 分钟 avg+max 聚合、日文件追加、
// 停机冲刷、损坏行跳过、保留清理与 offerBase 协同。

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCountersK 写第 k 分钟的累计计数。每分钟：整机与逐核都是 10%，
// wlan0 收 100 B/s、发 50 B/s；k=6 那一分钟整机飙到 50%（验证 max 不被
// 平均抹掉）。
func (f *fixture) writeCountersK(k int) {
	extra := 0
	if k >= 6 {
		extra = 240
	}
	stat := "cpu  " + itoa(1000+60*k+extra) + " 0 0 " + itoa(9000+540*k-extra) + " 0 0 0 0 0 0\n"
	for i := 0; i < 8; i++ {
		stat += "cpu" + itoa(i) + " " + itoa(100+6*k) + " 0 0 " + itoa(900+54*k) + " 0 0 0 0 0 0\n"
	}
	f.writeProc("stat", stat)
	f.writeNet(uint64(1_000_000+6000*k), uint64(500_000+3000*k))
}

func newTestRecorder(f *fixture, days int) (*Recorder, string) {
	dataDir := f.t.TempDir()
	rec := NewRecorder(f.c, dataDir, days, slog.New(slog.DiscardHandler))
	rec.now = func() time.Time { return f.nowAt }
	return rec, filepath.Join(dataDir, historyDirName)
}

func TestRecorderArchiveAndHistory(t *testing.T) {
	f := newFixture(t)
	f.installBoard()
	rec, dir := newTestRecorder(f, 7)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 12:01 起每分钟一 tick 到 12:11：k=1 只垫基点；12:02–12:09 八个点进
	// 12:00 桶；12:10 跨窗把它落盘；12:10、12:11 两点留在当前桶。
	start := f.nowAt
	for k := 1; k <= 11; k++ {
		f.nowAt = start.Add(time.Duration(k) * time.Minute)
		f.writeCountersK(k)
		rec.tick()
	}

	todayFile := filepath.Join(dir, start.UTC().Format(dayLayout)+".jsonl")
	data, err := os.ReadFile(todayFile)
	if err != nil {
		t.Fatalf("读日文件: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("日文件行数 = %d，期望 1：%q", len(lines), lines)
	}
	var arch HistoryPoint
	if err := json.Unmarshal([]byte(lines[0]), &arch); err != nil {
		t.Fatalf("归档行解析: %v", err)
	}
	if !arch.T.Equal(start) { // 桶窗口起点 12:00
		t.Errorf("归档点时间 = %v，期望 %v", arch.T, start)
	}
	// 整机：7 个 10% + 1 个 50%（k=6）→ avg 15.0，max 50.0。
	if arch.CPU == nil || arch.CPU.Avg != 15.0 || arch.CPU.Max != 50.0 {
		t.Errorf("归档 CPU = %+v，期望 avg 15.0 / max 50.0", arch.CPU)
	}
	if len(arch.Clusters) != 2 || arch.Clusters[0].Label != "大核" ||
		arch.Clusters[0].Avg != 10.0 || arch.Clusters[1].Avg != 10.0 {
		t.Errorf("归档簇 = %+v", arch.Clusters)
	}
	if arch.Memory == nil || arch.Memory.Avg != 7.0 {
		t.Errorf("归档内存 = %+v，期望 7.0%%", arch.Memory)
	}
	var wlan *NetMetric
	for i := range arch.Net {
		if arch.Net[i].Name == "wlan0" {
			wlan = &arch.Net[i]
		}
	}
	if wlan == nil || wlan.RxAvg != 100 || wlan.RxMax != 100 || wlan.TxAvg != 50 {
		t.Errorf("归档 wlan0 = %+v", wlan)
	}
	if len(arch.Temps) == 0 || arch.Temps[0].Label != "CPU 小核" || arch.Temps[0].Avg != 54.3 {
		t.Errorf("归档温度 = %+v", arch.Temps)
	}
	if arch.Disk == nil {
		t.Error("归档磁盘缺失")
	}

	// 分钟层：12:02–12:11 十个点。
	h6 := rec.History("6h")
	if !h6.Enabled || h6.IntervalSeconds != 60 || len(h6.Points) != 10 {
		t.Fatalf("6h = enabled %v interval %d points %d", h6.Enabled, h6.IntervalSeconds, len(h6.Points))
	}
	if !h6.Points[0].T.Equal(start.Add(2 * time.Minute)) {
		t.Errorf("6h 首点 = %v", h6.Points[0].T)
	}
	// 归档层查询附带未落盘桶：1 行文件 + 当前桶（12:10、12:11 → avg 10）。
	h7 := rec.History("7d")
	if h7.IntervalSeconds != 600 || h7.RetentionDays != 7 || len(h7.Points) != 2 {
		t.Fatalf("7d = interval %d retention %d points %d", h7.IntervalSeconds, h7.RetentionDays, len(h7.Points))
	}
	if h7.Points[1].CPU == nil || h7.Points[1].CPU.Avg != 10.0 {
		t.Errorf("7d 未落盘桶 = %+v", h7.Points[1].CPU)
	}

	// offerBase 协同：Recorder 刚喂过基点，即时快照不等采样窗。
	if _, err := f.c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sleeps) != 0 {
		t.Errorf("有 Recorder 基点时快照不应等采样窗：%v", f.sleeps)
	}

	// 停机冲刷：当前桶落盘成第二行。
	rec.flushPending()
	data, _ = os.ReadFile(todayFile)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("冲刷后行数 = %d，期望 2", len(lines))
	}
	var tail HistoryPoint
	if err := json.Unmarshal([]byte(lines[1]), &tail); err != nil || tail.CPU.Avg != 10.0 {
		t.Errorf("冲刷行 = %+v（err %v）", tail.CPU, err)
	}

	// 损坏行（掉电截断）只跳过自身。
	fh, _ := os.OpenFile(todayFile, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString("{broken\n")
	fh.Close()
	if got := rec.History("7d"); len(got.Points) != 2 {
		t.Errorf("含损坏行的 7d 点数 = %d，期望 2", len(got.Points))
	}
}

func TestRecorderRetentionCleanup(t *testing.T) {
	f := newFixture(t)
	rec, dir := newTestRecorder(f, 7)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, f.nowAt.AddDate(0, 0, -20).UTC().Format(dayLayout)+".jsonl")
	edge := filepath.Join(dir, f.nowAt.AddDate(0, 0, -7).UTC().Format(dayLayout)+".jsonl")
	bogus := filepath.Join(dir, "readme.txt")
	for _, p := range []string{old, edge, bogus} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec.cleanup(f.nowAt)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("过期文件未删除")
	}
	if _, err := os.Stat(edge); err != nil {
		t.Error("保留窗口边界文件被误删")
	}
	if _, err := os.Stat(bogus); err != nil {
		t.Error("非历史文件被误删")
	}
}

func TestRecorderDisabled(t *testing.T) {
	f := newFixture(t)
	f.installBoard()
	rec, dir := newTestRecorder(f, 0)

	// Run 直接返回（ctx 未取消也一样），不建目录、不采样。
	done := make(chan struct{})
	go func() { rec.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("关闭态 Run 未立即返回")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("关闭态不应创建历史目录")
	}
	h := rec.History("7d")
	if h.Enabled || len(h.Points) != 0 {
		t.Errorf("关闭态 History = %+v", h)
	}
}
