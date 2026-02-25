package logging

import (
	"net/http"
	"strings"
	"testing"
)

func TestShouldLogAccessLevels(t *testing.T) {
	if !shouldLogAccess("all", http.MethodGet, "/printers", http.StatusOK) {
		t.Fatal("all should log GET")
	}
	if shouldLogAccess("actions", http.MethodGet, "/printers", http.StatusOK) {
		t.Fatal("actions should not log successful GET")
	}
	if !shouldLogAccess("actions", http.MethodPost, "/ipp/print", http.StatusOK) {
		t.Fatal("actions should log POST")
	}
	if !shouldLogAccess("config", http.MethodGet, "/admin", http.StatusOK) {
		t.Fatal("config should log /admin")
	}
	if shouldLogAccess("config", http.MethodGet, "/printers", http.StatusOK) {
		t.Fatal("config should not log non-admin GET")
	}
}

func TestParseAuthUser(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://localhost/printers", nil)
	req.SetBasicAuth("alice", "secret")
	if got := parseAuthUser(req); got != "alice" {
		t.Fatalf("basic user = %q, want alice", got)
	}

	req, _ = http.NewRequest(http.MethodGet, "http://localhost/printers", nil)
	req.Header.Set("Authorization", `Digest username="bob", realm="CUPS", nonce="n", uri="/", response="r"`)
	if got := parseAuthUser(req); got != "bob" {
		t.Fatalf("digest user = %q, want bob", got)
	}
}

func TestPageLogLineFormat(t *testing.T) {
	Configure("", "", "", 0, "actions", "%p %u %j %T %P %C %{job-billing} %{job-originating-host-name} %{job-name} %{media} %{sides}")
	line := PageLogLine(PageLogEntry{
		JobID:      42,
		User:       "alice",
		Printer:    "Office",
		Title:      "report.pdf",
		Copies:     2,
		OriginHost: "workstation.local",
		Media:      "iso_a4_210x297mm",
		Sides:      "two-sided-long-edge",
		Billing:    "B123",
	})

	if !strings.Contains(line, "Office alice 42") {
		t.Fatalf("unexpected page log prefix: %q", line)
	}
	if !strings.Contains(line, "2 B123 workstation.local report.pdf iso_a4_210x297mm two-sided-long-edge") {
		t.Fatalf("missing page log fields: %q", line)
	}
	if !strings.Contains(line, "[") || !strings.Contains(line, "]") {
		t.Fatalf("missing timestamp brackets: %q", line)
	}
}
