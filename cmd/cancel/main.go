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
	cancelAll bool
	user      string
	jobs      []string
	server    string
	encrypt   bool
	authUser  string
	purge     bool
	destKinds map[string]destinationKind
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
		cupsclient.WithUser(opts.authUser),
	)

	known, _ := listDestinations(client)
	opts.destKinds = known

	if len(opts.jobs) == 0 {
		if err := cancelWithoutExplicitTargets(client, opts); err != nil {
			fail(err)
		}
		return
	}

	for i := 0; i < len(opts.jobs); i++ {
		target := strings.TrimSpace(opts.jobs[i])
		if target == "" {
			continue
		}

		if target == "-" {
			if err := cancelCurrentJob(client, "", opts.purge, opts.user); err != nil {
				fail(err)
			}
			continue
		}

		if isKnownDestination(target, known) {
			if err := cancelDestinationTarget(client, opts, target); err != nil {
				fail(err)
			}
			continue
		}

		dest, id := splitJobSpec(target)
		if id > 0 {
			if err := cancelJobTarget(client, opts, dest, id); err != nil {
				fail(err)
			}

			// Solaris LP compatibility: ignore a destination name after a
			// destination-id operand (for example "cancel Office-123 Office").
			if i+1 < len(opts.jobs) && isKnownDestination(opts.jobs[i+1], known) {
				i++
			}
			continue
		}

		fail(fmt.Errorf("unknown destination %q", target))
	}
}

func usage() {
	fmt.Println("Usage: cancel [options] [id]")
	fmt.Println("       cancel [options] [destination]")
	fmt.Println("       cancel [options] [destination-id]")
	fmt.Println("Options:")
	fmt.Println("-a                      Cancel all jobs")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-h server[:port]        Connect to the named server and port")
	fmt.Println("-u owner                Specify the owner to use for jobs")
	fmt.Println("-U username             Specify the username to use for authentication")
	fmt.Println("-x                      Purge jobs rather than just canceling")
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
				case 'a':
					opts.cancelAll = true
				case 'E':
					opts.encrypt = true
				case 'h':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.server = strings.TrimSpace(v)
				case 'u':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.user = strings.TrimSpace(v)
				case 'U':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.authUser = strings.TrimSpace(v)
				case 'x':
					opts.purge = true
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		opts.jobs = append(opts.jobs, arg)
	}

	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cancel:", err)
	os.Exit(1)
}

func cancelWithoutExplicitTargets(client *cupsclient.Client, opts options) error {
	switch {
	case opts.cancelAll && opts.user != "":
		return cancelUserJobs(client, opts.user, opts.purge, "")
	case opts.cancelAll:
		return cancelAllJobsEverywhere(client, opts.purge, opts.user)
	case opts.user != "":
		return cancelUserJobs(client, opts.user, opts.purge, "")
	default:
		// Match CUPS behavior: no explicit target and no scope flags means no-op.
		return nil
	}
}

func cancelDestinationTarget(client *cupsclient.Client, opts options, destination string) error {
	switch {
	case opts.cancelAll && opts.user != "":
		return cancelUserJobs(client, opts.user, opts.purge, destination, opts.destKinds)
	case opts.cancelAll:
		return cancelAllJobs(client, destination, opts.purge, opts.user, opts.destKinds)
	case opts.user != "":
		return cancelUserJobs(client, opts.user, opts.purge, destination, opts.destKinds)
	default:
		return cancelCurrentJob(client, destination, opts.purge, "", opts.destKinds)
	}
}

func cancelJobTarget(client *cupsclient.Client, opts options, destination string, jobID int) error {
	user := opts.user
	switch {
	case destination != "":
		return cancelJobOnPrinter(client, destination, jobID, opts.purge, user, opts.destKinds)
	default:
		return cancelJob(client, jobID, opts.purge, user)
	}
}

func cancelJob(client *cupsclient.Client, jobID int, purge bool, user string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI(jobID))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-job", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelJobOnPrinter(client *cupsclient.Client, printer string, jobID int, purge bool, user string, known ...map[string]destinationKind) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(client, printer, known...))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-job", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelAllJobs(client *cupsclient.Client, printer string, purge bool, user string, known ...map[string]destinationKind) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(client, printer, known...))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelUserJobs(client *cupsclient.Client, user string, purge bool, printer string, known ...map[string]destinationKind) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelMyJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(client, printer, known...))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelCurrentJob(client *cupsclient.Client, printer string, purge bool, user string, known ...map[string]destinationKind) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(client, printer, known...))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-job", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelAllJobsEverywhere(client *cupsclient.Client, purge bool, user string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(""))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(effectiveRequestingUser(client, user))))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(true)))
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
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
		resp, err := client.Send(context.Background(), req, nil)
		if err != nil {
			if len(out) > 0 {
				continue
			}
			return nil, err
		}
		for _, g := range resp.Groups {
			if g.Tag != goipp.TagPrinterGroup {
				continue
			}
			if name := strings.TrimSpace(findAttr(g.Attrs, "printer-name")); name != "" {
				key := strings.ToLower(name)
				if existing, exists := out[key]; !exists || item.kind == destinationClass || existing == destinationUnknown {
					out[key] = item.kind
				}
			}
		}
	}
	return out, nil
}

func isKnownDestination(value string, known map[string]destinationKind) bool {
	v := strings.TrimSpace(value)
	if v == "" || v == "-" {
		return true
	}
	if len(known) == 0 {
		_, id := splitJobSpec(v)
		return id == 0
	}
	kind, ok := known[strings.ToLower(v)]
	return ok && kind != destinationUnknown
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func destinationURI(client *cupsclient.Client, destination string, known ...map[string]destinationKind) string {
	_ = client
	_ = known
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return "ipp://localhost/printers/"
	}
	if strings.Contains(destination, "://") {
		return destination
	}
	return fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(destination))
}

func effectiveRequestingUser(client *cupsclient.Client, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if client != nil {
		if v := strings.TrimSpace(client.User); v != "" {
			return v
		}
	}
	for _, key := range []string{"CUPS_USER", "USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "anonymous"
}

func splitJobSpec(value string) (string, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0
	}
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		return "", n
	}
	if idx := strings.LastIndex(value, "-"); idx != -1 && idx < len(value)-1 {
		if n, err := strconv.Atoi(value[idx+1:]); err == nil && n > 0 {
			// CUPS treats destination-id operands as job IDs.
			return "", n
		}
	}
	return "", 0
}

func jobURI(jobID int) string {
	return fmt.Sprintf("ipp://localhost/jobs/%d", jobID)
}
