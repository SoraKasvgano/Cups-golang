package main

import (
	"errors"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestParseArgsSupportsClusterAndInterval(t *testing.T) {
	opts, err := parseArgs([]string{"+5", "-Ehlocalhost:8631", "-Ualice", "-POffice/inst", "-l", "bob", "123"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if opts.interval != 5 {
		t.Fatalf("interval = %d, want 5", opts.interval)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
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
	if !opts.longStatus {
		t.Fatal("expected longStatus=true")
	}
	if opts.userFilter != "bob" {
		t.Fatalf("userFilter = %q, want bob", opts.userFilter)
	}
	if opts.jobID != 123 {
		t.Fatalf("jobID = %d, want 123", opts.jobID)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestRankString(t *testing.T) {
	if got := rankString(4, 1); got != "active" {
		t.Fatalf("rankString processing = %q, want active", got)
	}
	if got := rankString(3, 1); got != "1st" {
		t.Fatalf("rankString(1) = %q, want 1st", got)
	}
	if got := rankString(3, 2); got != "2nd" {
		t.Fatalf("rankString(2) = %q, want 2nd", got)
	}
	if got := rankString(3, 11); got != "11th" {
		t.Fatalf("rankString(11) = %q, want 11th", got)
	}
}

func TestSanitizeDestination(t *testing.T) {
	if got := sanitizeDestination(" Office/inst "); got != "Office" {
		t.Fatalf("sanitizeDestination = %q, want Office", got)
	}
}

func TestParseJobsFallsBackToSingleJobGroup(t *testing.T) {
	resp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
	resp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(42)))
	resp.Job.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("doc.txt")))
	resp.Job.Add(goipp.MakeAttribute("job-originating-user-name", goipp.TagName, goipp.String("alice")))
	resp.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))

	jobs := parseJobs(resp)
	if len(jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(jobs))
	}
	if jobs[0].ID != 42 {
		t.Fatalf("job id = %d, want 42", jobs[0].ID)
	}
	if !jobs[0].HasDest || jobs[0].Dest != "Office" {
		t.Fatalf("dest = %q hasDest=%v, want Office true", jobs[0].Dest, jobs[0].HasDest)
	}
}

func TestParseJobViewHandlesInvalidURI(t *testing.T) {
	attrs := goipp.Attributes{
		goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(7)),
		goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String(":/bad uri")),
	}
	job := parseJobView(attrs)
	if job.HasDest {
		t.Fatalf("expected HasDest=false for invalid URI, got true")
	}
}

func TestDestinationURIEscapesName(t *testing.T) {
	got := destinationURI("Office Team")
	if !strings.Contains(got, "/printers/Office%20Team") {
		t.Fatalf("destinationURI = %q, want encoded path segment", got)
	}
}
