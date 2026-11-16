package sync

import (
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

type NodeStatus int

const (
	NotReady NodeStatus = 0
	Ready    NodeStatus = 1
)

type ControllerResources struct {
	Service       corev1.Service          `json:"service"`
	Secrets       []corev1.Secret         `json:"secrets"`
	EndpointSlice discovery.EndpointSlice `json:"endpoints"`
	// EndpointSlices 保留各分片的端口映射；优先于旧的合并字段。
	EndpointSlices []discovery.EndpointSlice `json:"endpoint_slices,omitempty"`
	Nodes          []corev1.Node             `json:"nodes"`
	// ForceReload 由 controller 在重试（上一轮该 Service 同步失败）时置位，
	// 让 agent 即便配置无变更也强制 reload，避免卡在旧配置。omitempty 保证向后兼容。
	ForceReload bool `json:"force_reload,omitempty"`
}

type RespBody struct {
	ExternalIPs []string `json:"external_ip"`
	InternalIPs []string `json:"internal_ip"`
}

// ServiceRef 标识一个被纳管的 Service，用于 cleanup/Delete 定位配置。
type ServiceRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
