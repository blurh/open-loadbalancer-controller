package controllers

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// agentSyncTotal 记录 controller 对 agent 的同步调用次数
	// Labels: instance (agent 地址), status (success/failure), operation (setup/cleanup)
	agentSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openlb_agent_sync_total",
			Help: "Total number of sync operations to agents",
		},
		[]string{"instance", "status", "operation"},
	)

	// serviceInstancesGauge 记录每个 Service 当前管理的实例数
	// Labels: namespace, service
	serviceInstancesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openlb_service_instances",
			Help: "Number of instances managed per service",
		},
		[]string{"namespace", "service"},
	)

	// serviceSyncDuration 记录每个 Service 的完整同步耗时（秒）
	// Labels: namespace, service
	serviceSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "openlb_service_sync_duration_seconds",
			Help:    "Duration of service sync operations in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]string{"namespace", "service"},
	)
)

func init() {
	// 注册到 controller-runtime 的全局 registry
	// 这些指标会自动暴露在 /metrics 端点
	metrics.Registry.MustRegister(
		agentSyncTotal,
		serviceInstancesGauge,
		serviceSyncDuration,
	)
}
