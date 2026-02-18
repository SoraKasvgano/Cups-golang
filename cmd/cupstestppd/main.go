package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"cupsgolang/internal/config"
)

const (
	warnNone = 0

	warnConstraints  = 1
	warnDefaults     = 2
	warnFilters      = 4
	warnProfiles     = 8
	warnTranslations = 16
	warnDuplex       = 32
	warnSizes        = 64
	warnFilename     = 128
	warnAll          = 255
)

const (
	exitOK          = 0
	exitUsage       = 1
	exitFileOpen    = 2
	exitPPDFormat   = 3
	exitConformance = 4
)

var errShowHelp = errors.New("show-help")

type options struct {
	ignore  int
	rootDir string
	warn    int
	verbose int
	relaxed bool
	files   []string
}

type ppdIssue struct {
	category int
	message  string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if errors.Is(err, errShowHelp) {
		usage(stdout)
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(stderr, "cupstestppd: %v\n", err)
		return exitUsage
	}
	if len(opts.files) == 0 {
		usage(stdout)
		return exitUsage
	}

	status := exitOK
	var stdinData []byte
	var stdinLoaded bool

	for i, path := range opts.files {
		if i > 0 && opts.verbose >= 0 {
			fmt.Fprintln(stdout)
		}

		display := path
		if path == "-" {
			display = "(stdin)"
		}
		if opts.verbose >= 0 {
			fmt.Fprintf(stdout, "%s:", display)
		}

		raw, ppd, loadCode, loadErr := loadPPD(path, stdin, &stdinData, &stdinLoaded)
		if loadErr != "" {
			if opts.verbose >= 0 {
				if opts.verbose == 0 {
					fmt.Fprint(stdout, " FAIL")
				}
				fmt.Fprintf(stdout, "\n      **FAIL**  %s", loadErr)
				fmt.Fprintln(stdout)
			}
			status = maxStatus(status, loadCode)
			continue
		}

		if !looksLikePPD(raw) {
			if opts.verbose >= 0 {
				if opts.verbose == 0 {
					fmt.Fprint(stdout, " FAIL")
				}
				fmt.Fprintln(stdout, "\n      **FAIL**  Missing required *PPD-Adobe header.")
			}
			status = maxStatus(status, exitPPDFormat)
			continue
		}

		issues := validatePPD(path, raw, ppd, opts)
		var hardErrors []string
		var warnings []string
		for _, issue := range issues {
			if opts.ignore&issue.category != 0 {
				continue
			}
			if opts.warn&issue.category != 0 {
				warnings = append(warnings, issue.message)
			} else {
				hardErrors = append(hardErrors, issue.message)
			}
		}

		if len(hardErrors) > 0 {
			status = maxStatus(status, exitConformance)
		}

		if opts.verbose >= 0 {
			if opts.verbose == 0 {
				if len(hardErrors) > 0 {
					fmt.Fprint(stdout, " FAIL")
				} else {
					fmt.Fprint(stdout, " PASS")
				}
			} else {
				fmt.Fprintln(stdout)
				fmt.Fprintln(stdout, "    DETAILED CONFORMANCE TEST RESULTS")
			}

			for _, msg := range hardErrors {
				fmt.Fprintf(stdout, "\n      **FAIL**  %s", msg)
			}
			for _, msg := range warnings {
				fmt.Fprintf(stdout, "\n        WARN    %s", msg)
			}

			if opts.verbose >= 2 {
				printVerboseSummary(stdout, ppd)
			}

			fmt.Fprintln(stdout)
		}
	}

	return status
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Warning: This program will be removed in a future version of CUPS.")
	fmt.Fprintln(w, "Usage: cupstestppd [options] filename1.ppd[.gz] [... filenameN.ppd[.gz]]")
	fmt.Fprintln(w, "       program | cupstestppd [options] -")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "-I {filename,filters,none,profiles}  Ignore specific warnings")
	fmt.Fprintln(w, "-R root-directory                    Set alternate root")
	fmt.Fprintln(w, "-W {all,none,constraints,defaults,duplex,filters,profiles,sizes,translations}")
	fmt.Fprintln(w, "                                     Issue warnings instead of errors")
	fmt.Fprintln(w, "-q                                   Run silently")
	fmt.Fprintln(w, "-r                                   Use relaxed open mode")
	fmt.Fprintln(w, "-v                                   Be verbose")
	fmt.Fprintln(w, "-vv                                  Be very verbose")
}

func parseArgs(args []string) (options, error) {
	opts := options{
		ignore:  warnNone,
		rootDir: "",
		warn:    warnNone,
	}

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
		if strings.HasPrefix(arg, "-") && arg != "-" {
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
					return strings.TrimSpace(args[i]), nil
				}

				switch ch {
				case 'I':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					category, ok := parseIgnoreCategory(v)
					if !ok {
						return opts, fmt.Errorf("unknown -I category %q", v)
					}
					if strings.EqualFold(v, "none") {
						opts.ignore = warnNone
					} else if strings.EqualFold(v, "all") {
						// CUPS "all" maps to filters+profiles only.
						opts.ignore = warnFilters | warnProfiles
					} else {
						opts.ignore |= category
					}
				case 'R':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.rootDir = v
				case 'W':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					category, ok := parseWarnCategory(v)
					if !ok {
						return opts, fmt.Errorf("unknown -W category %q", v)
					}
					if strings.EqualFold(v, "none") {
						opts.warn = warnNone
					} else if strings.EqualFold(v, "all") {
						opts.warn = warnAll
					} else {
						opts.warn |= category
					}
				case 'q':
					if opts.verbose > 0 {
						return opts, errors.New("The -q option is incompatible with the -v option.")
					}
					opts.verbose--
				case 'r':
					opts.relaxed = true
				case 'v':
					if opts.verbose < 0 {
						return opts, errors.New("The -v option is incompatible with the -q option.")
					}
					opts.verbose++
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}

		opts.files = append(opts.files, arg)
	}

	return opts, nil
}

func parseIgnoreCategory(raw string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none":
		return warnNone, true
	case "filename":
		return warnFilename, true
	case "filters":
		return warnFilters, true
	case "profiles":
		return warnProfiles, true
	case "all":
		return warnFilters | warnProfiles, true
	default:
		return 0, false
	}
}

func parseWarnCategory(raw string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none":
		return warnNone, true
	case "constraints":
		return warnConstraints, true
	case "defaults":
		return warnDefaults, true
	case "duplex":
		return warnDuplex, true
	case "filters":
		return warnFilters, true
	case "profiles":
		return warnProfiles, true
	case "sizes":
		return warnSizes, true
	case "translations":
		return warnTranslations, true
	case "all":
		return warnAll, true
	default:
		return 0, false
	}
}

func loadPPD(path string, stdin io.Reader, stdinData *[]byte, stdinLoaded *bool) ([]byte, *config.PPD, int, string) {
	var data []byte
	if path == "-" {
		if !*stdinLoaded {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				return nil, nil, exitFileOpen, fmt.Sprintf("Unable to open PPD file - %v.", err)
			}
			*stdinData = raw
			*stdinLoaded = true
		}
		data = append([]byte(nil), (*stdinData)...)
	} else {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, exitFileOpen, fmt.Sprintf("Unable to open PPD file - %v.", err)
		}
		data = raw
	}

	if decoded, ok, err := maybeDecompressGzip(data); err != nil {
		return nil, nil, exitPPDFormat, fmt.Sprintf("Unable to open PPD file - %v.", err)
	} else if ok {
		data = decoded
	}

	ppd, err := loadPPDFromBytes(data)
	if err != nil {
		return data, nil, exitPPDFormat, fmt.Sprintf("Unable to open PPD file - %v.", err)
	}
	if ppd == nil {
		return data, nil, exitPPDFormat, "Unable to open PPD file - invalid PPD data."
	}

	return data, ppd, exitOK, ""
}

func maybeDecompressGzip(data []byte) ([]byte, bool, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, false, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, false, err
	}
	return decoded, true, nil
}

func loadPPDFromBytes(data []byte) (*config.PPD, error) {
	tmp, err := os.CreateTemp("", "cupstestppd-*.ppd")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return config.LoadPPD(tmpPath)
}

func looksLikePPD(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, "*PPD-Adobe:") {
			return true
		}
	}
	return false
}

func validatePPD(path string, raw []byte, ppd *config.PPD, opts options) []ppdIssue {
	issues := make([]ppdIssue, 0, 32)
	issues = append(issues, checkDefaults(ppd)...)
	issues = append(issues, checkConstraints(ppd)...)
	issues = append(issues, checkSizes(ppd)...)
	issues = append(issues, checkDuplex(ppd)...)
	issues = append(issues, checkTranslations(ppd)...)
	issues = append(issues, checkFilterPaths(ppd, opts.rootDir)...)
	issues = append(issues, checkProfilePaths(raw, opts.rootDir)...)
	if !opts.relaxed {
		issues = append(issues, checkPCFileName(path, raw)...)
	}
	return issues
}

func checkDefaults(ppd *config.PPD) []ppdIssue {
	issues := make([]ppdIssue, 0)
	for option, choices := range ppd.Options {
		if option == "PageSize" && len(choices) == 0 {
			for name := range ppd.PageSizes {
				choices = append(choices, name)
			}
			sort.Strings(choices)
		}
		if len(choices) == 0 {
			continue
		}
		def := strings.TrimSpace(ppd.Defaults[option])
		if def == "" {
			issues = append(issues, ppdIssue{
				category: warnDefaults,
				message:  fmt.Sprintf("REQUIRED Default%s", option),
			})
			continue
		}
		if !containsChoice(choices, def) {
			issues = append(issues, ppdIssue{
				category: warnDefaults,
				message:  fmt.Sprintf("Bad Default%s %s", option, def),
			})
		}
	}
	return issues
}

func checkConstraints(ppd *config.PPD) []ppdIssue {
	issues := make([]ppdIssue, 0)
	for _, c := range ppd.Constraints {
		if c.Option1 != "" {
			choices, ok := ppd.Options[c.Option1]
			if !ok {
				issues = append(issues, ppdIssue{
					category: warnConstraints,
					message:  fmt.Sprintf("UIConstraints references unknown option %s", c.Option1),
				})
			} else if c.Choice1 != "" && !containsChoice(choices, c.Choice1) {
				issues = append(issues, ppdIssue{
					category: warnConstraints,
					message:  fmt.Sprintf("UIConstraints references unknown choice %s/%s", c.Option1, c.Choice1),
				})
			}
		}

		if c.Option2 != "" {
			choices, ok := ppd.Options[c.Option2]
			if !ok {
				issues = append(issues, ppdIssue{
					category: warnConstraints,
					message:  fmt.Sprintf("UIConstraints references unknown option %s", c.Option2),
				})
			} else if c.Choice2 != "" && !containsChoice(choices, c.Choice2) {
				issues = append(issues, ppdIssue{
					category: warnConstraints,
					message:  fmt.Sprintf("UIConstraints references unknown choice %s/%s", c.Option2, c.Choice2),
				})
			}
		}
	}
	return issues
}

func checkSizes(ppd *config.PPD) []ppdIssue {
	issues := make([]ppdIssue, 0)
	pageChoices := append([]string(nil), ppd.Options["PageSize"]...)
	if len(pageChoices) == 0 {
		for name := range ppd.PageSizes {
			pageChoices = append(pageChoices, name)
		}
		sort.Strings(pageChoices)
	}
	if len(pageChoices) == 0 {
		issues = append(issues, ppdIssue{
			category: warnSizes,
			message:  "REQUIRED PageSize option",
		})
		return issues
	}
	if len(ppd.PageSizes) == 0 {
		issues = append(issues, ppdIssue{
			category: warnSizes,
			message:  "No *PageSize dimensions found",
		})
		return issues
	}

	for _, choice := range pageChoices {
		lower := strings.ToLower(strings.TrimSpace(choice))
		if lower == "custom" || strings.HasPrefix(lower, "custom.") {
			continue
		}
		if _, ok := ppd.PageSizes[choice]; !ok {
			issues = append(issues, ppdIssue{
				category: warnSizes,
				message:  fmt.Sprintf("PageSize %s has no matching dimensions", choice),
			})
		}
	}

	for name := range ppd.PageSizes {
		if !containsChoice(pageChoices, name) {
			issues = append(issues, ppdIssue{
				category: warnSizes,
				message:  fmt.Sprintf("PageSize %s has no corresponding option choice", name),
			})
		}
	}

	return issues
}

func checkDuplex(ppd *config.PPD) []ppdIssue {
	choices := ppd.Options["Duplex"]
	if len(choices) == 0 {
		return nil
	}

	issues := make([]ppdIssue, 0)
	if !containsChoice(choices, "None") {
		issues = append(issues, ppdIssue{
			category: warnDuplex,
			message:  "Duplex option is missing None/Off choice",
		})
	}
	if !containsChoice(choices, "DuplexNoTumble") || !containsChoice(choices, "DuplexTumble") {
		issues = append(issues, ppdIssue{
			category: warnDuplex,
			message:  "Duplex option should provide DuplexNoTumble and DuplexTumble choices",
		})
	}
	if def := strings.TrimSpace(ppd.Defaults["Duplex"]); def != "" && !containsChoice(choices, def) {
		issues = append(issues, ppdIssue{
			category: warnDuplex,
			message:  fmt.Sprintf("Bad DefaultDuplex %s", def),
		})
	}
	return issues
}

func checkTranslations(ppd *config.PPD) []ppdIssue {
	issues := make([]ppdIssue, 0)
	check := func(label, value string) {
		if value == "" {
			return
		}
		if !utf8.ValidString(value) {
			issues = append(issues, ppdIssue{
				category: warnTranslations,
				message:  fmt.Sprintf("%s contains invalid UTF-8 text", label),
			})
		}
	}

	check("NickName", ppd.NickName)
	check("ModelName", ppd.Model)
	check("Manufacturer", ppd.Make)

	for _, option := range ppd.OptionDetails {
		if option == nil {
			continue
		}
		check("Option text "+option.Keyword, option.Text)
		for _, choice := range option.Choices {
			check("Choice text "+option.Keyword+"/"+choice.Choice, choice.Text)
		}
	}

	return issues
}

func checkFilterPaths(ppd *config.PPD, rootDir string) []ppdIssue {
	issues := make([]ppdIssue, 0)
	for _, filter := range ppd.Filters {
		prog := strings.TrimSpace(filter.Program)
		if prog == "" {
			issues = append(issues, ppdIssue{
				category: warnFilters,
				message:  "cupsFilter contains an empty program path",
			})
			continue
		}

		progPath := strings.Fields(prog)[0]
		if !filepath.IsAbs(progPath) {
			issues = append(issues, ppdIssue{
				category: warnFilters,
				message:  fmt.Sprintf("Filter path %s is not absolute", progPath),
			})
			continue
		}

		fullPath := resolveRootPath(rootDir, progPath)
		info, err := os.Stat(fullPath)
		if err != nil {
			issues = append(issues, ppdIssue{
				category: warnFilters,
				message:  fmt.Sprintf("cupsFilter file %q does not exist", fullPath),
			})
			continue
		}
		if info.IsDir() {
			issues = append(issues, ppdIssue{
				category: warnFilters,
				message:  fmt.Sprintf("cupsFilter path %q is a directory", fullPath),
			})
		}
	}
	return issues
}

func checkProfilePaths(raw []byte, rootDir string) []ppdIssue {
	issues := make([]ppdIssue, 0)
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if !strings.HasPrefix(line, "*cupsICCProfile") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(strings.Trim(line[idx+1:], "\""))
		if value == "" {
			continue
		}
		path := strings.Fields(value)
		if len(path) == 0 {
			continue
		}
		iccPath := path[len(path)-1]
		if !filepath.IsAbs(iccPath) {
			issues = append(issues, ppdIssue{
				category: warnProfiles,
				message:  fmt.Sprintf("cupsICCProfile path %s is not absolute", iccPath),
			})
			continue
		}
		fullPath := resolveRootPath(rootDir, iccPath)
		info, err := os.Stat(fullPath)
		if err != nil {
			issues = append(issues, ppdIssue{
				category: warnProfiles,
				message:  fmt.Sprintf("cupsICCProfile file %q does not exist", fullPath),
			})
			continue
		}
		if info.IsDir() {
			issues = append(issues, ppdIssue{
				category: warnProfiles,
				message:  fmt.Sprintf("cupsICCProfile path %q is a directory", fullPath),
			})
		}
	}
	return issues
}

func checkPCFileName(path string, raw []byte) []ppdIssue {
	if path == "-" {
		return nil
	}

	ppdFileName := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if !strings.HasPrefix(line, "*PCFileName:") {
			continue
		}
		ppdFileName = strings.TrimSpace(strings.Trim(line[len("*PCFileName:"):], "\""))
		fields := strings.Fields(ppdFileName)
		if len(fields) == 0 {
			ppdFileName = ""
			break
		}
		ppdFileName = fields[0]
		break
	}
	if ppdFileName == "" {
		return nil
	}

	actual := filepath.Base(path)
	if ppdFileName == actual {
		return nil
	}
	return []ppdIssue{{
		category: warnFilename,
		message:  fmt.Sprintf("PCFileName %q does not match file name %q", ppdFileName, actual),
	}}
}

func resolveRootPath(rootDir, path string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" || rootDir == "/" || rootDir == "." {
		return path
	}
	trimmed := strings.TrimLeft(path, "/\\")
	return filepath.Join(rootDir, trimmed)
}

func containsChoice(choices []string, want string) bool {
	for _, choice := range choices {
		if strings.EqualFold(choice, want) {
			return true
		}
	}
	return false
}

func printVerboseSummary(w io.Writer, ppd *config.PPD) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    FILE SUMMARY")
	fmt.Fprintf(w, "        NickName = %s\n", fallbackValue(ppd.NickName, "(none)"))
	fmt.Fprintf(w, "        ModelName = %s\n", fallbackValue(ppd.Model, "(none)"))
	fmt.Fprintf(w, "        Manufacturer = %s\n", fallbackValue(ppd.Make, "(none)"))
	fmt.Fprintf(w, "        LanguageVersion = %s\n", fallbackValue(ppd.LanguageVersion, "(none)"))
	fmt.Fprintf(w, "        ColorDevice = %t\n", ppd.ColorDevice)

	if len(ppd.OptionDetails) == 0 {
		return
	}
	keys := make([]string, 0, len(ppd.OptionDetails))
	for key := range ppd.OptionDetails {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	fmt.Fprintln(w, "    OPTIONS")
	for _, key := range keys {
		detail := ppd.OptionDetails[key]
		if detail == nil {
			continue
		}
		choices := make([]string, 0, len(detail.Choices))
		for _, choice := range detail.Choices {
			choices = append(choices, choice.Choice)
		}
		fmt.Fprintf(w, "        %s (default=%s): %s\n", key, fallbackValue(detail.Default, "(none)"), strings.Join(choices, ", "))
	}
}

func fallbackValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func maxStatus(a, b int) int {
	if b > a {
		return b
	}
	return a
}
