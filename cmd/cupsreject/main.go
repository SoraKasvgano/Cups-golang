package main

import (
	"context"
	"fmt"
	"os"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsreject:", err)
		os.Exit(1)
	}
	if len(opts.printers) == 0 {
		opts.printers = []string{"Default"}
	}
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
	for _, p := range opts.printers {
		if err := holdNewJobs(client, p); err != nil {
			fmt.Fprintln(os.Stderr, "cupsreject:", err)
			os.Exit(1)
		}
	}
}

type options struct {
	server   string
	encrypt  bool
	user     string
	printers []string
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
			opts.user = args[i]
		default:
			opts.printers = append(opts.printers, args[i])
		}
		if args[i] != "-h" && args[i] != "-E" && args[i] != "-U" {
			seenOther = true
		}
	}
	return opts, nil
}

func holdNewJobs(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsRejectJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	_, err := client.Send(context.Background(), req, nil)
	return err
}
