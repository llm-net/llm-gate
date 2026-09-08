package mihomo

// 节点延迟：管理员在内核卡上手动发起，设备并发直连各节点服务器测 TCP 握手耗时（含域名
// 解析）。这是选节点用的参考读数，与内核运行中「自动选择」组内部按完整代理链路做的
// url-test 是两个口径；不要求组件已安装或内核在跑，也不经出站代理（内核到节点本来就是
// 直连）。结果只留在内存，不落盘；节点服务器地址是订阅数据（§15.1），拨号错误不出类别
// 之外的任何内容，日志只记数量。

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// latencyDialTimeout 是单个节点的拨号时限；latencyBudget 是整轮测试的总预算。
	latencyDialTimeout = 5 * time.Second
	latencyBudget      = 60 * time.Second
	latencyConcurrency = 64
)

// ErrLatencyBusy 表示已有一轮延迟测试在进行。
var ErrLatencyBusy = errors.New("节点延迟测试进行中，请稍候")

// NodeLatency 是单个节点的最近一次测试结果。
type NodeLatency struct {
	MS     int64
	Failed bool
}

// LatencySummary 是一轮测试的汇总（不含节点地址）。
type LatencySummary struct {
	Total     int
	Reachable int
}

// TestLatency 对全部节点测一轮 TCP 连接延迟，结果并进 Status 的节点读数。
func (m *Manager) TestLatency(ctx context.Context) (*LatencySummary, error) {
	nodes, err := m.loadNodes()
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, ErrNoNodes
	}
	if !m.latencyMu.TryLock() {
		return nil, ErrLatencyBusy
	}
	defer m.latencyMu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, latencyBudget)
	defer cancel()
	dialer := &net.Dialer{Timeout: latencyDialTimeout}
	results := make([]NodeLatency, len(nodes))
	sem := make(chan struct{}, latencyConcurrency)
	var wg sync.WaitGroup
	for i := range nodes {
		addr, ok := nodeDialAddr(nodes[i])
		if !ok {
			results[i] = NodeLatency{Failed: true}
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			conn, err := dialer.DialContext(dctx, "tcp", addr)
			if err != nil {
				results[i] = NodeLatency{Failed: true}
				return
			}
			conn.Close()
			ms := time.Since(start).Milliseconds()
			if ms < 1 {
				ms = 1
			}
			results[i] = NodeLatency{MS: ms}
		}(i, addr)
	}
	wg.Wait()
	latency := make(map[string]NodeLatency, len(nodes))
	sum := &LatencySummary{Total: len(nodes)}
	for i, n := range nodes {
		latency[n.Name] = results[i]
		if !results[i].Failed {
			sum.Reachable++
		}
	}
	m.mu.Lock()
	m.latency = latency
	m.latencyAt = m.opt.Now()
	m.mu.Unlock()
	m.logger.Info("节点延迟测试完成", "nodes", sum.Total, "reachable", sum.Reachable)
	return sum, nil
}

// nodeDialAddr 取节点的 server:port（只用于拨号，不进日志、错误与响应）。
func nodeDialAddr(n Node) (string, bool) {
	server, _ := n.Raw["server"].(string)
	server = strings.TrimSpace(server)
	port, ok := portOf(n.Raw["port"])
	if server == "" || !ok {
		return "", false
	}
	return net.JoinHostPort(server, strconv.Itoa(port)), true
}
