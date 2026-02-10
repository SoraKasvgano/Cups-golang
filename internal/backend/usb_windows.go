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
	devices := envUSBDevices()
	enum, err := enumLocalPrinters()
	if err != nil {
		return devices, nil
	}
	for _, p := range enum {
		port := strings.ToUpper(strings.TrimSpace(p.Port))
		if !strings.HasPrefix(port, "USB") {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		makeModel := strings.TrimSpace(p.Driver)
		if makeModel == "" {
			makeModel = "USB"
		}
		uri := "usb://" + url.PathEscape(name)
		location := strings.TrimSpace(p.Location)
		devices = append(devices, Device{
			URI:      uri,
			Info:     name,
			Make:     makeModel,
			Class:    "direct",
			Location: location,
		})
	}
	return devices, nil
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
				if host, err := url.PathUnescape(u.Host); err == nil {
					return host
				}
				return u.Host
			}
			if u.Path != "" {
				path := strings.TrimPrefix(u.Path, "/")
				if decoded, err := url.PathUnescape(path); err == nil {
					return decoded
				}
				return path
			}
		}
		if strings.HasPrefix(printer.URI, "usb://") {
			name := strings.TrimPrefix(printer.URI, "usb://")
			if decoded, err := url.PathUnescape(name); err == nil {
				return decoded
			}
			return name
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
	procEnumPrinters = modWinspool.NewProc("EnumPrintersW")
)

type printerInfo2 struct {
	ServerName      *uint16
	PrinterName     *uint16
	ShareName       *uint16
	PortName        *uint16
	DriverName      *uint16
	Comment         *uint16
	Location        *uint16
	DevMode         uintptr
	SepFile         *uint16
	PrintProcessor  *uint16
	Datatype        *uint16
	Parameters      *uint16
	SecurityDesc    uintptr
	Attributes      uint32
	Priority        uint32
	DefaultPriority uint32
	StartTime       uint32
	UntilTime       uint32
	Status          uint32
	Jobs            uint32
	AveragePPM      uint32
}

type printerInfo struct {
	Name     string
	Port     string
	Driver   string
	Location string
}

const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
)

func enumLocalPrinters() ([]printerInfo, error) {
	flags := printerEnumLocal | printerEnumConnections
	level := uint32(2)
	var needed uint32
	var returned uint32
	r1, _, err := procEnumPrinters.Call(
		uintptr(flags),
		0,
		uintptr(level),
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 && needed == 0 {
		return nil, err
	}
	buf := make([]byte, needed)
	r1, _, err = procEnumPrinters.Call(
		uintptr(flags),
		0,
		uintptr(level),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 {
		return nil, err
	}
	out := make([]printerInfo, 0, returned)
	entrySize := unsafe.Sizeof(printerInfo2{})
	base := uintptr(unsafe.Pointer(&buf[0]))
	for i := 0; i < int(returned); i++ {
		ptr := (*printerInfo2)(unsafe.Pointer(base + uintptr(i)*entrySize))
		name := windows.UTF16PtrToString(ptr.PrinterName)
		port := windows.UTF16PtrToString(ptr.PortName)
		driver := windows.UTF16PtrToString(ptr.DriverName)
		location := windows.UTF16PtrToString(ptr.Location)
		out = append(out, printerInfo{
			Name:     name,
			Port:     port,
			Driver:   driver,
			Location: location,
		})
	}
	return out, nil
}

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
