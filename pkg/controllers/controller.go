package controllers

import (
	"context"
	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"reflect"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/store"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
)

var (
	IndexServiceTypeLoadbalancer = "service-type-loadbalancer"
)

type ServiceController struct {
	mgr manager.Manager
}

func NewServiceController(cfg *config.Config, syncer sync.Syncer) *ServiceController {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discovery.AddToScheme(scheme)
	_ = statev1.AddToScheme(scheme)
	leaderElection := cfg.LeaderElectionEnabled()
	metricsAddr := cfg.MetricsBindAddress()
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Leader Election 配置
		// 启用后支持多副本，同一时刻只有 leader 执行 reconcile，非 leader 待命
		// 可通过 OPENLB_LEADER_ELECTION=false 禁用（单副本场景）
		LeaderElection:          leaderElection,
		LeaderElectionID:        "openlb-controller-leader",
		LeaderElectionNamespace: utils.GetMyNamespace(),
		// Metrics 配置
		// 暴露 Prometheus metrics 端点（默认 :8081/metrics）
		// 设置 OPENLB_METRICS_BIND_ADDRESS=0 可禁用
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
	})
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	// 构建 "kubernetes.io/openlb.cert-secrets" 的索引, 用于通过 secret 直接查到 svc
	err = mgr.GetFieldIndexer().IndexField(ctx,
		&corev1.Service{},
		SvcAnnCertSecrets,
		func(rawObj client.Object) []string {
			svc := rawObj.(*corev1.Service)
			secrets := svc.Annotations[SvcAnnCertSecrets]
			if secrets == "" {
				return nil
			}
			return utils.SplitCommaTrimSpace(secrets)
		},
	)
	if err != nil {
		panic(err)
	}

	// 构建 "service.Sepc.type" 索引
	err = mgr.GetFieldIndexer().IndexField(ctx,
		&corev1.Service{},
		IndexServiceTypeLoadbalancer,
		func(rawObj client.Object) []string {
			svc := rawObj.(*corev1.Service)
			if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
				return []string{string(svc.Spec.Type)}
			}
			return nil
		},
	)
	if err != nil {
		panic(err)
	}

	var storer store.Repository
	switch cfg.GetStoreType() {
	case "crd":
		storer = store.NewCRDStore(mgr.GetClient(), mgr.GetAPIReader())
	case "configmap", "file":
		storer = store.NewKVRepository(store.NewStore(cfg.GetStoreType()))
	default:
		panic("unsupported OPENLB_STORE_TYPE: " + cfg.GetStoreType())
	}
	r := &ServiceReconciler{
		APIReader:         mgr.GetAPIReader(),
		Client:            mgr.GetClient(),
		Log:               logf.Log.WithName(ManagerName),
		Eventer:           mgr.GetEventRecorderFor(ManagerName),
		ServiceRepository: storer,
		Syncer:            syncer,
		forceReload:       map[string]struct{}{},
	}

	// 只关心 LoadBalancer 类型 Service 的谓词。注意：必须用 per-watch 谓词挂在
	// For 上，而不是全局 WithEventFilter——后者会作用于所有 watch，把
	// Secret/EndpointSlice/Node 的事件一并拦掉（且 Delete 断言会 panic）。

	b := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(serviceEventPredicate())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToServiceMapper)).
		Watches(&discovery.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.endpointsToServiceMapper)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToServiceMapper),
			builder.WithPredicates(nodeEventPredicate()),
		)
	if cfg.GetStoreType() == "crd" {
		b = b.Watches(&statev1.OpenLBServiceState{}, handler.EnqueueRequestsFromMapFunc(r.stateToServiceMapper), builder.WithPredicates(stateEventPredicate()))
	}
	err = b.Complete(r)
	if err != nil {
		panic(err)
	}

	// 不按 Secret.type 过滤缓存：即使被改成非 TLS，也必须观察已有 finalizer。
	secretController := ctrl.NewControllerManagedBy(mgr).
		Named("openlb-certificate-protection").
		For(&corev1.Secret{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return slices.Contains(obj.GetFinalizers(), statev1.CertificateFinalizer)
		})))
	if cfg.GetStoreType() == "crd" {
		secretController = secretController.Watches(&statev1.OpenLBServiceState{}, handler.EnqueueRequestsFromMapFunc(stateToSecretMapper),
			builder.WithPredicates(predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
				return !reflect.DeepEqual(e.ObjectOld.(*statev1.OpenLBServiceState).Status.CertificateRefs, e.ObjectNew.(*statev1.OpenLBServiceState).Status.CertificateRefs)
			}}))
	}
	if err := secretController.Complete(r.certificateProtector()); err != nil {
		panic(err)
	}

	return &ServiceController{
		mgr: mgr,
	}
}

func (c *ServiceController) Start() {
	if err := c.mgr.Start(signals.SetupSignalHandler()); err != nil {
		panic("unable to start service controller manager " + err.Error())
	}
}

// 状态更新不触发自身的同步循环；启动 List、删除和身份/finalizer 变化仍触发恢复。
func stateEventPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok1 := e.ObjectOld.(*statev1.OpenLBServiceState)
			next, ok2 := e.ObjectNew.(*statev1.OpenLBServiceState)
			return ok1 && ok2 && (!reflect.DeepEqual(old.Spec, next.Spec) || !reflect.DeepEqual(old.Finalizers, next.Finalizers) || !old.DeletionTimestamp.Equal(next.DeletionTimestamp))
		},
	}
}

func (r *ServiceReconciler) stateToServiceMapper(ctx context.Context, obj client.Object) []ctrl.Request {
	state, ok := obj.(*statev1.OpenLBServiceState)
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(state)}}
}

// Service 的 annotation 更新不会改变 generation，不能使用 GenerationChangedPredicate。
func serviceEventPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return svc.Spec.Type == corev1.ServiceTypeLoadBalancer
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSvc, oldOk := e.ObjectOld.(*corev1.Service)
			newSvc, newOk := e.ObjectNew.(*corev1.Service)
			if !oldOk || !newOk {
				return false
			}
			// 覆盖 非LB->LB（建立）与 LB->非LB（清理）两种迁移
			return oldSvc.Spec.Type == corev1.ServiceTypeLoadBalancer ||
				newSvc.Spec.Type == corev1.ServiceTypeLoadBalancer
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return svc.Spec.Type == corev1.ServiceTypeLoadBalancer
		},
	}

}

func nodeEventPredicate() predicate.Funcs {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldNode, oldOK := e.ObjectOld.(*corev1.Node)
		newNode, newOK := e.ObjectNew.(*corev1.Node)
		if !oldOK || !newOK {
			return false
		}
		return utils.GetNodeStatus(oldNode) != utils.GetNodeStatus(newNode) ||
			utils.GetNodeInternalIP(oldNode) != utils.GetNodeInternalIP(newNode) ||
			utils.GetNodeExternalIP(oldNode) != utils.GetNodeExternalIP(newNode)
	}}
}
