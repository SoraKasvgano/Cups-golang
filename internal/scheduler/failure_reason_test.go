package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cupsgolang/internal/backend"
	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

func TestFailureReasonForErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: "job-completed-successfully"},
		{name: "unsupported", err: backend.WrapUnsupported("op", "uri", errors.New("unsupported")), want: "document-unprintable-error"},
		{name: "permanent", err: backend.WrapPermanent("op", "uri", errors.New("permanent")), want: "document-unprintable-error"},
		{name: "temporary", err: backend.WrapTemporary("op", "uri", errors.New("temporary")), want: "job-stopped"},
		{name: "filter", err: errFilterPipeline, want: "document-unprintable-error"},
		{name: "format text", err: errors.New("format not supported"), want: "document-unprintable-error"},
		{name: "generic", err: errors.New("network timeout"), want: "job-stopped"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureReasonForError(tc.err); got != tc.want {
				t.Fatalf("failureReasonForError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestRunFilterPipelineReturnsErrorWhenFilterExecutionFails(t *testing.T) {
	tempDir := t.TempDir()
	inPath := filepath.Join(tempDir, "input.txt")
	outPath := filepath.Join(tempDir, "output.out")
	if err := os.WriteFile(inPath, []byte("hello"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	s := &Scheduler{
		Mime: &config.MimeDB{
			Types:     map[string]config.MimeType{},
			ExtToType: map[string]string{},
			Convs: []config.MimeConv{
				{
					Source:  "text/plain",
					Dest:    "application/octet-stream",
					Cost:    100,
					Program: "definitely_missing_filter_binary",
				},
			},
		},
		Config: config.Config{
			PPDDir: tempDir,
		},
	}

	job := model.Job{
		ID:      42,
		UserName: "alice",
		Name:     "doc",
	}
	printer := model.Printer{
		Name:    "Office",
		URI:     "file:///tmp/out",
		PPDName: model.DefaultPPDName,
	}
	doc := model.Document{
		FileName: "input.txt",
		MimeType: "text/plain",
		Path:     inPath,
	}

	_, err := s.runFilterPipeline(job, printer, doc, outPath)
	if err == nil {
		t.Fatalf("expected filter pipeline error, got nil")
	}
	if !errors.Is(err, errFilterPipeline) {
		t.Fatalf("expected errFilterPipeline, got %v", err)
	}
	if got := failureReasonForError(err); got != "document-unprintable-error" {
		t.Fatalf("failure reason = %q, want %q", got, "document-unprintable-error")
	}
}

func TestSubmitToBackendWithMissingURIIsUnprintableError(t *testing.T) {
	s := &Scheduler{}
	err := s.submitToBackend(context.Background(), model.Printer{}, model.Job{}, model.Document{}, "")
	if err == nil {
		t.Fatalf("expected backend error for missing URI")
	}
	if got := failureReasonForError(err); got != "document-unprintable-error" {
		t.Fatalf("failure reason = %q, want %q", got, "document-unprintable-error")
	}
}
