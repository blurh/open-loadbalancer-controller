package lvs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	stdsync "sync"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type fakeDriver struct {
	rules      map[listener]virtualService
	operations []string
	failAction string
	failOnce   bool
	before     func()
	listErr    error
}

func (d *fakeDriver) List() (map[listener]virtualService, error) {
	result := make(map[listener]virtualService)
	for key, rule := range d.rules {
		rule.Destinations = slices.Clone(rule.Destinations)
		result[key] = rule
	}
	return result, d.listErr
}
func (d *fakeDriver) call(action string) error {
	if d.before != nil {
		d.before()
	}
	d.operations = append(d.operations, action)
	if action == d.failAction {
		if d.failOnce {
			d.failAction = ""
		}
		return errors.New("injected IPVS failure")
	}
	return nil
}
func (d *fakeDriver) Service(action string, rule virtualService) error {
	if err := d.call(action); err != nil {
		return err
	}
	old, exists := d.rules[rule.listener]
	switch action {
	case "-A":
		if exists {
			return errors.New("service exists")
		}
		rule.Destinations = nil
		d.rules[rule.listener] = rule
	case "-E":
		if !exists {
			return errors.New("service missing")
		}
		old.Scheduler = rule.Scheduler
		d.rules[rule.listener] = old
	case "-D":
		if !exists {
			return errors.New("service missing")
		}
		delete(d.rules, rule.listener)
	default:
		panic(action)
	}
	return nil
}
func (d *fakeDriver) Destination(action string, key listener, dst destination) error {
	if err := d.call(action); err != nil {
		return err
	}
	rule, exists := d.rules[key]
	if !exists {
		return errors.New("service missing")
	}
	index := slices.IndexFunc(rule.Destinations, func(old destination) bool { return old.Address == dst.Address })
	switch action {
	case "-a":
		if index >= 0 {
			return errors.New("destination exists")
		}
		rule.Destinations = append(rule.Destinations, dst)
	case "-e":
		if index < 0 {
			return errors.New("destination missing")
		}
		rule.Destinations[index] = dst
	case "-d":
		if index < 0 {
			return errors.New("destination missing")
		}
		rule.Destinations = slices.Delete(rule.Destinations, index, index+1)
	default:
		panic(action)
	}
	d.rules[key] = rule
	return nil
}

func testAgent(t *testing.T) (*Agent, *fakeDriver) {
	t.Helper()
	d := &fakeDriver{rules: make(map[listener]virtualService)}
	a, err := openAgent(d, t.TempDir(), []string{"192.0.2.10"}, "rr", "nat")
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}
func podResources() sync.ControllerResources {
	ready := true
	return sync.ControllerResources{
		Service:       corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", Annotations: map[string]string{apis.SvcAnnClass: "lvs", apis.SvcAnnNetworkType: "pod", apis.SvcAnnProtolPort: "https:443", apis.SvcAnnCertSecrets: "missing"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8443)}}}},
		EndpointSlice: discovery.EndpointSlice{Endpoints: []discovery.Endpoint{{Addresses: []string{"10.0.0.2"}, Conditions: discovery.EndpointConditions{Ready: &ready}}}},
	}
}
func refOf(cr sync.ControllerResources) sync.ServiceRef {
	return sync.ServiceRef{Namespace: cr.Service.Namespace, Name: cr.Service.Name}
}
func apply(t *testing.T, a *Agent, cr sync.ControllerResources) {
	t.Helper()
	if _, err := a.Apply(cr); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTCPUDPIgnoresL7AndIsIdempotent(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	appProtocol := "https"
	cr.Service.Spec.Ports[0].AppProtocol = &appProtocol
	cr.Service.Spec.Ports = append(cr.Service.Spec.Ports, corev1.ServicePort{Name: "dns", Port: 443, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(5353)})
	cr.Secrets = []corev1.Secret{{Data: map[string][]byte{"tls.crt": []byte("invalid")}}}
	d.before = func() {
		data, err := os.ReadFile(filepath.Join(a.dir, stateFilename(refOf(cr))))
		if err != nil || len(data) == 0 || a.records[refOf(cr)].Pending == nil {
			t.Fatal("kernel mutated before durable ownership")
		}
	}
	resp, err := a.Apply(cr)
	if err != nil || !reflect.DeepEqual(resp.ExternalIPs, []string{"192.0.2.10"}) {
		t.Fatalf("Apply: %+v, %v", resp, err)
	}
	d.before = nil
	for protocol, backend := range map[string]string{"tcp": "10.0.0.2:8443", "udp": "10.0.0.2:5353"} {
		rule := d.rules[listener{protocol, "192.0.2.10:443"}]
		if rule.Scheduler != "rr" || len(rule.Destinations) != 1 || rule.Destinations[0] != (destination{backend, "nat", 1}) {
			t.Fatalf("wrong %s rule: %+v", protocol, rule)
		}
	}
	if !reflect.DeepEqual(a.L4Ports(), []int{443}) || !reflect.DeepEqual(a.UDPPorts(), []int{443}) {
		t.Fatal("wrong listener port lists")
	}
	before, err := os.Stat(filepath.Join(a.dir, stateFilename(refOf(cr))))
	if err != nil {
		t.Fatal(err)
	}
	d.operations = nil
	apply(t, a, cr)
	after, _ := os.Stat(filepath.Join(a.dir, stateFilename(refOf(cr))))
	if len(d.operations) != 0 || before.ModTime() != after.ModTime() {
		t.Fatal("unchanged apply mutated kernel or state")
	}
	if cr.Service.Spec.Ports[0].AppProtocol == nil || cr.Service.Annotations[apis.SvcAnnCertSecrets] != "missing" {
		t.Fatal("modified caller resources")
	}
}

func TestUpdateBackendsPortsAndDeleteOnlyOwnedRules(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	apply(t, a, cr)
	unowned := virtualService{listener: listener{"tcp", "203.0.113.5:443"}, Scheduler: "wlc"}
	d.rules[unowned.listener] = unowned
	cr.EndpointSlice.Endpoints[0].Addresses[0] = "10.0.0.3"
	d.operations = nil
	apply(t, a, cr)
	if !reflect.DeepEqual(d.operations, []string{"-a", "-d"}) {
		t.Fatalf("must add before deleting old backend: %v", d.operations)
	}
	cr.Service.Spec.Ports[0].Port = 444
	apply(t, a, cr)
	if _, exists := d.rules[listener{"tcp", "192.0.2.10:443"}]; exists {
		t.Fatal("old listener remains")
	}
	if err := a.Delete(refOf(cr)); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(refOf(cr)); err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 1 || d.rules[unowned.listener].Scheduler != "wlc" {
		t.Fatal("cleanup changed unmanaged rules")
	}
	refs, _ := a.List()
	if len(refs) != 0 {
		t.Fatal("cleanup left ownership")
	}
}

func TestFailureRollbackRestartAndCleanup(t *testing.T) {
	for _, action := range []string{"-A", "-a", "-D"} {
		t.Run(action, func(t *testing.T) {
			a, d := testAgent(t)
			cr := podResources()
			apply(t, a, cr)
			before, _ := d.List()
			cr.Service.Spec.Ports[0].Port = 444
			d.failAction, d.failOnce = action, true
			if _, err := a.Apply(cr); err == nil {
				t.Fatal("expected injected failure")
			}
			if !sameServices(d.rules, before) {
				t.Fatal("kernel rollback failed")
			}
			if a.records[refOf(cr)].Pending == nil {
				t.Fatal("failed operation lost pending ownership")
			}
			restarted, err := openAgent(d, a.dir, a.vips, a.scheduler, a.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.PreflightCheck(); err != nil {
				t.Fatal(err)
			}
			ports, _ := restarted.ListListeners()
			if len(ports[refOf(cr)]) != 2 {
				t.Fatal("restart lost old/new listener reservations")
			}
			apply(t, restarted, cr)
			d.failAction, d.failOnce = "-D", true
			if err := restarted.Delete(refOf(cr)); err == nil {
				t.Fatal("expected cleanup failure")
			}
			if len(d.rules) != 1 {
				t.Fatal("failed cleanup did not retain previous rule")
			}
			if err := restarted.Delete(refOf(cr)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartRestoresKernelAfterHostReboot(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	apply(t, a, cr)
	before, _ := d.List()
	d.rules = make(map[listener]virtualService)
	restarted, err := openAgent(d, a.dir, a.vips, a.scheduler, a.mode)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.PreflightCheck(); err != nil {
		t.Fatal(err)
	}
	if !sameServices(before, d.rules) {
		t.Fatal("host reboot lost committed IPVS rules")
	}
}

func TestUnmanagedConflictAndPersistenceFailureDoNotMutateKernel(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	key := listener{"tcp", "192.0.2.10:443"}
	d.rules[key] = virtualService{listener: key, Scheduler: "rr"}
	if _, err := a.Apply(cr); !errors.Is(err, servicelock.ErrConflict) {
		t.Fatalf("unmanaged collision: %v", err)
	}
	if len(d.operations) != 0 || len(a.records) != 0 {
		t.Fatal("conflict modified state")
	}
	delete(d.rules, key)
	a.dir = filepath.Join(a.dir, "missing")
	if _, err := a.Apply(cr); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(d.operations) != 0 || len(a.records) != 0 {
		t.Fatal("write failure modified kernel or memory")
	}
}

func TestValidationRetainsPreviousConfiguration(t *testing.T) {
	for _, failure := range []string{"zero-backends", "sctp", "invalid-ip", "dr-port-translation", "network-type"} {
		t.Run(failure, func(t *testing.T) {
			a, d := testAgent(t)
			cr := podResources()
			apply(t, a, cr)
			before, _ := d.List()
			d.operations = nil
			switch failure {
			case "zero-backends":
				cr.EndpointSlice.Endpoints = nil
			case "sctp":
				cr.Service.Spec.Ports[0].Protocol = corev1.ProtocolSCTP
			case "invalid-ip":
				cr.EndpointSlice.Endpoints[0].Addresses[0] = "bad-ip"
			case "dr-port-translation":
				a.mode = "dr"
			case "network-type":
				cr.Service.Annotations[apis.SvcAnnNetworkType] = "bad"
			}
			if _, err := a.Apply(cr); err == nil {
				t.Fatal("expected validation failure")
			}
			if len(d.operations) != 0 || !sameServices(before, d.rules) {
				t.Fatal("invalid request changed kernel")
			}
		})
	}
}

func TestDRAndTunnelWithMatchingPorts(t *testing.T) {
	for _, mode := range []string{"dr", "tun"} {
		t.Run(mode, func(t *testing.T) {
			a, d := testAgent(t)
			a.mode = mode
			cr := podResources()
			cr.Service.Spec.Ports[0].TargetPort = intstr.FromInt(443)
			apply(t, a, cr)
			if d.rules[listener{"tcp", "192.0.2.10:443"}].Destinations[0].Mode != mode {
				t.Fatal("incorrect forwarding mode")
			}
		})
	}
}

func TestCorruptOrConflictingStateFailsStartup(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	apply(t, a, cr)
	path := filepath.Join(a.dir, stateFilename(refOf(cr)))
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openAgent(d, a.dir, a.vips, a.scheduler, a.mode); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func TestConcurrentApplyAndListenerReads(t *testing.T) {
	a, _ := testAgent(t)
	cr := podResources()
	apply(t, a, cr)
	var wg stdsync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Apply(cr); err != nil {
				t.Error(err)
			}
			a.L4Ports()
			a.UDPPorts()
			_, _ = a.List()
		}()
	}
	wg.Wait()
}

func TestCommitFailureRetainsDurablePendingState(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	originalDir := a.dir
	d.before = func() { a.dir = filepath.Join(originalDir, "missing") }
	if _, err := a.Apply(cr); err == nil {
		t.Fatal("expected result persistence failure")
	}
	if a.records[refOf(cr)].Pending == nil {
		t.Fatal("lost pending record after result write failure")
	}
	d.before = nil
	restarted, err := openAgent(d, originalDir, a.vips, a.scheduler, a.mode)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.PreflightCheck(); err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("uncommitted first apply must recover empty previous config")
	}
	apply(t, restarted, cr)
}

func TestUnfinishedRollbackKeepsOwnershipForCleanup(t *testing.T) {
	a, d := testAgent(t)
	cr := podResources()
	apply(t, a, cr)
	cr.Service.Spec.Ports[0].Port = 444
	d.failAction = "-D" // 更新删除旧 listener 失败，回滚删除新 listener 也失败。
	if _, err := a.Apply(cr); err == nil {
		t.Fatal("expected partial update and rollback failure")
	}
	if len(d.rules) != 2 {
		t.Fatal("test must leave partially applied rules")
	}
	ports, _ := a.ListListeners()
	if len(ports[refOf(cr)]) != 2 {
		t.Fatal("lost one of the uncertain listeners")
	}
	d.failAction = ""
	if err := a.Delete(refOf(cr)); err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("cleanup missed partially applied rules")
	}
}

func TestMultipleVIPsAndSchedulerChange(t *testing.T) {
	a, d := testAgent(t)
	a.vips = []string{"192.0.2.10", "192.0.2.11"}
	cr := podResources()
	apply(t, a, cr)
	if len(d.rules) != 2 {
		t.Fatal("not all VIPs programmed")
	}
	a.vips = []string{"192.0.2.11"}
	a.scheduler = "wlc"
	d.operations = nil
	apply(t, a, cr)
	if len(d.rules) != 1 || d.rules[listener{"tcp", "192.0.2.11:443"}].Scheduler != "wlc" {
		t.Fatal("VIP/scheduler change failed")
	}
	if !slices.Contains(d.operations, "-E") || !slices.Contains(d.operations, "-D") {
		t.Fatalf("expected scoped edit/delete: %v", d.operations)
	}
}
