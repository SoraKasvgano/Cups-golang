package main

import "testing"

func TestParseArgsSupportsShortClustersAndAttachedValues(t *testing.T) {
	opts := parseArgs([]string{"-rD", "-pOffice", "-ualice,bob", "-Wcompleted", "-E"})
	if !opts.showStatus {
		t.Fatalf("expected showStatus")
	}
	if opts.longStatus != 1 {
		t.Fatalf("expected longStatus=1, got %d", opts.longStatus)
	}
	if !opts.showPrinters {
		t.Fatalf("expected showPrinters")
	}
	if len(opts.printerFilter) != 1 || opts.printerFilter[0] != "Office" {
		t.Fatalf("unexpected printer filter: %v", opts.printerFilter)
	}
	if len(opts.userFilter) != 2 || opts.userFilter[0] != "alice" || opts.userFilter[1] != "bob" {
		t.Fatalf("unexpected user filter: %v", opts.userFilter)
	}
	if opts.whichJobs != "completed" {
		t.Fatalf("expected whichJobs completed, got %q", opts.whichJobs)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
}

func TestParseArgsSupportsPaperAndCharsetsFlags(t *testing.T) {
	opts := parseArgs([]string{"-POffice,Lab", "-SOffice"})
	if !opts.showPaper {
		t.Fatalf("expected showPaper")
	}
	if !opts.showCharsets {
		t.Fatalf("expected showCharsets")
	}
	if len(opts.printerFilter) != 3 {
		t.Fatalf("unexpected printer filters: %v", opts.printerFilter)
	}
}

func TestParseArgsPositionalDestinationsBecomeJobFilters(t *testing.T) {
	opts := parseArgs([]string{"Office", "Lab"})
	if !opts.showJobs {
		t.Fatalf("expected showJobs for positional destinations")
	}
	if len(opts.printerFilter) != 2 || opts.printerFilter[0] != "Office" || opts.printerFilter[1] != "Lab" {
		t.Fatalf("unexpected positional printer filters: %v", opts.printerFilter)
	}
}

func TestParseArgsSupportsFormsCompatibilityFlag(t *testing.T) {
	opts := parseArgs([]string{"-f", "ignored-form"})
	if !opts.showForms {
		t.Fatalf("expected showForms")
	}
	if opts.showJobs {
		t.Fatalf("did not expect showJobs when only -f is used")
	}
}

func TestDestinationTypeResolution(t *testing.T) {
	if got := destinationType(printerInfo{temporary: true, uri: "ipp://example/printers/p"}); got != "temporary" {
		t.Fatalf("expected temporary, got %q", got)
	}
	if got := destinationType(printerInfo{uri: "ipp://example/printers/p"}); got != "permanent" {
		t.Fatalf("expected permanent, got %q", got)
	}
	if got := destinationType(printerInfo{}); got != "network" {
		t.Fatalf("expected network, got %q", got)
	}
}
