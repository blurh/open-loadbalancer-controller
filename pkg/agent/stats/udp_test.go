package stats

import (
	"os"
	"testing"
)

func TestParseProcNetUDP(t *testing.T) {
	// 模拟 /proc/net/udp 内容（端口 53/DNS 和 123/NTP）
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
   0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 12345 2 ffff8800 0
   1: 0100007F:007B 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 12346 2 ffff8801 0`

	tmpfile := createTempProcFile(t, content)
	defer removeTempFile(t, tmpfile)

	tests := []struct {
		name      string
		portSet   map[int]struct{}
		wantCount int
	}{
		{
			name:      "match port 53 (DNS)",
			portSet:   map[int]struct{}{53: {}},
			wantCount: 1,
		},
		{
			name:      "match port 123 (NTP)",
			portSet:   map[int]struct{}{123: {}},
			wantCount: 1,
		},
		{
			name:      "match multiple ports",
			portSet:   map[int]struct{}{53: {}, 123: {}},
			wantCount: 2,
		},
		{
			name:      "no match",
			portSet:   map[int]struct{}{8080: {}},
			wantCount: 0,
		},
		{
			name:      "empty port set",
			portSet:   map[int]struct{}{},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseProcNetUDP(tmpfile, tt.portSet)
			if got != tt.wantCount {
				t.Errorf("parseProcNetUDP() = %v, want %v", got, tt.wantCount)
			}
		})
	}
}

func TestParseProcNetUDP_InvalidFile(t *testing.T) {
	// 不存在的文件应返回 0 而非 panic
	count := parseProcNetUDP("/nonexistent/file", map[int]struct{}{53: {}})
	if count != 0 {
		t.Errorf("parseProcNetUDP on missing file = %v, want 0", count)
	}
}

// createTempProcFile 创建临时文件用于测试
func createTempProcFile(t *testing.T, content string) string {
	t.Helper()
	tmpfile, err := os.CreateTemp("", "proc_net_udp_test")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	if _, err := tmpfile.WriteString(content); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatalf("failed to close temp file: %v", err)
	}
	return tmpfile.Name()
}

// removeTempFile 清理临时文件
func removeTempFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Errorf("failed to remove temp file: %v", err)
	}
}
