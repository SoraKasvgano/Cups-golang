package backend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

type ippBackend struct{}

func init() {
	Register(ippBackend{})
}

func (ippBackend) Schemes() []string {
	return []string{"ipp", "ipps"}
}

func (ippBackend) ListDevices(ctx context.Context) ([]Device, error) {
	devices := envDevices("CUPS_IPP_DEVICES", "network", "IPP")
	return uniqueDevices(devices), nil
}

func (ippBackend) SubmitJob(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, filePath string) error {
	if printer.URI == "" {
		return errors.New("missing printer URI")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printer.URI)))
	user := job.UserName
	if user == "" {
		user = "anonymous"
	}
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	jobName := job.Name
	if jobName == "" {
		jobName = "Untitled"
	}
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(jobName)))
	docFormat := doc.MimeType
	if docFormat == "" {
		docFormat = "application/octet-stream"
	}
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(docFormat)))

	for _, attr := range buildJobAttributesFromOptions(job.Options) {
		req.Job.Add(attr)
	}

	payload, err := req.EncodeBytes()
	if err != nil {
		return err
	}

	body := io.MultiReader(bytes.NewBuffer(payload), f)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, printer.URI, body)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	httpReq.Header.Set("Accept", goipp.ContentType)

	client := &http.Client{Transport: ippTransport(printer.URI)}
	resp, err := client.Do(httpReq)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return errors.New(resp.Status)
	}
	ippResp := &goipp.Message{}
	if err := ippResp.Decode(resp.Body); err != nil {
		return err
	}
	status := goipp.Status(ippResp.Code)
	if status >= goipp.StatusRedirectionOtherSite {
		return errors.New(status.String())
	}
	return nil
}

func (ippBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	return SupplyStatus{State: "unknown"}, nil
}

func ippTransport(uri string) *http.Transport {
	u, _ := url.Parse(uri)
	insecure := strings.ToLower(os.Getenv("CUPS_IPP_INSECURE"))
	skipVerify := insecure == "1" || insecure == "true" || insecure == "yes" || insecure == "on"
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if u != nil && strings.EqualFold(u.Scheme, "ipps") && skipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	return &http.Transport{
		TLSClientConfig: tlsConfig,
	}
}

func buildJobAttributesFromOptions(optionsJSON string) []goipp.Attribute {
	if optionsJSON == "" {
		return nil
	}
	var opts map[string]string
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return nil
	}
	out := []goipp.Attribute{}
	for k, v := range opts {
		if v == "" {
			continue
		}
		switch k {
		case "copies", "job-priority", "number-up", "number-of-retries", "retry-interval", "retry-time-out", "job-cancel-after":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(k, goipp.TagInteger, goipp.Integer(n)))
			}
		case "print-quality", "finishings", "orientation-requested":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(k, goipp.TagEnum, goipp.Integer(n)))
			}
		case "page-ranges":
			if r, ok := parseRange(v); ok {
				out = append(out, goipp.MakeAttribute(k, goipp.TagRange, r))
			} else {
				out = append(out, goipp.MakeAttribute(k, goipp.TagKeyword, goipp.String(v)))
			}
		case "printer-resolution":
			if res, ok := parseResolution(v); ok {
				out = append(out, goipp.MakeAttribute(k, goipp.TagResolution, res))
			}
		default:
			out = append(out, goipp.MakeAttribute(k, goipp.TagKeyword, goipp.String(v)))
		}
	}
	return out
}

func parseRange(value string) (goipp.Range, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return goipp.Range{}, false
	}
	first := strings.Split(value, ",")[0]
	parts := strings.SplitN(first, "-", 2)
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start <= 0 {
		return goipp.Range{}, false
	}
	end := start
	if len(parts) == 2 {
		if v := strings.TrimSpace(parts[1]); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= start {
				end = n
			}
		}
	}
	return goipp.Range{Lower: start, Upper: end}, true
}

func parseResolution(value string) (goipp.Resolution, bool) {
	v := strings.TrimSpace(strings.ToLower(value))
	if v == "" {
		return goipp.Resolution{}, false
	}
	v = strings.TrimSuffix(v, "dpi")
	parts := strings.Split(v, "x")
	if len(parts) == 1 {
		n, err := strconv.Atoi(parts[0])
		if err != nil || n <= 0 {
			return goipp.Resolution{}, false
		}
		return goipp.Resolution{Xres: n, Yres: n, Units: goipp.UnitsDpi}, true
	}
	if len(parts) == 2 {
		x, err1 := strconv.Atoi(parts[0])
		y, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || x <= 0 || y <= 0 {
			return goipp.Resolution{}, false
		}
		return goipp.Resolution{Xres: x, Yres: y, Units: goipp.UnitsDpi}, true
	}
	return goipp.Resolution{}, false
}
