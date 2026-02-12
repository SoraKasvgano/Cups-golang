package server

import (
	"cupsgolang/internal/config"
	"cupsgolang/internal/store"
)

var appCfg config.Config
var appSt *store.Store

func SetAppConfig(cfg config.Config) {
	appCfg = cfg
}

func SetAppStore(st *store.Store) {
	appSt = st
}

func appConfig() config.Config {
	return appCfg
}

func appStore() *store.Store {
	return appSt
}
