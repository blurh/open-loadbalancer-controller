#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
arch=${GOARCH:-$(go env GOARCH)}
case "$arch" in
    amd64|arm64) ;;
    *) printf 'Unsupported GOARCH: %s (use amd64 or arm64)\n' "$arch" >&2; exit 1 ;;
esac
out_dir="$repo_dir/dist"
package_name="openlb-agent-linux-$arch"
mkdir -p "$out_dir"
stage=$(mktemp -d "$out_dir/.agent-package.XXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM
package_dir="$stage/$package_name"
mkdir -p "$package_dir/bin"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$package_dir/bin/openlb-agent" ./cmd/agent/
cp -R deploy/agent/. "$package_dir/"
cp docs/AGENT_DEPLOYMENT.md "$package_dir/README.md"
chmod 0755 "$package_dir/install.sh"
(cd "$package_dir" && sha256sum bin/openlb-agent > SHA256SUMS)
tar -C "$stage" -czf "$stage/$package_name.tar.gz" "$package_name"
# 替换仅限本构建生成的目录，不触碰主机安装目录。
rm -rf "$out_dir/$package_name"
mv "$package_dir" "$out_dir/$package_name"
mv "$stage/$package_name.tar.gz" "$out_dir/$package_name.tar.gz"
printf 'Package: %s\nBinary: %s\n' "$out_dir/$package_name.tar.gz" "$out_dir/$package_name/bin/openlb-agent"
