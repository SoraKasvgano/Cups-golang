package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

var errShowHelp = errors.New("show-help")

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
	removeOpts  []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errShowHelp) {
		usage()
		return
	}
	if err != nil {
		fail(err)
	}
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

	if opts.printer != "" || opts.deviceURI != "" || opts.description != "" || opts.location != "" || len(opts.extraOpts) > 0 || len(opts.removeOpts) > 0 || opts.ppdFile != "" || opts.ppdName != "" {
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

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: lpadmin [options]")
	fmt.Fprintln(os.Stderr, "Options:")
	fmt.Fprintln(os.Stderr, "  -E                      Encrypt connection or enable queue when used after -p")
	fmt.Fprintln(os.Stderr, "  -h server[:port]        Connect to server")
	fmt.Fprintln(os.Stderr, "  -U username             Authenticate as user")
	fmt.Fprintln(os.Stderr, "  -p printer              Add/modify printer")
	fmt.Fprintln(os.Stderr, "  -v device-uri           Set device URI")
	fmt.Fprintln(os.Stderr, "  -m model                Set ppd-name")
	fmt.Fprintln(os.Stderr, "  -P file                 Upload PPD file")
	fmt.Fprintln(os.Stderr, "  -i file                 Alias for -P")
	fmt.Fprintln(os.Stderr, "  -o name=value           Set default option")
	fmt.Fprintln(os.Stderr, "  -R name                 Remove default option")
	fmt.Fprintln(os.Stderr, "  -D info                 Set printer-info")
	fmt.Fprintln(os.Stderr, "  -L location             Set printer-location")
	fmt.Fprintln(os.Stderr, "  -c class                Add printer to class")
	fmt.Fprintln(os.Stderr, "  -r class                Remove printer from class")
	fmt.Fprintln(os.Stderr, "  -d printer              Set default destination")
	fmt.Fprintln(os.Stderr, "  -x destination          Delete printer/class")
	os.Exit(1)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpadmin:", err)
	os.Exit(1)
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if arg == "--help" {
			return opts, errShowHelp
		}
		if strings.HasPrefix(arg, "--") {
			return opts, fmt.Errorf("unknown option %q", arg)
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}

		short := strings.TrimPrefix(arg, "-")
		for pos := 0; pos < len(short); pos++ {
			ch := short[pos]
			rest := short[pos+1:]
			consume := func(name byte) (string, error) {
				if rest != "" {
					pos = len(short)
					return rest, nil
				}
				if i+1 >= len(args) {
					return "", fmt.Errorf("missing argument for -%c", name)
				}
				i++
				return args[i], nil
			}
			switch ch {
			case 'h':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.server = strings.TrimSpace(v)
			case 'U':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.user = strings.TrimSpace(v)
			case 'p':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.printer = strings.TrimSpace(v)
			case 'v':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.deviceURI = strings.TrimSpace(v)
			case 'D':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.description = strings.TrimSpace(v)
			case 'L':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.location = strings.TrimSpace(v)
			case 'x':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.deleteName = strings.TrimSpace(v)
			case 'P', 'i':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.ppdFile = strings.TrimSpace(v)
			case 'm':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.ppdName = strings.TrimSpace(v)
			case 'd':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.defaultName = strings.TrimSpace(v)
			case 'E':
				if strings.TrimSpace(opts.printer) == "" {
					opts.encrypt = true
				} else {
					opts.enable = true
				}
			case 'o':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				name, val := parseOption(v)
				if name == "" {
					return opts, fmt.Errorf("invalid -o value %q", v)
				}
				if opts.extraOpts == nil {
					opts.extraOpts = map[string]string{}
				}
				opts.extraOpts[name] = val
			case 'R':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				for _, name := range splitList(v, 0) {
					name = strings.TrimSpace(name)
					if name != "" {
						opts.removeOpts = append(opts.removeOpts, name)
					}
				}
			case 'c':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.classAdd = strings.TrimSpace(v)
			case 'r':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.classRemove = strings.TrimSpace(v)
			default:
				return opts, fmt.Errorf("unknown option \"-%c\"", ch)
			}
		}
	}
	return opts, nil
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
	applyLpadminRemovals(req, opts.removeOpts)
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

func applyLpadminRemovals(req *goipp.Message, names []string) {
	if req == nil || len(names) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, raw := range names {
		attrName := normalizeLpadminRemoveOption(raw)
		if attrName == "" {
			continue
		}
		if seen[attrName] {
			continue
		}
		seen[attrName] = true
		req.Printer.Add(goipp.MakeAttribute(attrName, goipp.TagDeleteAttr, goipp.Void{}))
	}
}

func normalizeLpadminRemoveOption(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	switch name {
	case "job-sheets":
		return "job-sheets-default"
	case "printer-error-policy", "printer-op-policy", "port-monitor", "printer-is-shared":
		return name
	}
	if strings.HasSuffix(name, "-default") {
		return name
	}
	if isJobDefaultOption(name) {
		return name + "-default"
	}
	return name
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
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
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
		uris := attrValues(g.Attrs, "member-uris")
		members := make([]string, 0, len(uris))
		for _, uri := range uris {
			if n := destinationNameFromURI(uri); n != "" {
				members = append(members, n)
			}
		}
		return members, nil
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

func destinationNameFromURI(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	if u, err := url.Parse(uri); err == nil {
		path := strings.Trim(u.Path, "/")
		if path == "" {
			return ""
		}
		parts := strings.Split(path, "/")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	if idx := strings.LastIndex(uri, "/"); idx >= 0 && idx+1 < len(uri) {
		return strings.TrimSpace(uri[idx+1:])
	}
	return strings.TrimSpace(uri)
}
