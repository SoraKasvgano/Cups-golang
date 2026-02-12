package server

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"

	"cupsgolang/internal/backend"
	"cupsgolang/internal/store"
)

type Device struct {
	URI      string
	Info     string
	Make     string
	Class    string
	DeviceID string
	Location string
}

const deviceCacheTTL = 45 * time.Second

func discoverDevices(ctx context.Context, st *store.Store, useCache bool) []Device {
	if ctx == nil {
		ctx = context.Background()
	}
	if st != nil {
		if useCache {
			if cached := loadDeviceCache(ctx, st); len(cached) > 0 {
				return cached
			}
		}
		_ = st.WithTx(ctx, false, func(tx *sql.Tx) error {
			_ = st.PruneDeviceCache(ctx, tx, 24*time.Hour)
			return nil
		})
	}

	devices := []Device{}
	devices = append(devices, discoverLocalDevices(ctx)...)
	devices = append(devices, discoverNetworkIPP(ctx)...)
	devices = append(devices, discoverMDNSIPP(ctx)...)
	devices = uniqueDevices(devices)

	if st != nil && len(devices) > 0 {
		persistDeviceCache(ctx, st, devices)
	}
	return devices
}

func loadDeviceCache(ctx context.Context, st *store.Store) []Device {
	if st == nil {
		return nil
	}
	items := []store.DeviceCacheEntry{}
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		items, err = st.ListDeviceCache(ctx, tx, deviceCacheTTL, 4096)
		return err
	})
	if len(items) == 0 {
		return nil
	}
	out := make([]Device, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.URI) == "" {
			continue
		}
		out = append(out, Device{
			URI:      item.URI,
			Info:     item.Info,
			Make:     item.MakeModel,
			Class:    item.Class,
			DeviceID: item.DeviceID,
			Location: item.Location,
		})
	}
	return uniqueDevices(out)
}

func persistDeviceCache(ctx context.Context, st *store.Store, devices []Device) {
	if st == nil || len(devices) == 0 {
		return
	}
	now := time.Now().UTC()
	_ = st.WithTx(ctx, false, func(tx *sql.Tx) error {
		for _, d := range devices {
			if strings.TrimSpace(d.URI) == "" {
				continue
			}
			_ = st.UpsertDeviceCache(ctx, tx, store.DeviceCacheEntry{
				URI:       d.URI,
				Info:      d.Info,
				MakeModel: d.Make,
				Class:     d.Class,
				DeviceID:  d.DeviceID,
				Location:  d.Location,
				UpdatedAt: now,
			})
		}
		return st.PruneDeviceCache(ctx, tx, 24*time.Hour)
	})
}

func uniqueDevices(in []Device) []Device {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]Device, 0, len(in))
	for _, d := range in {
		uri := strings.TrimSpace(d.URI)
		if uri == "" {
			continue
		}
		key := strings.ToLower(uri)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

func discoverLocalDevices(ctx context.Context) []Device {
	if ctx == nil {
		ctx = context.Background()
	}
	devices := []Device{}
	// Use env-provided device list when available.
	if env := os.Getenv("CUPS_DEVICE_URIS"); env != "" {
		for _, entry := range splitDeviceEnv(env) {
			if ctx.Err() != nil {
				return devices
			}
			if d, ok := parseDeviceEntry(entry, "Env Device", "CUPS-Golang", "file"); ok {
				devices = append(devices, d)
			}
		}
	}
	for _, d := range backend.ListDevices(ctx) {
		if ctx.Err() != nil {
			break
		}
		devices = append(devices, Device{
			URI:      d.URI,
			Info:     d.Info,
			Make:     d.Make,
			Class:    d.Class,
			DeviceID: d.DeviceID,
			Location: d.Location,
		})
	}
	return devices
}

func discoverNetworkIPP(ctx context.Context) []Device {
	if ctx == nil {
		ctx = context.Background()
	}
	devices := []Device{}
	if hosts := os.Getenv("CUPS_IPP_SCAN"); hosts != "" {
		for _, host := range strings.Split(hosts, ",") {
			if ctx.Err() != nil {
				return devices
			}
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

func discoverMDNSIPP(ctx context.Context) []Device {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.ToLower(os.Getenv("CUPS_ENABLE_MDNS")) != "1" && strings.ToLower(os.Getenv("CUPS_ENABLE_MDNS")) != "true" {
		return nil
	}
	devices := []Device{}
	seen := map[string]bool{}
	services := []string{"_ipp._tcp", "_ipps._tcp", "_ipp-tls._tcp", "_printer._tcp", "_pdl-datastream._tcp"}
	for _, service := range services {
		if ctx.Err() != nil {
			break
		}
		entries := make(chan *mdns.ServiceEntry, 64)
		timeout := 2 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
				timeout = remaining
			}
		}
		ctxQuery, cancel := context.WithTimeout(ctx, timeout)
		go func() {
			_ = mdns.Query(&mdns.QueryParam{
				Service: service,
				Domain:  "local",
				Timeout: timeout,
				Entries: entries,
			})
			close(entries)
		}()
		for {
			select {
			case <-ctxQuery.Done():
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
					URI:      deviceURI,
					Info:     info,
					Make:     makeModel,
					Class:    "network",
					Location: txt["note"],
				})
			}
		}
	nextService:
	}
	return devices
}

func splitDeviceEnv(env string) []string {
	parts := strings.FieldsFunc(env, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDeviceEntry(entry, defaultInfo, defaultMake, className string) (Device, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return Device{}, false
	}
	parts := strings.Split(entry, "|")
	uri := strings.TrimSpace(parts[0])
	if uri == "" {
		return Device{}, false
	}
	info := ""
	makeVal := defaultMake
	deviceID := ""
	location := ""
	if len(parts) > 1 {
		info = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		makeVal = strings.TrimSpace(parts[2])
	}
	if len(parts) > 3 {
		deviceID = strings.TrimSpace(parts[3])
	}
	if len(parts) > 4 {
		location = strings.TrimSpace(parts[4])
	}
	if info == "" {
		if defaultInfo != "" {
			info = defaultInfo
		} else {
			info = uri
		}
	}
	return Device{
		URI:      uri,
		Info:     info,
		Make:     makeVal,
		Class:    className,
		DeviceID: deviceID,
		Location: location,
	}, true
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
