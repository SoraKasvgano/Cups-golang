package main

import (
	"errors"
	"testing"
)

func TestParseArgsSupportsShortClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-hlocalhost:8631", "-Ualice", "-vEl", "-m"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.showDevices || !opts.showModels {
		t.Fatalf("expected both showDevices/showModels, got %+v", opts)
	}
	if !opts.encrypt || !opts.longListing {
		t.Fatalf("expected encrypt and longListing, got %+v", opts)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user %+v", opts)
	}
}

func TestParseArgsSplitsSchemeListsAndSupportsEquals(t *testing.T) {
	opts, err := parseArgs([]string{
		"--include-schemes=ipp,ipps socket",
		"--exclude-schemes", "dnssd;usb",
		"--include-schemes", "ipp",
	})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if len(opts.includeSchemes) != 3 {
		t.Fatalf("expected 3 include schemes, got %v", opts.includeSchemes)
	}
	if len(opts.excludeSchemes) != 2 {
		t.Fatalf("expected 2 exclude schemes, got %v", opts.excludeSchemes)
	}
}

func TestParseArgsRejectsLateHostOption(t *testing.T) {
	_, err := parseArgs([]string{"-v", "-h", "localhost:631"})
	if err == nil {
		t.Fatalf("expected error for late -h")
	}
}

func TestParseArgsHelpReturnsSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}
