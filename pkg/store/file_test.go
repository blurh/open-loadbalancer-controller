package store

import (
	"path/filepath"
	"sort"
	"testing"
)

func newTestFileStore(t *testing.T) *FileStore {
	t.Helper()
	fs := &FileStore{
		path:  filepath.Join(t.TempDir(), "loadbalance.store"),
		cache: map[string]string{},
	}
	if err := fs.load(); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFileStore_SetGetExistsDel(t *testing.T) {
	fs := newTestFileStore(t)

	if ok, _ := fs.Exists("default/web"); ok {
		t.Fatal("key should not exist yet")
	}

	if err := fs.Set("default/web", "10.0.0.1,10.0.0.2"); err != nil {
		t.Fatal(err)
	}

	v, err := fs.Get("default/web")
	if err != nil {
		t.Fatal(err)
	}
	if v != "10.0.0.1,10.0.0.2" {
		t.Fatalf("got %q", v)
	}

	if ok, _ := fs.Exists("default/web"); !ok {
		t.Fatal("key should exist after Set")
	}

	if err := fs.Del("default/web"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fs.Exists("default/web"); ok {
		t.Fatal("key should be gone after Del")
	}
	if _, err := fs.Get("default/web"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loadbalance.store")

	fs1 := &FileStore{path: path, cache: map[string]string{}}
	if err := fs1.load(); err != nil {
		t.Fatal(err)
	}
	if err := fs1.Set("a/b", "ip1"); err != nil {
		t.Fatal(err)
	}
	if err := fs1.Set("c/d", "ip2"); err != nil {
		t.Fatal(err)
	}

	// 用同一路径新建一个 store，应能从磁盘恢复
	fs2 := &FileStore{path: path, cache: map[string]string{}}
	if err := fs2.load(); err != nil {
		t.Fatal(err)
	}
	v, err := fs2.Get("a/b")
	if err != nil || v != "ip1" {
		t.Fatalf("got %q, err %v", v, err)
	}

	keys := fs2.Keys()
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a/b" || keys[1] != "c/d" {
		t.Fatalf("unexpected keys %v", keys)
	}
}

func TestFileStore_DelMissingIsNoop(t *testing.T) {
	fs := newTestFileStore(t)
	if err := fs.Del("does/not-exist"); err != nil {
		t.Fatalf("Del on missing key should be no-op, got %v", err)
	}
}
