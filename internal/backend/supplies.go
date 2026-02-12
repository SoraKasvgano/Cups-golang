package backend

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"

	"cupsgolang/internal/model"
)

const defaultSNMPPort = "161"

// querySuppliesViaSNMP asks the SNMP backend for supply state using host data
// derived from the destination URI. Returns ok=false when no network host exists.
func querySuppliesViaSNMP(ctx context.Context, printer model.Printer) (SupplyStatus, error, bool) {
	host, port := snmpTargetFromPrinterURI(printer.URI)
	if host == "" {
		return SupplyStatus{State: "unknown"}, nil, false
	}
	uri := snmpURI(host, port)
	status, err := (snmpBackend{}).QuerySupplies(ctx, model.Printer{URI: uri, Name: printer.Name})
	if err != nil {
		return SupplyStatus{State: "unknown"}, WrapTemporary("snmp-supplies", printer.URI, err), true
	}
	return status, nil, true
}

func snmpURI(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	hostport := host
	if strings.TrimSpace(port) != "" && strings.TrimSpace(port) != defaultSNMPPort {
		hostport = net.JoinHostPort(host, strings.TrimSpace(port))
	}
	u := &url.URL{Scheme: "snmp", Host: hostport}
	return u.String()
}

func snmpTargetFromPrinterURI(rawURI string) (string, string) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return "", ""
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return "", ""
	}
	query := u.Query()
	host := strings.TrimSpace(query.Get("snmp-host"))
	if host == "" {
		host = strings.TrimSpace(u.Hostname())
	}
	if host == "" {
		pathHost := strings.Trim(strings.TrimSpace(u.Path), "/")
		if pathHost != "" && !strings.Contains(pathHost, "/") {
			host = pathHost
		}
	}
	port := strings.TrimSpace(query.Get("snmp-port"))
	if port == "" {
		port = strings.TrimSpace(query.Get("snmp-port-number"))
	}
	if port == "" && strings.EqualFold(u.Scheme, "snmp") {
		port = strings.TrimSpace(u.Port())
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
			port = ""
		}
	}
	if port == "" {
		port = defaultSNMPPort
	}
	return host, port
}
