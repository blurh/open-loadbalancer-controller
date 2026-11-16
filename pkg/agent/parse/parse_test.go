package parse

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
)

// node 构造一个 Ready 节点（绝大多数用例假设节点健康）。
func node(name, internalIP, externalIP string) corev1.Node {
	return nodeWithReady(name, internalIP, externalIP, true)
}

func nodeWithReady(name, internalIP, externalIP string, ready bool) corev1.Node {
	addrs := []corev1.NodeAddress{}
	if internalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: internalIP})
	}
	if externalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: externalIP})
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  addrs,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func endpoint(addr, nodeName string, ready, terminating *bool) discovery.Endpoint {
	ep := discovery.Endpoint{
		Conditions: discovery.EndpointConditions{Ready: ready, Terminating: terminating},
	}
	if addr != "" {
		ep.Addresses = []string{addr}
	}
	if nodeName != "" {
		ep.NodeName = utils.PtrTool(nodeName)
	}
	return ep
}

func TestGenBackends_NodeportCluster(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports: []corev1.ServicePort{
					{Port: 80, NodePort: 30080},
				},
			},
		},
		Nodes: []corev1.Node{
			node("n1", "10.0.0.1", ""),
			node("n2", "10.0.0.2", ""),
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.0.0.1:30080", "10.0.0.2:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGenBackends_NodeportLocal_OnlyNodesWithReadyEndpoint(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{
			node("n1", "10.0.0.1", ""),
			node("n2", "10.0.0.2", ""),
		},
		EndpointSlice: discovery.EndpointSlice{
			Endpoints: []discovery.Endpoint{
				endpoint("10.244.0.1", "n1", utils.PtrTool(true), utils.PtrTool(false)),
				// n2 上只有未就绪端点，应被排除
				endpoint("10.244.0.2", "n2", utils.PtrTool(false), utils.PtrTool(false)),
			},
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.0.0.1:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGenBackends_NodeportExternalNodeIP(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					apis.SvcAnnNetworkType:     string(apis.NetworkTypeNodeport),
					apis.SvcAnnNodeNetworkType: string(apis.NodeNetworkTypeExternal),
				},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{node("n1", "10.0.0.1", "1.2.3.4")},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"1.2.3.4:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGenBackends_Pod_FiltersReadyNonTerminating(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypePod)},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8080)}},
			},
		},
		EndpointSlice: discovery.EndpointSlice{
			Endpoints: []discovery.Endpoint{
				endpoint("10.244.0.1", "n1", utils.PtrTool(true), utils.PtrTool(false)),  // 纳入
				endpoint("10.244.0.2", "n1", utils.PtrTool(true), utils.PtrTool(true)),   // terminating，排除
				endpoint("10.244.0.3", "n1", utils.PtrTool(false), utils.PtrTool(false)), // 未就绪，排除
				endpoint("10.244.0.4", "n1", nil, nil),                                   // ready 为 nil，按 Kubernetes 约定视为就绪
			},
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.244.0.1:8080", "10.244.0.4:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGenBackends_Pod_NamedTargetPortResolvedFromEndpointSlice(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypePod)},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromString("http")},
				},
			},
		},
		EndpointSlice: discovery.EndpointSlice{
			Ports: []discovery.EndpointPort{
				{Name: utils.PtrTool("http"), Port: utils.PtrTool(int32(8443))},
			},
			Endpoints: []discovery.Endpoint{
				endpoint("10.244.0.1", "n1", utils.PtrTool(true), utils.PtrTool(false)),
			},
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.244.0.1:8443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGenBackends_NodeportSkipsNodeWithoutIP(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{
			node("n1", "10.0.0.1", ""),
			node("n2", "", ""), // 无任何 IP → 应被跳过
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.0.0.1:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (node without IP must be skipped)", got, want)
	}
}

func TestGenBackends_NodeportSkipsNotReadyNode(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{
			nodeWithReady("n1", "10.0.0.1", "", true),
			nodeWithReady("n2", "10.0.0.2", "", false), // NotReady → 应被跳过
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.0.0.1:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (NotReady node must be skipped)", got, want)
	}
}

func TestGenBackends_NoNetworkTypeAnnotation(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}}}},
		Nodes:   []corev1.Node{node("n1", "10.0.0.1", "")},
	}
	got, err := GenBackends(cr, "80")
	if err != nil || !reflect.DeepEqual(got, []string{"10.0.0.1:30080"}) {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestGenBackends_UnknownNetworkType(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: "bogus"},
			},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
	}
	got, err := GenBackends(cr, "80")
	if err == nil {
		t.Fatalf("expected error for unknown network type, got backends %v", got)
	}
}

func TestGenBackends_SortsBackendsDeterministically(t *testing.T) {
	// 节点以乱序给出，期望输出按字典序稳定排序，与遍历顺序无关
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{
			node("n3", "10.0.0.3", ""),
			node("n1", "10.0.0.1", ""),
			node("n2", "10.0.0.2", ""),
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	want := []string{"10.0.0.1:30080", "10.0.0.2:30080", "10.0.0.3:30080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (backends must be sorted)", got, want)
	}
}

func TestGenBackends_Pod_NamedTargetPortUnresolvedSkips(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypePod)},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromString("http")},
				},
			},
		},
		// EndpointSlice.Ports 中没有名为 "http" 的端口 → 端口无法解析为数字
		EndpointSlice: discovery.EndpointSlice{
			Endpoints: []discovery.Endpoint{
				endpoint("10.244.0.1", "n1", utils.PtrTool(true), utils.PtrTool(false)),
			},
		},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no backends when named port is unresolved, got %v", got)
	}
}

func TestGenBackends_NodeportZeroNodePortSkips(t *testing.T) {
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport)},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Port: 80, NodePort: 0}}, // 未分配 nodePort
			},
		},
		Nodes: []corev1.Node{node("n1", "10.0.0.1", "")},
	}

	got, err := GenBackends(cr, "80")
	if err != nil {
		t.Fatalf("GenBackends returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no backends when nodePort is 0, got %v", got)
	}
}

func TestGetFrontendProtocol(t *testing.T) {
	tcp := "TCP"
	tests := []struct {
		name  string
		anns  map[string]string
		ports []corev1.ServicePort
		port  string
		want  string
	}{
		{
			name: "from annotation http",
			anns: map[string]string{apis.SvcAnnProtolPort: "http:80, https:443"},
			port: "80",
			want: "http",
		},
		{
			name: "from annotation https",
			anns: map[string]string{apis.SvcAnnProtolPort: "http:80, https:443"},
			port: "443",
			want: "https",
		},
		{
			name:  "fallback to appProtocol",
			ports: []corev1.ServicePort{{Port: 3000, AppProtocol: &tcp}},
			port:  "3000",
			want:  "tcp",
		},
		{
			name:  "appProtocol nil and no annotation",
			ports: []corev1.ServicePort{{Port: 3000}},
			port:  "3000",
			want:  "tcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := sync.ControllerResources{
				Service: corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Annotations: tt.anns},
					Spec:       corev1.ServiceSpec{Ports: tt.ports},
				},
			}
			if got := GetFrontendProtocol(cr, tt.port); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNodeHasReadyEndpoint(t *testing.T) {
	eps := []discovery.Endpoint{
		endpoint("10.0.0.1", "n1", utils.PtrTool(true), utils.PtrTool(false)),
		endpoint("10.0.0.2", "n2", utils.PtrTool(false), utils.PtrTool(false)),
	}
	if !nodeHasReadyEndpoint("n1", eps) {
		t.Error("n1 should have a ready endpoint")
	}
	if nodeHasReadyEndpoint("n2", eps) {
		t.Error("n2 has only an unready endpoint")
	}
	if nodeHasReadyEndpoint("n3", eps) {
		t.Error("n3 has no endpoint")
	}
}
