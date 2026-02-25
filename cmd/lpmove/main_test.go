package main

import (
	"errors"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-hlocalhost:8631", "-E", "-Ualice", "Office-123", "Color"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
	if opts.server != "localhost:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user: %+v", opts)
	}
	if opts.jobID != 123 || opts.source != "" {
		t.Fatalf("expected job id source, got %+v", opts)
	}
	if opts.destination != "Color" {
		t.Fatalf("unexpected destination %q", opts.destination)
	}
}

func TestParseArgsSourceDestinationMode(t *testing.T) {
	opts, err := parseArgs([]string{"Office", "Color"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.jobID != 0 || opts.source != "Office" {
		t.Fatalf("expected source queue mode, got %+v", opts)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsAllowsHostOptionAfterOtherOptions(t *testing.T) {
	opts, err := parseArgs([]string{"-E", "-h", "localhost:8631", "Office", "Color"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("expected server localhost:8631, got %q", opts.server)
	}
}

func TestParseArgsAllowsHostOptionAfterOperand(t *testing.T) {
	opts, err := parseArgs([]string{"Office", "-h", "localhost:8631", "Color"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("expected server localhost:8631, got %q", opts.server)
	}
}

func TestBuildMoveRequestIncludesDestinationAttributes(t *testing.T) {
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer("localhost:8631"),
		cupsclient.WithUser("alice"),
	)
	req := buildMoveRequest(client, options{
		jobID:       42,
		destination: "Color",
		destKinds: map[string]destinationKind{
			"color": destinationPrinter,
		},
	})

	if got := findAttr(req.Operation, "job-uri"); got != "ipp://localhost/jobs/42" {
		t.Fatalf("expected localhost job-uri for id 42, got %q", got)
	}
	if got := findAttr(req.Job, "job-printer-uri"); got != "ipp://localhost/printers/Color" {
		t.Fatalf("expected localhost job-printer-uri for Color, got %q", got)
	}
	if got := findAttr(req.Operation, "printer-uri-destination"); got != "" {
		t.Fatalf("did not expect printer-uri-destination, got %q", got)
	}
	if got := findAttr(req.Operation, "requesting-user-name"); got != "alice" {
		t.Fatalf("expected requesting-user-name alice, got %q", got)
	}
}

func TestNormalizeMoveSourcePrefersKnownDestination(t *testing.T) {
	opts := options{
		sourceToken: "Office-123",
		jobID:       123,
		destination: "Color",
	}
	normalized := normalizeMoveSource(opts, map[string]destinationKind{"office-123": destinationPrinter})
	if normalized.jobID != 0 || normalized.source != "Office-123" {
		t.Fatalf("expected source queue mode, got %+v", normalized)
	}
}

func TestNormalizeMoveSourceKeepsJobIDForUnknownDestination(t *testing.T) {
	opts := options{
		sourceToken: "Office-123",
		jobID:       123,
		destination: "Color",
	}
	normalized := normalizeMoveSource(opts, map[string]destinationKind{"office": destinationPrinter})
	if normalized.jobID != 123 || normalized.source != "" {
		t.Fatalf("expected job id mode, got %+v", normalized)
	}
}

func TestDestinationURIUsesPrinterPathForKnownClass(t *testing.T) {
	client := cupsclient.NewFromConfig(cupsclient.WithServer("localhost:8631"))
	known := map[string]destinationKind{"team": destinationClass}
	if got := destinationURI(client, "Team", known); got != "ipp://localhost/printers/Team" {
		t.Fatalf("expected printer URI, got %q", got)
	}
}

func TestBuildMoveRequestUsesPrinterURIForSourceAndDestination(t *testing.T) {
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer("localhost:8631"),
		cupsclient.WithUser("alice"),
	)
	req := buildMoveRequest(client, options{
		source:      "Team",
		destination: "Team",
		destKinds: map[string]destinationKind{
			"team": destinationClass,
		},
	})

	if got := findAttr(req.Operation, "printer-uri"); got != "ipp://localhost/printers/Team" {
		t.Fatalf("expected printer source URI, got %q", got)
	}
	if got := findAttr(req.Job, "job-printer-uri"); got != "ipp://localhost/printers/Team" {
		t.Fatalf("expected printer destination URI, got %q", got)
	}
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name == name && len(attr.Values) > 0 {
			return attr.Values[0].V.String()
		}
	}
	return ""
}
