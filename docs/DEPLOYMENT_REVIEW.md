# 部署优化进度

2026-11-17。agent 采用主机二进制与 systemd；以下列表沿用 deploy 评审的范围。

## 本轮已完成

- [x] agent 构建输出 Linux amd64/arm64 二进制和 systemd 安装包，移除 agent 镜像文件与启动入口。
- [x] 补 agent 主机部署、持久化和升级示例；nginx 未安装只提示，已有 nginx.conf 备份后覆盖。
- [x] agent 响应 SIGTERM/SIGINT 时排空 HTTP 请求、关闭 metrics。
- [x] 移除专用 nginx unit、依赖、用户创建和模块探测；nginx 由主机自行管理。
- [x] 修正 CRD schema、CRD 集成和 nginx 集成测试中的旧部署路径。
- [x] nginx Service 示例补 loadBalancerClass；修正文档中 pod 模式自动禁用 NodePort 的错误说明。

## 未完成

- [ ] P1：目标 LB 主机验证 agent 的 systemd 启停、开机恢复及滚动升级。
  本地可校验 unit 和配置、运行独立 nginx；未在实际 LB 上执行 agent 服务启停。
- [ ] P2：controller 增加 healthz/readyz 实现与 startup/readiness/liveness 探针。
- [ ] P2：controller 增加跨节点调度策略与 PDB，明确滚动更新策略。
- [ ] P2：controller 发布镜像固定版本或 digest，通过 Kustomize images 替换；当前仍是 latest。
- [ ] P2：controller 构建补 .dockerignore、构建缓存和可复现的基础镜像版本。
- [ ] P2：真实集群验证 informer/队列、CRD/RBAC、leader 切换和卸载清理；LVS 验证实际网络回程。

agent 镜像版本、容器网络与挂载、容器 PID 1 等优化项已不适用，不继续列为待办；nginx 的安装、模块和服务生命周期按用户要求交由主机管理。
nginx reload 仍以命令退出状态判定成功，未增加 worker 切换确认；这属于数据面可靠性后续工作。
