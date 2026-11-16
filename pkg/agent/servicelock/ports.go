package servicelock

import (
	"fmt"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

// RequestedPorts 使用与渲染器相同的前端协议；没有应用层协议时仍预留 Service 声明的端口。
// 零后端或暂时没有生成配置不代表端口可以交给其他 Service。
func RequestedPorts(cr sync.ControllerResources) ([]Port, error) {
	if parse.IsLVS(cr.Service) {
		cr = parse.LVSResources(cr)
	}
	var ports []Port
	for _, p := range cr.Service.Spec.Ports {
		protocol, err := parse.FrontendProtocol(cr, p)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if protocol == "http" || protocol == "https" {
			protocol = "tcp"
		}

		if (protocol != "tcp" && protocol != "udp") || p.Port < 1 || p.Port > 65535 {
			return nil, fmt.Errorf("%w: unsupported port %s/%d", ErrInvalid, protocol, p.Port)
		}
		ports = append(ports, Port{Protocol: protocol, Number: int(p.Port)})
	}
	return ports, nil
}
