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
	template := strings.TrimSpace(opts["finishing-template"])
	ignoreFinishings := template != "" && !strings.EqualFold(template, "none")
	out := []goipp.Attribute{}
	if ignoreFinishings {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
		out = append(out, goipp.MakeAttribute("finishings-col", goipp.TagBeginCollection, col))
	}
	for k, v := range opts {
		if v == "" {
			continue
		}
		if k == "finishing-template" {
			continue
		}
		if k == "output-mode" {
			mode := strings.ToLower(strings.TrimSpace(v))
			if mode == "color" || mode == "monochrome" {
				if _, ok := opts["print-color-mode"]; !ok {
					opts["print-color-mode"] = mode
				}
			}
			continue
		}
		switch k {
		case "copies", "job-priority", "number-up", "number-of-retries", "retry-interval", "retry-time-out", "job-cancel-after":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(k, goipp.TagInteger, goipp.Integer(n)))
			}
		case "print-quality", "orientation-requested":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(k, goipp.TagEnum, goipp.Integer(n)))
			}
		case "finishings":
			if ignoreFinishings {
				continue
			}
			if enums := parseFinishingsEnums(v); len(enums) > 0 {
				vals := make([]goipp.Value, 0, len(enums))
				for _, n := range enums {
					vals = append(vals, goipp.Integer(n))
				}
				out = append(out, goipp.MakeAttr(k, goipp.TagEnum, vals[0], vals[1:]...))
			} else if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(k, goipp.TagEnum, goipp.Integer(n)))
			} else {
				out = append(out, goipp.MakeAttribute(k, goipp.TagKeyword, goipp.String(v)))
			}
		case "page-ranges":
			if ranges, ok := parseRangesList(v); ok {
				vals := make([]goipp.Value, 0, len(ranges))
				for _, r := range ranges {
					vals = append(vals, r)
				}
				out = append(out, goipp.MakeAttr(k, goipp.TagRange, vals[0], vals[1:]...))
			} else {
				out = append(out, goipp.MakeAttribute(k, goipp.TagKeyword, goipp.String(v)))
			}
		case "job-sheets":
			parts := splitList(v, 2)
			if len(parts) == 1 {
				out = append(out, goipp.MakeAttribute(k, goipp.TagKeyword, goipp.String(parts[0])))
			} else if len(parts) > 1 {
				vals := make([]goipp.Value, 0, len(parts))
				for _, p := range parts {
					vals = append(vals, goipp.String(p))
				}
				out = append(out, goipp.MakeAttr(k, goipp.TagKeyword, vals[0], vals[1:]...))
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

func parseRangesList(value string) ([]goipp.Range, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	parts := strings.Split(value, ",")
	out := make([]goipp.Range, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r, ok := parseRange(part)
		if !ok {
			return nil, false
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func parseFinishingsEnums(value string) []int {
	parts := splitList(value, 0)
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func finishingTemplateTag(value string) goipp.Tag {
	value = strings.TrimSpace(value)
	if value == "" {
		return goipp.TagKeyword
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch >= 'A' && ch <= 'Z' {
			return goipp.TagName
		}
	}
	if strings.Contains(value, " ") {
		return goipp.TagName
	}
	return goipp.TagKeyword
}

func splitList(value string, max int) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
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
