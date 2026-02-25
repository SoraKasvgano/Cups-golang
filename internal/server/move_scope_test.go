package server

import "testing"

func TestParseMoveSourceURI(t *testing.T) {
	tests := []struct {
		raw        string
		wantClass  bool
		wantName   string
		expectOkay bool
	}{
		{raw: "ipp://localhost/printers/Office", wantClass: false, wantName: "Office", expectOkay: true},
		{raw: "ipp://localhost/classes/Team", wantClass: true, wantName: "Team", expectOkay: true},
		{raw: "ipp://localhost/printers/", expectOkay: false},
		{raw: "ipp://localhost/printers//", expectOkay: false},
		{raw: "ipp://localhost/printers/Office/..", expectOkay: false},
		{raw: "ipp://localhost/classes/a/b", expectOkay: false},
		{raw: "ipp://localhost/jobs/1", expectOkay: false},
		{raw: "not a uri", expectOkay: false},
	}

	for _, tc := range tests {
		isClass, name, ok := parseMoveSourceURI(tc.raw)
		if ok != tc.expectOkay {
			t.Fatalf("parseMoveSourceURI(%q) ok=%v want=%v", tc.raw, ok, tc.expectOkay)
		}
		if !ok {
			continue
		}
		if isClass != tc.wantClass || name != tc.wantName {
			t.Fatalf("parseMoveSourceURI(%q) got class=%v name=%q want class=%v name=%q", tc.raw, isClass, name, tc.wantClass, tc.wantName)
		}
	}
}

func TestParseMoveJobURI(t *testing.T) {
	tests := []struct {
		raw      string
		wantID   int64
		wantOkay bool
	}{
		{raw: "ipp://localhost/jobs/123", wantID: 123, wantOkay: true},
		{raw: "ipp://localhost/jobs/123/extra", wantID: 123, wantOkay: true},
		{raw: "ipp://localhost/jobs/abc", wantID: 0, wantOkay: true},
		{raw: "ipp://localhost/jobs/0", wantID: 0, wantOkay: true},
		{raw: "ipp://localhost/jobs/", wantID: 0, wantOkay: true},
		{raw: "ipp://localhost/not-a-job-uri", wantID: 0, wantOkay: false},
		{raw: "not a uri", wantID: 0, wantOkay: false},
	}

	for _, tc := range tests {
		gotID, gotOK := parseMoveJobURI(tc.raw)
		if gotOK != tc.wantOkay {
			t.Fatalf("parseMoveJobURI(%q) ok=%v want=%v", tc.raw, gotOK, tc.wantOkay)
		}
		if gotID != tc.wantID {
			t.Fatalf("parseMoveJobURI(%q) id=%d want=%d", tc.raw, gotID, tc.wantID)
		}
	}
}
