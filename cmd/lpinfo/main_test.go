package main

import (
	"errors"
	"testing"
)

func TestParseArgsSupportsShortClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-hlocalhost:8631", "-vEl", "-m"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.showDevices || !opts.showModels {
		t.Fatalf("expected both showDevices/showModels, got %+v", opts)
	}
	if !opts.encrypt || !opts.longListing {
		t.Fatalf("expected encrypt and longListing, got %+v", opts)
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("unexpected server %+v", opts)
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

func TestParseArgsAcceptsHostAfterOtherOptions(t *testing.T) {
	opts, err := parseArgs([]string{"-v", "-h", "localhost:631"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:631" {
		t.Fatalf("expected late -h to be accepted, got server=%q", opts.server)
	}
}

func TestParseArgsRejectsUnsupportedUserFlag(t *testing.T) {
	_, err := parseArgs([]string{"-U", "alice"})
	if err == nil {
		t.Fatalf("expected error for unsupported -U option")
	}
}

func TestParseArgsHelpReturnsSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestFormatDeviceOutputMatchesCUPSStyle(t *testing.T) {
	short := formatDeviceShort("network", "ipp://printer.local/ipp/print")
	if short != "network ipp://printer.local/ipp/print\n" {
		t.Fatalf("unexpected short output %q", short)
	}

	long := formatDeviceLong("network", "ipp://printer.local/ipp/print", "Office Printer", "IPP Everywhere", "MFG:Test;MDL:One;", "Floor1")
	expected := "Device: uri = ipp://printer.local/ipp/print\n        class = network\n        info = Office Printer\n        make-and-model = IPP Everywhere\n        device-id = MFG:Test;MDL:One;\n        location = Floor1\n"
	if long != expected {
		t.Fatalf("unexpected long output:\n%s", long)
	}
}

func TestEverywhereSchemeFilter(t *testing.T) {
	if !shouldAppendEverywhere(nil, nil) {
		t.Fatalf("expected everywhere when no filters")
	}
	if shouldAppendEverywhere([]string{"ipp", "ipps"}, nil) {
		t.Fatalf("did not expect everywhere when include list excludes it")
	}
	if shouldAppendEverywhere([]string{"everywhere"}, []string{"everywhere"}) {
		t.Fatalf("did not expect everywhere when excluded")
	}
}
