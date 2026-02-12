package spool

import (
	"path/filepath"
	"testing"
)

func TestOutputPath_FallsBackToSpoolDir(t *testing.T) {
	dir := t.TempDir()
	s := Spool{Dir: dir}

	got := s.OutputPath(1, `a/b:c?.pdf`)
	want := filepath.Join(dir, "job-1-abc.pdf")
	if got != want {
		t.Fatalf("OutputPath()=%q, want %q", got, want)
	}
}

func TestOutputPath_UsesOutputDirWhenSet(t *testing.T) {
	dir := t.TempDir()
	out := t.TempDir()
	s := Spool{Dir: dir, OutputDir: out}

	got := s.OutputPath(2, "doc.txt")
	want := filepath.Join(out, "job-2-doc.txt")
	if got != want {
		t.Fatalf("OutputPath()=%q, want %q", got, want)
	}
}
