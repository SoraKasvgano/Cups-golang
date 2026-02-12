package backend

import (
	"encoding/json"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestBuildJobAttributesFromOptions_FiltersInternalKeys(t *testing.T) {
	opts := map[string]string{
		"custom.PageSize.Width": "100",
		"cups-retry-count":      "1",
		"compression-supplied":  "gzip",
		"copies":                "2",
		"media":                 "A4",
		"page-ranges":           "1-2",
		"finishings":            "3",
		"output-mode":           "color",
	}
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	attrs := buildJobAttributesFromOptions(string(b))
	if hasAttr(attrs, "custom.pagesize.width") || hasAttr(attrs, "custom.PageSize.Width") {
		t.Fatalf("expected custom.* keys to be omitted")
	}
	if hasAttr(attrs, "cups-retry-count") {
		t.Fatalf("expected cups-* keys to be omitted")
	}
	if hasAttr(attrs, "compression-supplied") {
		t.Fatalf("expected *-supplied keys to be omitted")
	}
	if hasAttr(attrs, "output-mode") {
		t.Fatalf("expected output-mode to be omitted (mapped to print-color-mode)")
	}

	if a, ok := findAttr(attrs, "copies"); !ok {
		t.Fatalf("expected copies attribute")
	} else if len(a.Values) != 1 || a.Values[0].T != goipp.TagInteger {
		t.Fatalf("expected copies to be TagInteger, got %+v", a)
	} else if v, ok := a.Values[0].V.(goipp.Integer); !ok || int(v) != 2 {
		t.Fatalf("expected copies=2, got %+v", a.Values[0].V)
	}

	if a, ok := findAttr(attrs, "media"); !ok {
		t.Fatalf("expected media attribute")
	} else if len(a.Values) != 1 || a.Values[0].T != goipp.TagKeyword || a.Values[0].V.String() != "A4" {
		t.Fatalf("expected media=A4 keyword, got %+v", a)
	}

	if a, ok := findAttr(attrs, "page-ranges"); !ok {
		t.Fatalf("expected page-ranges attribute")
	} else if len(a.Values) != 1 || a.Values[0].T != goipp.TagRange {
		t.Fatalf("expected page-ranges to be TagRange, got %+v", a)
	} else if r, ok := a.Values[0].V.(goipp.Range); !ok || r.Lower != 1 || r.Upper != 2 {
		t.Fatalf("expected page-ranges 1-2, got %+v", a.Values[0].V)
	}

	if a, ok := findAttr(attrs, "finishings"); !ok {
		t.Fatalf("expected finishings attribute")
	} else if len(a.Values) != 1 || a.Values[0].T != goipp.TagEnum {
		t.Fatalf("expected finishings to be TagEnum, got %+v", a)
	} else if v, ok := a.Values[0].V.(goipp.Integer); !ok || int(v) != 3 {
		t.Fatalf("expected finishings=3, got %+v", a.Values[0].V)
	}

	if a, ok := findAttr(attrs, "print-color-mode"); !ok {
		t.Fatalf("expected print-color-mode attribute from output-mode mapping")
	} else if len(a.Values) != 1 || a.Values[0].T != goipp.TagKeyword || a.Values[0].V.String() != "color" {
		t.Fatalf("expected print-color-mode=color keyword, got %+v", a)
	}
}

func hasAttr(attrs []goipp.Attribute, name string) bool {
	_, ok := findAttr(attrs, name)
	return ok
}

func findAttr(attrs []goipp.Attribute, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}
