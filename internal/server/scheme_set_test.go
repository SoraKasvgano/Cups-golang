package server

import "testing"

func TestNormalizeSchemeSetSplitsDelimitedValues(t *testing.T) {
	set := normalizeSchemeSet([]string{"ipp,ipps", " socket ; lpd", "\tusb\nfile "})
	if len(set) != 6 {
		t.Fatalf("expected 6 entries, got %d (%v)", len(set), set)
	}
	for _, key := range []string{"ipp", "ipps", "socket", "lpd", "usb", "file"} {
		if !set[key] {
			t.Fatalf("missing scheme %q in %v", key, set)
		}
	}
}

func TestSchemeAllowedUsesNormalizedSets(t *testing.T) {
	include := normalizeSchemeSet([]string{"ipp,ipps"})
	exclude := normalizeSchemeSet([]string{"ipps"})
	if !schemeAllowed("ipp://printer.local/ipp/print", include, exclude) {
		t.Fatalf("expected ipp scheme to pass include/exclude")
	}
	if schemeAllowed("ipps://printer.local/ipp/print", include, exclude) {
		t.Fatalf("expected ipps scheme to be excluded")
	}
}
