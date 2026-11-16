package nginx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// journal 在修改前保存本 Service 的完整文件集合。进程中断后启动时恢复，
// 防止新 server、旧证书或半写入的私钥混用；目录必须和 nginx 配置一起持久化。
const transactionFile = ".openlb-transaction.json"

type configTransaction struct {
	Prefix string            `json:"prefix"`
	Files  map[string][]byte `json:"files"`
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".openlb-write-*")
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
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (na *Agent) beginTransaction(prefix string) error {
	// 上一次回滚失败时禁止覆盖日志；先恢复，再尝试新的配置。
	if err := na.recoverTransaction(); err != nil {
		return err
	}
	tx := configTransaction{Prefix: prefix, Files: make(map[string][]byte)}
	for _, dir := range na.managedDirs() {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), prefix) {
				continue
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("managed config %s is not a regular file", filepath.Join(dir, entry.Name()))
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(na.nginxHome, path)
			if err != nil {
				return err
			}
			tx.Files[rel] = data
		}
	}
	data, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(na.nginxHome, transactionFile), data)
}

func (na *Agent) commitTransaction() error {
	// GC 的删除也必须落盘，然后才删除恢复日志。
	for _, dir := range na.managedDirs() {
		if err := syncDir(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(na.nginxHome, transactionFile)); err != nil {
		return err
	}
	return syncDir(na.nginxHome)
}

func (na *Agent) recoverTransaction() error {
	data, err := os.ReadFile(filepath.Join(na.nginxHome, transactionFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var tx configTransaction
	if err := json.Unmarshal(data, &tx); err != nil {
		return fmt.Errorf("invalid nginx transaction: %w", err)
	}
	parts := strings.Split(tx.Prefix, "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "" || strings.ContainsAny(tx.Prefix, "/\\") {
		return errors.New("invalid nginx transaction service prefix")
	}
	var keep []string
	for rel := range tx.Files {
		dir := filepath.ToSlash(filepath.Dir(rel)) + "/"
		if (dir != ConfigHttpPath && dir != ConfigStreamPath && dir != TLSFilePath) || filepath.Clean(rel) != rel || !strings.HasPrefix(filepath.Base(rel), tx.Prefix) {
			return errors.New("invalid nginx transaction file path")
		}
		keep = append(keep, filepath.Join(na.nginxHome, rel))
	}
	// 先还原旧文件，再删除本次新增文件；任意一步失败都保留日志供重试。
	for rel, contents := range tx.Files {
		if _, err := writeFileIfChanged(filepath.Join(na.nginxHome, rel), contents); err != nil {
			return fmt.Errorf("restore nginx configuration: %w", err)
		}
	}
	if _, err := na.gcServiceFiles(tx.Prefix, keep); err != nil {
		return err
	}
	return na.commitTransaction()
}
