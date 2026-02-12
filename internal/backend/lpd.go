package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
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
		return WrapUnsupported("lpd-parse", printer.URI, err)
	}
	host := u.Host
	if host == "" {
		return WrapUnsupported("lpd-parse", printer.URI, fmt.Errorf("invalid lpd uri"))
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "515")
	}
	queue := strings.TrimPrefix(u.Path, "/")
	if queue == "" {
		queue = "lp"
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return WrapTemporary("lpd-connect", printer.URI, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	if err := lpdSendReceiveJob(rw, queue); err != nil {
		return WrapTemporary("lpd-send", printer.URI, err)
	}

	hostName, _ := os.Hostname()
	if hostName == "" {
		hostName = "localhost"
	}
	user := job.UserName
	if user == "" {
		user = "anonymous"
	}
	title := strings.TrimSpace(job.Name)
	if title == "" {
		title = strings.TrimSpace(doc.FileName)
	}
	if title == "" {
		title = "Untitled"
	}
	opts := parseBackendOptions(job.Options)
	copies := parseIntOption(opts, "copies", 1)
	if copies < 1 {
		copies = 1
	}
	banner := shouldSendLPDBanner(opts)
	jobID := int(job.ID % 1000)
	hostShort := truncateLPD(hostName, 15)
	cfName := fmt.Sprintf("cfA%03d%s", jobID, hostShort)
	dfName := fmt.Sprintf("dfA%03d%s", jobID, hostShort)

	control := buildLPDControl(hostName, user, title, dfName, banner, copies)
	if err := lpdSendControl(rw, cfName, []byte(control)); err != nil {
		return WrapTemporary("lpd-control", printer.URI, err)
	}
	if err := lpdSendData(rw, dfName, filePath); err != nil {
		return WrapTemporary("lpd-data", printer.URI, err)
	}
	return nil
}

func (lpdBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	if status, err, ok := querySuppliesViaSNMP(ctx, printer); ok {
		return status, err
	}
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

func buildLPDControl(host, user, title, dataFile string, banner bool, copies int) string {
	host = truncateLPD(host, 31)
	user = truncateLPD(user, 31)
	title = truncateLPD(title, 99)
	lines := []string{
		"H" + host,
		"P" + user,
		"J" + title,
	}
	if banner {
		lines = append(lines,
			"C"+host,
			"L"+user,
		)
	}
	if copies < 1 {
		copies = 1
	}
	for i := 0; i < copies; i++ {
		lines = append(lines, "l"+dataFile)
	}
	lines = append(lines,
		"U"+dataFile,
		"N"+truncateLPD(title, 131),
	)
	return strings.Join(lines, "\n") + "\n"
}

func parseBackendOptions(optionsJSON string) map[string]string {
	opts := map[string]string{}
	if strings.TrimSpace(optionsJSON) == "" {
		return opts
	}
	_ = json.Unmarshal([]byte(optionsJSON), &opts)
	return opts
}

func parseIntOption(opts map[string]string, key string, def int) int {
	if opts == nil {
		return def
	}
	if v := strings.TrimSpace(opts[key]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func shouldSendLPDBanner(opts map[string]string) bool {
	if opts == nil {
		return false
	}
	val := strings.TrimSpace(opts["job-sheets"])
	if val == "" || strings.EqualFold(val, "none") {
		return false
	}
	parts := strings.Split(val, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !strings.EqualFold(p, "none") {
			return true
		}
	}
	return false
}

func truncateLPD(val string, max int) string {
	if max <= 0 {
		return ""
	}
	val = strings.TrimSpace(val)
	if len(val) <= max {
		return val
	}
	return val[:max]
}
