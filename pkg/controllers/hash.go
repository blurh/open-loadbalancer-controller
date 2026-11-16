package controllers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// forwardingHash 排除对象版本、状态回写和心跳时间；排序后计算转发输入的稳定摘要。
// 相同配置重试仍可调用 agent，但无需反复把 Applied 改成 PendingApply 再改回来。
func forwardingHash(cr sync.ControllerResources) (string, error) {
	cr.ForceReload = false
	svc := cr.Service.DeepCopy()
	annotations := make(map[string]string)
	for _, key := range []string{apis.SvcAnnClass, apis.SvcAnnProtolPort, apis.SvcAnnNetworkType, apis.SvcAnnNodeNetworkType, apis.SvcAnnCertSecrets} {
		if value, ok := svc.Annotations[key]; ok {
			annotations[key] = value
		}
	}
	svc.ObjectMeta = metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name, Annotations: annotations}
	svc.Status = corev1.ServiceStatus{}
	cr.Service = *svc
	cr.EndpointSlice = normalizedSlice(cr.EndpointSlice)
	cr.EndpointSlices = append([]discovery.EndpointSlice(nil), cr.EndpointSlices...)
	for i := range cr.EndpointSlices {
		cr.EndpointSlices[i] = normalizedSlice(cr.EndpointSlices[i])
	}
	sortJSON(cr.EndpointSlices)
	// 新协议中合并字段仅用于兼容，不参与摘要，避免首片顺序导致抖动。
	if len(cr.EndpointSlices) != 0 {
		cr.EndpointSlice = discovery.EndpointSlice{}
	}
	cr.Nodes = append([]corev1.Node(nil), cr.Nodes...)
	for i := range cr.Nodes {
		node := cr.Nodes[i].DeepCopy()
		node.ObjectMeta = metav1.ObjectMeta{Name: node.Name}
		node.Spec = corev1.NodeSpec{}
		status := corev1.NodeStatus{Addresses: node.Status.Addresses}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				status.Conditions = append(status.Conditions, corev1.NodeCondition{Type: condition.Type, Status: condition.Status})
			}
		}
		sortJSON(status.Addresses)
		node.Status = status
		cr.Nodes[i] = *node
	}
	sortJSON(cr.Nodes)
	cr.Secrets = append([]corev1.Secret(nil), cr.Secrets...)
	for i := range cr.Secrets {
		secret := cr.Secrets[i].DeepCopy()
		secret.ObjectMeta = metav1.ObjectMeta{Namespace: secret.Namespace, Name: secret.Name}
		cr.Secrets[i] = *secret
	}
	sortJSON(cr.Secrets)
	data, err := json.Marshal(cr)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func sortJSON[T any](items []T) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := json.Marshal(items[i])
		b, _ := json.Marshal(items[j])
		return string(a) < string(b)
	})
}

func normalizedSlice(input discovery.EndpointSlice) discovery.EndpointSlice {
	eps := input.DeepCopy()
	eps.ObjectMeta = metav1.ObjectMeta{}
	for i := range eps.Endpoints {
		sort.Strings(eps.Endpoints[i].Addresses)
	}
	sortJSON(eps.Endpoints)
	sortJSON(eps.Ports)
	return *eps
}
