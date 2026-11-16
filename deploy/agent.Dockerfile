# agent 端镜像：nginx + ipvsadm + openlb-agent。按 Service class 使用 nginx 或 Linux IPVS。
#
# 构建上下文需为仓库根目录：
#   docker build -f deploy/agent.Dockerfile -t ghcr.io/blurh/openlb-agent:latest .
FROM golang:1.25.5 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/openlb-agent ./cmd/agent/

FROM nginx:stable
RUN apt-get update \
    && apt-get install -y --no-install-recommends ipvsadm \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/openlb-agent /usr/local/bin/openlb-agent
COPY deploy/agent/nginx.conf /etc/nginx/nginx.conf
COPY deploy/agent/entrypoint.sh /entrypoint.sh
RUN rm -f /etc/nginx/conf.d/default.conf \
    && chmod +x /entrypoint.sh \
    && mkdir -p /etc/nginx/conf.d/http /etc/nginx/conf.d/stream /etc/nginx/tls /var/lib/openlb-agent

ENV OPENLB_NGINX_HOME=/etc/nginx/
ENV OPENLB_SERVE_PORT=:8080
ENV OPENLB_AGENT_STATE_DIR=/var/lib/openlb-agent

# 8080 = agent 同步接口；80/443 = nginx 转发监听（按实际 listener 端口调整/扩展）
EXPOSE 8080 80 443

ENTRYPOINT ["/entrypoint.sh"]
