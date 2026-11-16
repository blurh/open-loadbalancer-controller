package controllers

import (
	"testing"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestServiceWatchAcceptsAnnotationCRUDWithoutGenerationChange(t *testing.T) {
	p := serviceEventPredicate()
	svc := lbService("default", "web", "10.0.0.1:8080")
	if !p.Create(event.CreateEvent{Object: svc}) || !p.Delete(event.DeleteEvent{Object: svc}) {
		t.Fatal("Service create/delete ignored")
	}
	for _, key := range []string{apis.SvcAnnInstances, apis.SvcAnnClass, apis.SvcAnnProtolPort, apis.SvcAnnNetworkType, apis.SvcAnnNodeNetworkType, apis.SvcAnnCertSecrets} {
		for _, value := range []string{"first", "second", ""} {
			old := svc.DeepCopy()
			if value == "" {
				delete(svc.Annotations, key)
			} else {
				svc.Annotations[key] = value
			}
			if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: svc}) {
				t.Fatalf("ignored %s=%q with unchanged generation", key, value)
			}
		}
	}
	old := svc.DeepCopy()
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: svc}) {
		t.Fatal("LB removal ignored")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: svc, ObjectNew: old}) {
		t.Fatal("LB addition ignored")
	}
}

func TestNodeWatchObservesAddressesAndReadiness(t *testing.T) {
	p := nodeEventPredicate()
	old := node("n1", "10.0.0.1")
	newNode := old.DeepCopy()
	newNode.Status.Addresses[0].Address = "10.0.0.2"
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newNode}) {
		t.Fatal("address update ignored")
	}
	newNode = old.DeepCopy()
	newNode.Status.Conditions[0].Status = corev1.ConditionFalse
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newNode}) {
		t.Fatal("readiness update ignored")
	}
	newNode = old.DeepCopy()
	newNode.ResourceVersion = "2"
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newNode}) {
		t.Fatal("irrelevant update triggered sync")
	}
}

func TestForwardingHashPreservesSlicePortAssociation(t *testing.T) {
	a, b := endpointSlice("default", "web"), endpointSlice("default", "web")
	b.Name = "web-second"
	b.Ports[0].Port = ptrInt32(9090)
	b.Endpoints[0].Addresses = []string{"10.244.1.11"}
	cr := sync.ControllerResources{Service: *lbService("default", "web", "agent"), EndpointSlices: []discovery.EndpointSlice{*a, *b}}
	first, err := forwardingHash(cr)
	if err != nil {
		t.Fatal(err)
	}
	cr.EndpointSlices[0], cr.EndpointSlices[1] = cr.EndpointSlices[1], cr.EndpointSlices[0]
	cr.EndpointSlices[0].ResourceVersion = "changed"
	second, _ := forwardingHash(cr)
	if first != second {
		t.Fatal("slice order/metadata changed hash")
	}
	cr.EndpointSlices[0].Ports[0].Port = ptrInt32(9091)
	third, _ := forwardingHash(cr)
	if third == first {
		t.Fatal("backend port change ignored")
	}
}
