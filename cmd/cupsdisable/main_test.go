package main

import (
	"errors"
	"testing"
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
