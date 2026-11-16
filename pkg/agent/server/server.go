package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server 是 agent 侧的 HTTP 服务端，接收 controller 推来的资源并交给 Forwarder 处理。
type Server struct {
	fwd         agent.Forwarder
	addr        string
	metricsAddr string
}

func New(f agent.Forwarder, addr string) *Server {
	cfg := config.NewConfig()
	return &Server{
		fwd:         f,
		addr:        addr,
		metricsAddr: cfg.AgentMetricsAddr(),
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var cr sync.ControllerResources
	if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.fwd.Apply(cr)
	if err != nil {
		if errors.Is(err, servicelock.ErrConflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, agent.ErrInvalidClass) || errors.Is(err, servicelock.ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var ref sync.ServiceRef
	if err := json.NewDecoder(r.Body).Decode(&ref); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.fwd.Delete(ref); err != nil {
		if errors.Is(err, servicelock.ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) Run() error {
	// metrics server：在独立端口暴露 /metrics（设置 "0" 可禁用）
	// 与业务端口分离，避免 metrics 抓取与 sync 请求相互影响
	if s.metricsAddr != "" && s.metricsAddr != "0" {
		go func() {
			metricsMux := http.NewServeMux()
			metricsMux.Handle("/metrics", promhttp.Handler())
			metricsSrv := &http.Server{
				Addr:    s.metricsAddr,
				Handler: metricsMux,
			}
			// metrics server 挂了不影响主服务，仅打印日志
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics server error: %v", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:    s.addr,
		Handler: s.Handler(),
	}
	return srv.ListenAndServe()
}

// Handler 供主服务和集成测试共享同一套路由及 HTTP 方法约束。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+sync.RouteSyncPath, s.handleSync)
	mux.HandleFunc("POST "+sync.RouteCleanupPath, s.handleCleanup)
	return mux
}
