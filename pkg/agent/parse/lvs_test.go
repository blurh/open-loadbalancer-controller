package parse

import (
	"reflect"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestLVSBackendPortsSeparatedByTransport(t *testing.T) {
	cr := sync.ControllerResources{Service: corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "default"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "tcp", Port: 53, Protocol: corev1.ProtocolTCP, NodePort: 30053},
		{Name: "udp", Port: 53, Protocol: corev1.ProtocolUDP, NodePort: 31053},
	}}}, Nodes: []corev1.Node{{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}}
	for i, want := range []string{"10.0.0.2:30053", "10.0.0.2:31053"} {
		got, err := GenBackendsForPort(cr, cr.Service.Spec.Ports[i])
		if err != nil || !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("port=%d: %v %v", i, got, err)
		}
	}
	cr.Service.Annotations = map[string]string{"kubernetes.io/openlb.network-type": "pod"}
	ready := true
	name := "tcp"
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	p1, p2 := int32(8053), int32(9053)
	cr.Service.Spec.Ports[0].TargetPort = intstr.FromString("dns")
	cr.EndpointSlice = discovery.EndpointSlice{Ports: []discovery.EndpointPort{{Name: &name, Protocol: &udp, Port: &p2}, {Name: &name, Protocol: &tcp, Port: &p1}}, Endpoints: []discovery.Endpoint{{Addresses: []string{"10.0.0.3"}, Conditions: discovery.EndpointConditions{Ready: &ready}}}}
	got, err := GenBackendsForPort(cr, cr.Service.Spec.Ports[0])
	if err != nil || !reflect.DeepEqual(got, []string{"10.0.0.3:8053"}) {
		t.Fatalf("named port: %v %v", got, err)
	}
}

func TestLVSLocalPolicyExcludesTerminatingEndpoints(t *testing.T) {
	ready, terminating := true, true
	name := "node-a"
	cr := sync.ControllerResources{Service: corev1.Service{Spec: corev1.ServiceSpec{ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}}, Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}, EndpointSlice: discovery.EndpointSlice{Endpoints: []discovery.Endpoint{{NodeName: &name, Conditions: discovery.EndpointConditions{Ready: &ready, Terminating: &terminating}}}}}
	got, err := GenBackendsForPort(cr, corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 30080})
	if err != nil || len(got) != 0 {
		t.Fatalf("terminating endpoint selected node: %v %v", got, err)
	}
}
