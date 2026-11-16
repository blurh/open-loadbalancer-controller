# LVS 四层转发

`kubernetes.io/openlb.class: lvs` 使用 Linux IPVS 转发 IPv4 TCP/UDP，agent 通过
`ipvsadm` 增量管理 virtual service 和 real server。默认轮询调度、NAT，后端权重均为 1。
支持 Pod 直连、NodePort、Ready 节点/端点筛选，以及 NodePort 的 ExternalTrafficPolicy=Local。

## 四层与七层配置

传输协议以 `spec.ports[].protocol` 为准，省略时为 TCP。同一个数字端口可以同时声明
TCP 和 UDP，分别选择各自的 NodePort 或命名 targetPort。

LVS 忽略 `kubernetes.io/openlb.protocol-port`、`spec.ports[].appProtocol` 和
`kubernetes.io/openlb.cert-secrets`。其中存在 HTTP/HTTPS、其他应用协议或证书引用时，
controller 在原 Service 上记录 Warning 事件，reason 为 `L7ConfigurationIgnored`。
事件消息列出被忽略的配置；正常同步仍继续。

例如 `protocol: TCP`、`appProtocol: https` 和 `cert-secrets: missing` 会按 TCP 转发，
不会读取、下发或保护该 Secret，也不会因 Secret 不存在而失败。HTTPS 原样透传，TLS
终止由后端承担。纯 TCP/UDP 且没有七层设置时不产生忽略事件。

从 nginx 切换到 LVS 时，原来的证书引用会等所有 agent 完成切换且结果落盘后才释放。
部分 agent 失败仍保留旧 Secret 的 finalizer。

## agent 配置

| 环境变量 | 默认值 | 含义 |
|---|---|---|
| `OPENLB_LVS_VIPS` | 自动发现本机内网 IPv4 | 可选覆盖，逗号分隔的 IPv4 监听地址，用于独立 VIP 或多网卡选择 |
| `OPENLB_LVS_BIN` | `ipvsadm` | 管理工具路径或命令名 |
| `OPENLB_LVS_FORWARDING` | `nat` | `nat`、`dr` 或 `tun`（IPIP） |
| `OPENLB_LVS_SCHEDULER` | `rr` | `rr/wrr/lc/wlc/lblc/lblcr/dh/sh/sed/nq`，需相应内核模块 |
| `OPENLB_LVS_COMMAND_TIMEOUT` | `3s` | 每次 ipvsadm 调用超时 |
| `OPENLB_AGENT_STATE_DIR` | `/var/lib/openlb-agent` | 持久化目录；LVS 记录位于 `lvs/<namespace>_<name>.json` |

默认在启动时读取本机接口和 IPv4 路由，优先使用默认路由网卡上的内网 IPv4；存在多个
默认路由时优先选择 metric 最小的可用网卡。没有匹配的默认路由时，使用唯一可用的内网
IPv4，不需要公网连接或 DNS。自动发现仅选择 RFC1918 地址，排除回环、停用接口、点对点
接口及常见容器网桥/CNI 接口。候选仍有多个或没有可用地址时返回明确错误，可通过
`OPENLB_LVS_VIPS` 指定；显式配置完整覆盖自动发现结果。

每个 Service 的端口会配置到所选地址，并将实际监听地址返回 controller，写入
`status.loadBalancer.ingress`。内网地址使用响应的 InternalIPs；不再额外追加未监听的
管理地址。地址在启动时确定，本机地址变化后需重启 agent，并由后续同步更新规则。
容器自动发现的是其所在网络 namespace 的地址；使用宿主机内网 IP 时需使用宿主网络。

需要独立 VIP 时，仍可显式配置一个或多个地址；额外 VIP 的绑定、路由、ARP 和主备漂移
由部署环境管理。agent 不修改系统地址、路由或全局 IPVS 参数。

NAT 支持监听端口到后端端口的转换，适用于常见 NodePort 和不同 targetPort 的场景。
必须配置 IP 转发和后端经 LVS 返回的路由。DR/TUN 要求前后端端口一致，并按对应模式
配置 real server 的 VIP、ARP 或隧道；不符合端口要求时在修改内核前拒绝请求。
IPVS 命令和模式依据 [ipvsadm 官方手册](https://github.com/horms/ipvsadm/blob/master/ipvsadm.8)。

运行环境需要 Linux IPVS、相应协议/调度模块和 `CAP_NET_ADMIN`。容器镜像已安装
ipvsadm；容器需使用承载 VIP 的网络 namespace，例如宿主网络。内核模块应由宿主机
预先加载，不能仅靠容器里安装 ipvsadm 获得内核支持。

```bash
# 在已配置路由和 IPVS 模块的 LB 上运行，自动使用本机内网 IPv4
export OPENLB_LVS_FORWARDING=nat
export OPENLB_AGENT_STATE_DIR=/var/lib/openlb-agent
openlb-agent

# 或使用项目镜像，共享宿主网络并持久化归属
docker run --network host --cap-add NET_ADMIN \
  -v /srv/openlb/state:/var/lib/openlb-agent \
  openlb-agent
```

示例 Service 见 `deploy/example-lvs-service.yaml`。`openlb.instances` 填写 agent 的管理
地址；示例包含 HTTPS 和不存在的 Secret，以演示忽略事件及 TLS 透传。

```bash
kubectl describe service tls-passthrough
ipvsadm -L -n
```

## 失败与恢复

agent 修改内核前原子持久化已提交配置和新旧 listener 的归属并集；失败时保留 pending
记录及端口锁，并尝试回滚本 Service。重试读取当前内核表，能处理命令成功但响应丢失的
情况。逐条内核更新不是原子切换，失败回滚也可能失败，此时返回错误并保留恢复记录。

新增/调整后端先于移除旧后端执行，不清空全局 IPVS 表。未变化的配置不执行写命令、
不改写状态文件。零后端或输入非法时保留旧配置并返回失败，controller 继续重试。

启动时读取持久化归属并恢复最后已提交的内核规则，支持主机重启后 IPVS 表为空的场景。
未确认操作保留新旧端口占用，等待 controller 带最新资源重试。删除成功后才删除归属文件。
这能恢复规则，不会恢复主机重启前的活动连接。

只修改归属文件记录的 listener。目标地址/协议/端口已有未纳管规则时返回冲突，不自动
接管 kube-proxy 或手工规则，也不执行 `ipvsadm -C`。同一网络 namespace 的托管 listener
只能由一个 agent 管理；不能丢弃归属文件后继续管理仍存在的内核规则。

当前不支持 IPv6、SCTP、fwmark 服务、Service ClientIP sessionAffinity 或主动后端健康探测。
明确请求不支持的传输协议或 sessionAffinity 会报错。连接统计仍是已有的本地 socket
采集器，其 `/proc/net/tcp`/`udp` 指标不代表 IPVS 转发连接数；查看 IPVS 连接请使用
`ipvsadm -L -n --connection` 或 `ipvsadm -L -n`。

## 验证

```bash
go test ./pkg/agent/lvs ./pkg/agent/parse ./pkg/controllers
go test -race ./pkg/agent/... ./pkg/controllers
```

单元测试用内存内核表和命令脚本覆盖增删改、幂等、端口/后端变化、失败回滚、恢复记录、
主机重启、权限错误和命令超时；地址测试覆盖默认路由、多网卡、容器接口排除、显式覆盖
及内网 IP 回报；controller 测试覆盖七层忽略事件、证书引用释放和实际监听地址回写。
真实内核转发、VIP/ARP、NAT 回程和 DR/TUN 网络连通性需要在目标 LB 环境验证。
