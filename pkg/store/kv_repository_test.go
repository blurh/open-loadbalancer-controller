package store

import (
	"context"
	"testing"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

func TestKVRepositoryFailedPersistenceKeepsPreviousState(t *testing.T) {
	ctx := context.Background()
	kv := newTestFileStore(t)
	s := NewKVRepository(kv)
	svc := stateTestService("a")
	state, err := s.Ensure(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) {
		status.Instances = []statev1.InstanceState{{Address: "agent:8080", Phase: statev1.InstanceApplied}}
	})
	if err != nil {
		t.Fatal(err)
	}
	oldPath := kv.path
	kv.path = oldPath + "/missing"
	if _, err := s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances = nil }); err == nil {
		t.Fatal("expected disk failure")
	}
	loaded, err := s.Load(ctx, types.NamespacedName{Namespace: "a", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Status.Instances) != 1 {
		t.Fatal("failed persistence advanced memory state")
	}
	kv.path = oldPath
	state, err = s.UpdateStatus(ctx, state, func(status *statev1.ServiceStateStatus) { status.Instances[0].Phase = statev1.InstanceCleaned })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, state); err != nil {
		t.Fatal(err)
	}
	if len(kv.Keys()) != 0 {
		t.Fatal("state not removed")
	}
}
