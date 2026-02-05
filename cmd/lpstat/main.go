package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	showDefault   bool
	showStatus    bool
	showPrinters  bool
	showAccepting bool
	showJobs      bool
	showDevices   bool
	showSummary   bool
	showAll       bool
	printerFilter string
	userFilter    string
}

func main() {
	opts := parseArgs(os.Args[1:])
	client := cupsclient.NewFromEnv()

	if opts.showAll {
		opts.showSummary = true
		opts.showJobs = true
		opts.showDevices = true
		opts.showAccepting = true
	}
	if opts.showSummary {
		opts.showDefault = true
		opts.showStatus = true
		opts.showPrinters = true
	}
	if !opts.showDefault && !opts.showStatus && !opts.showPrinters && !opts.showAccepting && !opts.showJobs && !opts.showDevices {
		opts.showPrinters = true
	}

	if opts.showStatus {
		printSchedulerStatus()
	}
	if opts.showDefault {
		if err := printDefault(client); err != nil {
			fail(err)
		}
	}

	var printers []printerInfo
	var printersErr error
	if opts.showPrinters || opts.showAccepting || opts.showDevices {
		printers, printersErr = fetchPrinters(client)
		if printersErr != nil {
			fail(printersErr)
		}
	}
	if opts.showPrinters {
		printPrinters(printers, opts.printerFilter)
	}
	if opts.showAccepting {
		printAccepting(printers, opts.printerFilter)
	}
	if opts.showDevices {
		printDevices(printers, opts.printerFilter)
	}
	if opts.showJobs {
		if err := printJobs(client, opts.printerFilter, opts.userFilter); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpstat:", err)
	os.Exit(1)
}

func parseArgs(args []string) options {
	opts := options{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-d":
			opts.showDefault = true
		case "-r":
			opts.showStatus = true
		case "-p":
			opts.showPrinters = true
			if next := peekArg(args, &i); next != "" {
				opts.printerFilter = next
			}
		case "-a":
			opts.showAccepting = true
			if next := peekArg(args, &i); next != "" {
				opts.printerFilter = next
			}
		case "-o":
			opts.showJobs = true
			if next := peekArg(args, &i); next != "" {
				opts.printerFilter = next
			}
		case "-u":
			opts.showJobs = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.userFilter = args[i]
			}
		case "-v":
			opts.showDevices = true
			if next := peekArg(args, &i); next != "" {
				opts.printerFilter = next
			}
		case "-s":
			opts.showSummary = true
		case "-t":
			opts.showAll = true
		}
	}
	return opts
}

func peekArg(args []string, idx *int) string {
	if *idx+1 >= len(args) {
		return ""
	}
	next := args[*idx+1]
	if strings.HasPrefix(next, "-") {
		return ""
	}
	*idx++
	return next
}

func printSchedulerStatus() {
	fmt.Println("scheduler is running")
}

func printDefault(client *cupsclient.Client) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	name := findAttr(resp.Printer, "printer-name")
	if name != "" {
		fmt.Printf("system default destination: %s\n", name)
	}
	return nil
}

type printerInfo struct {
	name      string
	state     string
	accepting bool
	deviceURI string
}

func fetchPrinters(client *cupsclient.Client) ([]printerInfo, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPrinters, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("printer-name"),
		goipp.String("printer-state"),
		goipp.String("printer-is-accepting-jobs"),
		goipp.String("device-uri"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return nil, err
	}
	var printers []printerInfo
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		name := findAttr(g.Attrs, "printer-name")
		if name == "" {
			continue
		}
		state := printerStateString(findAttr(g.Attrs, "printer-state"))
		accepting := strings.EqualFold(findAttr(g.Attrs, "printer-is-accepting-jobs"), "true")
		device := findAttr(g.Attrs, "device-uri")
		printers = append(printers, printerInfo{
			name:      name,
			state:     state,
			accepting: accepting,
			deviceURI: device,
		})
	}
	return printers, nil
}

func printPrinters(printers []printerInfo, filter string) {
	for _, p := range printers {
		if filter != "" && !strings.EqualFold(filter, p.name) {
			continue
		}
		fmt.Printf("printer %s is %s\n", p.name, p.state)
	}
}

func printAccepting(printers []printerInfo, filter string) {
	for _, p := range printers {
		if filter != "" && !strings.EqualFold(filter, p.name) {
			continue
		}
		if p.accepting {
			fmt.Printf("printer %s accepting requests\n", p.name)
		} else {
			fmt.Printf("printer %s not accepting requests\n", p.name)
		}
	}
}

func printDevices(printers []printerInfo, filter string) {
	for _, p := range printers {
		if filter != "" && !strings.EqualFold(filter, p.name) {
			continue
		}
		if p.deviceURI != "" {
			fmt.Printf("device for %s: %s\n", p.name, p.deviceURI)
		}
	}
}

func printJobs(client *cupsclient.Client, printerFilter, userFilter string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("job-id"),
		goipp.String("job-name"),
		goipp.String("job-originating-user-name"),
		goipp.String("job-state"),
		goipp.String("job-printer-uri"),
	))
	if printerFilter != "" {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printerFilter))))
	}
	if userFilter != "" {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(userFilter)))
		req.Operation.Add(goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagJobGroup {
			continue
		}
		id := findAttr(g.Attrs, "job-id")
		name := findAttr(g.Attrs, "job-name")
		user := findAttr(g.Attrs, "job-originating-user-name")
		state := jobStateString(findAttr(g.Attrs, "job-state"))
		printerURI := findAttr(g.Attrs, "job-printer-uri")
		printerName := printerNameFromURI(printerURI)
		if printerFilter != "" && !strings.EqualFold(printerName, printerFilter) {
			continue
		}
		if userFilter != "" && !strings.EqualFold(user, userFilter) {
			continue
		}
		if id != "" {
			fmt.Printf("job %s %s %s %s\n", id, user, state, name)
		}
	}
	return nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func printerStateString(val string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
		switch n {
		case 3:
			return "idle"
		case 4:
			return "processing"
		case 5:
			return "stopped"
		}
	}
	if val == "" {
		return "idle"
	}
	return val
}

func jobStateString(val string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
		switch n {
		case 3:
			return "pending"
		case 4:
			return "held"
		case 5:
			return "processing"
		case 6:
			return "stopped"
		case 7:
			return "canceled"
		case 8:
			return "aborted"
		case 9:
			return "completed"
		}
	}
	if val == "" {
		return "pending"
	}
	return val
}

func printerNameFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
