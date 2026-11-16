package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	stdsync "sync"
	"time"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/certificates"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Secret 与 Service 的 reconcile 共用此锁；引用登记和最后一个引用的释放不能交错。
// 多副本由 manager leader election 保证只有一个 writer；读取均绕过 informer 缓存。
type CertificateProtector struct {
	mu         stdsync.Mutex
	client     client.Client
	reader     client.Reader
	repository store.Repository
}

func (r *ServiceReconciler) certificateProtector() *CertificateProtector {
	r.certificatesOnce.Do(func() {
		r.certificates = &CertificateProtector{client: r.Client, reader: r.reader(), repository: r.ServiceRepository}
	})
	return r.certificates
}

func secretRefs(secrets []corev1.Secret) []statev1.SecretReference {
	var refs []statev1.SecretReference
	for _, secret := range secrets {
		refs = append(refs, statev1.SecretReference{Name: secret.Name, UID: secret.UID})
	}
	return refs
}

// acquire 先持久化引用并保护 Secret，再允许发送证书。失败时宁可多保留引用。
func (p *CertificateProtector) acquire(ctx context.Context, svc *corev1.Service, state *statev1.OpenLBServiceState) ([]corev1.Secret, *statev1.OpenLBServiceState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var secrets []corev1.Secret
	seen := make(map[string]bool)
	for _, name := range utils.SplitCommaTrimSpace(svc.Annotations[SvcAnnCertSecrets]) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		secret := &corev1.Secret{}
		if err := p.reader.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, secret); err != nil {
			return nil, state, fmt.Errorf("read required TLS Secret %s/%s: %w", svc.Namespace, name, err)
		}
		if _, err := certificates.Parse(*secret); err != nil {
			return nil, state, err
		}
		if secret.UID == "" {
			return nil, state, fmt.Errorf("TLS Secret %s/%s has no UID", svc.Namespace, name)
		}
		ref := statev1.SecretReference{Name: name, UID: secret.UID}
		if !secret.DeletionTimestamp.IsZero() && (!slices.Contains(state.Status.CertificateRefs, ref) || !controllerutil.ContainsFinalizer(secret, statev1.CertificateFinalizer)) {
			return nil, state, fmt.Errorf("cannot acquire deleting TLS Secret %s/%s", svc.Namespace, name)
		}
		secrets = append(secrets, *secret)
	}
	previous := slices.Clone(state.Status.CertificateRefs)
	next, err := p.repository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		for _, ref := range secretRefs(secrets) {
			if !slices.Contains(status.CertificateRefs, ref) {
				status.CertificateRefs = append(status.CertificateRefs, ref)
			}
		}
	})
	if err != nil {
		return nil, state, err
	}
	state = next
	if !state.DeletionTimestamp.IsZero() {
		return nil, state, store.ErrStateDeleting
	}
	for _, ref := range secretRefs(secrets) {
		if !slices.Contains(state.Status.CertificateRefs, ref) {
			return nil, state, errors.New("certificate references were not persisted; install the current OpenLBServiceState CRD schema")
		}
	}
	for i := range secrets {
		ref := statev1.SecretReference{Name: secrets[i].Name, UID: secrets[i].UID}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &corev1.Secret{}
			if err := p.reader.Get(ctx, client.ObjectKeyFromObject(&secrets[i]), current); err != nil {
				return err
			}
			if current.UID != ref.UID {
				return fmt.Errorf("TLS Secret %s/%s UID changed", current.Namespace, current.Name)
			}
			protected := controllerutil.ContainsFinalizer(current, statev1.CertificateFinalizer)
			if !current.DeletionTimestamp.IsZero() && (!protected || !slices.Contains(previous, ref)) {
				return fmt.Errorf("cannot acquire deleting TLS Secret %s/%s", current.Namespace, current.Name)
			}
			if !protected {
				base := current.DeepCopy()
				controllerutil.AddFinalizer(current, statev1.CertificateFinalizer)
				if err := p.client.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
				if !controllerutil.ContainsFinalizer(current, statev1.CertificateFinalizer) {
					return fmt.Errorf("TLS Secret %s/%s protection finalizer was not persisted", current.Namespace, current.Name)
				}
			}
			secrets[i] = *current
			return nil
		})
		if err != nil {
			return nil, state, err
		}
	}
	return secrets, state, nil
}

// replace 仅在所有 agent 确认成功后调用。先提交引用释放，再回收无主 finalizer。
func (p *CertificateProtector) replace(ctx context.Context, state *statev1.OpenLBServiceState, refs []statev1.SecretReference) (*statev1.OpenLBServiceState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := slices.Clone(state.Status.CertificateRefs)
	next, err := p.repository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		status.CertificateRefs = slices.Clone(refs)
	})
	if err != nil {
		return state, err
	}
	var errs []error
	for _, ref := range old {
		if slices.Contains(refs, ref) {
			continue
		}
		if err := p.releaseUnused(ctx, types.NamespacedName{Namespace: state.Namespace, Name: ref.Name}); err != nil {
			errs = append(errs, err)
		}
	}
	return next, errors.Join(errs...)
}

// 调用方必须持有 mu。List 失败时禁止释放，UID 避免同名 Secret 重建后串用引用。
func (p *CertificateProtector) releaseUnused(ctx context.Context, key types.NamespacedName) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := p.reader.Get(ctx, key, secret); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(secret, statev1.CertificateFinalizer) {
			return nil
		}
		states, err := p.repository.List(ctx, key.Namespace)
		if err != nil {
			return err
		}
		ref := statev1.SecretReference{Name: key.Name, UID: secret.UID}
		for _, state := range states {
			if slices.Contains(state.Status.CertificateRefs, ref) {
				return nil
			}
		}
		base := secret.DeepCopy()
		controllerutil.RemoveFinalizer(secret, statev1.CertificateFinalizer)
		return p.client.Patch(ctx, secret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// Secret 的启动 List 和 CR 引用变化事件恢复中断的释放操作，独立于 Service 是否还存在。
func (p *CertificateProtector) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.releaseUnused(ctx, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	// 可选 KV 后端没有 CR watch；周期检查保证状态释放后也能回收 finalizer。
	if _, crd := p.repository.(*store.CRDStore); !crd {
		secret := &corev1.Secret{}
		err := p.reader.Get(ctx, req.NamespacedName, secret)
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if controllerutil.ContainsFinalizer(secret, statev1.CertificateFinalizer) {
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
	}
	return ctrl.Result{}, nil
}

func stateToSecretMapper(_ context.Context, obj client.Object) []ctrl.Request {
	state := obj.(*statev1.OpenLBServiceState)
	var requests []ctrl.Request
	for _, ref := range state.Status.CertificateRefs {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: state.Namespace, Name: ref.Name}})
	}
	return requests
}
