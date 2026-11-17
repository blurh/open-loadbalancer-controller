# TLS Secret 引用保护

Service 通过 `kubernetes.io/openlb.cert-secrets` 引用同 namespace 的 TLS Secret。
controller 在第一次下发前，把 Secret 的名称、UID 写入对应
`OpenLBServiceState.status.certificateRefs`，再给 Secret 添加
`openlb.io/certificate-protection` finalizer。两步任一步失败都不会调用 agent。
CR 不存储证书和私钥。

`class: lvs` 忽略七层配置，不读取或保护证书 Secret，并记录 `L7ConfigurationIgnored`
事件。nginx 切换到 LVS 时，已有引用仍等全部 agent 成功后释放；详见 [LVS](LVS.md)。

## 删除与切换

- 删除受保护的 Secret 时，Kubernetes 接受 DELETE，Secret 进入 Terminating，
  但数据会保留。已有引用可以继续使用；新的 Service 不允许引用正在删除的 Secret。
- 从 Secret A 切换到 B 时，先保护 B，同时保留 A。所有目标 agent 确认新配置生效、
  且结果写入 CR 后，才释放 A。某个 agent 超时、失败或结果写入失败时，两个引用都保留。
- 多个 Service 共用一个 Secret 时，最后一个持久化引用释放后才移除保护 finalizer。
  引用按 namespace 和 UID 判断，同名 Secret 重建不会继承旧引用。
- Service 删除、退出管理或 CR 被删除时，先完成全部 agent 清理，再释放证书引用，
  最后移除 Service/CR 的清理 finalizer。namespace 删除采用同样顺序，不等待 Service
  对象先消失。agent 长期不可达仍会阻塞清理及 namespace 删除。

controller 用同一把锁串行处理引用登记和 Secret finalizer 回收，并通过 APIReader
读取最新引用，List 失败时不会释放。多副本部署必须启用 leader election。
独立的 Secret reconcile 监听 Secret 和 CR 引用变化；启动扫描会回收中断操作留下的
无主 finalizer，即使原 Service 已不存在。可选 file/configmap 后端没有 CR watch，
额外每分钟检查受保护 Secret；其原有存储限制仍然存在，默认使用 CRD 后端。

目前不启用动态准入：DELETE 不会被同步拒绝，也不会拦截用户修改 Secret 内容或强制
移除 finalizer。finalizer 解决正常删除流程中的引用保护；缺失或无效证书由同步流程拒绝，
保留已有配置及 Service ingress。只有 controller 应维护这些 finalizer 和 CR 引用。

## agent 的配置保护

controller 和 nginx agent 都检查证书集合是否完整、Secret 类型、证书与私钥是否匹配，
以及 DNS SAN 是否可用于 nginx 的 server_name 和证书文件名。HTTPS 必须配置证书引用；
缺少任意一个 Secret、空证书、缺失/不匹配私钥或多个 Secret 声明相同域名都会拒绝同步。
此校验不执行 CA 信任验证，也不是证书签发或续期功能。

nginx 的 Apply/Delete 在同一把锁内执行。修改前保存本 Service 的配置、证书和私钥，
文件通过临时文件原子替换；渲染、清理、`nginx -t` 或 reload 失败时整组回滚，其他
Service 不受影响。零后端保留整份旧配置并返回失败，让 controller 继续重试且保留旧引用。
成功判定仍依赖 `nginx -t` 和 `nginx -s reload` 的退出状态，不额外确认 worker 切换。

两条命令都显式使用 `-p OPENLB_NGINX_HOME -c OPENLB_NGINX_HOME/nginx.conf`，
主机上的 nginx 启动时应使用相同配置路径。基础配置的 include 使用相对路径；自定义目录时需提供
该目录下的主配置。`OPENLB_NGINX_COMMAND_TIMEOUT` 默认 `3s`，命令失败返回 nginx
诊断输出，超时触发错误和文件回滚。

恢复日志位于 `OPENLB_NGINX_HOME/.openlb-transaction.json`，包含上一份配置及私钥，
权限为 `0600`，成功提交或回滚后删除。它必须与 `conf.d/`、`tls/` 一起持久化，
同一配置目录只允许一个 agent 写入。agent 启动时先恢复未完成事务，再进行配置检查；
损坏的日志会阻止 nginx forwarder 启动，避免使用不完整配置。

安装包只管理 agent 的 systemd 服务；nginx 由主机自行管理。需要在 nginx 重启前恢复
中断事务时，可手动执行 `openlb-agent --recover-nginx-config`（先加载 agent 的环境配置）。
安装时默认将 `/etc/nginx/nginx.conf` 备份为 `nginx.conf.bak` 再覆盖。主机部署见
[Agent 主机部署](AGENT_DEPLOYMENT.md)。

## 部署

先更新 `deploy/install/crd.yaml` 和 `deploy/install/rbac.yaml`，再更新 controller 和 agent。
Secret RBAC 新增 `patch` 权限。旧 schema 会裁剪 `certificateRefs`，controller 检测到引用
没有持久化时拒绝下发。不要通过删除 CRD 或强制去除 finalizer 来代替正常清理。

```bash
kubectl apply -f deploy/install/crd.yaml
kubectl apply -f deploy/install/rbac.yaml
kubectl get olbstate web -n team-a -o yaml
kubectl get secret tls-cert -n team-a -o yaml
```

回归测试覆盖共享引用、namespace 隔离、UID 变化、部分切换和清理失败、持久化失败、
并发登记/回收、controller 重启恢复，以及 agent 的证书缺失、私钥不匹配、配置检查/reload
失败回滚和中断事务恢复。测试中的 nginx 命令用可注入失败的脚本替代。
