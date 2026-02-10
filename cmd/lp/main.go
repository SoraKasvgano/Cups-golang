package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	server   string
	encrypt  bool
	silent   bool
	opts     []string
	files    []string
	docFormat string
	raw       bool
}

type lpOptionsFile struct {
	Default string
	Dests   map[string]map[string]string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lp:", err)
		os.Exit(1)
	}

	store, err := loadLpOptions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lp:", err)
		os.Exit(1)
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
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

	dest := resolveDest(opts.dest, store)
	if dest == "" {
		if def, err := fetchDefaultDestination(client); err == nil && def != "" {
			dest = def
		}
	}
	if dest == "" {
		dest = "Default"
	}
	mergeLocalOptions(&opts, store, dest)

	if len(opts.files) > 1 {
		for _, f := range opts.files {
			if f == "-" {
				fmt.Fprintln(os.Stderr, "lp: '-' can only be used for a single document")
				os.Exit(1)
			}
		}
		jobID, err := createJobAndSend(client, user, dest, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "lp:", err)
			os.Exit(1)
		}
		if !opts.silent && jobID > 0 {
			printJobResult(dest, jobID, len(opts.files))
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

	jobID, err := printJob(client, user, dest, opts, fileName, input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lp:", err)
		os.Exit(1)
	}
	if !opts.silent && jobID > 0 {
		printJobResult(dest, jobID, 1)
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	seenOther := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
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
			opts.user = args[i]
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
		case "-P":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -P")
			}
			i++
			opts.opts = append(opts.opts, "page-ranges="+args[i])
		case "-m":
			// mail when complete - not implemented, ignored.
		case "-s":
			opts.silent = true
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
		if arg != "-h" && arg != "-E" && arg != "-U" {
			seenOther = true
		}
	}
	return opts, nil
}

func resolveDest(dest string, store *lpOptionsFile) string {
	if dest != "" {
		return dest
	}
	if store != nil && store.Default != "" {
		return store.Default
	}
	if env := os.Getenv("LPDEST"); env != "" {
		return env
	}
	if env := os.Getenv("PRINTER"); env != "" {
		return env
	}
	if env := os.Getenv("CUPS_PRINTER"); env != "" {
		return env
	}
	return ""
}

func printJob(client *cupsclient.Client, user, dest string, opts options, fileName string, input io.Reader) (int, error) {
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
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(resolveDocFormat(fileName, opts))))

	applyJobOptions(req, opts)

	resp, err := client.Send(context.Background(), req, input)
	if err != nil {
		return 0, err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return 0, fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	jobID, err := extractJobID(resp)
	if err != nil {
		return 0, err
	}
	return jobID, nil
}

func createJobAndSend(client *cupsclient.Client, user, dest string, opts options) (int, error) {
	jobID, err := createJob(client, user, dest, opts)
	if err != nil {
		return 0, err
	}
	for i, fileName := range opts.files {
		last := i == len(opts.files)-1
		if err := sendDocument(client, user, jobID, fileName, last, opts); err != nil {
			return 0, err
		}
	}
	return jobID, nil
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
	return extractJobID(resp)
}

func sendDocument(client *cupsclient.Client, user string, jobID int, fileName string, last bool, opts options) error {
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
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(resolveDocFormat(fileName, opts))))
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
	opt = strings.TrimSpace(opt)
	if opt == "" {
		return "", ""
	}
	if strings.Contains(opt, "=") {
		parts := strings.SplitN(opt, "=", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(opt), ""
}

func addJobOption(req *goipp.Message, key, val string) {
	switch key {
	case "copies", "job-priority", "number-up", "job-cancel-after", "number-of-retries", "retry-interval", "retry-time-out":
		if n, err := strconv.Atoi(val); err == nil {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagInteger, goipp.Integer(n)))
		}
	case "print-quality", "finishings", "orientation-requested":
		if key == "print-quality" {
			if n, ok := parsePrintQuality(val); ok {
				req.Job.Add(goipp.MakeAttribute(key, goipp.TagEnum, goipp.Integer(n)))
				return
			}
		}
		if key == "finishings" {
			if enums := parseFinishingsEnums(val); len(enums) > 0 {
				vals := make([]goipp.Value, 0, len(enums))
				for _, n := range enums {
					vals = append(vals, goipp.Integer(n))
				}
				req.Job.Add(goipp.MakeAttr(key, goipp.TagEnum, vals[0], vals[1:]...))
				return
			}
		}
		if n, err := strconv.Atoi(val); err == nil {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagEnum, goipp.Integer(n)))
		} else {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
		}
	case "page-ranges":
		if ranges, ok := parseRangesList(val); ok {
			vals := make([]goipp.Value, 0, len(ranges))
			for _, r := range ranges {
				vals = append(vals, r)
			}
			req.Job.Add(goipp.MakeAttr(key, goipp.TagRange, vals[0], vals[1:]...))
		} else {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
		}
	case "printer-resolution":
		if res, ok := parseResolution(val); ok {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagResolution, res))
		} else {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
		}
	case "job-hold-until":
		req.Job.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(val)))
	case "job-sheets":
		parts := splitList(val, 2)
		if len(parts) == 0 {
			req.Job.Add(goipp.MakeAttribute(key, goipp.TagName, goipp.String("none")))
			return
		}
		vals := make([]goipp.Value, 0, len(parts))
		for _, p := range parts {
			vals = append(vals, goipp.String(p))
		}
		req.Job.Add(goipp.MakeAttr(key, goipp.TagName, vals[0], vals[1:]...))
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

func resolveDocFormat(fileName string, opts options) string {
	if strings.TrimSpace(opts.docFormat) != "" {
		return opts.docFormat
	}
	if opts.raw {
		return "application/vnd.cups-raw"
	}
	return guessMime(fileName)
}

func mergeLocalOptions(opts *options, store *lpOptionsFile, dest string) {
	if opts == nil || store == nil || dest == "" {
		return
	}
	local := store.Dests[dest]
	if len(local) == 0 {
		return
	}
	merged := map[string]string{}
	for k, v := range local {
		merged[k] = v
	}
	for _, opt := range opts.opts {
		if opt == "" {
			continue
		}
		key, val := splitOpt(opt)
		if key == "" {
			continue
		}
		if val == "" {
			val = "true"
		}
		merged[key] = val
	}
	if opts.copies == 0 {
		if v := strings.TrimSpace(merged["copies"]); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				opts.copies = n
			}
			delete(merged, "copies")
		}
	}
	if opts.priority == 0 {
		if v := strings.TrimSpace(merged["job-priority"]); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				opts.priority = n
			}
			delete(merged, "job-priority")
		}
	}
	if opts.hold == "" {
		if v := strings.TrimSpace(merged["job-hold-until"]); v != "" {
			opts.hold = v
			delete(merged, "job-hold-until")
		}
	}
	if v := strings.TrimSpace(merged["document-format"]); v != "" {
		opts.docFormat = v
		delete(merged, "document-format")
	}
	if v := strings.TrimSpace(merged["raw"]); v != "" && isTruthy(v) {
		opts.raw = true
		delete(merged, "raw")
	}
	opts.opts = flattenOptions(merged)
}

func flattenOptions(opts map[string]string) []string {
	if len(opts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := opts[k]
		if strings.TrimSpace(v) == "" {
			out = append(out, k)
		} else {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func loadLpOptions() (*lpOptionsFile, error) {
	path := lpOptionsPath()
	store := &lpOptionsFile{Dests: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := strings.ToLower(fields[0])
		dest := fields[1]
		opts := map[string]string{}
		for _, token := range fields[2:] {
			key, val := splitOpt(token)
			if key == "" {
				continue
			}
			if val == "" {
				val = "true"
			}
			opts[key] = val
		}
		if kind == "default" {
			store.Default = dest
			store.Dests[dest] = opts
			continue
		}
		if kind == "dest" || kind == "printer" {
			store.Dests[dest] = opts
		}
	}
	return store, nil
}

func lpOptionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".lpoptions"
	}
	return filepath.Join(home, ".cups", "lpoptions")
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

func parsePrintQuality(val string) (int, bool) {
	val = strings.TrimSpace(strings.ToLower(val))
	switch val {
	case "3", "draft":
		return 3, true
	case "4", "normal":
		return 4, true
	case "5", "high":
		return 5, true
	}
	if n, err := strconv.Atoi(val); err == nil && n >= 3 && n <= 5 {
		return n, true
	}
	return 0, false
}

func parseFinishingsEnums(value string) []int {
	parts := splitList(value, 0)
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func parseRangesList(value string) ([]goipp.Range, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	parts := strings.Split(value, ",")
	out := make([]goipp.Range, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r, ok := parseRange(part)
		if !ok {
			return nil, false
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func parseRange(value string) (goipp.Range, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return goipp.Range{}, false
	}
	first := strings.Split(value, ",")[0]
	parts := strings.SplitN(first, "-", 2)
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start <= 0 {
		return goipp.Range{}, false
	}
	end := start
	if len(parts) == 2 {
		if v := strings.TrimSpace(parts[1]); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= start {
				end = n
			}
		}
	}
	return goipp.Range{Lower: start, Upper: end}, true
}

func parseResolution(value string) (goipp.Resolution, bool) {
	v := strings.TrimSpace(strings.ToLower(value))
	if v == "" {
		return goipp.Resolution{}, false
	}
	v = strings.TrimSuffix(v, "dpi")
	parts := strings.Split(v, "x")
	if len(parts) == 1 {
		n, err := strconv.Atoi(parts[0])
		if err != nil || n <= 0 {
			return goipp.Resolution{}, false
		}
		return goipp.Resolution{Xres: n, Yres: n, Units: goipp.UnitsDpi}, true
	}
	if len(parts) == 2 {
		x, err1 := strconv.Atoi(parts[0])
		y, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || x <= 0 || y <= 0 {
			return goipp.Resolution{}, false
		}
		return goipp.Resolution{Xres: x, Yres: y, Units: goipp.UnitsDpi}, true
	}
	return goipp.Resolution{}, false
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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

func extractJobID(resp *goipp.Message) (int, error) {
	if resp == nil {
		return 0, fmt.Errorf("job id not returned")
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

func printJobResult(dest string, jobID int, files int) {
	if jobID <= 0 {
		return
	}
	if files <= 0 {
		files = 1
	}
	fmt.Printf("request id is %s-%d (%d file(s))\n", dest, jobID, files)
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
