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
	server      string
	encrypt     bool
	user        string
	destination string
	targets     []string
}

type destinationKind int

const (
	destinationUnknown destinationKind = iota
	destinationPrinter
	destinationClass
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

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

	known, err := listDestinations(client)
	if err != nil {
		fail(err)
	}
	if opts.destination != "" && !isKnownDestination(opts.destination, known) {
		fail(fmt.Errorf("unknown destination %q", opts.destination))
	}

	activeDest := strings.TrimSpace(opts.destination)
	didCancel := false
	for _, raw := range opts.targets {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}

		switch {
		case target == "-":
			if activeDest == "" {
				def, err := defaultDestinationName(client)
				if err != nil {
					fail(err)
				}
				activeDest = def
			}
			if err := cancelAllJobs(client, activeDest); err != nil {
				fail(err)
			}
			didCancel = true

		case isPositiveInt(target):
			jobID, _ := strconv.Atoi(target)
			if err := cancelJobID(client, activeDest, jobID); err != nil {
				fail(err)
			}
			didCancel = true

		case isKnownDestination(target, known):
			activeDest = sanitizeDestination(target)
			if err := cancelCurrentJob(client, activeDest); err != nil {
				fail(err)
			}
			didCancel = true

		default:
			fail(fmt.Errorf("unknown destination %q", target))
		}
	}

	if didCancel {
		return
	}
	if activeDest == "" {
		def, err := defaultDestinationName(client)
		if err != nil {
			fail(err)
		}
		activeDest = def
	}
	if err := cancelCurrentJob(client, activeDest); err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Println("Usage: lprm [options] [id]")
	fmt.Println("       lprm [options] -")
	fmt.Println("Options:")
	fmt.Println("-                       Cancel all jobs")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-h server[:port]        Connect to the named server and port")
	fmt.Println("-P destination          Specify the destination")
	fmt.Println("-U username             Specify the username to use for authentication")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lprm:", err)
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
		if strings.HasPrefix(arg, "-") && arg != "-" {
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
				case 'E':
					opts.encrypt = true
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
				case 'P':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.destination, _ = parseDestinationSpec(v)
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		opts.targets = append(opts.targets, arg)
	}
	return opts, nil
}

func sanitizeDestination(raw string) string {
	destination, _ := parseDestinationSpec(raw)
	return destination
}

func parseDestinationSpec(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.HasPrefix(raw, "ipp://") || strings.HasPrefix(raw, "ipps://") {
		if parsed, err := url.Parse(raw); err == nil {
			raw = strings.Trim(parsed.Path, "/")
			raw = strings.TrimPrefix(raw, "printers/")
			raw = strings.TrimPrefix(raw, "classes/")
		}
	}
	destination := raw
	instance := ""
	if idx := strings.Index(destination, "/"); idx >= 0 {
		instance = strings.TrimSpace(destination[idx+1:])
		destination = destination[:idx]
	}
	return strings.TrimSpace(destination), strings.TrimSpace(instance)
}

func isPositiveInt(value string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && n > 0
}

func cancelCurrentJob(client *cupsclient.Client, destination string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
}

func cancelJobID(client *cupsclient.Client, destination string, jobID int) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)
	if destination != "" {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
		req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	} else {
		req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI(jobID))))
	}
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
}

func cancelAllJobs(client *cupsclient.Client, destination string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
}

func addOperationDefaults(req *goipp.Message) {
	if req == nil {
		return
	}
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
}

func destinationURI(destination string) string {
	return fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(strings.TrimSpace(destination)))
}

func jobURI(jobID int) string {
	return fmt.Sprintf("ipp://localhost/jobs/%d", jobID)
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

func listDestinations(client *cupsclient.Client) (map[string]destinationKind, error) {
	out := map[string]destinationKind{}
	for _, item := range []struct {
		op   goipp.Op
		kind destinationKind
	}{
		{op: goipp.OpCupsGetPrinters, kind: destinationPrinter},
		{op: goipp.OpCupsGetClasses, kind: destinationClass},
	} {
		req := goipp.NewRequest(goipp.DefaultVersion, item.op, uint32(time.Now().UnixNano()))
		addOperationDefaults(req)
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
		resp, err := client.Send(context.Background(), req, nil)
		if err != nil {
			return nil, err
		}
		if err := checkIPPStatus(resp); err != nil {
			return nil, err
		}
		for _, group := range resp.Groups {
			if group.Tag != goipp.TagPrinterGroup {
				continue
			}
			name := strings.TrimSpace(findAttr(group.Attrs, "printer-name"))
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if existing, exists := out[key]; !exists || item.kind == destinationClass || existing == destinationUnknown {
				out[key] = item.kind
			}
		}
	}
	return out, nil
}

func defaultDestinationName(client *cupsclient.Client) (string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDefault, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return "", err
	}
	if err := checkIPPStatus(resp); err != nil {
		return "", err
	}
	for _, group := range resp.Groups {
		if group.Tag != goipp.TagPrinterGroup {
			continue
		}
		if name := strings.TrimSpace(findAttr(group.Attrs, "printer-name")); name != "" {
			return name, nil
		}
	}
	return "", errors.New("no default destination available")
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

func isKnownDestination(value string, known map[string]destinationKind) bool {
	value = sanitizeDestination(value)
	if value == "" {
		return false
	}
	kind, ok := known[strings.ToLower(value)]
	return ok && kind != destinationUnknown
}
