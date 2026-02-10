package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	printer     string
	deviceURI   string
	description string
	location    string
	ppdFile     string
	ppdName     string
	deleteName  string
	defaultName string
	enable      bool
	encrypt     bool
	server      string
	user        string
	classAdd    string
	classRemove string
	extraOpts   map[string]string
}

func main() {
	opts := parseArgs(os.Args[1:])
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

	if opts.deleteName != "" {
		if err := deleteDestination(client, opts.deleteName); err != nil {
			fail(err)
		}
		return
	}

	if opts.printer != "" || opts.deviceURI != "" || opts.description != "" || opts.location != "" {
		if opts.printer == "" {
			fail(fmt.Errorf("missing -p printer"))
		}
		if err := addModifyPrinter(client, opts); err != nil {
			fail(err)
		}
	}

	if opts.classAdd != "" || opts.classRemove != "" {
		if opts.printer == "" {
			fail(fmt.Errorf("missing -p printer for class operation"))
		}
		if err := updateClassMembers(client, opts.printer, opts.classAdd, opts.classRemove); err != nil {
			fail(err)
		}
	}

	if opts.enable && opts.printer != "" {
		if err := resumePrinter(client, opts.printer); err != nil {
			fail(err)
		}
		if err := acceptPrinter(client, opts.printer); err != nil {
			fail(err)
		}
	}

	if opts.defaultName != "" {
		if err := setDefault(client, opts.defaultName); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpadmin:", err)
	os.Exit(1)
}

func parseArgs(args []string) options {
	opts := options{}
	seenDestOp := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h":
			if i+1 < len(args) {
				i++
				opts.server = args[i]
			}
		case "-U":
			if i+1 < len(args) {
				i++
				opts.user = args[i]
			}
		case "-p":
			if i+1 < len(args) {
				i++
				opts.printer = args[i]
				seenDestOp = true
			}
		case "-v":
			if i+1 < len(args) {
				i++
				opts.deviceURI = args[i]
				seenDestOp = true
			}
		case "-D":
			if i+1 < len(args) {
				i++
				opts.description = args[i]
				seenDestOp = true
			}
		case "-L":
			if i+1 < len(args) {
				i++
				opts.location = args[i]
				seenDestOp = true
			}
		case "-x":
			if i+1 < len(args) {
				i++
				opts.deleteName = args[i]
				seenDestOp = true
			}
		case "-P":
			if i+1 < len(args) {
				i++
				opts.ppdFile = args[i]
				seenDestOp = true
			}
		case "-m":
			if i+1 < len(args) {
				i++
				opts.ppdName = args[i]
				seenDestOp = true
			}
		case "-d":
			if i+1 < len(args) {
				i++
				opts.defaultName = args[i]
				seenDestOp = true
			}
		case "-E":
			if !seenDestOp {
				opts.encrypt = true
			} else {
				opts.enable = true
			}
		case "-o":
			if i+1 < len(args) {
				i++
				name, val := parseOption(args[i])
				if name != "" {
					if opts.extraOpts == nil {
						opts.extraOpts = map[string]string{}
					}
					opts.extraOpts[name] = val
				}
				seenDestOp = true
			}
		case "-c":
			if i+1 < len(args) {
				i++
				opts.classAdd = args[i]
				seenDestOp = true
			}
		case "-r":
			if i+1 < len(args) {
				i++
				opts.classRemove = args[i]
				seenDestOp = true
			}
		}
	}
	return opts
}

func addModifyPrinter(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(opts.printer))))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(opts.printer)))

	if opts.deviceURI != "" {
		req.Printer.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(opts.deviceURI)))
	}
	if opts.ppdName != "" {
		req.Printer.Add(goipp.MakeAttribute("ppd-name", goipp.TagName, goipp.String(opts.ppdName)))
	}
	if opts.description != "" {
		req.Printer.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(opts.description)))
	}
	if opts.location != "" {
		req.Printer.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(opts.location)))
	}
	applyLpadminOptions(req, opts.extraOpts)
	var payload *os.File
	if opts.ppdFile != "" {
		f, err := os.Open(opts.ppdFile)
		if err != nil {
			return err
		}
		payload = f
		defer payload.Close()
	}
	resp, err := client.Send(context.Background(), req, payload)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func applyLpadminOptions(req *goipp.Message, opts map[string]string) {
	if req == nil || len(opts) == 0 {
		return
	}
	for name, val := range opts {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		attrName, tag, values := normalizeLpadminOption(name, val)
		if attrName == "" || len(values) == 0 {
			continue
		}
		req.Printer.Add(goipp.MakeAttr(attrName, tag, values[0], values[1:]...))
	}
}

func normalizeLpadminOption(name, value string) (string, goipp.Tag, []goipp.Value) {
	lower := strings.ToLower(strings.TrimSpace(name))
	value = strings.TrimSpace(value)
	if value == "" {
		value = "true"
	}
	switch lower {
	case "printer-error-policy", "printer-op-policy", "port-monitor":
		return lower, goipp.TagName, []goipp.Value{goipp.String(value)}
	case "printer-is-shared":
		return "printer-is-shared", goipp.TagBoolean, []goipp.Value{goipp.Boolean(isTruthy(value))}
	case "job-sheets", "job-sheets-default":
		vals := splitList(value, 2)
		if len(vals) == 0 {
			vals = []string{"none", "none"}
		} else if len(vals) == 1 {
			vals = append(vals, "none")
		}
		out := make([]goipp.Value, 0, len(vals))
		for _, v := range vals {
			out = append(out, goipp.String(v))
		}
		return "job-sheets-default", goipp.TagName, out
	}

	attrName := lower
	if !strings.HasSuffix(attrName, "-default") && isJobDefaultOption(attrName) {
		attrName = attrName + "-default"
	}
	switch attrName {
	case "copies-default", "job-priority-default", "number-up-default", "job-cancel-after-default", "number-of-retries-default", "retry-interval-default", "retry-time-out-default":
		if n, err := strconv.Atoi(value); err == nil {
			return attrName, goipp.TagInteger, []goipp.Value{goipp.Integer(n)}
		}
	case "print-quality-default", "orientation-requested-default":
		if n, err := strconv.Atoi(value); err == nil {
			return attrName, goipp.TagEnum, []goipp.Value{goipp.Integer(n)}
		}
	}
	return attrName, goipp.TagKeyword, []goipp.Value{goipp.String(value)}
}

func isJobDefaultOption(name string) bool {
	switch name {
	case "media", "media-col", "media-source", "media-type", "sides", "print-quality",
		"print-color-mode", "output-mode", "finishings", "finishings-col", "output-bin",
		"number-up", "orientation-requested", "print-scaling", "printer-resolution",
		"page-ranges", "page-delivery", "multiple-document-handling", "print-as-raster",
		"job-hold-until", "job-priority", "job-cancel-after", "job-account-id", "job-accounting-user-id":
		return true
	default:
		return false
	}
}

func parseOption(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.Contains(raw, "=") {
		parts := strings.SplitN(raw, "=", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return raw, "true"
}

func splitList(value string, max int) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func deleteDestination(client *cupsclient.Client, name string) error {
	if name == "" {
		return nil
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeletePrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err == nil && goipp.Status(resp.Code) < goipp.StatusRedirectionOtherSite {
		return nil
	}

	// Try delete class if printer delete failed.
	req = goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeleteClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	resp, err = client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func setDefault(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsSetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func resumePrinter(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpResumePrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func acceptPrinter(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAcceptJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func updateClassMembers(client *cupsclient.Client, printerName, classAdd, classRemove string) error {
	className := classAdd
	if className == "" {
		className = classRemove
	}
	if className == "" {
		return nil
	}

	members, err := fetchClassMembers(client, className)
	if err != nil {
		return err
	}

	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m] = true
	}
	if classAdd != "" {
		memberSet[printerName] = true
	}
	if classRemove != "" {
		delete(memberSet, printerName)
	}

	memberList := make([]goipp.Value, 0, len(memberSet))
	for name := range memberSet {
		memberList = append(memberList, goipp.String(client.PrinterURI(name)))
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(className)))
	if len(memberList) > 0 {
		req.Printer.Add(goipp.MakeAttr("member-uris", goipp.TagURI, memberList[0], memberList[1:]...))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func fetchClassMembers(client *cupsclient.Client, className string) ([]string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetClasses, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return nil, err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		name := findAttr(g.Attrs, "printer-name")
		if !strings.EqualFold(name, className) {
			continue
		}
		return attrValues(g.Attrs, "member-uris"), nil
	}
	return nil, nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func attrValues(attrs goipp.Attributes, name string) []string {
	out := []string{}
	for _, a := range attrs {
		if a.Name != name {
			continue
		}
		for _, v := range a.Values {
			out = append(out, v.V.String())
		}
	}
	return out
}
