# 监听的资源

controller 监听以下 Kubernetes 资源，本文档记录各资源的关键字段格式，供开发和排查参考。

## Service

controller 的核心输入。通过注解描述转发行为。

### 注解

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    kubernetes.io/openlb.instances: "10.10.0.1, 10.10.0.2"
    kubernetes.io/openlb.class: "nginx" # lvs
    kubernetes.io/openlb.protocol-port: "http:80, https:443"
    kubernetes.io/openlb.cert-secrets: "a-com,b-com"
    kubernetes.io/openlb.network-type: "nodeport" # pod
    kubernetes.io/openlb.node-network-type: "internal" # external
```

| 注解 | 含义 |
|---|---|
| `kubernetes.io/openlb.instances` | 逗号分隔的 LB 实例地址（要同步到的 agent） |
| `kubernetes.io/openlb.class` | 按 Service 选择转发器；缺省 `nginx`，不再由环境变量选取。`lvs` 支持 IPv4 TCP/UDP，默认使用 agent 本机内网 IP，可显式覆盖 VIP |
| `kubernetes.io/openlb.protocol-port` | 如 `http:80, https:443`；优先级为此注解 > `spec.ports[].appProtocol` > `spec.ports[].protocol`（默认 TCP），删除注解后按此顺序回退 |
| `kubernetes.io/openlb.cert-secrets` | TLS secret 名称（做反向索引；必须在 Service 所在命名空间） |
| `kubernetes.io/openlb.network-type` | `nodeport`（默认）→ 后端为 `nodeIP:nodePort`，Service 必须已分配 NodePort；`pod` → `podIP:targetPort`，可在 Service spec 中设置 `allocateLoadBalancerNodePorts: false` |
| `kubernetes.io/openlb.node-network-type` | `internal`（默认）/ `external` — nodeport 后端使用哪个节点 IP |

### 计划中的注解（TODO）

```yaml
    kubernetes.io/openlb.adaptive-weight: "true" # 根据节点上的 pod 动态调整后端服务器的权重
    kubernetes.io/openlb.acl: "" # firewalld 规则, 指定 cm 名称进行使用
    kubernetes.io/openlb.session-affinity-mode: "source-ip" # 需要 sticky 模块
    kubernetes.io/openlb.session-affinity-option: '{"timeout": "300s"}'
```

### 完整示例

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    kubernetes.io/openlb.instances: "10.202.0.1, 10.202.0.2"
    kubernetes.io/openlb.class: "nginx" # lvs
    kubernetes.io/openlb.protocol-port: "http:80, https:443"
    kubernetes.io/openlb.cert-secrets: "a-com,b-com"
    kubernetes.io/openlb.network-type: "nodeport" # pod
    kubernetes.io/openlb.node-network-type: "internal" 
  labels:
    app: xxx
  name: xxx
spec:
  type: LoadBalancer
  externalTrafficPolicy: Cluster # <--
  internalTrafficPolicy: Cluster # <--
  allocateLoadBalancerNodePorts: true # <--
  sessionAffinity: None # LB 的会话粘性在注解中读取, 这里是 service 策略, LB 实现应当忽略
  loadBalancerIP: 10.202.0.1 # 已弃用的字段, 不必理会了
  ports:
  - name: http
    port: 80
    protocol: TCP
    targetPort: 8080
  - name: https
    port: 443
    protocol: TCP
    targetPort: 8080
  selector:
    app: xxx
```

## Node

nodeport 后端从 Node 的 `status.addresses` 读取 IP（按 `node-network-type` 选 `InternalIP` 或 `ExternalIP`）。

```yaml
apiVersion: v1
kind: Node
metadata:
  name: lke233625-354437-1f1a9fd90000
status:
  addresses:
  - address: lke233625-354437-1f1a9fd90000
    type: Hostname
  - address: 172.233.56.11
    type: ExternalIP
  - address: 2600:3c0e::f03c:95ff:fec1:2c2e
    type: ExternalIP
  - address: 192.168.149.235
    type: InternalIP
  conditions:
  - lastHeartbeatTime: "2025-03-25T15:41:47Z"
    lastTransitionTime: "2024-09-25T08:09:55Z"
    message: kubelet is posting ready status
    reason: KubeletReady
    status: "True"
    type: Ready
```

## EndpointSlice

pod 后端从 EndpointSlice 的 `endpoints[].addresses` 读取 podIP。controller 下发同一 Service 的 IPv4 分片集合，保留每片独立的 `ports`；命名 targetPort 按 Service 端口名和传输协议逐片解析，支持滚动升级时同名端口对应不同数字端口。重复后端去重，明确未就绪或正在终止的端点被排除；未设置 ready 按 Kubernetes 约定视为就绪。

nginx 支持 `tcp`、`udp`、`http`、`https`。未知/格式错误的协议和前后端传输协议不兼容会拒绝同步，保留旧配置；HTTP/HTTPS 后端必须使用 TCP。同一数字同时声明 TCP/UDP 时，分别选择对应的 NodePort/targetPort。Node 的就绪状态或所用 IPv4 地址改变都会触发重新同步。

```yaml
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: istio-ingressgateway-11-2t74r
  namespace: istio-system
addressType: IPv4
ports:
- name: http
  port: 8080
  protocol: TCP
- name: https
  port: 8080
  protocol: TCP
endpoints:
- addresses:
  - 10.246.73.134
  conditions:
    ready: true
    serving: true
    terminating: false
  nodeName: eklet-subnet-dgqzzz2s-8qycuprm
  targetRef:
    kind: Pod
    name: istio-ingressgateway-8d4bbbdcd-m6vpv
    namespace: istio-system
    uid: f4f94923-2552-48ca-b50e-e7544d4886ec
  zone: "220002"
```

## LVS

`class: lvs` 的传输协议以 `spec.ports[].protocol` 为准。前端协议注解、appProtocol、
证书注解不参与四层转发；存在七层配置时记录 `L7ConfigurationIgnored` 事件。
HTTPS 原样透传，不读取/下发 TLS Secret。详见 [LVS](LVS.md)。

## Secret

TLS 证书从注解 `cert-secrets` 指定的同 namespace Secret 读取，类型必须是 `kubernetes.io/tls`。
controller 观察所有 Secret，以便类型被修改后仍能处理已有保护 finalizer。
首次下发前登记名称/UID 并添加 `openlb.io/certificate-protection`；切换或清理全部成功后才释放。
缺失或无效证书拒绝同步并保留现有配置，详见 [TLS Secret 引用保护](CERTIFICATES.md)。

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: tec-do-cn
  namespace: pac-dsp
type: kubernetes.io/tls
data:
  tls.crt: xx
  tls.key: xx
```
