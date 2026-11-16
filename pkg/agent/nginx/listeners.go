package nginx

import (
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

// ListListeners 从生成文件恢复端口归属。upstream 残留也保守地占用端口，
// 以覆盖只写入一部分配置后失败的情况；HTTP/HTTPS 共用 TCP 端口空间。
func (na *Agent) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) {
	listeners := make(map[sync.ServiceRef][]servicelock.Port)
	for _, dir := range na.streamAndHttpDirs() {
		files, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".conf") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimSuffix(file.Name(), ".conf"), "_upstream")
			parts := strings.Split(name, "_")
			if len(parts) != 4 {
				continue
			}
			ref := sync.ServiceRef{Namespace: parts[0], Name: parts[1]}
			if servicelock.ValidateService(ref) != nil {
				continue
			}
			port, err := strconv.Atoi(parts[3])
			if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != parts[3] {
				continue
			}
			protocol := parts[2]
			if dir == na.nginxHome+ConfigHttpPath {
				if protocol != "http" && protocol != "https" {
					continue
				}
				protocol = "tcp"
			} else if protocol != "tcp" && protocol != "udp" {
				continue
			}
			p := servicelock.Port{Protocol: protocol, Number: port}
			if !slices.Contains(listeners[ref], p) {
				listeners[ref] = append(listeners[ref], p)
			}
		}
	}
	return listeners, nil
}
