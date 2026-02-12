package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildCupsOptions_SkipsInternalKeys(t *testing.T) {
	opts := map[string]string{
		"copies":                 "2",
		"PageSize":               "A4",
		"custom.PageSize.Width":  "100",
		"custom.PageSize.Length": "200",
		"cups-retry-count":       "1",
		"compression-supplied":   "gzip",
		"job-attribute-fidelity": "true",
		"finishing-template":     "staple",
		"finishings":             "3",
	}
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	got := buildCupsOptions(string(b))
	if !strings.Contains(got, "copies=2") {
		t.Fatalf("expected copies in options, got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "custom.") {
		t.Fatalf("expected custom.* keys to be omitted, got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "cups-retry-count") {
		t.Fatalf("expected cups-* keys to be omitted, got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "compression-supplied") {
		t.Fatalf("expected *-supplied keys to be omitted, got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "job-attribute-fidelity") {
		t.Fatalf("expected job-attribute-fidelity to be omitted, got %q", got)
	}
	if !strings.Contains(got, "cupsFinishingTemplate=staple") {
		t.Fatalf("expected finishing-template to be mapped, got %q", got)
	}
	if strings.Contains(got, "finishings=") {
		t.Fatalf("expected finishings to be omitted when finishing-template is set, got %q", got)
	}
}
