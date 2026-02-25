package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

func TestIPPMovePolicyContextsSkipWhenDestinationURIIsInvalid(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var source model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		source, err = s.Store.CreatePrinter(ctx, tx, "Source", "ipp://localhost/printers/Source", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "doc", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(testJobURI(job.ID))))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/not-a-destination")))

	contexts, err := s.ippPolicyCheckContexts(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/jobs/", nil), req)
	if err != nil {
		t.Fatalf("ippPolicyCheckContexts error: %v", err)
	}
	if len(contexts) != 0 {
		t.Fatalf("expected no policy contexts, got %d", len(contexts))
	}
}

func TestIPPMovePolicyContextsSkipWhenDestinationDoesNotExist(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var source model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		source, err = s.Store.CreatePrinter(ctx, tx, "Source", "ipp://localhost/printers/Source", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "doc", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(testJobURI(job.ID))))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/NoSuch")))

	contexts, err := s.ippPolicyCheckContexts(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/jobs/", nil), req)
	if err != nil {
		t.Fatalf("ippPolicyCheckContexts error: %v", err)
	}
	if len(contexts) != 0 {
		t.Fatalf("expected no policy contexts, got %d", len(contexts))
	}
}

func TestIPPMovePolicyContextsSkipWhenDestinationPathIsMalformed(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var source model.Printer
	var destination model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		source, err = s.Store.CreatePrinter(ctx, tx, "Source", "ipp://localhost/printers/Source", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		destination, err = s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "doc", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(testJobURI(job.ID))))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Source/../Destination")))
	_ = destination

	contexts, err := s.ippPolicyCheckContexts(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/jobs/", nil), req)
	if err != nil {
		t.Fatalf("ippPolicyCheckContexts error: %v", err)
	}
	if len(contexts) != 0 {
		t.Fatalf("expected no policy contexts, got %d", len(contexts))
	}
}

func TestIPPMovePolicyContextsSkipWhenExplicitJobIDIsZero(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		_, err := s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Destination")))

	contexts, err := s.ippPolicyCheckContexts(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/jobs/", nil), req)
	if err != nil {
		t.Fatalf("ippPolicyCheckContexts error: %v", err)
	}
	if len(contexts) != 0 {
		t.Fatalf("expected no policy contexts, got %d", len(contexts))
	}
}

func testJobURI(id int64) string {
	return "ipp://localhost/jobs/" + strconv.FormatInt(id, 10)
}
