package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"

	"cupsgolang/internal/model"
)

type dnssdBackend struct{}

func init() {
	Register(dnssdBackend{})
}

func (dnssdBackend) Schemes() []string {
	return []string{"dnssd"}
}

func (dnssdBackend) ListDevices(ctx context.Context) ([]Device, error) {
	devices := envDevices("CUPS_DNSSD_DEVICES", "network", "DNSSD")
	seen := map[string]bool{}
	for _, d := range devices {
		if d.URI != "" {
			seen[d.URI] = true
		}
	}
	services := []string{"_ipp._tcp", "_ipps._tcp", "_ipp-tls._tcp", "_printer._tcp", "_pdl-datastream._tcp"}
	for _, service := range services {
		entries := make(chan *mdns.ServiceEntry, 64)
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		go func() {
			_ = mdns.Query(&mdns.QueryParam{
				Service: service,
				Domain:  "local",
				Timeout: 2 * time.Second,
				Entries: entries,
			})
			close(entries)
		}()
		for {
			select {
			case <-qctx.Done():
				cancel()
				goto nextService
			case entry, ok := <-entries:
				if !ok {
					cancel()
					goto nextService
				}
				if entry == nil {
					continue
				}
				host := entry.Host
				if host == "" && entry.AddrV4 != nil {
					host = entry.AddrV4.String()
				} else if host == "" && entry.AddrV6 != nil {
					host = entry.AddrV6.String()
				}
				if host == "" || entry.Port == 0 {
					continue
				}
				key := host + ":" + strconv.Itoa(entry.Port) + "/" + entry.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				txt := parseTxtRecords(entry.InfoFields)
				deviceURI := buildIPPURI(service, host, entry.Port, entry.Name, txt)
				info := firstNonEmptyDevice(txt["ty"], txt["note"], entry.Name)
				makeModel := firstNonEmptyDevice(txt["product"], txt["ty"], "IPP")
				devices = append(devices, Device{
					URI:   deviceURI,
					Info:  info,
					Make:  makeModel,
					Class: "network",
				})
			}
		}
	nextService:
	}
	return uniqueDevices(devices), nil
}

func (dnssdBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	target, err := resolveDNSSDTarget(ctx, printer.URI)
	if err != nil {
		return err
	}
	if target == "" {
		return ErrUnsupported
	}
	b := ForURI(target)
	if b == nil {
		return ErrUnsupported
	}
	cp := printer
	cp.URI = target
	return b.SubmitJob(ctx, cp, job, doc, filePath)
}

func (dnssdBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}

var ErrUnsupported = errorString("backend not supported")

type errorString string

func (e errorString) Error() string { return string(e) }

func parseTxtRecords(records []string) map[string]string {
	out := map[string]string{}
	for _, record := range records {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if key != "" {
				out[strings.ToLower(key)] = val
			}
		}
	}
	return out
}

func buildIPPURI(service, host string, port int, name string, txt map[string]string) string {
	scheme := "ipp"
	if strings.Contains(service, "ipps") || strings.Contains(service, "ipp-tls") {
		scheme = "ipps"
	}
	resource := txt["rp"]
	if resource == "" {
		resource = "ipp/print"
	}
	resource = strings.TrimPrefix(resource, "/")
	return scheme + "://" + host + ":" + strconv.Itoa(port) + "/" + resource
}

func resolveDNSSDTarget(ctx context.Context, uri string) (string, error) {
	instance, service, domain, err := parseDNSSDURI(uri)
	if err != nil {
		return "", err
	}
	if service == "" {
		return "", ErrUnsupported
	}
	if domain == "" {
		domain = "local"
	}
	var chosen *mdns.ServiceEntry
	entries := make(chan *mdns.ServiceEntry, 64)
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		_ = mdns.Query(&mdns.QueryParam{
			Service: service,
			Domain:  domain,
			Timeout: 3 * time.Second,
			Entries: entries,
		})
		close(entries)
	}()
	for {
		select {
		case <-qctx.Done():
			if chosen == nil {
				return "", fmt.Errorf("dnssd resolution timeout")
			}
			return dnssdEntryURI(service, chosen), nil
		case entry, ok := <-entries:
			if !ok {
				if chosen == nil {
					return "", fmt.Errorf("dnssd resolution failed")
				}
				return dnssdEntryURI(service, chosen), nil
			}
			if entry == nil {
				continue
			}
			if instance != "" && !strings.EqualFold(entry.Name, instance) {
				continue
			}
			chosen = entry
			if instance != "" {
				return dnssdEntryURI(service, chosen), nil
			}
		}
	}
}

func parseDNSSDURI(uri string) (string, string, string, error) {
	if uri == "" {
		return "", "", "", errors.New("empty dnssd uri")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", "", err
	}
	host := strings.TrimSpace(u.Host)
	if host == "" {
		host = strings.TrimSpace(u.Opaque)
	}
	host = strings.TrimPrefix(host, "//")
	if host == "" {
		return "", "", "", errors.New("invalid dnssd uri")
	}
	if strings.Contains(host, ":") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		} else {
			host = strings.Split(host, ":")[0]
		}
	}
	host, _ = url.PathUnescape(host)
	host = strings.TrimSuffix(host, ".")
	services := []string{
		"_ipp-tls._tcp",
		"_ipps._tcp",
		"_ipp._tcp",
		"_printer._tcp",
		"_pdl-datastream._tcp",
	}
	lower := strings.ToLower(host)
	for _, svc := range services {
		if idx := strings.Index(lower, svc); idx >= 0 {
			instance := strings.TrimSuffix(host[:idx], ".")
			domain := strings.TrimPrefix(host[idx+len(svc):], ".")
			return instance, svc, domain, nil
		}
	}
	return host, "", "", nil
}

func dnssdEntryURI(service string, entry *mdns.ServiceEntry) string {
	if entry == nil {
		return ""
	}
	host := entry.Host
	if host == "" && entry.AddrV4 != nil {
		host = entry.AddrV4.String()
	} else if host == "" && entry.AddrV6 != nil {
		host = entry.AddrV6.String()
	}
	if host == "" {
		return ""
	}
	txt := parseTxtRecords(entry.InfoFields)
	switch {
	case strings.Contains(service, "_pdl-datastream"):
		port := entry.Port
		if port == 0 {
			port = 9100
		}
		return "socket://" + host + ":" + strconv.Itoa(port)
	case strings.Contains(service, "_printer"):
		port := entry.Port
		if port == 0 {
			port = 515
		}
		queue := strings.TrimSpace(txt["rp"])
		if queue == "" {
			queue = strings.TrimSpace(entry.Name)
		}
		if queue == "" {
			queue = "lp"
		}
		return "lpd://" + host + ":" + strconv.Itoa(port) + "/" + queue
	default:
		return buildIPPURI(service, host, entry.Port, entry.Name, txt)
	}
}

func firstNonEmptyDevice(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
