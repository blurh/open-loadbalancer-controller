# Metrics 使用说明

## 概述

`openlb-controller` 和 `openlb-agent` 均暴露 **Prometheus metrics** 端点，用于监控运行状态、性能和健康度。

## 默认配置

### Controller
- **端点地址**：`:8081/metrics`
- **格式**：Prometheus text format
- **认证**：无（建议通过 NetworkPolicy 限制访问）

### Agent
- **端点地址**：`:8082/metrics`
- **格式**：Prometheus text format
- **认证**：无（建议通过 NetworkPolicy 限制访问）

## 配置

### 环境变量

#### Controller

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENLB_METRICS_BIND_ADDRESS` | `:8081` | controller metrics 监听地址 |

**禁用 metrics**：
```yaml
env:
  - name: OPENLB_METRICS_BIND_ADDRESS
    value: "0"
```

**自定义端口**：
```yaml
env:
  - name: OPENLB_METRICS_BIND_ADDRESS
    value: ":9090"
```

#### Agent

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENLB_AGENT_METRICS_ADDRESS` | `:8082` | agent metrics 监听地址 |
| `OPENLB_NGINX_STATUS_URL` | `http://127.0.0.1:8888/nginx_status` | nginx stub_status 采集地址（七层指标） |
| `OPENLB_ENABLE_L4_STATS` | `true` | 启用四层 TCP stream 连接数统计（从 /proc/net/tcp 按 stream 端口过滤） |
| `OPENLB_CONNECTION_STATS_INTERVAL` | `15` | 连接数采集间隔（秒），同时适用于七层和四层 |

**禁用 agent metrics**：
```yaml
env:
  - name: OPENLB_AGENT_METRICS_ADDRESS
    value: "0"
```

**禁用七层连接数采集**（仅采集四层）：
```yaml
env:
  - name: OPENLB_NGINX_STATUS_URL
    value: "0"
```

**禁用四层连接数采集**（仅采集七层）：
```yaml
env:
  - name: OPENLB_ENABLE_L4_STATS
    value: "false"
```

## 暴露的指标

### Controller 业务指标

#### 1. Agent 同步调用

```
# Controller 对 agent 的同步调用次数（按实例、状态、操作分类）
openlb_agent_sync_total{instance="10.10.0.1:8080",status="success|failure",operation="setup|cleanup"}
```

**Labels**：
- `instance`：agent 地址（包含端口）
- `status`：`success`（成功）/ `failure`（失败）
- `operation`：`setup`（同步配置）/ `cleanup`（清理配置）

**用途**：
- 每个 agent 的成功率：`rate(openlb_agent_sync_total{status="success"}[5m]) / rate(openlb_agent_sync_total[5m])`
- 识别问题实例：`rate(openlb_agent_sync_total{status="failure"}[5m]) > 0.1`
- 对比 setup vs cleanup 失败率

#### 2. Service 实例数

```
# 每个 Service 当前管理的实例数
openlb_service_instances{namespace="default",service="my-svc"}
```

**用途**：
- 检测实例配置变更
- 统计总实例数：`sum(openlb_service_instances)`
- 找出高实例数的 Service：`topk(10, openlb_service_instances)`

#### 3. Service 同步耗时

```
# Service 完整同步操作的耗时分布（秒）
openlb_service_sync_duration_seconds_bucket{namespace="default",service="my-svc",le="0.01|0.05|..."}
openlb_service_sync_duration_seconds_sum{namespace="default",service="my-svc"}
openlb_service_sync_duration_seconds_count{namespace="default",service="my-svc"}
```

**用途**：
- Service 同步 P95 延迟：`histogram_quantile(0.95, rate(openlb_service_sync_duration_seconds_bucket[5m]))`
- 识别慢 Service：`topk(5, rate(openlb_service_sync_duration_seconds_sum[5m]) / rate(openlb_service_sync_duration_seconds_count[5m]))`

---

### Agent 业务指标

#### 1. Agent 操作计数

```
# Agent Apply/Delete 操作次数（按操作类型、状态、Service 分类）
openlb_agent_operation_total{operation="apply|delete",status="success|failure",service="namespace/name"}
```

**Labels**：
- `operation`：`apply`（同步配置）/ `delete`（删除配置）
- `status`：`success`（成功）/ `failure`（失败）
- `service`：`namespace/name` 格式的 Service 标识

**用途**：
- Agent 操作成功率：`rate(openlb_agent_operation_total{status="success"}[5m]) / rate(openlb_agent_operation_total[5m])`
- 识别频繁失败的 Service：`rate(openlb_agent_operation_total{status="failure"}[5m]) > 0.1`

#### 2. Agent 操作耗时

```
# Agent 操作耗时分布（秒）
openlb_agent_operation_duration_seconds_bucket{operation="apply|delete",le="0.001|0.005|..."}
openlb_agent_operation_duration_seconds_sum{operation="apply|delete"}
openlb_agent_operation_duration_seconds_count{operation="apply|delete"}
```

**用途**：
- Apply 操作 P95 延迟：`histogram_quantile(0.95, rate(openlb_agent_operation_duration_seconds_bucket{operation="apply"}[5m]))`
- 对比 apply vs delete 性能

#### 3. Agent 管理的 Service 数量

```
# 当前 agent 管理的 Service 数量
openlb_agent_services_managed
```

**用途**：
- 监控 agent 负载
- 检测配置泄漏（数量异常增长）

#### 4. Forwarder Reload 计数

```
# nginx reload 次数（LVS 直接修改内核规则，不执行 reload）
openlb_agent_reload_total{status="success|failure"}
```

**用途**：
- Reload 成功率：`rate(openlb_agent_reload_total{status="success"}[5m]) / rate(openlb_agent_reload_total[5m])`
- 检测 reload 风暴：`rate(openlb_agent_reload_total[1m]) > 10`

#### 5. 连接数指标

```
# 当前连接数（按层级和状态分类）
openlb_agent_connections{layer="l7|l4",state="active|inactive|waiting"}
```

**Labels**：
- `layer`：协议层级
  - `l7`：七层（HTTP/HTTPS），从 nginx stub_status 采集
  - `l4`：四层（TCP stream），从 /proc/net/tcp 按 stream 端口过滤采集
- `state`：连接状态
  - `active`：活跃连接
    - L7: nginx 的 Active connections（Reading + Writing + Waiting）
    - L4: TCP `ESTABLISHED` 状态的连接
  - `inactive`：非活跃连接
    - L7: 固定为 0（HTTP 短连接居多，inactive 概念不适用）
    - L4: TCP 其他状态（`TIME_WAIT`、`CLOSE_WAIT`、`SYN_SENT` 等）
  - `waiting`：等待中的连接
    - L7: nginx 的 Waiting（Keep-Alive 空闲连接）
    - L4: 固定为 0（TCP 层无此概念）

**用途**：

七层监控：
```promql
# HTTP 并发连接数
openlb_agent_connections{layer="l7", state="active"}

# HTTP Keep-Alive 空闲连接
openlb_agent_connections{layer="l7", state="waiting"}

# Keep-Alive 空闲连接过多告警
openlb_agent_connections{layer="l7", state="waiting"} > 1000
```

四层监控：
```promql
# TCP stream 活跃连接数
openlb_agent_connections{layer="l4", state="active"}

# TCP 非活跃连接（TIME_WAIT 等）
openlb_agent_connections{layer="l4", state="inactive"}

# TIME_WAIT 等堆积告警
openlb_agent_connections{layer="l4", state="inactive"} > 5000

# TCP 总连接数
openlb_agent_connections{layer="l4", state="active"} + openlb_agent_connections{layer="l4", state="inactive"}
```

**采集机制**：

- **七层（L7）**：nginx 特有能力（依赖 stub_status 模块），实现在 `pkg/agent/nginx.L7Collector`。agent 通过 HTTP GET 请求 nginx `stub_status` 端点（默认 `http://127.0.0.1:8888/nginx_status`），解析响应文本提取 `Active connections` 和 `Waiting` 字段。采集失败时静默忽略。

- **四层（L4）**：转发器无关的通用能力，实现在 `pkg/agent/stats.L4Collector`，对所有 agent 类型（nginx/lvs/...）生效。agent 读取 `/proc/net/tcp` 和 `/proc/net/tcp6`，解析每行的本地端口（第 2 列 16 进制）和 TCP 状态（第 4 列，`01` = ESTABLISHED）。统计范围由各转发器的 `Forwarder.L4Ports()` 提供：nginx 从 `conf.d/stream/` 配置文件名解析监听端口，lvs 返回托管的 IPVS virtual server 端口，但 IPVS 转发连接不出现在本地 socket 表中，这些计数不代表 LVS 连接数。仅统计这些端口上的连接，避免把七层 HTTP 连接重复计入四层。无监听端口时四层指标清零。

- **采集间隔**：默认 15 秒，通过 `OPENLB_CONNECTION_STATS_INTERVAL` 统一配置。

**权限要求**：
- 七层：需访问 nginx stub_status 端点（LB 主机本地默认可达）
- 四层：需读取 `/proc/net/tcp*` 的权限（默认主机 systemd 服务可读）

#### 6. UDP 套接字数指标

```
# 当前 UDP 套接字数量
openlb_agent_udp_sockets
```

**说明**：

UDP 是无连接协议，不像 TCP 有 ESTABLISHED 等连接状态。`/proc/net/udp` 记录的是本地绑定的 UDP 套接字（监听端口），该指标统计当前 nginx stream UDP 监听端口对应的套接字数量。

**用途**：

```promql
# UDP 套接字数量
openlb_agent_udp_sockets

# UDP 端口泄漏告警（套接字数异常增长）
openlb_agent_udp_sockets > 100
```

**采集机制**：

- 转发器无关的通用能力，实现在 `pkg/agent/stats.UDPCollector`
- 读取 `/proc/net/udp` 和 `/proc/net/udp6`，解析每行的本地端口（第 2 列 16 进制）
- 统计范围由各转发器的 `Forwarder.UDPPorts()` 提供：nginx 从 `conf.d/stream/` 配置文件名解析 proto="udp" 的监听端口
- 仅统计这些端口上的套接字，过滤掉其他系统 UDP 端口（如 systemd-resolved 的 53 端口）
- 无 UDP 监听端口时指标清零
- 采集间隔：默认 15 秒，通过 `OPENLB_CONNECTION_STATS_INTERVAL` 统一配置

**权限要求**：
- 需读取 `/proc/net/udp*` 的权限（默认主机 systemd 服务可读）

---

### controller-runtime 内置指标

#### 1. Reconcile 性能

```
# Reconcile 调用总数（按结果分类）
controller_runtime_reconcile_total{controller="service",result="success|error|requeue"}

# Reconcile 耗时分布（秒）
controller_runtime_reconcile_time_seconds_bucket{controller="service",le="0.005|0.01|..."}
controller_runtime_reconcile_time_seconds_sum{controller="service"}
controller_runtime_reconcile_time_seconds_count{controller="service"}
```

**用途**：
- 监控 reconcile 成功率：`rate(controller_runtime_reconcile_total{result="success"}[5m])`
- 监控错误率：`rate(controller_runtime_reconcile_total{result="error"}[5m])`
- P99 延迟：`histogram_quantile(0.99, rate(controller_runtime_reconcile_time_seconds_bucket[5m]))`

#### 2. 工作队列

```
# 队列深度（当前待处理的 reconcile 请求数）
workqueue_depth{name="service"}

# 队列中最老请求的等待时间（秒）
workqueue_longest_running_processor_seconds{name="service"}

# 入队速率
workqueue_adds_total{name="service"}

# 重试次数
workqueue_retries_total{name="service"}
```

**用途**：
- 检测队列积压：`workqueue_depth{name="service"} > 100`
- 监控重试率：`rate(workqueue_retries_total[5m])`

#### 3. Kubernetes API 客户端

```
# API 调用次数（按动词和资源分类）
rest_client_requests_total{code="200|404|500",method="GET|PATCH|PUT",host="..."}

# API 调用延迟
rest_client_request_duration_seconds_bucket{verb="GET|PATCH|PUT",le="0.001|0.01|..."}
```

**用途**：
- 监控 API 错误率：`rate(rest_client_requests_total{code=~"5.."}[5m])`
- API 性能：`histogram_quantile(0.95, rate(rest_client_request_duration_seconds_bucket[5m]))`

#### 4. Leader Election

```
# Leader 状态（1=当前是 leader，0=follower）
leader_election_master_status{name="openlb-controller-leader"}
```

**用途**：
- 检测频繁切换：`changes(leader_election_master_status[10m]) > 2`

#### 5. Go Runtime

```
# Goroutine 数量
go_goroutines

# 堆内存使用（字节）
go_memstats_heap_alloc_bytes

# GC 暂停时间
go_gc_duration_seconds
```

## 集成 Prometheus

### 1. ServiceMonitor（Prometheus Operator）

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: openlb-controller
  namespace: openlb-system
spec:
  selector:
    matchLabels:
      app: openlb-controller
  endpoints:
  - port: metrics
    interval: 30s
    path: /metrics
```

需要先为 Deployment 添加 metrics Service：

```yaml
apiVersion: v1
kind: Service
metadata:
  name: openlb-controller-metrics
  namespace: openlb-system
  labels:
    app: openlb-controller
spec:
  ports:
  - name: metrics
    port: 8081
    targetPort: 8081
  selector:
    app: openlb-controller
```

### 2. Prometheus 配置（原生）

```yaml
scrape_configs:
  - job_name: 'openlb-controller'
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - openlb-system
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app]
        action: keep
        regex: openlb-controller
      - source_labels: [__meta_kubernetes_pod_ip]
        action: replace
        target_label: __address__
        replacement: $1:8081
```

## 示例告警规则

```yaml
groups:
  - name: openlb-controller
    interval: 30s
    rules:
      # Agent 同步失败率过高
      - alert: OpenLBHighAgentSyncFailureRate
        expr: |
          rate(openlb_agent_sync_total{status="failure"}[5m])
          / rate(openlb_agent_sync_total[5m])
          > 0.05
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "OpenLB Agent 同步失败率超过 5%"
          description: "实例 {{ $labels.instance }} 操作 {{ $labels.operation }} 失败率: {{ $value | humanizePercentage }}"

      # 特定 agent 完全不可用
      - alert: OpenLBAgentDown
        expr: |
          rate(openlb_agent_sync_total{status="success"}[5m]) == 0
          and rate(openlb_agent_sync_total{status="failure"}[5m]) > 0
        for: 3m
        labels:
          severity: critical
        annotations:
          summary: "OpenLB Agent 完全不可用"
          description: "实例 {{ $labels.instance }} 同步持续失败，无成功调用"

      # Service 同步耗时过长
      - alert: OpenLBSlowServiceSync
        expr: |
          histogram_quantile(0.95, 
            rate(openlb_service_sync_duration_seconds_bucket[5m])
          ) > 5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "OpenLB Service 同步耗时过长"
          description: "Service {{ $labels.namespace }}/{{ $labels.service }} P95 同步耗时: {{ $value }}s"

      # Reconcile 错误率过高
      - alert: OpenLBHighReconcileErrorRate
        expr: |
          rate(controller_runtime_reconcile_total{controller="service",result="error"}[5m])
          / rate(controller_runtime_reconcile_total{controller="service"}[5m])
          > 0.05
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "OpenLB Controller reconcile 错误率超过 5%"
          description: "当前错误率: {{ $value | humanizePercentage }}"

      # 队列积压
      - alert: OpenLBQueueBacklog
        expr: workqueue_depth{name="service"} > 50
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "OpenLB Controller 工作队列积压"
          description: "当前队列深度: {{ $value }}"

      # Leader 频繁切换
      - alert: OpenLBLeaderFlapping
        expr: changes(leader_election_master_status{name="openlb-controller-leader"}[10m]) > 2
        labels:
          severity: warning
        annotations:
          summary: "OpenLB Controller leader 频繁切换"
          description: "过去 10 分钟切换了 {{ $value }} 次"

      # API 错误率过高
      - alert: OpenLBHighAPIErrorRate
        expr: |
          rate(rest_client_requests_total{code=~"5.."}[5m])
          / rate(rest_client_requests_total[5m])
          > 0.01
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "OpenLB Controller 访问 K8s API 错误率过高"
          description: "当前错误率: {{ $value | humanizePercentage }}"
```

## Grafana Dashboard

### 关键面板

1. **Overview**
   - Agent 同步成功率（`openlb_agent_sync_total`）
   - Service 实例数总览（`openlb_service_instances`）
   - Reconcile 成功率（`controller_runtime_reconcile_total`）
   - Reconcile P95/P99 延迟（`controller_runtime_reconcile_time_seconds`）
   - 队列深度趋势（`workqueue_depth`）
   - Leader 状态（`leader_election_master_status`）

2. **Agent Health**（业务指标面板）
   - 每个 agent 的成功率（按 instance 分组）
   - Setup vs Cleanup 失败率对比
   - Agent 同步调用 QPS
   - 问题实例 Top 5

3. **Service Performance**（业务指标面板）
   - Service 同步耗时 P95/P99（按 namespace/service 分组）
   - Service 实例数分布直方图
   - 慢 Service Top 10

4. **Controller Performance**
   - Reconcile 耗时分布热力图
   - API 调用延迟 P95/P99
   - 工作队列重试率

5. **Resources**
   - Goroutine 数量
   - 内存使用
   - GC 频率和暂停时间

### 示例 PromQL 查询

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

# Setup vs Cleanup 失败率对比
rate(openlb_agent_sync_total{status="failure"}[5m]) by (operation)

# 总的 agent 同步 QPS
sum(rate(openlb_agent_sync_total[5m]))

# 当前管理的总实例数
sum(openlb_service_instances)

# 实例数最多的 Service Top 5
topk(5, openlb_service_instances)

# Service 同步 P95 延迟
histogram_quantile(0.95, 
  rate(openlb_service_sync_duration_seconds_bucket[5m])
) by (namespace, service)

# 最慢的 Service Top 10
topk(10, 
  rate(openlb_service_sync_duration_seconds_sum[5m])
  / rate(openlb_service_sync_duration_seconds_count[5m])
)

# === controller-runtime 指标查询 ===

# Reconcile 成功率（5 分钟）
sum(rate(controller_runtime_reconcile_total{controller="service",result="success"}[5m]))
/ sum(rate(controller_runtime_reconcile_total{controller="service"}[5m]))

# Reconcile P99 延迟
histogram_quantile(0.99, 
  rate(controller_runtime_reconcile_time_seconds_bucket{controller="service"}[5m])
)

# 平均队列深度
avg_over_time(workqueue_depth{name="service"}[5m])

# API 调用 QPS（按资源分类）
sum by (resource) (rate(rest_client_requests_total[5m]))
```

## 验证

### 1. 本地验证

启动 controller 后，curl metrics 端点：

```bash
# Port-forward（如果在 K8s 内）
kubectl port-forward -n openlb-system deployment/openlb-controller 8081:8081

# 拉取指标
curl http://localhost:8081/metrics

# 过滤 controller 相关指标
curl -s http://localhost:8081/metrics | grep controller_runtime
```

### 2. 检查特定指标

```bash
# Reconcile 总数
curl -s http://localhost:8081/metrics | grep 'controller_runtime_reconcile_total{controller="service"'

# 队列深度
curl -s http://localhost:8081/metrics | grep 'workqueue_depth{name="service"'

# Leader 状态
curl -s http://localhost:8081/metrics | grep 'leader_election_master_status'
```

### 3. Prometheus 中查询

在 Prometheus UI (http://prometheus:9090/graph) 中执行：

```promql
controller_runtime_reconcile_total{controller="service"}
```

应该能看到 `result="success"` 和 `result="error"` 两个时间序列。

## 故障排查

### 问题：无法访问 metrics 端点

**原因**：
- `OPENLB_METRICS_BIND_ADDRESS=0`（metrics 被禁用）
- NetworkPolicy 阻止访问
- Pod 未监听 8081 端口

**解决**：
```bash
# 检查环境变量
kubectl get deploy -n openlb-system openlb-controller -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OPENLB_METRICS_BIND_ADDRESS")].value}'

# 检查端口监听
kubectl exec -n openlb-system deployment/openlb-controller -- netstat -tlnp | grep 8081
```

### 问题：Prometheus 无法抓取指标

**原因**：
- Service selector 不匹配
- Endpoint 未创建
- Prometheus RBAC 权限不足

**解决**：
```bash
# 检查 Service endpoints
kubectl get endpoints -n openlb-system openlb-controller-metrics

# 检查 Prometheus ServiceMonitor
kubectl get servicemonitor -n openlb-system openlb-controller -o yaml

# 在 Prometheus UI 检查 targets
# http://prometheus:9090/targets
```

### 问题：看不到 leader_election_master_status 指标

**原因**：
- Leader election 被禁用（`OPENLB_LEADER_ELECTION=false`）
- 当前 pod 还未参与选举（刚启动）

**预期行为**：
- Leader: `leader_election_master_status{...} 1`
- Follower: `leader_election_master_status{...} 0`

## 后续扩展

当前暴露的是 **controller-runtime 内置指标**（通用框架指标）。未来可以添加 **业务指标**：

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
    // Agent 同步成功次数（按实例分类）
    agentSyncTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "openlb_agent_sync_total",
            Help: "Total number of agent sync operations",
        },
        []string{"instance", "status"}, // status: success/failure
    )

    // 每个 Service 管理的实例数
    serviceInstancesGauge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "openlb_service_instances",
            Help: "Number of instances per service",
        },
        []string{"namespace", "service"},
    )
)

func init() {
    metrics.Registry.MustRegister(agentSyncTotal, serviceInstancesGauge)
}
```

在 `ServiceReconciler.setup()` 中记录：

```go
agentSyncTotal.WithLabelValues(instance, "success").Inc()
serviceInstancesGauge.WithLabelValues(svc.Namespace, svc.Name).Set(float64(len(instances)))
```

这些业务指标提供更细粒度的可观测性。

## 参考

- [controller-runtime metrics](https://book.kubebuilder.io/reference/metrics.html)
- [Prometheus best practices](https://prometheus.io/docs/practices/naming/)
- [Grafana dashboard for controller-runtime](https://github.com/kubernetes-sigs/controller-runtime/tree/main/examples/grafana)
