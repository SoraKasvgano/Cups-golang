package server

import "testing"

func TestDNSSDPDLFromFormatsCuratedOrder(t *testing.T) {
	formats := []string{"image/urf", "application/pdf", "image/jpeg", "application/octet-stream"}
	pdl := dnssdPDLFromFormats(formats)
	if len(pdl) != 3 {
		t.Fatalf("expected 3 curated formats, got %v", pdl)
	}
	if pdl[0] != "application/pdf" || pdl[1] != "image/jpeg" || pdl[2] != "image/urf" {
		t.Fatalf("unexpected curated order: %v", pdl)
	}
}

func TestDNSSDPDLFromFormatsRawOnlyFallback(t *testing.T) {
	pdl := dnssdPDLFromFormats([]string{"application/octet-stream", "application/vnd.cups-raw"})
	if len(pdl) != 1 || pdl[0] != "application/octet-stream" {
		t.Fatalf("expected raw fallback pdl, got %v", pdl)
	}
}
