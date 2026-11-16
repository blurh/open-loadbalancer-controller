package utils

import (
	"net"
	"os"

	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
)

func GetMyNamespace() string {
	return os.Getenv("POD_NAMESPACE")
}

func GetNodeStatus(node *corev1.Node) sync.NodeStatus {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return sync.Ready
		}
	}
	return sync.NotReady
}

func GetNodeExternalIP(node *corev1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeExternalIP {
			if net.ParseIP(address.Address).To4() == nil {
				continue
			}
			return address.Address
		}
	}
	return ""
}

func GetNodeInternalIP(node *corev1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && net.ParseIP(address.Address).To4() != nil {
			return address.Address
		}
	}
	return ""
}
