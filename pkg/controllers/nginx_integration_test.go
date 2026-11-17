//go:build nginxintegration

package controllers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blurh/open-loadbalancer-controller/internal/testutil"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/server"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// 使用真实 nginx、真实 HTTP sync/cleanup、真实 agent/端口锁；只有 Kubernetes API 使用 fake。
// 显式启用此标签时缺少 nginx/stream 会失败，不会悄悄 skip。
func TestNginxControllerHTTPDataPlane(t *testing.T) {
	home, fwd := startIntegrationNginx(t)
	api := httptest.NewServer(server.New(fwd, "").Handler())
	defer api.Close()
	backend := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Observed-Proto", r.Header.Get("X-Forwarded-Proto"))
			fmt.Fprint(w, body)
		}))
	}
	a, b := backend("pod-a"), backend("node-b")
	defer a.Close()
	defer b.Close()
	portOf := func(address string) int32 {
		_, port, err := net.SplitHostPort(strings.TrimPrefix(address, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			t.Fatal(err)
		}
		return int32(n)
	}
	front := freeNginxPort(t)
	svc := lbService("default", "web", "")
	svc.Annotations = map[string]string{}
	svc.Spec.Ports[0] = corev1.ServicePort{Name: "http", Port: front, Protocol: corev1.ProtocolTCP, NodePort: portOf(b.URL), TargetPort: intstr.FromString("web")}
	eps := endpointSlice("default", "web")
	eps.Endpoints[0].Addresses = []string{"127.0.0.1"}
	eps.Ports[0].Port = ptrInt32(portOf(a.URL))
	cert := testutil.TLSSecret(t, "default", "web-tls", "web.example.test")
	r, _, _ := newTestReconciler([]client.Object{svc, node("n1", "127.0.0.1"), eps, cert})
	r.Syncer = sync.NewHttpSyncer()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	reconcile := func() error { _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); return err }
	apply := func() {
		t.Helper()
		if err := reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	mutate := func(change func(*corev1.Service)) {
		t.Helper()
		current := &corev1.Service{}
		if err := r.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		old := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		change(current)
		if !serviceEventPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
			t.Fatal("annotation event filtered")
		}
		if err := r.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	setProtocol := func(protocol string) {
		mutate(func(s *corev1.Service) {
			if protocol == "" {
				delete(s.Annotations, apis.SvcAnnProtolPort)
			} else {
				s.Annotations[apis.SvcAnnProtolPort] = fmt.Sprintf("%s:%d", protocol, front)
			}
		})
	}
	request := func(scheme, body, forwarded string, secret *corev1.Secret) {
		t.Helper()
		transport := &http.Transport{DisableKeepAlives: true}
		if secret != nil {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(secret.Data[corev1.TLSCertKey]) {
				t.Fatal("invalid test certificate")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "web.example.test"}
		}
		defer transport.CloseIdleConnections()
		hc := &http.Client{Transport: transport, Timeout: time.Second}
		eventuallyNginx(t, func() error {
			resp, err := hc.Get(fmt.Sprintf("%s://127.0.0.1:%d/", scheme, front))
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if string(data) != body || resp.Header.Get("X-Observed-Proto") != forwarded {
				return fmt.Errorf("body=%q forwarded=%q", data, resp.Header.Get("X-Observed-Proto"))
			}
			return nil
		})
	}

	apply() // 尚未声明 instances，不创建转发配置。
	if refs, err := fwd.List(); err != nil || len(refs) != 0 {
		t.Fatalf("unmanaged: %v %v", refs, err)
	}
	mutate(func(s *corev1.Service) {
		s.Annotations[apis.SvcAnnInstances] = strings.TrimPrefix(api.URL, "http://")
		s.Annotations[apis.SvcAnnClass] = "nginx"
		s.Annotations[apis.SvcAnnNetworkType] = "pod"
		s.Annotations[apis.SvcAnnProtolPort] = fmt.Sprintf("http:%d", front)
	})
	apply()
	request("http", "pod-a", "http", nil)
	t.Log("annotation add → HTTP JSON → nginx HTTP proxy passed")

	// 同一 targetPort 名称，不同分片的实际端口不同；两组流量都必须能到达。
	second := eps.DeepCopy()
	second.Name = "web-second"
	second.ResourceVersion = ""
	second.Ports[0].Port = ptrInt32(portOf(b.URL))
	if err := r.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	apply()
	request("http", "node-b", "http", nil)
	request("http", "pod-a", "http", nil)
	if err := r.Delete(ctx, second); err != nil {
		t.Fatal(err)
	}
	apply()
	t.Log("per-EndpointSlice named targetPort forwarding passed")

	mutate(func(s *corev1.Service) { s.Annotations[apis.SvcAnnCertSecrets] = cert.Name })
	setProtocol("https")
	apply()
	request("https", "pod-a", "https", cert)
	rotated := testutil.TLSSecret(t, cert.Namespace, cert.Name, "web.example.test")
	currentSecret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: cert.Namespace, Name: cert.Name}
	if err := r.Get(ctx, secretKey, currentSecret); err != nil {
		t.Fatal(err)
	}
	currentSecret.Data = rotated.Data
	if err := r.Update(ctx, currentSecret); err != nil {
		t.Fatal(err)
	}
	apply()
	request("https", "pod-a", "https", rotated)
	t.Log("HTTPS and certificate rotation passed with certificate verification")

	// 非法协议和 nginx -t 失败都必须保留正在工作的 HTTPS 配置。
	setProtocol("htpp")
	if err := reconcile(); err == nil {
		t.Fatal("invalid protocol accepted")
	}
	request("https", "pod-a", "https", rotated)
	confPath := filepath.Join(home, "nginx.conf")
	base, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confPath, append(append([]byte(nil), base...), []byte("\ninvalid_openlb_test_directive;\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	mutate(func(s *corev1.Service) { delete(s.Annotations, apis.SvcAnnCertSecrets) })
	setProtocol("http")
	if err := reconcile(); err == nil || !strings.Contains(err.Error(), "invalid_openlb_test_directive") {
		t.Fatalf("expected nginx diagnostic, got %v", err)
	}
	request("https", "pod-a", "https", rotated)
	state := &statev1.OpenLBServiceState{}
	if err := r.Get(ctx, key, state); err != nil {
		t.Fatal(err)
	}
	if len(state.Status.CertificateRefs) == 0 {
		t.Fatal("failed reload released live certificate")
	}
	if err := os.WriteFile(confPath, base, 0600); err != nil {
		t.Fatal(err)
	}
	apply()
	request("http", "pod-a", "http", nil)
	t.Log("invalid input/reload failure preserves old traffic; retry loads desired config")

	setProtocol("")
	apply()
	request("http", "pod-a", "", nil) // TCP 透传不会注入 HTTP 头。
	mutate(func(s *corev1.Service) {
		delete(s.Annotations, apis.SvcAnnNetworkType)
		delete(s.Annotations, apis.SvcAnnClass)
	})
	apply()
	request("http", "node-b", "", nil)
	t.Log("annotation removal → TCP/default NodePort/default nginx passed")

	// 同一数字的 TCP 和 UDP 使用不同 NodePort，验证协议和端口映射不会串线。
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, peer, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(buf[:n], peer)
		}
	}()
	mutate(func(s *corev1.Service) {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "udp", Port: front, Protocol: corev1.ProtocolUDP, NodePort: portOf(udp.LocalAddr().String())})
	})
	apply()
	eventuallyNginx(t, func() error {
		conn, err := net.DialTimeout("udp4", fmt.Sprintf("127.0.0.1:%d", front), time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte("udp-ok")); err != nil {
			return err
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if string(buf[:n]) != "udp-ok" {
			return fmt.Errorf("UDP response %q", buf[:n])
		}
		return nil
	})
	request("http", "node-b", "", nil)
	t.Log("simultaneous TCP/UDP forwarding on same number passed")

	mutate(func(s *corev1.Service) { delete(s.Annotations, apis.SvcAnnInstances) })
	apply()
	if refs, err := fwd.List(); err != nil || len(refs) != 0 {
		t.Fatalf("cleanup: %v %v", refs, err)
	}
	if err := r.Get(ctx, key, state); !apierrors.IsNotFound(err) {
		t.Fatalf("state not removed: %v", err)
	}
	eventuallyNginx(t, func() error {
		conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", front), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Errorf("listener still open")
		}
		return nil
	})
	t.Log("instances annotation removal → HTTP cleanup → listener/state removal passed")
}

func startIntegrationNginx(t *testing.T) (string, *agent.Agent) {
	t.Helper()
	bin, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal("nginxintegration requires a real nginx binary: ", err)
	}
	home := t.TempDir()
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("OPENLB_LVS_BIN", filepath.Join(home, "no-ipvsadm"))
	t.Setenv("OPENLB_SYNC_MAX_RETRIES", "0")
	base, err := os.ReadFile("../../deploy/agent/nginx/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	conf := strings.NewReplacer(
		"worker_processes auto;", "worker_processes 1;",
		"worker_shutdown_timeout 30s;", "worker_shutdown_timeout 1s;",
		"error_log stderr notice;", "error_log "+home+"/error.log notice;",
		"pid /run/nginx.pid;", "pid "+home+"/nginx.pid;",
		"access_log /dev/stdout;", "access_log off;",
		"default_type application/octet-stream;", "default_type application/octet-stream;\nclient_body_temp_path "+home+"/body;\nproxy_temp_path "+home+"/proxy;\nfastcgi_temp_path "+home+"/fastcgi;\nuwsgi_temp_path "+home+"/uwsgi;\nscgi_temp_path "+home+"/scgi;",
		"127.0.0.1:8888", fmt.Sprintf("127.0.0.1:%d", freeNginxPort(t)),
	).Replace(string(base))

	version, err := exec.Command(bin, "-V").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(version), "--with-stream=dynamic") {
		module := os.Getenv("OPENLB_TEST_NGINX_STREAM_MODULE")
		if module == "" {
			module = "/usr/lib/nginx/modules/ngx_stream_module.so"
		}
		conf = "load_module " + module + ";\n" + conf
	}
	if err := os.WriteFile(filepath.Join(home, "nginx.conf"), []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	fwd, err := agent.NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	if err := fwd.PreflightCheck(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-p", home+"/", "-c", filepath.Join(home, "nginx.conf"), "-g", "daemon off;")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			log, _ := os.ReadFile(filepath.Join(home, "error.log"))
			t.Logf("nginx log:\n%s", log)
		}
	})
	eventuallyNginx(t, func() error { _, err := os.Stat(filepath.Join(home, "nginx.pid")); return err })
	return home, fwd
}

func freeNginxPort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return int32(l.Addr().(*net.TCPAddr).Port)
}

func eventuallyNginx(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
}
