package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseArgsSupportsClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{
		"-hserver.example:8631",
		"-Ualice",
		"-dOffice",
		"-n2",
		"-q70",
		"-tMonthly",
		"-o", "media=A4 sides=two-sided-long-edge",
		"report.pdf",
	})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "server.example:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user: %+v", opts)
	}
	if opts.dest != "Office" || opts.copies != 2 || opts.priority != 70 || opts.title != "Monthly" {
		t.Fatalf("unexpected core options: %+v", opts)
	}
	if len(opts.files) != 1 || opts.files[0] != "report.pdf" {
		t.Fatalf("unexpected files: %#v", opts.files)
	}
	if !hasOption(opts.opts, "media=A4") || !hasOption(opts.opts, "sides=two-sided-long-edge") {
		t.Fatalf("missing parsed -o options: %#v", opts.opts)
	}
}

func TestParseArgsMailOptionAddsNotifyAndSilent(t *testing.T) {
	opts, err := parseArgs([]string{"-m"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.silent {
		t.Fatalf("expected silent mode for -m")
	}
	found := false
	for _, opt := range opts.opts {
		if strings.HasPrefix(opt, "notify-recipient-uri=mailto:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected notify-recipient-uri option, got %#v", opts.opts)
	}
}

func TestParseArgsRejectsInvalidCopies(t *testing.T) {
	_, err := parseArgs([]string{"-n0"})
	if err == nil {
		t.Fatalf("expected error for invalid copies")
	}
}

func TestParseArgsRejectsStdinWithFiles(t *testing.T) {
	_, err := parseArgs([]string{"file1.txt", "-"})
	if err == nil {
		t.Fatalf("expected error for stdin + files")
	}
}

func TestParseArgsRestartRequiresPriorJobID(t *testing.T) {
	_, err := parseArgs([]string{"-Hrestart", "-i42"})
	if err == nil {
		t.Fatalf("expected error when -H restart appears before -i")
	}
}

func TestParseArgsRestartWithJobIDAccepted(t *testing.T) {
	opts, err := parseArgs([]string{"-i42", "-Hrestart"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.jobID != "42" || !strings.EqualFold(opts.hold, "restart") {
		t.Fatalf("unexpected parsed restart args: %+v", opts)
	}
}

func TestParseArgsIgnoredOptionsEmitWarnings(t *testing.T) {
	opts, err := parseArgs([]string{"-fformA", "-yduplex", "-Sutf-8", "-Tapplication/pdf"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if len(opts.warnings) != 4 {
		t.Fatalf("expected 4 warnings, got %d (%#v)", len(opts.warnings), opts.warnings)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestSplitOptionWordsPreservesQuotedValues(t *testing.T) {
	got := splitOptionWords("media=A4 job-sheets='none none' print-color-mode=monochrome")
	if len(got) != 3 {
		t.Fatalf("unexpected tokens: %#v", got)
	}
	if got[1] != "job-sheets=none none" {
		t.Fatalf("expected quoted value to stay joined, got %#v", got)
	}
}

func hasOption(options []string, want string) bool {
	for _, opt := range options {
		if opt == want {
			return true
		}
	}
	return false
}
