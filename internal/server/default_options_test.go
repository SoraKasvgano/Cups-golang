package server

import (
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestCollectPrinterDefaultOptionsDeleteAttribute(t *testing.T) {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("media-default", goipp.TagDeleteAttr, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-error-policy", goipp.TagDeleteAttr, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("job-sheets-default", goipp.TagDeleteAttr, goipp.Void{}))
	defaults, sheets, sheetsOK, _ := collectPrinterDefaultOptions(attrs)
	if defaults["media"] != "" {
		t.Fatalf("expected media default to be removed marker, got %q", defaults["media"])
	}
	if defaults["printer-error-policy"] != "" {
		t.Fatalf("expected printer-error-policy remove marker")
	}
	if defaults["job-sheets"] != "" {
		t.Fatalf("expected job-sheets remove marker")
	}
	if !sheetsOK || sheets != "" {
		t.Fatalf("expected job sheets delete state, got sheets=%q ok=%v", sheets, sheetsOK)
	}
}

func TestApplyDefaultOptionUpdatesDeletesEmptyValues(t *testing.T) {
	target := map[string]string{
		"media":         "iso_a4_210x297mm",
		"print-quality": "5",
	}
	applyDefaultOptionUpdates(target, map[string]string{
		"media":         "",
		"print-quality": "4",
	})
	if _, ok := target["media"]; ok {
		t.Fatalf("expected media to be deleted")
	}
	if target["print-quality"] != "4" {
		t.Fatalf("expected print-quality update, got %q", target["print-quality"])
	}
}
