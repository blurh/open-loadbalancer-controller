package nginx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

func TestListListenersRecoversOnlyManagedFiles(t *testing.T) {
	home := t.TempDir() + "/"
	for dir, files := range map[string][]string{
		ConfigHttpPath:   {"default_web_http_80.conf", "default_web_http_80_upstream.conf", "other_tls_https_443_upstream.conf", "x_y_tcp_443.conf", "x_y_http_0.conf", "x_y_http_65536.conf", "x_y_http_80.conf.bak", "a_b_c.conf", "bad.name_web_http_80.conf"},
		ConfigStreamPath: {"default_dns_udp_53.conf", "other_tcp_tcp_80_upstream.conf", "x_y_https_443.conf", "x_y_tcp_080.conf", "x_y_tcp_80_extra.conf"},
	} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0755); err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if err := os.WriteFile(filepath.Join(home, dir, file), []byte("# config"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := &Agent{nginxHome: home}
	got, err := a.ListListeners()
	want := map[sync.ServiceRef][]servicelock.Port{
		{Namespace: "default", Name: "web"}: {{Protocol: "tcp", Number: 80}},
		{Namespace: "other", Name: "tls"}:   {{Protocol: "tcp", Number: 443}},
		{Namespace: "default", Name: "dns"}: {{Protocol: "udp", Number: 53}},
		{Namespace: "other", Name: "tcp"}:   {{Protocol: "tcp", Number: 80}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners=%v error=%v", got, err)
	}
}

func TestListListenersReportsReadFailure(t *testing.T) {
	home := t.TempDir() + "/"
	if err := os.WriteFile(filepath.Join(home, "conf.d"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{nginxHome: home}
	if _, err := a.ListListeners(); err == nil {
		t.Fatal("read failure must not look like no listeners")
	}
}
