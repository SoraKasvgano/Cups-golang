package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"cupsgolang/internal/model"
)

type socketBackend struct{}

func init() {
	Register(socketBackend{})
}

func (socketBackend) Schemes() []string {
	return []string{"socket"}
}

func (socketBackend) ListDevices(ctx context.Context) ([]Device, error) {
	devices := envDevices("CUPS_SOCKET_DEVICES", "network", "Socket")
	return uniqueDevices(devices), nil
}

func (socketBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	u, err := url.Parse(printer.URI)
	if err != nil {
		return err
	}
	host := u.Host
	if host == "" {
		return fmt.Errorf("invalid socket uri")
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "9100")
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(conn, f)
	return err
}

func (socketBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}
