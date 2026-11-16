package parse

import (
	"fmt"
	"net"
	slicespkg "slices"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
)

// parse 包提供与具体转发器无关的「Service 解析」逻辑：把 controller 推来的
// ControllerResources（Service + Nodes + EndpointSlice）解析成后端列表与前端协议，
// 供 nginx / lvs 等各类 agent 实现共享复用。

// FrontendProtocol 按 annotation > appProtocol > Service.protocol 解析，拒绝非法配置。
// 按完整 ServicePort 解析，避免同号 TCP/UDP 端口互相覆盖。
func FrontendProtocol(cr sync.ControllerResources, port corev1.ServicePort) (string, error) {
	protocol := ""
	for _, entry := range utils.SplitCommaTrimSpace(cr.Service.Annotations[apis.SvcAnnProtolPort]) {
		if entry == "" {
			continue
		}
		proto, number, ok := strings.Cut(entry, ":")
		proto = strings.ToLower(strings.TrimSpace(proto))
		n, err := strconv.Atoi(strings.TrimSpace(number))
		if !ok || err != nil || n < 1 || n > 65535 || !supportedProtocol(proto) {
			return "", fmt.Errorf("invalid %s entry %q", apis.SvcAnnProtolPort, entry)
		}
		if int32(n) != port.Port {
			continue
		}
		// 同一数字同时声明 TCP/UDP 时，annotation 只作用于对应的传输协议。
		shared := false
		for _, other := range cr.Service.Spec.Ports {
			if other.Port == port.Port && transport(other.Protocol) != transport(port.Protocol) {
				shared = true
			}
		}
		if shared && frontendTransport(proto) != transport(port.Protocol) {
			continue
		}
		if protocol != "" {
			return "", fmt.Errorf("duplicate protocol annotation for port %d", port.Port)
		}
		protocol = proto
	}
	if protocol == "" && port.AppProtocol != nil {
		protocol = strings.ToLower(strings.TrimSpace(*port.AppProtocol))
	}
	if protocol == "" {
		protocol = strings.ToLower(string(transport(port.Protocol)))
	}
	if !supportedProtocol(protocol) {
		return "", fmt.Errorf("unsupported frontend protocol %q on port %d", protocol, port.Port)
	}
	if port.Protocol != "" && frontendTransport(protocol) != port.Protocol {
		return "", fmt.Errorf("frontend %s on port %d cannot forward to %s backends", protocol, port.Port, port.Protocol)
	}
	return protocol, nil
}

func supportedProtocol(protocol string) bool {
	return protocol == "tcp" || protocol == "udp" || protocol == "http" || protocol == "https"
}

func transport(protocol corev1.Protocol) corev1.Protocol {
	if protocol == "" {
		return corev1.ProtocolTCP
	}
	return protocol
}

func frontendTransport(protocol string) corev1.Protocol {
	if protocol == "udp" {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// GetFrontendProtocol 保留按端口号查询的入口；渲染和校验使用 FrontendProtocol 返回错误。
func GetFrontendProtocol(cr sync.ControllerResources, frontendPort string) string {
	for _, port := range cr.Service.Spec.Ports {
		if strconv.Itoa(int(port.Port)) == frontendPort {
			protocol, _ := FrontendProtocol(cr, port)
			return protocol
		}
	}
	n, _ := strconv.Atoi(frontendPort)
	protocol, _ := FrontendProtocol(cr, corev1.ServicePort{Port: int32(n)})
	return protocol
}

// resolvePodBackendPort 返回 pod 直连模式下的后端端口：targetPort 为数字时直接用；
// 为命名端口时按 Service 端口名从 EndpointSlice.Ports 解析出实际数字端口
// （EndpointSlice 端口名与 Service 端口名一致）。
func resolvePodBackendPort(svcPort corev1.ServicePort, epPorts []discovery.EndpointPort) string {
	if svcPort.TargetPort.Type == intstr.Int {
		return strconv.Itoa(svcPort.TargetPort.IntValue())
	}
	for _, ep := range epPorts {
		epProtocol := corev1.ProtocolTCP
		if ep.Protocol != nil {
			epProtocol = transport(*ep.Protocol)
		}
		if ep.Port == nil || epProtocol != transport(svcPort.Protocol) {
			continue
		}
		name := ""
		if ep.Name != nil {
			name = *ep.Name
		}
		if name == svcPort.Name {
			return strconv.Itoa(int(*ep.Port))
		}
	}
	// 命名端口在 EndpointSlice.Ports 中找不到对应数字端口：返回空，
	// 由调用方按「无效端口」跳过，避免渲染出 `ip:<name>` 这类非法后端。
	return ""
}

// validBackendPort 校验后端端口是合法的 TCP/UDP 端口（1-65535）。
// nginx 是全局共享配置 + 单次 reload，任一 Service 渲染出 `ip:0` 或 `ip:<name>`
// 这类非法 upstream 行都会让整个 reload 失败、波及所有 Service，故在源头拦掉。
func validBackendPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// GenBackends 保留端口号查询入口；生产转发器使用完整 ServicePort 防止 TCP/UDP 混淆。
func GenBackends(cr sync.ControllerResources, frontendPort string) ([]string, error) {
	for _, port := range cr.Service.Spec.Ports {
		if strconv.Itoa(int(port.Port)) == frontendPort {
			return GenBackendsForPort(cr, port)
		}
	}
	return []string{}, nil
}

func GenBackendsForPort(cr sync.ControllerResources, port corev1.ServicePort) ([]string, error) {
	anns := cr.Service.Annotations
	networkType := anns[apis.SvcAnnNetworkType]
	if networkType == "" {
		networkType = string(apis.NetworkTypeNodeport)
	}
	slices := cr.EndpointSlices
	if len(slices) == 0 {
		slices = []discovery.EndpointSlice{cr.EndpointSlice}
	}
	endpoints := []discovery.Endpoint{}
	for _, slice := range slices {
		if slice.AddressType == "" || slice.AddressType == discovery.AddressTypeIPv4 {
			endpoints = append(endpoints, slice.Endpoints...)
		}
	}
	backends := []string{}
	switch networkType {
	case string(apis.NetworkTypeNodeport):
		backendPort := strconv.Itoa(int(port.NodePort))
		if !validBackendPort(backendPort) {
			return backends, nil
		}
		nodeNetwork := anns[apis.SvcAnnNodeNetworkType]
		if nodeNetwork != "" && nodeNetwork != string(apis.NodeNetworkTypeInternal) && nodeNetwork != string(apis.NodeNetworkTypeExternal) {
			return nil, fmt.Errorf("unknown node network type %q", nodeNetwork)
		}
		for _, node := range cr.Nodes {
			if utils.GetNodeStatus(&node) != sync.Ready {
				continue
			}
			if cr.Service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && !nodeHasReadyEndpoint(node.Name, endpoints) {
				continue
			}
			ip := utils.GetNodeInternalIP(&node)
			if nodeNetwork == string(apis.NodeNetworkTypeExternal) {
				ip = utils.GetNodeExternalIP(&node)
			}
			if ip != "" {
				backends = append(backends, net.JoinHostPort(ip, backendPort))
			}
		}
	case string(apis.NetworkTypePod):
		if port.TargetPort.IntVal == 0 && port.TargetPort.StrVal == "" {
			port.TargetPort.IntVal = port.Port
		}
		for _, slice := range slices {
			if slice.AddressType != "" && slice.AddressType != discovery.AddressTypeIPv4 {
				continue
			}
			// 每个分片独立解析命名 targetPort，滚动升级时不同 pod 可使用不同端口。
			backendPort := resolvePodBackendPort(port, slice.Ports)
			if !validBackendPort(backendPort) {
				continue
			}
			for _, endpoint := range slice.Endpoints {
				if !endpointReady(endpoint) {
					continue
				}
				for _, address := range endpoint.Addresses {
					if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
						backends = append(backends, net.JoinHostPort(ip.String(), backendPort))
					}
				}
			}
		}
	default:
		return nil, fmt.Errorf("unknown network type %q (expected %q or %q)", networkType, apis.NetworkTypeNodeport, apis.NetworkTypePod)
	}
	sort.Strings(backends)
	return slicespkg.Compact(backends), nil
}

func endpointReady(endpoint discovery.Endpoint) bool {
	return (endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready) &&
		(endpoint.Conditions.Terminating == nil || !*endpoint.Conditions.Terminating)
}

func nodeHasReadyEndpoint(nodeName string, endpoints []discovery.Endpoint) bool {
	for _, endpoint := range endpoints {
		if endpoint.NodeName != nil && *endpoint.NodeName == nodeName && endpointReady(endpoint) {
			return true
		}
	}
	return false
}
