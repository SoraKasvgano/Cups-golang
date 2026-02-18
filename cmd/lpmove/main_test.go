package main

import (
	"errors"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-E", "-hlocalhost:8631", "-Ualice", "Office-123", "Color"})
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

func TestBuildMoveRequestIncludesDestinationAttributes(t *testing.T) {
	client := cupsclient.NewFromConfig(
		cupsclient.WithServer("localhost:8631"),
		cupsclient.WithUser("alice"),
	)
	req := buildMoveRequest(client, options{
		jobID:       42,
		destination: "Color",
	})

	if got := findAttr(req.Operation, "job-uri"); !strings.Contains(got, "/jobs/42") {
		t.Fatalf("expected job-uri for id 42, got %q", got)
	}
	if got := findAttr(req.Job, "job-printer-uri"); !strings.Contains(got, "/printers/Color") {
		t.Fatalf("expected job-printer-uri for Color, got %q", got)
	}
	if got := findAttr(req.Operation, "printer-uri-destination"); got != "" {
		t.Fatalf("did not expect printer-uri-destination, got %q", got)
	}
	if got := findAttr(req.Operation, "requesting-user-name"); got != "alice" {
		t.Fatalf("expected requesting-user-name alice, got %q", got)
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
