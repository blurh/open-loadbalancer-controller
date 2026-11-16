package parse

import (
	"reflect"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestFrontendProtocolAnnotationLifecycle(t *testing.T) {
	cr := sync.ControllerResources{}
	p := corev1.ServicePort{Port: 80, Protocol: corev1.ProtocolTCP}
	cr.Service.Spec.Ports = []corev1.ServicePort{p}
	for _, step := range []struct{ annotation, want string }{
		{"", "tcp"}, {" http : 80 ", "http"}, {"https:80", "https"}, {"", "tcp"},
		{"htpp:80", ""}, {"http", ""}, {"udp:80", ""}, {"http:80,tcp:80", ""},
	} {
		cr.Service.Annotations = map[string]string{apis.SvcAnnProtolPort: step.annotation}
		got, err := FrontendProtocol(cr, p)
		if got != step.want || (err != nil) != (step.want == "") {
			t.Fatalf("annotation=%q: protocol=%q error=%v", step.annotation, got, err)
		}
	}
	p.AppProtocol = utils.PtrTool("http")
	cr.Service.Annotations = nil
	if got, err := FrontendProtocol(cr, p); got != "http" || err != nil {
		t.Fatalf("appProtocol fallback: %q %v", got, err)
	}
}

func TestSameNumberTCPUDPAndPerSliceNamedPorts(t *testing.T) {
	cr := sync.ControllerResources{}
	cr.Service.Annotations = map[string]string{apis.SvcAnnNetworkType: "pod", apis.SvcAnnProtolPort: "http:80,udp:80"}
	cr.Service.Spec.Ports = []corev1.ServicePort{
		{Name: "tcp", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("web")},
		{Name: "udp", Port: 80, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns")},
	}
	for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		cr.EndpointSlices = append(cr.EndpointSlices, discovery.EndpointSlice{
			AddressType: discovery.AddressTypeIPv4,
			Endpoints:   []discovery.Endpoint{{Addresses: []string{ip}}},
			Ports: []discovery.EndpointPort{
				{Name: utils.PtrTool("tcp"), Port: utils.PtrTool(int32(8080 + i)), Protocol: utils.PtrTool(corev1.ProtocolTCP)},
				{Name: utils.PtrTool("udp"), Port: utils.PtrTool(int32(5353 + i)), Protocol: utils.PtrTool(corev1.ProtocolUDP)},
			},
		})
	}
	for i, want := range [][]string{{"10.0.0.1:8080", "10.0.0.2:8081"}, {"10.0.0.1:5353", "10.0.0.2:5354"}} {
		p := cr.Service.Spec.Ports[i]
		got, err := GenBackendsForPort(cr, p)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v %v", p.Name, got, err)
		}
		protocol, err := FrontendProtocol(cr, p)
		if err != nil || protocol != []string{"http", "udp"}[i] {
			t.Fatalf("%s: %s %v", p.Name, protocol, err)
		}
	}
}

func TestLocalExcludesTerminatingAndUsesIPv4NodeAddress(t *testing.T) {
	cr := sync.ControllerResources{}
	cr.Service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	p := corev1.ServicePort{Port: 80, NodePort: 30080}
	n := node("n1", "10.0.0.1", "")
	n.Status.Addresses = append([]corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "::1"}}, n.Status.Addresses...)
	cr.Nodes = []corev1.Node{n}
	cr.EndpointSlice.Endpoints = []discovery.Endpoint{endpoint("10.1.0.1", "n1", utils.PtrTool(true), utils.PtrTool(true))}
	if got, err := GenBackendsForPort(cr, p); err != nil || len(got) != 0 {
		t.Fatalf("terminating: %v %v", got, err)
	}
	cr.EndpointSlice.Endpoints[0].Conditions.Terminating = nil
	if got, err := GenBackendsForPort(cr, p); err != nil || !reflect.DeepEqual(got, []string{"10.0.0.1:30080"}) {
		t.Fatalf("IPv4: %v %v", got, err)
	}
}
