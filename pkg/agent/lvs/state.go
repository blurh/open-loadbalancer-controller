package lvs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/servicelock"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

type pendingChange struct {
	Desired []virtualService `json:"desired,omitempty"`
	Owned   []listener       `json:"owned"`
}
type record struct {
	Version int              `json:"version"`
	Service sync.ServiceRef  `json:"service"`
	Applied []virtualService `json:"applied,omitempty"`
	Pending *pendingChange   `json:"pending,omitempty"`
}

func openAgent(d driver, dir string, vips []string, scheduler, mode string) (*Agent, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	a := &Agent{driver: d, dir: dir, vips: vips, scheduler: scheduler, mode: mode, records: make(map[sync.ServiceRef]record)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	owners := make(map[listener]sync.ServiceRef)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var state record
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("read LVS state %s: %w", entry.Name(), err)
		}
		if err := validateRecord(state); err != nil {
			return nil, err
		}
		if entry.Name() != stateFilename(state.Service) {
			return nil, fmt.Errorf("LVS state filename does not match service identity: %s", entry.Name())
		}
		for _, key := range ownedListeners(state) {
			if previous, exists := owners[key]; exists {
				return nil, fmt.Errorf("conflicting LVS ownership: %s/%s and %s/%s", previous.Namespace, previous.Name, state.Service.Namespace, state.Service.Name)
			}
			owners[key] = state.Service
		}
		a.records[state.Service] = state
	}
	// 不在启动时重放旧期望；controller 会带最新资源重试。pending 的并集用于恢复端口锁。
	return a, nil
}

func stateFilename(ref sync.ServiceRef) string { return ref.Namespace + "_" + ref.Name + ".json" }
func ownedListeners(state record) []listener {
	var keys []listener
	for _, rule := range state.Applied {
		keys = append(keys, rule.listener)
	}
	if state.Pending != nil {
		keys = append(keys, state.Pending.Owned...)
	}
	sortListeners(keys)
	return slices.Compact(keys)
}
func validateRecord(state record) error {
	if state.Version != 1 {
		return fmt.Errorf("unsupported LVS state version %d", state.Version)
	}
	if err := servicelock.ValidateService(state.Service); err != nil {
		return err
	}
	for _, key := range ownedListeners(state) {
		if key.Protocol != "tcp" && key.Protocol != "udp" {
			return fmt.Errorf("invalid LVS protocol %q", key.Protocol)
		}
		if normalized, err := ipv4Endpoint(key.Address); err != nil || normalized != key.Address {
			return fmt.Errorf("invalid persisted LVS listener %q", key.Address)
		}
	}
	rules := append([]virtualService{}, state.Applied...)
	if state.Pending != nil {
		rules = append(rules, state.Pending.Desired...)
	}
	for _, rule := range rules {
		if !slices.Contains(ownedListeners(state), rule.listener) {
			return fmt.Errorf("LVS desired listener missing ownership: %+v", rule.listener)
		}
		if !slices.Contains(schedulers, rule.Scheduler) {
			return fmt.Errorf("invalid persisted LVS scheduler %q", rule.Scheduler)
		}
		for _, dst := range rule.Destinations {
			if _, err := ipv4Endpoint(dst.Address); err != nil {
				return err
			}
			if !slices.Contains([]string{"nat", "dr", "tun"}, dst.Mode) || dst.Weight < 0 || dst.Weight > 65535 {
				return fmt.Errorf("invalid persisted LVS destination %+v", dst)
			}
		}
	}
	return nil
}

func (a *Agent) saveRecord(state record) error {
	if err := validateRecord(state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := filepath.Join(a.dir, stateFilename(state.Service))
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		a.records[state.Service] = state
		return nil
	}
	f, err := os.CreateTemp(a.dir, ".lvs-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err := syncDirectory(a.dir); err != nil {
		return err
	}
	a.records[state.Service] = state
	return nil
}

func (a *Agent) removeRecord(ref sync.ServiceRef) error {
	if err := os.Remove(filepath.Join(a.dir, stateFilename(ref))); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDirectory(a.dir); err != nil {
		return err
	}
	delete(a.records, ref)
	return nil
}
func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
