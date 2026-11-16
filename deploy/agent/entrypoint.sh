#!/bin/sh
set -e

# 在 nginx 读取配置前恢复中断的配置/证书事务。
openlb-agent --recover-nginx-config

# 先以 daemon 方式拉起 nginx master（常驻），供 agent 后续 `nginx -s reload`
nginx_home="${OPENLB_NGINX_HOME:-/etc/nginx}"
nginx_home=$(cd "$nginx_home" && pwd)
"${OPENLB_NGINX_BIN:-nginx}" -p "$nginx_home/" -c "$nginx_home/nginx.conf"

# 前台运行 agent：收到 controller 的同步请求后渲染配置并 reload nginx
exec openlb-agent
