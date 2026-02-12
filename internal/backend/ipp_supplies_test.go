package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

func TestIPPQuerySuppliesFromMarkerLevels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		resp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		resp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
		resp.Printer.Add(goipp.MakeAttr("marker-names", goipp.TagName,
			goipp.String("Black Toner"),
			goipp.String("Cyan Toner"),
		))
		resp.Printer.Add(goipp.MakeAttr("marker-levels", goipp.TagInteger,
			goipp.Integer(5),
			goipp.Integer(80),
		))
		resp.Printer.Add(goipp.MakeAttr("marker-high-levels", goipp.TagInteger,
			goipp.Integer(100),
			goipp.Integer(100),
		))
		payload, err := resp.EncodeBytes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	printer := model.Printer{URI: "ipp://" + u.Host + "/printers/test"}

	status, err := (ippBackend{}).QuerySupplies(context.Background(), printer)
	if err != nil {
		t.Fatalf("QuerySupplies error: %v", err)
	}
	if status.State != "low" {
		t.Fatalf("expected low state, got %q", status.State)
	}
	if got := status.Details["supply.1.percent"]; got != "5" {
		t.Fatalf("expected supply.1.percent=5, got %q", got)
	}
	if got := status.Details["supply.1.desc"]; got != "Black Toner" {
		t.Fatalf("expected supply.1.desc, got %q", got)
	}
	if got := status.Details["supply.2.percent"]; got != "80" {
		t.Fatalf("expected supply.2.percent=80, got %q", got)
	}
}

func TestIPPQuerySuppliesFromPrinterSupplyValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		resp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		resp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
		resp.Printer.Add(goipp.MakeAttr("printer-supply", goipp.TagText,
			goipp.String("index=1;level=50;maxcapacity=200"),
			goipp.String("index=2;level=0;maxcapacity=100"),
		))
		resp.Printer.Add(goipp.MakeAttr("printer-supply-description", goipp.TagText,
			goipp.String("Black Toner"),
			goipp.String("Cyan Toner"),
		))
		resp.Printer.Add(goipp.MakeAttr("printer-state-reasons", goipp.TagKeyword,
			goipp.String("marker-supply-empty-warning"),
		))
		payload, err := resp.EncodeBytes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	printer := model.Printer{URI: "ipp://" + u.Host + "/printers/test"}

	status, err := (ippBackend{}).QuerySupplies(context.Background(), printer)
	if err != nil {
		t.Fatalf("QuerySupplies error: %v", err)
	}
	if status.State != "empty" {
		t.Fatalf("expected empty state, got %q", status.State)
	}
	if got := status.Details["supply.1.percent"]; got != "25" {
		t.Fatalf("expected supply.1.percent=25, got %q", got)
	}
	if got := status.Details["supply.2.percent"]; got != "0" {
		t.Fatalf("expected supply.2.percent=0, got %q", got)
	}
}
