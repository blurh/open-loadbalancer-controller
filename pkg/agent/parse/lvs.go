package parse

import (
	"sort"
	"strings"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
)

func IsLVS(svc corev1.Service) bool {
	return strings.TrimSpace(svc.Annotations[apis.SvcAnnClass]) == "lvs"
}

// IgnoredL7Fields 供 controller 生成 Service 事件；agent 不访问 Kubernetes API。
func IgnoredL7Fields(svc corev1.Service) []string {
	var ignored []string
	if strings.TrimSpace(svc.Annotations[apis.SvcAnnCertSecrets]) != "" {
		ignored = append(ignored, apis.SvcAnnCertSecrets)
	}
	for _, entry := range strings.Split(svc.Annotations[apis.SvcAnnProtolPort], ",") {
		protocol, _, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if ok && protocol != "" && !isTransport(protocol) {
			ignored = append(ignored, apis.SvcAnnProtolPort)
			break
		}
	}
	for _, port := range svc.Spec.Ports {
		if port.AppProtocol != nil && *port.AppProtocol != "" && !isTransport(*port.AppProtocol) {
			ignored = append(ignored, "spec.ports[].appProtocol")
			break
		}
	}
	sort.Strings(ignored)
	return ignored
}

func isTransport(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), "tcp") || strings.EqualFold(strings.TrimSpace(protocol), "udp")
}

// LVSResources 只保留四层输入。协议以 spec.ports[].protocol 为准；不读取或保护 TLS Secret。
// 对副本操作，原 Service 仍用于 Kubernetes status/finalizer/事件。
func LVSResources(cr sync.ControllerResources) sync.ControllerResources {
	cr.Service = *cr.Service.DeepCopy()
	cr.Secrets = nil
	delete(cr.Service.Annotations, apis.SvcAnnCertSecrets)
	delete(cr.Service.Annotations, apis.SvcAnnProtolPort)
	for i := range cr.Service.Spec.Ports {
		cr.Service.Spec.Ports[i].AppProtocol = nil
		if cr.Service.Spec.Ports[i].Protocol == "" {
			cr.Service.Spec.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	return cr
}
