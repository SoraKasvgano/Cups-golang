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
	server        string
	encrypt       bool
	user          string
	showDefault   bool
	showStatus    bool
	showPrinters  bool
	showAccepting bool
	showJobs      bool
	showDevices   bool
	showSummary   bool
	showAll       bool
	showHost      bool
	showRanking   bool
	longStatus    int
	showClasses   bool
	showAllDests  bool
	whichJobs     string
	printerFilter []string
	userFilter    []string
}

func main() {
	opts := parseArgs(os.Args[1:])
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

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
	if !opts.showDefault && !opts.showStatus && !opts.showPrinters && !opts.showAccepting && !opts.showJobs && !opts.showDevices && !opts.showClasses && !opts.showAllDests {
		opts.showJobs = true
		if len(opts.userFilter) == 0 {
			opts.userFilter = []string{client.User}
		}
	}
	if opts.whichJobs == "" {
		opts.whichJobs = "not-completed"
	}

	if opts.showHost {
		printServerHost(client)
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
	if opts.showPrinters || opts.showAccepting || opts.showDevices || opts.showAllDests {
		printers, printersErr = fetchPrinters(client)
		if printersErr != nil {
			fail(printersErr)
		}
	}
	if opts.showPrinters {
		printPrinters(client, printers, opts.printerFilter, opts.longStatus)
	}
	if opts.showAccepting {
		printAccepting(printers, opts.printerFilter)
	}
	if opts.showDevices {
		printDevices(printers, opts.printerFilter)
	}
	if opts.showAllDests {
		printDestinations(printers)
	}
	if opts.showClasses {
		if err := printClasses(client, opts.printerFilter, opts.longStatus > 0); err != nil {
			fail(err)
		}
	}
	if opts.showJobs {
		if err := printJobs(client, opts.printerFilter, opts.userFilter, opts.whichJobs, opts.showRanking, opts.longStatus); err != nil {
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
	seenOther := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h":
			if seenOther {
				fail(fmt.Errorf("-h must appear before all other options"))
			}
			if i+1 >= len(args) {
				fail(fmt.Errorf("missing argument for -h"))
			}
			i++
			opts.server = args[i]
		case "-E":
			opts.encrypt = true
		case "-U":
			if i+1 >= len(args) {
				fail(fmt.Errorf("missing argument for -U"))
			}
			i++
			opts.user = args[i]
		case "-H":
			opts.showHost = true
		case "-R":
			opts.showRanking = true
		case "-D":
			if opts.longStatus < 1 {
				opts.longStatus = 1
			}
		case "-l":
			opts.longStatus = 2
		case "-W":
			if i+1 >= len(args) {
				fail(fmt.Errorf("missing argument for -W"))
			}
			i++
			opts.whichJobs = strings.ToLower(strings.TrimSpace(args[i]))
			switch opts.whichJobs {
			case "completed", "not-completed", "successful", "all":
			default:
				fail(fmt.Errorf("need \"completed\", \"not-completed\", \"successful\", or \"all\" after -W"))
			}
		case "-d":
			opts.showDefault = true
		case "-r":
			opts.showStatus = true
		case "-p":
			opts.showPrinters = true
			opts.printerFilter = append(opts.printerFilter, parseListArg(peekArg(args, &i))...)
		case "-a":
			opts.showAccepting = true
			opts.printerFilter = append(opts.printerFilter, parseListArg(peekArg(args, &i))...)
		case "-o":
			opts.showJobs = true
			opts.printerFilter = append(opts.printerFilter, parseListArg(peekArg(args, &i))...)
		case "-u":
			opts.showJobs = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.userFilter = append(opts.userFilter, parseListArg(args[i])...)
			}
		case "-v":
			opts.showDevices = true
			opts.printerFilter = append(opts.printerFilter, parseListArg(peekArg(args, &i))...)
		case "-c":
			opts.showClasses = true
			if next := peekArg(args, &i); next != "" {
				opts.printerFilter = append(opts.printerFilter, parseListArg(next)...)
			}
		case "-e":
			opts.showAllDests = true
		case "-s":
			opts.showSummary = true
		case "-t":
			opts.showAll = true
		}
		if arg != "-h" && arg != "-E" && arg != "-U" {
			seenOther = true
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

func parseListArg(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printSchedulerStatus() {
	fmt.Println("scheduler is running")
}

func printServerHost(client *cupsclient.Client) {
	if client == nil {
		return
	}
	fmt.Printf("scheduler is running on %s:%d\n", client.Host, client.Port)
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
	} else {
		fmt.Println("no system default destination")
	}
	return nil
}

type printerInfo struct {
	name        string
	state       int
	accepting   bool
	deviceURI   string
	location    string
	info        string
	stateReason string
	stateMsg    string
	stateChange int64
	ptype       int
	makeModel   string
	uri         string
	allowed     []string
	denied      []string
	reasons     []string
}

func fetchPrinters(client *cupsclient.Client) ([]printerInfo, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPrinters, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("printer-name"),
		goipp.String("printer-state"),
		goipp.String("printer-state-message"),
		goipp.String("printer-state-reasons"),
		goipp.String("printer-state-change-time"),
		goipp.String("printer-type"),
		goipp.String("printer-is-accepting-jobs"),
		goipp.String("device-uri"),
		goipp.String("printer-location"),
		goipp.String("printer-info"),
		goipp.String("printer-make-and-model"),
		goipp.String("printer-uri-supported"),
		goipp.String("requesting-user-name-allowed"),
		goipp.String("requesting-user-name-denied"),
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
		state := parseInt(findAttr(g.Attrs, "printer-state"))
		accepting := strings.EqualFold(findAttr(g.Attrs, "printer-is-accepting-jobs"), "true")
		device := findAttr(g.Attrs, "device-uri")
		location := findAttr(g.Attrs, "printer-location")
		info := findAttr(g.Attrs, "printer-info")
		stateReason := findAttr(g.Attrs, "printer-state-reasons")
		stateMsg := findAttr(g.Attrs, "printer-state-message")
		stateChange := parseInt64(findAttr(g.Attrs, "printer-state-change-time"))
		ptype := parseInt(findAttr(g.Attrs, "printer-type"))
		makeModel := findAttr(g.Attrs, "printer-make-and-model")
		uri := findAttr(g.Attrs, "printer-uri-supported")
		allowed := attrStrings(g.Attrs, "requesting-user-name-allowed")
		denied := attrStrings(g.Attrs, "requesting-user-name-denied")
		reasons := attrStrings(g.Attrs, "printer-state-reasons")
		printers = append(printers, printerInfo{
			name:        name,
			state:       state,
			accepting:   accepting,
			deviceURI:   device,
			location:    location,
			info:        info,
			stateReason: stateReason,
			stateMsg:    stateMsg,
			stateChange: stateChange,
			ptype:       ptype,
			makeModel:   makeModel,
			uri:         uri,
			allowed:     allowed,
			denied:      denied,
			reasons:     reasons,
		})
	}
	return printers, nil
}

func printPrinters(client *cupsclient.Client, printers []printerInfo, filter []string, longStatus int) {
	for _, p := range printers {
		if !matchesFilter(filter, p.name) {
			continue
		}
		stateTime := formatCupsDate(p.stateChange)
		switch p.state {
		case 3: // idle
			if containsReason(p.reasons, "hold-new-jobs") {
				fmt.Printf("printer %s is holding new jobs.  enabled since %s\n", p.name, stateTime)
			} else {
				fmt.Printf("printer %s is idle.  enabled since %s\n", p.name, stateTime)
			}
		case 4, 5: // processing
			jobID := fetchCurrentJobID(client, p.name)
			if jobID > 0 {
				fmt.Printf("printer %s now printing %s-%d.  enabled since %s\n", p.name, p.name, jobID, stateTime)
			} else {
				fmt.Printf("printer %s now printing %s-%d.  enabled since %s\n", p.name, p.name, 0, stateTime)
			}
		case 6: // stopped
			fmt.Printf("printer %s disabled since %s -\n", p.name, stateTime)
		default:
			fmt.Printf("printer %s is idle.  enabled since %s\n", p.name, stateTime)
		}
		if p.stateMsg != "" || p.state == 6 {
			if p.stateMsg != "" {
				fmt.Printf("\t%s\n", p.stateMsg)
			} else {
				fmt.Println("\treason unknown")
			}
		}
		if longStatus > 0 {
			fmt.Printf("\tDescription: %s\n", p.info)
			if len(p.reasons) > 0 {
				fmt.Printf("\tAlerts: %s\n", strings.Join(p.reasons, " "))
			}
		}
		if longStatus > 1 {
			fmt.Printf("\tLocation: %s\n", p.location)
			if p.ptype&0x0002 != 0 {
				fmt.Println("\tConnection: remote")
			} else {
				fmt.Println("\tConnection: direct")
			}
			fmt.Println("\tOn fault: no alert")
			fmt.Println("\tAfter fault: continue")
			if len(p.allowed) > 0 {
				fmt.Println("\tUsers allowed:")
				for _, u := range p.allowed {
					fmt.Printf("\t\t%s\n", u)
				}
			} else if len(p.denied) > 0 {
				fmt.Println("\tUsers denied:")
				for _, u := range p.denied {
					fmt.Printf("\t\t%s\n", u)
				}
			} else {
				fmt.Println("\tUsers allowed:")
				fmt.Println("\t\t(all)")
			}
		}
	}
}

func printAccepting(printers []printerInfo, filter []string) {
	for _, p := range printers {
		if !matchesFilter(filter, p.name) {
			continue
		}
		stateTime := formatCupsDate(p.stateChange)
		if p.accepting {
			fmt.Printf("%s accepting requests since %s\n", p.name, stateTime)
		} else {
			fmt.Printf("%s not accepting requests since %s -\n", p.name, stateTime)
			if p.stateMsg != "" {
				fmt.Printf("\t%s\n", p.stateMsg)
			} else {
				fmt.Println("\treason unknown")
			}
		}
	}
}

func printDevices(printers []printerInfo, filter []string) {
	for _, p := range printers {
		if !matchesFilter(filter, p.name) {
			continue
		}
		if p.deviceURI != "" {
			fmt.Printf("device for %s: %s\n", p.name, trimFileScheme(p.deviceURI))
		} else if p.uri != "" {
			fmt.Printf("device for %s: %s\n", p.name, p.uri)
		}
	}
}

func printDestinations(printers []printerInfo) {
	for _, p := range printers {
		fmt.Println(p.name)
	}
}

func printClasses(client *cupsclient.Client, filter []string, longListing bool) error {
	_ = longListing
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetClasses, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("printer-name"),
		goipp.String("member-names"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		name := findAttr(g.Attrs, "printer-name")
		if name == "" {
			continue
		}
		if !matchesFilter(filter, name) {
			continue
		}
		members := attrStrings(g.Attrs, "member-names")
		if len(members) == 0 {
			fmt.Printf("class %s is empty\n", name)
			continue
		}
		fmt.Printf("class %s is %s\n", name, strings.Join(members, " "))
	}
	return nil
}

func printJobs(client *cupsclient.Client, printerFilter, userFilter []string, whichJobs string, showRanking bool, longStatus int) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("job-id"),
		goipp.String("job-k-octets"),
		goipp.String("job-originating-user-name"),
		goipp.String("job-printer-state-message"),
		goipp.String("job-printer-uri"),
		goipp.String("job-state-reasons"),
		goipp.String("time-at-creation"),
		goipp.String("time-at-completed"),
	))
	if whichJobs != "" {
		use := whichJobs
		if whichJobs == "successful" {
			use = "completed"
		}
		req.Operation.Add(goipp.MakeAttribute("which-jobs", goipp.TagKeyword, goipp.String(use)))
	}
	if len(printerFilter) == 1 {
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printerFilter[0]))))
	}
	if len(userFilter) == 1 {
		req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(userFilter[0])))
		req.Operation.Add(goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	rank := -1
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagJobGroup {
			continue
		}
		id := findAttr(g.Attrs, "job-id")
		user := findAttr(g.Attrs, "job-originating-user-name")
		stateMsg := findAttr(g.Attrs, "job-printer-state-message")
		reasons := attrStrings(g.Attrs, "job-state-reasons")
		printerURI := findAttr(g.Attrs, "job-printer-uri")
		printerName := printerNameFromURI(printerURI)
		jobSize := parseInt(findAttr(g.Attrs, "job-k-octets"))
		jobTime := parseInt64(findAttr(g.Attrs, "time-at-creation"))
		if whichJobs == "completed" || whichJobs == "successful" || whichJobs == "all" {
			if t := parseInt64(findAttr(g.Attrs, "time-at-completed")); t > 0 {
				jobTime = t
			}
		}
		if !matchesFilter(printerFilter, printerName) {
			continue
		}
		if !matchesFilter(userFilter, user) {
			continue
		}
		if whichJobs == "successful" {
			if len(reasons) == 0 || reasons[0] != "job-completed-successfully" {
				continue
			}
		}
		if id == "" {
			continue
		}
		rank++
		jobName := fmt.Sprintf("%s-%s", printerName, id)
		date := formatCupsDate(jobTime)
		bytes := float64(jobSize) * 1024.0
		if showRanking {
			fmt.Printf("%3d %-21s %-13s %8.0f %s\n", rank, jobName, defaultUser(user), bytes, date)
		} else {
			fmt.Printf("%-23s %-13s %8.0f   %s\n", jobName, defaultUser(user), bytes, date)
		}
		if longStatus > 0 {
			if stateMsg != "" {
				fmt.Printf("\tStatus: %s\n", stateMsg)
			}
			if len(reasons) > 0 {
				fmt.Printf("\tAlerts: %s\n", strings.Join(reasons, " "))
			}
			fmt.Printf("\tqueued for %s\n", printerName)
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

func attrStrings(attrs goipp.Attributes, name string) []string {
	for _, a := range attrs {
		if a.Name != name || len(a.Values) == 0 {
			continue
		}
		out := make([]string, 0, len(a.Values))
		for _, v := range a.Values {
			out = append(out, v.V.String())
		}
		return out
	}
	return nil
}

func matchesFilter(filter []string, value string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if strings.EqualFold(strings.TrimSpace(f), "all") {
			return true
		}
	}
	for _, f := range filter {
		if strings.EqualFold(strings.TrimSpace(f), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
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

func trimFileScheme(uri string) string {
	if strings.HasPrefix(uri, "file:") {
		return strings.TrimPrefix(uri, "file:")
	}
	return uri
}

func parseInt(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

func parseInt64(raw string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return n
}

func formatCupsDate(epoch int64) string {
	if epoch <= 0 {
		return ""
	}
	t := time.Unix(epoch, 0).Local()
	return t.Format("Mon Jan _2 15:04:05 2006")
}

func containsReason(list []string, reason string) bool {
	for _, v := range list {
		if strings.EqualFold(v, reason) {
			return true
		}
	}
	return false
}

func defaultUser(user string) string {
	if strings.TrimSpace(user) == "" {
		return "unknown"
	}
	return user
}

func fetchCurrentJobID(client *cupsclient.Client, printer string) int {
	if client == nil || printer == "" {
		return 0
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(printer))))
	req.Operation.Add(goipp.MakeAttribute("limit", goipp.TagInteger, goipp.Integer(20)))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("job-id"),
		goipp.String("job-state"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return 0
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagJobGroup {
			continue
		}
		state := parseInt(findAttr(g.Attrs, "job-state"))
		if state != 5 {
			continue
		}
		id := parseInt(findAttr(g.Attrs, "job-id"))
		if id > 0 {
			return id
		}
	}
	return 0
}
