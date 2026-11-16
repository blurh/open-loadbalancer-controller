package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// agentOperationTotal 记录 agent 的 Apply/Delete 操作次数
	// Labels: operation (apply/delete), status (success/failure), service (namespace/name)
	agentOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openlb_agent_operation_total",
			Help: "Total number of agent operations (apply/delete)",
		},
		[]string{"operation", "status", "service"},
	)

	// agentOperationDuration 记录 agent 操作耗时（秒）
	// Labels: operation (apply/delete)
	agentOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "openlb_agent_operation_duration_seconds",
			Help:    "Duration of agent operations in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 2.0, 5.0},
		},
		[]string{"operation"},
	)

	// agentServicesManaged 记录当前 agent 管理的 Service 数量
	agentServicesManaged = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "openlb_agent_services_managed",
			Help: "Number of services currently managed by this agent",
		},
	)

	// agentReloadTotal 记录 nginx/lvs reload 次数
	// Labels: status (success/failure)
	agentReloadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openlb_agent_reload_total",
			Help: "Total number of forwarder reloads",
		},
		[]string{"status"},
	)

	// agentConnectionsGauge 记录转发器的连接数（按层级和状态分类）
	// l7: 从 nginx stub_status 采集（HTTP/HTTPS）
	// l4: 从 /proc/net/tcp 按 stream 端口过滤采集（TCP stream）
	// Labels: layer (l7/l4), state (active/inactive/waiting)
	agentConnectionsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openlb_agent_connections",
			Help: "Current number of connections by layer and state (layer=l7|l4, state=active|inactive|waiting)",
		},
		[]string{"layer", "state"},
	)

	// agentUDPSocketsGauge 记录 UDP 监听套接字数量
	// 从 /proc/net/udp 按监听端口过滤采集
	agentUDPSocketsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "openlb_agent_udp_sockets",
			Help: "Current number of UDP listening sockets",
		},
	)
)

func init() {
	// 注册到全局 registry
	prometheus.MustRegister(
		agentOperationTotal,
		agentOperationDuration,
		agentServicesManaged,
		agentReloadTotal,
		agentConnectionsGauge,
		agentUDPSocketsGauge,
	)
}

// RecordOperation 记录操作结果
func RecordOperation(operation, status, service string) {
	agentOperationTotal.WithLabelValues(operation, status, service).Inc()
}

// RecordReload 记录 reload 结果
func RecordReload(status string) {
	agentReloadTotal.WithLabelValues(status).Inc()
}

// SetServicesManaged 更新当前管理的 Service 数量
func SetServicesManaged(count int) {
	agentServicesManaged.Set(float64(count))
}

// SetConnectionsByLayer 更新指定层级(l7/l4)的连接数指标
func SetConnectionsByLayer(layer string, active, inactive, waiting int) {
	agentConnectionsGauge.WithLabelValues(layer, "active").Set(float64(active))
	agentConnectionsGauge.WithLabelValues(layer, "inactive").Set(float64(inactive))
	agentConnectionsGauge.WithLabelValues(layer, "waiting").Set(float64(waiting))
}

// SetUDPSockets 更新 UDP 监听套接字数量指标
func SetUDPSockets(count int) {
	agentUDPSocketsGauge.Set(float64(count))
}

// ObserveOperationDuration 返回一个 timer，用于记录操作耗时
// 用法: timer := metrics.ObserveOperationDuration("apply"); defer timer.ObserveDuration()
func ObserveOperationDuration(operation string) *prometheus.Timer {
	return prometheus.NewTimer(agentOperationDuration.WithLabelValues(operation))
}
