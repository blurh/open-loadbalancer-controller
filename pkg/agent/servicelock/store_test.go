package servicelock

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
)

func TestStoreRejectsCorruptOrConflictingState(t *testing.T) {
	for _, data := range []string{
		`{`,
		`{"version":2}`,
		`{"version":1,"reservations":[{"service":{"namespace":"","name":"web"},"class":"nginx"}]}`,
		`{"version":1,"reservations":[{"service":{"namespace":"default","name":"web"},"class":"nginx","ports":[{"protocol":"tcp","port":0}]}]}`,
		`{"version":1,"reservations":[{"service":{"namespace":"default","name":"web"},"class":"nginx"},{"service":{"namespace":"default","name":"web"},"class":"nginx"}]}`,
		`{"version":1,"reservations":[{"service":{"namespace":"default","name":"a"},"class":"nginx","ports":[{"protocol":"tcp","port":80}]},{"service":{"namespace":"default","name":"b"},"class":"lvs","ports":[{"protocol":"tcp","port":80}]}]}`,
	} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "service-locks.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); err == nil {
				t.Fatal("invalid state must fail startup")
			}
		})
	}
}

func TestStoreFailedCommitOrReleaseKeepsPersistedReservation(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "release"}[release], func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			ref := sync.ServiceRef{Namespace: "default", Name: "web"}
			if err := s.Reserve(ref, "nginx", []Port{{Protocol: "tcp", Number: 80}}); err != nil {
				t.Fatal(err)
			}
			before := s.Reservations()
			// 写临时文件或 rename 失败都不能推进内存状态。
			s.path = filepath.Join(dir, "missing", "service-locks.json")
			if release {
				err = s.Release(ref, "nginx")
			} else {
				err = s.Commit(ref, "nginx", nil)
			}
			if err == nil || !reflect.DeepEqual(before, s.Reservations()) {
				t.Fatal("failed write lost reservation")
			}
			restored, err := Open(dir)
			if err != nil || !reflect.DeepEqual(before, restored.Reservations()) {
				t.Fatalf("persisted state changed: %v", err)
			}
		})
	}
}

func TestStoreRestoreConflictIsAtomic(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	port := []Port{{Protocol: "tcp", Number: 80}}
	err = s.Restore([]Reservation{
		{Service: sync.ServiceRef{Namespace: "default", Name: "one"}, Class: "nginx", Ports: port},
		{Service: sync.ServiceRef{Namespace: "default", Name: "two"}, Class: "nginx", Ports: port},
	})
	if !errors.Is(err, ErrConflict) || len(s.Reservations()) != 0 {
		t.Fatalf("restore must fail atomically: %v", err)
	}
}

func TestRequestedPortsUsesRenderingProtocolAndValidatesPorts(t *testing.T) {
	http := "http"
	for _, tt := range []struct {
		name       string
		annotation string
		port       corev1.ServicePort
		want       Port
		invalid    bool
	}{
		{name: "annotation overrides appProtocol", annotation: "UDP:53", port: corev1.ServicePort{Port: 53, AppProtocol: &http}, want: Port{"udp", 53}},
		{name: "reject incompatible backend transport", port: corev1.ServicePort{Port: 80, Protocol: corev1.ProtocolUDP, AppProtocol: &http}, invalid: true},
		{name: "UDP without appProtocol", port: corev1.ServicePort{Port: 53, Protocol: corev1.ProtocolUDP}, want: Port{"udp", 53}},
		{name: "default TCP", port: corev1.ServicePort{Port: 80}, want: Port{"tcp", 80}},
		{name: "zero port", port: corev1.ServicePort{Port: 0}, invalid: true},
		{name: "large port", port: corev1.ServicePort{Port: 65536}, invalid: true},
		{name: "unsupported transport", port: corev1.ServicePort{Port: 80, Protocol: corev1.ProtocolSCTP}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cr := sync.ControllerResources{}
			cr.Service.Annotations = map[string]string{apis.SvcAnnProtolPort: tt.annotation}
			cr.Service.Spec.Ports = []corev1.ServicePort{tt.port}
			ports, err := RequestedPorts(cr)
			if tt.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("want validation error, got %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(ports, []Port{tt.want}) {
				t.Fatalf("ports=%v error=%v", ports, err)
			}
		})
	}
}
