package stats

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseProcNetTCP 测试解析 /proc/net/tcp 并按端口过滤
func TestParseProcNetTCP(t *testing.T) {
	tests := []struct {
		name            string
		content         string
		portSet         map[int]struct{}
		wantEstablished int
		wantOthers      int
	}{
		{
			name: "正常输出，过滤指定端口",
			content: `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1
   1: 0100007F:0BB8 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 12346 1
   2: 0100007F:1388 0100007F:1F91 01 00000000:00000000 00:00000000 00000000  1000        0 12347 1
   3: 0100007F:1F92 0100007F:0050 08 00000000:00000000 00:00000000 00000000  1000        0 12348 1`,
			portSet: map[int]struct{}{
				3000: {}, // 0x0BB8
			},
			wantEstablished: 1, // 只有端口 3000 的 ESTABLISHED
			wantOthers:      1, // 只有端口 3000 的 LISTEN(0A)
		},
		{
			name: "过滤多个端口",
			content: `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 12345 1
   1: 0100007F:1388 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 12346 1
   2: 0100007F:1770 0100007F:1F91 08 00000000:00000000 00:00000000 00000000  1000        0 12347 1`,
			portSet: map[int]struct{}{
				3000: {}, // 0x0BB8
				5000: {}, // 0x1388
			},
			wantEstablished: 2,
			wantOthers:      0,
		},
		{
			name: "端口不在过滤列表中",
			content: `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0050 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 12345 1`,
			portSet: map[int]struct{}{
				3000: {},
			},
			wantEstablished: 0,
			wantOthers:      0,
		},
		{
			name:            "空文件",
			content:         "",
			portSet:         map[int]struct{}{3000: {}},
			wantEstablished: 0,
			wantOthers:      0,
		},
		{
			name: "仅标题行",
			content: `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
`,
			portSet:         map[int]struct{}{3000: {}},
			wantEstablished: 0,
			wantOthers:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tmpFile := filepath.Join(tmpDir, "tcp")
			if err := os.WriteFile(tmpFile, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}

			established, others := parseProcNetTCP(tmpFile, tt.portSet)

			if established != tt.wantEstablished {
				t.Errorf("established = %d, want %d", established, tt.wantEstablished)
			}
			if others != tt.wantOthers {
				t.Errorf("others = %d, want %d", others, tt.wantOthers)
			}
		})
	}
}

// TestL4Collector_Start 测试采集器启动与取消
func TestL4Collector_Start(t *testing.T) {
	var callCount atomic.Int64
	portsFunc := func() []int {
		callCount.Add(1)
		return []int{3000, 5000}
	}

	collector := NewL4Collector(portsFunc, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		collector.Start(ctx)
		close(done)
	}()
	<-done

	// 应至少采集了 2-3 次(启动立即 1 次 + 2 个 tick)
	if got := callCount.Load(); got < 2 {
		t.Errorf("expected at least 2 calls, got %d", got)
	}
}

// TestL4Collector_NoPorts 测试无监听端口时清零
func TestL4Collector_NoPorts(t *testing.T) {
	portsFunc := func() []int {
		return []int{}
	}

	collector := NewL4Collector(portsFunc, 1*time.Second)
	collector.collect()
	// 验证不 panic，实际应调用 SetConnectionsByLayer("l4", 0, 0, 0)
}
