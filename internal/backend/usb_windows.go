//go:build windows

package backend

import (
	"io"
	"net/url"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"cupsgolang/internal/model"
)

func listUSBDevices() ([]Device, error) {
	return envUSBDevices(), nil
}

func submitUSBJob(printer model.Printer, filePath string) error {
	if filePath == "" {
		return ErrUnsupported
	}
	name := usbPrinterName(printer)
	if name == "" {
		return ErrUnsupported
	}
	handle, err := openPrinter(name)
	if err != nil {
		return err
	}
	defer closePrinter(handle)

	jobName := "CUPS-Golang USB Job"
	if printer.Name != "" {
		jobName = printer.Name + " Job"
	}
	docID, err := startDocPrinter(handle, jobName)
	if err != nil {
		return err
	}
	defer endDocPrinter(handle, docID)

	if err := startPagePrinter(handle); err != nil {
		return err
	}
	defer endPagePrinter(handle)

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 64*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := writePrinter(handle, buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func usbPrinterName(printer model.Printer) string {
	if printer.URI != "" {
		if u, err := url.Parse(printer.URI); err == nil {
			if u.Host != "" {
				return u.Host
			}
			if u.Path != "" {
				return strings.TrimPrefix(u.Path, "/")
			}
		}
		if strings.HasPrefix(printer.URI, "usb://") {
			return strings.TrimPrefix(printer.URI, "usb://")
		}
	}
	return printer.Name
}

type docInfo1 struct {
	DocName    *uint16
	OutputFile *uint16
	Datatype   *uint16
}

var (
	modWinspool      = windows.NewLazySystemDLL("winspool.drv")
	procOpenPrinterW = modWinspool.NewProc("OpenPrinterW")
	procClosePrinter = modWinspool.NewProc("ClosePrinter")
	procStartDoc     = modWinspool.NewProc("StartDocPrinterW")
	procEndDoc       = modWinspool.NewProc("EndDocPrinter")
	procStartPage    = modWinspool.NewProc("StartPagePrinter")
	procEndPage      = modWinspool.NewProc("EndPagePrinter")
	procWritePrinter = modWinspool.NewProc("WritePrinter")
)

func openPrinter(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	var handle windows.Handle
	r1, _, err := procOpenPrinterW.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(unsafe.Pointer(&handle)), 0)
	if r1 == 0 {
		return 0, err
	}
	return handle, nil
}

func closePrinter(handle windows.Handle) {
	_, _, _ = procClosePrinter.Call(uintptr(handle))
}

func startDocPrinter(handle windows.Handle, name string) (uint32, error) {
	docName, _ := windows.UTF16PtrFromString(name)
	datatype, _ := windows.UTF16PtrFromString("RAW")
	doc := docInfo1{DocName: docName, Datatype: datatype}
	r1, _, err := procStartDoc.Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&doc)))
	if r1 == 0 {
		return 0, err
	}
	return uint32(r1), nil
}

func endDocPrinter(handle windows.Handle, jobID uint32) {
	_, _, _ = procEndDoc.Call(uintptr(handle))
}

func startPagePrinter(handle windows.Handle) error {
	r1, _, err := procStartPage.Call(uintptr(handle))
	if r1 == 0 {
		return err
	}
	return nil
}

func endPagePrinter(handle windows.Handle) {
	_, _, _ = procEndPage.Call(uintptr(handle))
}

func writePrinter(handle windows.Handle, data []byte) error {
	var written uint32
	r1, _, err := procWritePrinter.Call(uintptr(handle), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&written)))
	if r1 == 0 {
		return err
	}
	return nil
}
