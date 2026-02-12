package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/config"
	"cupsgolang/internal/cupsclient"
)

var errShowHelp = errors.New("show-help")

type options struct {
	printer    string
	defDest    string
	list       bool
	setOps     []string
	rmOps      []string
	removeDest string
	server     string
	encrypt    bool
	user       string
}

type lpOptionsFile struct {
	Default string
	Dests   map[string]map[string]string
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

	store, err := loadLpOptions()
	if err != nil {
		fail(err)
	}

	client := cupsclient.NewFromConfig(
		cupsclient.WithServer(opts.server),
		cupsclient.WithTLS(opts.encrypt),
		cupsclient.WithUser(opts.user),
	)

	if opts.defDest != "" {
		if err := setDefaultDestination(client, store, opts.defDest); err != nil {
			fail(err)
		}
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	if opts.removeDest != "" {
		removeDestination(store, opts.removeDest)
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	dest := resolveDest(opts.printer, store)
	if dest == "" {
		if def, err := fetchDefaultDestination(client); err == nil && def != "" {
			dest = def
		}
	}

	if len(opts.setOps) > 0 || len(opts.rmOps) > 0 {
		if dest == "" {
			fail(errors.New("No printers."))
		}
		applyOptionEdits(store, dest, opts.setOps, opts.rmOps)
		if err := saveLpOptions(store); err != nil {
			fail(err)
		}
		return
	}

	if opts.list {
		if dest == "" {
			fail(errors.New("No printers."))
		}
		if err := printSupported(client, dest, store); err != nil {
			fail(err)
		}
		return
	}

	if dest == "" {
		return
	}
	if err := printDefaults(client, dest, store); err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: lpoptions [options]")
	fmt.Fprintln(os.Stderr, "Options:")
	fmt.Fprintln(os.Stderr, "  -E                      Encrypt connection")
	fmt.Fprintln(os.Stderr, "  -h server[:port]        Connect to server")
	fmt.Fprintln(os.Stderr, "  -U username             Authenticate as user")
	fmt.Fprintln(os.Stderr, "  -p destination          Select destination")
	fmt.Fprintln(os.Stderr, "  -d destination          Set default destination")
	fmt.Fprintln(os.Stderr, "  -l                      List supported options")
	fmt.Fprintln(os.Stderr, "  -o name=value           Set option")
	fmt.Fprintln(os.Stderr, "  -r name                 Remove option")
	fmt.Fprintln(os.Stderr, "  -x destination          Remove destination from lpoptions")
	os.Exit(1)
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
			return opts, fmt.Errorf("unknown option %q", arg)
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
			case 'p':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.printer = strings.TrimSpace(v)
			case 'd':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.defDest = strings.TrimSpace(v)
			case 'l':
				opts.list = true
			case 'o':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.setOps = append(opts.setOps, strings.TrimSpace(v))
			case 'r':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.rmOps = append(opts.rmOps, strings.TrimSpace(v))
			case 'x':
				v, err := consume(ch)
				if err != nil {
					return opts, err
				}
				opts.removeDest = strings.TrimSpace(v)
			default:
				return opts, fmt.Errorf("unknown option '-%c'", ch)
			}
			if ch != 'h' && ch != 'E' && ch != 'U' {
				seenOther = true
			}
		}
	}
	return opts, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpoptions:", err)
	os.Exit(1)
}

func setDefaultDestination(client *cupsclient.Client, store *lpOptionsFile, dest string) error {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return errors.New("Unknown printer or class.")
	}
	if err := ensureDestinationExists(client, store, dest); err != nil {
		return err
	}
	store.Default = dest
	if store.Dests[dest] == nil {
		store.Dests[dest] = map[string]string{}
	}
	return nil
}

func ensureDestinationExists(client *cupsclient.Client, store *lpOptionsFile, dest string) error {
	if store != nil {
		if _, ok := store.Dests[dest]; ok {
			return nil
		}
	}
	name, _ := splitDestination(dest)
	if name == "" {
		return errors.New("Unknown printer or class.")
	}
	remote, err := remoteDestinations(client)
	if err != nil {
		return err
	}
	if remote[strings.ToLower(name)] {
		return nil
	}
	return errors.New("Unknown printer or class.")
}

func remoteDestinations(client *cupsclient.Client) (map[string]bool, error) {
	out := map[string]bool{}
	for _, op := range []goipp.Op{goipp.OpCupsGetPrinters, goipp.OpCupsGetClasses} {
		req := goipp.NewRequest(goipp.DefaultVersion, op, uint32(time.Now().UnixNano()))
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-name")))
		resp, err := client.Send(context.Background(), req, nil)
		if err != nil {
			if op == goipp.OpCupsGetClasses && strings.Contains(strings.ToLower(err.Error()), "operation-not-supported") {
				continue
			}
			return nil, err
		}
		for _, g := range resp.Groups {
			if g.Tag != goipp.TagPrinterGroup {
				continue
			}
			if name := findAttr(g.Attrs, "printer-name"); name != "" {
				out[strings.ToLower(name)] = true
			}
		}
	}
	return out, nil
}

func removeDestination(store *lpOptionsFile, target string) {
	if store == nil {
		return
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	name, instance := splitDestination(target)
	if instance != "" {
		removeDestKey(store, target)
		return
	}
	prefix := strings.ToLower(name) + "/"
	for key := range store.Dests {
		lower := strings.ToLower(key)
		if lower == strings.ToLower(name) || strings.HasPrefix(lower, prefix) {
			removeDestKey(store, key)
		}
	}
}

func removeDestKey(store *lpOptionsFile, key string) {
	delete(store.Dests, key)
	if strings.EqualFold(store.Default, key) {
		store.Default = ""
	}
}

func splitDestination(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if idx := strings.Index(value, "/"); idx > 0 && idx < len(value)-1 {
		return value[:idx], value[idx+1:]
	}
	return value, ""
}

func fetchDefaultDestination(client *cupsclient.Client) (string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return "", err
	}
	if name := findAttr(resp.Printer, "printer-name"); name != "" {
		return name, nil
	}
	return "", nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
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
	if store != nil && len(store.Dests) > 0 {
		names := make([]string, 0, len(store.Dests))
		for name := range store.Dests {
			names = append(names, name)
		}
		sort.Strings(names)
		return names[0]
	}
	return ""
}

func printDefaults(client *cupsclient.Client, dest string, store *lpOptionsFile) error {
	defaults := map[string]string{}
	remote, err := fetchPrinterDefaults(client, dest)
	if err != nil && (store == nil || len(store.Dests[dest]) == 0) {
		return err
	}
	for k, v := range remote {
		defaults[k] = v
	}
	if store != nil {
		if local := store.Dests[dest]; len(local) > 0 {
			for k, v := range local {
				defaults[k] = v
			}
		}
	}
	fmt.Println(strings.Join(formatOptionTokens(defaults), " "))
	return nil
}

func fetchPrinterDefaults(client *cupsclient.Client, dest string) (map[string]string, error) {
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
		return nil, err
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
	return defaults, nil
}

func addDefault(out map[string]string, attrs goipp.Attributes, src, dest string) {
	for _, a := range attrs {
		if a.Name != src || len(a.Values) == 0 {
			continue
		}
		parts := make([]string, 0, len(a.Values))
		for _, v := range a.Values {
			parts = append(parts, strings.TrimSpace(v.V.String()))
		}
		out[dest] = strings.TrimSpace(strings.Join(parts, ","))
		return
	}
}

func formatOptionTokens(options map[string]string) []string {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		value := options[key]
		if value == "" {
			out = append(out, key)
			continue
		}
		out = append(out, key+"="+quoteOptionValue(value))
	}
	return out
}

func quoteOptionValue(value string) string {
	if !strings.ContainsAny(value, " 	") {
		return value
	}
	escaped := strings.ReplaceAll(value, "'", "\\'")
	return "'" + escaped + "'"
}

func printSupported(client *cupsclient.Client, dest string, store *lpOptionsFile) error {
	ppd, err := fetchPPD(client, dest)
	if err != nil {
		return err
	}
	if ppd == nil {
		return errors.New("No printers.")
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
	for _, raw := range setOps {
		for _, opt := range splitOptionWords(raw) {
			key, val := splitOpt(opt)
			if key == "" {
				continue
			}
			if val == "" {
				val = "true"
			}
			store.Dests[dest][key] = val
		}
	}
	for _, opt := range rmOps {
		key := strings.TrimSpace(opt)
		if key == "" {
			continue
		}
		for existing := range store.Dests[dest] {
			if strings.EqualFold(existing, key) {
				delete(store.Dests[dest], existing)
			}
		}
	}
}

func splitOptionWords(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	out := []string{}
	var current strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		out = append(out, current.String())
		current.Reset()
	}
	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return out
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
		return nil, errors.New("No printers.")
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
		return nil, fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("Unable to get PPD file for %s.", dest)
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
