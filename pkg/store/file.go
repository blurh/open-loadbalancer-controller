package store

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

// FileStore 用一个 JSON 文件持久化 key=value，每次写操作整体落盘。
type FileStore struct {
	path   string
	cache  map[string]string
	Locker sync.Mutex
}

func newFileStore() Storer {
	fs := &FileStore{
		path:  "./loadbalance.store",
		cache: map[string]string{},
	}
	if err := fs.load(); err != nil {
		panic(err)
	}
	return fs
}

func (f *FileStore) load() error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &f.cache); err != nil {
		return err
	}
	if f.cache == nil {
		f.cache = make(map[string]string)
	}
	return nil
}

func (f *FileStore) flush(next map[string]string) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".openlb-store-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), f.path)
	}
	if err != nil {
		return err
	}
	f.cache = next
	return nil
}

func (f *FileStore) Set(k, v string) error {
	f.Locker.Lock()
	defer f.Locker.Unlock()

	if old, ok := f.cache[k]; ok && old == v {
		return nil
	}
	next := maps.Clone(f.cache)
	next[k] = v
	return f.flush(next)
}

func (f *FileStore) Get(k string) (string, error) {
	f.Locker.Lock()
	defer f.Locker.Unlock()

	v, ok := f.cache[k]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *FileStore) Exists(k string) (bool, error) {
	f.Locker.Lock()
	defer f.Locker.Unlock()

	_, ok := f.cache[k]
	return ok, nil
}

func (f *FileStore) Del(k string) error {
	f.Locker.Lock()
	defer f.Locker.Unlock()

	if _, ok := f.cache[k]; !ok {
		return nil
	}
	next := maps.Clone(f.cache)
	delete(next, k)
	return f.flush(next)
}

func (f *FileStore) Keys() []string {
	f.Locker.Lock()
	defer f.Locker.Unlock()

	keys := make([]string, 0, len(f.cache))
	for k := range f.cache {
		keys = append(keys, k)
	}
	return keys
}
