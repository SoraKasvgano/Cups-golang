package main

import (
	"errors"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClustersAndRemovals(t *testing.T) {
	opts, err := parseArgs([]string{"-E", "-hlocalhost:8631", "-Ualice", "-pOffice", "-E", "-vipp://printer.local/ipp/print", "-o", "copies=2", "-R", "media", "-Rjob-sheets-default"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.server != "localhost:8631" || opts.user != "alice" {
		t.Fatalf("unexpected server/user: %+v", opts)
	}
	if opts.printer != "Office" || opts.deviceURI == "" {
		t.Fatalf("unexpected printer/device: %+v", opts)
	}
	if !opts.enable {
		t.Fatalf("expected enable=true after -E with printer selected")
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true from -Ec cluster")
	}
	if got := opts.extraOpts["copies"]; got != "2" {
		t.Fatalf("expected copies=2, got %q", got)
	}
	if len(opts.removeOpts) != 2 {
		t.Fatalf("expected two remove options, got %v", opts.removeOpts)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsSupportsUserAccessList(t *testing.T) {
	opts, err := parseArgs([]string{"-p", "Office", "-u", "allow:alice,bob"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if got := opts.extraOpts["requesting-user-name-allowed"]; got != "alice,bob" {
		t.Fatalf("expected allow list, got %q", got)
	}
	if _, ok := opts.extraOpts["requesting-user-name-denied"]; ok {
		t.Fatalf("expected denied list to be cleared, got %+v", opts.extraOpts)
	}
}

func TestParseArgsRejectsInvalidUserAccessPolicy(t *testing.T) {
	if _, err := parseArgs([]string{"-p", "Office", "-u", "maybe:alice"}); err == nil {
		t.Fatal("expected parse error for invalid -u policy")
	}
}

func TestParseArgsCompatibilityOptionIAddsWarning(t *testing.T) {
	opts, err := parseArgs([]string{"-p", "Office", "-I", "application/pdf"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if len(opts.warnings) == 0 {
		t.Fatalf("expected compatibility warning, got none")
	}
	if !strings.Contains(opts.warnings[0], "content type list ignored") {
		t.Fatalf("unexpected warning text: %q", opts.warnings[0])
	}
}

func TestParseArgsOptionARejected(t *testing.T) {
	_, err := parseArgs([]string{"-A", "script.sh"})
	if err == nil {
		t.Fatal("expected parse error for -A")
	}
	if !strings.Contains(err.Error(), "System V interface scripts are no longer supported") {
		t.Fatalf("unexpected -A error: %v", err)
	}
}

func TestParseArgsRejectsInvalidPrinterName(t *testing.T) {
	_, err := parseArgs([]string{"-p", "Bad/Name"})
	if err == nil {
		t.Fatal("expected invalid printer name error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "printable characters") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateName(t *testing.T) {
	if !validateName("Office") {
		t.Fatal("expected Office to be valid")
	}
	if !validateName("Office@remote") {
		t.Fatal("expected Office@remote to be valid")
	}
	if validateName("Bad/Name") {
		t.Fatal("expected Bad/Name to be invalid")
	}
	if validateName("Bad#Name") {
		t.Fatal("expected Bad#Name to be invalid")
	}
}

func TestNormalizeLpadminRemoveOption(t *testing.T) {
	if got := normalizeLpadminRemoveOption("media"); got != "media-default" {
		t.Fatalf("expected media-default, got %q", got)
	}
	if got := normalizeLpadminRemoveOption("job-sheets"); got != "job-sheets-default" {
		t.Fatalf("expected job-sheets-default, got %q", got)
	}
	if got := normalizeLpadminRemoveOption("printer-op-policy"); got != "printer-op-policy" {
		t.Fatalf("unexpected normalization: %q", got)
	}
}

func TestNormalizeLpadminOptionUserAccessUsesNameTag(t *testing.T) {
	attr, tag, values := normalizeLpadminOption("requesting-user-name-allowed", "alice,bob")
	if attr != "requesting-user-name-allowed" {
		t.Fatalf("unexpected attr name %q", attr)
	}
	if tag != goipp.TagName {
		t.Fatalf("expected TagName, got %v", tag)
	}
	if len(values) != 2 || values[0].String() != "alice" || values[1].String() != "bob" {
		t.Fatalf("unexpected values: %#v", values)
	}
}

func TestApplyLpadminRemovalsUsesDeleteAttributeTag(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, 1)
	applyLpadminRemovals(req, []string{"media", "media-default", "job-sheets"})
	if len(req.Printer) != 2 {
		t.Fatalf("expected deduped removal attrs, got %d", len(req.Printer))
	}
	for _, attr := range req.Printer {
		if len(attr.Values) == 0 || attr.Values[0].T != goipp.TagDeleteAttr {
			t.Fatalf("expected delete-attribute tag, got %#v", attr)
		}
	}
}

func TestDestinationNameFromURI(t *testing.T) {
	if got := destinationNameFromURI("ipp://localhost/printers/Office"); got != "Office" {
		t.Fatalf("expected Office, got %q", got)
	}
	if got := destinationNameFromURI("/classes/Color"); got != "Color" {
		t.Fatalf("expected Color, got %q", got)
	}
	if got := destinationNameFromURI("ipp://localhost/printers/Office%20Laser"); got != "Office Laser" {
		t.Fatalf("expected unescaped name, got %q", got)
	}
}

func TestComputeClassMembersUpdate(t *testing.T) {
	tests := []struct {
		name       string
		found      bool
		existing   []string
		printer    string
		add        bool
		remove     bool
		className  string
		wantAction classMemberAction
		wantList   []string
		wantErr    bool
	}{
		{
			name:       "add creates class",
			found:      false,
			existing:   nil,
			printer:    "Office",
			add:        true,
			remove:     false,
			className:  "Team",
			wantAction: classMemberSet,
			wantList:   []string{"Office"},
		},
		{
			name:      "remove missing class errors",
			found:     false,
			existing:  nil,
			printer:   "Office",
			add:       false,
			remove:    true,
			className: "Team",
			wantErr:   true,
		},
		{
			name:      "remove non-member errors",
			found:     true,
			existing:  []string{"Color"},
			printer:   "Office",
			add:       false,
			remove:    true,
			className: "Team",
			wantErr:   true,
		},
		{
			name:       "remove sole member deletes class",
			found:      true,
			existing:   []string{"Office"},
			printer:    "Office",
			add:        false,
			remove:     true,
			className:  "Team",
			wantAction: classMemberDelete,
			wantList:   nil,
		},
		{
			name:       "add existing member is noop",
			found:      true,
			existing:   []string{"Office"},
			printer:    "office",
			add:        true,
			remove:     false,
			className:  "Team",
			wantAction: classMemberNoop,
			wantList:   []string{"Office"},
		},
		{
			name:       "remove from multi-member keeps order",
			found:      true,
			existing:   []string{"One", "Two", "Three"},
			printer:    "Two",
			add:        false,
			remove:     true,
			className:  "Team",
			wantAction: classMemberSet,
			wantList:   []string{"One", "Three"},
		},
		{
			name:       "add appends new member",
			found:      true,
			existing:   []string{"One", "Two"},
			printer:    "Three",
			add:        true,
			remove:     false,
			className:  "Team",
			wantAction: classMemberSet,
			wantList:   []string{"One", "Two", "Three"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, list, err := computeClassMembersUpdate(tc.found, tc.existing, tc.printer, tc.add, tc.remove, tc.className)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("computeClassMembersUpdate error: %v", err)
			}
			if action != tc.wantAction {
				t.Fatalf("action = %q, want %q", action, tc.wantAction)
			}
			if len(list) != len(tc.wantList) {
				t.Fatalf("list len = %d, want %d (%v)", len(list), len(tc.wantList), list)
			}
			for i := range list {
				if list[i] != tc.wantList[i] {
					t.Fatalf("list[%d] = %q, want %q", i, list[i], tc.wantList[i])
				}
			}
		})
	}
}

func TestNormalizeDestinationListDedupsCaseInsensitive(t *testing.T) {
	got := normalizeDestinationList([]string{" Office ", "office", "Color", "", "color"})
	if len(got) != 2 || got[0] != "Office" || got[1] != "Color" {
		t.Fatalf("unexpected normalized list: %v", got)
	}
}

func TestClassMemberURIsForNamesPreservesExistingURIs(t *testing.T) {
	client := cupsclient.NewFromConfig(cupsclient.WithServer("localhost:8631"))
	updated := []string{"Office", "Remote"}
	existingNames := []string{"Office", "Remote"}
	existingURIs := []string{"ipp://localhost/printers/Office", "ipp://192.168.1.99/printers/Remote"}

	got := classMemberURIsForNames(updated, existingNames, existingURIs, client)
	if len(got) != 2 {
		t.Fatalf("expected 2 member URIs, got %v", got)
	}
	if got[0] != "ipp://localhost/printers/Office" {
		t.Fatalf("expected preserved local URI, got %q", got[0])
	}
	if got[1] != "ipp://192.168.1.99/printers/Remote" {
		t.Fatalf("expected preserved remote URI, got %q", got[1])
	}
}

func TestClassMemberURIsForNamesUsesLocalURIForNewMembers(t *testing.T) {
	client := cupsclient.NewFromConfig(cupsclient.WithServer("localhost:8631"))
	updated := []string{"Office", "New Queue"}
	existingNames := []string{"Office"}
	existingURIs := []string{"ipp://localhost/printers/Office"}

	got := classMemberURIsForNames(updated, existingNames, existingURIs, client)
	if len(got) != 2 {
		t.Fatalf("expected 2 member URIs, got %v", got)
	}
	if got[1] != "ipp://localhost/printers/New%20Queue" {
		t.Fatalf("expected local URI for new member, got %q", got[1])
	}
}

func TestNormalizeClassMembersAlignsAndDedups(t *testing.T) {
	names, uris := normalizeClassMembers(
		[]string{" Office ", "office", "Color"},
		[]string{"ipp://localhost/printers/Office", "ipp://localhost/printers/office", "ipp://localhost/printers/Color"},
	)
	if len(names) != 2 || len(uris) != 2 {
		t.Fatalf("unexpected normalized members names=%v uris=%v", names, uris)
	}
	if names[0] != "Office" || names[1] != "Color" {
		t.Fatalf("unexpected normalized names: %v", names)
	}
	if uris[0] != "ipp://localhost/printers/Office" || uris[1] != "ipp://localhost/printers/Color" {
		t.Fatalf("unexpected normalized uris: %v", uris)
	}
}
