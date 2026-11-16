package nginx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestL7Collector_collect 测试七层 stub_status 解析
func TestL7Collector_collect(t *testing.T) {
	tests := []struct {
		name           string
		stubStatusBody string
		statusCode     int
	}{
		{
			name: "正常的 stub_status 输出",
			stubStatusBody: `Active connections: 42
server accepts handled requests
 123 456 789
Reading: 1 Writing: 2 Waiting: 3`,
			statusCode: http.StatusOK,
		},
		{
			name: "零连接",
			stubStatusBody: `Active connections: 0
server accepts handled requests
 0 0 0
Reading: 0 Writing: 0 Waiting: 0`,
			statusCode: http.StatusOK,
		},
		{
			name:           "HTTP 500 错误应静默忽略",
			stubStatusBody: "Internal Server Error",
			statusCode:     http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.stubStatusBody))
			}))
			defer ts.Close()

			collector := NewL7Collector(ts.URL, 100*time.Millisecond)
			collector.collect()
			// 验证不 panic 即可，实际指标值需通过 prometheus testutil 验证
		})
	}
}

// TestL7Collector_Start 测试采集器的启动与取消
func TestL7Collector_Start(t *testing.T) {
	var callCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Active connections: 10\nReading: 0 Writing: 0 Waiting: 5"))
	}))
	defer ts.Close()

	collector := NewL7Collector(ts.URL, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		collector.Start(ctx)
		close(done)
	}()
	<-done

	if got := callCount.Load(); got < 2 {
		t.Errorf("expected at least 2 calls, got %d", got)
	}
}
