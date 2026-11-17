package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/nginx"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/server"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/stats"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
)

func main() {
	recoverNginx := flag.Bool("recover-nginx-config", false, "recover and validate nginx configuration, then exit")
	flag.Parse()
	if *recoverNginx {
		forwarder, err := nginx.New()
		if err != nil {
			log.Fatal(err)
		}
		if err := forwarder.PreflightCheck(); err != nil {
			log.Fatal(err)
		}
		return
	}
	c := config.NewConfig()

	a, err := agent.NewAgent()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	interval := c.ConnectionStatsInterval()

	// 四层(TCP)连接数采集：转发器无关的通用能力，对所有 Forwarder 实现启用。
	// 各 agent 通过 L4Ports() 提供其监听端口，采集器从 /proc/net/tcp 过滤统计。
	if c.EnableL4Stats() {
		l4 := stats.NewL4Collector(a.L4Ports, interval)
		go l4.Start(ctx)
		log.Printf("L4 (TCP) stats collector started (interval: %s)", interval)
	}

	// UDP 套接字统计：转发器无关的通用能力，对所有 Forwarder 实现启用。
	// 各 agent 通过 UDPPorts() 提供其监听端口，采集器从 /proc/net/udp 过滤统计。
	if c.EnableL4Stats() {
		udp := stats.NewUDPCollector(a.UDPPorts, interval)
		go udp.Start(ctx)
		log.Printf("UDP stats collector started (interval: %s)", interval)
	}

	// 同一进程可承接不同 class 的 Service；按本机可用转发器启动专属采集器。
	if a.HasForwarder("nginx") {
		if stubStatusURL := c.NginxStatusURL(); stubStatusURL != "" && stubStatusURL != "0" {
			l7 := nginx.NewL7Collector(stubStatusURL, interval)
			go l7.Start(ctx)
			log.Printf("L7 (HTTP) stats collector started: %s (interval: %s)", stubStatusURL, interval)
		}
	}

	srv := server.New(a, c.ServePort())
	if err := srv.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
