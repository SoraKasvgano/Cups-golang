package backend

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"cupsgolang/internal/model"
)

type fileBackend struct{}

func init() {
	Register(fileBackend{})
}

func (fileBackend) Schemes() []string {
	return []string{"file"}
}

func (fileBackend) ListDevices(ctx context.Context) ([]Device, error) {
	devices := envDevices("CUPS_FILE_DEVICES", "direct", "File")
	return uniqueDevices(devices), nil
}

func (fileBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	u, err := url.Parse(printer.URI)
	if err != nil {
		return err
	}
	if u.Scheme != "file" {
		return fmt.Errorf("unsupported scheme")
	}
	target := u.Path
	if runtime.GOOS == "windows" && strings.HasPrefix(target, "/") && len(target) > 2 && target[2] == ':' {
		target = target[1:]
	}
	if target == "" {
		return fmt.Errorf("invalid file uri")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	src, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(target)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Sync()
}

func (fileBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}
