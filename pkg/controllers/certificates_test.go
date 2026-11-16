package controllers

import (
	"context"
	"errors"
	"slices"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/internal/testutil"
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
)

func reconcileCertificateService(t *testing.T, r *ServiceReconciler, svc *corev1.Service) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	return err
}

func changeCertificate(t *testing.T, r *ServiceReconciler, svc *corev1.Service, name string) {
	t.Helper()
	current := &corev1.Service{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(svc), current); err != nil {
		t.Fatal(err)
	}
	current.Annotations[SvcAnnCertSecrets] = name
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func assertSecretProtected(t *testing.T, c client.Client, secret *corev1.Secret, want bool) {
	t.Helper()
	current := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), current); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(current, statev1.CertificateFinalizer) != want {
		t.Fatalf("Secret %s finalizers = %v, protected want %v", secret.Name, current.Finalizers, want)
	}
}

func TestCertificateSwitchRetainsOldSecretThroughPartialFailureAndRestart(t *testing.T) {
	ctx := context.Background()
	svc := lbService("team-a", "web", "10.0.0.1:8080,10.0.0.2:8080")
	svc.Annotations[SvcAnnCertSecrets] = "old"
	old := testutil.TLSSecret(t, svc.Namespace, "old", "old.example.com")
	next := testutil.TLSSecret(t, svc.Namespace, "next", "new.example.com")
	r, base, repo := newTestReconciler([]client.Object{svc, old, next})
	r.Syncer = &checkingSyncer{mockSyncer: base, beforeSync: func(_ string, cr sync.ControllerResources) {
		state, err := repo.Load(ctx, client.ObjectKeyFromObject(svc))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range cr.Secrets {
			assertSecretProtected(t, r.Client, &secret, true)
			if !slices.Contains(state.Status.CertificateRefs, statev1.SecretReference{Name: secret.Name, UID: secret.UID}) {
				t.Fatal("agent called before certificate reference was persisted")
			}
		}
	}}
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	// DELETE 已接受，但已有使用者仍能读取和同步受保护的 Secret。
	if err := r.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	changeCertificate(t, r, svc, "next")
	base.syncErrors = map[string]error{"10.0.0.2:8080": errors.New("agent unavailable")}
	if err := reconcileCertificateService(t, r, svc); err == nil {
		t.Fatal("expected partial failure")
	}
	state, err := repo.Load(ctx, client.ObjectKeyFromObject(svc))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Status.CertificateRefs) != 2 {
		t.Fatalf("lost old/new certificate references: %+v", state.Status)
	}
	assertSecretProtected(t, r.Client, old, true)
	assertSecretProtected(t, r.Client, next, true)
	// 丢弃所有内存状态，仅凭 CR 恢复；两个 agent 成功后才完成旧 Secret 的删除。
	base.syncErrors = nil
	r = &ServiceReconciler{Client: r.Client, Log: r.Log, Eventer: r.Eventer, ServiceRepository: store.NewCRDStore(r.Client, r.Client), Syncer: base}
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(old), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old Secret not released: %v", err)
	}
	state, err = repo.Load(ctx, client.ObjectKeyFromObject(svc))
	if err != nil || len(state.Status.CertificateRefs) != 1 || state.Status.CertificateRefs[0].UID != next.UID {
		t.Fatalf("unexpected references: %+v, %v", state, err)
	}

	// 模拟 namespace 删除：Service、Secret、CR 同时 terminating，不互相等待删除完成。
	if err := r.Delete(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, state); err != nil {
		t.Fatal(err)
	}
	base.cleanErr = errors.New("cleanup failed")
	if err := reconcileCertificateService(t, r, svc); err == nil {
		t.Fatal("expected cleanup failure")
	}
	assertSecretProtected(t, r.Client, next, true)
	base.cleanErr = nil
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{next, svc, state} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("cleanup left %T: %v", obj, err)
		}
	}
}

func TestSharedSecretProtectionAndNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	one, two := lbService("team-a", "one", "10.0.0.1"), lbService("team-a", "two", "10.0.0.1")
	for _, svc := range []*corev1.Service{one, two} {
		svc.Annotations[SvcAnnCertSecrets] = "shared"
	}
	secret := testutil.TLSSecret(t, "team-a", "shared", "example.com")
	otherNS := testutil.TLSSecret(t, "team-b", "shared", "example.com")
	otherNS.Finalizers = []string{statev1.CertificateFinalizer, "example.com/other"}
	r, _, _ := newTestReconciler([]client.Object{one, two, secret, otherNS})
	for _, svc := range []*corev1.Service{one, two} {
		if err := reconcileCertificateService(t, r, svc); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	changeCertificate(t, r, one, "")
	if err := reconcileCertificateService(t, r, one); err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, secret, true)
	if _, err := r.certificateProtector().Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(otherNS)}); err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, otherNS, false)
	fresh := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(otherNS), fresh); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fresh.Finalizers, "example.com/other") {
		t.Fatal("removed unrelated finalizer")
	}
	changeCertificate(t, r, two, "")
	if err := reconcileCertificateService(t, r, two); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), fresh); !apierrors.IsNotFound(err) {
		t.Fatalf("last reference did not release Secret: %v", err)
	}
}

func TestCertificateAcquisitionFailuresNeverCallAgent(t *testing.T) {
	for _, failure := range []string{"missing", "invalid", "deleting", "uid-replaced", "finalizer-write"} {
		t.Run(failure, func(t *testing.T) {
			svc := lbService("default", "web", "10.0.0.1")
			svc.Annotations[SvcAnnCertSecrets] = "tls"
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.9"}}
			secret := testutil.TLSSecret(t, svc.Namespace, "tls", "example.com")
			objects := []client.Object{svc}
			if failure == "invalid" {
				delete(secret.Data, corev1.TLSPrivateKeyKey)
			}
			if failure == "deleting" || failure == "uid-replaced" {
				now := metav1.Now()
				secret.DeletionTimestamp = &now
				secret.Finalizers = []string{statev1.CertificateFinalizer}
			}
			if failure != "missing" {
				objects = append(objects, secret)
			}
			r, syncer, repo := newTestReconciler(objects)
			if failure == "uid-replaced" {
				state, err := repo.Ensure(context.Background(), svc)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repo.UpdateStatus(context.Background(), state, func(s *statev1.ServiceStateStatus) {
					s.CertificateRefs = []statev1.SecretReference{{Name: secret.Name, UID: "old-uid"}}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "finalizer-write" {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return errors.New("finalizer write failed")
					}
					return c.Patch(ctx, obj, patch, opts...)
				}})
			}
			if err := reconcileCertificateService(t, r, svc); err == nil {
				t.Fatal("expected acquisition failure")
			}
			if len(syncer.syncCalls) != 0 {
				t.Fatal("incomplete/unprotected certificate was sent to agent")
			}
			current := &corev1.Service{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(svc), current); err != nil {
				t.Fatal(err)
			}
			if len(current.Status.LoadBalancer.Ingress) != 1 || current.Status.LoadBalancer.Ingress[0].IP != "10.0.0.9" {
				t.Fatal("failure published Service status")
			}
		})
	}
}

func TestCertificateResultWriteFailureKeepsBothReferences(t *testing.T) {
	svc := lbService("default", "web", "10.0.0.1")
	svc.Annotations[SvcAnnCertSecrets] = "old"
	old := testutil.TLSSecret(t, svc.Namespace, "old", "old.example.com")
	next := testutil.TLSSecret(t, svc.Namespace, "new", "new.example.com")
	r, base, repo := newTestReconciler([]client.Object{svc, old, next})
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	changeCertificate(t, r, svc, "new")
	r.Syncer = &checkingSyncer{mockSyncer: base, beforeSync: func(_ string, _ sync.ControllerResources) { repo.setErr = errors.New("cannot persist agent result") }}
	if err := reconcileCertificateService(t, r, svc); err == nil || !strings.Contains(err.Error(), "cannot persist") {
		t.Fatalf("got %v", err)
	}
	state, err := repo.Load(context.Background(), client.ObjectKeyFromObject(svc))
	if err != nil || len(state.Status.CertificateRefs) != 2 {
		t.Fatalf("lost references: %+v, %v", state, err)
	}
	assertSecretProtected(t, r.Client, old, true)
	assertSecretProtected(t, r.Client, next, true)
}

type pausedCertificateRepository struct {
	store.Repository
	once       stdsync.Once
	registered chan struct{}
	resume     chan struct{}
}

func (p *pausedCertificateRepository) UpdateStatus(ctx context.Context, state *statev1.OpenLBServiceState, mutate func(*statev1.ServiceStateStatus)) (*statev1.OpenLBServiceState, error) {
	next, err := p.Repository.UpdateStatus(ctx, state, mutate)
	if err == nil && len(next.Status.CertificateRefs) != 0 {
		p.once.Do(func() { close(p.registered); <-p.resume })
	}
	return next, err
}

func TestSecretReleaseSerializesWithNewReference(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1")
	svc.Annotations[SvcAnnCertSecrets] = "tls"
	secret := testutil.TLSSecret(t, svc.Namespace, "tls", "example.com")
	secret.Finalizers = []string{statev1.CertificateFinalizer}
	r, _, repo := newTestReconciler([]client.Object{svc, secret})
	state, err := repo.Ensure(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedCertificateRepository{Repository: repo, registered: make(chan struct{}), resume: make(chan struct{})}
	p := &CertificateProtector{client: r.Client, reader: r.Client, repository: paused}
	acquired, released := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := p.acquire(ctx, svc, state); acquired <- err }()
	<-paused.registered
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := p.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)})
		released <- err
	}()
	<-started
	close(paused.resume)
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, secret, true)
}

func TestOrphanSecretRecoveryFailsClosedOnReferenceReadError(t *testing.T) {
	ctx := context.Background()
	secret := testutil.TLSSecret(t, "default", "orphan", "example.com")
	secret.Finalizers = []string{statev1.CertificateFinalizer}
	r, _, _ := newTestReconciler([]client.Object{secret})
	failedReader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("reference list unavailable")
	}})
	p := &CertificateProtector{client: r.Client, reader: r.Client, repository: store.NewCRDStore(r.Client, failedReader)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(secret)}
	if _, err := p.Reconcile(ctx, req); err == nil {
		t.Fatal("failed reference list allowed release")
	}
	assertSecretProtected(t, r.Client, secret, true)
	// 模拟引用已释放但 controller 在移除 Secret finalizer 前退出，启动扫描负责补偿。
	p.repository = store.NewCRDStore(r.Client, r.Client)
	if _, err := p.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, secret, false)
}
