package controllers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type checkingSyncer struct {
	*mockSyncer
	beforeSync  func(string, sync.ControllerResources)
	cleanErrors map[string]error
}

func (s *checkingSyncer) Sync(address string, cr sync.ControllerResources, resp *sync.RespBody) error {
	if s.beforeSync != nil {
		s.beforeSync(address, cr)
	}
	return s.mockSyncer.Sync(address, cr, resp)
}
func (s *checkingSyncer) Clean(address string, ref sync.ServiceRef) error {
	if err := s.mockSyncer.Clean(address, ref); err != nil {
		return err
	}
	return s.cleanErrors[address]
}

func TestStatePersistedBeforeAgentAndNoopReconcile(t *testing.T) {
	ctx := context.Background()
	svc := lbService("team-a", "web", "10.0.0.1,10.0.0.2:8080")
	r, base, repo := newTestReconciler([]client.Object{svc})
	key := client.ObjectKeyFromObject(svc)
	r.Syncer = &checkingSyncer{mockSyncer: base, beforeSync: func(address string, cr sync.ControllerResources) {
		state, err := repo.Load(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		current := &corev1.Service{}
		if err := r.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		if !controllerutil.ContainsFinalizer(current, DefaultFinalizer) || !controllerutil.ContainsFinalizer(state, statev1.CleanupFinalizer) || len(state.Status.Instances) != 2 {
			t.Fatalf("agent called before all cleanup records were persisted: %+v", state)
		}
	}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// 实例顺序、重复项和其他元数据不改变转发状态。
	current := &corev1.Service{}
	if err := r.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	current.Annotations[SvcAnnInstances] = "10.0.0.2:8080,10.0.0.1:8080,10.0.0.1"
	current.Annotations["example.com/note"] = "changed"
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion || !reflect.DeepEqual(first.Status, second.Status) {
		t.Fatalf("unchanged forwarding config rewrote CR: %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestInstanceIngressUsesReportedListenerAddresses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response sync.RespBody
		want     []corev1.LoadBalancerIngress
	}{
		{name: "local private IP", response: sync.RespBody{InternalIPs: []string{"10.1.0.10"}}, want: []corev1.LoadBalancerIngress{{IP: "10.1.0.10"}}},
		{name: "explicit VIP", response: sync.RespBody{ExternalIPs: []string{"192.0.2.10"}}, want: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}},
		{name: "mixed listeners", response: sync.RespBody{ExternalIPs: []string{"192.0.2.10"}, InternalIPs: []string{"10.1.0.10"}}, want: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}, {IP: "10.1.0.10"}}},
		{name: "nginx empty response fallback", want: []corev1.LoadBalancerIngress{{IP: "10.0.0.99"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceIngress("10.0.0.99:8080", tc.response); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ingress = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPendingStateForcesReloadAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1:8080")
	r, syncer, repo := newTestReconciler([]client.Object{svc})
	state, err := repo.Ensure(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		status.Instances = []statev1.InstanceState{{Address: "10.0.0.1:8080", Phase: statev1.InstancePendingApply}}
	})
	if err != nil {
		t.Fatal(err)
	}
	// 没有内存 forceReload、也没有上轮失败 Condition，模拟发送后进程直接退出。
	r.forceReload = nil
	r.ServiceRepository = store.NewCRDStore(r.Client, r.Client)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatal(err)
	}
	if len(syncer.syncCalls) != 1 || !syncer.syncCalls[0].forceReload {
		t.Fatal("pending state was not recovered")
	}
}

func TestCleanupProgressSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1:8080,10.0.0.2:8080")
	r, base, repo := newTestReconciler([]client.Object{svc})
	key := client.ObjectKeyFromObject(svc)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	current := &corev1.Service{}
	if err := r.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("agent offline")
	r.Syncer = &checkingSyncer{mockSyncer: base, cleanErrors: map[string]error{"10.0.0.2:8080": failure}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); !errors.Is(err, failure) {
		t.Fatalf("expected cleanup failure, got %v", err)
	}
	state, err := repo.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status.Instances[0].Phase != statev1.InstanceCleaned || state.Status.Instances[1].Phase != statev1.InstancePendingDelete {
		t.Fatalf("missing cleanup progress: %+v", state.Status)
	}
	restarted := &mockSyncer{}
	r.Syncer = restarted
	r.forceReload = nil
	r.ServiceRepository = store.NewCRDStore(r.Client, r.Client)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.cleanCalls) != 1 || restarted.cleanCalls[0].instance != "10.0.0.2:8080" {
		t.Fatalf("repeated successful cleanup: %+v", restarted.cleanCalls)
	}
	if _, err := repo.Load(ctx, key); !apierrors.IsNotFound(err) {
		t.Fatalf("record remained: %v", err)
	}
}

func TestOldUIDCleanupBeforeApplyingRecreatedService(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.2:8080")
	r, syncer, _ := newTestReconciler([]client.Object{svc})
	old := store.NewServiceState(svc)
	old.Spec.ServiceRef.UID = "old-uid"
	old.Status.Instances = []statev1.InstanceState{{Address: "10.0.0.1:8080", Phase: statev1.InstanceApplied}}
	if err := r.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(svc)
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil || result.RequeueAfter == 0 || len(syncer.syncCalls) != 0 || len(syncer.cleanCalls) != 1 {
		t.Fatalf("new service bypassed old cleanup: %+v %v %+v", result, err, syncer)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(syncer.syncCalls) != 1 {
		t.Fatal("new Service was not synchronized after old cleanup")
	}
}

func TestCRDeletionAndOrphanCleanup(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual CR deletion", true: "Service absent"}[orphan], func(t *testing.T) {
			ctx := context.Background()
			svc := lbService("default", "web", "10.0.0.1:8080")
			var objs []client.Object
			if !orphan {
				objs = append(objs, svc)
			}
			r, syncer, repo := newTestReconciler(objs)
			state := store.NewServiceState(svc)
			state.Status.Instances = []statev1.InstanceState{{Address: "10.0.0.1:8080", Phase: statev1.InstancePendingApply}}
			if err := r.Create(ctx, state); err != nil {
				t.Fatal(err)
			}
			if !orphan {
				if err := r.Delete(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
				t.Fatal(err)
			}
			if len(syncer.cleanCalls) != 1 || len(syncer.syncCalls) != 0 {
				t.Fatalf("wrong recovery: %+v", syncer)
			}
			if _, err := repo.Load(ctx, client.ObjectKeyFromObject(svc)); !apierrors.IsNotFound(err) {
				t.Fatalf("orphan state remained: %v", err)
			}
		})
	}
}

func TestReservationWriteFailurePreventsAgentCalls(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1:8080")
	r, syncer, _ := newTestReconciler([]client.Object{svc})
	failure := errors.New("status write failed")
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return failure
		},
	})
	r.ServiceRepository = store.NewCRDStore(c, c)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); !errors.Is(err, failure) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	if len(syncer.syncCalls) != 0 {
		t.Fatal("called agent without persisted reservation")
	}
}

func TestStateWatchIgnoresStatusButHandlesDeletion(t *testing.T) {
	p := stateEventPredicate()
	old := store.NewServiceState(lbService("default", "web", "agent"))
	changed := old.DeepCopy()
	changed.Status.Instances = []statev1.InstanceState{{Address: "agent", Phase: statev1.InstanceApplied}}
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) {
		t.Fatal("status update causes reconcile loop")
	}
	now := metav1.Now()
	changed.DeletionTimestamp = &now
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) {
		t.Fatal("CR deletion ignored")
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("startup/deletion recovery disabled")
	}
}

func TestChangedConfigurationIsPersistedBeforeRetry(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1:8080")
	eps := endpointSlice("default", "web")
	r, base, repo := newTestReconciler([]client.Object{svc, eps})
	key := client.ObjectKeyFromObject(svc)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	state, err := repo.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := state.Status.Instances[0].ConfigHash
	if err := r.Get(ctx, client.ObjectKeyFromObject(eps), eps); err != nil {
		t.Fatal(err)
	}
	eps.Endpoints[0].Addresses = []string{"10.244.1.99"}
	if err := r.Update(ctx, eps); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("reload failed")
	base.syncErr = failure
	r.Syncer = &checkingSyncer{mockSyncer: base, beforeSync: func(address string, cr sync.ControllerResources) {
		current, err := repo.Load(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		entry := current.Status.Instances[0]
		if entry.Phase != statev1.InstancePendingApply || entry.ConfigHash == oldHash {
			t.Fatalf("changed configuration not recorded before send: %+v", entry)
		}
	}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); !errors.Is(err, failure) {
		t.Fatalf("want failure, got %v", err)
	}
	state, err = repo.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status.Instances[0].Phase != statev1.InstancePendingApply || state.Status.Instances[0].LastError == "" {
		t.Fatal("failed operation lost its retry state")
	}
}
