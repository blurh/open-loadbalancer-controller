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

// UDPCollector 从 /proc/net/udp 采集 UDP 连接数。
// UDP 是无连接协议，/proc/net/udp 记录的是本地监听端口的套接字状态，
// 不像 TCP 有 ESTABLISHED 等状态，统计维度是 active(端口打开) vs closed(端口关闭)。
type UDPCollector struct {
	portsFunc func() []int
	interval  time.Duration
}

// NewUDPCollector 创建 UDP 连接数采集器。
// portsFunc 返回当前监听的 UDP 端口列表；返回空则该轮 UDP 指标清零。
func NewUDPCollector(portsFunc func() []int, interval time.Duration) *UDPCollector {
	return &UDPCollector{
		portsFunc: portsFunc,
		interval:  interval,
	}
}

// Start 启动采集循环（阻塞）。
func (uc *UDPCollector) Start(ctx context.Context) {
	ticker := time.NewTicker(uc.interval)
	defer ticker.Stop()

	// 启动时立即采集一次
	uc.collect()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			uc.collect()
		}
	}
}

// collect 采集一次 UDP 端口状态并更新 metrics。
func (uc *UDPCollector) collect() {
	ports := uc.portsFunc()
	if len(ports) == 0 {
		// 无监听端口，清零 UDP 指标
		metrics.SetUDPSockets(0)
		return
	}

	portSet := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		portSet[p] = struct{}{}
	}

	active := parseUDPStats(portSet)
	metrics.SetUDPSockets(active)
}

// parseUDPStats 解析 /proc/net/udp 和 /proc/net/udp6，
// 统计本地端口在 portSet 中的套接字数量。
func parseUDPStats(portSet map[int]struct{}) int {
	count4 := parseProcNetUDP("/proc/net/udp", portSet)
	count6 := parseProcNetUDP("/proc/net/udp6", portSet)
	return count4 + count6
}

// parseProcNetUDP 解析单个 /proc/net/udp 或 /proc/net/udp6 文件。
// 格式:
//
//	sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
//	 0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 12345 2 ffff... 0
//
// local_address 是 "IP:PORT"(16进制)，st 是状态(07=CLOSE/unconnected，UDP 无连接)。
// UDP 套接字只要绑定端口就会出现在这里，统计数量即可。
func parseProcNetUDP(path string, portSet map[int]struct{}) int {
	f, err := os.Open(path)
	if err != nil {
		// 文件打开失败静默忽略（权限问题或 IPv6 未启用）
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 跳过标题行
	if !scanner.Scan() {
		return 0
	}

	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		// local_address 在索引 1，格式为 "IP:PORT"(16进制)
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
		if _, ok := portSet[int(port)]; ok {
			count++
		}
	}

	return count
}
