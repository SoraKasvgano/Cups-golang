package server

import (
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

func TestPrinterSettableAttributesIncludePolicyDefaults(t *testing.T) {
	attrs := printerSettableAttributesSupported()
	for _, want := range []string{
		"printer-error-policy",
		"printer-op-policy",
		"port-monitor",
		"job-priority-default",
		"job-hold-until-default",
		"job-cancel-after-default",
		"job-sheets-default",
		"media-default",
		"output-bin-default",
		"print-quality-default",
		"printer-resolution-default",
	} {
		if !stringInList(want, attrs) {
			t.Fatalf("expected %s in printer settable attrs: %v", want, attrs)
		}
	}
}

func TestSupportedValueAttributesClassFiltersPrinterIsShared(t *testing.T) {
	printerAttrs := supportedValueAttributes(model.Printer{}, false)
	classAttrs := supportedValueAttributes(model.Printer{}, true)

	if !hasSupportedValueAttr(printerAttrs, "printer-is-shared") {
		t.Fatalf("expected printer-is-shared for printer destination")
	}
	if hasSupportedValueAttr(classAttrs, "printer-is-shared") {
		t.Fatalf("did not expect printer-is-shared for class destination")
	}

	for _, want := range []string{"printer-error-policy", "printer-op-policy", "port-monitor"} {
		if !hasSupportedValueAttr(printerAttrs, want) {
			t.Fatalf("expected %s for printer destination", want)
		}
		if !hasSupportedValueAttr(classAttrs, want) {
			t.Fatalf("expected %s for class destination", want)
		}
	}
}

func hasSupportedValueAttr(attrs goipp.Attributes, name string) bool {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return false
		}
		if attr.Values[0].T != goipp.TagAdminDefine {
			return false
		}
		if iv, ok := attr.Values[0].V.(goipp.Integer); ok {
			return int(iv) == 0
		}
		return false
	}
	return false
}
