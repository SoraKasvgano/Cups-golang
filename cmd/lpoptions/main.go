package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	printer string
	defDest string
	list    bool
	setOps  []string
	rmOps   []string
	remove  bool
}

type lpOptionsFile struct {
	Default string
	Dests   map[string]map[string]string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}

	store, err := loadLpOptions()
	if err != nil {
		fail(err)
	}

	if opts.defDest != "" {
		store.Default = opts.defDest
		if store.Dests[opts.defDest] == nil {
			store.Dests[opts.defDest] = map[string]string{}
		}
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	dest := resolveDest(opts.printer, store)
	if dest == "" {
		dest = "Default"
	}

	if opts.remove {
		delete(store.Dests, dest)
		if store.Default == dest {
			store.Default = ""
		}
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	if len(opts.setOps) > 0 || len(opts.rmOps) > 0 {
		applyOptionEdits(store, dest, opts.setOps, opts.rmOps)
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	client := cupsclient.NewFromEnv()
	if opts.list {
		if err := printSupported(client, dest); err != nil {
			fail(err)
		}
		return
	}
	if err := printDefaults(client, dest, store); err != nil {
		fail(err)
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -p")
			}
			i++
			opts.printer = args[i]
		case "-d":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -d")
			}
			i++
			opts.defDest = args[i]
		case "-l":
			opts.list = true
		case "-o":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -o")
			}
			i++
			opts.setOps = append(opts.setOps, args[i])
		case "-r":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing argument for -r")
			}
			i++
			opts.rmOps = append(opts.rmOps, args[i])
		case "-x":
			opts.remove = true
		}
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpoptions:", err)
	os.Exit(1)
}

func resolveDest(arg string, store *lpOptionsFile) string {
	if arg != "" {
		return arg
	}
	if store != nil && store.Default != "" {
		return store.Default
	}
	if env := os.Getenv("PRINTER"); env != "" {
		return env
	}
	if env := os.Getenv("CUPS_PRINTER"); env != "" {
		return env
	}
	return ""
}

func printDefaults(client *cupsclient.Client, dest string, store *lpOptionsFile) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(dest))))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("media-default"),
		goipp.String("sides-default"),
		goipp.String("print-color-mode-default"),
		goipp.String("printer-resolution-default"),
		goipp.String("output-bin-default"),
		goipp.String("print-quality-default"),
		goipp.String("finishings-default"),
		goipp.String("number-up-default"),
		goipp.String("orientation-requested-default"),
		goipp.String("page-delivery-default"),
		goipp.String("print-scaling-default"),
		goipp.String("job-sheets-default"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}

	defaults := map[string]string{}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		addDefault(defaults, g.Attrs, "media-default", "media")
		addDefault(defaults, g.Attrs, "sides-default", "sides")
		addDefault(defaults, g.Attrs, "print-color-mode-default", "print-color-mode")
		addDefault(defaults, g.Attrs, "printer-resolution-default", "printer-resolution")
		addDefault(defaults, g.Attrs, "output-bin-default", "output-bin")
		addDefault(defaults, g.Attrs, "print-quality-default", "print-quality")
		addDefault(defaults, g.Attrs, "finishings-default", "finishings")
		addDefault(defaults, g.Attrs, "number-up-default", "number-up")
		addDefault(defaults, g.Attrs, "orientation-requested-default", "orientation-requested")
		addDefault(defaults, g.Attrs, "page-delivery-default", "page-delivery")
		addDefault(defaults, g.Attrs, "print-scaling-default", "print-scaling")
		addDefault(defaults, g.Attrs, "job-sheets-default", "job-sheets")
	}

	if store != nil {
		if local := store.Dests[dest]; len(local) > 0 {
			for k, v := range local {
				defaults[k] = v
			}
		}
	}

	keys := make([]string, 0, len(defaults))
	for k := range defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, defaults[k])
	}
	return nil
}

func addDefault(out map[string]string, attrs goipp.Attributes, src, dest string) {
	for _, a := range attrs {
		if a.Name != src || len(a.Values) == 0 {
			continue
		}
		out[dest] = a.Values[0].V.String()
	}
}

func printSupported(client *cupsclient.Client, dest string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(dest))))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("media-supported"),
		goipp.String("sides-supported"),
		goipp.String("print-color-mode-supported"),
		goipp.String("printer-resolution-supported"),
		goipp.String("output-bin-supported"),
		goipp.String("print-quality-supported"),
		goipp.String("finishings-supported"),
		goipp.String("number-up-supported"),
		goipp.String("orientation-requested-supported"),
		goipp.String("page-delivery-supported"),
		goipp.String("print-scaling-supported"),
		goipp.String("job-sheets-supported"),
		goipp.String("job-hold-until-supported"),
	))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		printList(g.Attrs, "media-supported")
		printList(g.Attrs, "sides-supported")
		printList(g.Attrs, "print-color-mode-supported")
		printList(g.Attrs, "printer-resolution-supported")
		printList(g.Attrs, "output-bin-supported")
		printList(g.Attrs, "print-quality-supported")
		printList(g.Attrs, "finishings-supported")
		printList(g.Attrs, "number-up-supported")
		printList(g.Attrs, "orientation-requested-supported")
		printList(g.Attrs, "page-delivery-supported")
		printList(g.Attrs, "print-scaling-supported")
		printList(g.Attrs, "job-sheets-supported")
		printList(g.Attrs, "job-hold-until-supported")
	}
	return nil
}

func printList(attrs goipp.Attributes, name string) {
	for _, a := range attrs {
		if a.Name != name || len(a.Values) == 0 {
			continue
		}
		vals := []string{}
		for _, v := range a.Values {
			vals = append(vals, v.V.String())
		}
		fmt.Printf("%s=%s\n", strings.TrimSuffix(name, "-supported"), strings.Join(vals, ","))
	}
}

func applyOptionEdits(store *lpOptionsFile, dest string, setOps []string, rmOps []string) {
	if store.Dests[dest] == nil {
		store.Dests[dest] = map[string]string{}
	}
	for _, opt := range setOps {
		key, val := splitOpt(opt)
		if key == "" {
			continue
		}
		if val == "" {
			val = "true"
		}
		store.Dests[dest][key] = val
	}
	for _, opt := range rmOps {
		key := strings.TrimSpace(opt)
		if key != "" {
			delete(store.Dests[dest], key)
		}
	}
}

func splitOpt(opt string) (string, string) {
	if strings.Contains(opt, "=") {
		parts := strings.SplitN(opt, "=", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(opt), ""
}

func loadLpOptions() (*lpOptionsFile, error) {
	path := lpOptionsPath()
	store := &lpOptionsFile{Dests: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := strings.ToLower(fields[0])
		dest := fields[1]
		opts := map[string]string{}
		for _, token := range fields[2:] {
			key, val := splitOpt(token)
			if key == "" {
				continue
			}
			if val == "" {
				val = "true"
			}
			opts[key] = val
		}
		if kind == "default" {
			store.Default = dest
			store.Dests[dest] = opts
			continue
		}
		if kind == "dest" || kind == "printer" {
			store.Dests[dest] = opts
		}
	}
	return store, nil
}

func saveLpOptions(store *lpOptionsFile) error {
	if store == nil {
		return nil
	}
	path := lpOptionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	lines := []string{}
	if store.Default != "" {
		lines = append(lines, formatLpOptionsLine("Default", store.Default, store.Dests[store.Default]))
	}
	dests := make([]string, 0, len(store.Dests))
	for dest := range store.Dests {
		if dest == store.Default {
			continue
		}
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	for _, dest := range dests {
		lines = append(lines, formatLpOptionsLine("Dest", dest, store.Dests[dest]))
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func formatLpOptionsLine(prefix, dest string, opts map[string]string) string {
	line := prefix + " " + dest
	if len(opts) == 0 {
		return line
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line += " " + k + "=" + opts[k]
	}
	return line
}

func lpOptionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".lpoptions"
	}
	return filepath.Join(home, ".cups", "lpoptions")
}
