package store

import (
	"context"
	"testing"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
)

func TestCertificateReferencesNamespaceListAndCleanupGuard(t *testing.T) {
	for _, backend := range []string{"crd", "file"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var repository Repository
			if backend == "crd" {
				c := stateTestClient(t)
				repository = NewCRDStore(c, c)
			} else {
				repository = NewKVRepository(newTestFileStore(t))
			}
			for _, ns := range []string{"a", "b"} {
				state, err := repository.Ensure(ctx, stateTestService(ns))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repository.UpdateStatus(ctx, state, func(s *statev1.ServiceStateStatus) {
					s.CertificateRefs = []statev1.SecretReference{{Name: "tls", UID: "secret-uid"}}
				}); err != nil {
					t.Fatal(err)
				}
			}
			states, err := repository.List(ctx, "a")
			if err != nil || len(states) != 1 || states[0].Namespace != "a" {
				t.Fatalf("namespace list: %+v, %v", states, err)
			}
			if err := repository.Remove(ctx, &states[0]); err == nil {
				t.Fatal("removed state with outstanding certificate references")
			}
			copy := states[0].DeepCopy()
			copy.Status.CertificateRefs[0].UID = "changed"
			if states[0].Status.CertificateRefs[0].UID != "secret-uid" {
				t.Fatal("DeepCopy aliases certificate references")
			}
			state, err := repository.UpdateStatus(ctx, &states[0], func(s *statev1.ServiceStateStatus) { s.CertificateRefs = nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.Remove(ctx, state); err != nil {
				t.Fatal(err)
			}
		})
	}
}
