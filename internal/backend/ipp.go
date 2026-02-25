package backend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
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
		return WrapPermanent("ipp-submit", printer.URI, errors.New("missing printer URI"))
	}
	httpURL, err := ippTransportURL(printer.URI)
	if err != nil {
		return WrapUnsupported("ipp-submit", printer.URI, err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return WrapPermanent("ipp-submit", printer.URI, err)
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
		return WrapPermanent("ipp-encode", printer.URI, err)
	}

	body := io.MultiReader(bytes.NewBuffer(payload), f)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL, body)
	if err != nil {
		return WrapPermanent("ipp-request", printer.URI, err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	httpReq.Header.Set("Accept", goipp.ContentType)

	client := &http.Client{Transport: ippTransport(printer.URI)}
	resp, err := client.Do(httpReq)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return WrapTemporary("ipp-submit", printer.URI, err)
	}
	if resp.StatusCode/100 != 2 {
		return classifyIPPHTTPStatus("ipp-submit", printer.URI, resp.StatusCode, resp.Status)
	}
	ippResp := &goipp.Message{}
	if err := ippResp.Decode(resp.Body); err != nil {
		return WrapPermanent("ipp-decode", printer.URI, err)
	}
	status := goipp.Status(ippResp.Code)
	if err := classifyIPPStatus("ipp-submit", printer.URI, status); err != nil {
		return err
	}
	return nil
}

func (ippBackend) QuerySupplies(ctx context.Context, printer model.Printer) (SupplyStatus, error) {
	if strings.TrimSpace(printer.URI) == "" {
		return SupplyStatus{State: "unknown"}, nil
	}
	httpURL, err := ippTransportURL(printer.URI)
	if err != nil {
		return SupplyStatus{State: "unknown"}, WrapUnsupported("ipp-supplies", printer.URI, err)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printer.URI)))
	req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("printer-state-reasons"),
		goipp.String("printer-alert"),
		goipp.String("printer-supply"),
		goipp.String("printer-supply-description"),
		goipp.String("marker-names"),
		goipp.String("marker-types"),
		goipp.String("marker-colors"),
		goipp.String("marker-levels"),
		goipp.String("marker-high-levels"),
		goipp.String("marker-low-levels"),
	))
	payload, err := req.EncodeBytes()
	if err != nil {
		return SupplyStatus{State: "unknown"}, WrapPermanent("ipp-supplies", printer.URI, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL, bytes.NewReader(payload))
	if err != nil {
		return SupplyStatus{State: "unknown"}, WrapPermanent("ipp-supplies", printer.URI, err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	httpReq.Header.Set("Accept", goipp.ContentType)

	client := &http.Client{Transport: ippTransport(printer.URI)}
	resp, err := client.Do(httpReq)
	if err != nil {
		return SupplyStatus{State: "unknown"}, WrapTemporary("ipp-supplies", printer.URI, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return SupplyStatus{State: "unknown"}, classifyIPPHTTPStatus("ipp-supplies", printer.URI, resp.StatusCode, resp.Status)
	}

	ippResp := &goipp.Message{}
	if err := ippResp.Decode(resp.Body); err != nil {
		return SupplyStatus{State: "unknown"}, WrapPermanent("ipp-supplies", printer.URI, err)
	}
	status := goipp.Status(ippResp.Code)
	if err := classifyIPPStatus("ipp-supplies", printer.URI, status); err != nil {
		return SupplyStatus{State: "unknown"}, err
	}

	attrs := ippResp.Printer
	if len(attrs) == 0 {
		for _, g := range ippResp.Groups {
			if g.Tag == goipp.TagPrinterGroup {
				attrs = append(attrs, g.Attrs...)
			}
		}
	}
	statusOut := ippSuppliesFromAttributes(attrs)
	if statusOut.State != "unknown" || len(statusOut.Details) > 0 {
		return statusOut, nil
	}
	if snmpStatus, snmpErr, ok := querySuppliesViaSNMP(ctx, printer); ok {
		if snmpErr == nil || snmpStatus.State != "unknown" || len(snmpStatus.Details) > 0 {
			return snmpStatus, snmpErr
		}
	}
	return statusOut, nil
}

func ippSuppliesFromAttributes(attrs goipp.Attributes) SupplyStatus {
	details := map[string]string{}
	if len(attrs) == 0 {
		return SupplyStatus{State: "unknown", Details: details}
	}

	supplyRaw := ippAttrStrings(attrs, "printer-supply")
	supplyDesc := ippAttrStrings(attrs, "printer-supply-description")
	for i, raw := range supplyRaw {
		idx := i + 1
		if parsed := parseSupplyIndex(raw); parsed > 0 {
			idx = parsed
		}
		mergePrinterSupplyEntry(details, idx, raw)
	}
	for i, desc := range supplyDesc {
		if desc == "" {
			continue
		}
		idx := i + 1
		setSupplyDetail(details, idx, "desc", desc)
	}

	names := ippAttrStrings(attrs, "marker-names")
	types := ippAttrStrings(attrs, "marker-types")
	colors := ippAttrStrings(attrs, "marker-colors")
	levels := ippAttrInts(attrs, "marker-levels")
	highs := ippAttrInts(attrs, "marker-high-levels")
	count := maxInts(len(names), len(types), len(colors), len(levels), len(highs))
	for i := 0; i < count; i++ {
		idx := i + 1
		desc := strings.TrimSpace(getAt(names, i))
		if desc == "" {
			desc = strings.TrimSpace(getAt(types, i))
		}
		if desc == "" {
			desc = strings.TrimSpace(getAt(colors, i))
		}
		if desc != "" {
			setSupplyDetail(details, idx, "desc", desc)
		}
		if i < len(levels) {
			level := levels[i]
			if level >= 0 {
				setSupplyDetail(details, idx, "level", strconv.Itoa(level))
			}
			high := 0
			if i < len(highs) {
				high = highs[i]
			}
			if high > 0 {
				setSupplyDetail(details, idx, "max", strconv.Itoa(high))
				setSupplyDetail(details, idx, "percent", strconv.Itoa(clampPercent((level*100)/high)))
			} else if level >= 0 && level <= 100 {
				setSupplyDetail(details, idx, "max", "100")
				setSupplyDetail(details, idx, "percent", strconv.Itoa(level))
			}
		}
	}

	state := supplyStateFromDetails(details)
	if reason := ippSupplyStateFromReasons(ippAttrStrings(attrs, "printer-state-reasons"), ippAttrStrings(attrs, "printer-alert")); reason != "" {
		state = reason
	}
	if state == "" {
		if len(details) == 0 {
			state = "unknown"
		} else {
			state = "ok"
		}
	}
	return SupplyStatus{State: state, Details: details}
}

func parseSupplyIndex(raw string) int {
	pairs := parseSupplyPairs(raw)
	if v := strings.TrimSpace(pairs["index"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if v := strings.TrimSpace(pairs["marker-index"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func mergePrinterSupplyEntry(details map[string]string, idx int, raw string) {
	if idx <= 0 {
		return
	}
	pairs := parseSupplyPairs(raw)
	for key, val := range pairs {
		if val == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description", "desc", "name":
			setSupplyDetail(details, idx, "desc", val)
		case "level":
			if n, err := strconv.Atoi(val); err == nil {
				setSupplyDetail(details, idx, "level", strconv.Itoa(n))
			}
		case "max", "maxcapacity", "capacity", "max-capacity":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				setSupplyDetail(details, idx, "max", strconv.Itoa(n))
			}
		case "percent", "percentage":
			if n, err := strconv.Atoi(val); err == nil {
				setSupplyDetail(details, idx, "percent", strconv.Itoa(clampPercent(n)))
			}
		}
	}
	maxVal, maxOK := supplyDetailInt(details, idx, "max")
	levelVal, levelOK := supplyDetailInt(details, idx, "level")
	if maxOK && levelOK && maxVal > 0 {
		setSupplyDetail(details, idx, "percent", strconv.Itoa(clampPercent((levelVal*100)/maxVal)))
	}
}

func parseSupplyPairs(raw string) map[string]string {
	pairs := map[string]string{}
	for _, token := range strings.Split(raw, ";") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if strings.Contains(token, "=") {
			parts := strings.SplitN(token, "=", 2)
			key := strings.ToLower(strings.TrimSpace(parts[0]))
			val := ""
			if len(parts) == 2 {
				val = strings.TrimSpace(parts[1])
			}
			pairs[key] = val
			continue
		}
		if _, exists := pairs["index"]; !exists {
			pairs["index"] = token
		}
	}
	return pairs
}

func setSupplyDetail(details map[string]string, idx int, key, value string) {
	if details == nil || idx <= 0 {
		return
	}
	key = strings.TrimSpace(strings.ToLower(key))
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	details[fmt.Sprintf("supply.%d.%s", idx, key)] = value
}

func supplyDetailInt(details map[string]string, idx int, key string) (int, bool) {
	if details == nil || idx <= 0 {
		return 0, false
	}
	v := strings.TrimSpace(details[fmt.Sprintf("supply.%d.%s", idx, key)])
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func supplyStateFromDetails(details map[string]string) string {
	if len(details) == 0 {
		return ""
	}
	lowest := 101
	found := false
	for key, raw := range details {
		if !strings.HasSuffix(key, ".percent") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		n = clampPercent(n)
		if n < lowest {
			lowest = n
		}
		found = true
	}
	if !found {
		return ""
	}
	if lowest <= 0 {
		return "empty"
	}
	if lowest <= 10 {
		return "low"
	}
	return "ok"
}

func ippSupplyStateFromReasons(reasons []string, alerts []string) string {
	combined := append([]string{}, reasons...)
	combined = append(combined, alerts...)
	for _, raw := range combined {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if strings.Contains(v, "supply-empty") || strings.Contains(v, "toner-empty") || strings.Contains(v, "marker-empty") {
			return "empty"
		}
		if strings.Contains(v, "supply-low") || strings.Contains(v, "toner-low") || strings.Contains(v, "marker-low") {
			return "low"
		}
	}
	return ""
}

func ippAttrStrings(attrs goipp.Attributes, name string) []string {
	out := []string{}
	for _, attr := range attrs {
		if !strings.EqualFold(attr.Name, name) {
			continue
		}
		for _, value := range attr.Values {
			raw := strings.TrimSpace(value.V.String())
			if raw != "" {
				out = append(out, raw)
			}
		}
	}
	return out
}

func ippAttrInts(attrs goipp.Attributes, name string) []int {
	out := []int{}
	for _, attr := range attrs {
		if !strings.EqualFold(attr.Name, name) {
			continue
		}
		for _, value := range attr.Values {
			if n, ok := ippValueInt(value.V); ok {
				out = append(out, n)
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(value.V.String())); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

func ippValueInt(value goipp.Value) (int, bool) {
	switch v := value.(type) {
	case goipp.Integer:
		return int(v), true
	default:
		return 0, false
	}
}

func getAt(values []string, idx int) string {
	if idx < 0 || idx >= len(values) {
		return ""
	}
	return values[idx]
}

func maxInts(values ...int) int {
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func ippTransport(uri string) *http.Transport {
	u, _ := url.Parse(uri)
	insecure := strings.ToLower(os.Getenv("CUPS_IPP_INSECURE"))
	skipVerify := insecure == "1" || insecure == "true" || insecure == "yes" || insecure == "on"
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if u != nil && (strings.EqualFold(u.Scheme, "ipps") || strings.EqualFold(u.Scheme, "https")) && skipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	return &http.Transport{
		TLSClientConfig: tlsConfig,
	}
}

func ippTransportURL(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", errors.New("invalid printer URI")
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	case "http", "https":
		// Keep as-is.
	default:
		return "", fmt.Errorf("unsupported IPP scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func classifyIPPHTTPStatus(op, uri string, code int, statusText string) error {
	if code/100 == 2 {
		return nil
	}
	err := errors.New(strings.TrimSpace(statusText))
	switch {
	case code >= 500, code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return WrapTemporary(op, uri, err)
	case code == http.StatusNotFound || code == http.StatusGone || code == http.StatusNotImplemented:
		return WrapUnsupported(op, uri, err)
	default:
		return WrapPermanent(op, uri, err)
	}
}

func classifyIPPStatus(op, uri string, status goipp.Status) error {
	if status < goipp.StatusRedirectionOtherSite {
		return nil
	}
	err := errors.New(status.String())
	switch status {
	case goipp.StatusErrorOperationNotSupported,
		goipp.StatusErrorDocumentFormatNotSupported,
		goipp.StatusErrorDocumentUnprintable,
		goipp.StatusErrorDocumentFormatError,
		goipp.StatusErrorAttributesOrValues,
		goipp.StatusErrorURIScheme:
		return WrapUnsupported(op, uri, err)

	case goipp.StatusErrorTemporary,
		goipp.StatusErrorServiceUnavailable,
		goipp.StatusErrorNotAcceptingJobs,
		goipp.StatusErrorBusy,
		goipp.StatusErrorDevice,
		goipp.StatusErrorPrinterIsDeactivated,
		goipp.StatusErrorTimeout,
		goipp.StatusErrorTooManyJobs,
		goipp.StatusErrorTooManyDocuments:
		return WrapTemporary(op, uri, err)
	}
	if int(status) >= int(goipp.StatusErrorInternal) {
		return WrapTemporary(op, uri, err)
	}
	return WrapPermanent(op, uri, err)
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

	// Avoid mutating the map during iteration below.
	if mode := strings.ToLower(strings.TrimSpace(opts["output-mode"])); (mode == "color" || mode == "monochrome") && strings.TrimSpace(opts["print-color-mode"]) == "" {
		opts["print-color-mode"] = mode
	}
	out := []goipp.Attribute{}
	if ignoreFinishings {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
		out = append(out, goipp.MakeAttribute("finishings-col", goipp.TagBeginCollection, col))
	}
	for k, v := range opts {
		lk := strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(lk, "cups-") || strings.HasPrefix(lk, "custom.") || strings.HasSuffix(lk, "-supplied") || lk == "job-attribute-fidelity" {
			continue
		}
		if v == "" {
			continue
		}
		if lk == "finishing-template" || lk == "output-mode" {
			continue
		}
		switch lk {
		case "copies", "job-priority", "number-up", "number-of-retries", "retry-interval", "retry-time-out", "job-cancel-after":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(lk, goipp.TagInteger, goipp.Integer(n)))
			}
		case "print-quality", "orientation-requested":
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(lk, goipp.TagEnum, goipp.Integer(n)))
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
				out = append(out, goipp.MakeAttr(lk, goipp.TagEnum, vals[0], vals[1:]...))
			} else if n, err := strconv.Atoi(v); err == nil {
				out = append(out, goipp.MakeAttribute(lk, goipp.TagEnum, goipp.Integer(n)))
			}
		case "page-ranges":
			if ranges, ok := parseRangesList(v); ok {
				vals := make([]goipp.Value, 0, len(ranges))
				for _, r := range ranges {
					vals = append(vals, r)
				}
				out = append(out, goipp.MakeAttr(lk, goipp.TagRange, vals[0], vals[1:]...))
			}
		case "job-sheets":
			parts := splitList(v, 2)
			if len(parts) > 0 {
				vals := make([]goipp.Value, 0, len(parts))
				for _, p := range parts {
					vals = append(vals, goipp.String(p))
				}
				out = append(out, goipp.MakeAttr(lk, goipp.TagName, vals[0], vals[1:]...))
			}
		case "printer-resolution":
			if res, ok := parseResolution(v); ok {
				out = append(out, goipp.MakeAttribute(lk, goipp.TagResolution, res))
			}
		default:
			if !isIPPJobTemplateKey(lk) {
				continue
			}
			out = append(out, goipp.MakeAttribute(lk, goipp.TagKeyword, goipp.String(v)))
		}
	}
	return out
}

func isIPPJobTemplateKey(name string) bool {
	switch name {
	case "media", "media-source", "media-type", "sides",
		"print-color-mode", "output-bin",
		"multiple-document-handling", "page-delivery", "print-scaling",
		"number-up-layout", "print-as-raster",
		"job-hold-until", "job-account-id", "job-accounting-user-id", "job-password",
		"confirmation-sheet-print", "cover-sheet-info":
		return true
	default:
		return false
	}
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
