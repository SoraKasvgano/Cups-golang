package main

import (
	"errors"
	"testing"
)

func TestParseArgsSupportsShortAndLongOptions(t *testing.T) {
	opts, err := parseArgs([]string{
		"-hlocalhost:8631",
		"-Ualice",
		"-E",
		"--no-share-printers",
		"debug_logging=1",
		"custom_key=value",
	})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" || !opts.encrypt {
		t.Fatalf("unexpected option values: %+v", opts)
	}
	if got := opts.updates["_share_printers"]; got != "0" {
		t.Fatalf("expected _share_printers=0, got %q", got)
	}
	if got := opts.updates["_debug_logging"]; got != "1" {
		t.Fatalf("expected _debug_logging=1, got %q", got)
	}
	if got := opts.updates["custom_key"]; got != "value" {
		t.Fatalf("expected custom_key=value, got %q", got)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsRejectsUnknownInputs(t *testing.T) {
	if _, err := parseArgs([]string{"--unknown-option"}); err == nil {
		t.Fatalf("expected unknown long option error")
	}
	if _, err := parseArgs([]string{"bareword"}); err == nil {
		t.Fatalf("expected unknown argument error")
	}
}

func TestBlockedDirective(t *testing.T) {
	if blocked, ok := blockedDirective("Port"); !ok || blocked != "Port" {
		t.Fatalf("expected Port to be blocked, got ok=%v blocked=%q", ok, blocked)
	}
	if blocked, ok := blockedDirective("serverroot"); !ok || blocked != "ServerRoot" {
		t.Fatalf("expected ServerRoot case-insensitive block, got ok=%v blocked=%q", ok, blocked)
	}
	if _, ok := blockedDirective("_share_printers"); ok {
		t.Fatalf("did not expect internal toggle key to be blocked")
	}
}
