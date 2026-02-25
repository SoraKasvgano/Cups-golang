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
	sourceToken string
	jobID       int
	source      string
	destination string
	destKinds   map[string]destinationKind
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
		fmt.Fprintln(os.Stderr, "lpmove:", err)
		os.Exit(1)
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
	if known, err := listDestinations(client); err == nil {
		opts.destKinds = known
		opts = normalizeMoveSource(opts, known)
	}

	req := buildMoveRequest(client, opts)
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lpmove:", err)
		os.Exit(1)
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		fmt.Fprintln(os.Stderr, "lpmove:", goipp.Status(resp.Code))
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("Usage: lpmove [options] job destination")
	fmt.Println("       lpmove [options] source-destination destination")
	fmt.Println("Options:")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-h server[:port]        Connect to the named server and port")
	fmt.Println("-U username             Specify the username to use for authentication")
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	positional := make([]string, 0, 2)

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
		if strings.HasPrefix(arg, "-") {
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
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		positional = append(positional, arg)
	}

	if len(positional) != 2 {
		return opts, fmt.Errorf("usage: lpmove job destination")
	}

	jobID, source := parseMoveSource(positional[0])
	if jobID == 0 && source == "" {
		return opts, fmt.Errorf("invalid job id")
	}
	opts.sourceToken = positional[0]
	opts.jobID = jobID
	opts.source = source
	opts.destination = strings.TrimSpace(positional[1])
	if opts.destination == "" {
		return opts, fmt.Errorf("missing destination")
	}
	return opts, nil
}

func parseMoveSource(arg string) (int, string) {
	v := strings.TrimSpace(arg)
	if v == "" {
		return 0, ""
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n, ""
	}
	if idx := strings.LastIndex(v, "-"); idx > 0 && idx < len(v)-1 {
		if n, err := strconv.Atoi(v[idx+1:]); err == nil && n > 0 {
			return n, ""
		}
	}
	return 0, v
}

func buildMoveRequest(client *cupsclient.Client, opts options) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))

	if opts.jobID > 0 {
		req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI(client, opts.jobID))))
	} else {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destinationURI(client, opts.source, opts.destKinds))))
	}

	requestingUser := strings.TrimSpace(opts.user)
	if requestingUser == "" && client != nil {
		requestingUser = strings.TrimSpace(client.User)
	}
	if requestingUser != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(requestingUser)))
	}

	destURI := destinationURI(client, opts.destination, opts.destKinds)
	// CUPS sends destination URI in the job attribute group.
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String(destURI)))
	return req
}

func destinationURI(client *cupsclient.Client, destination string, known map[string]destinationKind) string {
	_ = client
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return ""
	}
	if strings.Contains(destination, "://") {
		return destination
	}
	_ = known
	return fmt.Sprintf("ipp://localhost/printers/%s", url.PathEscape(destination))
}

func jobURI(client *cupsclient.Client, jobID int) string {
	_ = client
	return fmt.Sprintf("ipp://localhost/jobs/%d", jobID)
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
				// Keep explicit class classification when present.
				if existing, exists := out[key]; !exists || item.kind == destinationClass || existing == destinationUnknown {
					out[key] = item.kind
				}
			}
		}
	}
	return out, nil
}

func normalizeMoveSource(opts options, known map[string]destinationKind) options {
	if len(known) == 0 {
		return opts
	}
	sourceToken := strings.TrimSpace(opts.sourceToken)
	if sourceToken == "" {
		return opts
	}
	if kind, ok := known[strings.ToLower(sourceToken)]; ok && kind != destinationUnknown {
		opts.jobID = 0
		opts.source = sourceToken
	}
	return opts
}
