package main

import (
	"errors"
	"testing"
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
