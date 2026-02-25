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
	warnings    []string
}

type classMemberAction string

const (
	classMemberNoop   classMemberAction = "noop"
	classMemberSet    classMemberAction = "set"
	classMemberDelete classMemberAction = "delete"
)

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errShowHelp) {
		usage()
		return
	}
	if err != nil {
		fail(err)
	}
	for _, warning := range opts.warnings {
		if strings.TrimSpace(warning) != "" {
			fmt.Fprintln(os.Stderr, "lpadmin:", warning)
		}
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
	fmt.Fprintln(os.Stderr, "  -u allow:userlist|deny:userlist")
	fmt.Fprintln(os.Stderr, "  -A file                 Not supported (System V interfaces are removed)")
	fmt.Fprintln(os.Stderr, "  -I type-list            Accepted for compatibility; ignored")
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
			case 'A':
				_, err := consume(ch)
				if err != nil {
					return opts, err
				}
				return opts, fmt.Errorf("System V interface scripts are no longer supported for security reasons")
			case 'I':
				_, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.warnings = append(opts.warnings, "Warning - content type list ignored.")
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
			case 'u':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				attrName, users, ok := parseAccessUsersOption(v)
				if !ok {
					return opts, fmt.Errorf("unknown allow/deny option %q", strings.TrimSpace(v))
				}
				if opts.extraOpts == nil {
					opts.extraOpts = map[string]string{}
				}
				opts.extraOpts[attrName] = users
				if attrName == "requesting-user-name-allowed" {
					delete(opts.extraOpts, "requesting-user-name-denied")
				} else {
					delete(opts.extraOpts, "requesting-user-name-allowed")
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
	if opts.printer != "" && !validateName(opts.printer) {
		return opts, fmt.Errorf("printer name can only contain printable characters")
	}
	if opts.classAdd != "" && !validateName(opts.classAdd) {
		return opts, fmt.Errorf("class name can only contain printable characters")
	}
	if opts.classRemove != "" && !validateName(opts.classRemove) {
		return opts, fmt.Errorf("class name can only contain printable characters")
	}
	if opts.defaultName != "" && !validateName(opts.defaultName) {
		return opts, fmt.Errorf("printer name can only contain printable characters")
	}
	if opts.deleteName != "" && !validateName(opts.deleteName) {
		return opts, fmt.Errorf("printer name can only contain printable characters")
	}
	return opts, nil
}

func validateName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	n := 0
	for _, r := range name {
		if r == '@' {
			break
		}
		n++
		if (r >= 0 && r <= ' ') || r == 127 || r == '/' || r == '\\' || r == '?' || r == '\'' || r == '"' || r == '#' {
			return false
		}
	}
	return n < 128
}

func addModifyPrinter(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(opts.printer))))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(opts.printer)))
	addRequestingUserName(&req.Operation, client)

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
	case "requesting-user-name-allowed":
		vals := splitList(value, 0)
		if len(vals) == 0 {
			vals = []string{"all"}
		}
		out := make([]goipp.Value, 0, len(vals))
		for _, v := range vals {
			out = append(out, goipp.String(v))
		}
		return lower, goipp.TagName, out
	case "requesting-user-name-denied":
		vals := splitList(value, 0)
		if len(vals) == 0 {
			vals = []string{"none"}
		}
		out := make([]goipp.Value, 0, len(vals))
		for _, v := range vals {
			out = append(out, goipp.String(v))
		}
		return lower, goipp.TagName, out
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

func parseAccessUsersOption(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "allow:") {
		return "requesting-user-name-allowed", strings.TrimSpace(raw[len("allow:"):]), true
	}
	if strings.HasPrefix(lower, "deny:") {
		return "requesting-user-name-denied", strings.TrimSpace(raw[len("deny:"):]), true
	}
	return "", "", false
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
	addRequestingUserName(&req.Operation, client)
	resp, err := client.Send(context.Background(), req, nil)
	if err == nil && goipp.Status(resp.Code) < goipp.StatusRedirectionOtherSite {
		return nil
	}
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) != goipp.StatusErrorNotFound {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}

	// Match CUPS behavior: only attempt class deletion when printer delete
	// reports a missing destination.
	req = goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeleteClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(classURI(client, name))))
	addRequestingUserName(&req.Operation, client)
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
	addRequestingUserName(&req.Operation, client)
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
	addRequestingUserName(&req.Operation, client)
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
	addRequestingUserName(&req.Operation, client)
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
	if strings.TrimSpace(printerName) == "" {
		return fmt.Errorf("missing printer name")
	}
	addClass := strings.TrimSpace(classAdd)
	removeClass := strings.TrimSpace(classRemove)
	if addClass != "" && removeClass != "" && !strings.EqualFold(addClass, removeClass) {
		return fmt.Errorf("cannot combine -c %s with -r %s", addClass, removeClass)
	}
	className := addClass
	if className == "" {
		className = removeClass
	}
	if className == "" {
		return nil
	}

	members, memberURIs, found, err := fetchClassMembers(client, className)
	if err != nil {
		return err
	}
	action, updatedMembers, err := computeClassMembersUpdate(found, members, printerName, addClass != "", removeClass != "", className)
	if err != nil {
		return err
	}
	switch action {
	case classMemberNoop:
		return nil
	case classMemberDelete:
		return deleteClass(client, className)
	case classMemberSet:
		updatedURIs := classMemberURIsForNames(updatedMembers, members, memberURIs, client)
		return setClassMemberURIs(client, className, updatedURIs)
	default:
		return fmt.Errorf("unsupported class update action %q", action)
	}
}

func fetchClassMembers(client *cupsclient.Client, className string) ([]string, []string, bool, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(classURI(client, className))))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("member-names"), goipp.String("member-uris")))
	addRequestingUserName(&req.Operation, client)
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return nil, nil, false, err
	}
	status := goipp.Status(resp.Code)
	if status == goipp.StatusErrorNotFound {
		return nil, nil, false, nil
	}
	if status >= goipp.StatusRedirectionOtherSite {
		return nil, nil, false, fmt.Errorf("%s", status)
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		memberNames := attrValues(g.Attrs, "member-names")
		memberURIs := attrValues(g.Attrs, "member-uris")
		if len(memberNames) == 0 && len(memberURIs) > 0 {
			for _, memberURI := range memberURIs {
				memberNames = append(memberNames, destinationNameFromURI(memberURI))
			}
		}
		if len(memberURIs) < len(memberNames) {
			for i := len(memberURIs); i < len(memberNames); i++ {
				name := strings.TrimSpace(memberNames[i])
				if name == "" {
					memberURIs = append(memberURIs, "")
					continue
				}
				memberURIs = append(memberURIs, client.PrinterURI(name))
			}
		}
		if len(memberNames) < len(memberURIs) {
			for i := len(memberNames); i < len(memberURIs); i++ {
				memberNames = append(memberNames, destinationNameFromURI(memberURIs[i]))
			}
		}
		names, uris := normalizeClassMembers(memberNames, memberURIs)
		return names, uris, true, nil
	}
	return nil, nil, true, nil
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
		name := strings.TrimSpace(parts[len(parts)-1])
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		return name
	}
	if idx := strings.LastIndex(uri, "/"); idx >= 0 && idx+1 < len(uri) {
		name := strings.TrimSpace(uri[idx+1:])
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		return name
	}
	return strings.TrimSpace(uri)
}

func computeClassMembersUpdate(found bool, existing []string, printerName string, wantAdd, wantRemove bool, className string) (classMemberAction, []string, error) {
	members := normalizeDestinationList(existing)
	printerName = strings.TrimSpace(printerName)
	if printerName == "" {
		return classMemberNoop, nil, fmt.Errorf("missing printer name")
	}
	if !found {
		if wantRemove && !wantAdd {
			return classMemberNoop, nil, fmt.Errorf("class %s does not exist", className)
		}
		members = nil
	}

	changed := false
	if wantRemove {
		var removed bool
		members, removed = removeDestinationName(members, printerName)
		if !removed && !wantAdd {
			return classMemberNoop, nil, fmt.Errorf("printer %s is not a member of class %s", printerName, className)
		}
		changed = changed || removed
	}
	if wantAdd && !containsDestinationName(members, printerName) {
		members = append(members, printerName)
		changed = true
	}
	if !changed {
		return classMemberNoop, normalizeDestinationList(members), nil
	}
	if len(members) == 0 {
		return classMemberDelete, nil, nil
	}
	return classMemberSet, normalizeDestinationList(members), nil
}

func containsDestinationName(names []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), target) {
			return true
		}
	}
	return false
}

func removeDestinationName(names []string, target string) ([]string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return normalizeDestinationList(names), false
	}
	out := make([]string, 0, len(names))
	removed := false
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), target) {
			removed = true
			continue
		}
		out = append(out, name)
	}
	return normalizeDestinationList(out), removed
}

func normalizeDestinationList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

func setClassMembers(client *cupsclient.Client, className string, members []string) error {
	return setClassMemberURIs(client, className, classMemberURIsForNames(members, nil, nil, client))
}

func setClassMemberURIs(client *cupsclient.Client, className string, memberURIs []string) error {
	memberURIs = normalizeMemberURIs(memberURIs)
	if len(memberURIs) == 0 {
		return deleteClass(client, className)
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(classURI(client, className))))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(className)))
	addRequestingUserName(&req.Operation, client)

	values := make([]goipp.Value, 0, len(memberURIs))
	for _, memberURI := range memberURIs {
		values = append(values, goipp.String(memberURI))
	}
	req.Printer.Add(goipp.MakeAttr("member-uris", goipp.TagURI, values[0], values[1:]...))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func deleteClass(client *cupsclient.Client, className string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeleteClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(classURI(client, className))))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(className)))
	addRequestingUserName(&req.Operation, client)
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func classURI(client *cupsclient.Client, className string) string {
	_ = client
	return fmt.Sprintf("ipp://localhost/classes/%s", url.PathEscape(strings.TrimSpace(className)))
}

func classMemberURIsForNames(updatedNames, existingNames, existingURIs []string, client *cupsclient.Client) []string {
	updatedNames = normalizeDestinationList(updatedNames)
	if len(updatedNames) == 0 {
		return nil
	}
	memberURIs := make([]string, 0, len(updatedNames))
	for _, name := range updatedNames {
		idx := indexDestinationName(existingNames, name)
		if idx >= 0 && idx < len(existingURIs) {
			if uri := strings.TrimSpace(existingURIs[idx]); uri != "" {
				memberURIs = append(memberURIs, uri)
				continue
			}
		}
		memberURIs = append(memberURIs, client.PrinterURI(name))
	}
	return normalizeMemberURIs(memberURIs)
}

func indexDestinationName(names []string, target string) int {
	target = strings.TrimSpace(target)
	if target == "" {
		return -1
	}
	for idx, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), target) {
			return idx
		}
	}
	return -1
}

func normalizeMemberURIs(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		uri := strings.TrimSpace(value)
		if uri == "" {
			continue
		}
		key := strings.ToLower(uri)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, uri)
	}
	return out
}

func normalizeClassMembers(names, uris []string) ([]string, []string) {
	if len(names) == 0 && len(uris) == 0 {
		return nil, nil
	}
	if len(names) < len(uris) {
		for i := len(names); i < len(uris); i++ {
			names = append(names, destinationNameFromURI(uris[i]))
		}
	}
	if len(uris) < len(names) {
		for i := len(uris); i < len(names); i++ {
			name := strings.TrimSpace(names[i])
			if name == "" {
				uris = append(uris, "")
				continue
			}
			uris = append(uris, fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(name)))
		}
	}

	seen := map[string]bool{}
	outNames := make([]string, 0, len(names))
	outURIs := make([]string, 0, len(names))
	for i := 0; i < len(names) && i < len(uris); i++ {
		name := strings.TrimSpace(names[i])
		uri := strings.TrimSpace(uris[i])
		if name == "" && uri != "" {
			name = destinationNameFromURI(uri)
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		if uri == "" {
			uri = fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(name))
		}
		outNames = append(outNames, name)
		outURIs = append(outURIs, uri)
	}
	return outNames, outURIs
}

func addRequestingUserName(attrs *goipp.Attributes, client *cupsclient.Client) {
	if attrs == nil {
		return
	}
	user := strings.TrimSpace(requestingUserName(client))
	if user == "" {
		return
	}
	attrs.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
}

func requestingUserName(client *cupsclient.Client) string {
	if client != nil {
		if user := strings.TrimSpace(client.User); user != "" {
			return user
		}
	}
	for _, key := range []string{"CUPS_USER", "USER", "USERNAME"} {
		if user := strings.TrimSpace(os.Getenv(key)); user != "" {
			return user
		}
	}
	return "anonymous"
}
