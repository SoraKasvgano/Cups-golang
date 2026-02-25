package cupsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestSendUsesCUPSLikeResourcePathByOperation(t *testing.T) {
	pathCh := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pathCh <- r.URL.Path

		w.Header().Set("Content-Type", goipp.ContentType)
		resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
		_ = resp.Encode(w)
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	client := NewFromConfig(WithServer(parsed.Host))

	tests := []struct {
		op         goipp.Op
		printerURI string
		wantPath   string
	}{
		{op: goipp.OpCancelJobs, wantPath: "/admin/"},
		{op: goipp.OpCancelJob, wantPath: "/jobs/"},
		{op: goipp.OpCupsMoveJob, wantPath: "/jobs/"},
		{op: goipp.OpGetPrinterAttributes, wantPath: "/"},
		{op: goipp.OpCupsGetDevices, wantPath: "/"},
		{op: goipp.OpCupsGetPrinters, wantPath: "/"},
		{op: goipp.OpCupsGetPpd, printerURI: "ipp://localhost/printers/Office", wantPath: "/"},
		{op: goipp.OpCupsAddModifyPrinter, printerURI: "ipp://localhost/printers/Office", wantPath: "/admin/"},
		{op: goipp.OpPrintJob, printerURI: "ipp://localhost/printers/Office", wantPath: "/printers/Office"},
		{op: goipp.OpCreateJob, printerURI: "ipp://localhost/printers/Office", wantPath: "/printers/Office"},
		{op: goipp.OpSendDocument, printerURI: "ipp://localhost/printers/Office", wantPath: "/printers/Office"},
	}

	for _, tc := range tests {
		req := goipp.NewRequest(goipp.DefaultVersion, tc.op, 1)
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
		if tc.printerURI != "" {
			req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(tc.printerURI)))
		}
		if _, err := client.Send(context.Background(), req, nil); err != nil {
			t.Fatalf("send %v: %v", tc.op, err)
		}
		got := <-pathCh
		if got != tc.wantPath {
			t.Fatalf("op %v path = %q, want %q", tc.op, got, tc.wantPath)
		}
	}
}

func TestPrinterURIUsesLocalhostAndEscapesName(t *testing.T) {
	client := NewFromConfig(WithServer("example.com:8631"), WithTLS(true))
	if got := client.PrinterURI(""); got != "ipp://localhost/printers/" {
		t.Fatalf("PrinterURI(empty) = %q", got)
	}
	if got := client.PrinterURI("Office Laser"); got != "ipp://localhost/printers/Office%20Laser" {
		t.Fatalf("PrinterURI(name) = %q", got)
	}
}
