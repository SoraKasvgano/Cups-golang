package server

import (
	"testing"

	"cupsgolang/internal/config"
)

func TestDeriveDocumentFormatsForPPDNoFiltersFallsBackToRaw(t *testing.T) {
	base := []string{"application/pdf", "image/jpeg", "text/plain"}
	formats := deriveDocumentFormatsForPPD(base, &config.MimeDB{}, &config.PPD{})

	if len(formats) != 2 {
		t.Fatalf("expected raw-only formats, got %v", formats)
	}
	if !stringInList("application/octet-stream", formats) {
		t.Fatalf("missing application/octet-stream in %v", formats)
	}
	if !stringInList("application/vnd.cups-raw", formats) {
		t.Fatalf("missing application/vnd.cups-raw in %v", formats)
	}
}

func TestDeriveDocumentFormatsForPPDUsesFilterPipeline(t *testing.T) {
	db := &config.MimeDB{
		Types: map[string]config.MimeType{
			"application/pdf":             {Type: "application/pdf"},
			"image/jpeg":                  {Type: "image/jpeg"},
			"application/postscript":      {Type: "application/postscript"},
			"application/vnd.cups-raster": {Type: "application/vnd.cups-raster"},
			"application/octet-stream":    {Type: "application/octet-stream"},
			"application/vnd.cups-raw":    {Type: "application/vnd.cups-raw"},
			"text/plain":                  {Type: "text/plain"},
		},
		Convs: []config.MimeConv{
			{Source: "application/pdf", Dest: "application/vnd.cups-raster", Cost: 5, Program: "-"},
			{Source: "image/jpeg", Dest: "application/vnd.cups-raster", Cost: 5, Program: "-"},
			{Source: "application/postscript", Dest: "application/octet-stream", Cost: 5, Program: "-"},
		},
	}
	ppd := &config.PPD{
		Filters: []config.PPDFilter{{
			Source:  "application/vnd.cups-raster",
			Dest:    "application/vnd.cups-raster",
			Cost:    0,
			Program: "-",
		}},
	}
	base := []string{"application/pdf", "image/jpeg", "application/postscript", "text/plain"}
	formats := deriveDocumentFormatsForPPD(base, db, ppd)

	for _, want := range []string{"application/pdf", "image/jpeg", "application/postscript", "application/octet-stream", "application/vnd.cups-raw"} {
		if !stringInList(want, formats) {
			t.Fatalf("expected %s in %v", want, formats)
		}
	}
	if stringInList("text/plain", formats) {
		t.Fatalf("unexpected text/plain in %v", formats)
	}
}

func TestParsePPDCachePayloadPreservesMetadataOnlyRows(t *testing.T) {
	payload := parsePPDCachePayload(`{"size":"100","mtime":"2026-02-12T10:00:00Z"}`)
	if payload.Size != "100" || payload.MTime != "2026-02-12T10:00:00Z" {
		t.Fatalf("unexpected metadata payload: %#v", payload)
	}
	if len(payload.Formats) != 0 {
		t.Fatalf("expected empty formats for metadata-only payload, got %v", payload.Formats)
	}
}

func TestParsePPDCachePayloadNormalizesFormats(t *testing.T) {
	payload := parsePPDCachePayload(`{"size":"100","mtime":"2026-02-12T10:00:00Z","formats":["application/pdf"]}`)
	for _, want := range []string{"application/pdf", "application/octet-stream", "application/vnd.cups-raw"} {
		if !stringInList(want, payload.Formats) {
			t.Fatalf("expected %s in %v", want, payload.Formats)
		}
	}
}

func TestDeriveDocumentFormatsSkipsUnavailableConverters(t *testing.T) {
	db := &config.MimeDB{
		Types: map[string]config.MimeType{
			"application/pdf":             {Type: "application/pdf"},
			"application/octet-stream":    {Type: "application/octet-stream"},
			"application/vnd.cups-raw":    {Type: "application/vnd.cups-raw"},
			"application/vnd.cups-raster": {Type: "application/vnd.cups-raster"},
		},
		Convs: []config.MimeConv{{
			Source:  "application/pdf",
			Dest:    "application/vnd.cups-raster",
			Cost:    5,
			Program: "this-filter-should-not-exist-123456",
		}},
	}
	ppd := &config.PPD{Filters: []config.PPDFilter{{
		Source:  "application/vnd.cups-raster",
		Dest:    "application/vnd.cups-raster",
		Cost:    0,
		Program: "-",
	}}}
	formats := deriveDocumentFormatsForPPD([]string{"application/pdf"}, db, ppd)
	if stringInList("application/pdf", formats) {
		t.Fatalf("expected pdf to be filtered out when converter is unavailable: %v", formats)
	}
}
