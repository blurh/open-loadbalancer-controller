package controllers

import (
	"context"
	"errors"
	"strings"
	stdsync "sync"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/certificates"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

const (
	DefaultFinalizer = "service.kubernetes.io/load-balancer-cleanup"

	ManagerName = "open-loadbalancer-controller"

	// 注解 key 的真身在 pkg/apis，这里做别名以减少调用处改动。
	SvcAnnInstances       = apis.SvcAnnInstances
	SvcAnnClass           = apis.SvcAnnClass
	SvcAnnProtolPort      = apis.SvcAnnProtolPort
	SvcAnnCertSecrets     = apis.SvcAnnCertSecrets
	SvcAnnNetworkType     = apis.SvcAnnNetworkType
	SvcAnnNodeNetworkType = apis.SvcAnnNodeNetworkType
)

type NetworkType = apis.NetworkType

const (
	NetworkTypeNodeport = apis.NetworkTypeNodeport
	NetworkTypePod      = apis.NetworkTypePod
)

type NodeNetworkType = apis.NodeNetworkType

const (
	NodeNetworkTypeInternal = apis.NodeNetworkTypeInternal
	NodeNetworkTypeExternal = apis.NodeNetworkTypeExternal
)

type ServiceReconciler struct {
	client.Client
	Log               logr.Logger
	Eventer           record.EventRecorder
	ServiceRepository store.Repository
	APIReader         client.Reader
	Syncer            sync.Syncer
	certificates      *CertificateProtector
	certificatesOnce  stdsync.Once

	// forceReload 记录「上一轮 setup 失败」的 Service（key=namespace/name）。
	// 下一轮对这些 Service 在请求里置 ControllerResources.ForceReload，让 agent
	// 即便配置无变更也强制 reload——避免「reload 失败 → requeue → 配置无变更 →
	// agent 跳过 reload」导致 nginx 永久停在旧配置。CR 的 Ready/实例状态负责重启恢复。
	forceReload   map[string]struct{}
	forceReloadMu stdsync.Mutex
}

// shouldForceReload 返回该 Service 上一轮是否同步失败（需要强制 reload）。
func (r *ServiceReconciler) shouldForceReload(key string) bool {
	r.forceReloadMu.Lock()
	defer r.forceReloadMu.Unlock()
	_, ok := r.forceReload[key]
	return ok
}

// markForceReload 标记该 Service 下一轮需要强制 reload。
func (r *ServiceReconciler) markForceReload(key string) {
	r.forceReloadMu.Lock()
	defer r.forceReloadMu.Unlock()
	if r.forceReload == nil {
		r.forceReload = make(map[string]struct{})
	}
	r.forceReload[key] = struct{}{}
}

// clearForceReload 清除该 Service 的强制 reload 标记。
func (r *ServiceReconciler) clearForceReload(key string) {
	r.forceReloadMu.Lock()
	defer r.forceReloadMu.Unlock()
	delete(r.forceReload, key)
}

func (r *ServiceReconciler) secretToServiceMapper(ctx context.Context, secret client.Object) []reconcile.Request {
	var services corev1.ServiceList
	err := r.List(ctx,
		&services,
		client.InNamespace(secret.GetNamespace()),                  // Service 与其 cert Secret 必须同命名空间
		client.MatchingFields{SvcAnnCertSecrets: secret.GetName()}, // 使用索引
	)
	if err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, svc := range services.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: svc.Namespace,
				Name:      svc.Name,
			},
		})
	}
	return requests
}

func (r *ServiceReconciler) endpointsToServiceMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	// EndpointSlice 通过 label kubernetes.io/service-name 关联到所属 Service
	svcName := obj.GetLabels()[discovery.LabelServiceName]
	if svcName == "" {
		return nil
	}
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      svcName,
			},
		},
	}
}

func (r *ServiceReconciler) nodeToServiceMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	var services corev1.ServiceList
	err := r.List(ctx,
		&services,
		client.MatchingFields{IndexServiceTypeLoadbalancer: string(corev1.ServiceTypeLoadBalancer)},
	)
	if err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, svc := range services.Items {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: svc.Namespace,
				Name:      svc.Name,
			},
		})
	}
	return requests
}

func (r *ServiceReconciler) setup(ctx context.Context, svc *corev1.Service, state *statev1.OpenLBServiceState, instances []string) (retErr error) {
	namespacename := client.ObjectKeyFromObject(svc)
	nsname, namespace := namespacename.String(), svc.Namespace
	force := r.shouldForceReload(nsname) || meta.IsStatusConditionFalse(state.Status.Conditions, "Ready")
	for _, instance := range state.Status.Instances {
		if instance.Phase == statev1.InstancePendingApply {
			force = true
		}
	}
	defer func() {
		if retErr != nil {
			r.markForceReload(nsname)
		} else {
			r.clearForceReload(nsname)
		}
		if retErr != nil {
			_, err := r.ServiceRepository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { setReadyCondition(status, retErr) })
			retErr = errors.Join(retErr, err)
		}
	}()

	// 记录同步开始时间（用于计算 setup 总耗时）
	timer := prometheus.NewTimer(serviceSyncDuration.WithLabelValues(namespace, namespacename.Name))
	defer timer.ObserveDuration()

	var errs []error

	// Get endpointSlice
	endpointSlice := discovery.EndpointSlice{}
	var endpointSlices []discovery.EndpointSlice
	endpointSliceList := &discovery.EndpointSliceList{}
	labelMap := map[string]string{
		discovery.LabelServiceName: namespacename.Name,
	}
	if err := r.List(ctx, endpointSliceList,
		client.InNamespace(namespace),
		client.MatchingLabels(labelMap),
	); err != nil {
		// List 失败会让后端算空，必须暴露以触发重试，不能静默当零后端
		r.Log.Error(err, "list endpointslice error")
		errs = append(errs, err)
	}
	// 合并同一 Service 的多个 EndpointSlice；只取 IPv4 分片（后端地址当前按 IPv4 拼接），
	// DeepCopy 首片避免 append 改到 informer 缓存的底层数组。
	initialized := false
	for _, eps := range endpointSliceList.Items {
		if eps.AddressType != discovery.AddressTypeIPv4 {
			continue
		}
		endpointSlices = append(endpointSlices, *eps.DeepCopy())
		if !initialized {
			endpointSlice = *eps.DeepCopy()
			initialized = true
			continue
		}
		endpointSlice.Endpoints = append(endpointSlice.Endpoints, eps.Endpoints...)
	}
	// 引用必须持久化且 Secret finalizer 写入成功后才能下发。
	var secrets []corev1.Secret
	forwardingSvc := svc
	if parse.IsLVS(*svc) {
		if ignored := parse.IgnoredL7Fields(*svc); len(ignored) != 0 {
			r.Eventer.Eventf(svc, corev1.EventTypeWarning, "L7ConfigurationIgnored", "LVS supports only TCP/UDP; ignoring layer 7 configuration: %s. HTTPS traffic is forwarded without TLS termination", strings.Join(ignored, ", "))
		}
		resources := parse.LVSResources(sync.ControllerResources{Service: *svc})
		forwardingSvc = &resources.Service
	} else {
		var certErr error
		secrets, state, certErr = r.certificateProtector().acquire(ctx, svc, state)
		if certErr != nil {
			return certErr
		}
	}
	// List Node
	nodes := []corev1.Node{}
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, &client.ListOptions{}); err != nil {
		r.Log.Error(err, "list node error")
		errs = append(errs, err)
	}
	nodes = nodeList.Items
	reqBody := sync.ControllerResources{Service: *forwardingSvc, Nodes: nodes, EndpointSlice: endpointSlice, EndpointSlices: endpointSlices, Secrets: secrets, ForceReload: force}
	if err := certificates.Validate(reqBody); err != nil {
		return err
	}
	hash, err := forwardingHash(reqBody)
	if err != nil {
		return err
	}
	// 所有可能触达的实例先写入 CR，写入失败时不调用任何 agent。
	prepared, err := r.prepareInstances(ctx, state, instances, hash)
	if err != nil {
		return err
	}
	state = prepared
	if !state.DeletionTimestamp.IsZero() {
		return store.ErrStateDeleting
	}
	if len(errs) != 0 {
		return errors.Join(errs...)
	}
	ingress := []corev1.LoadBalancerIngress{}
	results := make(map[string]statev1.InstanceState)
	for _, instance := range instances {
		ips := &sync.RespBody{}
		entry := statev1.InstanceState{Address: instance, ConfigHash: hash, Phase: statev1.InstancePendingApply}
		err := r.Syncer.Sync(instance, reqBody, ips)
		if err != nil {
			entry.LastError = errorMessage(err)
			agentSyncTotal.WithLabelValues(instance, "failure", "setup").Inc()
			r.Eventer.Eventf(svc, corev1.EventTypeWarning, "AgentSyncFailed", "sync to agent %q failed: %v", instance, err)
			errs = append(errs, err)
		} else {
			entry.Phase = statev1.InstanceApplied
			entry.Ingress = instanceIngress(instance, *ips)
			ingress = append(ingress, entry.Ingress...)
			agentSyncTotal.WithLabelValues(instance, "success", "setup").Inc()
		}
		results[instance] = entry
	}
	next, err := r.ServiceRepository.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		for i := range status.Instances {
			if result, ok := results[status.Instances[i].Address]; ok {
				status.Instances[i] = result
			}
		}
		setReadyCondition(status, errors.Join(errs...))
	})
	if err != nil {
		errs = append(errs, err)
	} else {
		state = next
	}

	// 记录当前 Service 的实例数到 gauge
	instanceCount := len(instances)
	serviceInstancesGauge.WithLabelValues(namespace, namespacename.Name).Set(float64(instanceCount))

	// 本轮任一步骤失败时不发布部分 ingress，也不发送成功事件。
	// 保留上一次完整同步的 status；首次同步失败时 status 仍为空。
	if len(errs) != 0 {
		return errors.Join(errs...)
	}

	// 全部目标成功且结果已落盘，此时才允许释放旧证书。
	state, err = r.certificateProtector().replace(ctx, state, secretRefs(secrets))
	if err != nil {
		return err
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		freshSvc := &corev1.Service{}
		if err := r.reader().Get(ctx, namespacename, freshSvc); err != nil {
			return err
		}
		if freshSvc.UID != svc.UID || !freshSvc.DeletionTimestamp.IsZero() {
			return store.ErrIdentityChanged
		}
		desired := corev1.LoadBalancerStatus{Ingress: ingress}
		if equality.Semantic.DeepEqual(freshSvc.Status.LoadBalancer, desired) {
			return nil
		}
		updatedSvc := freshSvc.DeepCopy()
		updatedSvc.Status.LoadBalancer = desired
		return r.Status().Patch(ctx, updatedSvc, client.MergeFromWithOptions(freshSvc, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		r.Log.Error(err, "patch service status", "service", nsname)
		return err
	}
	r.Eventer.Eventf(svc, corev1.EventTypeNormal, "service synced", "service synced to agent: `%s`", strings.Join(instances, ","))
	return nil
}
