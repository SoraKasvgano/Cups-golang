package backend

import (
	"bufio"
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

type lpdBackend struct{}

func init() {
	Register(lpdBackend{})
}

func (lpdBackend) Schemes() []string {
	return []string{"lpd"}
}

func (lpdBackend) ListDevices(ctx context.Context) ([]Device, error) {
	devices := envDevices("CUPS_LPD_DEVICES", "network", "LPD")
	return uniqueDevices(devices), nil
}

func (lpdBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	u, err := url.Parse(printer.URI)
	if err != nil {
		return err
	}
	host := u.Host
	if host == "" {
		return fmt.Errorf("invalid lpd uri")
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "515")
	}
	queue := strings.TrimPrefix(u.Path, "/")
	if queue == "" {
		queue = "lp"
	}

	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	if err := lpdSendReceiveJob(rw, queue); err != nil {
		return err
	}

	hostName, _ := os.Hostname()
	if hostName == "" {
		hostName = "localhost"
	}
	user := job.UserName
	if user == "" {
		user = "anonymous"
	}
	jobName := job.Name
	if jobName == "" {
		jobName = "Untitled"
	}
	docName := doc.FileName
	if docName == "" {
		docName = jobName
	}
	jobID := int(job.ID % 1000)
	cfName := fmt.Sprintf("cfA%03d%s", jobID, hostName)
	dfName := fmt.Sprintf("dfA%03d%s", jobID, hostName)

	control := buildLPDControl(hostName, user, jobName, docName, dfName)
	if err := lpdSendControl(rw, cfName, []byte(control)); err != nil {
		return err
	}
	if err := lpdSendData(rw, dfName, filePath); err != nil {
		return err
	}
	return nil
}

func (lpdBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}

func lpdSendReceiveJob(rw *bufio.ReadWriter, queue string) error {
	if _, err := rw.WriteString(string([]byte{0x02}) + queue + "\n"); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	return lpdAck(rw)
}

func lpdSendControl(rw *bufio.ReadWriter, name string, data []byte) error {
	if _, err := rw.WriteString(fmt.Sprintf("\x02%d %s\n", len(data), name)); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if err := lpdAck(rw); err != nil {
		return err
	}
	if _, err := rw.Write(data); err != nil {
		return err
	}
	if _, err := rw.Write([]byte{0x00}); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	return lpdAck(rw)
}

func lpdSendData(rw *bufio.ReadWriter, name string, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if _, err := rw.WriteString(fmt.Sprintf("\x03%d %s\n", info.Size(), name)); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if err := lpdAck(rw); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(rw, f); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if _, err := rw.Write([]byte{0x00}); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	return lpdAck(rw)
}

func lpdAck(rw *bufio.ReadWriter) error {
	b, err := rw.ReadByte()
	if err != nil {
		return err
	}
	if b != 0 {
		return fmt.Errorf("lpd error: %d", b)
	}
	return nil
}

func buildLPDControl(host, user, jobName, docName, dataFile string) string {
	lines := []string{
		"H" + host,
		"P" + user,
		"J" + jobName,
		"N" + docName,
		"U" + dataFile,
		"l" + dataFile,
	}
	return strings.Join(lines, "\n") + "\n"
}
