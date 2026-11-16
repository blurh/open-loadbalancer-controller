// Package lvs 使用 ipvsadm 管理 Linux IPVS 的 TCP/UDP virtual service。
package lvs

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
)

type listener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
}
type destination struct {
	Address string `json:"address"`
	Mode    string `json:"mode"`
	Weight  int    `json:"weight"`
}
type virtualService struct {
	listener
	Scheduler    string        `json:"scheduler"`
	Destinations []destination `json:"destinations,omitempty"`
	// 不静默丢弃内核表中手工添加的扩展选项。
	Unsupported bool `json:"-"`
}
type driver interface {
	List() (map[listener]virtualService, error)
	Service(string, virtualService) error
	Destination(string, listener, destination) error
}
type Agent struct {
	mu        stdsync.Mutex
	driver    driver
	dir       string
	vips      []string
	scheduler string
	mode      string
	records   map[sync.ServiceRef]record
	restored  bool
}

func New() (*Agent, error) {
	cfg := config.NewConfig()
	if !slices.Contains(schedulers, cfg.LVSScheduler()) {
		return nil, fmt.Errorf("unsupported LVS scheduler %q", cfg.LVSScheduler())
	}
	if !slices.Contains([]string{"nat", "dr", "tun"}, cfg.LVSForwarding()) {
		return nil, fmt.Errorf("unsupported LVS forwarding mode %q", cfg.LVSForwarding())
	}
	d, err := newCommandDriver(cfg.LVSBinary(), cfg.LVSCommandTimeout())
	if err != nil {
		return nil, err
	}
	vips, err := resolveVIPs(cfg.LVSVIPs(), discoverPrivateIPv4)
	if err != nil {
		return nil, err
	}
	return openAgent(d, filepath.Join(cfg.AgentStateDir(), "lvs"), vips, cfg.LVSScheduler(), cfg.LVSForwarding())
}

var schedulers = []string{"rr", "wrr", "lc", "wlc", "lblc", "lblcr", "dh", "sh", "sed", "nq"}

func (a *Agent) PreflightCheck() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, err := a.driver.List()
	if err != nil {
		return err
	}
	if a.restored {
		return nil
	}
	// 主机重启后 IPVS 表会消失；恢复最后一次已提交配置，未确认请求等待 controller 重试。
	var refs []sync.ServiceRef
	for ref := range a.records {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return stateFilename(refs[i]) < stateFilename(refs[j]) })
	for _, ref := range refs {
		state := a.records[ref]
		owned := ownedListeners(state)
		for _, key := range owned {
			if current[key].Unsupported {
				return fmt.Errorf("cannot restore externally modified LVS listener %v", key)
			}
		}
		if err := a.reconcile(current, serviceMap(state.Applied), owned); err != nil {
			return fmt.Errorf("restore LVS %s/%s: %w", ref.Namespace, ref.Name, err)
		}
	}
	a.restored = true
	return nil
}

func (a *Agent) desired(cr sync.ControllerResources) ([]virtualService, error) {
	cr = parse.LVSResources(cr)
	if cr.Service.Spec.SessionAffinity != "" && cr.Service.Spec.SessionAffinity != corev1.ServiceAffinityNone {
		return nil, errors.New("LVS sessionAffinity is not supported")
	}
	var desired []virtualService
	seen := make(map[listener]bool)
	for _, port := range cr.Service.Spec.Ports {
		protocol := strings.ToLower(string(port.Protocol))
		if (protocol != "tcp" && protocol != "udp") || port.Port < 1 || port.Port > 65535 {
			return nil, fmt.Errorf("unsupported LVS listener %s/%d", port.Protocol, port.Port)
		}
		backends, err := parse.GenBackendsForPort(cr, port)
		if err != nil {
			return nil, err
		}
		if len(backends) == 0 {
			return nil, fmt.Errorf("LVS listener %s/%d has no ready backends; retaining previous configuration", protocol, port.Port)
		}
		sort.Strings(backends)
		var destinations []destination
		for _, address := range slices.Compact(backends) {
			normalized, err := ipv4Endpoint(address)
			if err != nil {
				return nil, fmt.Errorf("invalid LVS backend: %w", err)
			}
			_, backendPort, _ := net.SplitHostPort(normalized)
			if a.mode != "nat" && backendPort != strconv.Itoa(int(port.Port)) {
				return nil, fmt.Errorf("LVS %s cannot translate port %d to %s; use nat or matching backend ports", a.mode, port.Port, backendPort)
			}
			destinations = append(destinations, destination{Address: normalized, Mode: a.mode, Weight: 1})
		}
		for _, vip := range a.vips {
			key := listener{Protocol: protocol, Address: net.JoinHostPort(vip, strconv.Itoa(int(port.Port)))}
			if seen[key] {
				return nil, fmt.Errorf("duplicate LVS listener %s/%s", protocol, key.Address)
			}
			seen[key] = true
			desired = append(desired, virtualService{listener: key, Scheduler: a.scheduler, Destinations: slices.Clone(destinations)})
		}
	}
	sortServices(desired)
	return desired, nil
}

func (a *Agent) Apply(cr sync.ControllerResources) (resp sync.RespBody, retErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ref := sync.ServiceRef{Namespace: cr.Service.Namespace, Name: cr.Service.Name}
	timer := metrics.ObserveOperationDuration("apply")
	defer timer.ObserveDuration()
	defer func() { recordOperation("apply", ref, retErr) }()
	if err := servicelock.ValidateService(ref); err != nil {
		return resp, err
	}
	desired, err := a.desired(cr)
	if err != nil {
		return resp, err
	}
	if err := a.change(ref, desired); err != nil {
		return resp, err
	}
	// 回写实际监听地址，自动发现的内网 IP 归入 InternalIPs。
	for _, address := range a.vips {
		if net.ParseIP(address).IsPrivate() {
			resp.InternalIPs = append(resp.InternalIPs, address)
		} else {
			resp.ExternalIPs = append(resp.ExternalIPs, address)
		}
	}
	return resp, nil
}

func (a *Agent) Delete(ref sync.ServiceRef) (retErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	timer := metrics.ObserveOperationDuration("delete")
	defer timer.ObserveDuration()
	defer func() { recordOperation("delete", ref, retErr) }()
	if err := servicelock.ValidateService(ref); err != nil {
		return err
	}
	if _, ok := a.records[ref]; !ok {
		return nil
	}
	if err := a.change(ref, nil); err != nil {
		return err
	}
	return a.removeRecord(ref)
}

func recordOperation(operation string, ref sync.ServiceRef, err error) {
	status := "success"
	if err != nil {
		status = "failure"
	}
	metrics.RecordOperation(operation, status, ref.Namespace+"/"+ref.Name)
}

// change 先记录新旧 listener 并集，再改内核。失败保留 pending，重试时读取实际内核状态。
func (a *Agent) change(ref sync.ServiceRef, desired []virtualService) error {
	current, err := a.driver.List()
	if err != nil {
		return err
	}
	old := a.records[ref]
	owned := ownedListeners(old)
	for _, rule := range desired {
		if _, exists := current[rule.listener]; exists && !slices.Contains(owned, rule.listener) {
			return fmt.Errorf("%w: unmanaged IPVS listener %s/%s", servicelock.ErrConflict, rule.Protocol, rule.Address)
		}
		for other, state := range a.records {
			if other != ref && slices.Contains(ownedListeners(state), rule.listener) {
				return fmt.Errorf("%w: LVS listener belongs to %s/%s", servicelock.ErrConflict, other.Namespace, other.Name)
			}
		}
		if !slices.Contains(owned, rule.listener) {
			owned = append(owned, rule.listener)
		}
	}
	sortListeners(owned)
	before := make(map[listener]virtualService)
	for _, key := range owned {
		if rule, ok := current[key]; ok {
			if rule.Unsupported {
				return fmt.Errorf("managed IPVS listener %s/%s has unsupported external options", key.Protocol, key.Address)
			}
			before[key] = rule
		}
	}
	if old.Pending == nil && reflect.DeepEqual(old.Applied, desired) && sameServices(before, serviceMap(desired)) {
		return nil
	}
	pending := record{Version: 1, Service: ref, Applied: old.Applied, Pending: &pendingChange{Desired: desired, Owned: owned}}
	if err := a.saveRecord(pending); err != nil {
		return err
	}
	if err := a.reconcile(current, serviceMap(desired), owned); err != nil {
		// 内核操作没有原子批量提交。仅回滚本 Service，绝不清空全局 IPVS 表。
		actual, readErr := a.driver.List()
		var rollbackErr error
		if readErr == nil {
			rollbackErr = a.reconcile(actual, before, owned)
		}
		return errors.Join(err, readErr, rollbackErr)
	}
	return a.saveRecord(record{Version: 1, Service: ref, Applied: desired})
}

func sameServices(a, b map[listener]virtualService) bool {
	if len(a) != len(b) {
		return false
	}
	for key, want := range b {
		have, ok := a[key]
		if !ok || have.Scheduler != want.Scheduler || have.Unsupported || len(have.Destinations) != len(want.Destinations) {
			return false
		}
		for _, dst := range want.Destinations {
			if !slices.Contains(have.Destinations, dst) {
				return false
			}
		}
	}
	return true
}

func (a *Agent) reconcile(current, desired map[listener]virtualService, owned []listener) error {
	// 先增改后移除，不先清空现有转发。
	for _, key := range owned {
		want, ok := desired[key]
		if !ok {
			continue
		}
		have, exists := current[key]
		if !exists {
			if err := a.driver.Service("-A", want); err != nil {
				return err
			}
		} else if have.Scheduler != want.Scheduler {
			if err := a.driver.Service("-E", want); err != nil {
				return err
			}
		}
		oldDest := make(map[string]destination)
		for _, dst := range have.Destinations {
			oldDest[dst.Address] = dst
		}
		for _, dst := range want.Destinations {
			previous, exists := oldDest[dst.Address]
			if !exists {
				if err := a.driver.Destination("-a", key, dst); err != nil {
					return err
				}
			} else if previous != dst {
				if err := a.driver.Destination("-e", key, dst); err != nil {
					return err
				}
			}
			delete(oldDest, dst.Address)
		}
		var stale []string
		for address := range oldDest {
			stale = append(stale, address)
		}
		sort.Strings(stale)
		for _, address := range stale {
			if err := a.driver.Destination("-d", key, oldDest[address]); err != nil {
				return err
			}
		}
	}
	for _, key := range owned {
		if _, wanted := desired[key]; wanted {
			continue
		}
		if have, exists := current[key]; exists {
			if err := a.driver.Service("-D", have); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) List() ([]sync.ServiceRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var refs []sync.ServiceRef
	for ref := range a.records {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Namespace+"/"+refs[i].Name < refs[j].Namespace+"/"+refs[j].Name })
	return refs, nil
}
func (a *Agent) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[sync.ServiceRef][]servicelock.Port)
	for ref, state := range a.records {
		for _, key := range ownedListeners(state) {
			_, number, _ := net.SplitHostPort(key.Address)
			port, _ := strconv.Atoi(number)
			p := servicelock.Port{Protocol: key.Protocol, Number: port}
			if !slices.Contains(result[ref], p) {
				result[ref] = append(result[ref], p)
			}
		}
	}
	return result, nil
}
func (a *Agent) L4Ports() []int  { return a.ports("tcp") }
func (a *Agent) UDPPorts() []int { return a.ports("udp") }
func (a *Agent) ports(protocol string) []int {
	listeners, _ := a.ListListeners()
	var ports []int
	for _, entries := range listeners {
		for _, entry := range entries {
			if entry.Protocol == protocol {
				ports = append(ports, entry.Number)
			}
		}
	}
	sort.Ints(ports)
	return slices.Compact(ports)
}
func serviceMap(services []virtualService) map[listener]virtualService {
	result := make(map[listener]virtualService)
	for _, rule := range services {
		result[rule.listener] = rule
	}
	return result
}
func sortListeners(keys []listener) {
	sort.Slice(keys, func(i, j int) bool { return keys[i].Protocol+keys[i].Address < keys[j].Protocol+keys[j].Address })
}
func sortServices(rules []virtualService) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].Protocol+rules[i].Address < rules[j].Protocol+rules[j].Address })
}
func ipv4Endpoint(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return "", fmt.Errorf("expected IPv4:port, got %q", value)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(n)), nil
}
