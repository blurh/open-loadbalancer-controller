package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var ErrIdentityChanged = errors.New("service state identity changed")
var ErrStateDeleting = errors.New("service state is being deleted")

// Repository 的成功返回意味着状态已经持久化；mutate 必须可安全重试且不能调用 agent。
type Repository interface {
	Load(context.Context, types.NamespacedName) (*statev1.OpenLBServiceState, error)
	List(context.Context, string) ([]statev1.OpenLBServiceState, error)
	Ensure(context.Context, *corev1.Service) (*statev1.OpenLBServiceState, error)
	UpdateStatus(context.Context, *statev1.OpenLBServiceState, func(*statev1.ServiceStateStatus)) (*statev1.OpenLBServiceState, error)
	Remove(context.Context, *statev1.OpenLBServiceState) error
}

func NewServiceState(svc *corev1.Service) *statev1.OpenLBServiceState {
	owner := metav1.NewControllerRef(svc, corev1.SchemeGroupVersion.WithKind("Service"))
	block := false
	owner.BlockOwnerDeletion = &block
	return &statev1.OpenLBServiceState{
		TypeMeta: metav1.TypeMeta{APIVersion: statev1.GroupVersion.String(), Kind: "OpenLBServiceState"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: svc.Namespace, Name: svc.Name,
			Finalizers:      []string{statev1.CleanupFinalizer},
			OwnerReferences: []metav1.OwnerReference{*owner},
		},
		Spec: statev1.ServiceStateSpec{ServiceRef: statev1.ServiceReference{Name: svc.Name, UID: svc.UID}},
	}
}

func checkIdentity(current, expected *statev1.OpenLBServiceState) error {
	if current.Spec.ServiceRef != expected.Spec.ServiceRef || (expected.UID != "" && current.UID != expected.UID) {
		return fmt.Errorf("%w: %s/%s", ErrIdentityChanged, current.Namespace, current.Name)
	}
	return nil
}

func canonicalStatus(s *statev1.ServiceStateStatus) {
	sort.Slice(s.CertificateRefs, func(i, j int) bool {
		if s.CertificateRefs[i].Name == s.CertificateRefs[j].Name {
			return s.CertificateRefs[i].UID < s.CertificateRefs[j].UID
		}
		return s.CertificateRefs[i].Name < s.CertificateRefs[j].Name
	})
	if len(s.CertificateRefs) == 0 {
		s.CertificateRefs = nil
	}
	sort.Slice(s.Instances, func(i, j int) bool { return s.Instances[i].Address < s.Instances[j].Address })
	sort.Slice(s.Conditions, func(i, j int) bool { return s.Conditions[i].Type < s.Conditions[j].Type })
	if len(s.Instances) == 0 {
		s.Instances = nil
	}
	if len(s.Conditions) == 0 {
		s.Conditions = nil
	}
}

func statusChanged(a, b statev1.ServiceStateStatus) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	canonicalStatus(x)
	canonicalStatus(y)
	return !reflect.DeepEqual(x, y)
}

func cleanupComplete(state *statev1.OpenLBServiceState) bool {
	if len(state.Status.CertificateRefs) != 0 {
		return false
	}
	for _, instance := range state.Status.Instances {
		if instance.Phase != statev1.InstanceCleaned {
			return false
		}
	}
	return true
}
