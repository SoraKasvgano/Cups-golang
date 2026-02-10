package cupsclient

import (
	"bufio"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type clientSettings struct {
	host               string
	port               int
	useTLS             bool
	user               string
	password           string
	insecureSkipVerify bool
}

type clientConf struct {
	serverName    string
	encryption    string
	user          string
	validateCerts *bool
}

func loadClientSettings() clientSettings {
	conf := loadClientConf()
	host, port, tlsFromServer := parseServer(conf.serverName)
	useTLS := tlsFromServer
	if enc := strings.ToLower(strings.TrimSpace(conf.encryption)); enc != "" {
		switch enc {
		case "never":
			useTLS = false
		case "required", "always":
			useTLS = true
		default:
			// ifrequested keeps current useTLS
		}
	}
	if host == "" {
		host = "localhost"
	}
	if port == 0 {
		port = defaultIPPPort()
	}
	user := strings.TrimSpace(conf.user)
	if user == "" {
		user = defaultUser()
	}
	password := os.Getenv("CUPS_PASSWORD")
	insecure := false
	if conf.validateCerts != nil && !*conf.validateCerts {
		insecure = true
	}
	if insecureEnv, ok := parseBoolEnv("CUPS_IPP_INSECURE"); ok {
		insecure = insecureEnv
	}
	return clientSettings{
		host:               host,
		port:               port,
		useTLS:             useTLS,
		user:               user,
		password:           password,
		insecureSkipVerify: insecure,
	}
}

func loadClientConf() clientConf {
	conf := clientConf{}
	if override := os.Getenv("CUPS_CLIENT_CONF"); strings.TrimSpace(override) != "" {
		readClientConf(override, &conf)
	} else {
		systemPath := filepath.Join(defaultClientConfDir(), "client.conf")
		readClientConf(systemPath, &conf)
		if userPath := filepath.Join(userClientConfDir(), "client.conf"); userPath != systemPath {
			readClientConf(userPath, &conf)
		}
	}
	if v := os.Getenv("CUPS_SERVER"); strings.TrimSpace(v) != "" {
		conf.serverName = v
	}
	if v := os.Getenv("CUPS_ENCRYPTION"); strings.TrimSpace(v) != "" {
		conf.encryption = v
	}
	if v := os.Getenv("CUPS_USER"); strings.TrimSpace(v) != "" {
		conf.user = v
	}
	if v, ok := parseBoolEnv("CUPS_VALIDATECERTS"); ok {
		conf.validateCerts = &v
	}
	return conf
}

func readClientConf(path string, conf *clientConf) {
	if conf == nil || strings.TrimSpace(path) == "" {
		return
	}
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
		line = stripConfComment(line)
		if line == "" {
			continue
		}
		key, value := splitConfKV(line)
		if key == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "servername":
			if value != "" {
				conf.serverName = value
			}
		case "encryption":
			if value != "" {
				conf.encryption = value
			}
		case "user":
			if value != "" {
				conf.user = value
			}
		case "validatecerts":
			if v, ok := parseBool(value); ok {
				conf.validateCerts = &v
			}
		}
	}
}

func stripConfComment(line string) string {
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '"' || ch == '\'' {
			if inQuote == 0 {
				inQuote = ch
			} else if inQuote == ch {
				inQuote = 0
			}
			continue
		}
		if ch == '#' && inQuote == 0 {
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

func splitConfKV(line string) (string, string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", ""
	}
	key := fields[0]
	value := strings.TrimSpace(strings.TrimPrefix(line, key))
	value = strings.TrimSpace(value)
	value = strings.Trim(value, " \"'")
	return key, value
}

func parseServer(value string) (string, int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, false
	}
	useTLS := false
	if strings.Contains(value, "://") {
		if u, err := url.Parse(value); err == nil {
			if u.Hostname() != "" {
				host := u.Hostname()
				port := 0
				if p := u.Port(); p != "" {
					if n, err := strconv.Atoi(p); err == nil {
						port = n
					}
				}
				switch strings.ToLower(u.Scheme) {
				case "https", "ipps":
					useTLS = true
				}
				return host, port, useTLS
			}
		}
	}
	if host, port, ok := splitHostPort(value); ok {
		return host, port, useTLS
	}
	return value, 0, useTLS
}

func splitHostPort(value string) (string, int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, false
	}
	if strings.HasPrefix(value, "[") && strings.Contains(value, "]") {
		if host, portStr, err := net.SplitHostPort(value); err == nil {
			if n, err := strconv.Atoi(portStr); err == nil {
				return host, n, true
			}
		}
	}
	if idx := strings.LastIndex(value, ":"); idx > 0 && idx < len(value)-1 {
		if n, err := strconv.Atoi(value[idx+1:]); err == nil {
			return value[:idx], n, true
		}
	}
	return "", 0, false
}

func defaultIPPPort() int {
	if v := os.Getenv("IPP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 631
}

func defaultUser() string {
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return "unknown"
}

func defaultClientConfDir() string {
	if v := os.Getenv("CUPS_CLIENT_CONF_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CUPS_CONF_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CUPS_DATA_DIR"); v != "" {
		return filepath.Join(v, "conf")
	}
	return filepath.Join("data", "conf")
}

func userClientConfDir() string {
	if v := os.Getenv("CUPS_USER_CONF_DIR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cups")
	}
	return ""
}

func parseBoolEnv(name string) (bool, bool) {
	if v := os.Getenv(name); v != "" {
		return parseBool(v)
	}
	return false, false
}

func parseBool(value string) (bool, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "1", "yes", "on", "true":
		return true, true
	case "0", "no", "off", "false":
		return false, true
	default:
		return false, false
	}
}
