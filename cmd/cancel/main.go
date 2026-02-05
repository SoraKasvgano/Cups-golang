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
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}

	client := cupsclient.NewFromEnv()

	if opts.cancelAll {
		if opts.printer == "" {
			opts.printer = resolveDest("")
		}
		if opts.printer == "" {
			fail(fmt.Errorf("missing destination for -a"))
		}
		if err := cancelAllJobs(client, opts.printer); err != nil {
			fail(err)
		}
		return
	}

	if opts.user != "" {
		if err := cancelUserJobs(client, opts.user); err != nil {
			fail(err)
		}
		return
	}

	if len(opts.jobs) == 0 {
		fail(fmt.Errorf("missing job id"))
	}
	for _, job := range opts.jobs {
		jobID := parseJobID(job)
		if jobID <= 0 {
			fail(fmt.Errorf("invalid job id: %s", job))
		}
		if err := cancelJob(client, jobID); err != nil {
			fail(err)
		}
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-a":
			opts.cancelAll = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.printer = args[i]
			}
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
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cancel:", err)
	os.Exit(1)
}

func cancelJob(client *cupsclient.Client, jobID int) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func cancelAllJobs(client *cupsclient.Client, printer string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printer))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func cancelUserJobs(client *cupsclient.Client, user string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	req.Operation.Add(goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
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
