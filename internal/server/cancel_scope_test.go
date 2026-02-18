package server

import "testing"

func TestIsAllPrintersScopeURI(t *testing.T) {
	tests := []struct {
		uri  string
		want bool
	}{
		{uri: "ipp://localhost/printers/", want: true},
		{uri: "ipp://localhost/printers", want: true},
		{uri: "ipp://localhost/classes/", want: true},
		{uri: "ipp://localhost/", want: true},
		{uri: "ipp://localhost/printers/Office", want: false},
		{uri: "ipp://localhost/classes/Team", want: false},
		{uri: "not a uri", want: false},
		{uri: "", want: false},
	}

	for _, tc := range tests {
		if got := isAllPrintersScopeURI(tc.uri); got != tc.want {
			t.Fatalf("isAllPrintersScopeURI(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}
