package agent

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	stdsync "sync"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/lvs"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/nginx"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

// kubernetes.io/openlb.class: "nginx" # lvs

// Forwarder 是转发器的抽象，操作的最小单元是一个 Service。
// 构造（New）只负责初始化转发器本身，渲染/reload 都在 Apply/Delete 中完成。
type Forwarder interface {
	// PreflightCheck 自检底层转发器是否可用（如 nginx -t）。
	PreflightCheck() error
	// Apply 全量幂等地同步一个 Service 的配置并 reload，返回回写 status 的 ingress IP。
	Apply(cr sync.ControllerResources) (sync.RespBody, error)
	// Delete 删除一个 Service 的配置并 reload。
	Delete(ref sync.ServiceRef) error
	// List 返回当前被纳管的 Service（用于启动对账 / GC 孤儿配置）。
	List() ([]sync.ServiceRef, error)
	// ListListeners 返回磁盘配置中的端口归属，包含 HTTP/HTTPS 和部分写入的配置。
	ListListeners() (map[sync.ServiceRef][]servicelock.Port, error)
	// L4Ports 返回当前转发器监听的四层(TCP)端口列表。
	// 通用四层连接数采集器(pkg/agent/stats)据此从 /proc/net/tcp 过滤统计连接数。
	// 无四层监听端口时返回 nil/空切片,四层指标将清零。
	L4Ports() []int
	// UDPPorts 返回当前转发器监听的 UDP 端口列表。
	// 通用 UDP 套接字采集器(pkg/agent/stats)据此从 /proc/net/udp 过滤统计套接字数。
	// 无 UDP 监听端口时返回 nil/空切片,UDP 指标将清零。
	UDPPorts() []int
}

// ErrInvalidClass 表示请求中的 class 非法，HTTP 层应返回 400 而非重试性错误。
var ErrInvalidClass = errors.New("invalid forwarder class")

type forwarderFactory func() (Forwarder, error)

// Agent 按 Service 的 class 分派请求。每种转发器只创建一个实例，以保留其配置锁。
// cleanup 不携带 class，因此持久化 Service 的归属，并在启动时合并已有配置。
type Agent struct {
	mu          stdsync.Mutex
	forwarders  map[string]Forwarder
	unavailable map[string]error
	owners      map[sync.ServiceRef]map[string]struct{}
	locks       *servicelock.Store
}

var _ Forwarder = (*Agent)(nil)

func NewAgent() (*Agent, error) {
	return newAgent(map[string]forwarderFactory{
		"nginx": func() (Forwarder, error) { return nginx.New() },
		"lvs":   func() (Forwarder, error) { return lvs.New() },
	}, config.NewConfig().AgentStateDir())
}

func newAgent(factories map[string]forwarderFactory, stateDir string) (*Agent, error) {
	locks, err := servicelock.Open(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open service locks: %w", err)
	}
	a := &Agent{
		forwarders:  make(map[string]Forwarder),
		unavailable: make(map[string]error),
		owners:      make(map[sync.ServiceRef]map[string]struct{}),
		locks:       locks,
	}
	var initErrors []error
	var observed []servicelock.Reservation
	for _, class := range sortedClasses(factories) {
		f, err := factories[class]()
		if err == nil {
			err = f.PreflightCheck()
		}
		if err != nil {
			a.unavailable[class] = err
			initErrors = append(initErrors, fmt.Errorf("%s: %w", class, err))
			log.Printf("forwarder %q unavailable: %v", class, err)
			continue
		}
		refs, err := f.List()
		if err != nil {
			// 已可用的转发器无法恢复归属时，不带着不完整状态处理 cleanup。
			return nil, fmt.Errorf("list services for %s: %w", class, err)
		}
		a.forwarders[class] = f
		for _, ref := range refs {
			a.remember(ref, class)
		}
		listeners, err := f.ListListeners()
		if err != nil {
			return nil, fmt.Errorf("list listeners for %s: %w", class, err)
		}
		for ref, ports := range listeners {
			observed = append(observed, servicelock.Reservation{Service: ref, Class: class, Ports: ports})
		}
	}
	if len(a.forwarders) == 0 {
		return nil, fmt.Errorf("no usable forwarders: %w", errors.Join(initErrors...))
	}
	if err := locks.Restore(observed); err != nil {
		return nil, fmt.Errorf("restore service locks: %w", err)
	}
	for _, entry := range locks.Reservations() {
		a.remember(entry.Service, entry.Class)
	}
	a.updateServicesMetric()
	return a, nil
}

// HasForwarder 表示本机已初始化该转发器，供专属指标采集器选择是否启动。
// forwarders 在构造完成后不再修改，可安全地并发读取。
func (a *Agent) HasForwarder(class string) bool {
	_, ok := a.forwarders[class]
	return ok
}

func (a *Agent) PreflightCheck() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var errs []error
	for _, class := range sortedClasses(a.forwarders) {
		if err := a.forwarders[class].PreflightCheck(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", class, err))
		}
	}
	return errors.Join(errs...)
}

func (a *Agent) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
	if parse.IsLVS(cr.Service) {
		cr = parse.LVSResources(cr)
	}
	class, specified := cr.Service.Annotations[apis.SvcAnnClass]
	if !specified {
		class = "nginx" // 兼容原先未填写 class 的 Service。
	}
	class = strings.TrimSpace(class)
	f, ok := a.forwarders[class]
	if !ok {
		if err, known := a.unavailable[class]; known {
			return sync.RespBody{}, fmt.Errorf("forwarder %q unavailable: %w", class, err)
		}
		return sync.RespBody{}, fmt.Errorf("%w: %s=%q", ErrInvalidClass, apis.SvcAnnClass, class)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.updateServicesMetric()
	ref := sync.ServiceRef{Namespace: cr.Service.Namespace, Name: cr.Service.Name}
	ports, err := servicelock.RequestedPorts(cr)
	if err != nil {
		return sync.RespBody{}, err
	}
	pending := a.locks.Pending(ref, class)
	// 冲突检查及预留必须早于旧 class 的清理，拒绝请求时不改动原有配置。
	if err := a.locks.Reserve(ref, class, ports); err != nil {
		return sync.RespBody{}, err
	}
	a.remember(ref, class)
	if err := a.deleteOtherClasses(ref, class); err != nil {
		return sync.RespBody{}, err
	}
	// 上次可能已写文件但 reload 失败；不能以文件无变更为由提前释放旧端口。
	cr.ForceReload = cr.ForceReload || pending
	resp, err := f.Apply(cr)
	if err != nil {
		return resp, err
	}
	listeners, err := f.ListListeners()
	if err != nil {
		return resp, fmt.Errorf("list listeners after apply: %w", err)
	}
	// 零后端时转发器可能保留旧配置，也必须继续持有相应端口。
	return resp, a.locks.Commit(ref, class, append(ports, listeners[ref]...))
}

func (a *Agent) Delete(ref sync.ServiceRef) error {
	if err := servicelock.ValidateService(ref); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.updateServicesMetric()
	// 文件已删除但 reload 失败后重启，List 可能恢复不到归属；此时让所有
	// 可用实现再做一次幂等删除/reload，不能直接把未知 Service 视为清理成功。
	if len(a.owners[ref]) == 0 {
		for class := range a.forwarders {
			a.remember(ref, class)
		}
	}
	return a.deleteOtherClasses(ref, "")
}

// 调用方持有 a.mu；只有删除成功才忘记归属，reload 失败时仍可重试。
func (a *Agent) deleteOtherClasses(ref sync.ServiceRef, keepClass string) error {
	var errs []error
	for _, class := range sortedClasses(a.owners[ref]) {
		if class == keepClass {
			continue
		}
		f, ok := a.forwarders[class]
		if !ok {
			errs = append(errs, fmt.Errorf("cleanup %s/%s: owning forwarder %q unavailable", ref.Namespace, ref.Name, class))
			continue
		}
		// 删除文件之前记录 pending，重启后仍能重试 reload 并保留端口。
		if err := a.locks.Reserve(ref, class, nil); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := f.Delete(ref); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s/%s from %s: %w", ref.Namespace, ref.Name, class, err))
			continue
		}
		if err := a.locks.Release(ref, class); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(a.owners[ref], class)
	}
	if len(a.owners[ref]) == 0 {
		delete(a.owners, ref)
	}
	return errors.Join(errs...)
}

func (a *Agent) remember(ref sync.ServiceRef, class string) {
	if a.owners[ref] == nil {
		a.owners[ref] = make(map[string]struct{})
	}
	a.owners[ref][class] = struct{}{}
}

func (a *Agent) List() ([]sync.ServiceRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listServices()
}

func (a *Agent) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	listeners := make(map[sync.ServiceRef][]servicelock.Port)
	for _, f := range a.forwarders {
		entries, err := f.ListListeners()
		if err != nil {
			return nil, err
		}
		for ref, ports := range entries {
			listeners[ref] = append(listeners[ref], ports...)
		}
	}
	return listeners, nil
}

func (a *Agent) listServices() ([]sync.ServiceRef, error) {
	set := make(map[sync.ServiceRef]struct{})
	for _, class := range sortedClasses(a.forwarders) {
		refs, err := a.forwarders[class].List()
		if err != nil {
			return nil, fmt.Errorf("list services for %s: %w", class, err)
		}
		for _, ref := range refs {
			set[ref] = struct{}{}
		}
	}
	refs := make([]sync.ServiceRef, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		return refs[i].Name < refs[j].Name
	})
	return refs, nil
}

func (a *Agent) updateServicesMetric() {
	if refs, err := a.listServices(); err == nil {
		metrics.SetServicesManaged(len(refs))
	}
}

func (a *Agent) L4Ports() []int {
	return a.ports(func(f Forwarder) []int { return f.L4Ports() })
}

func (a *Agent) UDPPorts() []int {
	return a.ports(func(f Forwarder) []int { return f.UDPPorts() })
}

func (a *Agent) ports(getPorts func(Forwarder) []int) []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	set := make(map[int]struct{})
	for _, f := range a.forwarders {
		for _, port := range getPorts(f) {
			set[port] = struct{}{}
		}
	}
	ports := make([]int, 0, len(set))
	for port := range set {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func sortedClasses[T any](classes map[string]T) []string {
	keys := make([]string, 0, len(classes))
	for class := range classes {
		keys = append(keys, class)
	}
	sort.Strings(keys)
	return keys
}
