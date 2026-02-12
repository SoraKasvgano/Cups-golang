package main

import (
	"errors"
	"testing"
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
