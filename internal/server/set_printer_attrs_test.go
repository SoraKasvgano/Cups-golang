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

func TestHandleSetPrinterAttributesUpdatesOnlyBasicPrinterFields(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	var printer model.Printer
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "L1", "Old Info", model.DefaultPPDName, true, false, false, "none", `{"printer-error-policy":"abort-job"}`)
		if err != nil {
			return err
		}
		geo := "geo:0,0"
		org := "Old Org"
		unit := "Old Unit"
		return s.Store.UpdatePrinterAttributes(ctx, tx, printer.ID, nil, nil, &geo, &org, &unit)
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newSetPrinterAttrsRequest("ipp://localhost/printers/Office")
	req.Printer.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String("New Info")))
	req.Printer.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String("L2")))
	req.Printer.Add(goipp.MakeAttribute("printer-geo-location", goipp.TagURI, goipp.String("geo:1,1")))
	req.Printer.Add(goipp.MakeAttribute("printer-organization", goipp.TagText, goipp.String("New Org")))
	req.Printer.Add(goipp.MakeAttribute("printer-organizational-unit", goipp.TagText, goipp.String("New Unit")))
	req.Printer.Add(goipp.MakeAttribute("printer-error-policy", goipp.TagKeyword, goipp.String("retry-current-job")))

	resp, err := s.handleSetPrinterAttributes(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleSetPrinterAttributes error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		got, err := s.Store.GetPrinterByName(ctx, tx, "Office")
		if err != nil {
			return err
		}
		if got.Info != "New Info" || got.Location != "L2" || got.Geo != "geo:1,1" || got.Org != "New Org" || got.OrgUnit != "New Unit" {
			t.Fatalf("unexpected printer fields: %+v", got)
		}
		if got.DefaultOptions != `{"printer-error-policy":"abort-job"}` {
			t.Fatalf("unexpected default options mutation: %q", got.DefaultOptions)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify store: %v", err)
	}
}

func TestHandleSetPrinterAttributesClassUpdatesInfoLocationOnly(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		printer, err := s.Store.CreatePrinter(ctx, tx, "Office", "ipp://localhost/printers/Office", "", "", model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		_, err = s.Store.CreateClass(ctx, tx, "Team", "Old Location", "Old Info", true, false, []int64{printer.ID})
		return err
	})
	if err != nil {
		t.Fatalf("setup store: %v", err)
	}

	req := newSetPrinterAttrsRequest("ipp://localhost/classes/Team")
	req.Printer.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String("New Class Info")))
	req.Printer.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String("New Class Location")))
	req.Printer.Add(goipp.MakeAttribute("printer-organization", goipp.TagText, goipp.String("Ignored For Class")))

	resp, err := s.handleSetPrinterAttributes(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/ipp/print", nil), req)
	if err != nil {
		t.Fatalf("handleSetPrinterAttributes error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		class, err := s.Store.GetClassByName(ctx, tx, "Team")
		if err != nil {
			return err
		}
		if class.Info != "New Class Info" || class.Location != "New Class Location" {
			t.Fatalf("unexpected class fields: %+v", class)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify store: %v", err)
	}
}

func newSetPrinterAttrsRequest(uri string) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSetPrinterAttributes, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(uri)))
	return req
}
