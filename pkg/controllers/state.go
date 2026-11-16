package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *ServiceReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &corev1.Service{}
	err := r.reader().Get(ctx, req.NamespacedName, svc)
	found := !apierrors.IsNotFound(err)
	if err != nil && found {
		return ctrl.Result{}, err
	}
	state, err := r.ServiceRepository.Load(ctx, req.NamespacedName)
	if apierrors.IsNotFound(err) {
		state = nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !found && state == nil {
		return ctrl.Result{}, nil
	}
	instances := normalizedInstances(svc.Annotations[SvcAnnInstances])
	managed := found && svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		(svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass == apis.LoadBalancerClass) && len(instances) != 0
	// 同名新 Service 不能接管旧 UID 的清理记录。所有请求都按 namespace/name 入同一队列。
	oldIdentity := state != nil && found && state.Spec.ServiceRef.UID != "" && state.Spec.ServiceRef.UID != svc.UID
	stateDeleting := state != nil && !state.DeletionTimestamp.IsZero()
	if oldIdentity || stateDeleting || !managed || !svc.DeletionTimestamp.IsZero() {
		if state != nil {
			state, err = r.cleanupState(ctx, state, nil)
			if err != nil {
				return ctrl.Result{}, err
			}
			state, err = r.certificateProtector().replace(ctx, state, nil)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.ServiceRepository.Remove(ctx, state); err != nil {
				return ctrl.Result{}, err
			}
		}
		if found && !oldIdentity && (state != nil || controllerutil.ContainsFinalizer(svc, DefaultFinalizer)) {
			if err := r.finishService(ctx, svc); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.clearForceReload(req.NamespacedName.String())
		if found && managed && svc.DeletionTimestamp.IsZero() {
			// CR 手动删除或 UID 变更后，先等旧记录消失，再建立新记录。
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, nil
	}
	// 先创建带清理 finalizer 的 CR，再保护 Service；二者都成功后才允许外部副作用。
	state, err = r.ServiceRepository.Ensure(ctx, svc)
	if err != nil {
		r.markForceReload(req.NamespacedName.String())
		return ctrl.Result{}, err
	}
	if err := r.ensureServiceFinalizer(ctx, svc); err != nil {
		r.markForceReload(req.NamespacedName.String())
		return ctrl.Result{}, err
	}
	wanted := make(map[string]bool, len(instances))
	for _, address := range instances {
		wanted[address] = true
	}
	remove := make(map[string]bool)
	for _, instance := range state.Status.Instances {
		if !wanted[instance.Address] {
			remove[instance.Address] = true
		}
	}
	state, err = r.cleanupState(ctx, state, remove)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setup(ctx, svc, state, instances)
}

func (r *ServiceReconciler) ensureServiceFinalizer(ctx context.Context, svc *corev1.Service) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.Service{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(svc), current); err != nil {
			return err
		}
		if current.UID != svc.UID || !current.DeletionTimestamp.IsZero() {
			return store.ErrIdentityChanged
		}
		if controllerutil.ContainsFinalizer(current, DefaultFinalizer) {
			return nil
		}
		base := current.DeepCopy()
		controllerutil.AddFinalizer(current, DefaultFinalizer)
		return r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *ServiceReconciler) finishService(ctx context.Context, svc *corev1.Service) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.Service{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(svc), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != svc.UID {
			return store.ErrIdentityChanged
		}
		if current.DeletionTimestamp.IsZero() {
			base := current.DeepCopy()
			current.Status.LoadBalancer = corev1.LoadBalancerStatus{}
			if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		if !controllerutil.ContainsFinalizer(current, DefaultFinalizer) {
			return nil
		}
		base := current.DeepCopy()
		// 类型已改为非 LoadBalancer 时，API 不允许 allocateLoadBalancerNodePorts；
		// 清理仅移除 finalizer，不向新类型写回 LoadBalancer 专有字段。
		controllerutil.RemoveFinalizer(current, DefaultFinalizer)
		return r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// selected=nil 表示全部清理；已清理实例保留墓碑，重试时不会再次调用成功的 agent。
func (r *ServiceReconciler) cleanupState(ctx context.Context, state *statev1.OpenLBServiceState, selected map[string]bool) (*statev1.OpenLBServiceState, error) {
	var err error
	state, err = r.ServiceRepository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		for i := range status.Instances {
			entry := &status.Instances[i]
			if (selected == nil || selected[entry.Address]) && entry.Phase != statev1.InstanceCleaned {
				entry.Phase = statev1.InstancePendingDelete
			}
		}
	})
	if err != nil {
		return state, err
	}
	var errs []error
	for _, entry := range state.Status.Instances {
		if entry.Phase == statev1.InstanceCleaned || (selected != nil && !selected[entry.Address]) {
			continue
		}
		cleanErr := r.Syncer.Clean(agentAddress(entry.Address), sync.ServiceRef{Namespace: state.Namespace, Name: state.Spec.ServiceRef.Name})
		outcome := "success"
		if cleanErr != nil {
			outcome = "failure"
			errs = append(errs, cleanErr)
		}
		agentSyncTotal.WithLabelValues(entry.Address, outcome, "cleanup").Inc()
		next, saveErr := r.ServiceRepository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
			for i := range status.Instances {
				if status.Instances[i].Address != entry.Address {
					continue
				}
				status.Instances[i].LastError = errorMessage(cleanErr)
				if cleanErr == nil {
					status.Instances[i].Phase = statev1.InstanceCleaned
					status.Instances[i].Ingress = nil
				}
			}
			if cleanErr != nil {
				setReadyCondition(status, fmt.Errorf("cleanup %s: %w", entry.Address, cleanErr))
			}
		})
		if saveErr != nil {
			errs = append(errs, saveErr)
		} else {
			state = next
		}
		if cleanErr != nil {
			svc := &corev1.Service{}
			if r.reader().Get(ctx, types.NamespacedName{Namespace: state.Namespace, Name: state.Spec.ServiceRef.Name}, svc) == nil {
				r.Eventer.Eventf(svc, corev1.EventTypeWarning, "AgentCleanFailed", "cleanup agent %q failed: %v", entry.Address, cleanErr)
			}
		}
	}
	return state, errors.Join(errs...)
}

func (r *ServiceReconciler) prepareInstances(ctx context.Context, state *statev1.OpenLBServiceState, addresses []string, hash string) (*statev1.OpenLBServiceState, error) {
	return r.ServiceRepository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		old := make(map[string]statev1.InstanceState)
		for _, entry := range status.Instances {
			old[entry.Address] = entry
		}
		var planned []statev1.InstanceState
		for _, address := range addresses {
			entry := old[address]
			if entry.Phase != statev1.InstanceApplied || entry.ConfigHash != hash {
				entry.Phase = statev1.InstancePendingApply
				entry.ConfigHash = hash
			}
			entry.Address = address
			planned = append(planned, entry)
			delete(old, address)
		}
		for _, entry := range old {
			if entry.Phase != statev1.InstanceCleaned {
				planned = append(planned, entry)
			}
		}
		status.Instances = planned
	})
}

func setReadyCondition(status *statev1.ServiceStateStatus, err error) {
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced", Message: "All desired instances are synchronized"}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "SyncFailed"
		condition.Message = errorMessage(err)
	}
	meta.SetStatusCondition(&status.Conditions, condition)
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	message := []rune(err.Error())
	if len(message) > 2048 {
		message = message[:2048]
	}
	return string(message)
}

func agentAddress(address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	if !strings.Contains(address, ":") || net.ParseIP(address) != nil {
		return net.JoinHostPort(address, config.NewConfig().AgentPort())
	}
	return address
}

func normalizedInstances(value string) []string {
	set := make(map[string]bool)
	for _, address := range utils.SplitCommaTrimSpace(value) {
		if address != "" {
			set[agentAddress(address)] = true
		}
	}
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses
}

func instanceIngress(address string, resp sync.RespBody) []corev1.LoadBalancerIngress {
	var ingress []corev1.LoadBalancerIngress
	for _, ip := range resp.ExternalIPs {
		ingress = append(ingress, corev1.LoadBalancerIngress{IP: ip})
	}
	for _, ip := range resp.InternalIPs {
		ingress = append(ingress, corev1.LoadBalancerIngress{IP: ip})
	}
	// agent 已报告监听地址时不再追加管理地址，后者可能没有承载转发规则。
	if len(ingress) == 0 {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		ingress = append(ingress, corev1.LoadBalancerIngress{IP: host})
	}
	return ingress
}
