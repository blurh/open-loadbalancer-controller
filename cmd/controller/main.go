package main

import (
	"github.com/blurh/open-loadbalancer-controller/pkg/config"
	"github.com/blurh/open-loadbalancer-controller/pkg/controllers"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

func main() {
	c := config.NewConfig()

	syncer := sync.NewSyncer(c.GetSyncType())

	controller := controllers.NewServiceController(c, syncer)
	controller.Start()
}
