package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func listeningService(name, class, protocol string, ports ...int32) sync.ControllerResources {
	cr := resources(name, class)
	for _, port := range ports {
		transport := corev1.ProtocolTCP
		if protocol == "udp" {
			transport = corev1.ProtocolUDP
		}
		cr.Service.Spec.Ports = append(cr.Service.Spec.Ports, corev1.ServicePort{Port: port, Protocol: transport, AppProtocol: &protocol})
	}
	return cr
}

func restartRouter(t *testing.T, dir string, forwarders ...*fakeForwarder) *Agent {
	t.Helper()
	factories := make(map[string]forwarderFactory)
	for _, f := range forwarders {
		factories[f.class] = func() (Forwarder, error) { return f, nil }
	}
	a, err := newAgent(factories, dir)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func applyOK(t *testing.T, a *Agent, cr sync.ControllerResources) {
	t.Helper()
	if _, err := a.Apply(cr); err != nil {
		t.Fatal(err)
	}
}

func expectConflict(t *testing.T, a *Agent, cr sync.ControllerResources, owner string) {
	t.Helper()
	_, err := a.Apply(cr)
	if !errors.Is(err, servicelock.ErrConflict) || !strings.Contains(err.Error(), "held by "+owner) {
		t.Fatalf("want conflict held by %s, got %v", owner, err)
	}
}

func TestServiceLocksTransportNamespaceAndClass(t *testing.T) {
	for _, first := range []string{"http", "https", "tcp", "udp"} {
		for _, second := range []string{"http", "https", "tcp", "udp"} {
			t.Run(first+"/"+second, func(t *testing.T) {
				ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
				a := routerForTest(t, ng, lv)
				applyOK(t, a, listeningService("web", "nginx", first, 80))
				other := listeningService("web", "lvs", second, 80)
				other.Service.Namespace = "other"
				if (first == "udp") == (second == "udp") {
					expectConflict(t, a, other, "default/web")
					if lv.applyCalls != 0 {
						t.Fatal("conflicting request reached forwarder")
					}
				} else {
					applyOK(t, a, other)
				}
			})
		}
	}
}

func TestLVSIgnoredApplicationProtocolDoesNotReserveWrongTransport(t *testing.T) {
	a := routerForTest(t, newFakeForwarder("nginx"), newFakeForwarder("lvs"))
	applyOK(t, a, listeningService("tcp-owner", "nginx", "tcp", 443))
	cr := listeningService("udp-owner", "lvs", "https", 443)
	cr.Service.Spec.Ports[0].Protocol = corev1.ProtocolUDP
	cr.Service.Annotations[apis.SvcAnnProtolPort] = "https:443"
	cr.Service.Annotations[apis.SvcAnnCertSecrets] = "missing"
	applyOK(t, a, cr)
	expectConflict(t, a, listeningService("another", "nginx", "udp", 443), "default/udp-owner")
}

func TestServiceLockConflictDoesNotPartiallyReserveOrCleanClass(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	a := routerForTest(t, ng, lv)
	applyOK(t, a, listeningService("owner", "nginx", "http", 80))
	old := listeningService("moving", "nginx", "tcp", 9000)
	applyOK(t, a, old)
	before := a.locks.Reservations()
	expectConflict(t, a, listeningService("moving", "lvs", "tcp", 8000, 80), "default/owner")
	if ng.deleteCalls != 0 || lv.applyCalls != 0 || !reflect.DeepEqual(before, a.locks.Reservations()) {
		t.Fatal("conflict changed configuration or reservations")
	}
	applyOK(t, a, listeningService("other", "lvs", "tcp", 8000))
	applyOK(t, a, old)
	if ng.lastForceReload {
		t.Fatal("successful idempotent apply should not force reload")
	}
	applyOK(t, a, listeningService("moving", "lvs", "tcp", 9000))
	expectConflict(t, a, listeningService("competitor", "nginx", "tcp", 9000), "default/moving")
}

func TestServiceLockFailedUpdateSurvivesRestartAndReleasesAfterRetry(t *testing.T) {
	dir := t.TempDir()
	ng := newFakeForwarder("nginx")
	a := restartRouter(t, dir, ng)
	applyOK(t, a, listeningService("owner", "nginx", "tcp", 80))
	ng.applyErr = errors.New("reload failed")
	updated := listeningService("owner", "nginx", "tcp", 81)
	if _, err := a.Apply(updated); !errors.Is(err, ng.applyErr) {
		t.Fatalf("Apply error = %v", err)
	}
	// 重启后只看得到新配置，旧的运行中端口仍由持久化锁保护。
	a = restartRouter(t, dir, ng)
	for _, port := range []int32{80, 81} {
		expectConflict(t, a, listeningService("other", "nginx", "tcp", port), "default/owner")
	}
	ng.applyErr = nil
	applyOK(t, a, updated)
	if !ng.lastForceReload {
		t.Fatal("pending operation did not force reload")
	}
	applyOK(t, a, listeningService("other", "nginx", "tcp", 80))
	expectConflict(t, a, listeningService("third", "nginx", "tcp", 81), "default/owner")
}

func TestServiceLockFailedDeleteWithoutFilesSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ng := newFakeForwarder("nginx")
	a := restartRouter(t, dir, ng)
	applyOK(t, a, listeningService("owner", "nginx", "tcp", 80))
	ref := sync.ServiceRef{Namespace: "default", Name: "owner"}
	ng.deleteErr = errors.New("reload failed")
	if err := a.Delete(ref); !errors.Is(err, ng.deleteErr) {
		t.Fatalf("Delete error = %v", err)
	}
	ng = newFakeForwarder("nginx") // 模拟文件已删除，无法从 List 恢复。
	a = restartRouter(t, dir, ng)
	expectConflict(t, a, listeningService("other", "nginx", "tcp", 80), "default/owner")
	if err := a.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 1 {
		t.Fatal("cleanup did not retry the owning forwarder")
	}
	applyOK(t, a, listeningService("other", "nginx", "tcp", 80))
	// 延迟到达的旧 Service cleanup 不能释放其他 Service 的锁。
	if err := a.Delete(ref); err != nil {
		t.Fatal(err)
	}
	expectConflict(t, a, listeningService("third", "nginx", "tcp", 80), "default/other")
}

func TestServiceLockRetainsUnavailableForwarderOwnership(t *testing.T) {
	dir := t.TempDir()
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	a := restartRouter(t, dir, ng, lv)
	applyOK(t, a, listeningService("owner", "nginx", "tcp", 80))
	ng.preflightErr = errors.New("unavailable")
	a = restartRouter(t, dir, ng, lv)
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "owner"}); err == nil {
		t.Fatal("cleanup must not succeed while owning forwarder is unavailable")
	}
	expectConflict(t, a, listeningService("other", "lvs", "tcp", 80), "default/owner")
}

func TestServiceLockRetainsPreservedAndDesiredPorts(t *testing.T) {
	ng := newFakeForwarder("nginx")
	a := routerForTest(t, ng)
	applyOK(t, a, listeningService("owner", "nginx", "tcp", 80))
	ng.preserveListeners = true
	applyOK(t, a, listeningService("owner", "nginx", "tcp", 81))
	for _, port := range []int32{80, 81} {
		expectConflict(t, a, listeningService("other", "nginx", "tcp", port), "default/owner")
	}
}

func TestServiceLockConcurrentCompetitors(t *testing.T) {
	ng := newFakeForwarder("nginx")
	a := routerForTest(t, ng)
	var wg stdsync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Apply(listeningService(fmt.Sprintf("service-%d", i), "nginx", "tcp", 80))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, servicelock.ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 || ng.applyCalls != 1 {
		t.Fatalf("successes=%d apply calls=%d", successes, ng.applyCalls)
	}
}

func TestServiceLockPersistenceFailureDoesNotTouchForwarder(t *testing.T) {
	dir := t.TempDir()
	ng := newFakeForwarder("nginx")
	a := restartRouter(t, dir, ng)
	// 把状态目录移走，下一次保存必须失败，且不能留下仅存在于内存的锁。
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(dir+"-moved", dir) })
	if _, err := a.Apply(listeningService("owner", "nginx", "tcp", 80)); err == nil {
		t.Fatal("expected persistence error")
	}
	if ng.applyCalls != 0 || len(a.locks.Reservations()) != 0 {
		t.Fatal("failed persistence modified forwarder or locks")
	}
}

func TestServiceLockRecoversLegacyNginxFiles(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	dir := filepath.Join(home, "conf.d", "http")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "default_owner_https_443_upstream.conf")
	if err := os.WriteFile(file, []byte("# legacy partial config"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	expectConflict(t, a, listeningService("other", "nginx", "tcp", 443), "default/owner")
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "owner"}); err != nil {
		t.Fatal(err)
	}
	cr := listeningService("other", "nginx", "tcp", 443)
	cr.Service.Annotations[apis.SvcAnnNetworkType] = "nodeport"
	// 无后端不报告同步成功，但仍保留 pending 端口，重启后不能被其他 Service 抢占。
	if _, err := a.Apply(cr); err == nil {
		t.Fatal("zero-backend apply must remain pending")
	}
	a, err = NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	expectConflict(t, a, listeningService("third", "nginx", "http", 443), "default/other")
}

func TestServiceLockWithNginxReloadFailureAndRestart(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	failed := filepath.Join(home, "fail-reload")
	logFile := filepath.Join(home, "reloads")
	script := "#!/bin/sh\nif [ \"$1\" = \"-s\" ]; then\n  printf 'reload\\n' >> \"$OPENLB_TEST_RELOAD_LOG\"\n  if [ -f \"$OPENLB_TEST_RELOAD_FAIL\" ]; then exit 1; fi\nfi\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("OPENLB_TEST_RELOAD_LOG", logFile)
	t.Setenv("OPENLB_TEST_RELOAD_FAIL", failed)
	start := func() *Agent {
		a, err := NewAgent()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := start()
	cr := listeningService("owner", "nginx", "tcp", 80)
	cr.Service.Annotations[apis.SvcAnnNetworkType] = "pod"
	cr.Service.Spec.Ports[0].TargetPort = intstr.FromInt(8080)
	ready := true
	cr.EndpointSlice.Endpoints = []discovery.Endpoint{{Addresses: []string{"10.0.0.2"}, Conditions: discovery.EndpointConditions{Ready: &ready}}}
	applyOK(t, a, cr)
	file := filepath.Join(home, "conf.d", "stream", "default_owner_tcp_80.conf")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	expectConflict(t, a, listeningService("other", "nginx", "http", 80), "default/owner")
	after, err := os.ReadFile(file)
	if err != nil || string(before) != string(after) {
		t.Fatal("conflict modified existing nginx config")
	}
	if err := os.WriteFile(failed, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cr.Service.Spec.Ports[0].Port = 81
	if _, err := a.Apply(cr); err == nil {
		t.Fatal("expected reload failure")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != string(before) {
		t.Fatal("failed reload must restore the old file")
	}
	a = start()
	for _, port := range []int32{80, 81} {
		expectConflict(t, a, listeningService("other", "nginx", "tcp", port), "default/owner")
	}
	before, err = os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(failed); err != nil {
		t.Fatal(err)
	}
	applyOK(t, a, cr) // 回滚后重试，并在 reload 成功后释放旧端口。
	after, err = os.ReadFile(logFile)
	if err != nil || string(after) != string(before)+"reload\n" {
		t.Fatal("retry did not reload unchanged nginx configuration")
	}
	other := cr
	other.Service = *cr.Service.DeepCopy()
	other.Service.Name = "other"
	other.Service.Spec.Ports[0].Port = 80
	applyOK(t, a, other)
	if err := os.WriteFile(failed, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ref := sync.ServiceRef{Namespace: "default", Name: "owner"}
	if err := a.Delete(ref); err == nil {
		t.Fatal("expected cleanup reload failure")
	}
	a = start()
	expectConflict(t, a, listeningService("third", "nginx", "tcp", 81), "default/owner")
	if err := os.Remove(failed); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ref); err != nil {
		t.Fatal(err)
	}
	third := cr
	third.Service = *cr.Service.DeepCopy()
	third.Service.Name = "third"
	applyOK(t, a, third)
}

func TestServiceLockFailsStartupOnConflictingExistingConfiguration(t *testing.T) {
	ng := newFakeForwarder("nginx")
	for _, name := range []string{"first", "second"} {
		ref := sync.ServiceRef{Namespace: "default", Name: name}
		ng.services[ref] = struct{}{}
		ng.listeners[ref] = []servicelock.Port{{Protocol: "tcp", Number: 80}}
	}
	_, err := newAgent(map[string]forwarderFactory{"nginx": func() (Forwarder, error) { return ng, nil }}, t.TempDir())
	if !errors.Is(err, servicelock.ErrConflict) {
		t.Fatalf("want startup conflict, got %v", err)
	}
}
