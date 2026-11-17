//go:build integration

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestCRDIntegration 使用真实 API 校验 schema、status 子资源、UID 不可变和 finalizer。
// envtest 不运行 namespace/GC controller；namespace 级清理由 controller 回归测试覆盖。
func TestCRDIntegration(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "install", "crd.yaml")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = statev1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := NewCRDStore(c, c)
	for _, ns := range []string{"team-a", "team-b"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
		if err := c.Create(ctx, svc); err != nil {
			t.Fatal(err)
		}
		state, err := s.Ensure(ctx, svc)
		if err != nil {
			t.Fatal(err)
		}
		if state.Spec.ServiceRef.UID != svc.UID || state.OwnerReferences[0].UID != svc.UID {
			t.Fatal("incorrect owner identity")
		}
		mutate := func(status *statev1.ServiceStateStatus) {
			status.Instances = []statev1.InstanceState{{Address: "agent:8080", Phase: statev1.InstancePendingApply}}
			status.CertificateRefs = []statev1.SecretReference{{Name: "tls", UID: "secret-uid"}}
		}
		state, err = s.UpdateStatus(ctx, state, mutate)
		if err != nil {
			t.Fatal(err)
		}
		rv := state.ResourceVersion
		if len(state.Status.CertificateRefs) != 1 || state.Status.CertificateRefs[0].UID != "secret-uid" {
			t.Fatal("CRD schema pruned certificate references")
		}
		state, err = s.UpdateStatus(ctx, state, mutate)
		if err != nil {
			t.Fatal(err)
		}
		if state.ResourceVersion != rv {
			t.Fatal("no-op wrote a new resource version")
		}
		invalid := state.DeepCopy()
		invalid.Spec.ServiceRef.UID = "replacement"
		if err := c.Update(ctx, invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("service UID must be immutable: %v", err)
		}
		invalid = state.DeepCopy()
		invalid.Status.Instances[0].Phase = "unknown"
		if err := c.Status().Update(ctx, invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid phase accepted: %v", err)
		}
		if err := c.Delete(ctx, state); err != nil {
			t.Fatal(err)
		}
		state, err = s.Load(ctx, client.ObjectKeyFromObject(svc))
		if err != nil {
			t.Fatal(err)
		}
		if state.DeletionTimestamp.IsZero() {
			t.Fatal("CR finalizer did not retain the record")
		}
		state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances[0].Phase = statev1.InstanceCleaned })
		if err != nil {
			t.Fatal(err)
		}
		// 实例已清理，但证书引用尚未释放，不能删除恢复记录。
		if err := s.Remove(ctx, state); err == nil {
			t.Fatal("removed state before releasing certificate references")
		}
		state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.CertificateRefs = nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(ctx, state); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(ctx, client.ObjectKeyFromObject(svc)); !apierrors.IsNotFound(err) {
			t.Fatalf("cleaned state not removed: %v", err)
		}
	}
}
