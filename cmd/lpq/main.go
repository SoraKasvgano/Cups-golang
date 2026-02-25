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
	authUser    string
	destination string
	userFilter  string
	jobID       int
	showAll     bool
	longStatus  bool
	interval    int
}

type jobView struct {
	ID      int
	SizeKB  int
	State   int
	Name    string
	User    string
	Dest    string
	Copies  int
	HasDest bool
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
		cupsclient.WithUser(opts.authUser),
	)

	dests, err := listDestinations(client)
	if err != nil {
		fail(err)
	}
	if opts.destination != "" {
		if _, ok := dests[strings.ToLower(opts.destination)]; !ok {
			fail(fmt.Errorf("unknown destination %q", opts.destination))
		}
	}

	if !opts.showAll && strings.TrimSpace(opts.destination) == "" {
		if env := defaultDestinationFromEnv(); env != "" {
			opts.destination = env
		}
		if strings.TrimSpace(opts.destination) == "" {
			def, err := defaultDestinationName(client)
			if err != nil {
				fail(err)
			}
			opts.destination = def
		}
		if strings.TrimSpace(opts.destination) != "" {
			if _, ok := dests[strings.ToLower(opts.destination)]; !ok {
				fail(fmt.Errorf("unknown destination %q", opts.destination))
			}
		}
	}

	for {
		if strings.TrimSpace(opts.destination) != "" {
			if err := showPrinter(client, opts.destination); err != nil {
				fail(err)
			}
		}

		count, err := showJobs(client, opts)
		if err != nil {
			fail(err)
		}
		if opts.interval <= 0 || count <= 0 {
			break
		}
		time.Sleep(time.Duration(opts.interval) * time.Second)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: lpq [options] [+interval]")
	fmt.Println("Options:")
	fmt.Println("-a                      Show jobs on all destinations")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-h server[:port]        Connect to the named server and port")
	fmt.Println("-l                      Show verbose (long) output")
	fmt.Println("-P destination          Show status for the specified destination")
	fmt.Println("-U username             Specify the username to use for authentication")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpq:", err)
	os.Exit(1)
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "+") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(arg, "+"))); err == nil && n >= 0 {
				opts.interval = n
				continue
			}
			return opts, fmt.Errorf("bad interval %q", arg)
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
					opts.showAll = true
				case 'E':
					opts.encrypt = true
				case 'h':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.server = strings.TrimSpace(v)
				case 'l':
					opts.longStatus = true
				case 'P':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.destination = sanitizeDestination(v)
				case 'U':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.authUser = strings.TrimSpace(v)
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		if n, err := strconv.Atoi(arg); err == nil && n > 0 {
			opts.jobID = n
		} else {
			opts.userFilter = strings.TrimSpace(arg)
		}
	}
	return opts, nil
}

func sanitizeDestination(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, "/"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw)
}

func defaultDestinationFromEnv() string {
	if v := sanitizeDestination(os.Getenv("LPDEST")); v != "" {
		return v
	}
	if v := sanitizeDestination(os.Getenv("PRINTER")); v != "" && !strings.EqualFold(v, "lp") {
		return v
	}
	return ""
}

func showPrinter(client *cupsclient.Client, destination string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(destination))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if err := checkIPPStatus(resp); err != nil {
		return err
	}

	state := 5
	for _, group := range resp.Groups {
		if group.Tag != goipp.TagPrinterGroup {
			continue
		}
		state = attrInt(group.Attrs, "printer-state", 5)
		break
	}

	switch state {
	case 3:
		fmt.Printf("%s is ready\n", destination)
	case 4:
		fmt.Printf("%s is ready and printing\n", destination)
	default:
		fmt.Printf("%s is not ready\n", destination)
	}
	return nil
}

func showJobs(client *cupsclient.Client, opts options) (int, error) {
	op := goipp.OpGetJobs
	if opts.jobID > 0 {
		op = goipp.OpGetJobAttributes
	}
	req := goipp.NewRequest(goipp.DefaultVersion, op, uint32(time.Now().UnixNano()))
	addOperationDefaults(req)

	if opts.jobID > 0 {
		req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI(opts.jobID))))
	} else if strings.TrimSpace(opts.destination) != "" {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(opts.destination))))
	} else {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/")))
	}

	if strings.TrimSpace(opts.userFilter) != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(opts.userFilter)))
		req.Operation.Add(goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))
	} else {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUserName(client))))
	}

	req.Operation.Add(goipp.MakeAttr(
		"requested-attributes",
		goipp.TagKeyword,
		goipp.String("copies"),
		goipp.String("job-id"),
		goipp.String("job-k-octets"),
		goipp.String("job-name"),
		goipp.String("job-originating-user-name"),
		goipp.String("job-printer-uri"),
		goipp.String("job-priority"),
		goipp.String("job-state"),
	))

	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return 0, err
	}
	if err := checkIPPStatus(resp); err != nil {
		return 0, err
	}

	jobs := parseJobs(resp)
	if len(jobs) == 0 {
		fmt.Println("no entries")
		return 0, nil
	}

	rank := 1
	if !opts.longStatus {
		fmt.Println("Rank    Owner   Job     File(s)                         Total Size")
	}
	displayed := 0
	for _, job := range jobs {
		if !job.HasDest || job.ID == 0 {
			continue
		}
		displayed++
		rankText := rankString(job.State, rank)
		if job.State != 4 {
			rank++
		}
		if opts.longStatus {
			fmt.Println()
			name := strings.TrimSpace(job.Name)
			if name == "" {
				name = "unknown"
			}
			if job.Copies > 1 {
				name = fmt.Sprintf("%d copies of %s", job.Copies, name)
			}
			fmt.Printf("%s: %-33.33s [job %d localhost]\n", trimOr(job.User, "unknown"), rankText, job.ID)
			fmt.Printf("        %-39.39s %.0f bytes\n", name, float64(job.SizeKB)*1024.0)
		} else {
			fmt.Printf("%-7s %-7.7s %-7d %-31.31s %.0f bytes\n",
				rankText,
				trimOr(job.User, "unknown"),
				job.ID,
				trimOr(job.Name, "unknown"),
				float64(job.SizeKB)*1024.0,
			)
		}
	}
	if displayed == 0 {
		fmt.Println("no entries")
	}
	return displayed, nil
}

func parseJobs(resp *goipp.Message) []jobView {
	if resp == nil {
		return nil
	}
	out := []jobView{}
	for _, group := range resp.Groups {
		if group.Tag != goipp.TagJobGroup {
			continue
		}
		out = append(out, parseJobView(group.Attrs))
	}
	if len(out) == 0 && len(resp.Job) > 0 {
		out = append(out, parseJobView(resp.Job))
	}
	return out
}

func parseJobView(attrs goipp.Attributes) jobView {
	j := jobView{
		ID:      attrInt(attrs, "job-id", 0),
		SizeKB:  attrInt(attrs, "job-k-octets", 0),
		State:   attrInt(attrs, "job-state", 3),
		Name:    findAttr(attrs, "job-name"),
		User:    findAttr(attrs, "job-originating-user-name"),
		Copies:  attrInt(attrs, "copies", 1),
		HasDest: false,
	}
	if j.Copies <= 0 {
		j.Copies = 1
	}
	jobURI := strings.TrimSpace(findAttr(attrs, "job-printer-uri"))
	if jobURI != "" {
		if u, err := url.Parse(jobURI); err == nil {
			j.Dest = strings.TrimPrefix(u.Path, "/printers/")
			j.Dest = strings.Trim(strings.TrimSpace(j.Dest), "/")
			j.HasDest = strings.TrimSpace(j.Dest) != ""
		}
	}
	return j
}

func rankString(state, rank int) string {
	if state == 4 {
		return "active"
	}
	if rank < 0 {
		rank = 0
	}
	if rank%100 >= 11 && rank%100 <= 13 {
		return fmt.Sprintf("%dth", rank)
	}
	suffix := "th"
	switch rank % 10 {
	case 1:
		suffix = "st"
	case 2:
		suffix = "nd"
	case 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", rank, suffix)
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

func attrInt(attrs goipp.Attributes, name string, fallback int) int {
	for _, attr := range attrs {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(attr.Values[0].V.String())); err == nil {
			return n
		}
	}
	return fallback
}

func trimOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func listDestinations(client *cupsclient.Client) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, op := range []goipp.Op{goipp.OpCupsGetPrinters, goipp.OpCupsGetClasses} {
		req := goipp.NewRequest(goipp.DefaultVersion, op, uint32(time.Now().UnixNano()))
		addOperationDefaults(req)
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
		resp, err := client.Send(context.Background(), req, nil)
		if err != nil {
			return nil, err
		}
		if err := checkIPPStatus(resp); err != nil {
			if goipp.Status(resp.Code) == goipp.StatusErrorNotFound {
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
