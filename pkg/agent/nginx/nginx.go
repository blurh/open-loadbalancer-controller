package nginx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/metrics"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/certificates"
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

type Agent struct {
	commandTimeout time.Duration
	nginx          string // nginx 二进制路径
	nginxHome      string // 配置根目录，带末尾 `/`

	// mu 串行化 Apply/Delete：二者都写共享配置目录并执行全局 `nginx -s reload`，
	// HTTP server 为每个请求开 goroutine，若并发执行会交错写文件、并发 reload。
	mu stdsync.Mutex
}

const (
	ConfigStreamPath = "conf.d/stream/"
	ConfigHttpPath   = "conf.d/http/"
	TLSFilePath      = "tls/"
)

func New() (*Agent, error) {
	cfg := config.NewConfig()

	nginxBin := cfg.NginxBin()
	if nginxBin == "" {
		p, err := exec.LookPath("nginx")
		if err != nil {
			return nil, err
		}
		nginxBin = p
	}

	// nginxHome 统一带末尾 `/`
	home, err := filepath.Abs(cfg.NginxHome())
	if err != nil {
		return nil, err
	}
	home = strings.TrimSuffix(home, "/") + "/"

	na := &Agent{
		nginx:          nginxBin,
		commandTimeout: cfg.NginxCommandTimeout(),
		nginxHome:      home,
	}

	// 确保 http/stream/tls 配置目录存在，否则首次渲染会失败
	for _, dir := range []string{
		home + ConfigHttpPath,
		home + ConfigStreamPath,
		home + TLSFilePath,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	if err := na.recoverTransaction(); err != nil {
		return nil, err
	}
	return na, nil
}

func (na *Agent) PreflightCheck() error {
	return na.testConfig()
}

// testConfig 执行 `nginx -t` 校验当前磁盘上的全量配置是否合法。
func (na *Agent) testConfig() error {
	return na.command("-t")
}

// reload 先 `nginx -t` 校验全量配置，通过后才真正 reload。
// nginx 是全局共享配置 + 单次 reload：任一 Service 的非法配置都会让 reload 整体失败、
// 波及所有 Service。reload 前先把关，校验不过则拒绝 reload，运行中的 nginx 保持旧配置不受影响。
func (na *Agent) reload() error {
	if err := na.testConfig(); err != nil {
		metrics.RecordReload("failure")
		return fmt.Errorf("nginx -t failed, refusing reload: %w", err)
	}
	if err := na.command("-s", "reload"); err != nil {
		metrics.RecordReload("failure")
		return err
	}
	metrics.RecordReload("success")
	return nil
}

// streamAndHttpDirs 返回承载 server/upstream 配置的目录（不含 tls）。
func (na *Agent) streamAndHttpDirs() []string {
	return []string{
		na.nginxHome + ConfigHttpPath,
		na.nginxHome + ConfigStreamPath,
	}
}

// managedDirs 返回所有存放托管文件的目录，含 tls。
// gc 需要覆盖 tls 才能回收证书；List 只看 http/stream（证书不是 Service，不应被解析成 ServiceRef）。
func (na *Agent) managedDirs() []string {
	return append(na.streamAndHttpDirs(), na.nginxHome+TLSFilePath)
}

// svcPrefix 是某个 Service 所有配置文件的统一前缀：`<ns>_<name>_`。
// ns/name 均为 DNS 标签，不含 `_`，故可用 `_` 做字段分隔符无歧义。
func svcPrefix(ns, name string) string {
	return ns + "_" + name + "_"
}

// listenerLocation 返回某协议 listener 配置所在目录与文件名中的协议段 key。
// ok=false 表示协议为空/未知，应跳过该 listener。
func (na *Agent) listenerLocation(protocol string) (dir, key string, ok bool) {
	switch protocol {
	case "tcp", "udp":
		return na.nginxHome + ConfigStreamPath, protocol, true
	case "http":
		return na.nginxHome + ConfigHttpPath, "http", true
	case "https":
		return na.nginxHome + ConfigHttpPath, "https", true
	}
	return "", "", false
}

func (na *Agent) Apply(cr sync.ControllerResources) (resp sync.RespBody, retErr error) {
	na.mu.Lock()
	defer na.mu.Unlock()

	// 记录操作耗时
	timer := metrics.ObserveOperationDuration("apply")
	defer timer.ObserveDuration()

	svc := cr.Service
	svcKey := svc.Namespace + "/" + svc.Name

	defer func() {
		result := "success"
		if retErr != nil {
			result = "failure"
		}
		metrics.RecordOperation("apply", result, svcKey)
		na.updateServicesMetric()
	}()
	if err := certificates.Validate(cr); err != nil {
		return resp, err
	}
	prefix := svcPrefix(svc.Namespace, svc.Name)
	if err := na.beginTransaction(prefix); err != nil {
		return resp, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, na.recoverTransaction())
		}
	}()

	var errs []error
	existsFiles := []string{}
	changed := false

	for _, svcSpecPortObj := range svc.Spec.Ports {
		listenerPort := strconv.Itoa(int(svcSpecPortObj.Port))
		protocol, err := parse.FrontendProtocol(cr, svcSpecPortObj)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dir, _, ok := na.listenerLocation(protocol)
		if !ok {
			errs = append(errs, fmt.Errorf("unsupported protocol %q", protocol))
			continue
		}
		backends, err := parse.GenBackendsForPort(cr, svcSpecPortObj)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// 零后端：跳过渲染，避免生成非法的空 upstream 导致整个 nginx reload 失败。
		// 若该 listener 之前已有配置则保留，避免滚动更新瞬间把端口摘掉。
		if len(backends) == 0 {
			// 保留整份旧配置并报告未完成，避免 controller 提前释放旧证书。
			errs = append(errs, fmt.Errorf("listener %s has no backends; retaining previous configuration", listenerPort))
			continue
		}

		switch protocol {
		case "tcp", "udp":
			streamConfig := StreamConfig{
				Protocol:     protocol,
				ListenerPort: listenerPort,
				SoKeepalive:  protocol == "tcp",
				Backends:     backends,
			}
			c, err := RenderStreamFile(dir, prefix, streamConfig, &existsFiles)
			if err != nil {
				errs = append(errs, err)
			}
			changed = changed || c

		case "http":
			httpConfig := HttpConfig{
				ListenerPort: listenerPort,
				Backends:     backends,
			}
			c, err := RenderHttpFile(dir, prefix, httpConfig, &existsFiles)
			if err != nil {
				errs = append(errs, err)
			}
			changed = changed || c

		case "https":
			domains, c, err := RenderSecrets(na.nginxHome+TLSFilePath, prefix, cr, &existsFiles)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			changed = changed || c
			httpConfig := HttpConfig{
				ListenerPort: listenerPort,
				Backends:     backends,
			}
			c, err = RenderHttpsFile(dir, prefix, httpConfig, domains, &existsFiles)
			if err != nil {
				errs = append(errs, err)
			}
			changed = changed || c
		}
	}

	if len(errs) != 0 {
		return resp, errors.Join(errs...)
	}

	// 清理本 Service 自己的过期文件（仅限本前缀）
	removed, err := na.gcServiceFiles(prefix, existsFiles)
	if err != nil {
		return resp, err
	}
	changed = changed || removed

	// 无文件变更则跳过 reload，避免频繁 reload。
	// cr.ForceReload 由 controller 在重试时置位（上一轮该 Service 同步失败），
	// 此时即便磁盘配置无变更也强制 reload，避免运行中的 nginx 永久停在旧配置。
	if changed || cr.ForceReload {
		if err := na.reload(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return resp, err
	}
	return resp, na.commitTransaction()
}

func (na *Agent) Delete(ref sync.ServiceRef) (retErr error) {
	na.mu.Lock()
	defer na.mu.Unlock()

	// 记录操作耗时
	timer := metrics.ObserveOperationDuration("delete")
	defer timer.ObserveDuration()

	svcKey := ref.Namespace + "/" + ref.Name
	prefix := svcPrefix(ref.Namespace, ref.Name)
	if err := na.beginTransaction(prefix); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, na.recoverTransaction())
		}
	}()
	// keep 传 nil：本 Service 的全部文件都删。
	// Delete 不频繁，reload 保持无条件（无需 force 标识，也规避 cleanup 路径的跳过陷阱）。
	if _, err := na.gcServiceFiles(prefix, nil); err != nil {
		metrics.RecordOperation("delete", "failure", svcKey)
		return err
	}
	if err := na.reload(); err != nil {
		metrics.RecordOperation("delete", "failure", svcKey)
		return err
	}

	if err := na.commitTransaction(); err != nil {
		return err
	}
	metrics.RecordOperation("delete", "success", svcKey)
	na.updateServicesMetric()
	return nil
}

func (na *Agent) List() ([]sync.ServiceRef, error) {
	set := map[sync.ServiceRef]struct{}{}
	for _, dir := range na.streamAndHttpDirs() {
		files, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			parts := strings.Split(file.Name(), "_")
			if len(parts) < 2 {
				continue
			}
			set[sync.ServiceRef{Namespace: parts[0], Name: parts[1]}] = struct{}{}
		}
	}

	refs := make([]sync.ServiceRef, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	return refs, nil
}

// updateServicesMetric 更新当前管理的 Service 数量指标
func (na *Agent) updateServicesMetric() {
	refs, err := na.List()
	if err != nil {
		// 记录失败但不中断，metrics 更新是辅助功能
		return
	}
	metrics.SetServicesManaged(len(refs))
}

// gcServiceFiles 删除 stream/http/tls 目录下命中 prefix 且不在 keep 中的文件，
// 返回是否真的删除了文件（供 Apply 判断是否需要 reload）。
func (na *Agent) gcServiceFiles(prefix string, keep []string) (bool, error) {
	var errs []error
	removed := false
	for _, dir := range na.managedDirs() {
		files, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasPrefix(file.Name(), prefix) {
				continue
			}
			full := dir + file.Name()
			if slices.Contains(keep, full) {
				continue
			}
			if err := os.Remove(full); err != nil {
				errs = append(errs, err)
			} else {
				removed = true
			}
		}
	}
	return removed, errors.Join(errs...)
}

// L4Ports 返回当前所有 stream 监听端口（用于四层连接数统计），实现 agent.Forwarder 接口。
// 从 conf.d/stream/ 目录解析文件名提取端口。
func (na *Agent) L4Ports() []int {
	streamDir := na.nginxHome + ConfigStreamPath
	files, err := os.ReadDir(streamDir)
	if err != nil {
		return nil
	}

	portSet := make(map[int]struct{})
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".conf") {
			continue
		}
		// 文件名格式: <ns>_<name>_<proto>_<port>.conf 或 <ns>_<name>_<proto>_<port>_upstream.conf
		// 需提取 <port> 部分
		name := strings.TrimSuffix(file.Name(), ".conf")
		name = strings.TrimSuffix(name, "_upstream") // 去掉 _upstream 后缀

		parts := strings.Split(name, "_")
		if len(parts) < 4 {
			continue
		}
		// parts: [ns, name, proto, port, ...]
		portStr := parts[3]
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		portSet[port] = struct{}{}
	}

	ports := make([]int, 0, len(portSet))
	for p := range portSet {
		ports = append(ports, p)
	}
	return ports
}

// UDPPorts 返回当前所有 UDP stream 监听端口（用于 UDP 套接字统计），实现 agent.Forwarder 接口。
// 从 conf.d/stream/ 目录解析文件名，提取 protocol="udp" 的端口。
func (na *Agent) UDPPorts() []int {
	streamDir := na.nginxHome + ConfigStreamPath
	files, err := os.ReadDir(streamDir)
	if err != nil {
		return nil
	}

	portSet := make(map[int]struct{})
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".conf") {
			continue
		}
		// 文件名格式: <ns>_<name>_<proto>_<port>.conf 或 <ns>_<name>_<proto>_<port>_upstream.conf
		// 需提取 <proto> 和 <port> 部分，仅统计 proto="udp"
		name := strings.TrimSuffix(file.Name(), ".conf")
		name = strings.TrimSuffix(name, "_upstream") // 去掉 _upstream 后缀

		parts := strings.Split(name, "_")
		if len(parts) < 4 {
			continue
		}
		// parts: [ns, name, proto, port, ...]
		proto := parts[2]
		if proto != "udp" {
			continue // 仅统计 UDP
		}
		portStr := parts[3]
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		portSet[port] = struct{}{}
	}

	ports := make([]int, 0, len(portSet))
	for p := range portSet {
		ports = append(ports, p)
	}
	return ports
}

// 校验和 reload 必须与写文件、启动 master 使用同一个 prefix 和主配置。
func (na *Agent) command(args ...string) error {
	timeout := na.commandTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args = append(args, "-p", na.nginxHome, "-c", filepath.Join(na.nginxHome, "nginx.conf"))
	cmd := exec.CommandContext(ctx, na.nginx, args...)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("nginx %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
