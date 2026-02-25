package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

func TestHandleCancelJobCompletedWithoutPurgeReturnsNotPossible(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, "done", "alice", "localhost", "")
		if err != nil {
			return err
		}
		done := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, job.ID, 9, "job-completed-successfully", &done)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobRequest(job.ID, "alice", false)
	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotPossible {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotPossible)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetJob(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		if got.State != 9 {
			t.Fatalf("job state = %d, want 9", got.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify state: %v", err)
	}
}

func TestHandleCancelJobCompletedWithPurgeDeletesJob(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, "done", "alice", "localhost", "")
		if err != nil {
			return err
		}
		done := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, job.ID, 9, "job-completed-successfully", &done)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobRequest(job.ID, "alice", true)
	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		_, err := s.Store.GetJob(ctx, tx, job.ID)
		if err == nil {
			t.Fatalf("job still exists after purge")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify purge: %v", err)
	}
}

func TestHandleCancelJobPrinterURIWithoutJobIDReturnsBadRequest(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCancelJobCurrentJobNotFoundReturnsNotPossible(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		_, err := s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotPossible {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotPossible)
	}
}

func TestHandleCancelJobIgnoresPurgeJobsAttribute(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, "done", "alice", "localhost", "")
		if err != nil {
			return err
		}
		done := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, job.ID, 9, "job-completed-successfully", &done)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(true)))

	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotPossible {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotPossible)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		_, err := s.Store.GetJob(ctx, tx, job.ID)
		return err
	})
	if err != nil {
		t.Fatalf("expected job to remain, got err=%v", err)
	}
}

func TestHandleCancelJobRejectsNonDestinationPrinterURIPath(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, "doc", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/Office")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotFound {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotFound)
	}
}

func TestHandleCancelJobJobURIWithTrailingPathUsesLeadingJobID(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, "doc", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(fmt.Sprintf("ipp://localhost/jobs/%d/extra", job.ID))))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetJob(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		if got.State != 7 {
			t.Fatalf("job state = %d, want 7", got.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify state: %v", err)
	}
}

func newCancelJobRequest(jobID int64, user string, purge bool) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	if purge {
		req.Operation.Add(goipp.MakeAttribute("purge-job", goipp.TagBoolean, goipp.Boolean(true)))
	}
	return req
}
