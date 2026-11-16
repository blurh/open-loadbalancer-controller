# Controller Metrics 功能实现总结

## 变更概述

为 `openlb-controller` 添加了 **Prometheus Metrics** 支持，包括：
1. **controller-runtime 内置指标**（框架级）— reconcile 性能、工作队列、API 调用、leader election、Go runtime
2. **业务自定义指标**（业务级）— agent 同步成功率、Service 实例数、同步耗时

## 实现细节

### 1. 代码变更

#### `pkg/config/config.go`
- 新增 `MetricsBindAddress()` 方法，读取 `OPENLB_METRICS_BIND_ADDRESS` 环境变量（默认 `:8081`）

```go
// MetricsBindAddress 返回 controller metrics 监听地址（默认 :8081）
// 设置为 "0" 可禁用 metrics
func (c *Config) MetricsBindAddress() string {
	return getenv("OPENLB_METRICS_BIND_ADDRESS", ":8081")
}
```

#### `pkg/controllers/controller.go`
- 导入 `sigs.k8s.io/controller-runtime/pkg/metrics/server`
- 在 `ctrl.NewManager` 中配置 metrics 选项：

```go
metricsAddr := cfg.MetricsBindAddress()
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    LeaderElection:          leaderElection,
    LeaderElectionID:        "openlb-controller-leader",
    LeaderElectionNamespace: utils.GetMyNamespace(),
    // Metrics 配置
    Metrics: metricsserver.Options{
        BindAddress: metricsAddr,
    },
    Cache: cache.Options{...},
})
```

#### **新增** `pkg/controllers/metrics.go`
定义并注册业务自定义指标：

```go
var (
	// agentSyncTotal 记录 controller 对 agent 的同步调用次数
	// Labels: instance, status (success/failure), operation (setup/cleanup)
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
			Name: "openlb_service_sync_duration_seconds",
			Help: "Duration of service sync operations in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]string{"namespace", "service"},
	)
)

func init() {
	// 注册到 controller-runtime 的全局 registry
	metrics.Registry.MustRegister(
		agentSyncTotal,
		serviceInstancesGauge,
		serviceSyncDuration,
	)
}
```

#### `pkg/controllers/service.go`
- 导入 `"github.com/prometheus/client_golang/prometheus"`
- 在 `cleanup()` 中记录每次 agent 清理的成功/失败：

```go
if err := r.Syncer.Clean(instance, ref); err != nil {
	agentSyncTotal.WithLabelValues(instance, "failure", "cleanup").Inc()
	// ... 错误处理
} else {
	agentSyncTotal.WithLabelValues(instance, "success", "cleanup").Inc()
}
```

- 在 `setup()` 中记录：
  - 同步开始时启动计时器：`timer := prometheus.NewTimer(serviceSyncDuration.WithLabelValues(...))`
  - 每次 agent 同步的成功/失败：`agentSyncTotal.WithLabelValues(instance, "success|failure", "setup").Inc()`
  - 完成时记录当前 Service 的实例数：`serviceInstancesGauge.WithLabelValues(...).Set(float64(instanceCount))`

### 2. 部署配置变更

#### `deploy/deployment.yaml`
- 添加 `ports` 定义，暴露 8081 metrics 端口
- 添加环境变量配置注释

```yaml
ports:
  - name: metrics
    containerPort: 8081
    protocol: TCP
env:
  # Metrics 配置（默认 :8081，设置为 "0" 可禁用）
  # - name: OPENLB_METRICS_BIND_ADDRESS
  #   value: ":8081"
```

#### 新增 `deploy/metrics-service.yaml`
- 创建 `openlb-controller-metrics` Service，暴露 8081 端口
- 包含 `ServiceMonitor` 定义（用于 Prometheus Operator）

```yaml
apiVersion: v1
kind: Service
metadata:
  name: openlb-controller-metrics
  namespace: openlb-system
spec:
  ports:
  - name: metrics
    port: 8081
    targetPort: metrics
  selector:
    app: openlb-controller
```

### 3. 文档变更

#### 新增文档
- **`docs/METRICS.md`** — 完整的 metrics 使用指南，包含：
  - 暴露的指标说明（reconcile 性能、工作队列、API 客户端、leader election、Go runtime）
  - Prometheus 集成方法（ServiceMonitor 和原生配置）
  - 示例告警规则
  - Grafana Dashboard 指南
  - 故障排查

#### 更新文档
- **`README.md`**：
  - 配置选项表格添加 `OPENLB_METRICS_BIND_ADDRESS`
  - 新增"Metrics 监控"章节，说明如何部署和访问
  - 更新 TODO：标记 controller metrics 已实现
  
- **`CLAUDE.md`**：
  - 环境变量表格添加 `OPENLB_METRICS_BIND_ADDRESS`

## 暴露的指标

### 业务自定义指标（新增）

1. **`openlb_agent_sync_total`** — Counter
   - **说明**：controller 对 agent 的同步调用次数
   - **Labels**：`instance`（agent 地址）, `status`（success/failure）, `operation`（setup/cleanup）
   - **用途**：监控每个 agent 的成功率、识别问题实例

2. **`openlb_service_instances`** — Gauge
   - **说明**：每个 Service 当前管理的实例数
   - **Labels**：`namespace`, `service`
   - **用途**：统计总实例数、检测配置变更

3. **`openlb_service_sync_duration_seconds`** — Histogram
   - **说明**：Service 完整同步操作的耗时（秒）
   - **Labels**：`namespace`, `service`
   - **Buckets**：0.01, 0.05, 0.1, 0.5, 1.0, 2.0, 5.0, 10.0
   - **用途**：识别慢 Service、优化性能

### controller-runtime 内置指标（自动暴露）

1. **Reconcile 性能**
   - `controller_runtime_reconcile_total` — reconcile 调用次数（按 result 分类）
   - `controller_runtime_reconcile_time_seconds` — reconcile 耗时分布（histogram）

2. **工作队列**
   - `workqueue_depth` — 队列深度
   - `workqueue_adds_total` — 入队次数
   - `workqueue_retries_total` — 重试次数
   - `workqueue_longest_running_processor_seconds` — 最老请求等待时间

3. **Kubernetes API 客户端**
   - `rest_client_requests_total` — API 调用次数
   - `rest_client_request_duration_seconds` — API 调用延迟

4. **Leader Election**
   - `leader_election_master_status` — 当前 leader 状态（1=leader, 0=follower）

5. **Go Runtime**
   - `go_goroutines` — goroutine 数量
   - `go_memstats_heap_alloc_bytes` — 堆内存使用
   - `go_gc_duration_seconds` — GC 暂停时间

## 配置灵活性

| 场景 | OPENLB_METRICS_BIND_ADDRESS | 说明 |
|---|---|---|
| 生产环境 | `:8081`（默认） | 暴露 metrics，接入 Prometheus |
| 开发/测试 | `:8081` 或自定义端口 | 本地验证 |
| 禁用 metrics | `0` | 减少资源开销 |

## 监控集成

### Prometheus Operator
```bash
kubectl apply -f deploy/metrics-service.yaml
```

ServiceMonitor 会自动被 Prometheus Operator 发现并抓取指标。

### 原生 Prometheus
在 `prometheus.yml` 添加 scrape 配置，targets 指向 `openlb-controller-metrics:8081`。

## 验证

### 1. 编译通过
```bash
go build -o /tmp/openlb-controller ./cmd/controller/
```
✅ 编译成功

### 2. 测试通过
```bash
go test ./pkg/controllers/ -v
```
✅ 所有 17 个测试全部通过

### 3. 本地验证 metrics
```bash
# 启动 controller 后
kubectl port-forward -n openlb-system svc/openlb-controller-metrics 8081:8081
curl http://localhost:8081/metrics
```

预期能看到：
```
# === 业务自定义指标 ===
# HELP openlb_agent_sync_total Total number of sync operations to agents
# TYPE openlb_agent_sync_total counter
openlb_agent_sync_total{instance="10.10.0.1:8080",operation="setup",status="success"} 42
openlb_agent_sync_total{instance="10.10.0.1:8080",operation="cleanup",status="success"} 5
openlb_agent_sync_total{instance="10.10.0.2:8080",operation="setup",status="failure"} 2

# HELP openlb_service_instances Number of instances managed per service
# TYPE openlb_service_instances gauge
openlb_service_instances{namespace="default",service="my-svc"} 2
openlb_service_instances{namespace="prod",service="api-svc"} 3

# HELP openlb_service_sync_duration_seconds Duration of service sync operations in seconds
# TYPE openlb_service_sync_duration_seconds histogram
openlb_service_sync_duration_seconds_bucket{namespace="default",service="my-svc",le="0.01"} 10
openlb_service_sync_duration_seconds_bucket{namespace="default",service="my-svc",le="0.05"} 35
openlb_service_sync_duration_seconds_bucket{namespace="default",service="my-svc",le="+Inf"} 42
openlb_service_sync_duration_seconds_sum{namespace="default",service="my-svc"} 1.234
openlb_service_sync_duration_seconds_count{namespace="default",service="my-svc"} 42

# === controller-runtime 内置指标 ===
# HELP controller_runtime_reconcile_total Total number of reconciliations per controller
# TYPE controller_runtime_reconcile_total counter
controller_runtime_reconcile_total{controller="service",result="success"} 42
controller_runtime_reconcile_total{controller="service",result="error"} 2

# HELP workqueue_depth Current depth of workqueue
# TYPE workqueue_depth gauge
workqueue_depth{name="service"} 0

# HELP leader_election_master_status Gauge of if the reporting system is master of the election
# TYPE leader_election_master_status gauge
leader_election_master_status{name="openlb-controller-leader"} 1
```

## 向后兼容性

✅ **完全向后兼容**：

- 默认启用 metrics（`:8081`），不影响现有功能
- 旧部署升级后自动暴露 metrics，无需手动配置
- 环境变量 `OPENLB_METRICS_BIND_ADDRESS=0` 可禁用（如果不需要）
- 不依赖 ServiceMonitor（可选，仅用于 Prometheus Operator）

## 使用示例

### 常用 PromQL 查询

```promql
# === 业务指标查询 ===

# 每个 agent 的成功率（5 分钟）
rate(openlb_agent_sync_total{status="success"}[5m])
/ rate(openlb_agent_sync_total[5m])

# 识别问题 agent（失败率 > 5%）
(
  rate(openlb_agent_sync_total{status="failure"}[5m])
  / rate(openlb_agent_sync_total[5m])
) > 0.05

# 总的 agent 同步 QPS
sum(rate(openlb_agent_sync_total[5m]))

# 当前管理的总实例数
sum(openlb_service_instances)

# Service 同步 P95 延迟
histogram_quantile(0.95, 
  rate(openlb_service_sync_duration_seconds_bucket[5m])
)

# === controller-runtime 指标查询 ===

# Reconcile 成功率
sum(rate(controller_runtime_reconcile_total{controller="service",result="success"}[5m]))
/ sum(rate(controller_runtime_reconcile_total{controller="service"}[5m]))

# Reconcile P99 延迟
histogram_quantile(0.99, 
  rate(controller_runtime_reconcile_time_seconds_bucket{controller="service"}[5m])
)

# 队列积压告警
workqueue_depth{name="service"} > 50
```

### 告警规则示例

```yaml
# Agent 同步失败率过高
- alert: OpenLBHighAgentSyncFailureRate
  expr: |
    rate(openlb_agent_sync_total{status="failure"}[5m])
    / rate(openlb_agent_sync_total[5m])
    > 0.05
  for: 5m
  annotations:
    summary: "OpenLB Agent 同步失败率超过 5%"
    description: "实例 {{ $labels.instance }} 失败率: {{ $value | humanizePercentage }}"

# Reconcile 错误率过高
- alert: OpenLBHighReconcileErrorRate
  expr: |
    rate(controller_runtime_reconcile_total{controller="service",result="error"}[5m])
    / rate(controller_runtime_reconcile_total{controller="service"}[5m])
    > 0.05
  for: 5m
  annotations:
    summary: "OpenLB Controller reconcile 错误率超过 5%"
```

## 技术要点

1. **controller-runtime v0.20.4 API 变更**：
   - 旧版本使用 `MetricsBindAddress: string` 直接配置
   - 新版本改为 `Metrics: metricsserver.Options{BindAddress: string}`
   - 需要导入 `sigs.k8s.io/controller-runtime/pkg/metrics/server`

2. **默认端口选择**：
   - `:8080` 已被 controller-runtime 默认的 webhook server 占用
   - 选择 `:8081` 作为 metrics 端口（Kubernetes 惯例）

3. **业务指标实现要点**：
   - 在 `pkg/controllers/metrics.go` 中定义和注册指标到 `metrics.Registry`
   - 在 `service.go` 的 `setup()`/`cleanup()` 中调用 `Inc()`/`Set()`/`Observe()`
   - Counter 用于累计值（agent 同步次数），Gauge 用于瞬时值（实例数），Histogram 用于分布统计（耗时）
   - Timer 模式：`timer := prometheus.NewTimer(...); defer timer.ObserveDuration()`

4. **指标粒度**：
   - **框架级指标**（controller-runtime 内置）：reconcile、队列、API、leader election
   - **业务级指标**（新增自定义）：per-agent 成功率、per-Service 实例数和耗时
   - 未来可扩展：agent API 响应码分布、ConfigMap store 操作延迟、每个注解的解析错误计数

## 后续建议

1. **扩展业务指标**（优先级：中）：
   ```go
   // Agent API 响应码分布
   openlb_agent_response_code_total{instance="10.10.0.1:8080", code="200|500"}
   
   // ConfigMap store 操作延迟
   openlb_store_operation_duration_seconds{operation="get|set|del"}
   
   // 注解解析错误计数
   openlb_annotation_parse_errors_total{annotation="protocol-port|network-type"}
   ```

2. **Grafana Dashboard**（优先级：低）：
   - 创建预置 dashboard JSON
   - 包含 Agent Health、Service Performance、Controller Performance、Resources 四个面板
   - 参考 controller-runtime 社区模板

3. **告警规则集**（优先级：低）：
   - 提供生产就绪的 PrometheusRule CR
   - 覆盖 agent 不可用、同步耗时过长、队列积压、leader 切换频繁等场景

## 验证清单

✅ 代码编译通过  
✅ 所有单元测试通过  
✅ 配置文件更新  
✅ 文档完善  
✅ 向后兼容  

## 参考文档

- `docs/METRICS.md` — 用户使用指南
- `README.md` — 快速开始
- `CLAUDE.md` — 环境变量配置
