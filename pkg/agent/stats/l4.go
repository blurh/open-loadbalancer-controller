// Package stats 提供转发器无关的连接数采集能力。
// 四层(TCP)连接数基于内核的 /proc/net/tcp,与具体转发器(nginx/lvs/...)无关,
// 因此在此统一实现,由各 agent 提供其监听端口即可。
package stats

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
)

// L4Collector 从 /proc/net/tcp 采集四层 TCP 连接数,
// 仅统计 portsFunc 返回的监听端口上的连接,避免把七层等其他连接算进来。
type L4Collector struct {
	// portsFunc 动态返回当前需要统计的 TCP 监听端口列表(由具体 agent 提供)
	portsFunc func() []int
	interval  time.Duration
}

// NewL4Collector 创建四层连接数采集器。
// portsFunc 返回当前监听的 TCP 端口列表;返回空则该轮四层指标清零。
func NewL4Collector(portsFunc func() []int, interval time.Duration) *L4Collector {
	return &L4Collector{
		portsFunc: portsFunc,
		interval:  interval,
	}
}

// Start 启动采集循环(阻塞)。
func (lc *L4Collector) Start(ctx context.Context) {
	ticker := time.NewTicker(lc.interval)
	defer ticker.Stop()

	// 启动时立即采集一次
	lc.collect()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lc.collect()
		}
	}
}

// collect 采集一次四层连接数并更新 metrics。
func (lc *L4Collector) collect() {
	ports := lc.portsFunc()
	if len(ports) == 0 {
		// 无监听端口,清零四层指标
		metrics.SetConnectionsByLayer("l4", 0, 0, 0)
		return
	}

	portSet := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		portSet[p] = struct{}{}
	}

	active, inactive := parseTCPStats(portSet)
	// 四层 TCP 无 waiting 概念(那是 HTTP keepalive 的概念),设为 0
	metrics.SetConnectionsByLayer("l4", active, inactive, 0)
}

// parseTCPStats 解析 /proc/net/tcp 和 /proc/net/tcp6,
// 仅统计本地端口在 portSet 中的连接。
func parseTCPStats(portSet map[int]struct{}) (established, others int) {
	e4, o4 := parseProcNetTCP("/proc/net/tcp", portSet)
	established += e4
	others += o4

	e6, o6 := parseProcNetTCP("/proc/net/tcp6", portSet)
	established += e6
	others += o6

	return
}

// parseProcNetTCP 解析单个 /proc/net/tcp 或 /proc/net/tcp6 文件。
// 格式:
//
//	sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
//	 0: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 ...
//
// local_address 是 "IP:PORT"(16进制),st 是 TCP 状态(01=ESTABLISHED)。
func parseProcNetTCP(path string, portSet map[int]struct{}) (established, others int) {
	f, err := os.Open(path)
	if err != nil {
		// 文件打开失败静默忽略(权限问题或 IPv6 未启用)
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 跳过标题行
	if !scanner.Scan() {
		return 0, 0
	}

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		// local_address 在索引 1,格式为 "IP:PORT"(16进制)
		localAddr := fields[1]
		colon := strings.LastIndex(localAddr, ":")
		if colon < 0 {
			continue
		}
		portHex := localAddr[colon+1:]
		port, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil {
			continue
		}

		// 仅统计在 portSet 中的端口
		if _, ok := portSet[int(port)]; !ok {
			continue
		}

		// st 字段在索引 3,01=ESTABLISHED
		if fields[3] == "01" {
			established++
		} else {
			others++
		}
	}

	return
}
