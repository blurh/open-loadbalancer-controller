package nginx

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
)

// L7Collector 从 nginx stub_status 采集七层(HTTP/HTTPS)连接数指标。
// 这是 nginx 特有的能力(依赖 stub_status 模块),故留在 nginx 包内。
// 四层(TCP)连接数是转发器无关的通用能力,见 pkg/agent/stats.
type L7Collector struct {
	statusURL string
	interval  time.Duration
	client    *http.Client
}

// NewL7Collector 创建七层连接数采集器。
// statusURL 为 nginx stub_status 地址,如 "http://127.0.0.1:8888/nginx_status"。
func NewL7Collector(statusURL string, interval time.Duration) *L7Collector {
	return &L7Collector{
		statusURL: statusURL,
		interval:  interval,
		client: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// Start 启动采集循环(阻塞)。
func (lc *L7Collector) Start(ctx context.Context) {
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

// collect 采集一次 stub_status 并更新七层 metrics。
// stub_status 输出格式:
//
//	Active connections: 42
//	server accepts handled requests
//	 123 456 789
//	Reading: 1 Writing: 2 Waiting: 3
func (lc *L7Collector) collect() {
	resp, err := lc.client.Get(lc.statusURL)
	if err != nil {
		// 采集失败静默忽略,不影响主流程
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	var active, waiting int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Active connections:") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				active, _ = strconv.Atoi(parts[2])
			}
		} else if strings.HasPrefix(line, "Reading:") {
			// Reading: 1 Writing: 2 Waiting: 3
			parts := strings.Fields(line)
			if len(parts) >= 6 {
				waiting, _ = strconv.Atoi(parts[5])
			}
		}
	}

	// 七层: active 已包含 reading+writing+waiting
	// inactive 对 HTTP 短连接意义不大,设为 0
	metrics.SetConnectionsByLayer("l7", active, 0, waiting)
}
