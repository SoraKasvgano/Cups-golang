package server

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"

	"cupsgolang/internal/backend"
)

type Device struct {
	URI   string
	Info  string
	Make  string
	Class string
}

func discoverLocalDevices() []Device {
	devices := []Device{}
	// Use env-provided device list when available.
	if env := os.Getenv("CUPS_DEVICE_URIS"); env != "" {
		for _, entry := range strings.Split(env, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			devices = append(devices, Device{URI: entry, Info: "Env Device", Make: "CUPS-Golang", Class: "file"})
		}
	}
	for _, d := range backend.ListDevices(context.Background()) {
		devices = append(devices, Device{
			URI:   d.URI,
			Info:  d.Info,
			Make:  d.Make,
			Class: d.Class,
		})
	}
	return devices
}

func discoverNetworkIPP() []Device {
	devices := []Device{}
	if hosts := os.Getenv("CUPS_IPP_SCAN"); hosts != "" {
		for _, host := range strings.Split(hosts, ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			if !strings.Contains(host, ":") {
				host = host + ":631"
			}
			if _, err := net.ResolveTCPAddr("tcp", host); err == nil {
				devices = append(devices, Device{URI: "ipp://" + host + "/printers/" + host, Info: "IPP Printer", Make: "IPP", Class: "network"})
			}
		}
	}
	return devices
}

func discoverMDNSIPP() []Device {
	if strings.ToLower(os.Getenv("CUPS_ENABLE_MDNS")) != "1" && strings.ToLower(os.Getenv("CUPS_ENABLE_MDNS")) != "true" {
		return nil
	}
	devices := []Device{}
	seen := map[string]bool{}
	services := []string{"_ipp._tcp", "_ipps._tcp", "_ipp-tls._tcp", "_printer._tcp", "_pdl-datastream._tcp"}
	for _, service := range services {
		entries := make(chan *mdns.ServiceEntry, 64)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
			case <-ctx.Done():
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
				deviceURI := buildDNSSDURI(service, host, entry.Port, entry.Name, txt)
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
	return devices
}

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

func buildDNSSDURI(service, host string, port int, name string, txt map[string]string) string {
	switch {
	case strings.Contains(service, "_pdl-datastream"):
		if port == 0 {
			port = 9100
		}
		return "socket://" + host + ":" + strconv.Itoa(port)
	case strings.Contains(service, "_printer"):
		if port == 0 {
			port = 515
		}
		queue := strings.TrimSpace(txt["rp"])
		if queue == "" {
			queue = strings.TrimSpace(name)
		}
		if queue == "" {
			queue = "lp"
		}
		queue = strings.TrimPrefix(queue, "/")
		return "lpd://" + host + ":" + strconv.Itoa(port) + "/" + queue
	default:
		return buildIPPURI(service, host, port, name, txt)
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
