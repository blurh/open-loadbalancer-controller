package lvs

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// resolveVIPs 默认发现本机内网地址；显式 VIP 覆盖发现结果，不合并两个集合。
func resolveVIPs(configured string, discover func() ([]string, error)) ([]string, error) {
	if strings.TrimSpace(configured) == "" {
		return discover()
	}
	var addresses []string
	for _, value := range strings.Split(configured, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() {
			return nil, fmt.Errorf("OPENLB_LVS_VIPS requires unicast IPv4 addresses: %q", value)
		}
		addresses = append(addresses, ip.String())
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("OPENLB_LVS_VIPS contains no IPv4 addresses")
	}
	sort.Strings(addresses)
	return slices.Compact(addresses), nil
}

type localInterface struct {
	name      string
	flags     net.Flags
	addresses []net.IP
}

// 只读取本机接口和路由，无需联网、DNS 或通过公网地址探测出口。
func discoverPrivateIPv4() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list local interfaces: %w", err)
	}
	var local []localInterface
	for _, intf := range interfaces {
		if !eligibleInterface(intf.Name, intf.Flags) {
			continue
		}
		addresses, err := intf.Addrs()
		if err != nil {
			return nil, fmt.Errorf("read addresses on %s: %w", intf.Name, err)
		}
		entry := localInterface{name: intf.Name, flags: intf.Flags}
		for _, addr := range addresses {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil {
				entry.addresses = append(entry.addresses, ip)
			}
		}
		local = append(local, entry)
	}
	// 纯内网机器可能没有默认路由；此时仍可选取唯一的内网地址。
	routes, _ := os.ReadFile("/proc/net/route")
	return selectPrivateIPv4(local, defaultRouteMetrics(string(routes)))
}

func eligibleInterface(name string, flags net.Flags) bool {
	if flags&net.FlagUp == 0 || flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
		return false
	}
	// 排除常见容器网桥、CNI 链路和 kube-proxy 的虚拟 VIP 接口。
	for _, prefix := range []string{"docker", "br-", "veth", "cni", "flannel", "cali", "virbr", "podman", "kube-ipvs"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func defaultRouteMetrics(data string) map[string]uint64 {
	metrics := make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x1 == 0 || flags&0x200 != 0 {
			continue
		} // RTF_UP / RTF_REJECT
		metric, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			continue
		}
		if old, ok := metrics[fields[0]]; !ok || metric < old {
			metrics[fields[0]] = metric
		}
	}
	return metrics
}

func selectPrivateIPv4(interfaces []localInterface, metrics map[string]uint64) ([]string, error) {
	var all, preferred []string
	var bestMetric uint64
	hasDefault := false
	for _, intf := range interfaces {
		if !eligibleInterface(intf.name, intf.flags) {
			continue
		}
		for _, ip := range intf.addresses {
			if ip.To4() == nil || !ip.IsPrivate() || !ip.IsGlobalUnicast() {
				continue
			}
			all = append(all, ip.String())
			if metric, ok := metrics[intf.name]; ok {
				if !hasDefault || metric < bestMetric {
					preferred = nil
					bestMetric, hasDefault = metric, true
				}
				if metric == bestMetric {
					preferred = append(preferred, ip.String())
				}
			}
		}
	}
	if hasDefault {
		all = preferred
	}
	sort.Strings(all)
	all = slices.Compact(all)
	switch len(all) {
	case 1:
		return all, nil
	case 0:
		return nil, fmt.Errorf("no usable local private IPv4 for LVS; check host networking or set OPENLB_LVS_VIPS")
	default:
		return nil, fmt.Errorf("ambiguous local private IPv4 addresses for LVS (%s); set OPENLB_LVS_VIPS to select listeners", strings.Join(all, ", "))
	}
}
