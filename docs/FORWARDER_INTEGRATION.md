# 转发器接入指南

本文档说明如何将新的转发器（如 HAProxy、Envoy、Traefik 等）接入到 openlb-controller 系统。

## 目录

- [架构概览](#架构概览)
- [接口定义](#接口定义)
- [实现步骤](#实现步骤)
- [指标采集](#指标采集)
- [配置管理](#配置管理)
- [测试验证](#测试验证)
- [完整示例：nginx](#完整示例nginx)

---

## 架构概览

openlb-controller 采用**插件化架构**，转发器通过实现 `Forwarder` 接口接入系统：

```
Controller (集群内)                Agent (LB 实例上)
      │                                  │
      │  HTTP /sync, /cleanup            │
      ├──────────────────────────────────►│
      │  ControllerResources              │
      │  (Service + Nodes + EndpointSlice)│
      │                                   │
      │                        ┌──────────▼─────────┐
      │                        │  Forwarder 接口     │
      │                        │  (nginx/lvs/...)   │
      │                        └──────────┬─────────┘
      │                                   │
      │                        ┌──────────▼─────────┐
      │                        │  渲染配置 + Reload │
      │                        │  nginx/haproxy/... │
      │                        └────────────────────┘
      │                                   │
      │◄──────────────────────────────────┤
      │  RespBody (external/internal IPs) │
```

### 核心组件

| 组件 | 职责 | 位置 |
|------|------|------|
| **Forwarder 接口** | 定义转发器的标准操作 | `pkg/agent/agent.go` |
| **转发器实现** | 具体的配置渲染和 reload 逻辑 | `pkg/agent/<forwarder>/` |
| **通用采集器** | L4 (TCP) 和 UDP 连接数采集 | `pkg/agent/stats/` |
| **特有采集器** | L7 或转发器专属指标采集 | `pkg/agent/<forwarder>/stats.go` |
| **指标定义** | Prometheus 指标注册和更新 | `pkg/agent/metrics/` |

---

## 接口定义

### Forwarder 接口

所有转发器必须实现 `pkg/agent/agent.go` 中定义的 `Forwarder` 接口：

```go
type Forwarder interface {
    // PreflightCheck 自检底层转发器是否可用（如 nginx -t）
    PreflightCheck() error
    
    // Apply 全量幂等地同步一个 Service 的配置并 reload
    // 返回回写到 Service.status.loadBalancer.ingress 的 IP 列表
    Apply(cr sync.ControllerResources) (sync.RespBody, error)
    
    // Delete 删除一个 Service 的配置并 reload
    Delete(ref sync.ServiceRef) error
    
    // List 返回当前被纳管的 Service（用于启动对账 / GC 孤儿配置）
    List() ([]sync.ServiceRef, error)

    // ListListeners 恢复配置中的全部监听端口归属（包含 HTTP/HTTPS 和部分写入的文件）
    ListListeners() (map[sync.ServiceRef][]servicelock.Port, error)
    
    // L4Ports 返回当前转发器监听的四层(TCP)端口列表
    // 通用四层连接数采集器据此从 /proc/net/tcp 过滤统计连接数
    L4Ports() []int
    
    // UDPPorts 返回当前转发器监听的 UDP 端口列表
    // 通用 UDP 套接字采集器据此从 /proc/net/udp 过滤统计套接字数
    UDPPorts() []int
}
```

### 数据结构

**输入：ControllerResources**（`pkg/sync/types.go`）

```go
type ControllerResources struct {
    Service        corev1.Service
    Nodes          []corev1.Node
    EndpointSlices []discoveryv1.EndpointSlice
    Secrets        []corev1.Secret
}
```

**输出：RespBody**（`pkg/sync/types.go`）

```go
type RespBody struct {
    ExternalIPs []string `json:"external_ips,omitempty"`
    InternalIPs []string `json:"internal_ips,omitempty"`
}
```

如果返回空的 `RespBody`，controller 会使用 agent 实例的地址作为 ingress IP。

---

## 实现步骤

### 步骤 1：创建转发器包

在 `pkg/agent/<forwarder>/` 下创建新包，例如 `pkg/agent/haproxy/`。

### 步骤 2：实现 Forwarder 接口

```go
// pkg/agent/haproxy/haproxy.go
package haproxy

import (
    "os"
    "os/exec"
    stdsync "sync"
    
    "github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
    "github.com/blurh/open-loadbalancer-controller/pkg/config"
    "github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

type Agent struct {
    haproxyBin    string
    haproxyConfig string
    
    // mu 串行化 Apply/Delete：避免并发写配置 + reload
    mu stdsync.Mutex
}

func New() (*Agent, error) {
    cfg := config.NewConfig()
    
    return &Agent{
        haproxyBin:    cfg.HaproxyBin(),
        haproxyConfig: cfg.HaproxyConfig(),
    }, nil
}

func (a *Agent) PreflightCheck() error {
    // 校验配置文件是否合法
    return exec.Command(a.haproxyBin, "-c", "-f", a.haproxyConfig).Run()
}

func (a *Agent) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    // 1. 解析 Service annotations，提取协议、端口、TLS 配置
    // 2. 根据 network-type 生成 backend 列表（nodeport 或 pod）
    // 3. 渲染 HAProxy 配置文件（frontend + backend）
    // 4. 写入磁盘
    // 5. 执行 reload
    
    return sync.RespBody{}, nil  // 返回空，controller 使用实例地址
}

func (a *Agent) Delete(ref sync.ServiceRef) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    // 1. 删除对应 Service 的配置文件
    // 2. 执行 reload
    
    return nil
}

func (a *Agent) List() ([]sync.ServiceRef, error) {
    // 扫描配置目录，提取当前管理的 Service 列表
    // 通常从配置文件名或注释中解析 namespace/name
    
    return []sync.ServiceRef{}, nil
}

func (a *Agent) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) {
    // TODO: 从持久化配置恢复 Service -> 端口；不能用 L4Ports 替代，它不含 HTTP/HTTPS 归属。
    // HTTP/HTTPS/TCP 统一返回 Protocol: "tcp"，UDP 返回 "udp"；Number 为监听端口。
    // 读取失败必须返回错误；部分生成的配置也应保守地计入。
    return map[sync.ServiceRef][]servicelock.Port{}, nil
}

func (a *Agent) L4Ports() []int {
    // 解析 HAProxy 配置，提取 mode tcp 的 frontend bind 端口
    // 示例：bind :8080  →  返回 [8080]
    
    ports := []int{}
    // TODO: 实现配置解析逻辑
    return ports
}

func (a *Agent) UDPPorts() []int {
    // 解析 HAProxy 配置，提取 bind 指令中带 ,udp 的端口
    // 示例：bind :53,udp  →  返回 [53]
    // 注意：HAProxy 2.3+ 才支持 UDP
    
    ports := []int{}
    // TODO: 实现配置解析逻辑
    return ports
}
```

### 步骤 3：注册到工厂函数

在 `pkg/agent/agent.go` 的 `NewAgent()` 工厂表中添加新转发器。进程启动时对各实现执行
`PreflightCheck()`、`List()` 和 `ListListeners()`，初始化本机可用实现并恢复 Service 归属。请求通过 Service 的
`kubernetes.io/openlb.class` 选择实现；缺少注解默认 nginx，未知 class 返回 HTTP 400。
未安装或未实现的转发器会被标记不可用，不影响其他可用实现；全部不可用时启动失败。

```go
func NewAgent() (*Agent, error) {
    return newAgent(map[string]forwarderFactory{
        "nginx":   func() (Forwarder, error) { return nginx.New() },
        "lvs":     func() (Forwarder, error) { return lvs.New() },
        "haproxy": func() (Forwarder, error) { return haproxy.New() },
    }, config.NewConfig().AgentStateDir())
}
```

每个 class 复用同一个转发器实例。Service 切换 class 时先清理旧配置，失败则保留归属并返回错误；
目标 class 不可用时不会清理旧配置。`/cleanup` 不携带 class，依赖 agent 的归属记录，
因此 `List()` 必须能在重启后从持久化配置恢复 Service，读取失败时应返回错误。
归属未知的 cleanup 会调用所有可用实现的幂等 `Delete()`，以覆盖文件删除后 reload 失败的重启情况。

端口锁由分派层统一管理，所有 class 共享 TCP/UDP 端口空间。分派层先持久化预留，再调用
`Apply/Delete`；失败保留锁，成功才释放过期端口。`Apply` 必须遵守 `cr.ForceReload`，
未完成操作在重启后的重试中会携带此标记，即使配置没变化也必须 reload。
`ListListeners()` 用于启动恢复和成功 Apply 后确认保留的配置，必须包含七层及部分写入的配置。
新增转发器的端口语义须与 `servicelock.RequestedPorts()` 保持一致；目前支持 TCP/UDP，HTTP/HTTPS 映射为 TCP。
状态文件位于 `OPENLB_AGENT_STATE_DIR`（默认 `/var/lib/openlb-agent`），需和转发器配置一起持久化。

### 步骤 4：添加配置项

在 `pkg/config/config.go` 中添加转发器相关的环境变量：

```go
type Config struct {
    // ... 现有字段
    
    // HAProxy 配置（新增）
    haproxyBin    string
    haproxyConfig string
}

func NewConfig() *Config {
    return &Config{
        // ... 现有初始化
        
        haproxyBin:    getEnvOrDefault("OPENLB_HAPROXY_BIN", "/usr/sbin/haproxy"),
        haproxyConfig: getEnvOrDefault("OPENLB_HAPROXY_CONFIG", "/etc/haproxy/haproxy.cfg"),
    }
}

func (c *Config) HaproxyBin() string    { return c.haproxyBin }
func (c *Config) HaproxyConfig() string { return c.haproxyConfig }
```

---

## 指标采集

指标采集分为**通用指标**（自动生效）和**特有指标**（需实现）。

### 自动生效的通用指标

以下指标**无需额外代码**，实现 `L4Ports()` 和 `UDPPorts()` 后自动采集：

#### 1. L4 (TCP) 连接数

- **指标名**：`openlb_agent_connections{layer="l4", state="active|inactive|waiting"}`
- **采集方式**：通过 `pkg/agent/stats/l4.go` 从 `/proc/net/tcp` 采集
- **工作原理**：
  1. 调用 `Forwarder.L4Ports()` 获取当前 TCP 监听端口
  2. 解析 `/proc/net/tcp` 和 `/proc/net/tcp6`
  3. 过滤出本地端口在 `L4Ports()` 返回列表中的连接
  4. 统计 ESTABLISHED（active）和其他状态（inactive）

#### 2. UDP 套接字数

- **指标名**：`openlb_agent_udp_sockets`
- **采集方式**：通过 `pkg/agent/stats/udp.go` 从 `/proc/net/udp` 采集
- **工作原理**：
  1. 调用 `Forwarder.UDPPorts()` 获取当前 UDP 监听端口
  2. 解析 `/proc/net/udp` 和 `/proc/net/udp6`
  3. 统计本地端口在 `UDPPorts()` 返回列表中的套接字数量

#### 3. 业务操作指标

以下指标由 `pkg/agent/metrics` 自动记录，无需转发器实现：

- `openlb_agent_operation_total{operation="apply|delete", status="success|failure", service="ns/name"}`
- `openlb_agent_operation_duration_seconds{operation="apply|delete"}`
- `openlb_agent_services_managed`：当前管理的 Service 数量
- `openlb_agent_reload_total{status="success|failure"}`：转发器 reload 次数

**使用方法**：

```go
// 在 Apply/Delete 中调用
import "github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"

func (a *Agent) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
    timer := metrics.ObserveOperationDuration("apply")
    defer timer.ObserveDuration()
    
    service := fmt.Sprintf("%s/%s", cr.Service.Namespace, cr.Service.Name)
    
    // ... 实现逻辑
    
    if err != nil {
        metrics.RecordOperation("apply", "failure", service)
        return sync.RespBody{}, err
    }
    
    metrics.RecordOperation("apply", "success", service)
    return sync.RespBody{}, nil
}

// 在 reload 中调用
func (a *Agent) reload() error {
    if err := exec.Command(a.haproxyBin, "-sf", ...).Run(); err != nil {
        metrics.RecordReload("failure")
        return err
    }
    metrics.RecordReload("success")
    return nil
}
```

### 转发器特有的 L7 指标

如果转发器有类似 nginx stub_status 的统计接口，可以实现专属采集器。

#### 示例：HAProxy Stats Socket

```go
// pkg/agent/haproxy/stats.go
package haproxy

import (
    "context"
    "net"
    "time"
    
    "github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
)

// HAPStatsCollector 从 HAProxy stats socket 采集七层连接数
type HAPStatsCollector struct {
    socketPath string
    interval   time.Duration
}

func NewHAPStatsCollector(socketPath string, interval time.Duration) *HAPStatsCollector {
    return &HAPStatsCollector{
        socketPath: socketPath,
        interval:   interval,
    }
}

func (hc *HAPStatsCollector) Start(ctx context.Context) {
    ticker := time.NewTicker(hc.interval)
    defer ticker.Stop()
    
    hc.collect()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            hc.collect()
        }
    }
}

func (hc *HAPStatsCollector) collect() {
    // 1. 连接 HAProxy stats socket
    conn, err := net.Dial("unix", hc.socketPath)
    if err != nil {
        return  // 采集失败静默忽略
    }
    defer conn.Close()
    
    // 2. 发送 "show stat" 命令
    conn.Write([]byte("show stat\n"))
    
    // 3. 解析 CSV 输出，提取 scur (current sessions)
    active := parseHAProxyStats(conn)
    
    // 4. 更新到 L7 指标（复用 nginx 的 layer="l7"）
    metrics.SetConnectionsByLayer("l7", active, 0, 0)
}
```

#### 在 agent 主程序中启动

在 `cmd/agent/main.go` 的 `main()` 中添加：

```go
// HAProxy 特有采集器
if a.HasForwarder("haproxy") {
    if statsSocket := c.HaproxyStatsSocket(); statsSocket != "" && statsSocket != "0" {
        hapStats := haproxy.NewHAPStatsCollector(statsSocket, interval)
        go hapStats.Start(ctx)
        log.Printf("HAProxy stats collector started: %s", statsSocket)
    }
}
```

### 指标设计原则

| 原则 | 说明 | 示例 |
|------|------|------|
| **持续暴露** | 采集器始终运行，端口列表为空时指标归零（不停止采集） | `L4Ports()` 返回 `[]` → `openlb_agent_connections{layer="l4"}` = 0 |
| **多转发器聚合** | 同进程多个 L7 采集器需增加 class 标签或统一聚合，避免相互覆盖 | 新增 HAProxy 采集器时同步调整指标定义 |
| **静默容错** | 采集失败不打日志、不中断，保持上次值 | stub_status 503 → 跳过本轮，等待下次 ticker |
| **配置控制** | 通过 URL/socket 为 `0` 或空字符串禁用采集器 | `OPENLB_NGINX_STATUS_URL=0` → 不启动 L7 采集器 |

---

## 配置管理

### 环境变量规范

| 类型 | 命名规范 | 示例 |
|------|---------|------|
| 转发器二进制 | `OPENLB_<FORWARDER>_BIN` | `OPENLB_HAPROXY_BIN=/usr/sbin/haproxy` |
| 配置文件路径 | `OPENLB_<FORWARDER>_CONFIG` | `OPENLB_HAPROXY_CONFIG=/etc/haproxy/haproxy.cfg` |
| 配置根目录 | `OPENLB_<FORWARDER>_HOME` | `OPENLB_NGINX_HOME=/etc/nginx/` |
| L7 统计接口 | `OPENLB_<FORWARDER>_STATUS_<TYPE>` | `OPENLB_HAPROXY_STATUS_SOCKET=/var/run/haproxy.sock` |

### 配置文件组织建议

**方式 1：子目录隔离**（推荐，类似 nginx）

```
/etc/haproxy/
├── haproxy.cfg          # 主配置，include conf.d/*.cfg
└── conf.d/
    ├── ns1_svc1.cfg     # Service 专属配置
    └── ns2_svc2.cfg
```

- `List()` 通过扫描 `conf.d/` 解析文件名获取 Service 列表
- `Apply()` 写入 `conf.d/<ns>_<name>.cfg`
- `Delete()` 删除对应文件

**方式 2：单文件追加**（适用于简单转发器）

```
/etc/forwarder/forwarder.cfg  # 包含所有 Service 配置，用注释分隔
```

- `List()` 解析注释标记（如 `# Service: ns/name`）
- `Apply()` 查找并替换对应 Service 的配置段
- `Delete()` 删除对应配置段

---

## 测试验证

### 单元测试

为核心逻辑编写单元测试：

```go
// pkg/agent/haproxy/haproxy_test.go
package haproxy

import (
    "testing"
    
    corev1 "k8s.io/api/core/v1"
    "github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

func TestApply(t *testing.T) {
    agent, _ := New()
    
    cr := sync.ControllerResources{
        Service: corev1.Service{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "test-svc",
                Namespace: "default",
            },
            Spec: corev1.ServiceSpec{
                Ports: []corev1.ServicePort{
                    {Port: 80, Protocol: corev1.ProtocolTCP},
                },
            },
        },
        // ... 填充 Nodes, EndpointSlices
    }
    
    _, err := agent.Apply(cr)
    if err != nil {
        t.Fatalf("Apply failed: %v", err)
    }
    
    // 验证配置文件是否生成
    // 验证 L4Ports() 是否返回 [80]
}
```

### 集成测试

1. **构建 agent 二进制**：`go build -o /tmp/openlb-agent ./cmd/agent/`
2. **准备转发器环境**：安装 haproxy/envoy/...
3. **启动 agent**：`/tmp/openlb-agent`
4. **发送测试请求**：在 `testdata/service.json` 的 `service.metadata.annotations` 中设置 `kubernetes.io/openlb.class: haproxy`：
   ```bash
   curl -X POST http://localhost:8080/sync \
     -H "Content-Type: application/json" \
     -d @testdata/service.json
   ```
5. **验证指标**：`curl http://localhost:8082/metrics | grep openlb_agent`

### 验证清单

- [ ] `PreflightCheck()` 能检测转发器未安装
- [ ] `Apply()` 成功渲染并 reload 配置
- [ ] `Delete()` 能清理配置并 reload
- [ ] `List()` 返回正确的 Service 列表
- [ ] `L4Ports()` 和 `UDPPorts()` 返回当前监听端口
- [ ] L4/UDP 通用指标正常采集
- [ ] 业务指标（apply/delete/reload）正常记录
- [ ] 配置变更后转发器能正常处理流量

---

## 完整示例：nginx

以下是 nginx 转发器的完整实现，可作为接入新转发器的参考。

### 1. 接口实现

```go
// pkg/agent/nginx/nginx.go
package nginx

import (
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "sync"
    
    "github.com/blurh/open-loadbalancer-controller/pkg/config"
    "github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

type Agent struct {
    nginxBin  string
    nginxHome string
    mu        sync.Mutex
}

func New() (*Agent, error) {
    cfg := config.NewConfig()
    return &Agent{
        nginxBin:  cfg.NginxBin(),
        nginxHome: cfg.NginxHome(),
    }, nil
}

func (a *Agent) PreflightCheck() error {
    // 检查 nginx 是否可执行
    return exec.Command(a.nginxBin, "-t").Run()
}

func (a *Agent) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    svc := cr.Service
    ns, name := svc.Namespace, svc.Name
    
    // 1. 解析 Service annotations
    proto := parseProtocolPort(svc.Annotations)
    networkType := getAnnotation(svc.Annotations, "network-type", "nodeport")
    
    // 2. 生成 backend 列表
    backends := a.genBackends(cr, networkType)
    
    // 3. 渲染配置文件
    for _, port := range svc.Spec.Ports {
        if len(backends[port.Port]) == 0 {
            continue  // 跳过零 backend 端口
        }
        
        // 渲染 upstream 文件
        upstreamPath := filepath.Join(a.nginxHome, "conf.d/http", 
            fmt.Sprintf("%s_%s_%d_upstream.conf", ns, name, port.Port))
        a.renderUpstream(upstreamPath, backends[port.Port])
        
        // 渲染 server 文件
        serverPath := filepath.Join(a.nginxHome, "conf.d/http",
            fmt.Sprintf("%s_%s_%d.conf", ns, name, port.Port))
        a.renderServer(serverPath, port, proto[port.Port])
    }
    
    // 4. GC 旧端口的配置文件
    a.gcServiceFiles(ns, name)
    
    // 5. Reload nginx
    return sync.RespBody{}, a.reload()
}

func (a *Agent) Delete(ref sync.ServiceRef) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    // 删除所有相关配置文件
    prefix := fmt.Sprintf("%s_%s_", ref.Namespace, ref.Name)
    confDirs := []string{
        filepath.Join(a.nginxHome, "conf.d/http"),
        filepath.Join(a.nginxHome, "conf.d/stream"),
    }
    
    for _, dir := range confDirs {
        files, _ := os.ReadDir(dir)
        for _, f := range files {
            if strings.HasPrefix(f.Name(), prefix) {
                os.Remove(filepath.Join(dir, f.Name()))
            }
        }
    }
    
    return a.reload()
}

func (a *Agent) List() ([]sync.ServiceRef, error) {
    // 扫描配置目录，从文件名解析 Service
    seen := make(map[string]struct{})
    var refs []sync.ServiceRef
    
    confDirs := []string{
        filepath.Join(a.nginxHome, "conf.d/http"),
        filepath.Join(a.nginxHome, "conf.d/stream"),
    }
    
    for _, dir := range confDirs {
        files, _ := os.ReadDir(dir)
        for _, f := range files {
            // 文件名格式: <ns>_<name>_<port>.conf
            parts := strings.SplitN(f.Name(), "_", 3)
            if len(parts) < 2 {
                continue
            }
            
            key := parts[0] + "/" + parts[1]  // ns/name
            if _, ok := seen[key]; ok {
                continue
            }
            seen[key] = struct{}{}
            
            refs = append(refs, sync.ServiceRef{
                Namespace: parts[0],
                Name:      parts[1],
            })
        }
    }
    
    return refs, nil
}

func (a *Agent) L4Ports() []int {
    // 解析 conf.d/stream/*.conf，提取 listen 端口
    ports := []int{}
    streamDir := filepath.Join(a.nginxHome, "conf.d/stream")
    
    files, _ := os.ReadDir(streamDir)
    for _, f := range files {
        // 从文件名提取端口: ns_name_8080.conf → 8080
        parts := strings.Split(strings.TrimSuffix(f.Name(), ".conf"), "_")
        if len(parts) >= 3 {
            port, _ := strconv.Atoi(parts[len(parts)-1])
            if port > 0 {
                ports = append(ports, port)
            }
        }
    }
    
    return ports
}

func (a *Agent) UDPPorts() []int {
    // nginx stream 模块的 UDP 配置: listen 53 udp;
    // 需解析配置文件内容，实现略
    return []int{}
}

func (a *Agent) reload() error {
    return exec.Command(a.nginxBin, "-s", "reload").Run()
}
```

### 2. L7 指标采集

```go
// pkg/agent/nginx/stats.go
package nginx

import (
    "bufio"
    "context"
    "net/http"
    "strconv"
    "strings"
    "time"
    
    "github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
)

type StatsCollector struct {
    stubStatusURL   string
    httpClient      *http.Client
    streamPortsFunc func() []int
    interval        time.Duration
}

func NewStatsCollector(stubStatusURL string, streamPortsFunc func() []int, 
                       interval time.Duration) *StatsCollector {
    return &StatsCollector{
        stubStatusURL:   stubStatusURL,
        streamPortsFunc: streamPortsFunc,
        interval:        interval,
        httpClient: &http.Client{
            Timeout: 3 * time.Second,
        },
    }
}

func (sc *StatsCollector) Start(ctx context.Context) {
    ticker := time.NewTicker(sc.interval)
    defer ticker.Stop()
    
    sc.collect()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            sc.collect()
        }
    }
}

func (sc *StatsCollector) collect() {
    // 采集七层指标
    if sc.stubStatusURL != "" && sc.stubStatusURL != "0" {
        sc.collectL7()
    }
    
    // 采集四层指标（基于 stream 端口）
    if sc.streamPortsFunc != nil {
        sc.collectL4()
    }
}

func (sc *StatsCollector) collectL7() {
    resp, err := sc.httpClient.Get(sc.stubStatusURL)
    if err != nil {
        return  // 静默忽略
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        return
    }
    
    scanner := bufio.NewScanner(resp.Body)
    var active, waiting int
    
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if strings.HasPrefix(line, "Active connections:") {
            parts := strings.Fields(line)
            if len(parts) >= 3 {
                active, _ = strconv.Atoi(parts[2])
            }
        } else if strings.HasPrefix(line, "Reading:") {
            parts := strings.Fields(line)
            if len(parts) >= 6 {
                waiting, _ = strconv.Atoi(parts[5])
            }
        }
    }
    
    // 更新 L7 指标
    metrics.SetConnectionsByLayer("l7", active, 0, waiting)
}

func (sc *StatsCollector) collectL4() {
    ports := sc.streamPortsFunc()
    if len(ports) == 0 {
        metrics.SetConnectionsByLayer("l4", 0, 0, 0)
        return
    }
    
    // 从 /proc/net/tcp 统计这些端口的连接数
    portSet := make(map[int]struct{}, len(ports))
    for _, p := range ports {
        portSet[p] = struct{}{}
    }
    
    active, inactive := sc.parseTCPStats(portSet)
    metrics.SetConnectionsByLayer("l4", active, inactive, 0)
}
```

### 3. 在 agent 主程序中启动

```go
// cmd/agent/main.go
func main() {
    // ... 初始化 agent
    
    c := config.NewConfig()
    interval := c.ConnectionStatsInterval()
    
    // 通用 L4/UDP 采集器（对所有转发器生效）
    if c.EnableL4Stats() {
        l4Collector := stats.NewL4Collector(a.L4Ports, interval)
        go l4Collector.Start(ctx)
    }
    
    if c.EnableL4Stats() {
        udpCollector := stats.NewUDPCollector(a.UDPPorts, interval)
        go udpCollector.Start(ctx)
    }
    
    // nginx 专属 L7 采集；L4/UDP 通过分派层汇总所有转发器的端口。
    if a.HasForwarder("nginx") {
        if statusURL := c.NginxStatusURL(); statusURL != "" && statusURL != "0" {
            statsCollector := nginx.NewL7Collector(statusURL, interval)
            go statsCollector.Start(ctx)
        }
    }
    
    // ... 启动 HTTP server
}
```

---

## 关键要点总结

### 必须实现

1. ✅ `Forwarder` 接口的 7 个方法
2. ✅ 在 `NewAgent()` 中注册转发器
3. ✅ 添加配置项到 `pkg/config/config.go`
4. ✅ 实现 `L4Ports()` 和 `UDPPorts()` 以启用通用指标

### 自动生效

1. ✅ L4 (TCP) 连接数指标：`openlb_agent_connections{layer="l4"}`
2. ✅ UDP 套接字数指标：`openlb_agent_udp_sockets`
3. ✅ 业务指标：apply/delete/reload 计数和耗时

### 可选实现

1. ⚠️ L7 指标采集器（如果转发器有统计接口）
2. ⚠️ 自定义指标（转发器特有的监控需求）

### 设计原则

- **接口驱动**：通过 `Forwarder` 接口解耦，新转发器无需修改 controller
- **分层清晰**：通用能力在 `pkg/agent/stats`，特有能力在 `pkg/agent/<forwarder>`
- **持续暴露**：采集器始终运行，端口列表为空时指标归零（不停止）
- **静默容错**：采集失败不影响主流程，不打日志、不中断

---

## 参考资料

- `pkg/agent/agent.go`：`Forwarder` 接口定义
- `pkg/agent/nginx/`：nginx 完整实现参考
- `pkg/agent/stats/`：通用采集器实现
- `pkg/agent/metrics/`：指标定义和注册
- `pkg/sync/types.go`：controller-agent 数据契约
- `CLAUDE.md`：项目架构和约定

