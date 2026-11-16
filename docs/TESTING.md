# ServiceReconciler 测试文档

## 概述

`service_test.go` 为 `ServiceReconciler.Reconcile` 方法提供了全面的单元测试覆盖，涵盖了三条核心路径和各种边界情况。

## 测试覆盖率

覆盖率随实现更新，使用 `go test -cover ./pkg/controllers ./pkg/store` 获取当前值。

## 测试架构

### Mock 实现

#### mockSyncer
模拟 `sync.Syncer` 接口，记录所有 `Sync` 和 `Clean` 调用，可配置返回错误。

```go
type mockSyncer struct {
    syncCalls  []syncCall
    cleanCalls []cleanCall
    syncErr    error
    cleanErr   error
}
```

#### mockStore

包装真实 `CRDStore` 和 fake Kubernetes API，测试实例状态、版本冲突与 finalizer。
保留地址列表辅助断言，并可注入写入错误；Service 和 CR 都启用 status 子资源。

### Fake Kubernetes Client

使用 `sigs.k8s.io/controller-runtime/pkg/client/fake` 提供的 fake client，支持：
- 完整的 CRUD 操作
- Status 子资源
- 冲突重试模拟

## 测试用例

### 1. TestReconcile_NormalPath
**测试场景**: 正常路径 - 创建/更新 LoadBalancer Service

**断言**:
- ✅ `Syncer.Sync` 被调用正确次数（每个 instance 一次）
- ✅ repo 记录实例列表
- ✅ `service.status.loadBalancer.ingress` 被写入
- ✅ finalizer 被添加
- ✅ 裸 IP 自动补上 agent 端口

### 2. TestReconcile_Deletion
**测试场景**: 删除路径 - Service 被标记删除

**断言**:
- ✅ 依据预先持久化的实例记录执行清理，跳过已成功的实例
- ✅ `Syncer.Clean` 被调用正确次数
- ✅ repo 记录被删除
- ✅ finalizer 被移除

### 3. TestReconcile_Deletion_CleanupFailed
**测试场景**: 删除时 cleanup 失败

**断言**:
- ✅ 返回 error 触发 requeue
- ✅ finalizer **保留**（防止 Service 卡在 Terminating）
- ✅ repo 记录**保留**（下一轮重试）

### 4. TestReconcile_TypeFlip
**测试场景**: 类型翻转 - LoadBalancer → ClusterIP

**断言**:
- ✅ 清理 agent 配置（调用 `Syncer.Clean`）
- ✅ `service.status.loadBalancer` 被清空
- ✅ finalizer 被移除
- ✅ repo 记录被删除

### 5. TestReconcile_InstanceDiff
**测试场景**: 实例增减 - annotation 实例列表变化

**断言**:
- ✅ cleanup 被调用，移除不在新列表中的实例
- ✅ sync 被调用，推送当前列表中的所有实例
- ✅ repo 记录更新为新列表

### 6. TestReconcile_SyncFailed
**测试场景**: setup 时 agent 同步失败

**断言**:
- ✅ 返回 error 触发 requeue
- ✅ `forceReload` 标记被设置（下一轮强制 reload）
- ✅ 先记录全部目标，部分成功时保存各实例的不同阶段

补充回归测试 `TestReconcile_SyncFailurePreservesStatusAndRecovers`：

- 首次部分失败、先失败后成功、已有 ingress、全部失败时均不调用 status patch，并保留原有 status。
- 失败仍保留 finalizer、全部目标实例和 `ForceReload` 标记，仅发送 `AgentSyncFailed` 事件。
- 恢复后所有 agent 都收到 `ForceReload=true`，一次发布完整 ingress，保留其他 status 字段并发送成功事件。

`TestReconcile_SetupErrorDoesNotPublishStatus` 还覆盖 EndpointSlice/Node/Secret 读取、finalizer、repo、status 写入失败：错误继续向上传播，不发布成功状态或成功事件。

### 7. TestReconcile_ForceReloadCleared
**测试场景**: 成功后清除 forceReload 标记

**断言**:
- ✅ 预置的 `forceReload` 标记在成功后被清除

### 8. TestReconcile_ServiceNotFound
**测试场景**: Service 不存在

**断言**:
- ✅ 直接返回成功，不 requeue
- ✅ syncer 和 repo 均未被调用

### 9. TestReconcile_AppendAgentPort
**测试场景**: 裸 IP 自动补端口

**断言**:
- ✅ annotation 中的裸 IP `10.0.0.1` 被补上 `:8080`（默认 agent 端口）

## 运行测试

```bash
# 运行所有 controllers 测试
go test ./pkg/controllers/ -v

# 运行单个测试
go test ./pkg/controllers/ -run TestReconcile_NormalPath -v

# 生成覆盖率报告
go test ./pkg/controllers/ -coverprofile=coverage.out
go tool cover -html=coverage.out -o coverage.html

# 运行整个项目的测试
go test ./... -timeout 30s
```

## 核心测试模式

### Table-Driven Tests
虽然当前实现为独立测试函数（便于精确断言每个场景），也可重构为 table-driven 风格：

```go
func TestReconcile(t *testing.T) {
    tests := []struct {
        name      string
        objects   []client.Object
        repoState map[string]string
        want      wantState
    }{
        // ...
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // ...
        })
    }
}
```

### 依赖注入
通过 `newTestReconciler` 注入 mock 依赖，实现完全的单元隔离：

```go
r, syncer, repo := newTestReconciler([]client.Object{svc, node, eps})
```

### 断言模式
- **正向断言**: 验证期望的行为发生（如 `len(syncer.syncCalls) == 2`）
- **负向断言**: 验证不期望的行为未发生（如 `len(syncer.cleanCalls) == 0`）
- **状态断言**: 验证系统状态变更（如 finalizer 被添加/移除）

## 未覆盖的场景

以下场景需要集成测试或 e2e 测试覆盖：

1. **EndpointSlice 聚合**: 多个 EndpointSlice 合并逻辑（当前假设单片）
2. **Secret 索引**: `cert-secrets` 注解的字段索引查找
3. **Node/EndpointSlice/Secret Watch**: mapper 函数触发 reconcile
4. **并发 Reconcile**: 多个 Service 同时 reconcile
5. **冲突重试**: `RetryOnConflict` 的真实重试行为
6. **Event 持久化**: setup 的成功/失败事件已通过 FakeRecorder 验证，实际 API Server 中的事件记录仍需集成测试

## 维护建议

1. **新增功能时同步更新测试**: 修改 `Reconcile` 逻辑时，确保对应测试用例也更新
2. **保持 mock 简单**: mockSyncer/mockStore 只记录调用，不模拟复杂逻辑
3. **使用辅助函数**: `lbService()` / `node()` / `endpointSlice()` 简化测试对象构造
4. **断言要具体**: 避免只检查 error 是否为 nil，要断言具体的状态变更

## 参考

- [controller-runtime fake client](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake)
- [Kubernetes client-go testing](https://github.com/kubernetes/client-go/tree/master/testing)
- [Table Driven Tests in Go](https://dave.cheney.net/2019/05/07/prefer-table-driven-tests)

## Agent Service 端口锁回归测试

测试位于 `pkg/agent/service_lock_test.go`、`pkg/agent/servicelock/store_test.go`、`pkg/agent/nginx/listeners_test.go` 和 `pkg/agent/server/server_test.go`。

- HTTP/HTTPS/TCP 冲突、TCP/UDP 独立、跨 namespace/class 的归属与并发抢占。
- 多端口请求原子检查，冲突时不清理旧 class、不写转发器配置。
- 幂等更新、成功更新释放旧端口、失败保留旧新端口、重复 cleanup 不释放他人的锁。
- Apply/Delete 失败且文件已变化或删除后重启，锁仍保留；重试强制 reload 后才释放。
- 持久化失败、损坏或冲突状态、旧配置迁移、无后端预留与不可用转发器的锁保留。
- HTTP 409 返回端口和持有者；实际 Nginx 渲染器配合模拟 nginx 命令验证 reload 失败/重试流程（不启动真实 nginx）。

```bash
go test -count=1 -timeout=90s ./...
go test -race -count=1 -timeout=120s ./pkg/agent/...
```

## CRD 状态存储测试

`pkg/store/crd_test.go`、`pkg/controllers/state_test.go` 覆盖 namespace 隔离、UID 重建、
无变化不写、冲突重试、先持久化后调用 agent、重启恢复、逐实例清理进度和 CR 删除。
`pkg/apis/v1alpha1/schema_test.go` 校验部署 CRD 的结构 schema。

真实 API 测试单独使用 integration 标签，并要求设置 `KUBEBUILDER_ASSETS`：

```bash
go test -tags=integration -run TestCRDIntegration -count=1 -timeout=120s ./pkg/store
```

该测试启动独立的 kube-apiserver/etcd，校验 schema/CEL、status 子资源和 finalizer。
envtest 不运行 namespace/GC controller；完整 namespace 删除仍需集群端到端验证。

## TLS Secret 与配置事务

`pkg/controllers/certificates_test.go` 覆盖 Secret 在下发前受保护、共享引用及 namespace
隔离、UID 重建、删除中的 Secret 禁止新引用、部分切换/清理和结果写入失败、controller
重启恢复、引用登记与回收并发，以及引用读取失败时保留 finalizer。

`pkg/agent/nginx/certificates_test.go` 覆盖缺失/不完整证书集、无效 PEM、私钥不匹配、
跨 namespace、零后端、nginx 校验/reload 失败回滚，以及进程中断后的恢复和损坏日志。
上述单元测试的 nginx 命令使用测试脚本；真实 nginx 链路由下述独立集成测试验证。

```bash
go test -race -count=1 -timeout=120s ./pkg/controllers ./pkg/store ./pkg/agent/... ./pkg/apis/v1alpha1
```

## Controller → HTTP → nginx 实际转发

```bash
go test -tags nginxintegration ./pkg/controllers -run TestNginxControllerHTTPDataPlane -count=1 -v
```

需要本机安装 nginx，启用 HTTP SSL 与 stream 模块。测试读取部署用基础配置，使用
独立临时目录和高位端口启动 nginx，结束时关闭进程；不使用系统 nginx 实例。
动态 stream 模块默认路径为 `/usr/lib/nginx/modules/ngx_stream_module.so`，可通过
`OPENLB_TEST_NGINX_STREAM_MODULE` 覆盖。显式启用标签但依赖缺失时测试失败。

测试使用实际 Reconcile、CRDStore、HTTP Syncer、agent 路由/端口锁、nginx 渲染和 reload，
验证 HTTP/HTTPS/TCP/UDP 实际流量、同号 TCP/UDP 独立转发、逐分片命名 targetPort、
TLS 证书轮换、协议和 network-type 注解删除后的回退、非法协议/配置失败保留旧流量、
失败后重试，以及删除 instances 注解后的端口与 CR 清理。

Kubernetes API 使用 fake client，事件触发后显式调用 Reconcile；不等同于真实集群的
informer/队列端到端验证。`watch_test.go` 验证生产 watch 谓词接收 generation 不变的
注解增删改、Service 类型切换和 Node 地址/就绪变化；事件到 Service 的映射在
`service_test.go` 中覆盖。

nginx reload 是异步信号，生产成功响应仍以命令退出状态为准；集成测试通过实际请求
等待 worker 切换并验证流量。agent 端口锁不检测其他进程占用，部署时仍需避开 agent
自身和其他进程的监听端口。
