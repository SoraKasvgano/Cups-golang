package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func main() {
	client := cupsclient.NewFromConfig()

	args := os.Args[1:]
	if len(args) > 0 {
		command := strings.TrimSpace(args[0])
		params := ""
		if len(args) > 1 {
			params = strings.TrimSpace(strings.Join(args[1:], " "))
		}
		doCommand(client, command, params)
		return
	}

	reader := bufio.NewScanner(os.Stdin)
	showPrompt("lpc> ")
	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			showPrompt("lpc> ")
			continue
		}
		parts := splitCommandLine(line)
		command := parts[0]
		params := ""
		if len(parts) > 1 {
			params = strings.TrimSpace(strings.Join(parts[1:], " "))
		}
		if isAbbrev(command, "quit", 1) || isAbbrev(command, "exit", 2) {
			break
		}
		doCommand(client, command, params)
		showPrompt("lpc> ")
	}
}

func showPrompt(message string) {
	_, _ = fmt.Fprint(os.Stdout, message)
}

func splitCommandLine(line string) []string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func isAbbrev(value, full string, min int) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if len(value) < min {
		return false
	}
	return strings.HasPrefix(full, value)
}

func doCommand(client *cupsclient.Client, command, params string) {
	switch {
	case isAbbrev(command, "status", 4):
		if err := showStatus(client, params); err != nil {
			_, _ = fmt.Fprintf(os.Stdout, "lpc: %v\n", err)
		}
	case command == "?" || isAbbrev(command, "help", 1):
		showHelp(params)
	default:
		_, _ = fmt.Fprintf(os.Stdout, "%s is not implemented by the CUPS version of lpc.\n", command)
	}
}

func showHelp(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		_, _ = fmt.Fprintln(os.Stdout, "Commands may be abbreviated.  Commands are:")
		_, _ = fmt.Fprintln(os.Stdout, "")
		_, _ = fmt.Fprintln(os.Stdout, "exit    help    quit    status  ?")
		return
	}
	switch {
	case command == "?" || isAbbrev(command, "help", 1):
		_, _ = fmt.Fprintln(os.Stdout, "help\t\tGet help on commands.")
	case isAbbrev(command, "status", 4):
		_, _ = fmt.Fprintln(os.Stdout, "status\t\tShow status of daemon and queue.")
	default:
		_, _ = fmt.Fprintln(os.Stdout, "?Invalid help command unknown.")
	}
}

type printerStatus struct {
	Name      string
	DeviceURI string
	Accepting bool
	State     int
	JobCount  int
}

func showStatus(client *cupsclient.Client, dests string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPrinters, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr(
		"requested-attributes",
		goipp.TagKeyword,
		goipp.String("device-uri"),
		goipp.String("printer-is-accepting-jobs"),
		goipp.String("printer-name"),
		goipp.String("printer-state"),
		goipp.String("queued-job-count"),
	))

	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if err := checkIPPStatus(resp); err != nil {
		return err
	}

	targets := parseStatusDestinations(dests)
	showAll := len(targets) == 0
	for _, ps := range parsePrinterStatuses(resp) {
		if strings.TrimSpace(ps.Name) == "" {
			continue
		}
		if !showAll && !destinationMatch(ps.Name, targets) {
			continue
		}
		printStatusEntry(ps)
	}
	return nil
}

func parsePrinterStatuses(resp *goipp.Message) []printerStatus {
	if resp == nil {
		return nil
	}
	out := []printerStatus{}
	for _, group := range resp.Groups {
		if group.Tag != goipp.TagPrinterGroup {
			continue
		}
		ps := printerStatus{
			Name:      strings.TrimSpace(findAttr(group.Attrs, "printer-name")),
			DeviceURI: strings.TrimSpace(findAttr(group.Attrs, "device-uri")),
			Accepting: attrBool(group.Attrs, "printer-is-accepting-jobs", true),
			State:     attrInt(group.Attrs, "printer-state", 3),
			JobCount:  attrInt(group.Attrs, "queued-job-count", 0),
		}
		if ps.DeviceURI == "" {
			ps.DeviceURI = "file:/dev/null"
		}
		out = append(out, ps)
	}
	return out
}

func parseStatusDestinations(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.EqualFold(raw, "all") {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func destinationMatch(name string, targets []string) bool {
	for _, target := range targets {
		if target == name {
			return true
		}
	}
	return false
}

func printStatusEntry(ps printerStatus) {
	_, _ = fmt.Fprintf(os.Stdout, "%s:\n", ps.Name)

	device := strings.TrimSpace(ps.DeviceURI)
	deviceDesc := strings.TrimPrefix(device, "file:")
	if !strings.HasPrefix(device, "file:") {
		if idx := strings.Index(device, ":"); idx > 0 {
			deviceDesc = device[:idx]
		} else {
			deviceDesc = device
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "\tprinter is on device '%s' speed -1\n", deviceDesc)

	if ps.Accepting {
		_, _ = fmt.Fprintln(os.Stdout, "\tqueuing is enabled")
	} else {
		_, _ = fmt.Fprintln(os.Stdout, "\tqueuing is disabled")
	}

	if ps.State != 5 {
		_, _ = fmt.Fprintln(os.Stdout, "\tprinting is enabled")
	} else {
		_, _ = fmt.Fprintln(os.Stdout, "\tprinting is disabled")
	}

	if ps.JobCount <= 0 {
		_, _ = fmt.Fprintln(os.Stdout, "\tno entries")
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "\t%d entries\n", ps.JobCount)
	}
	_, _ = fmt.Fprintln(os.Stdout, "\tdaemon present")
}

func checkIPPStatus(resp *goipp.Message) error {
	if resp == nil {
		return errors.New("empty ipp response")
	}
	status := goipp.Status(resp.Code)
	if status > goipp.StatusOkConflicting {
		return fmt.Errorf("%s", status)
	}
	return nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name == name && len(attr.Values) > 0 {
			return attr.Values[0].V.String()
		}
	}
	return ""
}

func attrInt(attrs goipp.Attributes, name string, fallback int) int {
	for _, attr := range attrs {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(attr.Values[0].V.String())); err == nil {
			return n
		}
	}
	return fallback
}

func attrBool(attrs goipp.Attributes, name string, fallback bool) bool {
	for _, attr := range attrs {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(attr.Values[0].V.String())) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return fallback
}
