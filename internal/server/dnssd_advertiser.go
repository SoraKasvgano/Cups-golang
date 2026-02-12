package server

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

// DNSSDAdvertiser provides a CUPS-like DNS-SD (mDNS) broadcaster for shared queues.
//
// This is intentionally "best effort": failures should not prevent the server
// from starting.
type DNSSDAdvertiser struct {
	srv    *Server
	zone   *dnssdZone
	server *mdns.Server

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type dnssdZone struct {
	mu       sync.RWMutex
	services []*mdns.MDNSService
}

func (z *dnssdZone) SetServices(services []*mdns.MDNSService) {
	z.mu.Lock()
	z.services = services
	z.mu.Unlock()
}

func (z *dnssdZone) Records(q dns.Question) []dns.RR {
	z.mu.RLock()
	services := append([]*mdns.MDNSService(nil), z.services...)
	z.mu.RUnlock()

	var out []dns.RR
	for _, svc := range services {
		if svc == nil {
			continue
		}
		out = append(out, svc.Records(q)...)
	}
	return out
}

// StartDNSSDAdvertiser starts DNS-SD broadcasting when BrowseLocalProtocols includes "dnssd".
// It returns (nil, nil) when DNS-SD sharing is disabled by config.
func StartDNSSDAdvertiser(ctx context.Context, srv *Server) (*DNSSDAdvertiser, error) {
	if srv == nil || srv.Store == nil {
		return nil, nil
	}
	if !dnssdEnabled(srv.Config) {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	zone := &dnssdZone{}
	mdnsServer, err := mdns.NewServer(&mdns.Config{
		Zone:              zone,
		LogEmptyResponses: false,
	})
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	adv := &DNSSDAdvertiser{
		srv:    srv,
		zone:   zone,
		server: mdnsServer,
		cancel: cancel,
	}
	adv.wg.Add(1)
	go adv.loop(runCtx)
	return adv, nil
}

func (a *DNSSDAdvertiser) Close() {
	if a == nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	if a.server != nil {
		_ = a.server.Shutdown()
	}
}

func (a *DNSSDAdvertiser) loop(ctx context.Context) {
	defer a.wg.Done()

	// Refresh periodically to pick up changes to queues/sharing settings.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	a.refresh(ctx)
	for {
		select {
		case <-ticker.C:
			a.refresh(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *DNSSDAdvertiser) refresh(ctx context.Context) {
	if a == nil || a.srv == nil || a.zone == nil {
		return
	}

	cfg := a.srv.Config
	st := a.srv.Store
	if !dnssdEnabled(cfg) || st == nil {
		a.zone.SetServices(nil)
		return
	}

	shareServer := cfg.BrowseLocal && sharingEnabled(nil, st)
	if !shareServer {
		a.zone.SetServices(nil)
		return
	}

	port := dnssdPort(cfg)
	hostName := dnssdHostName(cfg)
	services := []*mdns.MDNSService{}

	// Web interface service (CUPS BrowseWebIF equivalent).
	if cfg.WebInterface && port > 0 {
		webName := "CUPS"
		if strings.TrimSpace(cfg.DNSSDComputerName) != "" {
			webName = "CUPS @ " + strings.TrimSpace(cfg.DNSSDComputerName)
		}
		if svc, err := mdns.NewMDNSService(webName, "_http._tcp", "local", hostName, port, nil, nil); err == nil {
			services = append(services, svc)
		}
	}

	var printers []model.Printer
	var classes []model.Class
	memberMap := map[int64][]model.Printer{}

	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		printers, err = st.ListPrinters(ctx, tx)
		if err != nil {
			return err
		}
		classes, err = st.ListClasses(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range classes {
			members, err := st.ListClassMembers(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			memberMap[c.ID] = members
		}
		return nil
	})

	// Register printers.
	for _, p := range printers {
		if p.IsTemporary {
			continue
		}
		if !p.Shared {
			continue
		}
		txt := dnssdTxtRecordForPrinter(a.srv, cfg, p, false, shareServer, port)

		instance := dnssdInstanceName(cfg, p.Info, p.Name)
		if svc, err := mdns.NewMDNSService(instance, "_printer._tcp", "local", hostName, 0, nil, nil); err == nil {
			services = append(services, svc)
		}
		if svc, err := mdns.NewMDNSService(instance, "_ipp._tcp", "local", hostName, port, nil, txt); err == nil {
			services = append(services, svc)
		}
		if svc, err := mdns.NewMDNSService(instance, "_ipps._tcp", "local", hostName, port, nil, txt); err == nil {
			services = append(services, svc)
		}
	}

	// Register classes (best-effort, representative from first member).
	for _, c := range classes {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		members := memberMap[c.ID]
		if len(members) == 0 {
			continue
		}
		rep := applyClassDefaultsToPrinter(members[0], c)
		rep.Name = c.Name
		rep.Info = firstNonEmpty(c.Info, rep.Info, c.Name)
		rep.Location = firstNonEmpty(c.Location, rep.Location, "")
		rep.Accepting = c.Accepting
		rep.IsDefault = c.IsDefault
		rep.Shared = shareServer
		rep.State = c.State

		txt := dnssdTxtRecordForPrinter(a.srv, cfg, rep, true, shareServer, port)
		instance := dnssdInstanceName(cfg, rep.Info, c.Name)
		if svc, err := mdns.NewMDNSService(instance, "_printer._tcp", "local", hostName, 0, nil, nil); err == nil {
			services = append(services, svc)
		}
		if svc, err := mdns.NewMDNSService(instance, "_ipp._tcp", "local", hostName, port, nil, txt); err == nil {
			services = append(services, svc)
		}
		if svc, err := mdns.NewMDNSService(instance, "_ipps._tcp", "local", hostName, port, nil, txt); err == nil {
			services = append(services, svc)
		}
	}

	a.zone.SetServices(services)
}

func dnssdEnabled(cfg config.Config) bool {
	if !cfg.BrowseLocal {
		return false
	}
	for _, p := range cfg.BrowseLocalProtocols {
		if strings.EqualFold(strings.TrimSpace(p), "dnssd") {
			return true
		}
	}
	return false
}

func dnssdPort(cfg config.Config) int {
	parsePort := func(addr string) int {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return 0
		}
		// net.SplitHostPort requires a host for ":631", so normalize.
		if strings.HasPrefix(addr, ":") {
			addr = "0.0.0.0" + addr
		}
		_, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return 0
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			return 0
		}
		return port
	}
	for _, addr := range cfg.ListenHTTP {
		if p := parsePort(addr); p > 0 {
			return p
		}
	}
	for _, addr := range cfg.ListenHTTPS {
		if p := parsePort(addr); p > 0 {
			return p
		}
	}
	if p := parsePort(cfg.ListenAddr); p > 0 {
		return p
	}
	return 631
}

func dnssdHostName(cfg config.Config) string {
	host := strings.TrimSpace(cfg.DNSSDHostName)
	if host == "" {
		host = strings.TrimSpace(cfg.ServerName)
	}
	// mdns.NewMDNSService will infer a hostname when blank, but we prefer a stable one.
	if host == "" {
		return ""
	}
	if strings.Contains(host, ".") {
		if !strings.HasSuffix(host, ".") {
			host += "."
		}
		return host
	}
	return host + ".local."
}

func dnssdInstanceName(cfg config.Config, info string, fallback string) string {
	base := strings.TrimSpace(info)
	if base == "" {
		base = strings.TrimSpace(fallback)
	}
	if base == "" {
		base = "Printer"
	}
	if strings.TrimSpace(cfg.DNSSDComputerName) != "" {
		return base + " @ " + strings.TrimSpace(cfg.DNSSDComputerName)
	}
	return base
}

func dnssdTxtRecordForPrinter(srv *Server, cfg config.Config, printer model.Printer, isClass bool, shareServer bool, port int) []string {
	txt := map[string]string{
		"txtvers": "1",
		"qtotal":  "1",
	}
	rp := "printers/" + printer.Name
	if isClass {
		rp = "classes/" + printer.Name
	}
	txt["rp"] = rp

	ppd, _ := loadPPDForPrinter(printer)
	modelName := "Unknown"
	if ppd != nil {
		switch {
		case strings.TrimSpace(ppd.NickName) != "":
			modelName = strings.TrimSpace(ppd.NickName)
		case strings.TrimSpace(ppd.Model) != "":
			modelName = strings.TrimSpace(ppd.Model)
		case strings.TrimSpace(ppd.Make) != "":
			modelName = strings.TrimSpace(ppd.Make)
		}
	}
	txt["ty"] = modelName

	scheme := "http"
	if cfg.TLSEnabled {
		scheme = "https"
	}
	adminHost := strings.TrimSuffix(dnssdHostName(cfg), ".")
	if adminHost == "" {
		adminHost = "localhost"
	}
	txt["adminurl"] = fmt.Sprintf("%s://%s:%d/%s/%s", scheme, adminHost, port, func() string {
		if isClass {
			return "classes"
		}
		return "printers"
	}(), printer.Name)

	if strings.TrimSpace(printer.Location) != "" {
		txt["note"] = strings.TrimSpace(printer.Location)
	}
	txt["priority"] = "0"

	product := "Unknown"
	if ppd != nil && len(ppd.Products) > 0 && strings.TrimSpace(ppd.Products[0]) != "" {
		product = strings.TrimSpace(ppd.Products[0])
	}
	txt["product"] = product

	// CUPS advertises a curated subset of PDLs, but it should reflect what the
	// destination can actually accept with current PPD/filter availability.
	formats := []string{"application/octet-stream", "application/vnd.cups-raw"}
	if !isClass {
		formats = supportedDocumentFormatsForPrinter(printer, ppd)
	}
	txt["pdl"] = strings.Join(dnssdPDLFromFormats(formats), ",")

	// "air" auth-info-required token (negotiate vs username/password).
	if srv != nil {
		authInfo := srv.authInfoRequiredForDestination(isClass, printer.Name, func() string {
			if isClass {
				// For classes we encode the op-policy in default options as well.
				return printer.DefaultOptions
			}
			return printer.DefaultOptions
		}())
		if len(authInfo) == 1 && strings.EqualFold(authInfo[0], "negotiate") {
			txt["air"] = "negotiate"
		} else if len(authInfo) > 0 {
			txt["air"] = "username,password"
		}
		uuid := dnssdUUID(adminHost, port, printer.Name)
		if uuid != "" {
			txt["UUID"] = uuid
		}
	}

	txt["TLS"] = "1.3"

	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)
	urf := urfSupported(caps.resolutions, caps.colorModes, caps.sidesSupported, caps.finishingsSupported, caps.printQualitySupported)
	if len(urf) > 0 {
		txt["URF"] = strings.Join(urf, ",")
	}
	txt["mopria-certified"] = "1.3"

	// Capability booleans are only included when supported, like CUPS.
	authInfo := []string(nil)
	if srv != nil {
		authInfo = srv.authInfoRequiredForDestination(isClass, printer.Name, printer.DefaultOptions)
	}
	ptype := computePrinterType(printer, caps, ppd, false, authInfo)
	if isClass {
		ptype |= cupsPTypeClass
	}
	if shareServer {
		// Match CUPS "remote" bit in TXT: it always ORs CUPS_PTYPE_REMOTE.
		ptype |= cupsPTypeRemote
	}
	txt["printer-type"] = fmt.Sprintf("0x%X", ptype)
	if ptype&cupsPTypeColor != 0 {
		txt["Color"] = "T"
	}
	if ptype&cupsPTypeDuplex != 0 {
		txt["Duplex"] = "T"
	}
	if ptype&cupsPTypeStaple != 0 {
		txt["Staple"] = "T"
	}
	if ptype&cupsPTypeCopies != 0 {
		txt["Copies"] = "T"
	}
	if ptype&cupsPTypePunch != 0 {
		txt["Punch"] = "T"
	}
	if ptype&cupsPTypeBind != 0 {
		txt["Bind"] = "T"
	}

	// Serialize deterministically.
	keys := make([]string, 0, len(txt))
	for k := range txt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := txt[k]
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

func dnssdPDLFromFormats(formats []string) []string {
	formats = normalizeDocumentFormats(formats)
	pdl := make([]string, 0, len(formats))
	for _, mt := range []string{"application/pdf", "application/postscript", "image/jpeg", "image/png", "image/pwg-raster", "image/urf"} {
		if stringInList(mt, formats) {
			pdl = append(pdl, mt)
		}
	}
	if len(pdl) == 0 {
		for _, mt := range formats {
			if strings.EqualFold(mt, "application/octet-stream") || strings.EqualFold(mt, "application/vnd.cups-raw") {
				continue
			}
			pdl = append(pdl, mt)
			if len(pdl) >= 4 {
				break
			}
		}
	}
	if len(pdl) == 0 {
		pdl = []string{"application/octet-stream"}
	}
	return pdl
}

func dnssdUUID(host string, port int, name string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "localhost"
	}
	if port <= 0 {
		port = 631
	}
	urn := assembleUUID(host, port, name, 0, 0, 0)
	urn = strings.TrimSpace(urn)
	urn = strings.TrimPrefix(strings.ToLower(urn), "urn:uuid:")
	if urn == "" {
		return ""
	}
	return urn
}
