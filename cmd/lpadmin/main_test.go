package main

import (
	"errors"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestParseArgsSupportsClustersAndRemovals(t *testing.T) {
	opts, err := parseArgs([]string{"-E", "-hlocalhost:8631", "-Ualice", "-pOffice", "-E", "-vipp://printer.local/ipp/print", "-o", "copies=2", "-R", "media", "-Rjob-sheets-default"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user: %+v", opts)
	}
	if opts.printer != "Office" || opts.deviceURI == "" {
		t.Fatalf("unexpected printer/device: %+v", opts)
	}
	if !opts.enable {
		t.Fatalf("expected enable=true after -E with printer selected")
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true from -Ec cluster")
	}
	if got := opts.extraOpts["copies"]; got != "2" {
		t.Fatalf("expected copies=2, got %q", got)
	}
	if len(opts.removeOpts) != 2 {
		t.Fatalf("expected two remove options, got %v", opts.removeOpts)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestNormalizeLpadminRemoveOption(t *testing.T) {
	if got := normalizeLpadminRemoveOption("media"); got != "media-default" {
		t.Fatalf("expected media-default, got %q", got)
	}
	if got := normalizeLpadminRemoveOption("job-sheets"); got != "job-sheets-default" {
		t.Fatalf("expected job-sheets-default, got %q", got)
	}
	if got := normalizeLpadminRemoveOption("printer-op-policy"); got != "printer-op-policy" {
		t.Fatalf("unexpected normalization: %q", got)
	}
}

func TestApplyLpadminRemovalsUsesDeleteAttributeTag(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, 1)
	applyLpadminRemovals(req, []string{"media", "media-default", "job-sheets"})
	if len(req.Printer) != 2 {
		t.Fatalf("expected deduped removal attrs, got %d", len(req.Printer))
	}
	for _, attr := range req.Printer {
		if len(attr.Values) == 0 || attr.Values[0].T != goipp.TagDeleteAttr {
			t.Fatalf("expected delete-attribute tag, got %#v", attr)
		}
	}
}

func TestDestinationNameFromURI(t *testing.T) {
	if got := destinationNameFromURI("ipp://localhost/printers/Office"); got != "Office" {
		t.Fatalf("expected Office, got %q", got)
	}
	if got := destinationNameFromURI("/classes/Color"); got != "Color" {
		t.Fatalf("expected Color, got %q", got)
	}
}
