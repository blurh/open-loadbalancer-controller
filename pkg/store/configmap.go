package store

import (
	"context"
	"maps"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/retry"

	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
)

const (
	RepositoryCMName = "openlb-controller-repository"
)

type ConfigMapStore struct {
	client *kubernetes.Clientset
	cache  map[string]string
	Locker sync.Mutex
}

func newConfigMapStore() Storer {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(1000, 1000)

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	c, err := clientset.CoreV1().ConfigMaps(utils.GetMyNamespace()).Get(context.TODO(), RepositoryCMName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			c, err = clientset.CoreV1().ConfigMaps(utils.GetMyNamespace()).Create(context.TODO(), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: RepositoryCMName,
				},
				Data: map[string]string{},
			}, metav1.CreateOptions{})
			if err != nil {
				panic(err)
			}
		} else {
			panic(err)
		}
	}
	cache := make(map[string]string)
	maps.Copy(cache, c.Data)

	return &ConfigMapStore{
		client: clientset,
		cache:  cache,
		Locker: sync.Mutex{},
	}
}

func (c *ConfigMapStore) Set(k, v string) error {
	c.Locker.Lock()
	defer c.Locker.Unlock()

	ctx := context.TODO()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		repoCM, err := c.client.CoreV1().ConfigMaps(utils.GetMyNamespace()).Get(ctx, RepositoryCMName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if repoCM.Data == nil {
			repoCM.Data = map[string]string{}
		}
		if old, ok := repoCM.Data[k]; ok && old == v {
			return nil
		}
		repoCM.Data[k] = v
		_, err = c.client.CoreV1().ConfigMaps(utils.GetMyNamespace()).Update(ctx, repoCM, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}

	c.cache[k] = v
	return nil
}

func (c *ConfigMapStore) Get(k string) (string, error) {
	c.Locker.Lock()
	defer c.Locker.Unlock()

	v, ok := c.cache[k]
	if !ok {
		return "", ErrNotFound
	}

	return v, nil
}

func (c *ConfigMapStore) Del(k string) error {
	c.Locker.Lock()
	defer c.Locker.Unlock()

	ctx := context.TODO()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		repoCM, err := c.client.CoreV1().ConfigMaps(utils.GetMyNamespace()).Get(ctx, RepositoryCMName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if _, ok := repoCM.Data[k]; !ok {
			return nil
		}
		delete(repoCM.Data, k)
		_, err = c.client.CoreV1().ConfigMaps(utils.GetMyNamespace()).Update(ctx, repoCM, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}

	delete(c.cache, k)
	return nil
}

func (c *ConfigMapStore) Exists(k string) (bool, error) {
	c.Locker.Lock()
	defer c.Locker.Unlock()

	_, ok := c.cache[k]
	return ok, nil
}

func (c *ConfigMapStore) Keys() []string {
	c.Locker.Lock()
	defer c.Locker.Unlock()

	keys := make([]string, 0, len(c.cache))
	for k := range c.cache {
		keys = append(keys, k)
	}
	return keys
}
