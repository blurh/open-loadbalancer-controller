package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	statev1 "github.com/blurh/open-loadbalancer-controller/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// KVRepository 在显式选择的 file/configmap 后端保存有类型的记录。
// 这些后端不具备 CR 的 namespace 生命周期和 Watch 能力；默认使用 CRDStore。
type KVRepository struct {
	mu sync.Mutex
	kv Storer
}

func NewKVRepository(kv Storer) *KVRepository { return &KVRepository{kv: kv} }

func stateKey(key types.NamespacedName) string {
	return "state-" + base64.RawURLEncoding.EncodeToString([]byte(key.String()))
}

func (s *KVRepository) load(key types.NamespacedName) (*statev1.OpenLBServiceState, error) {
	value, err := s.kv.Get(stateKey(key))
	if errors.Is(err, ErrNotFound) {
		return nil, apierrors.NewNotFound(statev1.GroupVersion.WithResource("openlbservicestates").GroupResource(), key.Name)
	}
	if err != nil {
		return nil, err
	}
	state := &statev1.OpenLBServiceState{}
	if err := json.Unmarshal([]byte(value), state); err != nil {
		return nil, err
	}
	if state.Namespace != key.Namespace || state.Name != key.Name || state.Spec.ServiceRef.Name != key.Name {
		return nil, fmt.Errorf("stored service identity does not match %s", key)
	}
	return state, nil
}

func (s *KVRepository) save(state *statev1.OpenLBServiceState) error {
	canonicalStatus(&state.Status)
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.kv.Set(stateKey(types.NamespacedName{Namespace: state.Namespace, Name: state.Name}), string(data))
}

func (s *KVRepository) Load(ctx context.Context, key types.NamespacedName) (*statev1.OpenLBServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.load(key)
}

func (s *KVRepository) List(ctx context.Context, namespace string) ([]statev1.OpenLBServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var states []statev1.OpenLBServiceState
	for _, key := range s.kv.Keys() {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "state-"))
		if err != nil || !strings.HasPrefix(key, "state-") {
			return nil, fmt.Errorf("invalid state key %q", key)
		}
		ns, name, ok := strings.Cut(string(decoded), "/")
		if !ok {
			return nil, fmt.Errorf("invalid state key %q", key)
		}
		if ns != namespace {
			continue
		}
		state, err := s.load(types.NamespacedName{Namespace: ns, Name: name})
		if err != nil {
			return nil, err
		}
		states = append(states, *state)
	}
	return states, nil
}

func (s *KVRepository) Ensure(ctx context.Context, svc *corev1.Service) (*statev1.OpenLBServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := s.load(types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name})
	if apierrors.IsNotFound(err) {
		state = NewServiceState(svc)
		return state, s.save(state)
	}
	if err != nil {
		return nil, err
	}
	if err := checkIdentity(state, NewServiceState(svc)); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *KVRepository) UpdateStatus(ctx context.Context, expected *statev1.OpenLBServiceState, mutate func(*statev1.ServiceStateStatus)) (*statev1.OpenLBServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := s.load(types.NamespacedName{Namespace: expected.Namespace, Name: expected.Name})
	if err != nil {
		return nil, err
	}
	if err := checkIdentity(state, expected); err != nil {
		return nil, err
	}
	before := state.Status.DeepCopy()
	mutate(&state.Status)
	if statusChanged(*before, state.Status) {
		err = s.save(state)
	}
	return state, err
}

func (s *KVRepository) Remove(ctx context.Context, expected *statev1.OpenLBServiceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: expected.Namespace, Name: expected.Name}
	state, err := s.load(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkIdentity(state, expected); err != nil {
		return err
	}
	if !cleanupComplete(state) {
		return errors.New("instances or certificate references still require cleanup")
	}
	return s.kv.Del(stateKey(key))
}
