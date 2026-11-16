package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent"
	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

// fakeAgent 捕获 server 解码后交给 Agent 的入参，用于验证推送链路。
type fakeAgent struct {
	gotApply  *sync.ControllerResources
	gotDelete *sync.ServiceRef
	applyErr  error
}

func (f *fakeAgent) PreflightCheck() error { return nil }
func (f *fakeAgent) Apply(cr sync.ControllerResources) (sync.RespBody, error) {
	f.gotApply = &cr
	return sync.RespBody{InternalIPs: []string{"1.2.3.4"}}, f.applyErr
}
func (f *fakeAgent) Delete(ref sync.ServiceRef) error {
	f.gotDelete = &ref
	return nil
}
func (f *fakeAgent) List() ([]sync.ServiceRef, error)                               { return nil, nil }
func (f *fakeAgent) ListListeners() (map[sync.ServiceRef][]servicelock.Port, error) { return nil, nil }
func (f *fakeAgent) L4Ports() []int                                                 { return nil }
func (f *fakeAgent) UDPPorts() []int                                                { return nil }

func TestSyncErrorStatus(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{"invalid class", agent.ErrInvalidClass, http.StatusBadRequest},
		{"invalid lock", servicelock.ErrInvalid, http.StatusBadRequest},
		{"port conflict", servicelock.ErrConflict, http.StatusConflict},
		{"apply failure", errors.New("reload failed"), http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(&fakeAgent{applyErr: tt.err}, "")
			rec := httptest.NewRecorder()
			srv.handleSync(rec, httptest.NewRequest(http.MethodPost, sync.RouteSyncPath, bytes.NewBufferString(`{}`)))
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestSyncDispatchesByServiceAnnotation(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_TYPE", "lvs")
	a, err := agent.NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(a, "")
	for _, tt := range []struct {
		class  string
		status int
	}{
		{"nginx", http.StatusOK},
		{"lvs", http.StatusInternalServerError},
		{"unknown", http.StatusBadRequest},
	} {
		cr := sync.ControllerResources{Service: corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "web", Annotations: map[string]string{apis.SvcAnnClass: tt.class},
		}}}
		cr.Nodes = []corev1.Node{{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}
		data, err := json.Marshal(cr)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		srv.handleSync(rec, httptest.NewRequest(http.MethodPost, sync.RouteSyncPath, bytes.NewReader(data)))
		if rec.Code != tt.status {
			t.Fatalf("class %q: status = %d, want %d: %s", tt.class, rec.Code, tt.status, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	srv.handleCleanup(rec, httptest.NewRequest(http.MethodPost, sync.RouteCleanupPath, bytes.NewBufferString(`{"namespace":"default","name":"web"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSyncPortConflictReportsOwnerAndCleanupReleases(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_AGENT_STATE_DIR", t.TempDir())
	a, err := agent.NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(a, "")
	post := func(name string, want int) string {
		t.Helper()
		cr := sync.ControllerResources{Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}}},
		}}
		cr.Nodes = []corev1.Node{{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}
		data, err := json.Marshal(cr)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		srv.handleSync(rec, httptest.NewRequest(http.MethodPost, sync.RouteSyncPath, bytes.NewReader(data)))
		if rec.Code != want {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
		}
		return rec.Body.String()
	}
	post("owner", http.StatusOK)
	body := post("other", http.StatusConflict)
	if !strings.Contains(body, "tcp/80 held by default/owner (class nginx)") {
		t.Fatalf("missing conflict details: %s", body)
	}
	rec := httptest.NewRecorder()
	srv.handleCleanup(rec, httptest.NewRequest(http.MethodPost, sync.RouteCleanupPath, bytes.NewBufferString(`{"namespace":"default","name":"owner"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup: %d %s", rec.Code, rec.Body.String())
	}
	post("other", http.StatusOK)
}
func newTestServer(fa *fakeAgent) *httptest.Server {
	srv := New(fa, "")
	return httptest.NewServer(srv.Handler())
}

func TestSyncCarriesService(t *testing.T) {
	fa := &fakeAgent{}
	ts := newTestServer(fa)
	defer ts.Close()

	client := sync.NewSyncer("http")
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "default",
				Name:        "web",
				Annotations: map[string]string{"kubernetes.io/openlb.network-type": "nodeport"},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}},
			},
		},
	}

	resp := &sync.RespBody{}
	if err := client.Sync(ts.URL, cr, resp); err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	// agent 端确实收到了 Service
	if fa.gotApply == nil {
		t.Fatal("agent.Apply was not called")
	}
	if got := fa.gotApply.Service.Name; got != "web" {
		t.Errorf("Service.Name = %q, want web", got)
	}
	if got := fa.gotApply.Service.Namespace; got != "default" {
		t.Errorf("Service.Namespace = %q, want default", got)
	}
	if n := len(fa.gotApply.Service.Spec.Ports); n != 1 || fa.gotApply.Service.Spec.Ports[0].NodePort != 30080 {
		t.Errorf("Service ports not carried: %+v", fa.gotApply.Service.Spec.Ports)
	}
	if fa.gotApply.Service.Annotations["kubernetes.io/openlb.network-type"] != "nodeport" {
		t.Error("Service annotations not carried")
	}

	// RespBody 正常回传
	if len(resp.InternalIPs) != 1 || resp.InternalIPs[0] != "1.2.3.4" {
		t.Errorf("RespBody not returned: %+v", resp)
	}
}

func TestCleanupCarriesServiceRef(t *testing.T) {
	fa := &fakeAgent{}
	ts := newTestServer(fa)
	defer ts.Close()

	client := sync.NewSyncer("http")
	if err := client.Clean(ts.URL, sync.ServiceRef{Namespace: "default", Name: "web"}); err != nil {
		t.Fatalf("Clean error: %v", err)
	}

	if fa.gotDelete == nil {
		t.Fatal("agent.Delete was not called")
	}
	if fa.gotDelete.Name != "web" || fa.gotDelete.Namespace != "default" {
		t.Errorf("ServiceRef not carried: %+v", fa.gotDelete)
	}
}

func TestHandlerRejectsNonPOST(t *testing.T) {
	f := &fakeAgent{}
	for _, path := range []string{sync.RouteSyncPath, sync.RouteCleanupPath} {
		rec := httptest.NewRecorder()
		New(f, "").Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusMethodNotAllowed || f.gotApply != nil || f.gotDelete != nil {
			t.Fatalf("unexpected mutation/status: %d", rec.Code)
		}
	}
}
