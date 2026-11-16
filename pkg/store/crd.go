package store

import (
	"context"
	"fmt"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CRDStore 每个 Service 使用一个同 namespace 的 CR，没有全局仓库对象或启动快照缓存。
// reader 应使用 manager.GetAPIReader，避免写后读及 UID 检查被 informer 延迟影响。
type CRDStore struct {
	client client.Client
	reader client.Reader
}

var _ Repository = (*CRDStore)(nil)

func NewCRDStore(c client.Client, reader client.Reader) *CRDStore {
	return &CRDStore{client: c, reader: reader}
}

func (s *CRDStore) List(ctx context.Context, namespace string) ([]statev1.OpenLBServiceState, error) {
	states := &statev1.OpenLBServiceStateList{}
	if err := s.reader.List(ctx, states, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return states.Items, nil
}

func (s *CRDStore) Load(ctx context.Context, key types.NamespacedName) (*statev1.OpenLBServiceState, error) {
	state := &statev1.OpenLBServiceState{}
	if err := s.reader.Get(ctx, key, state); err != nil {
		return nil, err
	}
	if state.Spec.ServiceRef.Name != key.Name {
		return nil, fmt.Errorf("serviceRef.name must match state name: %s/%s", key.Namespace, key.Name)
	}
	return state, nil
}

func (s *CRDStore) Ensure(ctx context.Context, svc *corev1.Service) (*statev1.OpenLBServiceState, error) {
	wanted := NewServiceState(svc)
	var result *statev1.OpenLBServiceState
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.Load(ctx, client.ObjectKeyFromObject(svc))
		if apierrors.IsNotFound(err) {
			current = wanted.DeepCopy()
			err = s.client.Create(ctx, current)
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(statev1.GroupVersion.WithResource("openlbservicestates").GroupResource(), svc.Name, err)
			}
		}
		if err != nil {
			return err
		}
		if err := checkIdentity(current, wanted); err != nil {
			return err
		}
		if !current.DeletionTimestamp.IsZero() {
			return ErrStateDeleting
		}
		if !controllerutil.ContainsFinalizer(current, statev1.CleanupFinalizer) {
			base := current.DeepCopy()
			controllerutil.AddFinalizer(current, statev1.CleanupFinalizer)
			if err := s.client.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		result = current
		return nil
	})
	return result, err
}

func (s *CRDStore) UpdateStatus(ctx context.Context, expected *statev1.OpenLBServiceState, mutate func(*statev1.ServiceStateStatus)) (*statev1.OpenLBServiceState, error) {
	var result *statev1.OpenLBServiceState
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.Load(ctx, client.ObjectKeyFromObject(expected))
		if err != nil {
			return err
		}
		if err := checkIdentity(current, expected); err != nil {
			return err
		}
		before := current.Status.DeepCopy()
		mutate(&current.Status)
		canonicalStatus(&current.Status)
		if statusChanged(*before, current.Status) {
			if err := s.client.Status().Update(ctx, current); err != nil {
				return err
			}
		}
		result = current
		return nil
	})
	return result, err
}

// Remove 由 controller 确认外部清理完成后调用；UID/RV 条件防止误删同名新对象。
func (s *CRDStore) Remove(ctx context.Context, expected *statev1.OpenLBServiceState) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.Load(ctx, client.ObjectKeyFromObject(expected))
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := checkIdentity(current, expected); err != nil {
			return err
		}
		if !cleanupComplete(current) {
			return fmt.Errorf("cannot remove %s/%s: instances or certificate references still require cleanup", current.Namespace, current.Name)
		}
		if current.DeletionTimestamp.IsZero() {
			if err := s.client.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion}}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			current, err = s.Load(ctx, client.ObjectKeyFromObject(expected))
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := checkIdentity(current, expected); err != nil {
				return err
			}
			if !cleanupComplete(current) {
				return fmt.Errorf("cleanup state changed during deletion")
			}
		}
		if controllerutil.ContainsFinalizer(current, statev1.CleanupFinalizer) {
			base := current.DeepCopy()
			controllerutil.RemoveFinalizer(current, statev1.CleanupFinalizer)
			return s.client.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		return nil
	})
}
