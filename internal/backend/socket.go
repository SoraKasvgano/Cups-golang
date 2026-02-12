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
		return WrapUnsupported("socket-parse", printer.URI, err)
	}
	host := u.Host
	if host == "" {
		return WrapUnsupported("socket-parse", printer.URI, fmt.Errorf("invalid socket uri"))
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "9100")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return WrapTemporary("socket-connect", printer.URI, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return WrapPermanent("socket-open", printer.URI, err)
	}
	defer f.Close()

	_, err = io.Copy(conn, f)
	if err != nil {
		return WrapTemporary("socket-write", printer.URI, err)
	}
	return nil
}

func (socketBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	if status, err, ok := querySuppliesViaSNMP(ctx, printer); ok {
		return status, err
	}
	return SupplyStatus{State: "unknown"}, nil
}
