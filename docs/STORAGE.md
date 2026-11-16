# Controller 状态存储

默认 `OPENLB_STORE_TYPE=crd`。每个 Service 对应一个同 namespace、同名的
`openlb.io/v1alpha1 OpenLBServiceState`。CRD 定义是集群级资源，CR 实例是 namespace 级资源。
Service 注解仍是期望配置的来源，CR 由 controller 创建和维护。

## 实现边界

| 位置 | 职责 |
|------|------|
| `pkg/apis/v1alpha1/` | API 类型、Scheme 注册、DeepCopy |
| `pkg/store/repository.go` | 有类型的 Repository 接口 |
| `pkg/store/crd.go` | 读取 CR、创建记录、更新 status、版本冲突重试和删除 |
| `pkg/controllers/state.go` | 实例同步/清理生命周期、Service finalizer、恢复处理 |
| `pkg/controllers/service.go` | 收集转发资源、下发配置、发布 Service ingress |
| `pkg/controllers/certificates.go` | Secret 引用登记、finalizer 保护与回收 |
| `deploy/crd.yaml` | Namespaced CRD、schema 校验、status 子资源 |

Repository 的写操作成功返回后才允许推进下一步；状态更新回调可能因版本冲突重试，
回调中不能调用 agent。CRDStore 使用 manager 的 APIReader 读取最新状态，避免写后读和
UID 判断依赖 informer 的延迟；manager 的 List/Watch 负责启动恢复和删除事件触发。

## 记录示例

```yaml
apiVersion: openlb.io/v1alpha1
kind: OpenLBServiceState
metadata:
  namespace: team-a
  name: web
  finalizers:
    - openlb.io/agent-cleanup
spec:
  serviceRef:
    name: web
    uid: <Service UID>
status:
  certificateRefs:
    - name: tls-cert
      uid: <Secret UID>
  instances:
    - address: 10.0.0.1:8080
      phase: Applied
      configHash: <转发输入的 SHA-256>
      ingress:
        - ip: 10.0.0.1
    - address: 10.0.0.2:8080
      phase: PendingApply
      lastError: <最近一次错误>
```

`spec.serviceRef` 不可修改，`serviceRef.name` 必须与 CR 名称一致。
同名 Service 重建但 UID 不同时，先清理旧记录，再为新 Service 建立记录。
每个地址的阶段为 `PendingApply`、`Applied`、`PendingDelete` 或 `Cleaned`；
`status.conditions` 中的 `Ready` 记录本轮处理结果。

## 同步与恢复

1. 创建带 finalizer 和 Service ownerReference 的 CR，并写入 Service finalizer。
2. 在发送请求前，持久化所有目标 agent。新目标或配置变化标记为 `PendingApply`。
3. 下发后批量保存各实例结果；全部成功且状态保存成功后才发布 Service ingress。
4. 减少目标实例时先标记 `PendingDelete`，成功清理的实例记录为 `Cleaned`。
   重试跳过这些已成功的实例，其余实例继续清理。
5. controller 重启后，CR 的初始 List 会触发恢复。`PendingApply` 或失败的 Ready
   条件使下一次同步带上 `ForceReload`，不依赖进程内标记。

实例列表会补全端口、去重并排序。稳定的转发摘要排除资源版本、心跳、Service status、
finalizer 等元数据。无变化时继续向 agent 做幂等同步，但不会反复写 CR 的阶段、摘要和时间。
状态子资源更新不触发自身 reconcile 循环；CR 创建、删除和身份/finalizer 变化仍会触发处理。

## 删除与 namespace 隔离

Service 删除、退出管理、CR 被直接删除或 Service 已不存在时，都会依据 CR 清理 agent。
清理记录全部为 `Cleaned` 且证书引用已释放后才删除 CR 并释放清理 finalizer。CR 删除事件与 Service 事件
进入同一 namespace/name 队列，防止旧记录清理和同名新 Service 下发交错。

namespace 删除也会触发 CR 删除；CR finalizer 会保留未完成的外部清理记录。
agent 永久不可达时，Service/CR/namespace 可能持续 Terminating，目前没有自动放弃清理策略。
不要删除 CRD 或强制移除 finalizer 来替代正常清理。

RBAC 按 namespace 控制 CR 访问；默认 controller 管理所有 namespace，因此获得该资源的
ClusterRole 权限。namespace 隔离的是 Kubernetes 状态，多个 namespace 使用同一台 agent 时，
agent 的监听端口锁仍然跨 namespace 检查冲突。

## 部署与查看

```bash
kubectl apply -f deploy/crd.yaml
kubectl wait --for=condition=Established crd/openlbservicestates.openlb.io --timeout=60s
kubectl apply -k deploy/
kubectl get olbstate -A
kubectl get olbstate web -n team-a -o yaml
```

controller 不自动安装 CRD，部署时先安装定义。只有 controller 应写入 CR 的身份、实例进度
和 finalizer；用户修改 Service 注解即可。

`OPENLB_STORE_TYPE=file` 和 `configmap` 仍可显式选择，通过 `KVRepository` 保存相同的类型化状态。
这些后端不具备 CR 的 Watch、namespace 删除保护和 schema 校验；CRD 后端是默认部署方案。
file 后端使用原子替换，写入失败不推进内存状态；ConfigMap 后端仍有共享对象和启动缓存的限制。

`status.certificateRefs` 保存当前和可能已下发的 Secret 名称/UID，不存储私钥。
部分切换失败时保留新旧引用，全部目标成功且结果落盘后才收缩引用。
Service reconcile 忽略 status 变化，Secret reconcile 单独监听引用变化以完成释放。
完整生命周期及部署要求见 [TLS Secret 引用保护](CERTIFICATES.md)。

## 测试

```bash
go test -count=1 -timeout=90s ./...
go test -race -count=1 -timeout=120s ./pkg/controllers ./pkg/store ./pkg/apis/v1alpha1

# 准备 envtest 二进制后验证真实 Kubernetes API 行为
# KUBEBUILDER_ASSETS 指向包含 kube-apiserver、etcd、kubectl 的目录
go test -tags=integration -run TestCRDIntegration -count=1 -timeout=120s ./pkg/store
```

常规测试覆盖 namespace 隔离、UID 重建、版本冲突、无变化不写、先记录后下发、失败后恢复、
部分清理进度、孤儿记录和 CR 删除。integration 测试覆盖真实 API 的 schema、status 子资源及
finalizer；envtest 不运行垃圾回收与 namespace controller，不等同于完整集群的 namespace 删除测试。
