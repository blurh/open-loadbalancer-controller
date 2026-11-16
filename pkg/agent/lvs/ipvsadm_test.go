package lvs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseIPVSRules(t *testing.T) {
	rules, err := parseSave(`# ipvsadm -S -n
-A -t 192.0.2.10:443 -s rr
-a -t 192.0.2.10:443 -r 10.0.0.2:8443 -m -w 1
-A -u 192.0.2.10:53 -s wrr
-a -u 192.0.2.10:53 -r 10.0.0.3:53 -g -w 2
-A -f 100 -s rr
-a -f 100 -r 10.0.0.4:0 -g -w 1
-A -t [2001:db8::1]:80 -s rr
-a -t [2001:db8::1]:80 -r [2001:db8::2]:80 -g -w 1
-A -t 192.0.2.10:80 -s rr -p 300
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("unexpected rule count: %d", len(rules))
	}
	if rules[listener{"tcp", "192.0.2.10:443"}].Destinations[0] != (destination{"10.0.0.2:8443", "nat", 1}) {
		t.Fatal("incorrect TCP parsing")
	}
	if !rules[listener{"tcp", "192.0.2.10:80"}].Unsupported {
		t.Fatal("persistent service options were silently discarded")
	}
	for _, bad := range []string{"garbage", "-A -t 192.0.2.10:443 -s", "-a -t 192.0.2.10:443 -r 10.0.0.2:443 -m -w 1", "-A -t 192.0.2.10:443"} {
		if _, err := parseSave(bad); err == nil {
			t.Fatalf("accepted malformed save output %q", bad)
		}
	}
}

func TestCommandDriverArgumentsAndTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ipvsadm")
	logPath := filepath.Join(dir, "args")
	t.Setenv("OPENLB_TEST_LVS_ARGS", logPath)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OPENLB_TEST_LVS_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := newCommandDriver(bin, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rule := virtualService{listener: listener{"udp", "192.0.2.10:53"}, Scheduler: "rr"}
	if err := d.Service("-A", rule); err != nil {
		t.Fatal(err)
	}
	if err := d.Destination("-a", rule.listener, destination{"10.0.0.2:5353", "nat", 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.Service("-D", rule); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "-A -u 192.0.2.10:53 -s rr\n-a -u 192.0.2.10:53 -r 10.0.0.2:5353 -m -w 1\n-D -u 192.0.2.10:53\n-S -n\n"
	if string(data) != want {
		t.Fatalf("arguments: %s", data)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho permission-denied >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(); err == nil || !strings.Contains(err.Error(), "permission-denied") {
		t.Fatalf("missing diagnostic: %v", err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d.timeout = 20 * time.Millisecond
	if _, err := d.List(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command did not time out: %v", err)
	}
}
