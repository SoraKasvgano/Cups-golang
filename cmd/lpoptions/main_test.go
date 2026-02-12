package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseArgsSupportsClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-hlocalhost:8631", "-Ualice", "-pOffice", "-o", "copies=2", "-r", "media", "-E"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user: %+v", opts)
	}
	if opts.printer != "Office" || len(opts.setOps) != 1 || len(opts.rmOps) != 1 {
		t.Fatalf("unexpected parsed options: %+v", opts)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
}

func TestParseArgsRequiresDestinationForX(t *testing.T) {
	_, err := parseArgs([]string{"-x"})
	if err == nil {
		t.Fatalf("expected error for missing -x value")
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestSplitOptionWordsSupportsQuotedPairs(t *testing.T) {
	words := splitOptionWords("media=A4 job-sheets='none none' print-color-mode=monochrome")
	expected := []string{"media=A4", "job-sheets=none none", "print-color-mode=monochrome"}
	if !reflect.DeepEqual(words, expected) {
		t.Fatalf("unexpected words: %#v", words)
	}
}

func TestApplyOptionEditsParsesMultiOptionString(t *testing.T) {
	store := &lpOptionsFile{Dests: map[string]map[string]string{}}
	applyOptionEdits(store, "Office", []string{"media=A4 sides=two-sided-long-edge job-sheets='none none'"}, nil)
	opts := store.Dests["Office"]
	if opts["media"] != "A4" || opts["sides"] != "two-sided-long-edge" || opts["job-sheets"] != "none none" {
		t.Fatalf("unexpected options: %#v", opts)
	}
}

func TestRemoveDestinationRemovesInstancesForBaseName(t *testing.T) {
	store := &lpOptionsFile{Dests: map[string]map[string]string{
		"Office":         {},
		"Office/draft":   {},
		"Office/highres": {},
		"Lab":            {},
	}, Default: "Office/highres"}
	removeDestination(store, "Office")
	if _, ok := store.Dests["Office"]; ok {
		t.Fatalf("expected Office removed")
	}
	if _, ok := store.Dests["Office/draft"]; ok {
		t.Fatalf("expected Office/draft removed")
	}
	if _, ok := store.Dests["Office/highres"]; ok {
		t.Fatalf("expected Office/highres removed")
	}
	if store.Default != "" {
		t.Fatalf("expected default cleared, got %q", store.Default)
	}
	if _, ok := store.Dests["Lab"]; !ok {
		t.Fatalf("expected Lab to remain")
	}
}

func TestFormatOptionTokensQuotesValuesWithSpaces(t *testing.T) {
	tokens := formatOptionTokens(map[string]string{
		"job-sheets": "none none",
		"media":      "A4",
	})
	expected := []string{"job-sheets='none none'", "media=A4"}
	if !reflect.DeepEqual(tokens, expected) {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
}
