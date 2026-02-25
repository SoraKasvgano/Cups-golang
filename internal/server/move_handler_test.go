package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
	"cupsgolang/internal/store"
)

func TestHandleCupsMoveJobMoveAllHonorsOwnerScope(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var source model.Printer
	var destination model.Printer
	var aliceJob model.Job
	var bobJob model.Job

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
		aliceJob, err = s.Store.CreateJob(ctx, tx, source.ID, "alice-job", "alice", "localhost", "")
		if err != nil {
			return err
		}
		bobJob, err = s.Store.CreateJob(ctx, tx, source.ID, "bob-job", "bob", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := buildMoveRequest("ipp://localhost/printers/Source", "ipp://localhost/printers/Destination", "alice")
	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, aliceJob.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != destination.ID {
			t.Fatalf("alice job printer_id = %d, want %d", job.PrinterID, destination.ID)
		}

		job, err = s.Store.GetJob(ctx, tx, bobJob.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != source.ID {
			t.Fatalf("bob job printer_id = %d, want %d", job.PrinterID, source.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCupsMoveJobRequiresJobPrinterURI(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Source")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCupsMoveJobRejectsNonDestinationURIPath(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if _, err := s.Store.CreatePrinter(ctx, tx, "Source", "ipp://localhost/printers/Source", "", "", model.DefaultPPDName, true, false, false, "none", ""); err != nil {
			return err
		}
		_, err := s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := buildMoveRequest("ipp://localhost/printers/Source", "ipp://localhost/Destination", "alice")
	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotFound {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotFound)
	}
}

func TestHandleCupsMoveJobWithZeroJobIDReturnsNotFound(t *testing.T) {
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
		_, err = s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "job-a", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Source")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(0)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Destination")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotFound {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotFound)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetJob(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		if got.PrinterID != source.ID {
			t.Fatalf("job printer_id = %d, want %d", got.PrinterID, source.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify job: %v", err)
	}
}

func TestHandleCupsMoveJobWithInvalidJobURIReturnsBadRequest(t *testing.T) {
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
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String("ipp://localhost/not-a-job-uri")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Destination")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorBadRequest {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorBadRequest)
	}
}

func TestHandleCupsMoveJobWithJobURIZeroReturnsNotFound(t *testing.T) {
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
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String("ipp://localhost/jobs/0")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Destination")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorNotFound {
		t.Fatalf("status = %v, want %v", got, goipp.StatusErrorNotFound)
	}
}

func TestHandleCupsMoveJobWithJobURIExtraPathStillTargetsJobID(t *testing.T) {
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
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "job-a", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String("ipp://localhost/jobs/"+strconv.FormatInt(job.ID, 10)+"/extra")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Destination")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetJob(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		if got.PrinterID != destination.ID {
			t.Fatalf("job printer_id = %d, want %d", got.PrinterID, destination.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify job: %v", err)
	}
}

func TestHandleCupsMoveJobMoveAllFromClass(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var sourceA model.Printer
	var sourceB model.Printer
	var destination model.Printer
	var jobA model.Job
	var jobB model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		sourceA, err = s.Store.CreatePrinter(ctx, tx, "SourceA", "ipp://localhost/printers/SourceA", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		sourceB, err = s.Store.CreatePrinter(ctx, tx, "SourceB", "ipp://localhost/printers/SourceB", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		destination, err = s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		if _, err := s.Store.CreateClass(ctx, tx, "Team", "", "", true, false, []int64{sourceA.ID, sourceB.ID}); err != nil {
			return err
		}
		jobA, err = s.Store.CreateJob(ctx, tx, sourceA.ID, "job-a", "alice", "localhost", "")
		if err != nil {
			return err
		}
		jobB, err = s.Store.CreateJob(ctx, tx, sourceB.ID, "job-b", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := buildMoveRequest("ipp://localhost/classes/Team", "ipp://localhost/printers/Destination", "alice")
	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobA.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != destination.ID {
			t.Fatalf("jobA printer_id = %d, want %d", job.PrinterID, destination.ID)
		}

		job, err = s.Store.GetJob(ctx, tx, jobB.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != destination.ID {
			t.Fatalf("jobB printer_id = %d, want %d", job.PrinterID, destination.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCupsMoveJobMoveAllFromClassViaPrintersPath(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var sourceA model.Printer
	var sourceB model.Printer
	var destination model.Printer
	var jobA model.Job
	var jobB model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		sourceA, err = s.Store.CreatePrinter(ctx, tx, "SourceA", "ipp://localhost/printers/SourceA", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		sourceB, err = s.Store.CreatePrinter(ctx, tx, "SourceB", "ipp://localhost/printers/SourceB", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		destination, err = s.Store.CreatePrinter(ctx, tx, "Destination", "ipp://localhost/printers/Destination", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		if _, err := s.Store.CreateClass(ctx, tx, "Team", "", "", true, false, []int64{sourceA.ID, sourceB.ID}); err != nil {
			return err
		}
		jobA, err = s.Store.CreateJob(ctx, tx, sourceA.ID, "job-a", "alice", "localhost", "")
		if err != nil {
			return err
		}
		jobB, err = s.Store.CreateJob(ctx, tx, sourceB.ID, "job-b", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := buildMoveRequest("ipp://localhost/printers/Team", "ipp://localhost/printers/Destination", "alice")
	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobA.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != destination.ID {
			t.Fatalf("jobA printer_id = %d, want %d", job.PrinterID, destination.ID)
		}

		job, err = s.Store.GetJob(ctx, tx, jobB.ID)
		if err != nil {
			return err
		}
		if job.PrinterID != destination.ID {
			t.Fatalf("jobB printer_id = %d, want %d", job.PrinterID, destination.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func TestHandleCupsMoveJobDestinationClassViaPrintersPath(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var source model.Printer
	var memberA model.Printer
	var memberB model.Printer
	var job model.Job

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		source, err = s.Store.CreatePrinter(ctx, tx, "Source", "ipp://localhost/printers/Source", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		memberA, err = s.Store.CreatePrinter(ctx, tx, "MemberA", "ipp://localhost/printers/MemberA", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		memberB, err = s.Store.CreatePrinter(ctx, tx, "MemberB", "ipp://localhost/printers/MemberB", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		if _, err := s.Store.CreateClass(ctx, tx, "Team", "", "", true, false, []int64{memberA.ID, memberB.ID}); err != nil {
			return err
		}
		job, err = s.Store.CreateJob(ctx, tx, source.ID, "job-a", "alice", "localhost", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Team")))

	resp, err := s.handleCupsMoveJob(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleCupsMoveJob error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetJob(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		if got.PrinterID != memberA.ID {
			t.Fatalf("job printer_id = %d, want first class member %d", got.PrinterID, memberA.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify jobs: %v", err)
	}
}

func newMoveTestServer(t *testing.T) *Server {
	t.Helper()

	tempRoot, err := os.MkdirTemp("", "cupsgolang-server-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	st, err := store.Open(context.Background(), filepath.Join(tempRoot, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(tempRoot); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = os.RemoveAll(tempRoot)
	})

	return &Server{
		Config: config.Config{
			DefaultAuthType: "none",
			MaxEvents:       100,
			MaxJobTime:      int((2 * time.Hour).Seconds()),
		},
		Store: st,
		Policy: config.Policy{
			Locations: []config.LocationRule{
				{Path: "/", AuthType: "none"},
			},
		},
	}
}

func buildMoveRequest(sourceURI, destinationURI, user string) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(sourceURI)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	req.Job.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String(destinationURI)))
	return req
}
