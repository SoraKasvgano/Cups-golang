package config

import (
	"bufio"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr                 string
	ListenHTTP                 []string
	ListenHTTPS                []string
	TLSEnabled                 bool
	TLSOnly                    bool
	TLSCertPath                string
	TLSKeyPath                 string
	TLSAutoGenerate            bool
	DataDir                    string
	DBPath                     string
	SpoolDir                   string
	OutputDir                  string
	ConfDir                    string
	PPDDir                     string
	ServerName                 string
	ServerAlias                []string
	DefaultPolicy              string
	WebInterface               bool
	MaxRequestSize             int64
	MaxLogSize                 int64
	LogLevel                   string
	AccessLogLevel             string
	PageLogFormat              string
	ErrorLogPath               string
	AccessLogPath              string
	PageLogPath                string
	ErrorPolicy                string
	DefaultAuthType            string
	MaxJobTime                 int
	MaxEvents                  int
	MaxLeaseDuration           int
	DefaultLeaseDuration       int
	MaxSubscriptions           int
	MaxSubscriptionsPerJob     int
	MaxSubscriptionsPerPrinter int
	MaxSubscriptionsPerUser    int
	BrowseLocal                bool
	BrowseLocalProtocols       []string
	DNSSDHostName              string
	DNSSDComputerName          string
	DeviceBackendsDir          string
	FilterDir                  string
	ClientConfDir              string
	JobRetryLimit              int
	JobRetryInterval           int
	MultipleOperationTimeout   int
	ServerBin                  string
	RequestRoot                string
	StateDir                   string
	CacheDir                   string
	DocumentRoot               string
}

type configOverrides struct {
	dataDirLocked     bool
	confDirLocked     bool
	dbPath            bool
	spoolDir          bool
	outputDir         bool
	ppdDir            bool
	listenHTTPLocked  bool
	listenHTTPSLocked bool
	listenAddrLocked  bool
	tlsCertLocked     bool
	tlsKeyLocked      bool
	serverNameLocked  bool
}

func Load() Config {
	overrides := configOverrides{}

	dataDir := getenv("CUPS_DATA_DIR", filepath.Join("data"))
	confDir := getenv("CUPS_CONF_DIR", filepath.Join(dataDir, "conf"))

	cfg := Config{
		ListenAddr:                 getenv("CUPS_LISTEN_ADDR", ":631"),
		TLSEnabled:                 getenvBool("CUPS_TLS_ENABLED", true),
		TLSOnly:                    getenvBool("CUPS_TLS_ONLY", false),
		TLSCertPath:                getenv("CUPS_TLS_CERT", filepath.Join(confDir, "cupsd.crt")),
		TLSKeyPath:                 getenv("CUPS_TLS_KEY", filepath.Join(confDir, "cupsd.key")),
		TLSAutoGenerate:            getenvBool("CUPS_TLS_AUTOGEN", true),
		DataDir:                    dataDir,
		DBPath:                     getenv("CUPS_DB_PATH", filepath.Join(dataDir, "cupsgolang.db")),
		SpoolDir:                   getenv("CUPS_SPOOL_DIR", filepath.Join(dataDir, "spool")),
		OutputDir:                  getenv("CUPS_OUTPUT_DIR", filepath.Join(dataDir, "printed")),
		ConfDir:                    confDir,
		PPDDir:                     getenv("CUPS_PPD_DIR", filepath.Join(dataDir, "ppd")),
		ServerName:                 getenv("CUPS_SERVER_NAME", "CUPS-Golang"),
		WebInterface:               true,
		LogLevel:                   "info",
		AccessLogLevel:             "actions",
		PageLogFormat:              "%p %u %j %T %P %C %{job-billing} %{job-originating-host-name} %{job-name} %{media} %{sides}",
		BrowseLocal:                true,
		BrowseLocalProtocols:       []string{"dnssd"},
		MultipleOperationTimeout:   900,
		MaxJobTime:                 3 * 60 * 60,
		MaxEvents:                  100,
		MaxLeaseDuration:           0,
		DefaultLeaseDuration:       24 * 60 * 60,
		MaxSubscriptions:           100,
		MaxSubscriptionsPerJob:     0,
		MaxSubscriptionsPerPrinter: 0,
		MaxSubscriptionsPerUser:    0,
	}

	markEnvOverrides(&overrides)
	applyCupsFilesConf(&cfg, &overrides)
	applyCupsdConf(&cfg, &overrides)
	applyEnvOverrides(&cfg, &overrides)
	applyDerivedDefaults(&cfg, &overrides)

	if cfg.TLSOnly {
		cfg.TLSEnabled = true
	}

	if len(cfg.ListenHTTP) == 0 && len(cfg.ListenHTTPS) == 0 && strings.TrimSpace(cfg.ListenAddr) != "" {
		cfg.ListenHTTP = []string{ensurePort(strings.TrimSpace(cfg.ListenAddr), "631")}
	}
	if cfg.ClientConfDir == "" {
		cfg.ClientConfDir = cfg.ConfDir
	}
	if cfg.ServerBin != "" {
		if cfg.DeviceBackendsDir == "" {
			cfg.DeviceBackendsDir = filepath.Join(cfg.ServerBin, "backend")
		}
		if cfg.FilterDir == "" {
			cfg.FilterDir = filepath.Join(cfg.ServerBin, "filter")
		}
	}
	return cfg
}

func markEnvOverrides(overrides *configOverrides) {
	if overrides == nil {
		return
	}
	if _, ok := os.LookupEnv("CUPS_DATA_DIR"); ok {
		overrides.dataDirLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_CONF_DIR"); ok {
		overrides.confDirLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_DB_PATH"); ok {
		overrides.dbPath = true
	}
	if _, ok := os.LookupEnv("CUPS_SPOOL_DIR"); ok {
		overrides.spoolDir = true
	}
	if _, ok := os.LookupEnv("CUPS_OUTPUT_DIR"); ok {
		overrides.outputDir = true
	}
	if _, ok := os.LookupEnv("CUPS_PPD_DIR"); ok {
		overrides.ppdDir = true
	}
	if _, ok := os.LookupEnv("CUPS_LISTEN_ADDR"); ok {
		overrides.listenAddrLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_LISTEN_HTTP"); ok {
		overrides.listenHTTPLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_LISTEN_HTTPS"); ok {
		overrides.listenHTTPSLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_TLS_CERT"); ok {
		overrides.tlsCertLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_TLS_KEY"); ok {
		overrides.tlsKeyLocked = true
	}
	if _, ok := os.LookupEnv("CUPS_SERVER_NAME"); ok {
		overrides.serverNameLocked = true
	}
}

func applyEnvOverrides(cfg *Config, overrides *configOverrides) {
	if cfg == nil {
		return
	}
	if v, ok := os.LookupEnv("CUPS_DATA_DIR"); ok {
		cfg.DataDir = v
		if overrides != nil {
			overrides.dataDirLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_CONF_DIR"); ok {
		cfg.ConfDir = v
		if overrides != nil {
			overrides.confDirLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_DB_PATH"); ok {
		cfg.DBPath = v
		if overrides != nil {
			overrides.dbPath = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_SPOOL_DIR"); ok {
		cfg.SpoolDir = v
		cfg.RequestRoot = v
		if overrides != nil {
			overrides.spoolDir = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_OUTPUT_DIR"); ok {
		cfg.OutputDir = v
		if overrides != nil {
			overrides.outputDir = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_PPD_DIR"); ok {
		cfg.PPDDir = v
		if overrides != nil {
			overrides.ppdDir = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
		if overrides != nil {
			overrides.listenAddrLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_LISTEN_HTTP"); ok {
		cfg.ListenHTTP = splitListenList(v)
		if overrides != nil {
			overrides.listenHTTPLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_LISTEN_HTTPS"); ok {
		cfg.ListenHTTPS = splitListenList(v)
		if overrides != nil {
			overrides.listenHTTPSLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_TLS_CERT"); ok {
		cfg.TLSCertPath = v
		if overrides != nil {
			overrides.tlsCertLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_TLS_KEY"); ok {
		cfg.TLSKeyPath = v
		if overrides != nil {
			overrides.tlsKeyLocked = true
		}
	}
	if v, ok := os.LookupEnv("CUPS_ACCESS_LOG_LEVEL"); ok {
		cfg.AccessLogLevel = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("CUPS_PAGE_LOG_FORMAT"); ok {
		cfg.PageLogFormat = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("CUPS_MULTIPLE_OPERATION_TIMEOUT"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MultipleOperationTimeout = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_EVENTS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxEvents = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_LEASE_DURATION"); ok {
		if n, ok := parseTimeSeconds(v); ok {
			cfg.MaxLeaseDuration = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_DEFAULT_LEASE_DURATION"); ok {
		if n, ok := parseTimeSeconds(v); ok {
			cfg.DefaultLeaseDuration = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_SUBSCRIPTIONS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxSubscriptions = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_SUBSCRIPTIONS_PER_JOB"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxSubscriptionsPerJob = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_SUBSCRIPTIONS_PER_PRINTER"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxSubscriptionsPerPrinter = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_SUBSCRIPTIONS_PER_USER"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxSubscriptionsPerUser = n
		}
	}
	if v, ok := os.LookupEnv("CUPS_MAX_JOB_TIME"); ok {
		if n, ok := parseTimeSeconds(v); ok {
			cfg.MaxJobTime = n
		}
	}
	cfg.TLSEnabled = getenvBool("CUPS_TLS_ENABLED", cfg.TLSEnabled)
	cfg.TLSOnly = getenvBool("CUPS_TLS_ONLY", cfg.TLSOnly)
	cfg.TLSAutoGenerate = getenvBool("CUPS_TLS_AUTOGEN", cfg.TLSAutoGenerate)
	if v, ok := os.LookupEnv("CUPS_SERVER_NAME"); ok {
		cfg.ServerName = v
		if overrides != nil {
			overrides.serverNameLocked = true
		}
	}
}

func applyDerivedDefaults(cfg *Config, overrides *configOverrides) {
	if cfg == nil {
		return
	}
	if overrides == nil || !overrides.dbPath {
		cfg.DBPath = filepath.Join(cfg.DataDir, "cupsgolang.db")
	}
	if overrides == nil || !overrides.spoolDir {
		cfg.SpoolDir = filepath.Join(cfg.DataDir, "spool")
		cfg.RequestRoot = cfg.SpoolDir
	}
	if overrides == nil || !overrides.outputDir {
		cfg.OutputDir = filepath.Join(cfg.DataDir, "printed")
	}
	if overrides == nil || !overrides.ppdDir {
		cfg.PPDDir = filepath.Join(cfg.DataDir, "ppd")
	}
	if overrides == nil || !overrides.tlsCertLocked {
		cfg.TLSCertPath = filepath.Join(cfg.ConfDir, "cupsd.crt")
	}
	if overrides == nil || !overrides.tlsKeyLocked {
		cfg.TLSKeyPath = filepath.Join(cfg.ConfDir, "cupsd.key")
	}
	if cfg.RequestRoot == "" {
		cfg.RequestRoot = cfg.SpoolDir
	}
}

func applyCupsFilesConf(cfg *Config, overrides *configOverrides) {
	if cfg == nil {
		return
	}
	origConf := cfg.ConfDir
	parseCupsFilesConf(filepath.Join(cfg.ConfDir, "cups-files.conf"), cfg, overrides)
	if overrides != nil && overrides.confDirLocked {
		return
	}
	if cfg.ConfDir != origConf {
		parseCupsFilesConf(filepath.Join(cfg.ConfDir, "cups-files.conf"), cfg, overrides)
	}
}

func parseCupsFilesConf(path string, cfg *Config, overrides *configOverrides) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}
		keyToken := parts[0]
		key := strings.ToLower(keyToken)
		raw := strings.TrimSpace(line[len(keyToken):])
		value := unquoteValue(strings.TrimSpace(raw))
		if strings.Contains(value, "@") {
			continue
		}
		switch key {
		case "serverroot":
			if overrides != nil && overrides.confDirLocked {
				continue
			}
			if value != "" {
				cfg.ConfDir = resolvePath(cfg.ConfDir, value)
			}
		case "datadir":
			if overrides != nil && overrides.dataDirLocked {
				continue
			}
			if value != "" {
				cfg.DataDir = resolvePath(cfg.ConfDir, value)
			}
		case "requestroot":
			if overrides != nil && overrides.spoolDir {
				continue
			}
			if value != "" {
				cfg.SpoolDir = resolvePath(cfg.ConfDir, value)
				cfg.RequestRoot = cfg.SpoolDir
				if overrides != nil {
					overrides.spoolDir = true
				}
			}
		case "statedir":
			if value != "" {
				cfg.StateDir = resolvePath(cfg.ConfDir, value)
			}
		case "cachedir":
			if value != "" {
				cfg.CacheDir = resolvePath(cfg.ConfDir, value)
			}
		case "documentroot":
			if value != "" {
				cfg.DocumentRoot = resolvePath(cfg.ConfDir, value)
			}
		case "serverbin":
			if value != "" {
				cfg.ServerBin = resolvePath(cfg.ConfDir, value)
				if cfg.DeviceBackendsDir == "" {
					cfg.DeviceBackendsDir = filepath.Join(cfg.ServerBin, "backend")
				}
				if cfg.FilterDir == "" {
					cfg.FilterDir = filepath.Join(cfg.ServerBin, "filter")
				}
			}
		case "accesslog":
			if value == "" {
				cfg.AccessLogPath = ""
				continue
			}
			cfg.AccessLogPath = resolvePath(cfg.ConfDir, value)
		case "errorlog":
			if value == "" {
				cfg.ErrorLogPath = ""
				continue
			}
			cfg.ErrorLogPath = resolvePath(cfg.ConfDir, value)
		case "pagelog":
			if value == "" {
				cfg.PageLogPath = ""
				continue
			}
			cfg.PageLogPath = resolvePath(cfg.ConfDir, value)
		}
	}
}

func applyCupsdConf(cfg *Config, overrides *configOverrides) {
	if cfg == nil {
		return
	}
	parseCupsdConf(filepath.Join(cfg.ConfDir, "cupsd.conf"), cfg, overrides)
}

func parseCupsdConf(path string, cfg *Config, overrides *configOverrides) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	blockDepth := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "<") {
			if strings.HasPrefix(line, "</") {
				if blockDepth > 0 {
					blockDepth--
				}
			} else {
				blockDepth++
			}
			continue
		}
		if blockDepth > 0 {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(parts[0])
		raw := strings.TrimSpace(line[len(key):])
		value := unquoteValue(strings.TrimSpace(raw))
		if strings.Contains(value, "@") {
			continue
		}
		switch key {
		case "listen":
			if overrides != nil && overrides.listenHTTPLocked {
				continue
			}
			lower := strings.ToLower(value)
			if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "ipps://") || strings.HasPrefix(lower, "ssl://") {
				if overrides == nil || !overrides.listenHTTPSLocked {
					addListen(cfg, value, true)
				}
			} else {
				addListen(cfg, value, false)
			}
		case "port":
			if overrides != nil && overrides.listenHTTPLocked {
				continue
			}
			for _, p := range parts[1:] {
				if p == "" {
					continue
				}
				addListen(cfg, ":"+p, false)
			}
		case "servername":
			if overrides != nil && overrides.serverNameLocked {
				continue
			}
			cfg.ServerName = value
		case "serveralias":
			cfg.ServerAlias = appendUniqueList(cfg.ServerAlias, parts[1:]...)
		case "defaultpolicy":
			cfg.DefaultPolicy = value
		case "webinterface":
			if v, ok := parseBool(value); ok {
				cfg.WebInterface = v
			}
		case "maxrequestsize":
			if v, ok := parseSize(value); ok {
				cfg.MaxRequestSize = v
			}
		case "limitrequestbody":
			if v, ok := parseSize(value); ok {
				cfg.MaxRequestSize = v
			}
		case "maxlogsize":
			if v, ok := parseSize(value); ok {
				cfg.MaxLogSize = v
			}
		case "loglevel":
			cfg.LogLevel = value
		case "accessloglevel":
			cfg.AccessLogLevel = strings.TrimSpace(value)
		case "pagelogformat":
			cfg.PageLogFormat = value
		case "errorpolicy":
			cfg.ErrorPolicy = value
		case "defaultauthtype":
			cfg.DefaultAuthType = value
		case "browsing":
			if v, ok := parseBool(value); ok {
				cfg.BrowseLocal = v
			}
		case "browselocalprotocols":
			cfg.BrowseLocalProtocols = parseBrowseLocalProtocols(parts[1:])
		case "dnssdhostname":
			cfg.DNSSDHostName = value
		case "dnssdcomputername":
			cfg.DNSSDComputerName = value
		case "defaultencryption":
			applyDefaultEncryption(cfg, value)
		case "jobretrylimit":
			if n, ok := parseInt(value); ok {
				cfg.JobRetryLimit = n
			}
		case "jobretryinterval":
			if n, ok := parseInt(value); ok {
				cfg.JobRetryInterval = n
			}
		case "multipleoperationtimeout":
			if n, ok := parseInt(value); ok {
				cfg.MultipleOperationTimeout = n
			}
		case "maxjobtime":
			if n, ok := parseTimeSeconds(value); ok {
				cfg.MaxJobTime = n
			}
		case "maxevents":
			if n, ok := parseInt(value); ok {
				cfg.MaxEvents = n
			}
		case "maxleaseduration":
			if n, ok := parseTimeSeconds(value); ok {
				cfg.MaxLeaseDuration = n
			}
		case "defaultleaseduration":
			if n, ok := parseTimeSeconds(value); ok {
				cfg.DefaultLeaseDuration = n
			}
		case "maxsubscriptions":
			if n, ok := parseInt(value); ok {
				cfg.MaxSubscriptions = n
			}
		case "maxsubscriptionsperjob":
			if n, ok := parseInt(value); ok {
				cfg.MaxSubscriptionsPerJob = n
			}
		case "maxsubscriptionsperprinter":
			if n, ok := parseInt(value); ok {
				cfg.MaxSubscriptionsPerPrinter = n
			}
		case "maxsubscriptionsperuser":
			if n, ok := parseInt(value); ok {
				cfg.MaxSubscriptionsPerUser = n
			}
		}
	}
}

func applyDefaultEncryption(cfg *Config, value string) {
	if cfg == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "never", "off", "no":
		cfg.TLSEnabled = false
		cfg.TLSOnly = false
	case "required", "always":
		cfg.TLSEnabled = true
		cfg.TLSOnly = true
	case "ifrequested", "on", "yes", "true":
		cfg.TLSEnabled = true
		cfg.TLSOnly = false
	}
}

func addListen(cfg *Config, addr string, tls bool) {
	if cfg == nil {
		return
	}
	normalized := normalizeListenAddr(addr)
	if normalized == "" {
		return
	}
	if tls {
		cfg.ListenHTTPS = appendUnique(cfg.ListenHTTPS, normalized)
		return
	}
	cfg.ListenHTTP = appendUnique(cfg.ListenHTTP, normalized)
}

func normalizeListenAddr(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	if strings.Contains(v, "@") {
		return ""
	}
	low := strings.ToLower(v)
	if strings.HasPrefix(low, "unix:") || strings.HasPrefix(low, "/") {
		return ""
	}
	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil {
			if u.Host != "" {
				v = u.Host
			} else if u.Path != "" {
				v = u.Path
			}
		}
	}
	if idx := strings.Index(v, "/"); idx >= 0 {
		v = v[:idx]
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return ensurePort(v, "631")
}

func ensurePort(addr string, defaultPort string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "[") {
		if _, _, err := net.SplitHostPort(addr); err == nil {
			return addr
		}
		if strings.HasSuffix(addr, "]") {
			return addr + ":" + defaultPort
		}
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if port == "" {
			port = defaultPort
		}
		return net.JoinHostPort(host, port)
	}
	if strings.Count(addr, ":") > 1 {
		return net.JoinHostPort(addr, defaultPort)
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return net.JoinHostPort(addr, defaultPort)
}

func appendUnique(list []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return list
	}
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}

func parseBrowseLocalProtocols(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.EqualFold(value, "none") {
			return nil
		}
		out = appendUnique(out, value)
	}
	return out
}

func appendUniqueList(list []string, values ...string) []string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		list = appendUnique(list, v)
	}
	return list
}

func splitListenList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\r', '\n':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		addr := normalizeListenAddr(p)
		if addr != "" {
			out = appendUnique(out, addr)
		}
	}
	return out
}

func resolvePath(root, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.EqualFold(value, "syslog") {
		return value
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(root, value)
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func parseSize(value string) (int64, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	mult := int64(1)
	last := v[len(v)-1]
	switch last {
	case 'k', 'K':
		mult = 1024
		v = v[:len(v)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		v = v[:len(v)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		v = v[:len(v)-1]
	}
	num, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	if num < 0 {
		return 0, false
	}
	return int64(num * float64(mult)), true
}

func parseInt(value string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseTimeSeconds(value string) (int, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	mult := 1
	last := v[len(v)-1]
	switch last {
	case 's', 'S':
		mult = 1
		v = v[:len(v)-1]
	case 'm', 'M':
		mult = 60
		v = v[:len(v)-1]
	case 'h', 'H':
		mult = 60 * 60
		v = v[:len(v)-1]
	case 'd', 'D':
		mult = 24 * 60 * 60
		v = v[:len(v)-1]
	case 'w', 'W':
		mult = 7 * 24 * 60 * 60
		v = v[:len(v)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

func unquoteValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) ||
			(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
			value = value[1 : len(value)-1]
		}
	}
	return strings.TrimSpace(value)
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
