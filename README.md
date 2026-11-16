# open-loadbalancer-controller

## 概述

controller 监听 Service, 如果 Service 的类型为 LoadBalancer, 并且写上了相应的 annotations, 则将 Service 同步到 instances 的 agent  
agent 仅做配置解析, 将配置渲染为 class 的 nginx 或者 lvs 的配置, 并对转发器做重载  
转发器使用 nginx 或者 lvs 等, 只需要 agent 实现了 `pkg/agent/agent.go` 定义的接口, 就认为这是一个新的转发器 agent（接入方法见 [转发器接入指南](docs/FORWARDER_INTEGRATION.md)）  

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

## Service 示例

HTTPS 配置（需要 TLS Secret）

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  namespace: default
  annotations:
    kubernetes.io/openlb.instances: "192.168.1.10"
    kubernetes.io/openlb.protocol-port: "https:443"     # 必需：明确协议
    kubernetes.io/openlb.cert-secrets: "my-tls-secret"  # 必需：TLS 证书                                                                                               spec:
  type: LoadBalancer                                                                                                                                                     selector:
    app: my-app
  ports:
    - port: 443
      targetPort: 8080
```

## 文档

- [docs/RESOURCES.md](docs/RESOURCES.md) — 监听的资源（Service 注解、Node、EndpointSlice、Secret 格式参考）
- [docs/FORWARDER_INTEGRATION.md](docs/FORWARDER_INTEGRATION.md) — 转发器接入指南（如何接入 HAProxy、Envoy 等新转发器）
- [docs/LEADER_ELECTION.md](docs/LEADER_ELECTION.md) — 高可用与 Leader Election
- [docs/METRICS.md](docs/METRICS.md) — Prometheus Metrics 监控
- [docs/TESTING.md](docs/TESTING.md) — 测试指南
- [docs/ROADMAP.md](docs/ROADMAP.md) — 路线图与设计笔记
