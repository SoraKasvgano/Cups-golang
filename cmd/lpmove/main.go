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

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lpmove:", err)
		os.Exit(1)
	}
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
	jobID := opts.jobID
	dest := opts.dest
	destURI := dest
	if !strings.Contains(dest, "://") {
		destURI = client.PrinterURI(dest)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destURI)))

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

type options struct {
	server  string
	encrypt bool
	user    string
	jobID   int
	dest    string
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	seenOther := false
	positional := []string{}
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
			positional = append(positional, args[i])
		}
		if args[i] != "-h" && args[i] != "-E" && args[i] != "-U" {
			seenOther = true
		}
	}
	if len(positional) < 2 {
		return opts, fmt.Errorf("usage: lpmove job destination")
	}
	opts.jobID = parseJobID(positional[0])
	if opts.jobID == 0 {
		return opts, fmt.Errorf("invalid job id")
	}
	opts.dest = positional[1]
	return opts, nil
}

func parseJobID(arg string) int {
	if strings.Contains(arg, "-") {
		parts := strings.Split(arg, "-")
		arg = parts[len(parts)-1]
	}
	n, _ := strconv.Atoi(arg)
	return n
}
