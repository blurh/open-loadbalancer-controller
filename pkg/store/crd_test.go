package store

import (
	"context"
	"errors"
	"testing"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stateTestClient(t *testing.T) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = statev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&statev1.OpenLBServiceState{}).Build()
}

func stateTestService(ns string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web", UID: types.UID(ns + "-uid")}}
}

func TestCRDStoreNamespaceIsolationAndNoopWrites(t *testing.T) {
	ctx := context.Background()
	c := stateTestClient(t)
	writes := 0
	wrapped := interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	s := NewCRDStore(wrapped, c)
	for _, ns := range []string{"a", "b"} {
		state, err := s.Ensure(ctx, stateTestService(ns))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			_, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
				status.Instances = []statev1.InstanceState{{Address: ns + ":8080", Phase: statev1.InstanceApplied}}
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if writes != 2 {
		t.Fatalf("no-op wrote status: %d writes", writes)
	}
	state, err := s.Load(ctx, types.NamespacedName{Namespace: "a", Name: "web"})
	if err != nil || state.Status.Instances[0].Address != "a:8080" {
		t.Fatalf("namespace isolation: %+v %v", state, err)
	}
	// 外部状态变化应立即可见，不依赖进程启动时的快照。
	state.Status.Instances[0].LastError = "external change"
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	state, err = s.Load(ctx, client.ObjectKeyFromObject(state))
	if err != nil || state.Status.Instances[0].LastError != "external change" {
		t.Fatal("stale store cache")
	}
}

func TestCRDStoreIdentityAndCleanupGuard(t *testing.T) {
	ctx := context.Background()
	c := stateTestClient(t)
	s := NewCRDStore(c, c)
	svc := stateTestService("a")
	state, err := s.Ensure(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		status.Instances = []statev1.InstanceState{{Address: "agent:8080", Phase: statev1.InstancePendingApply}}
	})
	if err != nil {
		t.Fatal(err)
	}
	newSvc := svc.DeepCopy()
	newSvc.UID = "new-uid"
	if _, err := s.Ensure(ctx, newSvc); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("Ensure replaced old UID: %v", err)
	}
	if err := s.Remove(ctx, state); err == nil {
		t.Fatal("removed an uncleaned record")
	}
	if err := c.Delete(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(ctx, svc); !errors.Is(err, ErrStateDeleting) {
		t.Fatalf("Ensure accepted deleting state: %v", err)
	}
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances[0].Phase = statev1.InstanceCleaned })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, client.ObjectKeyFromObject(svc)); !apierrors.IsNotFound(err) {
		t.Fatalf("state not deleted: %v", err)
	}
	newState, err := s.Ensure(ctx, newSvc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances = nil }); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("stale writer accepted: %v", err)
	}
	if err := s.Remove(ctx, state); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("stale delete accepted: %v", err)
	}
	if _, err := s.Load(ctx, client.ObjectKeyFromObject(newState)); err != nil {
		t.Fatal(err)
	}
}

func TestCRDStoreConflictRetryKeepsUnrelatedProgress(t *testing.T) {
	ctx := context.Background()
	c := stateTestClient(t)
	s := NewCRDStore(c, c)
	state, err := s.Ensure(ctx, stateTestService("a"))
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		status.Instances = []statev1.InstanceState{{Address: "one", Phase: statev1.InstancePendingApply}, {Address: "two", Phase: statev1.InstancePendingApply}}
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.client = interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			calls++
			if calls == 1 {
				concurrent := &statev1.OpenLBServiceState{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), concurrent); err != nil {
					return err
				}
				concurrent.Status.Instances[1].Phase = statev1.InstanceApplied
				if err := c.Status().Update(ctx, concurrent); err != nil {
					return err
				}
				return apierrors.NewConflict(statev1.GroupVersion.WithResource("openlbservicestates").GroupResource(), obj.GetName(), errors.New("concurrent update"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances[0].Phase = statev1.InstanceApplied })
	if err != nil || calls != 2 {
		t.Fatalf("retry calls=%d error=%v", calls, err)
	}
	for _, entry := range state.Status.Instances {
		if entry.Phase != statev1.InstanceApplied {
			t.Fatalf("lost progress: %+v", state.Status)
		}
	}
}
