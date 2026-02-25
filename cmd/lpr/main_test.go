package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsSupportsCoreFlags(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(tmp, []byte("hello"), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	opts, err := parseArgs([]string{"-EHlocalhost:8631", "-Ualice", "-POffice/inst", "-#2", "-o", "media=A4 sides=two-sided-long-edge", "-q", "-h", "-T", "My Job", tmp})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt {
		t.Fatal("expected encrypt=true")
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("server = %q, want localhost:8631", opts.server)
	}
	if opts.authUser != "alice" {
		t.Fatalf("authUser = %q, want alice", opts.authUser)
	}
	if opts.destination != "Office" {
		t.Fatalf("destination = %q, want Office", opts.destination)
	}
	if opts.jobOptions["copies"] != "2" {
		t.Fatalf("copies = %q, want 2", opts.jobOptions["copies"])
	}
	if opts.jobOptions["media"] != "A4" {
		t.Fatalf("media = %q, want A4", opts.jobOptions["media"])
	}
	if opts.jobOptions["job-hold-until"] != "indefinite" {
		t.Fatalf("job-hold-until = %q, want indefinite", opts.jobOptions["job-hold-until"])
	}
	if opts.jobOptions["job-sheets"] != "none" {
		t.Fatalf("job-sheets = %q, want none", opts.jobOptions["job-sheets"])
	}
	if opts.title != "My Job" {
		t.Fatalf("title = %q, want My Job", opts.title)
	}
	if len(opts.files) != 1 || opts.files[0] != tmp {
		t.Fatalf("files = %v, want [%s]", opts.files, tmp)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsRejectsUnknownOption(t *testing.T) {
	_, err := parseArgs([]string{"-Z"})
	if err == nil {
		t.Fatal("expected unknown option error")
	}
}

func TestParseArgsOptionAStyleWarnings(t *testing.T) {
	opts, err := parseArgs([]string{"-v"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if len(opts.warnings) == 0 {
		t.Fatal("expected warning for unsupported format modifier")
	}
	if !strings.Contains(opts.warnings[0], "not supported") {
		t.Fatalf("unexpected warning: %q", opts.warnings[0])
	}
}

func TestDocumentFormat(t *testing.T) {
	if got := documentFormat(map[string]string{"raw": "true"}, "doc.pdf"); got != "application/octet-stream" {
		t.Fatalf("raw format = %q", got)
	}
	if got := documentFormat(map[string]string{}, "doc.pdf"); got != "application/pdf" {
		t.Fatalf("pdf format = %q", got)
	}
	if got := documentFormat(map[string]string{}, "doc.unknown"); got != "application/octet-stream" {
		t.Fatalf("fallback format = %q", got)
	}
}

func TestParseDestinationSpec(t *testing.T) {
	destination, instance := parseDestinationSpec(" Office/inst ")
	if destination != "Office" || instance != "inst" {
		t.Fatalf("parseDestinationSpec with instance = (%q,%q), want (Office,inst)", destination, instance)
	}

	destination, instance = parseDestinationSpec("ipp://localhost/printers/Lab")
	if destination != "Lab" || instance != "" {
		t.Fatalf("parseDestinationSpec uri = (%q,%q), want (Lab,\"\")", destination, instance)
	}
}

func TestSplitOptionWordsHonorsQuotes(t *testing.T) {
	got := splitOptionWords(`media=A4 job-sheets='none none'`)
	want := []string{"media=A4", "job-sheets=none none"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitOptionWords = %#v, want %#v", got, want)
	}
}

func TestMergeDestinationOptionsPrefersCLIAndInstance(t *testing.T) {
	store := &lpOptionsFile{
		Dests: map[string]map[string]string{
			"office": {
				"copies": "2",
				"sides":  "two-sided-long-edge",
			},
			"office/draft": {
				"copies":   "3",
				"job-hold": "true",
			},
		},
	}
	explicit := map[string]string{
		"copies": "4",
		"media":  "A4",
	}
	got := mergeDestinationOptions(store, "Office", "draft", explicit)

	if got["copies"] != "4" {
		t.Fatalf("copies = %q, want 4", got["copies"])
	}
	if got["sides"] != "two-sided-long-edge" {
		t.Fatalf("sides = %q, want two-sided-long-edge", got["sides"])
	}
	if got["job-hold"] != "true" {
		t.Fatalf("job-hold = %q, want true", got["job-hold"])
	}
	if got["media"] != "A4" {
		t.Fatalf("media = %q, want A4", got["media"])
	}
}
