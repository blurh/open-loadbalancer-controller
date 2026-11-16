# open-loadbalancer-controller

## 概述

controller 监听 Service, 如果 Service 的类型为 LoadBalancer, 并且写上了相应的 annotations, 则将 Service 同步到 instances 的 agent  
agent 仅做配置解析, 将配置渲染为 class 的 nginx 或者 lvs 的配置, 并对转发器做重载  
转发器使用 nginx 或者 lvs 等, 只需要 agent 实现了 `pkg/agent/agent.go` 定义的接口, 就认为这是一个新的转发器 agent（接入方法见 [转发器接入指南](docs/FORWARDER_INTEGRATION.md)）  

### 按 Service 选择转发器

agent 根据每次 `/sync` 请求中 Service 的 `kubernetes.io/openlb.class` 注解选择转发器，
不再读取 `OPENLB_AGENT_TYPE`。例如：

```yaml
metadata:
  annotations:
    kubernetes.io/openlb.class: "nginx"
```

- 未提供 class 注解时默认 `nginx`，兼容已有 Service；显式空值或未知 class 返回 HTTP 400。
- 每种本机可用的转发器在启动时初始化一次，同一进程可以处理不同 class 的 Service；没有可用转发器时启动失败。
- class 切换先清理旧转发器配置，再应用新配置；清理失败则返回错误供重试。目标不可用时保留旧配置。
- `/cleanup` 仍只需要 namespace/name；agent 启动时从持久化锁和各转发器配置恢复归属。无法定位归属时对所有可用实现执行幂等清理，以重试文件已删除但尚未 reload 的情况。
- Nginx 与 LVS 均已实现。LVS 使用 IPVS 转发 TCP/UDP，忽略七层配置并在 Service 上记录 `L7ConfigurationIgnored` 事件；默认自动使用本机内网 IPv4，`OPENLB_LVS_VIPS` 可选覆盖。详见 [LVS 部署与行为](docs/LVS.md)。

### Agent Service 端口锁

每个 agent 按「传输协议 + 监听端口」记录持有的 `namespace/name`，所有 class 共用锁。
HTTP、HTTPS、TCP 共用 TCP 端口空间，UDP 独立；例如两个 Service 不能同时占用 TCP/80，
但 TCP/53 和 UDP/53 可以分别由不同 Service 持有。没有后端时也保留请求中声明的端口。

冲突请求在修改配置、清理旧 class 之前返回 HTTP 409，响应包含端口、持有者和 class。
同一 Service 可幂等更新；更新成功后释放已移除的端口，cleanup/reload 成功后释放全部端口。
Apply 或 Delete 失败时保留旧、新端口预留；未完成操作会持久化，重试时强制 reload，避免
磁盘文件已变化但运行中的转发器仍使用旧配置时提前释放端口。

| 环境变量 | 默认值 | 用途 |
|----------|--------|------|
| `OPENLB_AGENT_STATE_DIR` | `/var/lib/openlb-agent` | 保存 `service-locks.json`，目录需可写 |

容器部署时，将此目录和 Nginx 生成配置目录一起持久化，例如为状态目录添加
`-v /srv/openlb/state:/var/lib/openlb-agent`。每个 agent 使用独立目录，单个目录只由一个进程写入。
首次升级会从已有 Nginx 配置文件恢复端口归属；锁文件损坏或已有归属冲突时拒绝启动。
锁只管理本 agent 的 Service，不检测宿主机上其他进程的监听端口。

## 部署

### Controller 状态存储

默认使用 `OPENLB_STORE_TYPE=crd`，每个 Service 对应一个同 namespace、同名的
`OpenLBServiceState`，由 `pkg/store/CRDStore` 保存实例同步和清理进度。
先持久化目标实例再调用 agent；失败后可从 CR 恢复，清理成功后才释放 finalizer。
无变化不写 CR，多个 Service 不再争用一个仓库对象。详见 [状态存储](docs/STORAGE.md)。

使用中的 TLS Secret 通过 finalizer 和 CR 中的名称/UID 引用保护；切换或清理全部成功后
才释放。缺失/无效证书拒绝同步，nginx 配置与证书在失败时一起回滚，支持中断后的恢复。
部署前需更新 CRD/RBAC，并持久化整个 nginx 配置目录。详见 [证书保护](docs/CERTIFICATES.md)。

### 快速开始

```bash
# 1. 安装 CRD 并等待 API 就绪
kubectl apply -f deploy/crd.yaml
kubectl wait --for=condition=Established crd/openlbservicestates.openlb.io --timeout=60s

# 2. 部署 Namespace、RBAC 和 controller（默认 2 副本 + leader election）
kubectl apply -k deploy/

# 3. 验证部署
kubectl get pods -n openlb-system
kubectl get lease -n openlb-system openlb-controller-leader
kubectl get olbstate -A
```

### 高可用与 Leader Election

controller 默认启用 **leader election**，支持多副本部署：

- **默认配置**：2 副本，同一时刻只有 leader 执行 reconcile
- **自动 failover**：leader 故障时，其他副本在 10-15 秒内接管
- **单写入者**：同一时刻由 leader 推进 agent 操作，CR 更新另外使用资源版本检查

**配置选项：**

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `OPENLB_LEADER_ELECTION` | `true` | 启用/禁用 leader election |
| `OPENLB_METRICS_BIND_ADDRESS` | `:8081` | Metrics 监听地址（设置为 "0" 可禁用） |
| `OPENLB_STORE_TYPE` | `crd` | 状态存储（crd/configmap/file），默认每 Service 一个 Namespaced CR |
| `OPENLB_SYNC_TYPE` | `http` | Syncer 类型（http/grpc） |
| `OPENLB_SYNC_TIMEOUT` | `10s` | 单次 controller-agent HTTP 请求超时 |
| `OPENLB_SYNC_MAX_RETRIES` | `2` | 首次失败后的最大重试次数 |
| `OPENLB_SYNC_RETRY_BACKOFF` | `200ms` | 第一次重试前的退避时间，后续指数增长 |
| `OPENLB_SYNC_RETRY_MAX_BACKOFF` | `2s` | 指数退避的最大时间 |

**单副本场景**（开发/测试）：

```yaml
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: controller
        env:
        - name: OPENLB_LEADER_ELECTION
          value: "false"  # 禁用可减少 Lease 开销
```

详细说明见 [docs/LEADER_ELECTION.md](docs/LEADER_ELECTION.md)

### Metrics 监控

controller 默认在 `:8081/metrics` 暴露 **Prometheus metrics**，包括：

- Reconcile 性能指标（成功率、延迟、错误率）
- 工作队列指标（队列深度、重试次数）
- Kubernetes API 调用指标
- Leader election 状态
- Go runtime 指标

**部署 metrics Service：**

```bash
kubectl apply -f deploy/metrics-service.yaml
```

**访问 metrics：**

```bash
kubectl port-forward -n openlb-system svc/openlb-controller-metrics 8081:8081
curl http://localhost:8081/metrics
```

详细说明见 [docs/METRICS.md](docs/METRICS.md)

## 文档

- [docs/RESOURCES.md](docs/RESOURCES.md) — 监听的资源（Service 注解、Node、EndpointSlice、Secret 格式参考）
- [docs/FORWARDER_INTEGRATION.md](docs/FORWARDER_INTEGRATION.md) — 转发器接入指南（如何接入 HAProxy、Envoy 等新转发器）
- [docs/LEADER_ELECTION.md](docs/LEADER_ELECTION.md) — 高可用与 Leader Election
- [docs/METRICS.md](docs/METRICS.md) — Prometheus Metrics 监控
- [docs/TESTING.md](docs/TESTING.md) — 测试指南
- [docs/ROADMAP.md](docs/ROADMAP.md) — 路线图与设计笔记
