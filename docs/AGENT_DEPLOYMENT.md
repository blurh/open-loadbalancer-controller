# Agent 主机部署

agent 以 Linux 二进制和一个 systemd 服务交付。controller 仍在 Kubernetes 中运行，
通过 Service 的 instances 注解访问主机上的 agent。agent 不需要 kubeconfig。
nginx 的安装、模块及启停由主机自行管理，安装包不提供 nginx 服务或进程管理。

## 构建

在仓库根目录使用 Go 1.25.5 或兼容版本、make、tar 和 sha256sum：

    make agent-package GOARCH=amd64
    make agent-package GOARCH=arm64

输出 dist/openlb-agent-linux-ARCH/ 和同名 tar.gz。安装包包含：

- bin/openlb-agent：Linux 静态二进制。
- systemd/openlb-agent.service：agent 的 systemd 服务。
- agent.env：环境配置示例。
- nginx/nginx.conf：安装时写入主机的基础配置。
- install.sh、SHA256SUMS 和本说明。

## 安装

将对应架构的压缩包复制到 LB 主机，解压并进入目录后执行：

    sudo sh ./install.sh
    sudoedit /etc/openlb/agent.env
    sudo systemctl enable --now openlb-agent.service
    sudo systemctl status openlb-agent.service
    sudo journalctl -u openlb-agent -n 100 --no-pager

安装脚本的行为：

1. 校验并安装 agent 二进制和 systemd unit，创建状态目录。
2. agent.env 不存在时使用模板；存在则保留，更新模板另存为 agent.env.example。
3. 按 agent.env 的 OPENLB_NGINX_BIN 检查 nginx 是否可执行。未安装时只提示，继续安装；
   不执行 nginx -V/-t、不安装软件包、不探测或自动加载模块。
4. 将基础配置写入 OPENLB_NGINX_HOME/nginx.conf，默认 /etc/nginx/nginx.conf。
   原文件存在时先复制为同目录 nginx.conf.bak，再覆盖。每次安装的 .bak 都保存覆盖前
   的版本；备份失败会终止安装，原 nginx.conf 不覆盖。
5. 保留 conf.d/http、conf.d/stream、tls 和 agent 状态目录中的已有文件。
   执行 systemctl daemon-reload，但不启动、停止、重启或 reload nginx/agent。

主机需自行准备实际使用的 nginx 或 IPVS 环境。检查缺失仅是提示，安装成功并不表示
该转发器已可用。nginx 是否加载新配置由主机操作决定；agent 启动仍会检查本机转发器。

## 环境配置与 Service 示例

交付包的默认环境配置：

    OPENLB_SERVE_PORT=:38080
    OPENLB_AGENT_METRICS_ADDRESS=:38082
    OPENLB_AGENT_STATE_DIR=/var/lib/openlb-agent
    OPENLB_NGINX_BIN=/usr/sbin/nginx
    OPENLB_NGINX_HOME=/etc/nginx

可将监听地址改为实际内网 IP，例如 192.168.10.10:38080。
controller 对未带端口的 instances 使用 OPENLB_AGENT_PORT（代码默认 8080），因此使用
本安装包默认端口时，Service 的 instances 必须明确填写 :38080，或同步修改 controller
的 OPENLB_AGENT_PORT。管理端口、metrics 端口与 nginx 本地状态端口 8888 不用于业务。

最小 HTTP Service 示例（替换主机 IP，后端 Pod 需具有 app=demo 标签并监听 8080）：

    apiVersion: v1
    kind: Service
    metadata:
      name: demo
      namespace: default
      annotations:
        kubernetes.io/openlb.instances: "192.168.10.10:38080"
        kubernetes.io/openlb.class: nginx
        kubernetes.io/openlb.protocol-port: "http:80"
    spec:
      type: LoadBalancer
      loadBalancerClass: kubernetes.io/openlb
      selector:
        app: demo
      ports:
        - name: http
          protocol: TCP
          port: 80
          targetPort: 8080

未写 network-type 时使用 NodePort；示例文件为 deploy/examples/example-agent-service.yaml。
HTTPS 示例为 deploy/examples/example-service.yaml，需预先创建同 namespace 的 TLS Secret，
并把 instances 改为实际主机地址及端口。

    kubectl describe service demo
    kubectl get olbstate demo -o yaml
    curl http://192.168.10.10/
    curl http://192.168.10.10:38082/metrics

## 升级、停止与状态

    sudo sh ./install.sh
    sudo systemctl restart openlb-agent.service

升级采用相同入口；二进制原子替换，保留 agent.env 与业务状态，但每次都会备份并覆盖
nginx.conf。安装后按需比较 nginx.conf.bak，主机自行决定何时加载新配置。
自定义 nginx 目录时在 agent.env 中设置绝对路径 OPENLB_NGINX_HOME；路径值可加单/双引号，
安装脚本不会把环境文件作为 shell 脚本执行。

agent 默认以 root 运行，以执行 nginx reload 和可选 IPVS 管理。收到 SIGTERM/SIGINT
后停止接受新请求，等待 Apply/Delete 完成，再关闭 metrics。OPENLB_SHUTDOWN_TIMEOUT
默认 30s，应小于 unit 的 TimeoutStopSec=45s。超时后依靠持久化事务和下次同步恢复。
停止 agent 不会停止 nginx，也不会清空 nginx/IPVS 转发。

长期保留 OPENLB_AGENT_STATE_DIR 与 OPENLB_NGINX_HOME 下生成的配置、TLS 和事务日志。
同一目录只允许一个 agent 写入。需要在 nginx 启动前手动恢复中断事务时，可在使用
agent 环境配置的进程中运行 openlb-agent --recover-nginx-config；安装脚本不自动执行。

卸载前从 Service instances 移除本机，等待 cleanup 成功，再停止/禁用 agent 服务。
不要删除状态目录或强制去掉 finalizer 来跳过清理。

## LVS

LVS 使用同一个安装入口，不需要单独安装模式。主机自行安装 ipvsadm、加载 IPVS 模块，
准备转发/回程路由，再配置 agent.env 并启动 openlb-agent.service。
默认 NAT、rr，VIP 自动发现本机内网 IPv4；需要时设置 OPENLB_LVS_VIPS。
nginx 缺失提示不会阻止安装或有可用 LVS 时的运行。更多网络要求见 docs/LVS.md。

## 验证

    python3 scripts/test-agent-install.py
    go test -race ./...
    go test -tags nginxintegration ./pkg/controllers -run TestNginxControllerHTTPDataPlane -count=1 -v

安装测试使用临时目录，验证缺失 nginx 的提示、已有配置备份与覆盖、重复安装、状态保留、
自定义路径以及备份失败。也可设置 DESTDIR=/tmp/openlb-root 进行离线安装；该模式检查
目标目录下的 nginx，不调用 systemctl。真实 LB 主机上的 agent systemd 启停、开机恢复
和 LVS 实际转发仍需目标环境验收。
