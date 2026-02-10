package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	server         string
	encrypt        bool
	user           string
	showDevices    bool
	showModels     bool
	longListing    bool
	timeout        int
	includeSchemes string
	excludeSchemes string
	deviceID       string
	language       string
	makeModel      string
	product        string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
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
		case "-l":
			opts.longListing = true
		case "--device-id":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --device-id")
			}
			i++
			opts.deviceID = args[i]
		case "--exclude-schemes":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --exclude-schemes")
			}
			i++
			opts.excludeSchemes = args[i]
		case "--include-schemes":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --include-schemes")
			}
			i++
			opts.includeSchemes = args[i]
		case "--language":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --language")
			}
			i++
			opts.language = args[i]
		case "--make-and-model":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --make-and-model")
			}
			i++
			opts.makeModel = args[i]
		case "--product":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --product")
			}
			i++
			opts.product = args[i]
		case "--timeout":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for --timeout")
			}
			i++
			if n, err := strconv.Atoi(args[i]); err == nil {
				opts.timeout = n
			}
		case "-v":
			opts.showDevices = true
		case "-m":
			opts.showModels = true
		}
		if args[i] != "-h" && args[i] != "-E" && args[i] != "-U" {
			seenOther = true
		}
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpinfo:", err)
	os.Exit(1)
}

func listDevices(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDevices, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	if opts.timeout > 0 {
		req.Operation.Add(goipp.MakeAttribute("timeout", goipp.TagInteger, goipp.Integer(opts.timeout)))
	}
	if opts.includeSchemes != "" {
		req.Operation.Add(goipp.MakeAttribute("include-schemes", goipp.TagName, goipp.String(opts.includeSchemes)))
	}
	if opts.excludeSchemes != "" {
		req.Operation.Add(goipp.MakeAttribute("exclude-schemes", goipp.TagName, goipp.String(opts.excludeSchemes)))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		uri := findAttr(g.Attrs, "device-uri")
		info := findAttr(g.Attrs, "device-info")
		makeModel := findAttr(g.Attrs, "device-make-and-model")
		deviceID := findAttr(g.Attrs, "device-id")
		location := findAttr(g.Attrs, "device-location")
		class := findAttr(g.Attrs, "device-class")
		if uri == "" {
			continue
		}
		if opts.longListing {
			fmt.Printf("device-class %s\n", class)
			fmt.Printf("device-uri %s\n", uri)
			if info != "" {
				fmt.Printf("device-info %s\n", info)
			}
			if makeModel != "" {
				fmt.Printf("device-make-and-model %s\n", makeModel)
			}
			if deviceID != "" {
				fmt.Printf("device-id %s\n", deviceID)
			}
			if location != "" {
				fmt.Printf("device-location %s\n", location)
			}
			fmt.Println()
		} else {
			fmt.Printf("%s %s \"%s\" (%s)\n", class, uri, info, makeModel)
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
	if opts.includeSchemes != "" {
		req.Operation.Add(goipp.MakeAttribute("include-schemes", goipp.TagName, goipp.String(opts.includeSchemes)))
	}
	if opts.excludeSchemes != "" {
		req.Operation.Add(goipp.MakeAttribute("exclude-schemes", goipp.TagName, goipp.String(opts.excludeSchemes)))
	}
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
			fmt.Println(out)
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
