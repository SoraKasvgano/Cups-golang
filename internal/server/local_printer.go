package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"
	"github.com/hashicorp/mdns"
)

func resolveMDNSURI(ctx context.Context, deviceURI string) (string, error) {
	deviceURI = strings.TrimSpace(deviceURI)
	if deviceURI == "" {
		return "", errors.New("empty device URI")
	}
	u, err := url.Parse(deviceURI)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", fmt.Errorf("invalid URI %q", deviceURI)
	}

	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		// dnssd:// URIs can end up in Opaque depending on how they are formed.
		host = strings.TrimSpace(strings.TrimPrefix(u.Opaque, "//"))
		if host == "" {
			host = strings.TrimSpace(u.Host)
		}
	}
	if host == "" {
		return deviceURI, nil
	}
	host, _ = url.PathUnescape(host)
	host = strings.TrimSuffix(host, ".")
	lower := strings.ToLower(host)

	services := []string{
		"_ipp-tls._tcp",
		"_ipps._tcp",
		"_ipp._tcp",
		"_printer._tcp",
		"_pdl-datastream._tcp",
	}
	instance := ""
	service := ""
	domain := ""
	for _, svc := range services {
		if idx := strings.Index(lower, svc); idx >= 0 {
			instance = strings.TrimSuffix(host[:idx], ".")
			service = svc
			domain = strings.TrimPrefix(host[idx+len(svc):], ".")
			break
		}
	}
	if service == "" {
		return deviceURI, nil
	}
	if domain == "" {
		domain = "local"
	}

	if ctx == nil {
		ctx = context.Background()
	}

	timeout := 3 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	entries := make(chan *mdns.ServiceEntry, 64)
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		_ = mdns.Query(&mdns.QueryParam{
			Service: service,
			Domain:  domain,
			Timeout: timeout,
			Entries: entries,
		})
		close(entries)
	}()

	var chosen *mdns.ServiceEntry
	for {
		select {
		case <-qctx.Done():
			if chosen == nil {
				return "", fmt.Errorf("couldn't resolve mDNS URI %q", deviceURI)
			}
			return resolvedDeviceURIFromMDNSEntry(service, chosen), nil
		case entry, ok := <-entries:
			if !ok {
				if chosen == nil {
					return "", fmt.Errorf("couldn't resolve mDNS URI %q", deviceURI)
				}
				return resolvedDeviceURIFromMDNSEntry(service, chosen), nil
			}
			if entry == nil {
				continue
			}
			if instance != "" && !strings.EqualFold(entry.Name, instance) {
				continue
			}
			chosen = entry
			if instance != "" {
				return resolvedDeviceURIFromMDNSEntry(service, chosen), nil
			}
		}
	}
}

func resolvedDeviceURIFromMDNSEntry(service string, entry *mdns.ServiceEntry) string {
	if entry == nil {
		return ""
	}
	host := strings.TrimSuffix(strings.TrimSpace(entry.Host), ".")
	if host == "" && entry.AddrV4 != nil {
		host = entry.AddrV4.String()
	} else if host == "" && entry.AddrV6 != nil {
		host = entry.AddrV6.String()
	}
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	txt := parseTxtRecords(entry.InfoFields)
	return buildDNSSDURI(service, host, entry.Port, entry.Name, txt)
}

func ippTransportURL(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", fmt.Errorf("invalid URI %q", uri)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	case "http", "https":
		// Keep.
	default:
		return "", fmt.Errorf("unsupported IPP scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func httpClientForIPPDeviceURI(deviceURI string) *http.Client {
	insecure := strings.ToLower(os.Getenv("CUPS_IPP_INSECURE"))
	skipVerify := insecure == "1" || insecure == "true" || insecure == "yes" || insecure == "on"
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if skipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
}

func doIPPRequest(ctx context.Context, deviceURI string, version goipp.Version, requested []string) (*goipp.Message, error) {
	httpURL, err := ippTransportURL(deviceURI)
	if err != nil {
		return nil, err
	}
	req := goipp.NewRequest(version, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(deviceURI)))
	if len(requested) > 0 {
		vals := make([]goipp.Value, 0, len(requested))
		for _, r := range requested {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			vals = append(vals, goipp.String(r))
		}
		if len(vals) > 0 {
			req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, vals[0], vals[1:]...))
		}
	}

	payload, err := req.EncodeBytes()
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	httpReq.Header.Set("Accept", goipp.ContentType)

	client := httpClientForIPPDeviceURI(deviceURI)
	resp, err := client.Do(httpReq)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %s", resp.Status)
	}
	out := &goipp.Message{}
	if err := out.Decode(resp.Body); err != nil {
		return nil, err
	}
	return out, nil
}

func getPrinterAttributesForLocalQueue(ctx context.Context, deviceURI string) (*goipp.Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := doIPPRequest(ctx, deviceURI, goipp.MakeVersion(2, 0), []string{"all", "media-col-database"})
	if err != nil {
		return nil, err
	}
	status := goipp.Status(resp.Code)
	if status == goipp.StatusErrorBadRequest || status == goipp.StatusErrorVersionNotSupported {
		// Fallback to IPP/1.1 for older servers/printers.
		resp, err = doIPPRequest(ctx, deviceURI, goipp.MakeVersion(1, 1), []string{"all"})
		if err != nil {
			return nil, err
		}
	}

	// Some printers cannot handle requested-attributes containing both "all" and
	// "media-col-database". Fetch it separately and merge when missing.
	if attrByName(resp.Printer, "media-col-database") == nil {
		if resp2, err2 := doIPPRequest(ctx, deviceURI, goipp.MakeVersion(2, 0), []string{"media-col-database"}); err2 == nil && resp2 != nil {
			if a := attrByName(resp2.Printer, "media-col-database"); a != nil && len(a.Values) > 0 {
				attr := *a
				resp.Printer = removeAttr(resp.Printer, "media-col-database")
				resp.Printer.Add(attr)
			}
		}
	}

	// CUPS requires one of these attributes to be present to generate the PPD.
	if attrByName(resp.Printer, "media-col-database") == nil && attrByName(resp.Printer, "media-supported") == nil && attrByName(resp.Printer, "media-size-supported") == nil {
		return nil, fmt.Errorf("the printer does not provide attributes required for IPP Everywhere")
	}
	return resp, nil
}

func removeAttr(attrs goipp.Attributes, name string) goipp.Attributes {
	if len(attrs) == 0 || name == "" {
		return attrs
	}
	out := make(goipp.Attributes, 0, len(attrs))
	for _, a := range attrs {
		if a.Name == name {
			continue
		}
		out = append(out, a)
	}
	return out
}

type ippPPDSize struct {
	Name          string
	Width, Length int
	Left, Bottom  int
	Right, Top    int
}

var cupsMediaSourceIndex = map[string]int{
	"auto":           0,
	"main":           1,
	"alternate":      2,
	"large-capacity": 3,
	"manual":         4,
	"envelope":       5,
	"disc":           6,
	"photo":          7,
	"hagaki":         8,
	"main-roll":      9,
	"alternate-roll": 10,
	"top":            11,
	"middle":         12,
	"bottom":         13,
	"side":           14,
	"left":           15,
	"right":          16,
	"center":         17,
	"rear":           18,
	"by-pass-tray":   19,
	"tray-1":         20,
	"tray-2":         21,
	"tray-3":         22,
	"tray-4":         23,
	"tray-5":         24,
	"tray-6":         25,
	"tray-7":         26,
	"tray-8":         27,
	"tray-9":         28,
	"tray-10":        29,
	"tray-11":        30,
	"tray-12":        31,
	"tray-13":        32,
	"tray-14":        33,
	"tray-15":        34,
	"tray-16":        35,
	"tray-17":        36,
	"tray-18":        37,
	"tray-19":        38,
	"tray-20":        39,
	"roll-1":         40,
	"roll-2":         41,
	"roll-3":         42,
	"roll-4":         43,
	"roll-5":         44,
	"roll-6":         45,
	"roll-7":         46,
	"roll-8":         47,
	"roll-9":         48,
	"roll-10":        49,
}

func extractPPDSizes(supported *goipp.Message) ([]ippPPDSize, string) {
	if supported == nil {
		return nil, ""
	}
	seen := map[string]bool{}
	sizes := []ippPPDSize{}
	add := func(name string, w, l, left, bottom, right, top int) {
		name = strings.TrimSpace(name)
		if name == "" || w <= 0 || l <= 0 {
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		sizes = append(sizes, ippPPDSize{
			Name:   name,
			Width:  w,
			Length: l,
			Left:   maxInt(left, 0),
			Bottom: maxInt(bottom, 0),
			Right:  maxInt(right, 0),
			Top:    maxInt(top, 0),
		})
	}

	if a := attrByName(supported.Printer, "media-col-database"); a != nil {
		for _, v := range a.Values {
			col, ok := v.V.(goipp.Collection)
			if !ok {
				continue
			}
			sizeCol, ok := collectionCollection(col, "media-size")
			if !ok {
				continue
			}
			x := collectionInt(sizeCol, "x-dimension")
			y := collectionInt(sizeCol, "y-dimension")
			if x <= 0 || y <= 0 {
				continue
			}
			name := lookupMediaPPDNameByDims(x, y)
			if name == "" {
				name = mediaNameFromDimensions(x, y)
			}
			if name == "" {
				name = customMediaNameFromDimensions(x, y)
			}
			add(name, x, y,
				collectionInt(col, "media-left-margin"),
				collectionInt(col, "media-bottom-margin"),
				collectionInt(col, "media-right-margin"),
				collectionInt(col, "media-top-margin"),
			)
		}
	}

	if len(sizes) == 0 {
		if a := attrByName(supported.Printer, "media-size-supported"); a != nil {
			for _, v := range a.Values {
				col, ok := v.V.(goipp.Collection)
				if !ok {
					continue
				}
				x := collectionInt(col, "x-dimension")
				y := collectionInt(col, "y-dimension")
				if x <= 0 || y <= 0 {
					continue
				}
				name := lookupMediaPPDNameByDims(x, y)
				if name == "" {
					name = mediaNameFromDimensions(x, y)
				}
				if name == "" {
					name = customMediaNameFromDimensions(x, y)
				}
				add(name, x, y, 0, 0, 0, 0)
			}
		}
	}

	if len(sizes) == 0 {
		if a := attrByName(supported.Printer, "media-supported"); a != nil {
			for _, v := range a.Values {
				media := strings.TrimSpace(v.V.String())
				if media == "" {
					continue
				}
				if sz, ok := lookupMediaSize(media); ok {
					add(media, sz.X, sz.Y, 0, 0, 0, 0)
				} else if sz, ok := parseCustomMediaSize(media); ok {
					add(media, sz.X, sz.Y, 0, 0, 0, 0)
				}
			}
		}
	}

	def := strings.TrimSpace(attrString(supported.Printer, "media-default"))
	if def == "" {
		if a := attrByName(supported.Printer, "media-col-default"); a != nil && len(a.Values) > 0 {
			if col, ok := a.Values[0].V.(goipp.Collection); ok {
				if sizeCol, ok := collectionCollection(col, "media-size"); ok {
					x := collectionInt(sizeCol, "x-dimension")
					y := collectionInt(sizeCol, "y-dimension")
					if x > 0 && y > 0 {
						def = lookupMediaPPDNameByDims(x, y)
						if def == "" {
							def = mediaNameFromDimensions(x, y)
						}
						if def == "" {
							def = customMediaNameFromDimensions(x, y)
						}
					}
				}
			}
		}
	}
	if def != "" {
		for _, s := range sizes {
			if strings.EqualFold(s.Name, def) {
				def = s.Name
				break
			}
		}
	}
	if def == "" && len(sizes) > 0 {
		def = sizes[0].Name
	}

	sort.SliceStable(sizes, func(i, j int) bool {
		if strings.EqualFold(sizes[i].Name, def) {
			return true
		}
		if strings.EqualFold(sizes[j].Name, def) {
			return false
		}
		return strings.ToLower(sizes[i].Name) < strings.ToLower(sizes[j].Name)
	})
	return sizes, def
}

func generatePPDFromIPP(ppdDir string, printerName string, supported *goipp.Message) (string, error) {
	if supported == nil {
		return "", errors.New("no IPP attributes")
	}
	if strings.TrimSpace(printerName) == "" {
		return "", errors.New("empty printer name")
	}

	makeModel := strings.TrimSpace(attrString(supported.Printer, "printer-make-and-model"))
	if makeModel == "" {
		makeModel = "Unknown"
	}
	makeVal, modelVal := splitMakeModel(makeModel)
	makeVal = sanitizePPDString(makeVal)
	modelVal = sanitizePPDString(modelVal)
	if makeVal == "" {
		makeVal = "Unknown"
	}
	if modelVal == "" {
		modelVal = "Printer"
	}

	colorDevice := attrBool(supported.Printer, "color-supported")

	sizes, defSize := extractPPDSizes(supported)
	if len(sizes) == 0 || defSize == "" {
		return "", fmt.Errorf("no media sizes in IPP response")
	}

	ppdName := printerName + ".ppd"
	ppdPath := safePPDPath(ppdDir, ppdName)
	if err := os.MkdirAll(filepath.Dir(ppdPath), 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(filepath.Dir(ppdPath), "ippeve-*.ppd")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	writeLine := func(format string, args ...any) error {
		if len(args) == 0 {
			_, err := io.WriteString(tmp, format+"\n")
			return err
		}
		_, err := fmt.Fprintf(tmp, format+"\n", args...)
		return err
	}

	if err := writeLine("*PPD-Adobe: \"4.3\""); err != nil {
		return "", err
	}
	_ = writeLine("*FormatVersion: \"4.3\"")
	_ = writeLine("*FileVersion: \"2.4\"")
	_ = writeLine("*LanguageVersion: English")
	_ = writeLine("*LanguageEncoding: ISOLatin1")
	_ = writeLine("*PSVersion: \"(3010.000) 0\"")
	_ = writeLine("*LanguageLevel: \"3\"")
	_ = writeLine("*FileSystem: False")
	_ = writeLine("*PCFileName: \"%s\"", "ippeve.ppd")
	_ = writeLine("*Manufacturer: \"%s\"", escapePPDString(makeVal))
	_ = writeLine("*ModelName: \"%s\"", escapePPDString(modelVal))
	_ = writeLine("*Product: \"(%s)\"", escapePPDString(modelVal))
	_ = writeLine("*NickName: \"%s - IPP Everywhere\"", escapePPDString(modelVal))
	_ = writeLine("*ShortNickName: \"%s - IPP Everywhere\"", escapePPDString(modelVal))
	if colorDevice {
		_ = writeLine("*ColorDevice: True")
	} else {
		_ = writeLine("*ColorDevice: False")
	}
	_ = writeLine("*cupsVersion: 2.4")
	_ = writeLine("*cupsSNMPSupplies: False")

	// Filters...
	formats := attrStrings(supported.Printer, "document-format-supported")
	hasFormat := func(needle string) bool {
		for _, v := range formats {
			if v == needle {
				return true
			}
		}
		return false
	}
	isApple := hasFormat("image/urf") && attrByName(supported.Printer, "urf-supported") != nil
	isPDF := hasFormat("application/pdf")
	isPWG := hasFormat("image/pwg-raster") && !isApple &&
		attrByName(supported.Printer, "pwg-raster-document-resolution-supported") != nil &&
		attrByName(supported.Printer, "pwg-raster-document-type-supported") != nil

	if hasFormat("image/jpeg") {
		_ = writeLine("*cupsFilter2: \"image/jpeg image/jpeg 0 -\"")
	}
	if hasFormat("image/png") {
		_ = writeLine("*cupsFilter2: \"image/png image/png 0 -\"")
	}
	if isPDF {
		// Don't locally filter PDF content when printing to a CUPS shared printer.
		if hasFormat("application/vnd.cups-pdf") {
			_ = writeLine("*cupsFilter2: \"application/pdf application/pdf 0 -\"")
		} else {
			_ = writeLine("*cupsFilter2: \"application/vnd.cups-pdf application/pdf 10 -\"")
		}
	} else {
		_ = writeLine("*cupsManualCopies: True")
	}
	if isApple {
		_ = writeLine("*cupsFilter2: \"image/urf image/urf 100 -\"")
	}
	if isPWG {
		_ = writeLine("*cupsFilter2: \"image/pwg-raster image/pwg-raster 100 -\"")
	}
	if !isApple && !isPDF && !isPWG {
		return "", fmt.Errorf("the printer does not provide attributes required for IPP Everywhere")
	}

	// Hardware margins and custom size ranges (for *HWMargins and *CustomPageSize).
	maxSupported := func(name string, fallback int) int {
		attr := attrByName(supported.Printer, name)
		if attr == nil || len(attr.Values) == 0 {
			return fallback
		}
		maxv := -1
		for _, v := range attr.Values {
			if n, ok := v.V.(goipp.Integer); ok {
				if int(n) > maxv {
					maxv = int(n)
				}
			}
		}
		if maxv < 0 {
			return fallback
		}
		return maxv
	}

	hwBottom := maxSupported("media-bottom-margin-supported", 1270)
	hwLeft := maxSupported("media-left-margin-supported", 635)
	hwRight := maxSupported("media-right-margin-supported", 635)
	hwTop := maxSupported("media-top-margin-supported", 1270)

	minWidth := math.MaxInt
	maxWidth := 0
	minLength := math.MaxInt
	maxLength := 0
	updateRange := func(lower, upper int, width bool) {
		if lower <= 0 || upper <= 0 {
			return
		}
		if width {
			if lower < minWidth {
				minWidth = lower
			}
			if upper > maxWidth {
				maxWidth = upper
			}
		} else {
			if lower < minLength {
				minLength = lower
			}
			if upper > maxLength {
				maxLength = upper
			}
		}
	}

	if a := attrByName(supported.Printer, "media-col-database"); a != nil {
		for _, v := range a.Values {
			col, ok := v.V.(goipp.Collection)
			if !ok {
				continue
			}
			sizeCol, ok := collectionCollection(col, "media-size")
			if !ok {
				continue
			}
			xLower, xUpper, xRange, okX := collectionIntOrRange(sizeCol, "x-dimension")
			yLower, yUpper, yRange, okY := collectionIntOrRange(sizeCol, "y-dimension")
			if !okX || !okY {
				continue
			}
			if xRange || yRange {
				updateRange(xLower, xUpper, true)
				updateRange(yLower, yUpper, false)
			}
		}

		// Some printers don't list custom size support in media-col-database...
		if (maxWidth == 0 || maxLength == 0) && attrByName(supported.Printer, "media-size-supported") != nil {
			if a2 := attrByName(supported.Printer, "media-size-supported"); a2 != nil {
				for _, v := range a2.Values {
					col, ok := v.V.(goipp.Collection)
					if !ok {
						continue
					}
					xLower, xUpper, xRange, okX := collectionIntOrRange(col, "x-dimension")
					yLower, yUpper, yRange, okY := collectionIntOrRange(col, "y-dimension")
					if !okX || !okY {
						continue
					}
					if xRange || yRange {
						updateRange(xLower, xUpper, true)
						updateRange(yLower, yUpper, false)
					}
				}
			}
		}
	} else if a := attrByName(supported.Printer, "media-size-supported"); a != nil {
		for _, v := range a.Values {
			col, ok := v.V.(goipp.Collection)
			if !ok {
				continue
			}
			xLower, xUpper, xRange, okX := collectionIntOrRange(col, "x-dimension")
			yLower, yUpper, yRange, okY := collectionIntOrRange(col, "y-dimension")
			if !okX || !okY {
				continue
			}
			if xRange || yRange {
				updateRange(xLower, xUpper, true)
				updateRange(yLower, yUpper, false)
			}
		}
	} else if a := attrByName(supported.Printer, "media-supported"); a != nil {
		for _, v := range a.Values {
			pwgSize := strings.TrimSpace(v.V.String())
			if pwgSize == "" {
				continue
			}
			sz, ok := lookupMediaSize(pwgSize)
			if !ok {
				continue
			}
			if strings.Contains(pwgSize, "_max_") || strings.Contains(pwgSize, "_max.") {
				if sz.X > maxWidth {
					maxWidth = sz.X
				}
				if sz.Y > maxLength {
					maxLength = sz.Y
				}
			} else if strings.Contains(pwgSize, "_min_") || strings.Contains(pwgSize, "_min.") {
				if sz.X < minWidth {
					minWidth = sz.X
				}
				if sz.Y < minLength {
					minLength = sz.Y
				}
			}
		}
	}

	// Page sizes.
	_ = writeLine("*OpenUI *PageSize: PickOne")
	_ = writeLine("*OrderDependency: 10 AnySetup *PageSize")
	_ = writeLine("*DefaultPageSize: %s", defSize)
	for _, sz := range sizes {
		wpt := hundredthMMToPoints(sz.Width)
		lpt := hundredthMMToPoints(sz.Length)
		_ = writeLine("*PageSize %s: \"<</PageSize[%s %s]>>setpagedevice\"", sz.Name, formatPPDNumber(wpt), formatPPDNumber(lpt))
	}
	_ = writeLine("*CloseUI: *PageSize")

	_ = writeLine("*OpenUI *PageRegion: PickOne")
	_ = writeLine("*OrderDependency: 10 AnySetup *PageRegion")
	_ = writeLine("*DefaultPageRegion: %s", defSize)
	for _, sz := range sizes {
		wpt := hundredthMMToPoints(sz.Width)
		lpt := hundredthMMToPoints(sz.Length)
		_ = writeLine("*PageRegion %s: \"<</PageSize[%s %s]>>setpagedevice\"", sz.Name, formatPPDNumber(wpt), formatPPDNumber(lpt))
	}
	_ = writeLine("*CloseUI: *PageRegion")

	_ = writeLine("*DefaultImageableArea: %s", defSize)
	_ = writeLine("*DefaultPaperDimension: %s", defSize)
	for _, sz := range sizes {
		wpt := hundredthMMToPoints(sz.Width)
		lpt := hundredthMMToPoints(sz.Length)
		left := hundredthMMToPoints(sz.Left)
		bottom := hundredthMMToPoints(sz.Bottom)
		rightCoord := wpt - hundredthMMToPoints(sz.Right)
		topCoord := lpt - hundredthMMToPoints(sz.Top)
		_ = writeLine("*ImageableArea %s: \"%s %s %s %s\"", sz.Name, formatPPDNumber(left), formatPPDNumber(bottom), formatPPDNumber(rightCoord), formatPPDNumber(topCoord))
		_ = writeLine("*PaperDimension %s: \"%s %s\"", sz.Name, formatPPDNumber(wpt), formatPPDNumber(lpt))
	}

	if maxWidth > 0 && minWidth < math.MaxInt && maxLength > 0 && minLength < math.MaxInt {
		_ = writeLine("*HWMargins: \"%s %s %s %s\"",
			formatPPDNumber(hundredthMMToPoints(hwLeft)),
			formatPPDNumber(hundredthMMToPoints(hwBottom)),
			formatPPDNumber(hundredthMMToPoints(hwRight)),
			formatPPDNumber(hundredthMMToPoints(hwTop)),
		)
		_ = writeLine("*ParamCustomPageSize Width: 1 points %s %s",
			formatPPDNumber(hundredthMMToPoints(minWidth)),
			formatPPDNumber(hundredthMMToPoints(maxWidth)),
		)
		_ = writeLine("*ParamCustomPageSize Height: 2 points %s %s",
			formatPPDNumber(hundredthMMToPoints(minLength)),
			formatPPDNumber(hundredthMMToPoints(maxLength)),
		)
		_ = writeLine("*ParamCustomPageSize WidthOffset: 3 points 0 0")
		_ = writeLine("*ParamCustomPageSize HeightOffset: 4 points 0 0")
		_ = writeLine("*ParamCustomPageSize Orientation: 5 int 0 3")
		_ = writeLine("*CustomPageSize True: \"pop pop pop <</PageSize[5 -2 roll]/ImagingBBox null>>setpagedevice\"")
	}

	// Duplex support.
	sides := attrStrings(supported.Printer, "sides-supported")
	hasLong := stringInList("two-sided-long-edge", sides)
	hasShort := stringInList("two-sided-short-edge", sides)
	if hasLong || hasShort {
		defSides := strings.TrimSpace(attrString(supported.Printer, "sides-default"))
		defDuplex := "None"
		if strings.EqualFold(defSides, "two-sided-short-edge") {
			defDuplex = "DuplexTumble"
		} else if strings.EqualFold(defSides, "two-sided-long-edge") {
			defDuplex = "DuplexNoTumble"
		}
		_ = writeLine("*OpenUI *Duplex/Duplex: PickOne")
		_ = writeLine("*DefaultDuplex: %s", defDuplex)
		_ = writeLine("*Duplex None/Off: \"<</Duplex false>>setpagedevice\"")
		if hasLong {
			_ = writeLine("*Duplex DuplexNoTumble/Long Edge: \"<</Duplex true /Tumble false>>setpagedevice\"")
		}
		if hasShort {
			_ = writeLine("*Duplex DuplexTumble/Short Edge: \"<</Duplex true /Tumble true>>setpagedevice\"")
		}
		_ = writeLine("*CloseUI: *Duplex")
	}

	// Resolution support.
	resList, defRes := extractIPPResolutions(supported)
	if len(resList) > 0 {
		_ = writeLine("*OpenUI *Resolution/Resolution: PickOne")
		if defRes != "" {
			_ = writeLine("*DefaultResolution: %s", defRes)
		}
		for _, res := range resList {
			choice := res
			label := strings.TrimSuffix(choice, "dpi") + " dpi"
			parsed, _ := parseResolution(choice)
			_ = writeLine("*Resolution %s/%s: \"<</HWResolution[%d %d]>>setpagedevice\"", choice, label, parsed.Xres, parsed.Yres)
		}
		_ = writeLine("*CloseUI: *Resolution")
	}

	// Media source/type/output bin.
	addKeywordPickOne(tmp, "InputSlot", "Input Slot", "media-source-supported", "media-source-default", supported)
	addKeywordPickOne(tmp, "MediaType", "Media Type", "media-type-supported", "media-type-default", supported)
	addKeywordPickOne(tmp, "OutputBin", "Output Bin", "output-bin-supported", "output-bin-default", supported)

	// Print quality.
	qualities, defQuality := extractIPPPrintQualities(supported)
	if len(qualities) > 0 {
		_ = writeLine("*OpenUI *cupsPrintQuality/Print Quality: PickOne")
		if defQuality != "" {
			_ = writeLine("*DefaultcupsPrintQuality: %s", defQuality)
		}
		for _, q := range qualities {
			_ = writeLine("*cupsPrintQuality %s/%s: \"\"", q, q)
		}
		_ = writeLine("*CloseUI: *cupsPrintQuality")
	}

	// Finishing templates.
	templates, defTemplate := extractIPPFinishingTemplates(supported)
	if len(templates) > 0 {
		_ = writeLine("*OpenUI *cupsFinishingTemplate/Finishing Template: PickOne")
		if defTemplate != "" {
			_ = writeLine("*DefaultcupsFinishingTemplate: %s", defTemplate)
		}
		for _, t := range templates {
			_ = writeLine("*cupsFinishingTemplate %s/%s: \"\"", t, t)
		}
		_ = writeLine("*CloseUI: *cupsFinishingTemplate")
	}

	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, ppdPath); err != nil {
		return "", err
	}
	return filepath.Base(ppdPath), nil
}

func splitMakeModel(makeModel string) (string, string) {
	makeModel = strings.TrimSpace(makeModel)
	if makeModel == "" {
		return "", ""
	}
	if strings.HasPrefix(strings.ToLower(makeModel), "hewlett packard ") || strings.HasPrefix(strings.ToLower(makeModel), "hewlett-packard ") {
		model := strings.TrimSpace(makeModel[16:])
		model = strings.TrimPrefix(model, "HP ")
		model = strings.TrimPrefix(model, "hp ")
		return "HP", strings.TrimSpace(model)
	}
	if idx := strings.Index(makeModel, " "); idx > 0 {
		makeVal := strings.TrimSpace(makeModel[:idx])
		modelVal := strings.TrimSpace(makeModel[idx+1:])
		return makeVal, modelVal
	}
	return makeModel, "Printer"
}

func sanitizePPDString(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < ' ' || ch >= 127 || ch == '"' {
			break
		}
		b.WriteByte(ch)
	}
	out := strings.TrimSpace(b.String())
	return out
}

func escapePPDString(s string) string {
	return strings.ReplaceAll(s, "\"", "")
}

func hundredthMMToPoints(v int) float64 {
	if v <= 0 {
		return 0
	}
	return float64(v) * 72.0 / 2540.0
}

func formatPPDNumber(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	// Use a stable ASCII representation.
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" {
		return "0"
	}
	return s
}

func maxInt(v, floor int) int {
	if v < floor {
		return floor
	}
	return v
}

func extractIPPResolutions(supported *goipp.Message) ([]string, string) {
	if supported == nil {
		return nil, ""
	}
	out := []string{}
	seen := map[string]bool{}
	for _, attr := range supported.Printer {
		if attr.Name != "printer-resolution-supported" && attr.Name != "pwg-raster-document-resolution-supported" {
			continue
		}
		for _, v := range attr.Values {
			res, ok := v.V.(goipp.Resolution)
			if !ok {
				continue
			}
			choice := resolutionChoice(res)
			if choice == "" {
				continue
			}
			key := strings.ToLower(choice)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, choice)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, _ := parseResolution(out[i])
		rj, _ := parseResolution(out[j])
		if ri.Xres == rj.Xres {
			return ri.Yres < rj.Yres
		}
		return ri.Xres < rj.Xres
	})

	def := ""
	if a := attrByName(supported.Printer, "printer-resolution-default"); a != nil && len(a.Values) > 0 {
		if res, ok := a.Values[0].V.(goipp.Resolution); ok {
			def = resolutionChoice(res)
		}
	}
	if def == "" && len(out) > 0 {
		def = out[0]
	}
	return out, def
}

func resolutionChoice(res goipp.Resolution) string {
	if res.Xres <= 0 || res.Yres <= 0 {
		return ""
	}
	if res.Xres == res.Yres {
		return strconv.Itoa(res.Xres) + "dpi"
	}
	return strconv.Itoa(res.Xres) + "x" + strconv.Itoa(res.Yres) + "dpi"
}

func addKeywordPickOne(dst io.Writer, optionKeyword, optionText, supportedAttr, defaultAttr string, supported *goipp.Message) {
	if supported == nil || dst == nil {
		return
	}
	values := []string{}
	seen := map[string]bool{}
	for _, v := range attrStrings(supported.Printer, supportedAttr) {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		values = append(values, v)
	}
	if len(values) == 0 {
		return
	}
	sort.SliceStable(values, func(i, j int) bool { return strings.ToLower(values[i]) < strings.ToLower(values[j]) })
	def := strings.TrimSpace(attrString(supported.Printer, defaultAttr))
	if def == "" {
		def = values[0]
	}
	// Keep default value stable with actual supported values.
	for _, v := range values {
		if strings.EqualFold(v, def) {
			def = v
			break
		}
	}

	fmt.Fprintf(dst, "*OpenUI *%s/%s: PickOne\n", optionKeyword, optionText)
	fmt.Fprintf(dst, "*Default%s: %s\n", optionKeyword, def)
	for _, v := range values {
		fmt.Fprintf(dst, "*%s %s/%s: \"\"\n", optionKeyword, v, v)
	}
	fmt.Fprintf(dst, "*CloseUI: *%s\n", optionKeyword)
}

func extractIPPPrintQualities(supported *goipp.Message) ([]string, string) {
	if supported == nil {
		return nil, ""
	}
	enums := []int{}
	seen := map[int]bool{}
	if a := attrByName(supported.Printer, "print-quality-supported"); a != nil {
		for _, v := range a.Values {
			if n, ok := v.V.(goipp.Integer); ok {
				val := int(n)
				if seen[val] {
					continue
				}
				seen[val] = true
				enums = append(enums, val)
			}
		}
	}
	sort.Ints(enums)

	// Map IPP enum to common PPD-ish names used by our parser.
	out := []string{}
	for _, n := range enums {
		switch n {
		case 3:
			out = append(out, "Draft")
		case 4:
			out = append(out, "Normal")
		case 5:
			out = append(out, "High")
		}
	}
	def := ""
	if a := attrByName(supported.Printer, "print-quality-default"); a != nil && len(a.Values) > 0 {
		if n, ok := a.Values[0].V.(goipp.Integer); ok {
			switch int(n) {
			case 3:
				def = "Draft"
			case 4:
				def = "Normal"
			case 5:
				def = "High"
			}
		}
	}
	if def == "" && len(out) > 0 {
		def = out[0]
	}
	return out, def
}

func extractIPPFinishingTemplates(supported *goipp.Message) ([]string, string) {
	if supported == nil {
		return nil, ""
	}
	templates := []string{}
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if seen[key] {
			return
		}
		seen[key] = true
		templates = append(templates, v)
	}
	for _, v := range attrStrings(supported.Printer, "finishing-template-supported") {
		add(v)
	}
	if a := attrByName(supported.Printer, "finishings-col-database"); a != nil {
		for _, val := range a.Values {
			col, ok := val.V.(goipp.Collection)
			if !ok {
				continue
			}
			if t := collectionString(col, "finishing-template"); t != "" {
				add(t)
			}
		}
	}
	if len(templates) == 0 {
		return nil, ""
	}
	sort.SliceStable(templates, func(i, j int) bool { return strings.ToLower(templates[i]) < strings.ToLower(templates[j]) })
	if !seen["none"] {
		templates = append([]string{"none"}, templates...)
	}
	def := strings.TrimSpace(attrString(supported.Printer, "finishing-template-default"))
	if def == "" {
		def = "none"
	}
	for _, v := range templates {
		if strings.EqualFold(v, def) {
			def = v
			break
		}
	}
	return templates, def
}

func collectionIntOrRange(col goipp.Collection, name string) (int, int, bool, bool) {
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		switch v := attr.Values[0].V.(type) {
		case goipp.Integer:
			n := int(v)
			return n, n, false, true
		case goipp.Range:
			return v.Lower, v.Upper, true, true
		default:
			return 0, 0, false, false
		}
	}
	return 0, 0, false, false
}

// pwgPPDizeName implements the same transformation as CUPS pwg_ppdize_name().
func pwgPPDizeName(ipp string) string {
	ipp = strings.TrimSpace(ipp)
	if ipp == "" {
		return ""
	}
	if !isASCIIAlnum(ipp[0]) {
		return ""
	}
	var b strings.Builder
	b.Grow(len(ipp))
	b.WriteByte(toUpperASCII(ipp[0]))
	for i := 1; i < len(ipp); {
		ch := ipp[i]
		if ch == '-' && i+1 < len(ipp) && isASCIIAlnum(ipp[i+1]) {
			i++
			b.WriteByte(toUpperASCII(ipp[i]))
			i++
			continue
		}
		if ch == '_' || ch == '.' || ch == '-' || isASCIIAlnum(ch) {
			b.WriteByte(ch)
		}
		i++
	}
	return b.String()
}

func isASCIIAlnum(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func toUpperASCII(ch byte) byte {
	if ch >= 'a' && ch <= 'z' {
		return ch - ('a' - 'A')
	}
	return ch
}

func parseURFResolution(vals []string) (int, int) {
	for _, v := range vals {
		rs := strings.TrimSpace(v)
		if len(rs) < 3 {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(rs), "RS") {
			i := 2
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				i++
			}
			if i == 2 {
				continue
			}
			lowdpi, _ := strconv.Atoi(rs[2:i])
			if lowdpi <= 0 {
				continue
			}
			hidpi := lowdpi
			if idx := strings.LastIndex(rs, "-"); idx != -1 && idx+1 < len(rs) {
				j := idx + 1
				k := j
				for k < len(rs) && rs[k] >= '0' && rs[k] <= '9' {
					k++
				}
				if k > j {
					if hi, err := strconv.Atoi(rs[j:k]); err == nil && hi > 0 {
						hidpi = hi
					}
				}
			}
			return lowdpi, hidpi
		}
	}
	return 0, 0
}

func resToDPI(res goipp.Resolution) (int, int) {
	xres := res.Xres
	yres := res.Yres
	if res.Units == goipp.UnitsDpcm {
		xres = int(float64(xres) * 2.54)
		yres = int(float64(yres) * 2.54)
	}
	return xres, yres
}
