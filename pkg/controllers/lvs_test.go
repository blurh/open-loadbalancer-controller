package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/internal/testutil"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestLVSServiceIgnoresL7WithoutFetchingSecrets(t *testing.T) {
	for _, configuration := range []string{"https", "appProtocol", "certificate", "none"} {
		t.Run(configuration, func(t *testing.T) {
			svc := lbService("default", "web", "10.0.0.1")
			svc.Annotations[SvcAnnClass] = "lvs"
			svc.Spec.Ports[0].Protocol = corev1.ProtocolUDP
			switch configuration {
			case "https":
				svc.Annotations[SvcAnnProtolPort] = "https:80"
			case "appProtocol":
				app := "kubernetes.io/h2c"
				svc.Spec.Ports[0].AppProtocol = &app
			case "certificate":
				svc.Annotations[SvcAnnCertSecrets] = "nonexistent-secret"
			}
			r, base, repo := newTestReconciler([]client.Object{svc})
			events := record.NewFakeRecorder(20)
			r.Eventer = events
			r.Syncer = &checkingSyncer{mockSyncer: base, beforeSync: func(_ string, cr sync.ControllerResources) {
				if len(cr.Secrets) != 0 || cr.Service.Annotations[SvcAnnCertSecrets] != "" || cr.Service.Annotations[SvcAnnProtolPort] != "" || cr.Service.Spec.Ports[0].AppProtocol != nil {
					t.Fatal("L7 configuration sent to LVS")
				}
				if cr.Service.Spec.Ports[0].Protocol != corev1.ProtocolUDP {
					t.Fatal("app protocol overrode Service transport")
				}
			}}
			if err := reconcileCertificateService(t, r, svc); err != nil {
				t.Fatal(err)
			}
			found := false
			for len(events.Events) > 0 {
				if strings.Contains(<-events.Events, "Warning L7ConfigurationIgnored") {
					found = true
				}
			}
			if found != (configuration != "none") {
				t.Fatalf("ignored event=%v for %s", found, configuration)
			}
			state, err := repo.Load(context.Background(), client.ObjectKeyFromObject(svc))
			if err != nil || len(state.Status.CertificateRefs) != 0 {
				t.Fatalf("LVS acquired certificate references: %+v %v", state, err)
			}
		})
	}
}

func TestNginxToLVSReleasesCertificatesOnlyAfterAllAgentsSucceed(t *testing.T) {
	ctx := context.Background()
	svc := lbService("default", "web", "10.0.0.1:8080,10.0.0.2:8080")
	svc.Annotations[SvcAnnCertSecrets] = "tls"
	secret := testutil.TLSSecret(t, "default", "tls", "example.com")
	r, base, _ := newTestReconciler([]client.Object{svc, secret})
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, secret, true)
	current := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(svc), current); err != nil {
		t.Fatal(err)
	}
	current.Annotations[SvcAnnClass] = "lvs"
	current.Annotations[SvcAnnProtolPort] = "https:80"
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	base.syncErrors = map[string]error{"10.0.0.2:8080": errors.New("agent unreachable")}
	if err := reconcileCertificateService(t, r, svc); err == nil {
		t.Fatal("expected partial failure")
	}
	assertSecretProtected(t, r.Client, secret, true)
	base.syncErrors = nil
	if err := reconcileCertificateService(t, r, svc); err != nil {
		t.Fatal(err)
	}
	assertSecretProtected(t, r.Client, secret, false)
}
