package store

import "errors"

var (
	ErrNotFound = errors.New("not found")
)

type Storer interface {
	Set(k, v string) error
	Get(k string) (string, error)
	Exists(k string) (bool, error)
	Del(k string) error
	Keys() []string
}

func NewStore(storeType string) Storer {
	switch storeType {
	case "configmap":
		return newConfigMapStore()
	case "file":
		return newFileStore()
	case "mysql":
		// TODO
		return nil
	default:
		panic("unknow store type")
	}
}
