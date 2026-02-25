package main

import (
	"errors"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClusterAndLongHold(t *testing.T) {
	opts, err := parseArgs([]string{"-Echlocalhost:8631", "-Ualice", "-rPaused", "--hold", "Printer1"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt || !opts.cancel || !opts.hold {
		t.Fatalf("expected encrypt/cancel/hold true, got %+v", opts)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" || opts.reason != "Paused" {
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
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOkConflicting, 1)); err != nil {
		t.Fatalf("expected nil status error, got %v", err)
	}
	if err := checkIPPStatus(goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorServiceUnavailable, 1)); err == nil {
		t.Fatal("expected status error for service unavailable")
	}
}

func TestAddRequestingUserNameUsesClientUser(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPausePrinter, 1)
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
