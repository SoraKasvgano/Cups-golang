package config

import "embed"

//go:embed defaults/mime.types defaults/mime.convs
var defaultConf embed.FS
