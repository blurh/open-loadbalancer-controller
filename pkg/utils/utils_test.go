package utils

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

func TestSplitCommaTrimSpace(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"10.0.0.1, 10.0.0.2 ,10.0.0.3", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
		{"single", []string{"single"}},
		{"", []string{""}},
	}
	for _, tt := range tests {
		if got := SplitCommaTrimSpace(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("SplitCommaTrimSpace(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestPtrTool(t *testing.T) {
	p := PtrTool(42)
	if p == nil || *p != 42 {
		t.Fatalf("PtrTool failed: %v", p)
	}
}

func nodeWith(addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{Status: corev1.NodeStatus{Addresses: addrs}}
}

func TestGetNodeInternalIP(t *testing.T) {
	n := nodeWith(
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
	)
	if got := GetNodeInternalIP(n); got != "10.0.0.1" {
		t.Fatalf("got %q", got)
	}
	if got := GetNodeInternalIP(nodeWith()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestGetNodeExternalIP_SkipsIPv6(t *testing.T) {
	n := nodeWith(
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "2600:3c0e::1"},
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
	)
	if got := GetNodeExternalIP(n); got != "1.2.3.4" {
		t.Fatalf("got %q, want ipv4 1.2.3.4", got)
	}
}

func TestGetNodeStatus(t *testing.T) {
	ready := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}}}
	if GetNodeStatus(ready) != sync.Ready {
		t.Error("expected Ready")
	}

	notReady := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}}}
	if GetNodeStatus(notReady) != sync.NotReady {
		t.Error("expected NotReady")
	}
}
