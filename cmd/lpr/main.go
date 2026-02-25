package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

var errShowHelp = errors.New("show-help")

type options struct {
	server      string
	encrypt     bool
	authUser    string
	destination string
	instance    string
	title       string
	deleteFiles bool
	jobOptions  map[string]string
	files       []string
	warnings    []string
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
	for _, warning := range opts.warnings {
		if strings.TrimSpace(warning) != "" {
			fmt.Fprintln(os.Stderr, "lpr:", warning)
		}
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.authUser),
	)

	lpStore, err := loadLpOptions()
	if err != nil {
		fail(err)
	}

	if strings.TrimSpace(opts.destination) == "" {
		if spec := strings.TrimSpace(lpStore.Default); spec != "" {
			opts.destination, opts.instance = parseDestinationSpec(spec)
		}
		if strings.TrimSpace(opts.destination) == "" {
			if envDest, envInstance := defaultDestinationFromEnv(); envDest != "" {
				opts.destination = envDest
				opts.instance = envInstance
			}
		}
		if strings.TrimSpace(opts.destination) == "" {
			def, err := defaultDestinationName(client)
			if err != nil {
				fail(err)
			}
			opts.destination = def
		}
	}
	if strings.TrimSpace(opts.destination) == "" {
		fail(errors.New("no default destination available"))
	}
	exists, err := destinationExists(client, opts.destination)
	if err != nil {
		fail(err)
	}
	if !exists {
		fail(fmt.Errorf("The printer or class does not exist"))
	}
	opts.jobOptions = mergeDestinationOptions(lpStore, opts.destination, opts.instance, opts.jobOptions)

	if len(opts.files) == 0 {
		if err := printStdin(client, opts); err != nil {
			fail(err)
		}
		return
	}

	if len(opts.files) == 1 {
		if err := printFile(client, opts, opts.files[0]); err != nil {
			fail(err)
		}
	} else {
		if err := printFilesAsSingleJob(client, opts, opts.files); err != nil {
			fail(err)
		}
	}

	if opts.deleteFiles {
		for _, file := range opts.files {
			_ = os.Remove(file)
		}
	}
}

func usage() {
	fmt.Println("Usage: lpr [options] [file(s)]")
	fmt.Println("Options:")
	fmt.Println("-# num-copies           Specify the number of copies to print")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-H server[:port]        Connect to the named server and port")
	fmt.Println("-m                      Send an email notification when the job completes")
	fmt.Println("-o option[=value]       Specify a printer-specific option")
	fmt.Println("-P destination          Specify the destination")
	fmt.Println("-q                      Specify the job should be held for printing")
	fmt.Println("-r                      Remove the file(s) after submission")
	fmt.Println("-T title                Specify the job title")
	fmt.Println("-U username             Specify the username to use for authentication")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpr:", err)
	os.Exit(1)
}

func parseArgs(args []string) (options, error) {
	opts := options{jobOptions: map[string]string{}}
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
						return "", fmt.Errorf("expected value after -%c", name)
					}
					i++
					return args[i], nil
				}

				switch ch {
				case 'E':
					opts.encrypt = true
				case 'U':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.authUser = strings.TrimSpace(v)
				case 'H':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.server = strings.TrimSpace(v)
				case 'P':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.destination, opts.instance = parseDestinationSpec(v)
				case '#':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil || n < 1 {
						return opts, fmt.Errorf("copies must be 1 or more")
					}
					opts.jobOptions["copies"] = strconv.Itoa(n)
				case 'o':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					applyOptionString(opts.jobOptions, v)
				case 'l':
					opts.jobOptions["raw"] = "true"
				case 'p':
					opts.jobOptions["prettyprint"] = "true"
				case 'h':
					opts.jobOptions["job-sheets"] = "none"
				case 'm':
					opts.jobOptions["notify-recipient-uri"] = mailtoRecipient(requestingUserNameFromStrings(opts.authUser))
				case 'q':
					opts.jobOptions["job-hold-until"] = "indefinite"
				case 'r':
					opts.deleteFiles = true
				case 'C', 'J', 'T':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.title = strings.TrimSpace(v)
				case 's':
					// CUPS keeps this for compatibility; it is ignored.
				case '1', '2', '3', '4', 'i', 'w':
					if rest == "" {
						if i+1 >= len(args) {
							return opts, fmt.Errorf("expected value after -%c option", ch)
						}
						i++
					}
					opts.warnings = append(opts.warnings, fmt.Sprintf("\"%c\" format modifier not supported - output may not be correct.", ch))
					pos = len(short)
				case 'c', 'd', 'f', 'g', 'n', 't', 'v':
					opts.warnings = append(opts.warnings, fmt.Sprintf("\"%c\" format modifier not supported - output may not be correct.", ch))
				default:
					return opts, fmt.Errorf("unknown option \"%c\"", ch)
				}
			}
			continue
		}
		if _, err := os.Stat(arg); err != nil {
			return opts, fmt.Errorf("unable to access %q - %v", arg, err)
		}
		if len(opts.files) >= 1000 {
			return opts, fmt.Errorf("too many files - %q", arg)
		}
		opts.files = append(opts.files, arg)
		if opts.title == "" {
			opts.title = filepath.Base(arg)
		}
	}
	return opts, nil
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

func defaultDestinationFromEnv() (string, string) {
	if v, i := parseDestinationSpec(os.Getenv("LPDEST")); v != "" {
		return v, i
	}
	if v, i := parseDestinationSpec(os.Getenv("PRINTER")); v != "" && !strings.EqualFold(v, "lp") {
		return v, i
	}
	return "", ""
}

func applyOptionString(out map[string]string, raw string) {
	if out == nil {
		return
	}
	for _, token := range splitOptionTokens(raw) {
		name, value := parseNameValue(token)
		if name == "" {
			continue
		}
		out[name] = value
	}
}

func splitOptionTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := splitOptionWords(raw)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		out = append(out, field)
	}
	return out
}

func parseNameValue(token string) (string, string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ""
	}
	if strings.Contains(token, "=") {
		parts := strings.SplitN(token, "=", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return token, "true"
}

func printFile(client *cupsclient.Client, opts options, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	title := opts.title
	if strings.TrimSpace(title) == "" {
		title = filepath.Base(path)
	}
	format := documentFormat(opts.jobOptions, path)
	req := buildPrintJobRequest(client, opts.destination, title, format, opts.jobOptions)
	resp, err := client.Send(context.Background(), req, f)
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
}

func printFilesAsSingleJob(client *cupsclient.Client, opts options, files []string) error {
	if len(files) == 0 {
		return errors.New("missing files")
	}
	title := opts.title
	if strings.TrimSpace(title) == "" {
		title = filepath.Base(files[0])
	}
	createReq := buildCreateJobRequest(client, opts.destination, title, opts.jobOptions)
	createResp, err := client.Send(context.Background(), createReq, nil)
	if err != nil {
		return err
	}
	if err := checkIPPStatus(createResp); err != nil {
		return err
	}

	jobID, err := responseJobID(createResp)
	if err != nil {
		return err
	}

	for i, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		last := i == len(files)-1
		format := documentFormat(opts.jobOptions, path)
		sendReq := buildSendDocumentRequest(
			client,
			opts.destination,
			jobID,
			filepath.Base(path),
			format,
			last,
		)
		resp, sendErr := client.Send(context.Background(), sendReq, f)
		_ = f.Close()
		if sendErr != nil {
			return sendErr
		}
		if err := checkIPPStatus(resp); err != nil {
			return err
		}
	}
	return nil
}

func printStdin(client *cupsclient.Client, opts options) error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	title := opts.title
	if strings.TrimSpace(title) == "" {
		title = "(stdin)"
	}
	format := documentFormat(opts.jobOptions, "")
	req := buildPrintJobRequest(client, opts.destination, title, format, opts.jobOptions)
	resp, err := client.Send(context.Background(), req, bytes.NewReader(data))
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
}

func buildCreateJobRequest(client *cupsclient.Client, destination, title string, jobOptions map[string]string) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCreateJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(strings.TrimSpace(title))))
	addJobOptions(req, jobOptions)
	return req
}

func buildSendDocumentRequest(client *cupsclient.Client, destination string, jobID int, documentName, format string, lastDocument bool) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	req.Operation.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(strings.TrimSpace(documentName))))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(strings.TrimSpace(format))))
	req.Operation.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(lastDocument)))
	return req
}

func buildPrintJobRequest(client *cupsclient.Client, destination, title, format string, jobOptions map[string]string) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(strings.TrimSpace(title))))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	addJobOptions(req, jobOptions)
	return req
}

func addJobOptions(req *goipp.Message, jobOptions map[string]string) {
	if req == nil {
		return
	}
	if copies := strings.TrimSpace(jobOptions["copies"]); copies != "" {
		if n, err := strconv.Atoi(copies); err == nil && n > 0 {
			req.Job.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	for name, value := range jobOptions {
		lower := strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if lower == "" || value == "" {
			continue
		}
		switch lower {
		case "copies", "raw":
			continue
		case "job-sheets":
			values := splitOptionTokens(value)
			if len(values) == 0 {
				values = []string{"none"}
			}
			attrs := make([]goipp.Value, 0, len(values))
			for _, v := range values {
				attrs = append(attrs, goipp.String(v))
			}
			req.Job.Add(goipp.MakeAttr("job-sheets", goipp.TagName, attrs[0], attrs[1:]...))
		default:
			req.Job.Add(goipp.MakeAttribute(lower, goipp.TagKeyword, goipp.String(value)))
		}
	}
}

func documentFormat(jobOptions map[string]string, path string) string {
	if strings.EqualFold(strings.TrimSpace(jobOptions["raw"]), "true") {
		return "application/octet-stream"
	}
	if v := strings.TrimSpace(jobOptions["document-format"]); v != "" {
		return v
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".ps":
		return "application/postscript"
	case ".txt":
		return "text/plain"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func destinationURI(destination string) string {
	return fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(strings.TrimSpace(destination)))
}

func requestingUserName(client *cupsclient.Client) string {
	if client != nil {
		if user := strings.TrimSpace(client.User); user != "" {
			return user
		}
	}
	return requestingUserNameFromStrings("")
}

func requestingUserNameFromStrings(primary string) string {
	if user := strings.TrimSpace(primary); user != "" {
		return user
	}
	for _, key := range []string{"CUPS_USER", "USER", "USERNAME"} {
		if user := strings.TrimSpace(os.Getenv(key)); user != "" {
			return user
		}
	}
	return "anonymous"
}

func mailtoRecipient(user string) string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "localhost"
	}
	return "mailto:" + user + "@" + host
}

func defaultDestinationName(client *cupsclient.Client) (string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
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

func responseJobID(resp *goipp.Message) (int, error) {
	if resp == nil {
		return 0, errors.New("empty ipp response")
	}
	for _, attrs := range []goipp.Attributes{resp.Operation, resp.Job} {
		if n := findAttrInt(attrs, "job-id"); n > 0 {
			return n, nil
		}
	}
	for _, group := range resp.Groups {
		if n := findAttrInt(group.Attrs, "job-id"); n > 0 {
			return n, nil
		}
	}
	return 0, errors.New("missing job-id in create-job response")
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name == name && len(attr.Values) > 0 {
			return attr.Values[0].V.String()
		}
	}
	return ""
}

func findAttrInt(attrs goipp.Attributes, name string) int {
	value := strings.TrimSpace(findAttr(attrs, name))
	if value == "" {
		return 0
	}
	n, _ := strconv.Atoi(value)
	return n
}

type lpOptionsFile struct {
	Default string
	Dests   map[string]map[string]string
}

func loadLpOptions() (*lpOptionsFile, error) {
	store := &lpOptionsFile{Dests: map[string]map[string]string{}}
	data, err := os.ReadFile(lpOptionsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(fields[0]))
		dest := strings.TrimSpace(fields[1])
		if dest == "" {
			continue
		}
		key := strings.ToLower(dest)
		options := map[string]string{}
		for _, token := range fields[2:] {
			name, value := parseNameValue(token)
			if name == "" {
				continue
			}
			if value == "" {
				value = "true"
			}
			options[name] = value
		}
		switch kind {
		case "default":
			store.Default = dest
			store.Dests[key] = options
		case "dest", "printer":
			store.Dests[key] = options
		}
	}
	return store, nil
}

func lpOptionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ".lpoptions"
	}
	return filepath.Join(home, ".cups", "lpoptions")
}

func mergeDestinationOptions(store *lpOptionsFile, destination, instance string, explicit map[string]string) map[string]string {
	merged := map[string]string{}
	if store != nil {
		destKey := strings.ToLower(strings.TrimSpace(destination))
		if opts, ok := store.Dests[destKey]; ok {
			for key, value := range opts {
				merged[key] = value
			}
		}
		if strings.TrimSpace(instance) != "" {
			instanceKey := strings.ToLower(strings.TrimSpace(destination + "/" + instance))
			if opts, ok := store.Dests[instanceKey]; ok {
				for key, value := range opts {
					merged[key] = value
				}
			}
		}
	}
	for key, value := range explicit {
		merged[key] = value
	}
	return merged
}

func splitOptionWords(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	out := []string{}
	var current strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		out = append(out, current.String())
		current.Reset()
	}
	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return out
}

func destinationExists(client *cupsclient.Client, name string) (bool, error) {
	dests, err := listDestinations(client)
	if err != nil {
		return false, err
	}
	_, ok := dests[strings.ToLower(strings.TrimSpace(name))]
	return ok, nil
}

func listDestinations(client *cupsclient.Client) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, op := range []goipp.Op{goipp.OpCupsGetPrinters, goipp.OpCupsGetClasses} {
		req := goipp.NewRequest(goipp.DefaultVersion, op, uint32(time.Now().UnixNano()))
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
		resp, err := client.Send(context.Background(), req, nil)
		if err != nil {
			return nil, err
		}
		if err := checkIPPStatus(resp); err != nil {
			status := goipp.Status(resp.Code)
			if status == goipp.StatusErrorNotFound {
				continue
			}
			return nil, err
		}
		for _, group := range resp.Groups {
			if group.Tag != goipp.TagPrinterGroup {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(findAttr(group.Attrs, "printer-name")))
			if name == "" {
				continue
			}
			out[name] = struct{}{}
		}
	}
	return out, nil
}
