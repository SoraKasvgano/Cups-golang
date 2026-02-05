package server

import "cupsgolang/internal/config"

var appCfg config.Config

func SetAppConfig(cfg config.Config) {
	appCfg = cfg
}

func appConfig() config.Config {
	return appCfg
}
