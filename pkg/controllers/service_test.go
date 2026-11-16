package controllers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/blurh/open-loadbalancer-controller/internal/testutil"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// mockSyncer 记录 Sync/Clean 调用，可模拟成功/失败
type mockSyncer struct {
	syncCalls  []syncCall
	cleanCalls []cleanCall
	syncErr    error
	cleanErr   error
	syncErrors map[string]error
}

type syncCall struct {
	instance    string
	svc         string // namespace/name
	forceReload bool
}

type cleanCall struct {
	instance string
	svc      string // namespace/name
}

func (m *mockSyncer) Sync(instance string, cr sync.ControllerResources, respBody *sync.RespBody) error {
	m.syncCalls = append(m.syncCalls, syncCall{
		instance:    instance,
		svc:         cr.Service.Namespace + "/" + cr.Service.Name,
		forceReload: cr.ForceReload,
	})
	if m.syncErr != nil {
		return m.syncErr
	}
	if err := m.syncErrors[instance]; err != nil {
		return err
	}
	// 模拟 agent 返回空 RespBody（controller 会 fallback 到 instance 本身作为 ingress IP）
	return nil
}

func (m *mockSyncer) Clean(instance string, ref sync.ServiceRef) error {
	m.cleanCalls = append(m.cleanCalls, cleanCall{
		instance: instance,
		svc:      ref.Namespace + "/" + ref.Name,
	})
	return m.cleanErr
}

// mockStore 使用真实 CRDStore + fake API，保留旧测试的地址列表断言辅助方法。
type mockStore struct {
	*store.CRDStore
	c      client.Client
	setErr error
}

func newMockStore(c client.Client) *mockStore {
	return &mockStore{CRDStore: store.NewCRDStore(c, c), c: c}
}

func (m *mockStore) Ensure(ctx context.Context, svc *corev1.Service) (*statev1.OpenLBServiceState, error) {
	if m.setErr != nil {
		return nil, m.setErr
	}
	return m.CRDStore.Ensure(ctx, svc)
}
func (m *mockStore) UpdateStatus(ctx context.Context, state *statev1.OpenLBServiceState, mutate func(*statev1.ServiceStateStatus)) (*statev1.OpenLBServiceState, error) {
	if m.setErr != nil {
		return nil, m.setErr
	}
	return m.CRDStore.UpdateStatus(ctx, state, mutate)
}
func (m *mockStore) Set(k, v string) error {
	parts := strings.SplitN(k, "/", 2)
	key := types.NamespacedName{Namespace: parts[0], Name: parts[1]}
	svc := &corev1.Service{}
	if err := m.c.Get(context.Background(), key, svc); err != nil {
		return err
	}
	state, err := m.Ensure(context.Background(), svc)
	if err != nil {
		return err
	}
	_, err = m.UpdateStatus(context.Background(), state, func(status *statev1.ServiceStateStatus) {
		status.Instances = nil
		for _, address := range normalizedInstances(v) {
			status.Instances = append(status.Instances, statev1.InstanceState{Address: address, Phase: statev1.InstanceApplied})
		}
	})
	return err
}
func (m *mockStore) Get(k string) (string, error) {
	parts := strings.SplitN(k, "/", 2)
	state, err := m.Load(context.Background(), types.NamespacedName{Namespace: parts[0], Name: parts[1]})
	if err != nil {
		return "", err
	}
	var addresses []string
	for _, entry := range state.Status.Instances {
		if entry.Phase != statev1.InstanceCleaned {
			addresses = append(addresses, entry.Address)
		}
	}
	return strings.Join(addresses, ","), nil
}
func (m *mockStore) Exists(k string) (bool, error) {
	_, err := m.Get(k)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
func (m *mockStore) Keys() []string {
	list := &statev1.OpenLBServiceStateList{}
	if m.c.List(context.Background(), list) != nil {
		return nil
	}
	var keys []string
	for _, state := range list.Items {
		keys = append(keys, state.Namespace+"/"+state.Name)
	}
	return keys
}

// newTestReconciler 构造测试用 reconciler，client 使用 fake，syncer/store 使用 mock
func newTestReconciler(objs []client.Object) (*ServiceReconciler, *mockSyncer, *mockStore) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discovery.AddToScheme(scheme)
	_ = statev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}, &statev1.OpenLBServiceState{}). // 支持 status 子资源
		Build()

	syncer := &mockSyncer{}
	repo := newMockStore(fakeClient)

	r := &ServiceReconciler{
		Client:            fakeClient,
		Log:               logr.Discard(),
		Eventer:           &record.FakeRecorder{},
		ServiceRepository: repo,
		Syncer:            syncer,
		forceReload:       make(map[string]struct{}),
	}
	return r, syncer, repo
}

// node 构造一个 Ready 节点
func node(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: internalIP},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// lbService 构造 type=LoadBalancer 的 Service
func lbService(ns, name string, instances string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			UID:       types.UID(ns + "-" + name),
			Annotations: map[string]string{
				apis.SvcAnnInstances:   instances,
				apis.SvcAnnClass:       "nginx",
				apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport),
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, NodePort: 30080, TargetPort: intstr.FromInt(8080)},
			},
		},
	}
}

// endpointSlice 构造一个 EndpointSlice
func endpointSlice(ns, svcName string) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      svcName + "-abcde",
			Labels: map[string]string{
				discovery.LabelServiceName: svcName,
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Addresses: []string{"10.244.1.10"},
				Conditions: discovery.EndpointConditions{
					Ready: ptrBool(true),
				},
			},
		},
		Ports: []discovery.EndpointPort{
			{Name: ptrString("http"), Port: ptrInt32(8080)},
		},
	}
}

func ptrBool(b bool) *bool       { return &b }
func ptrString(s string) *string { return &s }
func ptrInt32(i int32) *int32    { return &i }

// TestReconcile_NormalPath 测试正常路径：setup 推送到 agent，写 status，加 finalizer，记录 repo
func TestReconcile_NormalPath(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080,10.0.0.2:8080")
	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, repo := newTestReconciler([]client.Object{svc, n1, eps})

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue on success")
	}

	// 断言 Syncer.Sync 被调用了 2 次（两个 instance）
	if len(syncer.syncCalls) != 2 {
		t.Fatalf("expected 2 sync calls, got %d", len(syncer.syncCalls))
	}
	if syncer.syncCalls[0].instance != "10.0.0.1:8080" || syncer.syncCalls[0].svc != "default/web" {
		t.Errorf("sync call 0: %+v", syncer.syncCalls[0])
	}
	if syncer.syncCalls[1].instance != "10.0.0.2:8080" || syncer.syncCalls[1].svc != "default/web" {
		t.Errorf("sync call 1: %+v", syncer.syncCalls[1])
	}

	// 断言 repo 记录了实例列表
	repoVal, err := repo.Get("default/web")
	if err != nil || repoVal != "10.0.0.1:8080,10.0.0.2:8080" {
		t.Errorf("repo: got %q, err %v", repoVal, err)
	}

	// 断言 Service.status.loadBalancer.ingress 被写入（两个 instance IP）
	freshSvc := &corev1.Service{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, freshSvc); err != nil {
		t.Fatal(err)
	}
	if len(freshSvc.Status.LoadBalancer.Ingress) != 2 {
		t.Fatalf("expected 2 ingress IPs, got %d", len(freshSvc.Status.LoadBalancer.Ingress))
	}
	// agent 返回空 RespBody → fallback 到 instance 自身（去掉 :port）
	if freshSvc.Status.LoadBalancer.Ingress[0].IP != "10.0.0.1" {
		t.Errorf("ingress[0].IP = %q", freshSvc.Status.LoadBalancer.Ingress[0].IP)
	}
	if freshSvc.Status.LoadBalancer.Ingress[1].IP != "10.0.0.2" {
		t.Errorf("ingress[1].IP = %q", freshSvc.Status.LoadBalancer.Ingress[1].IP)
	}

	// 断言 finalizer 被加上
	if len(freshSvc.Finalizers) != 1 || freshSvc.Finalizers[0] != DefaultFinalizer {
		t.Errorf("finalizers = %v", freshSvc.Finalizers)
	}
}

// TestReconcile_Deletion 测试删除路径：合并 repo + annotation 实例，cleanup 全部，摘 finalizer
func TestReconcile_Deletion(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080") // 当前注解只有 1 个
	svc.Finalizers = []string{DefaultFinalizer}
	now := metav1.Now()
	svc.DeletionTimestamp = &now // 标记删除中

	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, repo := newTestReconciler([]client.Object{svc, n1, eps})
	// 预置 repo 记录：历史同步过 10.0.0.1 和 10.0.0.2
	_ = repo.Set("default/web", "10.0.0.1:8080,10.0.0.2:8080")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue after successful cleanup")
	}

	// 断言 Clean 被调用了 2 次（合并 repo + annotation，去重后 10.0.0.1 和 10.0.0.2）
	if len(syncer.cleanCalls) != 2 {
		t.Fatalf("expected 2 clean calls, got %d", len(syncer.cleanCalls))
	}
	cleaned := map[string]bool{
		syncer.cleanCalls[0].instance: true,
		syncer.cleanCalls[1].instance: true,
	}
	if !cleaned["10.0.0.1:8080"] || !cleaned["10.0.0.2:8080"] {
		t.Errorf("cleanup instances: %v", cleaned)
	}

	// 断言 repo 记录被删除
	if exists, _ := repo.Exists("default/web"); exists {
		t.Error("repo record should be deleted on successful cleanup")
	}

	// 断言 finalizer 被摘除（Service 应该已被 K8s 真正删除，但 fake client 不会自动删，我们检查 finalizers）
	freshSvc := &corev1.Service{}
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, freshSvc)
	if len(freshSvc.Finalizers) != 0 {
		t.Errorf("finalizer should be removed after cleanup, got %v", freshSvc.Finalizers)
	}
}

// TestReconcile_Deletion_CleanupFailed 测试删除时 cleanup 失败：保留 finalizer，返回 error 触发 requeue
func TestReconcile_Deletion_CleanupFailed(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080")
	svc.Finalizers = []string{DefaultFinalizer}
	now := metav1.Now()
	svc.DeletionTimestamp = &now

	r, syncer, repo := newTestReconciler([]client.Object{svc})
	_ = repo.Set("default/web", "10.0.0.1:8080")
	syncer.cleanErr = errors.New("agent unreachable")

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err == nil {
		t.Fatal("Reconcile should return error when cleanup fails")
	}

	// 断言 finalizer 仍在（Service 不会被删除）
	freshSvc := &corev1.Service{}
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, freshSvc)
	if len(freshSvc.Finalizers) == 0 {
		t.Error("finalizer must remain when cleanup fails, to prevent Service stuck in Terminating")
	}

	// 断言 repo 记录仍在（cleanup 失败时不删记录，下一轮 reconcile 能重试）
	if exists, _ := repo.Exists("default/web"); !exists {
		t.Error("repo record must remain when cleanup fails")
	}
}

// TestReconcile_TypeFlip 测试类型翻转路径：LoadBalancer → ClusterIP，清理 agent 配置，清空 status，去 finalizer
func TestReconcile_TypeFlip(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "web",
			Finalizers: []string{DefaultFinalizer},
			Annotations: map[string]string{
				apis.SvcAnnInstances:   "10.0.0.1:8080", // 残留的注解（用户改 type 后可能忘了删）
				apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport),
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, // 不再是 LoadBalancer
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080)},
			},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}}, // 旧 status
			},
		},
	}

	r, syncer, repo := newTestReconciler([]client.Object{svc})
	_ = repo.Set("default/web", "10.0.0.1:8080") // 历史记录

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue after type flip cleanup")
	}
	updated := &corev1.Service{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(svc), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.AllocateLoadBalancerNodePorts != nil {
		t.Fatal("cleanup wrote LoadBalancer-only field to ClusterIP Service")
	}

	// 断言 cleanup 被调用（清理 repo 记录的实例）
	if len(syncer.cleanCalls) != 1 || syncer.cleanCalls[0].instance != "10.0.0.1:8080" {
		t.Errorf("cleanCalls = %+v", syncer.cleanCalls)
	}

	// 断言 repo 记录被删除
	if exists, _ := repo.Exists("default/web"); exists {
		t.Error("repo should be cleaned on type flip")
	}

	// 断言 status 被清空
	freshSvc := &corev1.Service{}
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, freshSvc)
	if len(freshSvc.Status.LoadBalancer.Ingress) != 0 {
		t.Errorf("status.ingress should be cleared on type flip, got %+v", freshSvc.Status.LoadBalancer.Ingress)
	}

	// 断言 finalizer 被摘除
	if len(freshSvc.Finalizers) != 0 {
		t.Errorf("finalizer should be removed on type flip, got %v", freshSvc.Finalizers)
	}
}

// TestReconcile_InstanceDiff 测试实例增减：cleanup 移除的，setup 当前的，repo 记录更新
func TestReconcile_InstanceDiff(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.2:8080,10.0.0.3:8080") // 当前注解：2 和 3
	svc.Finalizers = []string{DefaultFinalizer}
	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, repo := newTestReconciler([]client.Object{svc, n1, eps})
	_ = repo.Set("default/web", "10.0.0.1:8080,10.0.0.2:8080") // 历史记录：1 和 2

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue on success")
	}

	// 断言 cleanup 被调用 1 次（移除 10.0.0.1，它不在新注解里）
	if len(syncer.cleanCalls) != 1 || syncer.cleanCalls[0].instance != "10.0.0.1:8080" {
		t.Errorf("cleanCalls = %+v", syncer.cleanCalls)
	}

	// 断言 sync 被调用 2 次（当前注解的 10.0.0.2 和 10.0.0.3）
	if len(syncer.syncCalls) != 2 {
		t.Fatalf("expected 2 sync calls, got %d", len(syncer.syncCalls))
	}
	synced := map[string]bool{
		syncer.syncCalls[0].instance: true,
		syncer.syncCalls[1].instance: true,
	}
	if !synced["10.0.0.2:8080"] || !synced["10.0.0.3:8080"] {
		t.Errorf("sync instances: %v", synced)
	}

	// 断言 repo 记录更新为当前实例列表
	repoVal, _ := repo.Get("default/web")
	if repoVal != "10.0.0.2:8080,10.0.0.3:8080" {
		t.Errorf("repo = %q, want %q", repoVal, "10.0.0.2:8080,10.0.0.3:8080")
	}
}

// TestReconcile_SyncFailed 测试 setup 时部分 agent 同步失败：记录失败实例，返回 error 触发 requeue，标记 forceReload
func TestReconcile_SyncFailed(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080")
	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, repo := newTestReconciler([]client.Object{svc, n1, eps})
	syncer.syncErr = errors.New("agent returned 500")

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err == nil {
		t.Fatal("Reconcile should return error when sync fails")
	}

	// 断言 forceReload 被标记（下一轮即使配置无变更也强制 reload）
	if !r.shouldForceReload("default/web") {
		t.Error("forceReload should be set after sync failure")
	}

	// 失败也记录全部目标实例，供下次重试或删除时清理。
	repoVal, _ := repo.Get("default/web")
	if repoVal != "10.0.0.1:8080" {
		t.Errorf("repo should be set even when sync fails (for next retry), got %q", repoVal)
	}
}

// 首次同步、已有 status、不同失败顺序及全部失败均不能发布部分结果；恢复后一次写入完整 ingress。
func TestReconcile_SyncFailurePreservesStatusAndRecovers(t *testing.T) {
	const instances = "10.0.0.1:8080,10.0.0.2:8080"
	agentErr := errors.New("agent returned 500")
	for _, tt := range []struct {
		name       string
		oldIngress []corev1.LoadBalancerIngress
		failures   map[string]error
	}{
		{"initial partial failure", nil, map[string]error{"10.0.0.2:8080": agentErr}},
		{"failure before success", nil, map[string]error{"10.0.0.1:8080": agentErr}},
		{"preserve previous result", []corev1.LoadBalancerIngress{{IP: "10.0.0.9"}}, map[string]error{"10.0.0.2:8080": agentErr}},
		{"all failed", []corev1.LoadBalancerIngress{{IP: "10.0.0.9"}}, map[string]error{"10.0.0.1:8080": agentErr, "10.0.0.2:8080": agentErr}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := lbService("default", "web", instances)
			svc.Status.LoadBalancer.Ingress = tt.oldIngress
			// status 中其他控制器的字段也应保留。
			svc.Status.Conditions = []metav1.Condition{{
				Type: "OtherControllerReady", Status: metav1.ConditionTrue,
				Reason: "Ready", LastTransitionTime: metav1.Now(),
			}}
			r, syncer, repo := newTestReconciler([]client.Object{svc, node("n1", "192.168.1.1"), endpointSlice("default", "web")})
			syncer.syncErrors = tt.failures
			events := record.NewFakeRecorder(10)
			r.Eventer = events
			patches := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if sub == "status" {
						patches++
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			ctx := context.Background()
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"}}
			before := &corev1.Service{}
			if err := r.Get(ctx, req.NamespacedName, before); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Reconcile(ctx, req); !errors.Is(err, agentErr) {
				t.Fatalf("expected agent error for requeue, got %v", err)
			}
			fresh := &corev1.Service{}
			if err := r.Get(ctx, req.NamespacedName, fresh); err != nil {
				t.Fatal(err)
			}
			if patches != 0 || !reflect.DeepEqual(fresh.Status, before.Status) {
				t.Fatalf("failed setup wrote status: patches=%d, got %+v, want %+v", patches, fresh.Status, before.Status)
			}
			if !slices.Contains(fresh.Finalizers, DefaultFinalizer) {
				t.Fatal("partially configured service must retain cleanup finalizer")
			}
			if got, err := repo.Get("default/web"); err != nil || got != instances {
				t.Fatalf("must retain all instances for cleanup: %q, %v", got, err)
			}
			if !r.shouldForceReload("default/web") || len(syncer.syncCalls) != 2 {
				t.Fatal("must attempt all agents and mark ForceReload after failure")
			}
			for range tt.failures {
				select {
				case event := <-events.Events:
					if !strings.Contains(event, "Warning AgentSyncFailed") {
						t.Fatalf("expected failure event, got %q", event)
					}
				default:
					t.Fatal("missing agent failure event")
				}
			}
			select {
			case event := <-events.Events:
				t.Fatalf("unexpected event after failure: %q", event)
			default:
			}

			// 下一轮全部成功，发布完整结果并清除强制 reload 标记。
			syncer.syncErrors = nil
			syncer.syncCalls = nil
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, req.NamespacedName, fresh); err != nil {
				t.Fatal(err)
			}
			want := before.Status.DeepCopy()
			want.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}, {IP: "10.0.0.2"}}
			if patches != 1 || !reflect.DeepEqual(fresh.Status, *want) {
				t.Fatalf("recovery should publish full ingress: patches=%d, got %+v, want %+v", patches, fresh.Status, *want)
			}
			if r.shouldForceReload("default/web") || len(syncer.syncCalls) != 2 {
				t.Fatal("recovery should sync every instance and clear ForceReload")
			}
			for _, call := range syncer.syncCalls {
				if !call.forceReload {
					t.Fatalf("retry missing ForceReload: %+v", call)
				}
			}
			select {
			case event := <-events.Events:
				if !strings.Contains(event, "Normal service synced") {
					t.Fatalf("expected success event, got %q", event)
				}
			default:
				t.Fatal("missing success event after recovery")
			}
		})
	}
}

// 资源读取、finalizer 或 repo 失败也不能把本轮 setup 发布成成功。
func TestReconcile_SetupErrorDoesNotPublishStatus(t *testing.T) {
	for _, failure := range []string{"endpoints", "nodes", "secret", "finalizer", "repository", "status"} {
		t.Run(failure, func(t *testing.T) {
			svc := lbService("default", "web", "10.0.0.1:8080")
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.9"}}
			svc.Annotations[SvcAnnCertSecrets] = "tls-cert"
			failureErr := errors.New("injected " + failure + " error")
			r, _, repo := newTestReconciler([]client.Object{svc, testutil.TLSSecret(t, "default", "tls-cert", "example.com")})
			events := record.NewFakeRecorder(10)
			r.Eventer = events
			if failure == "repository" {
				repo.setErr = failureErr
			}
			patches := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					switch list.(type) {
					case *discovery.EndpointSliceList:
						if failure == "endpoints" {
							return failureErr
						}
					case *corev1.NodeList:
						if failure == "nodes" {
							return failureErr
						}
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Secret); ok && failure == "secret" {
						return failureErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if failure == "finalizer" {
						return failureErr
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					patches++
					if failure == "status" {
						return failureErr
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"}}
			if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, failureErr) {
				t.Fatalf("expected %v, got %v", failureErr, err)
			}
			fresh := &corev1.Service{}
			if err := r.Get(context.Background(), req.NamespacedName, fresh); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fresh.Status, svc.Status) || (failure != "status" && patches != 0) {
				t.Fatalf("failed setup must preserve status: patches=%d, status=%+v", patches, fresh.Status)
			}
			if !r.shouldForceReload("default/web") {
				t.Fatal("failed setup must mark ForceReload")
			}
			if failure != "repository" && failure != "finalizer" && failure != "secret" {
				if got, err := repo.Get("default/web"); err != nil || got != svc.Annotations[SvcAnnInstances] {
					t.Fatalf("must retain cleanup record: %q, %v", got, err)
				}
			}
			select {
			case event := <-events.Events:
				t.Fatalf("unexpected success event on setup failure: %q", event)
			default:
			}
		})
	}
}

// TestReconcile_ForceReloadCleared 测试成功后 forceReload 标记被清除
func TestReconcile_ForceReloadCleared(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080")
	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, _ := newTestReconciler([]client.Object{svc, n1, eps})
	// 预置 forceReload 标记（模拟上一轮失败）
	r.markForceReload("default/web")

	// 本轮成功
	syncer.syncErr = nil
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// 断言 forceReload 被清除
	if r.shouldForceReload("default/web") {
		t.Error("forceReload should be cleared after successful reconcile")
	}

	if len(syncer.syncCalls) != 1 || !syncer.syncCalls[0].forceReload {
		t.Errorf("retry should pass ForceReload=true: %+v", syncer.syncCalls)
	}
}

// TestReconcile_ServiceNotFound 测试 Service 不存在：直接返回成功（无需处理）
func TestReconcile_ServiceNotFound(t *testing.T) {
	r, syncer, repo := newTestReconciler([]client.Object{})

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "ghost"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for non-existent service")
	}

	// 断言 syncer 和 repo 均未被调用
	if len(syncer.syncCalls) != 0 || len(syncer.cleanCalls) != 0 {
		t.Error("syncer should not be called for non-existent service")
	}
	if len(repo.Keys()) != 0 {
		t.Error("repo should remain empty")
	}
}

// TestReconcile_AppendAgentPort 测试裸 IP 自动补端口
func TestReconcile_AppendAgentPort(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1") // 裸 IP，无端口
	n1 := node("n1", "192.168.1.1")
	eps := endpointSlice("default", "web")

	r, syncer, _ := newTestReconciler([]client.Object{svc, n1, eps})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// 断言 sync 调用的 instance 被补上了 :8080（默认 OPENLB_AGENT_PORT）
	if len(syncer.syncCalls) != 1 {
		t.Fatalf("expected 1 sync call, got %d", len(syncer.syncCalls))
	}
	if syncer.syncCalls[0].instance != "10.0.0.1:8080" {
		t.Errorf("bare IP should be appended with agent port, got %q", syncer.syncCalls[0].instance)
	}
}

// ========================================
// Mapper 函数测试
// ========================================

// newReconcilerWithIndexes 为 mapper 测试单独构造 reconciler，注册两个字段索引：
//   - SvcAnnCertSecrets：供 secretToServiceMapper 通过 MatchingFields 查 Service
//   - IndexServiceTypeLoadbalancer：供 nodeToServiceMapper 通过 MatchingFields 查 Service
//
// fake client 通过 WithIndex 支持自定义字段索引，与 controller.go 中
// mgr.GetFieldIndexer().IndexField 的语义一致。
func newReconcilerWithIndexes(objs []client.Object) *ServiceReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discovery.AddToScheme(scheme)
	_ = statev1.AddToScheme(scheme)

	// cert-secrets 注解索引：从 Service 的注解中拆出 secret 名列表
	certSecretsIndexer := func(obj client.Object) []string {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return nil
		}
		v := svc.GetAnnotations()[SvcAnnCertSecrets]
		if v == "" {
			return nil
		}
		// 与 controller.go 中 IndexField 使用的 utils.SplitCommaTrimSpace 保持一致
		parts := []string{}
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				parts = append(parts, p)
			}
		}
		return parts
	}

	// service-type 索引：只索引 LoadBalancer 类型
	svcTypeIndexer := func(obj client.Object) []string {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return nil
		}
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			return []string{string(corev1.ServiceTypeLoadBalancer)}
		}
		return nil
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Service{}, SvcAnnCertSecrets, certSecretsIndexer).
		WithIndex(&corev1.Service{}, IndexServiceTypeLoadbalancer, svcTypeIndexer).
		Build()

	return &ServiceReconciler{
		Client:      fakeClient,
		Log:         logr.Discard(),
		Eventer:     &record.FakeRecorder{},
		forceReload: make(map[string]struct{}),
	}
}

// TestSecretToServiceMapper 测试 Secret 变更时正确触发同命名空间的关联 Service
func TestSecretToServiceMapper(t *testing.T) {
	// svc1：引用了 tls-cert
	svc1 := lbService("default", "web", "10.0.0.1:8080")
	svc1.Annotations[SvcAnnCertSecrets] = "tls-cert"

	// svc2：逗号分隔，引用了 tls-cert 和 other-cert
	svc2 := lbService("default", "api", "10.0.0.2:8080")
	svc2.Annotations[SvcAnnCertSecrets] = "tls-cert,other-cert"

	// svc3：不同命名空间，即使引用同名 Secret 也不应被触发（mapper 按 Secret 命名空间过滤）
	svc3 := lbService("kube-system", "system-svc", "10.0.0.3:8080")
	svc3.Annotations[SvcAnnCertSecrets] = "tls-cert"

	// svc4：不引用 tls-cert，不应被触发
	svc4 := lbService("default", "db", "10.0.0.4:8080")
	delete(svc4.Annotations, SvcAnnCertSecrets)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tls-cert"},
		Type:       corev1.SecretTypeTLS,
	}

	r := newReconcilerWithIndexes([]client.Object{svc1, svc2, svc3, svc4, secret})
	requests := r.secretToServiceMapper(context.Background(), secret)

	// 预期：default/web 和 default/api（svc3 不同命名空间，svc4 无注解）
	got := make(map[string]bool)
	for _, req := range requests {
		got[req.Namespace+"/"+req.Name] = true
	}

	if !got["default/web"] {
		t.Error("expected default/web in requests (cert-secrets annotation matches)")
	}
	if !got["default/api"] {
		t.Error("expected default/api in requests (cert-secrets annotation matches via comma-list)")
	}
	// 不同命名空间：mapper 以 Secret 的 namespace 过滤，kube-system 的 Service 不会被返回
	if got["kube-system/system-svc"] {
		t.Error("should NOT include kube-system/system-svc (different namespace from secret)")
	}
	// 无注解：不应被触发
	if got["default/db"] {
		t.Error("should NOT include default/db (no cert-secrets annotation)")
	}
	if len(requests) != 2 {
		t.Errorf("expected 2 requests, got %d: %v", len(requests), requests)
	}
}

// TestSecretToServiceMapper_NoMatch 测试 Secret 没有关联任何 Service 时返回空
func TestSecretToServiceMapper_NoMatch(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1:8080")
	delete(svc.Annotations, SvcAnnCertSecrets) // 无注解

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "orphan-secret"},
	}

	r := newReconcilerWithIndexes([]client.Object{svc, secret})
	requests := r.secretToServiceMapper(context.Background(), secret)

	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
}

// TestEndpointsToServiceMapper 测试 EndpointSlice 变更触发所属 Service reconcile
func TestEndpointsToServiceMapper(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantLen    int
		wantNSName string // "namespace/name"，wantLen==1 时才检查
	}{
		{
			name:       "带 service-name 标签：触发对应 Service",
			labels:     map[string]string{discovery.LabelServiceName: "web"},
			wantLen:    1,
			wantNSName: "default/web",
		},
		{
			name:    "无 service-name 标签：不触发任何 Service",
			labels:  map[string]string{},
			wantLen: 0,
		},
		{
			name:    "service-name 标签为空字符串：不触发",
			labels:  map[string]string{discovery.LabelServiceName: ""},
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eps := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "web-abcde",
					Labels:    tt.labels,
				},
			}

			// endpointsToServiceMapper 不依赖字段索引，用普通 reconciler 即可
			r, _, _ := newTestReconciler([]client.Object{})
			requests := r.endpointsToServiceMapper(context.Background(), eps)

			if len(requests) != tt.wantLen {
				t.Fatalf("expected %d requests, got %d", tt.wantLen, len(requests))
			}
			if tt.wantLen == 0 {
				return
			}
			got := requests[0].Namespace + "/" + requests[0].Name
			if got != tt.wantNSName {
				t.Errorf("request = %q, want %q", got, tt.wantNSName)
			}
		})
	}
}

// TestEndpointsToServiceMapper_CrossNamespace 测试跨命名空间的 EndpointSlice
// mapper 直接用 EndpointSlice 自己的 Namespace，不会跨命名空间
func TestEndpointsToServiceMapper_CrossNamespace(t *testing.T) {
	eps := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kube-system",
			Name:      "dns-abcde",
			Labels:    map[string]string{discovery.LabelServiceName: "dns"},
		},
	}

	r, _, _ := newTestReconciler([]client.Object{})
	requests := r.endpointsToServiceMapper(context.Background(), eps)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Namespace != "kube-system" || requests[0].Name != "dns" {
		t.Errorf("request = %+v, want kube-system/dns", requests[0])
	}
}

// TestNodeToServiceMapper 测试 Node 变更时只触发 type=LoadBalancer 的 Service
func TestNodeToServiceMapper(t *testing.T) {
	svc1 := lbService("default", "web", "10.0.0.1:8080")
	svc2 := lbService("kube-system", "dns-lb", "10.0.0.2:8080")

	// ClusterIP Service 不应被触发
	clusterIPSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "internal"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
	// NodePort Service 也不应被触发
	nodePordSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nodeport-svc"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort},
	}

	n1 := node("n1", "192.168.1.1")

	r := newReconcilerWithIndexes([]client.Object{svc1, svc2, clusterIPSvc, nodePordSvc, n1})
	requests := r.nodeToServiceMapper(context.Background(), n1)

	got := make(map[string]bool)
	for _, req := range requests {
		got[req.Namespace+"/"+req.Name] = true
	}

	// 应触发两个 LoadBalancer Service
	if !got["default/web"] {
		t.Error("expected default/web in requests")
	}
	if !got["kube-system/dns-lb"] {
		t.Error("expected kube-system/dns-lb in requests")
	}
	// 非 LoadBalancer 类型不应被触发
	if got["default/internal"] {
		t.Error("should NOT include ClusterIP service default/internal")
	}
	if got["default/nodeport-svc"] {
		t.Error("should NOT include NodePort service default/nodeport-svc")
	}
	if len(requests) != 2 {
		t.Errorf("expected exactly 2 requests, got %d: %v", len(requests), requests)
	}
}

// TestNodeToServiceMapper_NoLBServices 测试集群里没有 LoadBalancer Service 时返回空
func TestNodeToServiceMapper_NoLBServices(t *testing.T) {
	clusterIPSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "internal"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
	n1 := node("n1", "192.168.1.1")

	r := newReconcilerWithIndexes([]client.Object{clusterIPSvc, n1})
	requests := r.nodeToServiceMapper(context.Background(), n1)

	if len(requests) != 0 {
		t.Errorf("expected 0 requests when no LoadBalancer services, got %d", len(requests))
	}
}

// TestReconcile_LoadBalancerClass 测试 loadBalancerClass 过滤逻辑
func TestReconcile_LoadBalancerClass(t *testing.T) {
	openlbClass := apis.LoadBalancerClass
	otherClass := "example.com/other-lb"

	tests := []struct {
		name              string
		loadBalancerClass *string
		hasInstancesAnn   bool
		expectSync        bool // 是否应该同步到 agent
		reason            string
	}{
		{
			name:              "指定 openlb class + 有注解",
			loadBalancerClass: &openlbClass,
			hasInstancesAnn:   true,
			expectSync:        true,
			reason:            "明确指定 openlb 且有实例注解，应处理",
		},
		{
			name:              "指定 openlb class + 无注解",
			loadBalancerClass: &openlbClass,
			hasInstancesAnn:   false,
			expectSync:        false,
			reason:            "虽然 class 匹配，但缺少 instances 注解，忽略",
		},
		{
			name:              "指定其他 class",
			loadBalancerClass: &otherClass,
			hasInstancesAnn:   true,
			expectSync:        false,
			reason:            "class 不匹配，应忽略（即使有注解）",
		},
		{
			name:              "未指定 class + 有注解（向后兼容）",
			loadBalancerClass: nil,
			hasInstancesAnn:   true,
			expectSync:        true,
			reason:            "向后兼容：未设 class 时，根据注解判断",
		},
		{
			name:              "未指定 class + 无注解",
			loadBalancerClass: nil,
			hasInstancesAnn:   false,
			expectSync:        false,
			reason:            "未设 class 且无注解，忽略",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 构造 Service
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Type:              corev1.ServiceTypeLoadBalancer,
					LoadBalancerClass: tt.loadBalancerClass,
					Ports: []corev1.ServicePort{
						{Port: 80, TargetPort: intstr.FromInt(8080)},
					},
				},
			}
			if tt.hasInstancesAnn {
				svc.Annotations = map[string]string{
					apis.SvcAnnInstances: "10.10.0.1:8080",
				}
			}

			// 创建 reconciler
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = discovery.AddToScheme(scheme)
			_ = statev1.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).WithStatusSubresource(&corev1.Service{}, &statev1.OpenLBServiceState{}).Build()

			mockSync := &mockSyncer{}
			mockRepo := newMockStore(fakeClient)

			r := &ServiceReconciler{
				Client:            fakeClient,
				Log:               logr.Discard(),
				Eventer:           record.NewFakeRecorder(10),
				Syncer:            mockSync,
				ServiceRepository: mockRepo,
				forceReload:       make(map[string]struct{}),
			}

			// 执行 Reconcile
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "test-svc",
				},
			})
			if err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			// 验证是否调用了 Sync
			gotSync := len(mockSync.syncCalls) > 0

			if gotSync != tt.expectSync {
				t.Errorf("%s: 期望 sync=%v，实际=%v\n同步调用: %v",
					tt.reason, tt.expectSync, gotSync, mockSync.syncCalls)
			}
		})
	}
}
