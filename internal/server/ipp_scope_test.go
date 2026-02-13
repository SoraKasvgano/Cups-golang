package server

import "testing"

func TestParseSubscriptionScopeURI(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantKind  subscriptionScopeKind
		wantName  string
		wantJobID int64
		wantOK    bool
	}{
		{name: "root", raw: "ipp://localhost/", wantKind: subscriptionScopeAll, wantOK: true},
		{name: "jobs collection", raw: "ipp://localhost/jobs", wantKind: subscriptionScopeAll, wantOK: true},
		{name: "printers collection", raw: "ipp://localhost/printers/", wantKind: subscriptionScopeAll, wantOK: true},
		{name: "classes collection", raw: "ipp://localhost/classes", wantKind: subscriptionScopeAll, wantOK: true},
		{name: "job uri", raw: "ipp://localhost/jobs/42", wantKind: subscriptionScopeJob, wantJobID: 42, wantOK: true},
		{name: "printer uri", raw: "ipp://localhost/printers/Office", wantKind: subscriptionScopePrinter, wantName: "Office", wantOK: true},
		{name: "class uri", raw: "ipp://localhost/classes/Team", wantKind: subscriptionScopeClass, wantName: "Team", wantOK: true},
		{name: "invalid path", raw: "ipp://localhost/foo/bar", wantOK: false},
		{name: "invalid job id", raw: "ipp://localhost/jobs/abc", wantOK: false},
		{name: "invalid class nested", raw: "ipp://localhost/classes/a/b", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, name, jobID, ok := parseSubscriptionScopeURI(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", kind, tc.wantKind)
			}
			if name != tc.wantName {
				t.Fatalf("name = %q, want %q", name, tc.wantName)
			}
			if jobID != tc.wantJobID {
				t.Fatalf("jobID = %d, want %d", jobID, tc.wantJobID)
			}
		})
	}
}

func TestParseDocumentURIStrict(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantJobID int64
		wantDoc   int
		wantOK    bool
	}{
		{name: "valid", raw: "ipp://localhost/jobs/123/documents/2", wantJobID: 123, wantDoc: 2, wantOK: true},
		{name: "valid trailing slash", raw: "ipp://localhost/jobs/123/documents/2/", wantJobID: 123, wantDoc: 2, wantOK: true},
		{name: "missing doc", raw: "ipp://localhost/jobs/123/documents", wantOK: false},
		{name: "wrong resource", raw: "ipp://localhost/job/123/document/2", wantOK: false},
		{name: "nested", raw: "ipp://localhost/jobs/123/documents/2/extra", wantOK: false},
		{name: "bad job", raw: "ipp://localhost/jobs/abc/documents/2", wantOK: false},
		{name: "bad doc", raw: "ipp://localhost/jobs/123/documents/x", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jobID, docNum, ok := parseDocumentURIStrict(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if pJob, pDoc := parseDocumentURI(tc.raw); pJob != 0 || pDoc != 0 {
					t.Fatalf("parseDocumentURI(%q) = (%d,%d), want (0,0)", tc.raw, pJob, pDoc)
				}
				return
			}
			if jobID != tc.wantJobID || docNum != tc.wantDoc {
				t.Fatalf("got (%d,%d), want (%d,%d)", jobID, docNum, tc.wantJobID, tc.wantDoc)
			}
		})
	}
}
