package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	showDevices bool
	showModels  bool
	filter      string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}
	if !opts.showDevices && !opts.showModels {
		opts.showDevices = true
	}
	client := cupsclient.NewFromEnv()

	if opts.showDevices {
		if err := listDevices(client, opts.filter); err != nil {
			fail(err)
		}
	}
	if opts.showModels {
		if err := listModels(client, opts.filter); err != nil {
			fail(err)
		}
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-v":
			opts.showDevices = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.filter = args[i]
			}
		case "-m":
			opts.showModels = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.filter = args[i]
			}
		}
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpinfo:", err)
	os.Exit(1)
}

func listDevices(client *cupsclient.Client, filter string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDevices, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
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
		class := findAttr(g.Attrs, "device-class")
		if uri == "" {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(uri), strings.ToLower(filter)) && !strings.Contains(strings.ToLower(info), strings.ToLower(filter)) {
			continue
		}
		fmt.Printf("%s %s \"%s\" (%s)\n", class, uri, info, makeModel)
	}
	return nil
}

func listModels(client *cupsclient.Client, filter string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPpds, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
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
		out := fmt.Sprintf("%s %s", name, makeModel)
		if makeName != "" && makeModel == "" {
			out = fmt.Sprintf("%s %s", name, makeName)
		}
		if filter != "" && !strings.Contains(strings.ToLower(out), strings.ToLower(filter)) {
			continue
		}
		fmt.Println(out)
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
