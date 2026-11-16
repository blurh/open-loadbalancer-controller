package lvs

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
)

func testInterface(name string, flags net.Flags, addresses ...string) localInterface {
	intf := localInterface{name: name, flags: flags}
	for _, address := range addresses {
		intf.addresses = append(intf.addresses, net.ParseIP(address))
	}
	return intf
}

func TestSelectLocalPrivateIPv4(t *testing.T) {
	for _, tc := range []struct {
		name       string
		interfaces []localInterface
		metrics    map[string]uint64
		want       string
		err        string
	}{
		{name: "isolated LAN without default route", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10")}, want: "10.0.0.10"},
		{name: "default route wins over other NIC", interfaces: []localInterface{testInterface("eth1", net.FlagUp, "10.1.0.10"), testInterface("eth0", net.FlagUp, "192.168.1.10")}, metrics: map[string]uint64{"eth0": 100}, want: "192.168.1.10"},
		{name: "lowest metric wins", interfaces: []localInterface{testInterface("eth1", net.FlagUp, "10.1.0.10"), testInterface("eth0", net.FlagUp, "192.168.1.10")}, metrics: map[string]uint64{"eth0": 100, "eth1": 50}, want: "10.1.0.10"},
		{name: "private NIC when default route is public", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "203.0.113.10"), testInterface("eth1", net.FlagUp, "172.16.0.10")}, metrics: map[string]uint64{"eth0": 100}, want: "172.16.0.10"},
		{name: "skip loopback down and containers", interfaces: []localInterface{
			testInterface("lo", net.FlagUp|net.FlagLoopback, "10.0.0.1"),
			testInterface("eth1", 0, "10.1.0.1"),
			testInterface("docker0", net.FlagUp, "172.17.0.1"),
			testInterface("br-deadbeef", net.FlagUp, "172.18.0.1"),
			testInterface("cni0", net.FlagUp, "10.244.0.1"),
			testInterface("veth123", net.FlagUp, "10.245.0.1"),
			testInterface("kube-ipvs0", net.FlagUp, "10.96.0.1"),
			testInterface("tun0", net.FlagUp|net.FlagPointToPoint, "10.10.0.1"),
			testInterface("eth0", net.FlagUp, "127.0.0.1", "169.254.1.1", "0.0.0.0", "224.0.0.1", "fd00::1", "192.168.1.10"),
		}, metrics: map[string]uint64{"docker0": 0}, want: "192.168.1.10"},
		{name: "duplicate address", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10", "10.0.0.10")}, want: "10.0.0.10"},
		{name: "no private IPv4", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "203.0.113.10", "fd00::1")}, err: "no usable local private IPv4"},
		{name: "no NIC", err: "no usable local private IPv4"},
		{name: "ambiguous without route", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10"), testInterface("eth1", net.FlagUp, "10.1.0.10")}, err: "ambiguous"},
		{name: "equal default metrics", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10"), testInterface("eth1", net.FlagUp, "10.1.0.10")}, metrics: map[string]uint64{"eth0": 100, "eth1": 100}, err: "ambiguous"},
		{name: "multiple addresses on primary NIC", interfaces: []localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10", "10.0.0.11")}, metrics: map[string]uint64{"eth0": 100}, err: "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPrivateIPv4(tc.interfaces, tc.metrics)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v, %v", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestDefaultRouteMetrics(t *testing.T) {
	data := `Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 00000000 0100000A 0003 0 0 100 00000000 0 0 0
eth0 00000000 0100000A 0003 0 0 50 00000000 0 0 0
eth1 00000000 0101000A 0001 0 0 200 00000000 0 0 0
eth2 00000000 00000000 0201 0 0 0 00000000 0 0 0
eth3 00000000 00000000 0002 0 0 0 00000000 0 0 0
eth4 00000000 00000000 0003 0 0 0 000000FF 0 0 0
eth5 0001000A 00000000 0003 0 0 0 00FFFFFF 0 0 0
eth6 00000000 00000000 0003 0 0 invalid 00000000 0 0 0
invalid line
`
	if got := defaultRouteMetrics(data); !reflect.DeepEqual(got, map[string]uint64{"eth0": 50, "eth1": 200}) {
		t.Fatalf("unexpected routes: %v", got)
	}
}

func TestExplicitVIPsOverrideDiscovery(t *testing.T) {
	called := false
	discover := func() ([]string, error) { called = true; return nil, errors.New("no host network") }
	got, err := resolveVIPs("192.0.2.10, 10.0.0.10,192.0.2.10", discover)
	if err != nil || called || !reflect.DeepEqual(got, []string{"10.0.0.10", "192.0.2.10"}) {
		t.Fatalf("explicit VIPs: %v, %v, discovered=%v", got, err, called)
	}
	for _, value := range []string{"not-an-ip", "0.0.0.0", "127.0.0.1", "169.254.1.1", "224.0.0.1", "fd00::1", ",,"} {
		if _, err := resolveVIPs(value, discover); err == nil {
			t.Fatalf("accepted invalid VIP %q", value)
		}
	}
	if called {
		t.Fatal("explicit invalid configuration triggered discovery")
	}
	if _, err := resolveVIPs(" ", discover); err == nil || !called {
		t.Fatal("empty configuration did not propagate discovery failure")
	}
}

func TestDiscoveredAddressProgramsLVSAndReturnsInternalIP(t *testing.T) {
	addresses, err := resolveVIPs("", func() ([]string, error) {
		return selectPrivateIPv4([]localInterface{testInterface("eth0", net.FlagUp, "10.0.0.10")}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	a, d := testAgent(t)
	a.vips = addresses
	resp, err := a.Apply(podResources())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.rules[listener{"tcp", "10.0.0.10:443"}]; !ok {
		t.Fatal("discovered address was not used for IPVS listener")
	}
	if !reflect.DeepEqual(resp.InternalIPs, addresses) || len(resp.ExternalIPs) != 0 {
		t.Fatalf("incorrect reported addresses: %+v", resp)
	}
}
