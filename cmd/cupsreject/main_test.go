package main

import (
	"errors"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsAttachedReason(t *testing.T) {
	opts, err := parseArgs([]string{"-Ehlocalhost:8631", "-Ualice", "-rbusy", "Printer1"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
	if opts.server != "localhost:8631" || opts.user != "alice" || opts.reason != "busy" {
		t.Fatalf("unexpected values: %+v", opts)
	}
	if len(opts.printers) != 1 || opts.printers[0] != "Printer1" {
		t.Fatalf("unexpected printers: %v", opts.printers)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestCheckIPPStatus(t *testing.T) {
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOkIgnoredOrSubstituted, 1)); err != nil {
		t.Fatalf("expected nil status error, got %v", err)
	}
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorNotPossible, 1)); err == nil {
		t.Fatal("expected status error for non-success response")
	}
}

func TestAddRequestingUserNameUsesClientUser(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsRejectJobs, 1)
	client := cupsclient.NewFromConfig(cupsclient.WithUser("alice"))
	addRequestingUserName(&req.Operation, client)

	if got := findAttrValue(req.Operation, "requesting-user-name"); got != "alice" {
		t.Fatalf("requesting-user-name = %q, want %q", got, "alice")
	}
}

func findAttrValue(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		return attr.Values[0].V.String()
	}
	return ""
}
