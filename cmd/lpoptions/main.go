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

	"cupsgolang/internal/config"
	"cupsgolang/internal/cupsclient"
)

type options struct {
	printer string
	defDest string
	list    bool
	setOps  []string
	rmOps   []string
	remove  bool
	server  string
	encrypt bool
	user    string
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

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)
	if opts.list {
		if err := printSupported(client, dest, store); err != nil {
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
		if args[i] != "-h" && args[i] != "-E" && args[i] != "-U" {
			seenOther = true
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
	if env := os.Getenv("LPDEST"); env != "" {
		return env
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

func printSupported(client *cupsclient.Client, dest string, store *lpOptionsFile) error {
	ppd, err := fetchPPD(client, dest)
	if err != nil {
		return err
	}
	if ppd == nil {
		return fmt.Errorf("lpoptions: No printers.")
	}
	destOpts := map[string]string{}
	if store != nil && store.Dests[dest] != nil {
		for k, v := range store.Dests[dest] {
			destOpts[k] = v
		}
	}
	listPPDOptions(ppd, destOpts)
	return nil
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

func fetchPPD(client *cupsclient.Client, dest string) (*config.PPD, error) {
	if client == nil || dest == "" {
		return nil, fmt.Errorf("lpoptions: No printers.")
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPpd, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(dest))))
	resp, payload, err := client.SendWithPayload(context.Background(), req, nil)
	if err != nil {
		return nil, err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return nil, fmt.Errorf("lpoptions: %s", goipp.Status(resp.Code))
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("lpoptions: Unable to get PPD file for %s.", dest)
	}
	tmp, err := os.CreateTemp("", "lpoptions-*.ppd")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	ppd, err := config.LoadPPD(path)
	_ = os.Remove(path)
	if err != nil {
		return nil, err
	}
	return ppd, nil
}

func listPPDOptions(ppd *config.PPD, destOpts map[string]string) {
	if ppd == nil {
		return
	}
	seen := map[string]bool{}
	for _, group := range ppd.Groups {
		for _, opt := range group.Options {
			if opt == nil || strings.EqualFold(opt.Keyword, "PageRegion") {
				continue
			}
			if seen[opt.Keyword] {
				continue
			}
			seen[opt.Keyword] = true
			fmt.Println(formatPPDOption(ppd, opt, destOpts))
		}
	}
	for keyword, opt := range ppd.OptionDetails {
		if opt == nil || strings.EqualFold(keyword, "PageRegion") {
			continue
		}
		if seen[keyword] {
			continue
		}
		seen[keyword] = true
		fmt.Println(formatPPDOption(ppd, opt, destOpts))
	}
}

func formatPPDOption(ppd *config.PPD, opt *config.PPDOption, destOpts map[string]string) string {
	keyword := opt.Keyword
	if keyword == "" {
		return ""
	}
	title := opt.Text
	if title == "" {
		title = keyword
	}
	selected := resolveSelectedChoices(opt, destOpts)
	sb := strings.Builder{}
	sb.WriteString(keyword)
	sb.WriteString("/")
	sb.WriteString(title)
	sb.WriteString(":")
	for _, choice := range opt.Choices {
		name := choice.Choice
		if name == "" {
			continue
		}
		mark := ""
		if selected[name] {
			mark = "*"
		}
		if strings.EqualFold(name, "Custom") {
			sb.WriteString(" ")
			sb.WriteString(mark)
			sb.WriteString(formatCustomChoice(opt))
		} else {
			sb.WriteString(" ")
			sb.WriteString(mark)
			sb.WriteString(name)
		}
	}
	return sb.String()
}

func resolveSelectedChoices(opt *config.PPDOption, destOpts map[string]string) map[string]bool {
	selected := map[string]bool{}
	if opt == nil {
		return selected
	}
	value := ""
	if destOpts != nil {
		for k, v := range destOpts {
			if strings.EqualFold(k, opt.Keyword) {
				value = v
				break
			}
		}
	}
	if value == "" {
		value = opt.Default
	}
	if value == "" {
		return selected
	}
	values := splitChoiceList(value)
	for _, v := range values {
		if strings.HasPrefix(strings.ToLower(v), "custom") {
			selected["Custom"] = true
			continue
		}
		selected[v] = true
	}
	return selected
}

func splitChoiceList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formatCustomChoice(opt *config.PPDOption) string {
	if opt == nil || !opt.Custom || len(opt.CustomParams) == 0 {
		return "Custom"
	}
	if strings.EqualFold(opt.Keyword, "PageSize") || strings.EqualFold(opt.Keyword, "PageRegion") {
		return "Custom.WIDTHxHEIGHT"
	}
	if len(opt.CustomParams) == 1 {
		return "Custom." + opt.CustomParams[0].Type
	}
	parts := make([]string, 0, len(opt.CustomParams))
	for _, p := range opt.CustomParams {
		if p.Name == "" {
			continue
		}
		parts = append(parts, p.Name+"="+p.Type)
	}
	if len(parts) == 0 {
		return "Custom"
	}
	return "{" + strings.Join(parts, " ") + "}"
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
