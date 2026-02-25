package main

import (
	"errors"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClusterAndRelease(t *testing.T) {
	opts, err := parseArgs([]string{"-Ehlocalhost:8631", "-Ualice", "-c", "-rReady", "--release", "Printer1"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt || !opts.cancel || !opts.release {
		t.Fatalf("expected encrypt/cancel/release true, got %+v", opts)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" || opts.reason != "Ready" {
		t.Fatalf("unexpected server/user/reason: %+v", opts)
	}
	if len(opts.dests) != 1 || opts.dests[0] != "Printer1" {
		t.Fatalf("unexpected destinations: %v", opts.dests)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestCheckIPPStatus(t *testing.T) {
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)); err != nil {
		t.Fatalf("expected nil status error, got %v", err)
	}
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorForbidden, 1)); err == nil {
		t.Fatal("expected status error for forbidden response")
	}
}

func TestAddRequestingUserNameUsesClientUser(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpResumePrinter, 1)
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
