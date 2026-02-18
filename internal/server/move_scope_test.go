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
