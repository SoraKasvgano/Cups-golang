package backend

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"cupsgolang/internal/model"
)

type snmpBackend struct{}

func init() {
	Register(snmpBackend{})
}

func (snmpBackend) Schemes() []string {
	return []string{"snmp"}
}

func (snmpBackend) ListDevices(ctx context.Context) ([]Device, error) {
	hosts := snmpHostList()
	if len(hosts) == 0 && len(snmpScanSubnets()) == 0 {
		return nil, nil
	}
	community := snmpCommunity()
	devices := []Device{}
	for _, entry := range hosts {
		if ctx.Err() != nil {
			break
		}
		host, port := parseHostPort(entry)
		if host == "" {
			continue
		}
		name, location, descr, _ := snmpSysInfo(host, port, community)
		info := name
		if info == "" {
			info = host
		}
		makeModel := strings.TrimSpace(descr)
		if makeModel == "" {
			makeModel = "SNMP"
		}
		uri := "snmp://" + host
		if port != "" && port != "161" {
			uri = uri + ":" + port
		}
		devices = append(devices, Device{
			URI:      uri,
			Info:     info,
			Make:     makeModel,
			Class:    "network",
			Location: strings.TrimSpace(location),
		})
	}
	if subnets := snmpScanSubnets(); len(subnets) > 0 {
		scanned := scanSNMPSubnets(ctx, subnets, community)
		if len(scanned) > 0 {
			devices = append(devices, scanned...)
		}
	}
	return uniqueDevices(devices), nil
}

func (snmpBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	return WrapUnsupported("snmp-submit", printer.URI, ErrUnsupported)
}

func (snmpBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	u, err := url.Parse(printer.URI)
	if err != nil {
		return SupplyStatus{State: "unknown"}, err
	}
	host := u.Hostname()
	if host == "" {
		host = strings.TrimPrefix(u.Path, "/")
	}
	if host == "" {
		return SupplyStatus{State: "unknown"}, nil
	}
	port := u.Port()
	if port == "" {
		port = "161"
	}
	community := snmpCommunity()

	params := newSNMPParams(host, port, community)
	if err := params.Connect(); err != nil {
		return SupplyStatus{State: "unknown"}, err
	}
	defer params.Conn.Close()

	details := map[string]string{}
	if name, _, _, _ := snmpSysInfoWith(params); name != "" {
		details["sysName"] = name
	}

	desc := map[string]string{}
	maxCap := map[string]int{}
	level := map[string]int{}
	_ = params.BulkWalk(".1.3.6.1.2.1.43.11.1.1.6.1", func(pdu gosnmp.SnmpPDU) error {
		idx := snmpIndex(pdu.Name, ".1.3.6.1.2.1.43.11.1.1.6.1")
		if idx != "" {
			if s, ok := pdu.Value.(string); ok {
				desc[idx] = s
			}
		}
		return nil
	})
	_ = params.BulkWalk(".1.3.6.1.2.1.43.11.1.1.8.1", func(pdu gosnmp.SnmpPDU) error {
		idx := snmpIndex(pdu.Name, ".1.3.6.1.2.1.43.11.1.1.8.1")
		if idx != "" {
			if n, ok := snmpToInt(pdu.Value); ok {
				maxCap[idx] = n
			}
		}
		return nil
	})
	_ = params.BulkWalk(".1.3.6.1.2.1.43.11.1.1.9.1", func(pdu gosnmp.SnmpPDU) error {
		idx := snmpIndex(pdu.Name, ".1.3.6.1.2.1.43.11.1.1.9.1")
		if idx != "" {
			if n, ok := snmpToInt(pdu.Value); ok {
				level[idx] = n
			}
		}
		return nil
	})

	state := "unknown"
	lowest := 101
	for idx, lvl := range level {
		key := "supply." + idx
		if d := desc[idx]; d != "" {
			details[key+".desc"] = d
		}
		details[key+".level"] = strconv.Itoa(lvl)
		if max, ok := maxCap[idx]; ok {
			details[key+".max"] = strconv.Itoa(max)
			if max > 0 && lvl >= 0 {
				percent := (lvl * 100) / max
				details[key+".percent"] = strconv.Itoa(percent)
				if percent < lowest {
					lowest = percent
				}
			}
		}
	}
	if len(level) > 0 {
		state = "ok"
		if lowest >= 0 && lowest <= 10 {
			state = "low"
		}
		if lowest == 0 {
			state = "empty"
		}
	}
	return SupplyStatus{State: state, Details: details}, nil
}

func snmpCommunity() string {
	if community := os.Getenv("CUPS_SNMP_COMMUNITY"); community != "" {
		return community
	}
	return "public"
}

func snmpHostList() []string {
	env := os.Getenv("CUPS_SNMP_HOSTS")
	if env == "" {
		env = os.Getenv("CUPS_SNMP_DEVICES")
	}
	if env == "" {
		return nil
	}
	parts := strings.FieldsFunc(env, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func snmpScanSubnets() []string {
	env := os.Getenv("CUPS_SNMP_SUBNETS")
	if env == "" {
		return nil
	}
	parts := strings.FieldsFunc(env, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseHostPort(entry string) (string, string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", ""
	}
	if strings.Contains(entry, "://") {
		if u, err := url.Parse(entry); err == nil {
			return u.Hostname(), u.Port()
		}
	}
	if host, port, ok := strings.Cut(entry, ":"); ok {
		if host != "" {
			return host, port
		}
	}
	return entry, ""
}

func newSNMPParams(host, port, community string) *gosnmp.GoSNMP {
	params := &gosnmp.GoSNMP{
		Target:    host,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   2 * time.Second,
		Retries:   1,
	}
	if port != "" && port != "161" {
		if p, err := strconv.Atoi(port); err == nil {
			params.Port = uint16(p)
		}
	}
	return params
}

func newSNMPParamsWithTimeout(host, port, community string, timeout time.Duration) *gosnmp.GoSNMP {
	params := newSNMPParams(host, port, community)
	if timeout > 0 {
		params.Timeout = timeout
	}
	return params
}

func snmpSysInfo(host, port, community string) (string, string, string, error) {
	params := newSNMPParams(host, port, community)
	if err := params.Connect(); err != nil {
		return "", "", "", err
	}
	defer params.Conn.Close()
	return snmpSysInfoWith(params)
}

func snmpSysInfoWith(params *gosnmp.GoSNMP) (string, string, string, error) {
	oids := []string{
		".1.3.6.1.2.1.1.5.0", // sysName
		".1.3.6.1.2.1.1.6.0", // sysLocation
		".1.3.6.1.2.1.1.1.0", // sysDescr
	}
	result, err := params.Get(oids)
	if err != nil {
		return "", "", "", err
	}
	name := ""
	location := ""
	descr := ""
	for _, v := range result.Variables {
		switch v.Name {
		case ".1.3.6.1.2.1.1.5.0":
			if val, ok := v.Value.(string); ok {
				name = val
			}
		case ".1.3.6.1.2.1.1.6.0":
			if val, ok := v.Value.(string); ok {
				location = val
			}
		case ".1.3.6.1.2.1.1.1.0":
			if val, ok := v.Value.(string); ok {
				descr = val
			}
		}
	}
	return name, location, descr, nil
}

func snmpIndex(name, base string) string {
	if strings.HasPrefix(name, base+".") {
		return strings.TrimPrefix(name, base+".")
	}
	if strings.HasPrefix(name, base) {
		return strings.TrimPrefix(name, base)
	}
	return ""
}

func snmpToInt(val any) (int, bool) {
	if val == nil {
		return 0, false
	}
	if bi := gosnmp.ToBigInt(val); bi != nil {
		return int(bi.Int64()), true
	}
	switch v := val.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case int32:
		return int(v), true
	case uint:
		return int(v), true
	case uint64:
		return int(v), true
	case uint32:
		return int(v), true
	default:
		return 0, false
	}
}

func snmpScanTimeout() time.Duration {
	env := strings.TrimSpace(os.Getenv("CUPS_SNMP_SCAN_TIMEOUT"))
	if env == "" {
		return 800 * time.Millisecond
	}
	if ms, err := strconv.Atoi(env); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if d, err := time.ParseDuration(env); err == nil {
		return d
	}
	return 800 * time.Millisecond
}

func snmpScanConcurrency() int {
	env := strings.TrimSpace(os.Getenv("CUPS_SNMP_SCAN_CONCURRENCY"))
	if env == "" {
		return 32
	}
	if n, err := strconv.Atoi(env); err == nil && n > 0 {
		if n > 256 {
			return 256
		}
		return n
	}
	return 32
}

func scanSNMPSubnets(ctx context.Context, subnets []string, community string) []Device {
	type result struct {
		host     string
		info     string
		location string
		descr    string
	}
	ips := []string{}
	for _, subnet := range subnets {
		ips = append(ips, expandCIDR(subnet)...)
	}
	if len(ips) == 0 {
		return nil
	}

	jobs := make(chan string, 256)
	results := make(chan result, 256)
	timeout := snmpScanTimeout()
	workers := snmpScanConcurrency()
	if workers < 1 {
		workers = 1
	}

	for i := 0; i < workers; i++ {
		go func() {
			for host := range jobs {
				if ctx.Err() != nil {
					return
				}
				params := newSNMPParamsWithTimeout(host, "161", community, timeout)
				if err := params.Connect(); err != nil {
					results <- result{}
					continue
				}
				name, location, descr, _ := snmpSysInfoWith(params)
				_ = params.Conn.Close()
				if name == "" {
					name = host
				}
				results <- result{host: host, info: name, location: location, descr: descr}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, ip := range ips {
			select {
			case <-ctx.Done():
				return
			case jobs <- ip:
			}
		}
	}()

	devices := []Device{}
	pending := len(ips)
	for pending > 0 {
		select {
		case res := <-results:
			pending--
			if res.host != "" {
				makeModel := strings.TrimSpace(res.descr)
				if makeModel == "" {
					makeModel = "SNMP"
				}
				devices = append(devices, Device{
					URI:      "snmp://" + res.host,
					Info:     res.info,
					Make:     makeModel,
					Class:    "network",
					Location: strings.TrimSpace(res.location),
				})
			}
		case <-ctx.Done():
			return devices
		}
	}
	return devices
}

func expandCIDR(cidr string) []string {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return nil
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	ip = ip.To4()
	if ip == nil {
		return nil
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones < 24 {
		return nil
	}
	var ips []string
	network := ip.Mask(ipnet.Mask)
	broadcast := make(net.IP, len(network))
	copy(broadcast, network)
	for i := range broadcast {
		broadcast[i] |= ^ipnet.Mask[i]
	}
	cur := make(net.IP, len(network))
	copy(cur, network)
	for {
		cur = nextIPv4(cur)
		if cur == nil {
			break
		}
		if !ipLess(cur, broadcast) {
			break
		}
		ips = append(ips, cur.String())
	}
	return ips
}

func nextIPv4(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

func ipLess(a, b net.IP) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return false
}

func uniqueDevices(devices []Device) []Device {
	seen := map[string]bool{}
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		key := strings.ToLower(strings.TrimSpace(d.URI))
		if key == "" {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}
