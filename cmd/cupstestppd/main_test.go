package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgsCupstestFlags(t *testing.T) {
	opts, err := parseArgs([]string{"-W", "defaults", "-Wfilters", "-Ifilename", "-R", "/tmp/root", "-vv", "a.ppd"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.warn != (warnDefaults | warnFilters) {
		t.Fatalf("warn = %d", opts.warn)
	}
	if opts.ignore != warnFilename {
		t.Fatalf("ignore = %d", opts.ignore)
	}
	if opts.rootDir != "/tmp/root" {
		t.Fatalf("rootDir = %q", opts.rootDir)
	}
	if opts.verbose != 2 {
		t.Fatalf("verbose = %d", opts.verbose)
	}
	if len(opts.files) != 1 || opts.files[0] != "a.ppd" {
		t.Fatalf("unexpected files: %#v", opts.files)
	}
}

func TestParseArgsHelp(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestParseArgsRejectsQuietVerboseMix(t *testing.T) {
	_, err := parseArgs([]string{"-q", "-v", "x.ppd"})
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("expected incompatible error, got %v", err)
	}
}

func TestRunMissingFilenameShowsUsage(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer

	code := run(nil, strings.NewReader(""), &out, &errOut)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(out.String(), "Usage: cupstestppd") {
		t.Fatalf("expected usage output, got %q", out.String())
	}
}

func TestRunMissingFileReturnsFileOpen(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer

	code := run([]string{"not-found.ppd"}, strings.NewReader(""), &out, &errOut)
	if code != exitFileOpen {
		t.Fatalf("code = %d, want %d", code, exitFileOpen)
	}
	if !strings.Contains(out.String(), "Unable to open PPD file") {
		t.Fatalf("missing error message in output: %q", out.String())
	}
}

func TestRunInvalidFormatReturnsPPDFormat(t *testing.T) {
	path := writeTempFile(t, "not a ppd")
	defer os.Remove(path)

	var out bytes.Buffer
	var errOut bytes.Buffer
	code := run([]string{path}, strings.NewReader(""), &out, &errOut)
	if code != exitPPDFormat {
		t.Fatalf("code = %d, want %d", code, exitPPDFormat)
	}
	if !strings.Contains(out.String(), "Missing required *PPD-Adobe header") {
		t.Fatalf("expected format error, got %q", out.String())
	}
}

func TestRunConformanceFailureAndWarningOverride(t *testing.T) {
	ppd := `*PPD-Adobe: "4.3"
*FormatVersion: "4.3"
*NickName: "Demo"
*ModelName: "Demo"
*OpenUI *PageSize/Page Size: PickOne
*PageSize A4/A4: "<</PageSize[595 842]>>setpagedevice"
*CloseUI: *PageSize
`
	path := writeTempFile(t, ppd)
	defer os.Remove(path)

	var out1 bytes.Buffer
	var err1 bytes.Buffer
	code := run([]string{path}, strings.NewReader(""), &out1, &err1)
	if code != exitConformance {
		t.Fatalf("code = %d, want %d", code, exitConformance)
	}
	if !strings.Contains(out1.String(), "DefaultPageSize") {
		t.Fatalf("expected default failure, got %q", out1.String())
	}

	var out2 bytes.Buffer
	var err2 bytes.Buffer
	code = run([]string{"-W", "defaults", path}, strings.NewReader(""), &out2, &err2)
	if code != exitOK {
		t.Fatalf("code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(out2.String(), "WARN") {
		t.Fatalf("expected warning output, got %q", out2.String())
	}
}

func TestRunIgnoreFilters(t *testing.T) {
	missingFilter := filepath.ToSlash(filepath.Join(t.TempDir(), "missing-filter-bin"))
	ppd := `*PPD-Adobe: "4.3"
*FormatVersion: "4.3"
*NickName: "Demo"
*ModelName: "Demo"
*OpenUI *PageSize/Page Size: PickOne
*DefaultPageSize: A4
*PageSize A4/A4: "<</PageSize[595 842]>>setpagedevice"
*CloseUI: *PageSize
*cupsFilter: "application/vnd.cups-postscript 0 ` + missingFilter + `"
`
	path := writeTempFile(t, ppd)
	defer os.Remove(path)

	var out1 bytes.Buffer
	var err1 bytes.Buffer
	code := run([]string{path}, strings.NewReader(""), &out1, &err1)
	if code != exitConformance {
		t.Fatalf("code = %d, want %d", code, exitConformance)
	}
	if !strings.Contains(out1.String(), "cupsFilter") {
		t.Fatalf("expected filter failure, got %q", out1.String())
	}

	var out2 bytes.Buffer
	var err2 bytes.Buffer
	code = run([]string{"-I", "filters", path}, strings.NewReader(""), &out2, &err2)
	if code != exitOK {
		t.Fatalf("code = %d, want %d", code, exitOK)
	}
}

func TestRunReadsFromStdin(t *testing.T) {
	ppd := `*PPD-Adobe: "4.3"
*FormatVersion: "4.3"
*NickName: "Demo"
*ModelName: "Demo"
*OpenUI *PageSize/Page Size: PickOne
*DefaultPageSize: A4
*PageSize A4/A4: "<</PageSize[595 842]>>setpagedevice"
*CloseUI: *PageSize
`

	var out bytes.Buffer
	var errOut bytes.Buffer
	code := run([]string{"-"}, strings.NewReader(ppd), &out, &errOut)
	if code != exitOK {
		t.Fatalf("code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(out.String(), "(stdin): PASS") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestRunQuietModeSuppressesOutput(t *testing.T) {
	ppd := `*PPD-Adobe: "4.3"
*FormatVersion: "4.3"
*NickName: "Demo"
*ModelName: "Demo"
*OpenUI *PageSize/Page Size: PickOne
*PageSize A4/A4: "<</PageSize[595 842]>>setpagedevice"
*CloseUI: *PageSize
`
	path := writeTempFile(t, ppd)
	defer os.Remove(path)

	var out bytes.Buffer
	var errOut bytes.Buffer
	code := run([]string{"-q", path}, strings.NewReader(""), &out, &errOut)
	if code != exitConformance {
		t.Fatalf("code = %d, want %d", code, exitConformance)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("expected no output in quiet mode, got stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "cupstestppd-*.ppd")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.Name()
}
