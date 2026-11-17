# open-loadbalancer-controller

## 概述

controller 监听 Service, 如果 Service 的类型为 LoadBalancer, 并且写上了相应的 annotations, 则将 Service 同步到 instances 的 agent  
agent 仅做配置解析, 将配置渲染为 class 的 nginx 或者 lvs 的配置, 并对转发器做重载  
转发器使用 nginx 或者 lvs 等, 只需要 agent 实现了 `pkg/agent/agent.go` 定义的接口, 就认为这是一个新的转发器 agent（接入方法见 [转发器接入指南](docs/FORWARDER_INTEGRATION.md)）  

## 构建

测试

```bash
go test ./...
```

编译二进制

```bash
go build -o /tmp/openlb-controller ./cmd/controller/
go build -o /tmp/openlb-agent ./cmd/agent/
go build ./...     # 全部编译
```

### Controller 镜像

构建上下文为仓库根目录；agent 使用主机二进制，不再构建镜像：

```bash
docker build -f deploy/docker/Dockerfile -t ghcr.io/blurh/openlb-controller:latest .
```

### Agent 主机安装包

```bash
make agent-package GOARCH=amd64  # 也支持 arm64
# dist/openlb-agent-linux-amd64.tar.gz
```

安装包包含二进制、systemd 服务、环境文件、nginx 基础配置和安装脚本。将其解压到 LB
主机后执行：

```bash
sudo sh ./install.sh
sudoedit /etc/openlb/agent.env
sudo systemctl enable --now openlb-agent.service
```

安装脚本只检查 nginx 是否已安装，未安装时提示并继续。已有 `nginx.conf` 先备份为
`nginx.conf.bak`，再覆盖；保留 `agent.env` 和业务状态。nginx 由主机自行管理，不安装
专用 nginx 服务。完整示例见 [Agent 主机部署](docs/AGENT_DEPLOYMENT.md)。

## 部署

### 快速开始

```bash
# 1. 安装 CRD 并等待 API 就绪
kubectl apply -f deploy/install/crd.yaml
kubectl wait --for=condition=Established crd/openlbservicestates.openlb.io --timeout=60s

# 2. 部署 Namespace、RBAC 和 controller（默认 2 副本 + leader election）
kubectl apply -k deploy/install/

# 3. 验证部署
kubectl get pods -n openlb-system
kubectl get lease -n openlb-system openlb-controller-leader
kubectl get olbstate -A
```

### Service 示例

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
    kubernetes.io/openlb.cert-secrets: "my-tls-secret"  # 必需：明确协议为 HTTPS 时需要 TLS 证书
spec:
  type: LoadBalancer
  loadBalancerClass: kubernetes.io/openlb
  selector:
    app: my-app
  ports:
    - port: 443
      targetPort: 8080
```

更多注解见下一节。

## Service 注解

| 注解 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `kubernetes.io/openlb.instances` | 是 | — | 逗号分隔的 LB 实例地址（要同步到的 agent）。地址可带端口，不带端口时用 `OPENLB_AGENT_PORT`（默认 `8080`） |
| `kubernetes.io/openlb.class` | 否 | `nginx` | 转发器类型：`nginx`（四/七层）或 `lvs`（四层）。**不填该注解**时默认 `nginx`；**填写了空字符串或未知值**会被 agent 拒绝（HTTP 400） |
| `kubernetes.io/openlb.protocol-port` | 否 | TCP/UDP | 显式指定每个端口的协议，格式 `协议:端口`，逗号分隔，如 `http:80, https:443`。支持 `http`/`https`/`tcp`/`udp`；优先级为注解 > `appProtocol` > Service `protocol`，不按端口号猜测 HTTP/HTTPS |
| `kubernetes.io/openlb.cert-secrets` | 否（https 必填） | — | TLS Secret 名称，逗号分隔。必须与 Service 同命名空间；使用 `https` 时必填 |
| `kubernetes.io/openlb.network-type` | 否 | `nodeport` | 后端取址方式：`nodeport` → `nodeIP:nodePort`；`pod` → `podIP:targetPort`，可在 Service spec 中显式设置 `allocateLoadBalancerNodePorts: false` |
| `kubernetes.io/openlb.node-network-type` | 否 | `internal` | `nodeport` 模式下使用哪个 Node 地址：`internal`（内网）或 `external`（外网） |

完整示例见 [deploy/examples/example-service.yaml](deploy/examples/example-service.yaml)（nginx 七层）和 [deploy/examples/example-lvs-service.yaml](deploy/examples/example-lvs-service.yaml)（lvs 四层）。

## 文档

- [docs/AGENT_DEPLOYMENT.md](docs/AGENT_DEPLOYMENT.md) — Agent 二进制与 systemd 主机部署
- [docs/DEPLOYMENT_REVIEW.md](docs/DEPLOYMENT_REVIEW.md) — 部署优化进度与待办

- [docs/RESOURCES.md](docs/RESOURCES.md) — 监听的资源（Service 注解、Node、EndpointSlice、Secret 格式参考）
- [docs/FORWARDER_INTEGRATION.md](docs/FORWARDER_INTEGRATION.md) — 转发器接入指南（如何接入 HAProxy、Envoy 等新转发器）
- [docs/LEADER_ELECTION.md](docs/LEADER_ELECTION.md) — 高可用与 Leader Election
- [docs/METRICS.md](docs/METRICS.md) — Prometheus Metrics 监控
- [docs/TESTING.md](docs/TESTING.md) — 测试指南
- [docs/ROADMAP.md](docs/ROADMAP.md) — 路线图与设计笔记
