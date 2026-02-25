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
	server   string
	encrypt  bool
	user     string
	reason   string
	printers []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errShowHelp) {
		usage()
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsaccept:", err)
		os.Exit(1)
	}
	if len(opts.printers) == 0 {
		return
	}
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
	for _, p := range opts.printers {
		if err := acceptJobs(client, p, opts.reason); err != nil {
			fmt.Fprintln(os.Stderr, "cupsaccept:", err)
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Println("Usage: cupsaccept [options] destination(s)")
	fmt.Println("Options:")
	fmt.Println("-E                      Encrypt connection")
	fmt.Println("-h server[:port]        Connect to server")
	fmt.Println("-r reason               Set printer-state-message")
	fmt.Println("-U username             Authenticate as user")
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
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}
		opts.printers = append(opts.printers, arg)
	}
	return opts, nil
}

func acceptJobs(client *cupsclient.Client, name, reason string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAcceptJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	addRequestingUserName(&req.Operation, client)
	if strings.TrimSpace(reason) != "" {
		req.Operation.Add(goipp.MakeAttribute("printer-state-message", goipp.TagText, goipp.String(strings.TrimSpace(reason))))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	return checkIPPStatus(resp)
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

func addRequestingUserName(attrs *goipp.Attributes, client *cupsclient.Client) {
	if attrs == nil {
		return
	}
	user := requestingUserName(client)
	if user == "" {
		return
	}
	attrs.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
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
