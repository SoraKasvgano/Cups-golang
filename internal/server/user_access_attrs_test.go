package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestHandleCupsAddModifyPrinterStoresAllowedUsers(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("Office")))
	req.Printer.Add(goipp.MakeAttr(
		"requesting-user-name-allowed",
		goipp.TagName,
		goipp.String("alice"),
		goipp.String("bob"),
	))

	resp, err := s.handleCupsAddModifyPrinter(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/admin/", nil), req, nil)
	if err != nil {
		t.Fatalf("handleCupsAddModifyPrinter error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	var printerID int64
	var allowed string
	var denied string
	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		printer, err := s.Store.GetPrinterByName(ctx, tx, "Office")
		if err != nil {
			return err
		}
		printerID = printer.ID
		allowed, err = s.Store.GetSetting(ctx, tx, "printer."+strconv.FormatInt(printer.ID, 10)+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(ctx, tx, "printer."+strconv.FormatInt(printer.ID, 10)+".denied_users", "")
		return err
	})
	if err != nil {
		t.Fatalf("verify printer settings: %v", err)
	}
	if printerID == 0 {
		t.Fatal("expected created printer id")
	}
	if allowed != "alice,bob" {
		t.Fatalf("allowed_users = %q, want %q", allowed, "alice,bob")
	}
	if denied != "" {
		t.Fatalf("denied_users = %q, want empty", denied)
	}
}

func TestHandleCupsAddModifyClassStoresDeniedUsers(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyClass, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/classes/Team")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("Team")))
	req.Printer.Add(goipp.MakeAttr(
		"requesting-user-name-denied",
		goipp.TagName,
		goipp.String("guest"),
		goipp.String("unknown"),
	))

	resp, err := s.handleCupsAddModifyClass(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/admin/", nil), req)
	if err != nil {
		t.Fatalf("handleCupsAddModifyClass error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	var allowed string
	var denied string
	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		class, err := s.Store.GetClassByName(ctx, tx, "Team")
		if err != nil {
			return err
		}
		allowed, err = s.Store.GetSetting(ctx, tx, "class."+strconv.FormatInt(class.ID, 10)+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(ctx, tx, "class."+strconv.FormatInt(class.ID, 10)+".denied_users", "")
		return err
	})
	if err != nil {
		t.Fatalf("verify class settings: %v", err)
	}
	if allowed != "" {
		t.Fatalf("allowed_users = %q, want empty", allowed)
	}
	if denied != "guest,unknown" {
		t.Fatalf("denied_users = %q, want %q", denied, "guest,unknown")
	}
}

func TestHandleCupsAddModifyPrinterUserAccessLastAttributeWins(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipp://localhost/printers/Office")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("Office")))
	req.Printer.Add(goipp.MakeAttr("requesting-user-name-allowed", goipp.TagName, goipp.String("alice")))
	req.Printer.Add(goipp.MakeAttr("requesting-user-name-denied", goipp.TagName, goipp.String("guest")))

	resp, err := s.handleCupsAddModifyPrinter(ctx, httptest.NewRequest(http.MethodPost, "http://localhost/admin/", nil), req, nil)
	if err != nil {
		t.Fatalf("handleCupsAddModifyPrinter error: %v", err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusOk {
		t.Fatalf("status = %v, want %v", got, goipp.StatusOk)
	}

	var allowed string
	var denied string
	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		printer, err := s.Store.GetPrinterByName(ctx, tx, "Office")
		if err != nil {
			return err
		}
		allowed, err = s.Store.GetSetting(ctx, tx, "printer."+strconv.FormatInt(printer.ID, 10)+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(ctx, tx, "printer."+strconv.FormatInt(printer.ID, 10)+".denied_users", "")
		return err
	})
	if err != nil {
		t.Fatalf("verify printer settings: %v", err)
	}
	if allowed != "" {
		t.Fatalf("allowed_users = %q, want empty", allowed)
	}
	if denied != "guest" {
		t.Fatalf("denied_users = %q, want %q", denied, "guest")
	}
}
