package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

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

// Run 由调用方的退出信号驱动停止监听，并等待已接收的配置事务完成。
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	var metricsSrv *http.Server
	if s.metricsAddr != "" && s.metricsAddr != "0" {
		metricsListener, err := net.Listen("tcp", s.metricsAddr)
		if err != nil {
			log.Printf("metrics server error: %v", err)
		} else {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			metricsSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
			go func() {
				if err := metricsSrv.Serve(metricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("metrics server error: %v", err)
				}
			}()
		}
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	var runErr error
	select {
	case <-ctx.Done():
		log.Print("stopping HTTP listeners and draining in-flight requests")
	case runErr = <-done:
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	}
	// 不把取消的信号 context 传给 Shutdown；正在执行的 Apply/Delete 需要完成或留下恢复日志。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.NewConfig().ShutdownTimeout())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		runErr = errors.Join(runErr, fmt.Errorf("drain agent HTTP server: %w", err))
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			_ = metricsSrv.Close()
			runErr = errors.Join(runErr, fmt.Errorf("drain metrics server: %w", err))
		}
	}
	return runErr
}

// Handler 供主服务和集成测试共享同一套路由及 HTTP 方法约束。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+sync.RouteSyncPath, s.handleSync)
	mux.HandleFunc("POST "+sync.RouteCleanupPath, s.handleCleanup)
	return mux
}
