package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

func TestHandleGetPrinterSupportedValuesIgnoresRequestedAttributesFilter(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		_, err := s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterSupportedValues, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-info")))

	resp, err := s.handleGetPrinterSupportedValues(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleGetPrinterSupportedValues error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	expected := []string{
		"printer-geo-location",
		"printer-info",
		"printer-location",
		"printer-organization",
		"printer-organizational-unit",
	}
	for _, name := range expected {
		if !hasSupportedValueAttr(resp.Printer, name) {
			t.Fatalf("missing supported value attr %q in response: %#v", name, resp.Printer)
		}
	}
}

func TestHandleGetPrinterSupportedValuesForClass(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		printer, err := s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		_, err = s.Store.CreateClass(ctx, tx, "Team", "", "", true, false, []int64{printer.ID})
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterSupportedValues, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/classes/Team")))

	resp, err := s.handleGetPrinterSupportedValues(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleGetPrinterSupportedValues error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	for _, name := range []string{
		"printer-geo-location",
		"printer-info",
		"printer-location",
		"printer-organization",
		"printer-organizational-unit",
	} {
		if !hasSupportedValueAttr(resp.Printer, name) {
			t.Fatalf("missing supported value attr %q in class response", name)
		}
	}
}
