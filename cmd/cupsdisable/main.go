package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

var errShowHelp = errors.New("show-help")

type options struct {
	server  string
	encrypt bool
	user    string
	cancel  bool
	hold    bool
	reason  string
	dests   []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errShowHelp) {
		usage()
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsdisable:", err)
		os.Exit(1)
	}
	if len(opts.dests) == 0 {
		return
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

	for _, d := range opts.dests {
		if err := doDisable(client, d, opts); err != nil {
			fmt.Fprintln(os.Stderr, "cupsdisable:", err)
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Println("Usage: cupsdisable [options] destination(s)")
	fmt.Println("Options:")
	fmt.Println("-E                      Encrypt connection")
	fmt.Println("-h server[:port]        Connect to server")
	fmt.Println("-r reason               Set printer-state-message")
	fmt.Println("-U username             Authenticate as user")
	fmt.Println("-c                      Cancel jobs after disabling")
	fmt.Println("--hold                  Hold new jobs")
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
		if arg == "--hold" {
			opts.hold = true
			continue
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
				case 'U':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.user = strings.TrimSpace(v)
				case 'h':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.server = strings.TrimSpace(v)
				case 'r':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.reason = strings.TrimSpace(v)
				case 'c':
					opts.cancel = true
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		opts.dests = append(opts.dests, arg)
	}
	return opts, nil
}

func doDisable(client *cupsclient.Client, name string, opts options) error {
	op := goipp.OpPausePrinter
	if opts.hold {
		op = goipp.OpHoldNewJobs
	}
	req := goipp.NewRequest(goipp.DefaultVersion, op, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	if opts.reason != "" {
		req.Operation.Add(goipp.MakeAttribute("printer-state-message", goipp.TagText, goipp.String(opts.reason)))
	}
	if _, err := client.Send(context.Background(), req, nil); err != nil {
		return err
	}

	if !opts.cancel {
		return nil
	}
	purge := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPurgeJobs, uint32(time.Now().UnixNano()))
	purge.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	purge.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	purge.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	_, err := client.Send(context.Background(), purge, nil)
	return err
}
