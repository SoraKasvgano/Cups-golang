package main

import (
	"errors"
	"testing"
)

func TestParseArgsSupportsClustersAndDestination(t *testing.T) {
	opts, err := parseArgs([]string{"-Ehlocalhost:8631", "-Ualice", "-POffice/inst", "123"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("server = %q, want localhost:8631", opts.server)
	}
	if opts.user != "alice" {
		t.Fatalf("user = %q, want alice", opts.user)
	}
	if opts.destination != "Office" {
		t.Fatalf("destination = %q, want Office", opts.destination)
	}
	if len(opts.targets) != 1 || opts.targets[0] != "123" {
		t.Fatalf("unexpected targets: %v", opts.targets)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsRejectsUnknownOption(t *testing.T) {
	if _, err := parseArgs([]string{"-Z"}); err == nil {
		t.Fatal("expected unknown option error")
	}
}

func TestSanitizeDestination(t *testing.T) {
	if got := sanitizeDestination(" Office/inst "); got != "Office" {
		t.Fatalf("sanitizeDestination = %q, want Office", got)
	}
	if got := sanitizeDestination("ipp://localhost/printers/Lab"); got != "Lab" {
		t.Fatalf("sanitizeDestination uri = %q, want Lab", got)
	}
}

func TestIsPositiveInt(t *testing.T) {
	if !isPositiveInt("42") {
		t.Fatal("expected 42 to be positive int")
	}
	if isPositiveInt("0") {
		t.Fatal("expected 0 to be false")
	}
	if isPositiveInt("abc") {
		t.Fatal("expected abc to be false")
	}
}

func TestParseDestinationSpec(t *testing.T) {
	dest, instance := parseDestinationSpec("Office/draft")
	if dest != "Office" || instance != "draft" {
		t.Fatalf("parseDestinationSpec = (%q,%q), want (Office,draft)", dest, instance)
	}
}
