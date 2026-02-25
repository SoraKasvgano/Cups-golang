package server

import (
	"net/http/httptest"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestRequestingUserNamePrefersAuthenticatedDigestUser(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 1)
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	httpReq := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	httpReq.Header.Set("Authorization", `Digest username="bob", realm="CUPS-Golang", nonce="n", uri="/ipp/print", response="r"`)

	if got := requestingUserName(req, httpReq); got != "bob" {
		t.Fatalf("requestingUserName = %q, want bob", got)
	}
}

func TestRequestingUserNameFallsBackToIPPAttribute(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 1)
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("alice")))

	if got := requestingUserName(req, nil); got != "alice" {
		t.Fatalf("requestingUserName = %q, want alice", got)
	}
}

func TestRequestUserForFilterSupportsNegotiateIdentity(t *testing.T) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, 1)
	req.Operation.Add(goipp.MakeAttribute("requested-user-name", goipp.TagName, goipp.String("requested")))

	httpReq := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	httpReq.RemoteAddr = "127.0.0.1:50123"
	httpReq.Header.Set("Authorization", "Negotiate dG9rZW4=")
	httpReq.Header.Set("X-Remote-User", "proxyuser")

	if got := requestUserForFilter(httpReq, req); got != "proxyuser" {
		t.Fatalf("requestUserForFilter = %q, want proxyuser", got)
	}
}
