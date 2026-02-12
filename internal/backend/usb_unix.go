//go:build !windows

package backend

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"cupsgolang/internal/model"
)

func listUSBDevices() ([]Device, error) {
	devices := envUSBDevices()
	paths, _ := filepath.Glob("/dev/usb/lp*")
	paths2, _ := filepath.Glob("/dev/usblp*")
	for _, p := range append(paths, paths2...) {
		devices = append(devices, Device{
			URI:   "usb://" + p,
			Info:  "USB Printer",
			Make:  "USB",
			Class: "direct",
		})
	}
	return devices, nil
}

func submitUSBJob(printer model.Printer, filePath string) error {
	if filePath == "" {
		return WrapUnsupported("usb-submit", printer.URI, ErrUnsupported)
	}
	devPath := usbDevicePath(printer.URI)
	if devPath == "" {
		return WrapUnsupported("usb-submit", printer.URI, ErrUnsupported)
	}
	in, err := os.Open(filePath)
	if err != nil {
		return WrapPermanent("usb-open", printer.URI, err)
	}
	defer in.Close()

	out, err := os.OpenFile(devPath, os.O_WRONLY, 0600)
	if err != nil {
		return WrapTemporary("usb-open-device", printer.URI, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return WrapTemporary("usb-write", printer.URI, err)
	}
	if err := out.Sync(); err != nil {
		return WrapTemporary("usb-sync", printer.URI, err)
	}
	return nil
}

func usbDevicePath(uri string) string {
	if uri == "" {
		return ""
	}
	if u, err := url.Parse(uri); err == nil {
		if u.Path != "" {
			return u.Path
		}
		if u.Host != "" {
			return "/dev/usb/" + u.Host
		}
	}
	if strings.HasPrefix(uri, "usb://") {
		return strings.TrimPrefix(uri, "usb://")
	}
	return uri
}
