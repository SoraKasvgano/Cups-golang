package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

func TestHandleCancelJobsAllPrintersScopeRespectsOwner(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var aliceJob model.Job
	var bobJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		aliceJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "alice-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "bob-job", "bob", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/", "alice", false)
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("alice job state = %d, want 7", job.State)
		}

		job, err = s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("bob job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsReturnsNotAuthorizedWhenNoOwnedJobs(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var bobJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "bob-job", "bob", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/Office", "alice", false)
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotAuthorized {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotAuthorized)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("bob job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsWithJobIDsRespectsDestinationScope(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var office model.Printer
	var lab model.Printer
	var labJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		office, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		lab, err = s.Store.CreatePrinter(ctx, tx, "Lab", "ipp://localhost/printers/Lab", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		_ = office
		labJob, err = s.Store.CreateJob(ctx, tx, lab.ID, "lab-job", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/Office", "alice", false)
	req.Operation.Add(goipp.MakeAttr("job-ids", goipp.TagInteger, goipp.Integer(labJob.ID)))
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotFound {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotFound)
	}
}

func TestHandleCancelJobsWithJobIDsRejectsUnauthorized(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var bobJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "bob-job", "bob", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/Office", "alice", false)
	req.Operation.Add(goipp.MakeAttr("job-ids", goipp.TagInteger, goipp.Integer(bobJob.ID)))
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotAuthorized {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotAuthorized)
	}
}

func TestHandleCancelJobsWithJobIDsCancelsSelectedJob(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var aliceJob model.Job
	var otherJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		aliceJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "alice-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		otherJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "other-job", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/Office", "alice", false)
	req.Operation.Add(goipp.MakeAttr("job-ids", goipp.TagInteger, goipp.Integer(aliceJob.ID)))
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("selected job state = %d, want 7", job.State)
		}
		job, err = s.Store.GetJob(ctx, tx, otherJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("unselected job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsAllPrintersScopeMixedOwnership(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var office model.Printer
	var lab model.Printer
	var bobJob model.Job
	var aliceJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		office, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		lab, err = s.Store.CreatePrinter(ctx, tx, "Lab", "ipp://localhost/printers/Lab", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, office.ID, "bob-job", "bob", "localhost", "")
		if err != nil {
			return err
		}
		aliceJob, err = s.Store.CreateJob(ctx, tx, lab.ID, "alice-job", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/printers/", "alice", false)
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("alice job state = %d, want 7", job.State)
		}
		job, err = s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("bob job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsClassScopeMixedMembershipOwnership(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var office model.Printer
	var lab model.Printer
	var bobJob model.Job
	var aliceJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		office, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		lab, err = s.Store.CreatePrinter(ctx, tx, "Lab", "ipp://localhost/printers/Lab", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		if _, err := s.Store.CreateClass(ctx, tx, "Team", "", "", true, false, []int64{office.ID, lab.ID}); err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, office.ID, "bob-job", "bob", "localhost", "")
		if err != nil {
			return err
		}
		aliceJob, err = s.Store.CreateJob(ctx, tx, lab.ID, "alice-job", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newCancelJobsRequest("ipp://localhost/classes/Team", "alice", false)
	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("alice job state = %d, want 7", job.State)
		}
		job, err = s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("bob job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsDefaultDoesNotPurgeCompletedJobs(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var activeJob model.Job
	var completedJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		activeJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "active-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		completedJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "completed-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		done := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, completedJob.ID, 9, "job-completed-successfully", &done)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, activeJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("active job state = %d, want 7", job.State)
		}
		job, err = s.Store.GetJob(ctx, tx, completedJob.ID)
		if err != nil {
			return err
		}
		if job.State != 9 {
			t.Fatalf("completed job state = %d, want 9", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelJobsRequiresPrinterURI(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCancelMyJobsRequiresPrinterURI(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelMyJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCancelMyJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelMyJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCancelMyJobsRequiresUserIdentity(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelMyJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/")))

	resp, err := s.handleCancelMyJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelMyJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCancelMyJobsUsesAuthenticatedUserBeforeRequestingUserName(t *testing.T) {
	s := newMoveTestServer(t)
	s.Config.DefaultAuthType = "basic"
	s.Policy.Locations = []config.LocationRule{{Path: "/", AuthType: "basic"}}
	ctx := context.Background()

	var printer model.Printer
	var aliceJob model.Job
	var bobJob model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if err := s.Store.CreateUser(ctx, tx, "alice", "alicepass", false); err != nil {
			return err
		}
		if err := s.Store.CreateUser(ctx, tx, "bob", "bobpass", false); err != nil {
			return err
		}
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		aliceJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "alice-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "bob-job", "bob", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelMyJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("bob")))

	httpReq := httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil)
	httpReq.SetBasicAuth("alice", "alicepass")
	resp, err := s.handleCancelMyJobs(ctx, httpReq, req)
	if err != nil {
		t.Fatalf("handleCancelMyJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.State != 7 {
			t.Fatalf("alice job state = %d, want 7", job.State)
		}
		job, err = s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.State != 3 {
			t.Fatalf("bob job state = %d, want 3", job.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCancelMyJobsPurgeRemovesCompletedJobs(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	var activeJob model.Job
	var completedJob model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		activeJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "active-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		completedJob, err = s.Store.CreateJob(ctx, tx, printer.ID, "completed-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		done := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, completedJob.ID, 9, "job-completed-successfully", &done)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelMyJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(true)))

	resp, err := s.handleCancelMyJobs(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCancelMyJobs error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		if _, err := s.Store.GetJob(ctx, tx, activeJob.ID); err == nil {
			t.Fatalf("active job still exists after purge")
		}
		if _, err := s.Store.GetJob(ctx, tx, completedJob.ID); err == nil {
			t.Fatalf("completed job still exists after purge")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify purge: %v", err)
	}
}

func newCancelJobsRequest(printerURI, user string, purge bool) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCancelJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printerURI)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	req.Operation.Add(goipp.MakeAttribute("purge-jobs", goipp.TagBoolean, goipp.Boolean(purge)))
	return req
}
