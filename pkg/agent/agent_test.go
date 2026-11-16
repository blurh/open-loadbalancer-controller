package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	stdsync "sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeForwarder 模拟各转发器独立持久化的配置，以及操作中途失败的情况。
type fakeForwarder struct {
	class             string
	services          map[sync.ServiceRef]struct{}
	listeners         map[sync.ServiceRef][]servicelock.Port
	listenerErr       error
	lastForceReload   bool
	preserveListeners bool
	events            *[]string
	applyErr          error
	deleteErr         error
	listErr           error
	preflightErr      error
	tcp               []int
	udp               []int
	applyCalls        int
	deleteCalls       int
	preflights        int
}

func (f *fakeForwarder) PreflightCheck() error {
	f.preflights++
	return f.preflightErr
}

func (f *fakeForwarder) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
	f.applyCalls++
	f.lastForceReload = cr.ForceReload
	ref := sync.ServiceRef{Namespace: cr.Service.Namespace, Name: cr.Service.Name}
	f.services[ref] = struct{}{}
	if !f.preserveListeners {
		f.listeners[ref], _ = servicelock.RequestedPorts(cr)
	}
	// 模拟具体转发器写入自身 Service 数量；分派层应改为聚合值。
	metrics.SetServicesManaged(len(f.services))
	if f.events != nil {
		*f.events = append(*f.events, f.class+":apply")
	}
	return sync.RespBody{InternalIPs: []string{f.class}}, f.applyErr
}

func (f *fakeForwarder) Delete(ref sync.ServiceRef) error {
	f.deleteCalls++
	if f.events != nil {
		*f.events = append(*f.events, f.class+":delete")
	}
	// 和 nginx 一样，允许文件已删除但 reload 失败。
	delete(f.services, ref)
	delete(f.listeners, ref)
	metrics.SetServicesManaged(len(f.services))
	return f.deleteErr
}

func (f *fakeForwarder) List() ([]sync.ServiceRef, error) {
	var refs []sync.ServiceRef
	for ref := range f.services {
		refs = append(refs, ref)
	}
	return refs, f.listErr
}
func (f *fakeForwarder) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) {
	return f.listeners, f.listenerErr
}
func (f *fakeForwarder) L4Ports() []int  { return f.tcp }
func (f *fakeForwarder) UDPPorts() []int { return f.udp }

func newFakeForwarder(class string) *fakeForwarder {
	return &fakeForwarder{class: class, services: make(map[sync.ServiceRef]struct{}), listeners: make(map[sync.ServiceRef][]servicelock.Port)}
}

func routerForTest(t *testing.T, forwarders ...*fakeForwarder) *Agent {
	t.Helper()
	factories := make(map[string]forwarderFactory)
	for _, f := range forwarders {
		factories[f.class] = func() (Forwarder, error) { return f, nil }
	}
	a, err := newAgent(factories, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func resources(name, class string) sync.ControllerResources {
	return sync.ControllerResources{Service: corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: name, Annotations: map[string]string{apis.SvcAnnClass: class},
	}}}
}

func TestAgentRoutesEachServiceByClass(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	a := routerForTest(t, ng, lv)
	for _, cr := range []sync.ControllerResources{resources("web", "nginx"), resources("dns", "lvs"), resources("web", "nginx")} {
		resp, err := a.Apply(cr)
		if err != nil || !reflect.DeepEqual(resp.InternalIPs, []string{cr.Service.Annotations[apis.SvcAnnClass]}) {
			t.Fatalf("Apply(%s) = %+v, %v", cr.Service.Name, resp, err)
		}
	}
	if ng.applyCalls != 2 || lv.applyCalls != 1 || ng.preflights != 1 || lv.preflights != 1 {
		t.Fatalf("forwarder not reused: nginx=%+v lvs=%+v", ng, lv)
	}
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 1 || lv.deleteCalls != 0 {
		t.Fatal("cleanup must only call the owning forwarder")
	}
}

func TestAgentDefaultAndInvalidClass(t *testing.T) {
	ng := newFakeForwarder("nginx")
	a := routerForTest(t, ng)
	cr := resources("web", "nginx")
	cr.Service.Annotations = nil
	if _, err := a.Apply(cr); err != nil || ng.applyCalls != 1 {
		t.Fatalf("missing class should default to nginx: %v", err)
	}
	for _, class := range []string{"", " 	", "unknown", "NGINX"} {
		if _, err := a.Apply(resources("web", class)); !errors.Is(err, ErrInvalidClass) {
			t.Fatalf("class %q: want invalid class, got %v", class, err)
		}
	}
	if ng.applyCalls != 1 || ng.deleteCalls != 0 {
		t.Fatal("invalid class modified existing configuration")
	}
}

func TestAgentClassChangeCleansOldForwarderFirst(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	var events []string
	ng.events, lv.events = &events, &events
	a := routerForTest(t, ng, lv)
	for _, class := range []string{"nginx", "lvs"} {
		if _, err := a.Apply(resources("web", class)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"nginx:apply", "nginx:delete", "lvs:apply"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 1 || lv.deleteCalls != 1 {
		t.Fatal("cleanup used the old class")
	}
}

func TestAgentClassChangeRetriesFailedCleanup(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	a := routerForTest(t, ng, lv)
	if _, err := a.Apply(resources("web", "nginx")); err != nil {
		t.Fatal(err)
	}
	ng.deleteErr = errors.New("reload failed")
	if _, err := a.Apply(resources("web", "lvs")); !errors.Is(err, ng.deleteErr) {
		t.Fatalf("want cleanup error, got %v", err)
	}
	if lv.applyCalls != 0 {
		t.Fatal("new class applied before old class cleanup succeeded")
	}
	ng.deleteErr = nil
	if _, err := a.Apply(resources("web", "lvs")); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 2 || lv.applyCalls != 1 {
		t.Fatal("failed cleanup was not retried")
	}
}

func TestAgentUnavailableClassDoesNotCleanWorkingService(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	lv.preflightErr = errors.New("not implemented")
	a := routerForTest(t, ng, lv)
	if !a.HasForwarder("nginx") || a.HasForwarder("lvs") {
		t.Fatal("incorrect available forwarders")
	}
	if _, err := a.Apply(resources("web", "nginx")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(resources("web", "lvs")); !errors.Is(err, lv.preflightErr) {
		t.Fatalf("want unavailable class error, got %v", err)
	}
	if ng.deleteCalls != 0 || lv.applyCalls != 0 {
		t.Fatal("unavailable class changed working service")
	}
}

func TestAgentRestoresOwnershipAndCleansAllCopies(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	ref := sync.ServiceRef{Namespace: "default", Name: "web"}
	ng.services[ref], lv.services[ref] = struct{}{}, struct{}{}
	a := routerForTest(t, ng, lv) // 无 Apply，模拟 agent 重启后直接接收 cleanup。
	ng.deleteErr = errors.New("reload failed")
	if err := a.Delete(ref); !errors.Is(err, ng.deleteErr) {
		t.Fatalf("want delete error, got %v", err)
	}
	ng.deleteErr = nil
	if err := a.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 2 || lv.deleteCalls != 1 {
		t.Fatal("must retry only the failed class")
	}
	if err := a.Delete(ref); err != nil || len(ng.services) != 0 || len(lv.services) != 0 {
		t.Fatal("repeated cleanup must leave no configuration")
	}
}

func TestAgentCleanupAfterRestartWithoutConfigFiles(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	a := routerForTest(t, ng, lv)
	ref := sync.ServiceRef{Namespace: "default", Name: "web"}
	if err := a.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if ng.deleteCalls != 1 || lv.deleteCalls != 1 {
		t.Fatal("unknown ownership must still trigger cleanup/reload")
	}
}

func TestAgentUnavailableForwarderDoesNotBlockOtherClasses(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	ng.preflightErr = errors.New("nginx not installed")
	a := routerForTest(t, ng, lv)
	if _, err := a.Apply(resources("web", "lvs")); err != nil {
		t.Fatal(err)
	}
	if a.HasForwarder("nginx") || !a.HasForwarder("lvs") || lv.applyCalls != 1 {
		t.Fatal("unavailable nginx blocked another class")
	}
}

func TestAgentFailedApplyStillGetsCleaned(t *testing.T) {
	ng := newFakeForwarder("nginx")
	a := routerForTest(t, ng)
	ng.applyErr = errors.New("reload failed after writing config")
	if _, err := a.Apply(resources("web", "nginx")); !errors.Is(err, ng.applyErr) {
		t.Fatalf("want apply error, got %v", err)
	}
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil || ng.deleteCalls != 1 {
		t.Fatalf("partial config not cleaned: %v", err)
	}
}

func TestAgentAggregatesServicesAndPorts(t *testing.T) {
	ng, lv := newFakeForwarder("nginx"), newFakeForwarder("lvs")
	ng.tcp, lv.tcp = []int{443, 80}, []int{80, 8080}
	ng.udp, lv.udp = []int{53}, []int{5353, 53}
	a := routerForTest(t, ng, lv)
	for _, cr := range []sync.ControllerResources{resources("web", "nginx"), resources("dns", "lvs")} {
		if _, err := a.Apply(cr); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := a.List()
	want := []sync.ServiceRef{{Namespace: "default", Name: "dns"}, {Namespace: "default", Name: "web"}}
	if err != nil || !reflect.DeepEqual(refs, want) {
		t.Fatalf("List = %v, %v, want %v", refs, err, want)
	}
	if !reflect.DeepEqual(a.L4Ports(), []int{80, 443, 8080}) || !reflect.DeepEqual(a.UDPPorts(), []int{53, 5353}) {
		t.Fatal("ports were not aggregated and deduplicated")
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "openlb_agent_services_managed" {
			if got := family.Metric[0].GetGauge().GetValue(); got != 2 {
				t.Fatalf("managed services = %v, want 2", got)
			}
			return
		}
	}
	t.Fatal("managed service metric missing")
}

func TestAgentStartupFailures(t *testing.T) {
	_, err := newAgent(map[string]forwarderFactory{
		"nginx": func() (Forwarder, error) { return nil, errors.New("binary missing") },
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no usable forwarders") {
		t.Fatalf("want startup failure, got %v", err)
	}
	ng := newFakeForwarder("nginx")
	ng.listErr = errors.New("cannot read configuration")
	_, err = newAgent(map[string]forwarderFactory{
		"nginx": func() (Forwarder, error) { return ng, nil },
	}, t.TempDir())
	if !errors.Is(err, ng.listErr) {
		t.Fatalf("must not lose ownership on List failure: %v", err)
	}
}

func TestAgentConcurrentRequests(t *testing.T) {
	a := routerForTest(t, newFakeForwarder("nginx"), newFakeForwarder("lvs"))
	var wg stdsync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, class := range []string{"nginx", "lvs"} {
				if _, err := a.Apply(resources("web", class)); err != nil {
					t.Error(err)
				}
			}
			a.L4Ports()
			a.UDPPorts()
			if _, err := a.List(); err != nil {
				t.Error(err)
			}
			if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestNewAgentIgnoresLegacyEnvironmentAndRestoresNginx(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_TYPE", "lvs") // 旧环境变量不能改变 Service 的选择。
	t.Setenv("OPENLB_LVS_VIPS", "")
	t.Setenv("OPENLB_LVS_BIN", filepath.Join(home, "missing-ipvsadm"))
	streamDir := filepath.Join(home, "conf.d", "stream")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(streamDir, "default_web_tcp_9000.conf")
	if err := os.WriteFile(file, []byte("# restored config"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent()
	if err != nil || !a.HasForwarder("nginx") || a.HasForwarder("lvs") {
		t.Fatalf("unexpected startup result: %v, %v", a, err)
	}
	if _, err := a.Apply(resources("unavailable", "lvs")); err == nil || !strings.Contains(err.Error(), "find ipvsadm") {
		t.Fatalf("LVS must report missing ipvsadm, got %v", err)
	}
	if err := a.Delete(sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("restored config not removed: %v", err)
	}
	if _, err := a.Apply(resources("defaulted", "nginx")); err != nil {
		t.Fatal(err)
	}
}
