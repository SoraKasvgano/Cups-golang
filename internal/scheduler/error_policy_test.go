package scheduler

import (
	"encoding/json"
	"testing"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

func TestErrorPolicyForJob_Precedence(t *testing.T) {
	printerDefaults, err := json.Marshal(map[string]string{
		"printer-error-policy": "abort-job",
	})
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}

	s := &Scheduler{
		Config: config.Config{ErrorPolicy: "retry-job"},
	}
	printer := model.Printer{DefaultOptions: string(printerDefaults)}

	if got := s.errorPolicyForJob(printer, map[string]string{"cups-error-policy": "retry-current-job"}); got != "retry-current-job" {
		t.Fatalf("job option policy = %q, want %q", got, "retry-current-job")
	}

	if got := s.errorPolicyForJob(printer, map[string]string{}); got != "abort-job" {
		t.Fatalf("printer default policy = %q, want %q", got, "abort-job")
	}
}

func TestErrorPolicyForJob_ConfigFallbackAndNormalization(t *testing.T) {
	s := &Scheduler{
		Config: config.Config{ErrorPolicy: "ReTrY-JoB"},
	}
	printer := model.Printer{}

	if got := s.errorPolicyForJob(printer, nil); got != "retry-job" {
		t.Fatalf("config fallback policy = %q, want %q", got, "retry-job")
	}

	if got := s.errorPolicyForJob(printer, map[string]string{"cups-error-policy": "INVALID"}); got != "retry-job" {
		t.Fatalf("invalid job policy fallback = %q, want %q", got, "retry-job")
	}
}

func TestErrorPolicyForJob_InvalidValuesFallbackToStopPrinter(t *testing.T) {
	printerDefaults, err := json.Marshal(map[string]string{
		"printer-error-policy": "not-a-policy",
	})
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}

	s := &Scheduler{
		Config: config.Config{ErrorPolicy: "also-invalid"},
	}
	printer := model.Printer{DefaultOptions: string(printerDefaults)}

	if got := s.errorPolicyForJob(printer, map[string]string{"cups-error-policy": "bogus"}); got != "stop-printer" {
		t.Fatalf("final fallback policy = %q, want %q", got, "stop-printer")
	}
}

func TestNormalizeErrorPolicy(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "retry-current-job", want: "retry-current-job"},
		{in: "RETRY-JOB", want: "retry-job"},
		{in: " abort-job ", want: "abort-job"},
		{in: "stop-printer", want: "stop-printer"},
		{in: "", want: ""},
		{in: "unknown", want: ""},
	}
	for _, tc := range tests {
		if got := normalizeErrorPolicy(tc.in); got != tc.want {
			t.Fatalf("normalizeErrorPolicy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
