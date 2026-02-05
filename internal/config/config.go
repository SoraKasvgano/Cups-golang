package config

import (
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ListenAddr      string
	TLSEnabled      bool
	TLSOnly         bool
	TLSCertPath     string
	TLSKeyPath      string
	TLSAutoGenerate bool
	DataDir         string
	DBPath          string
	SpoolDir        string
	OutputDir       string
	ConfDir         string
	PPDDir          string
	ServerName      string
}

func Load() Config {
	dataDir := getenv("CUPS_DATA_DIR", filepath.Join("data"))
	dbPath := getenv("CUPS_DB_PATH", filepath.Join(dataDir, "cupsgolang.db"))
	spoolDir := getenv("CUPS_SPOOL_DIR", filepath.Join(dataDir, "spool"))
	outputDir := getenv("CUPS_OUTPUT_DIR", filepath.Join(dataDir, "printed"))
	confDir := getenv("CUPS_CONF_DIR", filepath.Join(dataDir, "conf"))
	ppdDir := getenv("CUPS_PPD_DIR", filepath.Join(dataDir, "ppd"))

	return Config{
		ListenAddr:      getenv("CUPS_LISTEN_ADDR", ":631"),
		TLSEnabled:      getenvBool("CUPS_TLS_ENABLED", true),
		TLSOnly:         getenvBool("CUPS_TLS_ONLY", false),
		TLSCertPath:     getenv("CUPS_TLS_CERT", filepath.Join(confDir, "cupsd.crt")),
		TLSKeyPath:      getenv("CUPS_TLS_KEY", filepath.Join(confDir, "cupsd.key")),
		TLSAutoGenerate: getenvBool("CUPS_TLS_AUTOGEN", true),
		DataDir:         dataDir,
		DBPath:          dbPath,
		SpoolDir:        spoolDir,
		OutputDir:       outputDir,
		ConfDir:         confDir,
		PPDDir:          ppdDir,
		ServerName:      getenv("CUPS_SERVER_NAME", "CUPS-Golang"),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		v = strings.ToLower(strings.TrimSpace(v))
		return v == "1" || v == "true" || v == "yes" || v == "on"
	}
	return fallback
}
