package backend

import (
	"context"
	"os"
	"strings"

	"cupsgolang/internal/model"
)

type usbBackend struct{}

func init() {
	Register(usbBackend{})
}

func (usbBackend) Schemes() []string {
	return []string{"usb"}
}

func (usbBackend) ListDevices(ctx context.Context) ([]Device, error) {
	return listUSBDevices()
}

func (usbBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	return submitUSBJob(printer, filePath)
}

func (usbBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}

func envUSBDevices() []Device {
	devices := []Device{}
	if env := os.Getenv("CUPS_USB_DEVICES"); env != "" {
		for _, entry := range strings.Split(env, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			devices = append(devices, Device{URI: entry, Info: "USB Device", Make: "USB", Class: "direct"})
		}
	}
	return devices
}
