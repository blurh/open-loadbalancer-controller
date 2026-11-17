GOARCH ?= $(shell go env GOARCH)

.PHONY: agent agent-package test
agent: agent-package

# 输出 Linux 静态二进制、systemd unit、环境配置、安装脚本及 tar.gz。
agent-package:
	GOARCH=$(GOARCH) sh scripts/build-agent.sh

test:
	go test ./...
