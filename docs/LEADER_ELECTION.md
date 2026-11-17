# Leader Election 使用说明

## 概述

从本版本开始，`openlb-controller` 支持 **Leader Election（领导者选举）**，允许在多副本部署时确保：

- **高可用**：一个副本故障时，另一个副本自动接管
- **一致性**：同一时刻只有 leader 执行 reconcile，避免多副本同时推进同一 Service 的 agent 操作

## 工作原理

使用 controller-runtime 内置的 leader election 机制：

1. 启动时，所有副本竞争 `openlb-system` 命名空间下的 `openlb-controller-leader` Lease
2. 获得 Lease 的副本成为 **leader**，开始执行 reconcile
3. 其他副本成为 **follower**，保持待命状态
4. leader 定期续约 Lease；如果 leader 故障，Lease 过期后其他副本自动接管

## 配置

### 1. 环境变量（可选）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENLB_LEADER_ELECTION` | `true` | 是否启用 leader election |

**何时禁用：**
- 单副本部署（replicas: 1）时可设为 `false` 以减少 Lease 开销
- 开发/测试环境

**示例（禁用）：**
```yaml
env:
  - name: OPENLB_LEADER_ELECTION
    value: "false"
```

### 2. RBAC 权限

leader election 需要操作 `coordination.k8s.io/leases` 资源，已包含在 `deploy/install/rbac.yaml`：

```yaml
# deploy/install/rbac.yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
```

### 3. 副本数

`deploy/install/deployment.yaml` 已默认设置为 2 副本：

```yaml
spec:
  replicas: 2  # 高可用，推荐至少 2 副本
```

**副本数建议：**
- **生产环境**：2-3 副本（平衡可用性与资源成本）
- **开发/测试**：1 副本 + 禁用 leader election

## 验证

### 1. 查看 leader 日志

```bash
kubectl logs -n openlb-system -l app=openlb-controller --tail=50 | grep leader
```

成功获得 leader 时会看到：
```
I1231 12:34:56.789012       1 leaderelection.go:248] attempting to acquire leader lease openlb-system/openlb-controller-leader...
I1231 12:34:56.890123       1 leaderelection.go:258] successfully acquired lease openlb-system/openlb-controller-leader
```

### 2. 查看 Lease 对象

```bash
kubectl get lease -n openlb-system openlb-controller-leader -o yaml
```

输出：
```yaml
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: openlb-controller-leader
  namespace: openlb-system
spec:
  holderIdentity: openlb-controller-7d8f9b5c4d-abcde_1a2b3c4d  # 当前 leader pod
  leaseDurationSeconds: 15
  acquireTime: "2026-07-22T12:34:56.123456Z"
  renewTime: "2026-07-22T12:35:10.234567Z"
```

`holderIdentity` 字段显示当前 leader 的 pod 名和 UID。

### 3. 测试 failover

杀死当前 leader，观察另一个副本是否接管：

```bash
# 找到当前 leader pod
LEADER_POD=$(kubectl get lease -n openlb-system openlb-controller-leader -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1)

# 删除 leader
kubectl delete pod -n openlb-system $LEADER_POD

# 观察新 leader 选举（10-15 秒内完成）
kubectl logs -n openlb-system -l app=openlb-controller --tail=20 -f | grep "acquired lease"
```

## 与状态存储的关系

默认 CRDStore 每个 Service 保存一个 Namespaced CR，通过 APIReader 读取最新状态，
写入采用资源版本冲突重试。CR 的 List/Watch 触发新 leader 的恢复处理，状态中的
PendingApply/PendingDelete 支持继续未完成操作。

Leader election 仍然必要：资源版本检查保护 CR 写入，但不会替 HTTP 请求提供跨进程排他。
只有 leader 执行 agent 的 Apply/Cleanup。显式选择 ConfigMap 后端仍有启动缓存限制，
leader election 本身不会刷新该缓存。详见 [状态存储](STORAGE.md)。

## 迁移指南

从旧版本（无 leader election）迁移：

1. **应用新的 RBAC**（包含 leases 权限）：
   ```bash
   kubectl apply -f deploy/install/rbac.yaml
   ```

2. **更新 Deployment**（新镜像 + replicas: 2）：
   ```bash
   kubectl apply -f deploy/install/deployment.yaml
   ```

3. **验证**：
   ```bash
   kubectl get pods -n openlb-system
   kubectl get lease -n openlb-system openlb-controller-leader
   ```

**零停机迁移**：

- Deployment 滚动更新时，新副本会竞争 Lease
- 旧副本退出前会释放 Lease（如果它是 leader）
- 新副本接管后继续处理 Service，无丢失

## 故障排查

### 问题：副本日志显示 "failed to get lease"

**原因**：RBAC 权限不足

**解决**：确认 `deploy/install/rbac.yaml` 中的 leases 权限已应用：
```bash
kubectl auth can-i get leases --as=system:serviceaccount:openlb-system:openlb-controller -n openlb-system
# 应输出 "yes"
```

### 问题：两个副本都在执行 reconcile

**原因**：leader election 被禁用

**解决**：检查环境变量：
```bash
kubectl get deploy -n openlb-system openlb-controller -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OPENLB_LEADER_ELECTION")].value}'
```
应输出 `true` 或为空（默认启用）。

### 问题：leader 切换频繁（几分钟一次）

**原因**：
- 网络抖动导致 Lease 续约失败
- leader pod 资源不足，续约超时

**解决**：
1. 检查 leader pod 资源使用：
   ```bash
   kubectl top pod -n openlb-system -l app=openlb-controller
   ```
2. 提高资源 limits（如果接近上限）
3. 检查集群网络稳定性

## 环境变量完整列表

| 变量 | 默认值 | 用途 |
|---|---|---|
| `OPENLB_LEADER_ELECTION` | `true` | 启用/禁用 leader election |
| `OPENLB_STORE_TYPE` | `crd` | 状态存储（crd/configmap/file） |
| `OPENLB_SYNC_TYPE` | `http` | Syncer 类型（http/grpc） |
| `OPENLB_SYNC_TIMEOUT` | `10s` | 单次 controller-agent HTTP 请求超时 |
| `OPENLB_SYNC_MAX_RETRIES` | `2` | 首次失败后的最大重试次数 |
| `OPENLB_SYNC_RETRY_BACKOFF` | `200ms` | 第一次重试前的退避时间，后续指数增长 |
| `OPENLB_SYNC_RETRY_MAX_BACKOFF` | `2s` | 指数退避的最大时间 |
| `OPENLB_AGENT_PORT` | `8080` | agent 默认端口 |
| `POD_NAMESPACE` | — | 当前 pod 命名空间（downward API） |

## 参考

- [controller-runtime leader election 文档](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/manager#Options)
- [Kubernetes Lease 对象规范](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/lease-v1/)
