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
	cancelAll bool
	user      string
	printer   string
	jobs      []string
	server    string
	encrypt   bool
	authUser  string
	purge     bool
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.authUser),
	)

	if opts.cancelAll {
		if opts.printer == "" {
			if def, err := fetchDefaultDestination(client); err == nil && def != "" {
				opts.printer = def
			}
		}
		if opts.printer == "" {
			if err := cancelAllDestinations(client, opts.purge); err != nil {
				fail(err)
			}
		} else {
			if err := cancelAllJobs(client, opts.printer, opts.purge); err != nil {
				fail(err)
			}
		}
		return
	}

	if opts.user != "" {
		if err := cancelUserJobs(client, opts.user, opts.purge); err != nil {
			fail(err)
		}
		return
	}

	if len(opts.jobs) == 0 {
		if def, err := fetchDefaultDestination(client); err == nil && def != "" {
			if err := cancelCurrentJob(client, def, opts.purge); err != nil {
				fail(err)
			}
			return
		}
		fail(fmt.Errorf("missing job id"))
	}
	for _, job := range opts.jobs {
		if dest, id := splitJobSpec(job); id > 0 {
			if dest != "" {
				if err := cancelJobOnPrinter(client, dest, id, opts.purge); err != nil {
					fail(err)
				}
			} else if err := cancelJob(client, id, opts.purge); err != nil {
				fail(err)
			}
			continue
		}
		if err := cancelCurrentJob(client, job, opts.purge); err != nil {
			fail(err)
		}
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	seenOther := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h":
			if seenOther {
				return opts, fmt.Errorf("-h must appear before all other options")
			}
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -h")
			}
			i++
			opts.server = args[i]
		case "-E":
			opts.encrypt = true
		case "-U":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -U")
			}
			i++
			opts.authUser = args[i]
		case "-a":
			opts.cancelAll = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.printer = args[i]
			}
		case "-x":
			opts.purge = true
		case "-u":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -u")
			}
			i++
			opts.user = args[i]
		case "-P":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -P")
			}
			i++
			opts.printer = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return opts, fmt.Errorf("unknown option %s", args[i])
			}
			opts.jobs = append(opts.jobs, args[i])
		}
		if args[i] != "-h" && args[i] != "-E" && args[i] != "-U" {
			seenOther = true
		}
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cancel:", err)
	os.Exit(1)
}

func cancelJob(client *cupsclient.Client, jobID int, purge bool) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
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

func cancelJobOnPrinter(client *cupsclient.Client, printer string, jobID int, purge bool) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printer))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
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

func cancelAllJobs(client *cupsclient.Client, printer string, purge bool) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printer))))
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

func cancelUserJobs(client *cupsclient.Client, user string, purge bool) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	req.Operation.Add(goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))
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

func cancelCurrentJob(client *cupsclient.Client, printer string, purge bool) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printer))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
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

func cancelAllDestinations(client *cupsclient.Client, purge bool) error {
	printers, err := listPrinters(client)
	if err != nil {
		return err
	}
	for _, p := range printers {
		if err := cancelAllJobs(client, p, purge); err != nil {
			return err
		}
	}
	return nil
}

func listPrinters(client *cupsclient.Client) ([]string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPrinters, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		if name := findAttr(g.Attrs, "printer-name"); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func fetchDefaultDestination(client *cupsclient.Client) (string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return "", err
	}
	if name := findAttr(resp.Printer, "printer-name"); name != "" {
		return name, nil
	}
	return "", nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
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
			return value[:idx], n
		}
	}
	return value, 0
}

func parseJobID(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	if idx := strings.LastIndex(raw, "-"); idx != -1 {
		if n, err := strconv.Atoi(raw[idx+1:]); err == nil {
			return n
		}
	}
	return 0
}

func resolveDest(dest string) string {
	if dest != "" {
		return dest
	}
	if env := os.Getenv("PRINTER"); env != "" {
		return env
	}
	if env := os.Getenv("CUPS_PRINTER"); env != "" {
		return env
	}
	return ""
}
