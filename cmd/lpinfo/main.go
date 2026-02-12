package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

var errShowHelp = errors.New("show-help")

type options struct {
	server         string
	encrypt        bool
	user           string
	showDevices    bool
	showModels     bool
	longListing    bool
	timeout        int
	includeSchemes []string
	excludeSchemes []string
	deviceID       string
	language       string
	makeModel      string
	product        string
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
	if !opts.showDevices && !opts.showModels {
		opts.showDevices = true
	}
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

	if opts.showDevices {
		if err := listDevices(client, opts); err != nil {
			fail(err)
		}
	}
	if opts.showModels {
		if err := listModels(client, opts); err != nil {
			fail(err)
		}
	}
}

func usage() {
	fmt.Println("Usage: lpinfo [options] -v | -m")
	fmt.Println("       lpinfo [options]")
	fmt.Println("Options:")
	fmt.Println("  -E                      Encrypt connection")
	fmt.Println("  -h server[:port]        Connect to server")
	fmt.Println("  -U username             Authenticate as user")
	fmt.Println("  -v                      Show devices")
	fmt.Println("  -m                      Show models (PPDs)")
	fmt.Println("  -l                      Long listing")
	fmt.Println("  --device-id id          Filter models by IEEE-1284 device id")
	fmt.Println("  --exclude-schemes list  Exclude comma/space separated URI schemes")
	fmt.Println("  --include-schemes list  Include comma/space separated URI schemes")
	fmt.Println("  --language lang         Filter models by natural language")
	fmt.Println("  --make-and-model text   Filter models by make-and-model")
	fmt.Println("  --product text          Filter models by product")
	fmt.Println("  --timeout seconds       Device discovery timeout")
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	seenOther := false
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if arg == "--help" {
			return opts, errShowHelp
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue := splitLongOption(arg)
			consume := func(optionName string) (string, error) {
				if hasValue {
					return value, nil
				}
				if i+1 >= len(args) {
					return "", fmt.Errorf("missing argument for --%s", optionName)
				}
				i++
				return args[i], nil
			}
			switch name {
			case "device-id":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.deviceID = strings.TrimSpace(v)
			case "exclude-schemes":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.excludeSchemes = append(opts.excludeSchemes, splitSchemeList(v)...)
			case "include-schemes":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.includeSchemes = append(opts.includeSchemes, splitSchemeList(v)...)
			case "language":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.language = strings.TrimSpace(v)
			case "make-and-model":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.makeModel = strings.TrimSpace(v)
			case "product":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				opts.product = strings.TrimSpace(v)
			case "timeout":
				v, err := consume(name)
				if err != nil {
					return opts, err
				}
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 0 {
					return opts, fmt.Errorf("bad timeout value %q", v)
				}
				opts.timeout = n
			default:
				return opts, fmt.Errorf("unknown option %q", arg)
			}
			seenOther = true
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
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
			case 'h':
				if seenOther {
					return opts, fmt.Errorf("-h must appear before all other options")
				}
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.server = strings.TrimSpace(v)
			case 'E':
				opts.encrypt = true
			case 'U':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.user = strings.TrimSpace(v)
			case 'l':
				opts.longListing = true
			case 'v':
				opts.showDevices = true
			case 'm':
				opts.showModels = true
			default:
				return opts, fmt.Errorf("unknown option \"-%c\"", ch)
			}
			if ch != 'h' && ch != 'E' && ch != 'U' {
				seenOther = true
			}
		}
	}
	opts.includeSchemes = uniqueStrings(opts.includeSchemes)
	opts.excludeSchemes = uniqueStrings(opts.excludeSchemes)
	return opts, nil
}

func splitLongOption(arg string) (name string, value string, hasValue bool) {
	trimmed := strings.TrimPrefix(arg, "--")
	if idx := strings.Index(trimmed, "="); idx >= 0 {
		return strings.TrimSpace(trimmed[:idx]), trimmed[idx+1:], true
	}
	return strings.TrimSpace(trimmed), "", false
}

func splitSchemeList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		norm := strings.ToLower(strings.TrimSpace(v))
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	return out
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpinfo:", err)
	os.Exit(1)
}

func addNameValues(attrs *goipp.Attributes, name string, values []string) {
	if len(values) == 0 {
		return
	}
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		vals = append(vals, goipp.String(strings.TrimSpace(v)))
	}
	if len(vals) == 0 {
		return
	}
	attrs.Add(goipp.MakeAttr(name, goipp.TagName, vals[0], vals[1:]...))
}

func listDevices(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDevices, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	if opts.timeout > 0 {
		req.Operation.Add(goipp.MakeAttribute("timeout", goipp.TagInteger, goipp.Integer(opts.timeout)))
	}
	addNameValues(&req.Operation, "include-schemes", opts.includeSchemes)
	addNameValues(&req.Operation, "exclude-schemes", opts.excludeSchemes)
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("device-class"),
		goipp.String("device-uri"),
		goipp.String("device-info"),
		goipp.String("device-make-and-model"),
		goipp.String("device-id"),
		goipp.String("device-location"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	type deviceRow struct {
		class    string
		uri      string
		info     string
		make     string
		deviceID string
		location string
	}
	rows := []deviceRow{}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		uri := findAttr(g.Attrs, "device-uri")
		if strings.TrimSpace(uri) == "" {
			continue
		}
		rows = append(rows, deviceRow{
			class:    findAttr(g.Attrs, "device-class"),
			uri:      uri,
			info:     findAttr(g.Attrs, "device-info"),
			make:     findAttr(g.Attrs, "device-make-and-model"),
			deviceID: findAttr(g.Attrs, "device-id"),
			location: findAttr(g.Attrs, "device-location"),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		li := strings.ToLower(strings.TrimSpace(rows[i].info))
		lj := strings.ToLower(strings.TrimSpace(rows[j].info))
		if li != lj {
			return li < lj
		}
		lc := strings.ToLower(strings.TrimSpace(rows[i].class))
		ljc := strings.ToLower(strings.TrimSpace(rows[j].class))
		if lc != ljc {
			return lc < ljc
		}
		return strings.ToLower(strings.TrimSpace(rows[i].uri)) < strings.ToLower(strings.TrimSpace(rows[j].uri))
	})
	for _, row := range rows {
		class := strings.TrimSpace(row.class)
		if class == "" {
			class = "direct"
		}
		info := row.info
		if strings.TrimSpace(info) == "" {
			info = row.uri
		}
		if opts.longListing {
			fmt.Printf("device-class %s\n", class)
			fmt.Printf("device-uri %s\n", row.uri)
			fmt.Printf("device-info %s\n", info)
			if row.make != "" {
				fmt.Printf("device-make-and-model %s\n", row.make)
			}
			if row.deviceID != "" {
				fmt.Printf("device-id %s\n", row.deviceID)
			}
			if row.location != "" {
				fmt.Printf("device-location %s\n", row.location)
			}
			fmt.Println()
		} else {
			fmt.Printf("%s %s \"%s\" (%s)\n", class, row.uri, info, row.make)
		}
	}
	return nil
}

func listModels(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPpds, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("ppd-name"),
		goipp.String("ppd-make"),
		goipp.String("ppd-make-and-model"),
		goipp.String("ppd-device-id"),
		goipp.String("ppd-natural-language"),
		goipp.String("ppd-product"),
		goipp.String("ppd-psversion"),
		goipp.String("ppd-type"),
		goipp.String("ppd-model-number"),
	))
	if opts.deviceID != "" {
		req.Operation.Add(goipp.MakeAttribute("ppd-device-id", goipp.TagText, goipp.String(opts.deviceID)))
	}
	if opts.language != "" {
		req.Operation.Add(goipp.MakeAttribute("ppd-natural-language", goipp.TagLanguage, goipp.String(opts.language)))
	}
	if opts.makeModel != "" {
		req.Operation.Add(goipp.MakeAttribute("ppd-make-and-model", goipp.TagText, goipp.String(opts.makeModel)))
	}
	if opts.product != "" {
		req.Operation.Add(goipp.MakeAttribute("ppd-product", goipp.TagText, goipp.String(opts.product)))
	}
	addNameValues(&req.Operation, "include-schemes", opts.includeSchemes)
	addNameValues(&req.Operation, "exclude-schemes", opts.excludeSchemes)
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		name := findAttr(g.Attrs, "ppd-name")
		makeModel := findAttr(g.Attrs, "ppd-make-and-model")
		makeName := findAttr(g.Attrs, "ppd-make")
		if name == "" {
			continue
		}
		if opts.longListing {
			fmt.Printf("ppd-name %s\n", name)
			if makeName != "" {
				fmt.Printf("ppd-make %s\n", makeName)
			}
			if makeModel != "" {
				fmt.Printf("ppd-make-and-model %s\n", makeModel)
			}
			if v := findAttr(g.Attrs, "ppd-device-id"); v != "" {
				fmt.Printf("ppd-device-id %s\n", v)
			}
			if v := findAttr(g.Attrs, "ppd-natural-language"); v != "" {
				fmt.Printf("ppd-natural-language %s\n", v)
			}
			if vals := findAttr(g.Attrs, "ppd-product"); vals != "" {
				fmt.Printf("ppd-product %s\n", vals)
			}
			if vals := findAttr(g.Attrs, "ppd-psversion"); vals != "" {
				fmt.Printf("ppd-psversion %s\n", vals)
			}
			if v := findAttr(g.Attrs, "ppd-type"); v != "" {
				fmt.Printf("ppd-type %s\n", v)
			}
			if v := findAttr(g.Attrs, "ppd-model-number"); v != "" {
				fmt.Printf("ppd-model-number %s\n", v)
			}
			fmt.Println()
		} else {
			out := fmt.Sprintf("%s %s", name, makeModel)
			if makeName != "" && makeModel == "" {
				out = fmt.Sprintf("%s %s", name, makeName)
			}
			fmt.Println(strings.TrimSpace(out))
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
