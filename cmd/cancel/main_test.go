package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func TestParseArgsSupportsClustersAndAttachedValues(t *testing.T) {
	opts, err := parseArgs([]string{"-E", "-hlocalhost:8631", "-Ualice", "-u", "bob", "-ax", "Office-12"})
	if err != nil {
		t.Fatalf("parseArgs error: %v", err)
	}
	if !opts.encrypt {
		t.Fatalf("expected encrypt=true")
	}
	if opts.server != "localhost:8631" {
		t.Fatalf("unexpected server %q", opts.server)
	}
	if opts.authUser != "alice" || opts.user != "bob" {
		t.Fatalf("unexpected users: auth=%q owner=%q", opts.authUser, opts.user)
	}
	if !opts.cancelAll || !opts.purge {
		t.Fatalf("expected cancelAll and purge from -ax: %+v", opts)
	}
	if len(opts.jobs) != 1 || opts.jobs[0] != "Office-12" {
		t.Fatalf("unexpected jobs: %v", opts.jobs)
	}
}

func TestParseArgsHelpSentinel(t *testing.T) {
	_, err := parseArgs([]string{"--help"})
	if !errors.Is(err, errShowHelp) {
		t.Fatalf("expected errShowHelp, got %v", err)
	}
}

func TestSplitJobSpec(t *testing.T) {
	dest, id := splitJobSpec("Office-321")
	if dest != "Office" || id != 321 {
		t.Fatalf("unexpected split: %q/%d", dest, id)
	}
	dest, id = splitJobSpec("44")
	if dest != "" || id != 44 {
		t.Fatalf("unexpected numeric split: %q/%d", dest, id)
	}
	dest, id = splitJobSpec("Office")
	if dest != "" || id != 0 {
		t.Fatalf("expected destination-only operand to not parse as job id")
	}
}

func TestCancelWithoutExplicitTargetsNoOp(t *testing.T) {
	client := cupsclient.NewFromConfig(cupsclient.WithServer("localhost:8631"))
	if err := cancelWithoutExplicitTargets(client, options{}); err != nil {
		t.Fatalf("expected no-op without flags, got %v", err)
	}
}

func TestDestinationURIEmptyUsesAllPrintersScope(t *testing.T) {
	client := cupsclient.NewFromConfig(cupsclient.WithServer("localhost:8631"))
	uri := destinationURI(client, "")
	if !strings.Contains(uri, "/printers/") {
		t.Fatalf("expected all-printers URI, got %q", uri)
	}
}

func TestIsKnownDestination(t *testing.T) {
	known := map[string]bool{"office": true}
	if !isKnownDestination("Office", known) {
		t.Fatalf("expected Office to be known")
	}
	if isKnownDestination("Unknown", known) {
		t.Fatalf("did not expect Unknown to be known")
	}
}

func TestCancelUserJobsAlwaysSendsPrinterURI(t *testing.T) {
	reqCh := make(chan goipp.Message, 1)
	errCh := make(chan error, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			errCh <- err
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reqCh <- req

		w.Header().Set("Content-Type", goipp.ContentType)
		resp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		_ = resp.Encode(w)
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	client := cupsclient.NewFromConfig(cupsclient.WithServer(parsed.Host))

	if err := cancelUserJobs(client, "alice", false, ""); err != nil {
		t.Fatalf("cancelUserJobs error: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("decode request: %v", err)
	case req := <-reqCh:
		if got := findAttrValue(req.Operation, "printer-uri"); !strings.Contains(got, "/printers/") {
			t.Fatalf("expected printer-uri all-printers scope, got %q", got)
		}
		if got := findAttrValue(req.Operation, "requesting-user-name"); got != "alice" {
			t.Fatalf("expected requesting-user-name alice, got %q", got)
		}
	}
}

func findAttrValue(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		return attr.Values[0].V.String()
	}
	return ""
}
