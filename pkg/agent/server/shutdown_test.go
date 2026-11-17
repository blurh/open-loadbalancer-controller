package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

type blockingForwarder struct {
	fakeAgent
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (f *blockingForwarder) Apply(sync.ControllerResources) (sync.RespBody, error) {
	close(f.entered)
	<-f.release
	defer close(f.finished)
	return sync.RespBody{}, nil
}

func TestShutdownDrainsApplyAndStopsMetrics(t *testing.T) {
	f := &blockingForwarder{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsAddr := metrics.Addr().String()
	metrics.Close()
	s := New(f, "")
	s.metricsAddr = metricsAddr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, l) }()
	response := make(chan error, 1)
	hc := &http.Client{Timeout: 3 * time.Second}
	go func() {
		resp, err := hc.Post("http://"+l.Addr().String()+sync.RouteSyncPath, "application/json", strings.NewReader(`{}`))
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = errors.New("unexpected response status")
			}
		}
		response <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		close(f.release)
		t.Fatal("Apply did not start")
	}
	resp, err := hc.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		close(f.release)
		t.Fatal(err)
	}
	resp.Body.Close()
	cancel()
	// 取消不应在配置事务完成前退出进程。
	select {
	case err := <-done:
		close(f.release)
		t.Fatalf("server stopped during Apply: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(f.release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	for _, address := range []string{l.Addr().String(), metricsAddr} {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("listener still open: %s", address)
		}
	}
}

func TestShutdownTimeoutIsBounded(t *testing.T) {
	t.Setenv("OPENLB_SHUTDOWN_TIMEOUT", "30ms")
	f := &blockingForwarder{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(f, "")
	s.metricsAddr = "0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, l) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		hc := &http.Client{Timeout: time.Second}
		resp, _ := hc.Post("http://"+l.Addr().String()+sync.RouteSyncPath, "application/json", strings.NewReader(`{}`))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("Apply did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			close(f.release)
			t.Fatalf("expected deadline, got %v", err)
		}
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("shutdown exceeded deadline")
	}
	close(f.release)
	<-f.finished
	<-requestDone
}
