// Package servicelock 保存单个 agent 上 Service 的监听端口归属。
package servicelock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	ErrConflict = errors.New("service port already owned")
	ErrInvalid  = errors.New("invalid service lock request")
)

// Port 按传输协议区分端口；HTTP、HTTPS、TCP 都占用 tcp。
type Port struct {
	Protocol string `json:"protocol"`
	Number   int    `json:"port"`
}

type Reservation struct {
	Service sync.ServiceRef `json:"service"`
	Class   string          `json:"class"`
	Ports   []Port          `json:"ports"`
	Pending bool            `json:"pending"`
}

type snapshot struct {
	Version      int           `json:"version"`
	Reservations []Reservation `json:"reservations"`
}

// Store 的读写由 Agent 的互斥锁串行化；一个目录只供一个 agent 进程使用。
type Store struct {
	path    string
	entries []Reservation
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "service-locks.json")}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var state snapshot
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("read service locks: %w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported service lock version: %d", state.Version)
	}
	if err := validate(state.Reservations); err != nil {
		return nil, fmt.Errorf("read service locks: %w", err)
	}
	s.entries = state.Reservations
	return s, nil
}

func ValidateService(ref sync.ServiceRef) error {
	if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Label(ref.Name)) != 0 {
		return fmt.Errorf("%w: invalid namespace/name %q/%q", ErrInvalid, ref.Namespace, ref.Name)
	}
	return nil
}

func (s *Store) Reservations() []Reservation {
	entries := slices.Clone(s.entries)
	for i := range entries {
		entries[i].Ports = slices.Clone(entries[i].Ports)
	}
	return entries
}

func (s *Store) Pending(ref sync.ServiceRef, class string) bool {
	for _, entry := range s.entries {
		if entry.Service == ref && entry.Class == class {
			return entry.Pending
		}
	}
	return false
}

// Restore 合并磁盘配置的归属，不丢弃上次失败操作留下的预留。
// 老版本没有操作日志，恢复的配置保守地视为尚待 reload。
func (s *Store) Restore(observed []Reservation) error {
	entries := s.Reservations()
	for _, entry := range observed {
		entry.Pending = true
		entries = merge(entries, entry)
	}
	return s.save(entries)
}

// Reserve 在修改转发器之前持久化旧、新端口的并集，整批检查成功才写入。
func (s *Store) Reserve(ref sync.ServiceRef, class string, ports []Port) error {
	for _, owner := range s.entries {
		if owner.Service == ref {
			continue
		}
		for _, port := range ports {
			if slices.Contains(owner.Ports, port) {
				return fmt.Errorf("%w: %s/%d held by %s/%s (class %s), requested by %s/%s (class %s)",
					ErrConflict, port.Protocol, port.Number, owner.Service.Namespace, owner.Service.Name, owner.Class,
					ref.Namespace, ref.Name, class)
			}
		}
	}
	return s.save(merge(s.Reservations(), Reservation{Service: ref, Class: class, Ports: ports, Pending: true}))
}

// Commit 只在 Apply/reload 全部成功后收缩锁集合。
func (s *Store) Commit(ref sync.ServiceRef, class string, ports []Port) error {
	entries := s.Reservations()
	for i := range entries {
		if entries[i].Service == ref && entries[i].Class == class {
			entries[i].Ports = slices.Clone(ports)
			entries[i].Pending = false
			return s.save(entries)
		}
	}
	return fmt.Errorf("cannot commit unreserved service %s/%s (%s)", ref.Namespace, ref.Name, class)
}

// Release 只能在 Delete/reload 成功后调用，持久化失败时仍保留锁。
func (s *Store) Release(ref sync.ServiceRef, class string) error {
	entries := slices.DeleteFunc(s.Reservations(), func(entry Reservation) bool {
		return entry.Service == ref && entry.Class == class
	})
	return s.save(entries)
}

func merge(entries []Reservation, entry Reservation) []Reservation {
	for i := range entries {
		if entries[i].Service == entry.Service && entries[i].Class == entry.Class {
			entries[i].Ports = append(entries[i].Ports, entry.Ports...)
			entries[i].Pending = entries[i].Pending || entry.Pending
			return entries
		}
	}
	entry.Ports = slices.Clone(entry.Ports)
	return append(entries, entry)
}

func validate(entries []Reservation) error {
	owners := make(map[Port]Reservation)
	type key struct {
		ref   sync.ServiceRef
		class string
	}
	seen := make(map[key]bool)
	for _, entry := range entries {
		if err := ValidateService(entry.Service); err != nil {
			return err
		}
		if len(validation.IsDNS1123Label(entry.Class)) != 0 {
			return fmt.Errorf("%w: invalid class %q", ErrInvalid, entry.Class)
		}
		k := key{entry.Service, entry.Class}
		if seen[k] {
			return fmt.Errorf("duplicate service lock: %s/%s (%s)", entry.Service.Namespace, entry.Service.Name, entry.Class)
		}
		seen[k] = true
		for _, port := range entry.Ports {
			if (port.Protocol != "tcp" && port.Protocol != "udp") || port.Number < 1 || port.Number > 65535 {
				return fmt.Errorf("%w: port %s/%d", ErrInvalid, port.Protocol, port.Number)
			}
			if owner, ok := owners[port]; ok && owner.Service != entry.Service {
				return fmt.Errorf("%w: %s/%d held by %s/%s (class %s), requested by %s/%s (class %s)",
					ErrConflict, port.Protocol, port.Number, owner.Service.Namespace, owner.Service.Name, owner.Class,
					entry.Service.Namespace, entry.Service.Name, entry.Class)
			}
			owners[port] = entry
		}
	}
	return nil
}

func (s *Store) save(entries []Reservation) error {
	if err := validate(entries); err != nil {
		return err
	}
	for i := range entries {
		slices.SortFunc(entries[i].Ports, func(a, b Port) int {
			if a.Protocol < b.Protocol {
				return -1
			}
			if a.Protocol > b.Protocol {
				return 1
			}
			return a.Number - b.Number
		})
		entries[i].Ports = slices.Compact(entries[i].Ports)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Service.Namespace != b.Service.Namespace {
			return a.Service.Namespace < b.Service.Namespace
		}
		if a.Service.Name != b.Service.Name {
			return a.Service.Name < b.Service.Name
		}
		return a.Class < b.Class
	})
	data, err := json.MarshalIndent(snapshot{Version: 1, Reservations: entries}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".service-locks-*")
	if err != nil {
		return fmt.Errorf("persist service locks: %w", err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.path)
	}
	if err != nil {
		return fmt.Errorf("persist service locks: %w", err)
	}
	s.entries = entries
	return nil
}
