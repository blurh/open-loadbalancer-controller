# 路线图与存在的问题

记录待实现功能和相关设计思路。

## TODO

- [ ] 使用准入控制进行注解合法性校验
- [x] agent's service lock: 按 agent 的传输协议/端口记录 Service 归属，跨 class 检查冲突并返回 HTTP 409；
   锁先持久化再操作转发器，失败保留旧、新端口，重启合并已有配置恢复；更新/cleanup 成功后才释放。
- [x] 使用 kubernetes.io/openlb.class 用来决定 agent 的模式, 而不使用 OPENLB_AGENT_TYPE（已支持按 Service 分派、class 切换清理及启动恢复归属；LVS 已支持 IPv4 TCP/UDP）
- [x] LVS 四层转发：IPVS 增量同步、NAT/DR/TUN、持久化归属、失败回滚及启动恢复；七层配置忽略并记录 Service 事件。
- [ ] agent 永久不可达时的清理放弃策略
   CR 已持久化逐实例清理进度，重试跳过已成功的实例；永久不可达仍保留 finalizer，可能阻塞 Service/namespace 删除。
- [x] controller 重启后恢复 ForceReload
   CR 的 PendingApply 和失败 Ready 条件驱动重试强制 reload，agent 也保留本地未完成操作。
- [x] setup 部分失败仍写 status
   已修复：本轮 setup 有错误时保留原有 status，不发送同步成功事件；首次同步失败时 ingress 保持为空。
   finalizer 与全部目标实例仍会记录用于重试、清理；全部 agent 同步成功且 finalizer/repo 写入成功后才发布完整 ingress。
- [x] 默认仓库的启动快照缓存陈旧
   CRDStore 使用 APIReader 读取最新记录；通过 CR List/Watch 触发启动恢复和删除处理。
   显式选择的 ConfigMap 后端仍保留旧缓存限制。
- [x] ConfigMap 单点写放大
   默认改为 Namespaced OpenLBServiceState，每 Service 一个 CR；实例列表规范化、稳定配置摘要和无变化写入检测。
   Repository 接口与 CRDStore 位于 pkg/store，CRD/RBAC 已加入部署清单。
- [x] 使用中的 TLS Secret 删除保护
   CR 持久化名称/UID 引用，Secret finalizer 在下发前写入；全部 agent 切换或清理成功后释放旧引用。
   共享引用、失败重试和重启恢复已覆盖；动态准入暂不实现，DELETE 会进入 Terminating。
- [x] 证书缺失或无效时保留完整配置
   controller 不再忽略缺失 Secret；agent 预先校验完整证书集合和私钥匹配，失败不清理旧配置。
   零后端返回未完成，保留整份旧配置和证书，防止提前释放引用。
- [ ] 文件名解析无校验
   List() 按 _ 分割文件名重建 ns/name,若手工放入不合规文件(如 a_b_c.conf),会被解析成 ns=a, name=b,污染 List 结果(虽然当前无启动对账逻辑,但未来若实现会误清理)。
- [x] Apply/Delete 失败回滚文件
   配置、证书和私钥作为一个 Service 的文件集合备份；写入、校验或 reload 失败恢复旧集合。
   本地恢复日志支持中断后的启动恢复，主机可在 nginx 启动前手动执行恢复命令。
- [ ] 大规模 Service, agent 优化
