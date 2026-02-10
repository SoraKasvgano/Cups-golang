package backend

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"

	"cupsgolang/internal/model"
)

type Device struct {
	URI      string
	Info     string
	Make     string
	Class    string
	DeviceID string
	Location string
}

type SupplyStatus struct {
	State   string
	Details map[string]string
}

type Backend interface {
	Schemes() []string
	ListDevices(ctx context.Context) ([]Device, error)
	SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error
	QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error)
}

var registry struct {
	sync.RWMutex
	backends []Backend
}

func Register(b Backend) {
	if b == nil {
		return
	}
	registry.Lock()
	registry.backends = append(registry.backends, b)
	registry.Unlock()
}

func ForURI(uri string) Backend {
	u, err := url.Parse(uri)
	if err != nil {
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return nil
	}
	registry.RLock()
	defer registry.RUnlock()
	for _, b := range registry.backends {
		for _, s := range b.Schemes() {
			if strings.EqualFold(s, scheme) {
				return b
			}
		}
	}
	return nil
}

func ListDevices(ctx context.Context) []Device {
	registry.RLock()
	backends := append([]Backend(nil), registry.backends...)
	registry.RUnlock()

	var out []Device
	for _, b := range backends {
		devs, err := b.ListDevices(ctx)
		if err != nil {
			continue
		}
		out = append(out, devs...)
	}
	return out
}

func envDevices(envKey, className, makeName string) []Device {
	val := strings.TrimSpace(os.Getenv(envKey))
	if val == "" {
		return nil
	}
	devices := []Device{}
	for _, entry := range splitEnvList(val) {
		if d, ok := parseDeviceEntry(entry, "", makeName, className); ok {
			devices = append(devices, d)
		}
	}
	return devices
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

func splitEnvList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
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
