package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	dest     string
	copies   int
	title    string
	hold     string
	jobID    string
	priority int
	user     string
	opts     []string
	files    []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lp:", err)
		os.Exit(1)
	}

	client := cupsclient.NewFromEnv()
	user := opts.user
	if user == "" {
		user = client.User
	}

	if opts.jobID != "" {
		jobID := parseJobID(opts.jobID)
		if jobID <= 0 {
			fmt.Fprintln(os.Stderr, "lp: invalid job id")
			os.Exit(1)
		}
		if err := modifyJob(client, user, jobID, opts); err != nil {
			fmt.Fprintln(os.Stderr, "lp:", err)
			os.Exit(1)
		}
		return
	}

	dest := resolveDest(opts.dest)
	if dest == "" {
		dest = "Default"
	}

	if len(opts.files) > 1 {
		for _, f := range opts.files {
			if f == "-" {
				fmt.Fprintln(os.Stderr, "lp: '-' can only be used for a single document")
				os.Exit(1)
			}
		}
		if err := createJobAndSend(client, user, dest, opts); err != nil {
			fmt.Fprintln(os.Stderr, "lp:", err)
			os.Exit(1)
		}
		return
	}

	var input io.Reader
	fileName := ""
	if len(opts.files) == 1 && opts.files[0] != "-" {
		fileName = opts.files[0]
		f, err := os.Open(fileName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "lp:", err)
			os.Exit(1)
		}
		defer f.Close()
		input = f
	} else {
		input = os.Stdin
	}

	if err := printJob(client, user, dest, opts, fileName, input); err != nil {
		fmt.Fprintln(os.Stderr, "lp:", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-d":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -d")
			}
			i++
			opts.dest = args[i]
		case "-n":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -n")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid copies")
			}
			opts.copies = n
		case "-t":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -t")
			}
			i++
			opts.title = args[i]
		case "-o":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -o")
			}
			i++
			opts.opts = append(opts.opts, args[i])
		case "-H":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -H")
			}
			i++
			opts.hold = args[i]
		case "-i":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -i")
			}
			i++
			opts.jobID = args[i]
		case "-q":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -q")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid priority")
			}
			opts.priority = n
		case "-U":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -U")
			}
			i++
			opts.user = args[i]
		case "-P":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -P")
			}
			i++
			opts.opts = append(opts.opts, "page-ranges="+args[i])
		case "-h":
			opts.opts = append(opts.opts, "job-sheets=none")
		case "-m":
			// mail when complete - not implemented, ignored.
		case "--":
			if i+1 < len(args) {
				opts.files = append(opts.files, args[i+1:]...)
			}
			i = len(args)
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown option %s", arg)
			}
			opts.files = append(opts.files, arg)
		}
	}
	return opts, nil
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

func printJob(client *cupsclient.Client, user, dest string, opts options, fileName string, input io.Reader) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(dest))))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	}
	jobName := opts.title
	if jobName == "" {
		if fileName != "" {
			jobName = filepath.Base(fileName)
		} else {
			jobName = "stdin"
		}
	}
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(jobName)))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(guessMime(fileName))))

	applyJobOptions(req, opts)

	resp, err := client.Send(context.Background(), req, input)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func createJobAndSend(client *cupsclient.Client, user, dest string, opts options) error {
	jobID, err := createJob(client, user, dest, opts)
	if err != nil {
		return err
	}
	for i, fileName := range opts.files {
		last := i == len(opts.files)-1
		if err := sendDocument(client, user, jobID, fileName, last); err != nil {
			return err
		}
	}
	return nil
}

func createJob(client *cupsclient.Client, user, dest string, opts options) (int, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCreateJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(dest))))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	}
	jobName := opts.title
	if jobName == "" {
		jobName = "stdin"
	}
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(jobName)))

	applyJobOptions(req, opts)

	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return 0, err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return 0, fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagJobGroup {
			continue
		}
		if id := findAttr(g.Attrs, "job-id"); id != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(id)); err == nil {
				return n, nil
			}
		}
	}
	if id := findAttr(resp.Job, "job-id"); id != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(id)); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("job id not returned")
}

func sendDocument(client *cupsclient.Client, user string, jobID int, fileName string, last bool) error {
	f, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	}
	req.Operation.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(filepath.Base(fileName))))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(guessMime(fileName))))
	req.Operation.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(last)))

	resp, err := client.Send(context.Background(), req, f)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func modifyJob(client *cupsclient.Client, user string, jobID int, opts options) error {
	hold := strings.ToLower(strings.TrimSpace(opts.hold))
	hasChanges := opts.title != "" || opts.priority > 0 || len(opts.opts) > 0 || hold != ""

	if hold != "" && !hasChanges {
		hold = ""
	}

	if hold != "" && !hasOtherChanges(opts) {
		switch hold {
		case "resume", "release", "no-hold":
			return releaseJob(client, user, jobID)
		case "hold", "indefinite":
			return holdJob(client, user, jobID, "indefinite")
		}
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSetJobAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	}
	if opts.title != "" {
		req.Job.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(opts.title)))
	}
	if opts.priority > 0 {
		req.Job.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(opts.priority)))
	}
	if hold != "" {
		req.Job.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String(resolveHoldValue(hold))))
	}
	for _, opt := range opts.opts {
		if opt == "" {
			continue
		}
		key, val := splitOpt(opt)
		if key == "" {
			continue
		}
		addJobOption(req, key, val)
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

func hasOtherChanges(opts options) bool {
	return opts.title != "" || opts.priority > 0 || len(opts.opts) > 0
}

func holdJob(client *cupsclient.Client, user string, jobID int, holdValue string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpHoldJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	}
	if holdValue != "" {
		req.Job.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String(holdValue)))
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

func releaseJob(client *cupsclient.Client, user string, jobID int) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpReleaseJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	if user != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
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

func resolveHoldValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "hold", "indefinite":
		return "indefinite"
	case "resume", "release", "no-hold":
		return "no-hold"
	case "immediate":
		return "no-hold"
	default:
		return value
	}
}

func applyJobOptions(req *goipp.Message, opts options) {
	if opts.copies > 0 {
		req.Job.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(opts.copies)))
	}
	if opts.priority > 0 {
		req.Job.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(opts.priority)))
	}
	if opts.hold != "" {
		hold := resolveHoldValue(opts.hold)
		req.Job.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String(hold)))
		if strings.EqualFold(opts.hold, "immediate") {
			req.Job.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(100)))
		}
	}
	for _, opt := range opts.opts {
		if opt == "" {
			continue
		}
		key, val := splitOpt(opt)
		if key == "" {
			continue
		}
		addJobOption(req, key, val)
	}
}

func splitOpt(opt string) (string, string) {
	if strings.Contains(opt, "=") {
		parts := strings.SplitN(opt, "=", 2)
		return parts[0], parts[1]
	}
	return opt, ""
}

func addJobOption(req *goipp.Message, key, val string) {
	switch key {
	case "copies", "job-priority", "number-up", "job-cancel-after", "number-of-retries", "retry-interval", "retry-time-out":
		if n, err := strconv.Atoi(val); err == nil {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagInteger, goipp.Integer(n)))
		}
	case "print-quality", "finishings", "orientation-requested":
		if n, err := strconv.Atoi(val); err == nil {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagEnum, goipp.Integer(n)))
		}
	case "page-ranges":
		req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
	case "printer-resolution":
		req.Job.Add(goipp.MakeAttribute(key, goipp.TagResolution, goipp.String(val)))
	case "job-hold-until":
		req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
	default:
		req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
	}
}

func guessMime(file string) string {
	if file == "" || file == "-" {
		return "application/octet-stream"
	}
	ext := strings.ToLower(filepath.Ext(file))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".ps":
		return "application/postscript"
	case ".txt", ".log":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
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

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}
