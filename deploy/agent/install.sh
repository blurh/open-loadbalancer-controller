#!/bin/sh
# 仅安装 agent 和配置文件；nginx 由主机自行安装和管理。
set -eu
package_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
dest=${DESTDIR:-}
if [ "$#" -ne 0 ]; then
    printf 'Usage: sudo %s\n' "$0" >&2
    exit 2
fi
if [ -n "$dest" ]; then
    case "$dest" in /*) ;; *) printf 'DESTDIR must be absolute\n' >&2; exit 1 ;; esac
else
    [ "$(id -u)" = 0 ] || { printf 'Run installation as root\n' >&2; exit 1; }
fi
binary="$package_dir/bin/openlb-agent"
[ -f "$binary" ] || { printf 'Build the package with make agent-package first\n' >&2; exit 1; }
(cd "$package_dir" && sha256sum -c SHA256SUMS)

install -d -m 0755 "$dest/usr/local/bin" "$dest/etc/systemd/system" "$dest/etc/openlb"
install -d -m 0700 "$dest/var/lib/openlb-agent"
# rename 原子替换二进制，避免截断正在运行的版本。
binary_tmp=$(mktemp "$dest/usr/local/bin/.openlb-agent.XXXXXX")
trap 'rm -f "$binary_tmp"' EXIT
trap 'exit 1' HUP INT TERM
install -m 0755 "$binary" "$binary_tmp"
mv -f "$binary_tmp" "$dest/usr/local/bin/openlb-agent"
install -m 0644 "$package_dir/systemd/openlb-agent.service" "$dest/etc/systemd/system/openlb-agent.service"
install -m 0600 "$package_dir/agent.env" "$dest/etc/openlb/agent.env.example"
if [ ! -e "$dest/etc/openlb/agent.env" ]; then
    install -m 0600 "$package_dir/agent.env" "$dest/etc/openlb/agent.env"
fi

# 只读取路径，不 source 环境文件，避免把配置作为 shell 代码执行。
read_setting() {
    value=""
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in "$1="*) value=${line#*=} ;; esac
    done < "$dest/etc/openlb/agent.env"
    case "$value" in
        \"*\") value=${value#\"}; value=${value%\"} ;;
        \'*\') value=${value#\'}; value=${value%\'} ;;
    esac
    printf '%s' "$value"
}
nginx_home=$(read_setting OPENLB_NGINX_HOME)
nginx_bin=$(read_setting OPENLB_NGINX_BIN)
nginx_home=${nginx_home:-/etc/nginx}
nginx_bin=${nginx_bin:-/usr/sbin/nginx}
case "$nginx_home" in /*) ;; *) printf 'OPENLB_NGINX_HOME must be absolute\n' >&2; exit 1 ;; esac

# 只检查可执行文件，不探测模块、不安装软件包、不调用 nginx 命令。
nginx_present=false
if [ -z "$dest" ]; then
    if command -v "$nginx_bin" >/dev/null 2>&1; then nginx_present=true; fi
elif [ -x "$dest$nginx_bin" ]; then
    nginx_present=true
fi
if [ "$nginx_present" = false ]; then
    printf 'Warning: nginx not installed or not executable at %s; continuing agent installation.\n' "$nginx_bin" >&2
fi

install -d -m 0755 "$dest$nginx_home" "$dest$nginx_home/conf.d/http" "$dest$nginx_home/conf.d/stream"
install -d -m 0700 "$dest$nginx_home/tls"
nginx_conf="$dest$nginx_home/nginx.conf"
# 每次覆盖前备份上一份配置；备份失败时 set -e 阻止覆盖。
if [ -e "$nginx_conf" ] || [ -L "$nginx_conf" ]; then
    cp -pT --remove-destination -- "$nginx_conf" "$nginx_conf.bak"
    printf 'Backed up %s to %s.bak\n' "$nginx_conf" "$nginx_conf"
fi
install -m 0644 "$package_dir/nginx/nginx.conf" "$nginx_conf"
if [ -z "$dest" ]; then systemctl daemon-reload; fi
printf 'Agent installed. nginx.conf replaced; agent.env and forwarding state preserved.\n'
printf 'Review /etc/openlb/agent.env, then enable/start openlb-agent.service.\n'
