package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/backend"
	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/store"
)

var (
	mimeOnce         sync.Once
	mimeTypes        []string
	errPPDConstraint = errors.New("ppd-constraint-violation")
	errUnsupported   = errors.New("unsupported-attribute-value")
	policyNamesOnce  sync.Once
	policyNames      []string
	errNotAuthorized = errors.New("not-authorized")
	errNotPossible   = errors.New("not-possible")
)

const supplyCacheTTL = 60 * time.Second

func (s *Server) handleIPPRequest(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(body)

	var req goipp.Message
	if err := req.Decode(buf); err != nil {
		return err
	}

	op := goipp.Op(req.Code)
	ctx := r.Context()
	authType := s.authTypeForRequest(r, op.String())

	if limit := s.Policy.LimitFor(r.URL.Path, op.String()); limit != nil {
		if limit.DenyAll {
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID)
			addOperationDefaults(resp)
			w.Header().Set("Content-Type", goipp.ContentType)
			w.WriteHeader(http.StatusOK)
			return resp.Encode(w)
		}
		if limit.RequireUser || limit.RequireAdmin {
			if !s.authorize(r, authType, limit.RequireAdmin) {
				resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID)
				addOperationDefaults(resp)
				w.Header().Set("Content-Type", goipp.ContentType)
				w.WriteHeader(http.StatusOK)
				return resp.Encode(w)
			}
		}
	}

	// CUPS semantics: "admin" operations require admin auth, job operations are
	// typically allowed for the job owner (see per-handler checks).
	if isAdminOnlyOp(op) && !s.authorize(r, authType, true) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID)
		addOperationDefaults(resp)
		w.Header().Set("Content-Type", goipp.ContentType)
		w.WriteHeader(http.StatusOK)
		return resp.Encode(w)
	}

	var resp *goipp.Message
	var payload []byte
	var payloadReader io.ReadCloser
	switch op {
	case goipp.OpGetPrinterAttributes:
		resp, err = s.handleGetPrinterAttributes(ctx, r, &req)
	case goipp.OpGetPrinterSupportedValues:
		resp, err = s.handleGetPrinterSupportedValues(ctx, r, &req)
	case goipp.OpSetPrinterAttributes:
		resp, err = s.handleSetPrinterAttributes(ctx, r, &req)
	case goipp.OpCupsGetPrinters:
		resp, err = s.handleCupsGetPrinters(ctx, r, &req)
	case goipp.OpCupsGetClasses:
		resp, err = s.handleCupsGetClasses(ctx, r, &req)
	case goipp.OpCupsGetDevices:
		resp, err = s.handleCupsGetDevices(ctx, r, &req)
	case goipp.OpCupsGetPpds:
		resp, err = s.handleCupsGetPpds(ctx, r, &req)
	case goipp.OpCupsGetPpd:
		resp, payload, err = s.handleCupsGetPpd(ctx, r, &req)
	case goipp.OpCupsAddModifyPrinter:
		resp, err = s.handleCupsAddModifyPrinter(ctx, r, &req, buf)
	case goipp.OpCupsMoveJob:
		resp, err = s.handleCupsMoveJob(ctx, r, &req)
	case goipp.OpCupsDeletePrinter:
		resp, err = s.handleCupsDeletePrinter(ctx, r, &req)
	case goipp.OpCupsAcceptJobs:
		resp, err = s.handleCupsAcceptJobs(ctx, r, &req)
	case goipp.OpCupsRejectJobs:
		resp, err = s.handleCupsRejectJobs(ctx, r, &req)
	case goipp.OpCupsAddModifyClass:
		resp, err = s.handleCupsAddModifyClass(ctx, r, &req)
	case goipp.OpCupsDeleteClass:
		resp, err = s.handleCupsDeleteClass(ctx, r, &req)
	case goipp.OpCupsGetDefault:
		resp, err = s.handleCupsGetDefault(ctx, r, &req)
	case goipp.OpCupsSetDefault:
		resp, err = s.handleCupsSetDefault(ctx, r, &req)
	case goipp.OpCreatePrinterSubscriptions:
		resp, err = s.handleCreatePrinterSubscription(ctx, r, &req)
	case goipp.OpCreateJobSubscriptions:
		resp, err = s.handleCreateJobSubscription(ctx, r, &req)
	case goipp.OpGetNotifications:
		resp, err = s.handleGetNotifications(ctx, r, &req)
	case goipp.OpGetSubscriptionAttributes:
		resp, err = s.handleGetSubscriptionAttributes(ctx, r, &req)
	case goipp.OpGetSubscriptions:
		resp, err = s.handleGetSubscriptions(ctx, r, &req)
	case goipp.OpRenewSubscription:
		resp, err = s.handleRenewSubscription(ctx, r, &req)
	case goipp.OpCancelSubscription:
		resp, err = s.handleCancelSubscription(ctx, r, &req)
	case goipp.OpPrintJob:
		resp, err = s.handlePrintJob(ctx, r, &req, buf)
	case goipp.OpCreateJob:
		resp, err = s.handleCreateJob(ctx, r, &req)
	case goipp.OpSendDocument:
		resp, err = s.handleSendDocument(ctx, r, &req, buf)
	case goipp.OpGetJobs:
		resp, err = s.handleGetJobs(ctx, r, &req)
	case goipp.OpGetJobAttributes:
		resp, err = s.handleGetJobAttributes(ctx, r, &req)
	case goipp.OpGetDocuments:
		resp, err = s.handleGetDocuments(ctx, r, &req)
	case goipp.OpGetDocumentAttributes:
		resp, err = s.handleGetDocumentAttributes(ctx, r, &req)
	case goipp.OpSetJobAttributes:
		resp, err = s.handleSetJobAttributes(ctx, r, &req)
	case goipp.OpCancelJob:
		resp, err = s.handleCancelJob(ctx, r, &req)
	case goipp.OpCancelMyJobs:
		resp, err = s.handleCancelMyJobs(ctx, r, &req)
	case goipp.OpValidateJob:
		resp, err = s.handleValidateJob(ctx, r, &req)
	case goipp.OpHoldJob:
		resp, err = s.handleHoldJob(ctx, r, &req)
	case goipp.OpReleaseJob:
		resp, err = s.handleReleaseJob(ctx, r, &req)
	case goipp.OpRestartJob:
		resp, err = s.handleRestartJob(ctx, r, &req)
	case goipp.OpResumeJob:
		resp, err = s.handleResumeJob(ctx, r, &req)
	case goipp.OpCloseJob:
		resp, err = s.handleCloseJob(ctx, r, &req)
	case goipp.OpPausePrinter:
		resp, err = s.handlePausePrinter(ctx, r, &req)
	case goipp.OpPausePrinterAfterCurrentJob:
		resp, err = s.handlePausePrinterAfterCurrentJob(ctx, r, &req)
	case goipp.OpResumePrinter:
		resp, err = s.handleResumePrinter(ctx, r, &req)
	case goipp.OpEnablePrinter:
		resp, err = s.handleEnablePrinter(ctx, r, &req)
	case goipp.OpDisablePrinter:
		resp, err = s.handleDisablePrinter(ctx, r, &req)
	case goipp.OpHoldNewJobs:
		resp, err = s.handleHoldNewJobs(ctx, r, &req)
	case goipp.OpReleaseHeldNewJobs:
		resp, err = s.handleReleaseHeldNewJobs(ctx, r, &req)
	case goipp.OpRestartPrinter:
		resp, err = s.handleRestartPrinter(ctx, r, &req)
	case goipp.OpPurgeJobs:
		resp, err = s.handlePurgeJobs(ctx, r, &req)
	case goipp.OpCancelJobs:
		resp, err = s.handleCancelJobs(ctx, r, &req)
	case goipp.OpPauseAllPrinters:
		resp, err = s.handlePauseAllPrinters(ctx, r, &req)
	case goipp.OpPauseAllPrintersAfterCurrentJob:
		resp, err = s.handlePauseAllPrintersAfterCurrentJob(ctx, r, &req)
	case goipp.OpResumeAllPrinters:
		resp, err = s.handleResumeAllPrinters(ctx, r, &req)
	case goipp.OpRestartSystem:
		resp, err = s.handleRestartSystem(ctx, r, &req)
	case goipp.OpValidateDocument:
		resp, err = s.handleValidateDocument(ctx, r, &req)
	case goipp.OpCupsAuthenticateJob:
		resp, err = s.handleCupsAuthenticateJob(ctx, r, &req)
	case goipp.OpCupsGetDocument:
		resp, payloadReader, err = s.handleCupsGetDocument(ctx, r, &req)
	default:
		resp = goipp.NewResponse(req.Version, goipp.StatusErrorOperationNotSupported, req.RequestID)
		addOperationDefaults(resp)
	}

	if err != nil {
		resp = goipp.NewResponse(req.Version, goipp.StatusErrorInternal, req.RequestID)
		addOperationDefaults(resp)
	}

	w.Header().Set("Content-Type", goipp.ContentType)
	w.WriteHeader(http.StatusOK)
	if err := resp.Encode(w); err != nil {
		return err
	}
	if payloadReader != nil {
		defer payloadReader.Close()
		if _, err := io.Copy(w, payloadReader); err != nil {
			return err
		}
		return nil
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func isAdminOnlyOp(op goipp.Op) bool {
	switch op {
	case goipp.OpCupsAddModifyPrinter,
		goipp.OpCupsDeletePrinter,
		goipp.OpCupsAddModifyClass,
		goipp.OpCupsDeleteClass,
		goipp.OpCupsSetDefault,
		goipp.OpCupsMoveJob,
		goipp.OpCupsAcceptJobs,
		goipp.OpCupsRejectJobs,
		goipp.OpPausePrinter,
		goipp.OpPausePrinterAfterCurrentJob,
		goipp.OpResumePrinter,
		goipp.OpEnablePrinter,
		goipp.OpDisablePrinter,
		goipp.OpHoldNewJobs,
		goipp.OpReleaseHeldNewJobs,
		goipp.OpRestartPrinter,
		goipp.OpPauseAllPrinters,
		goipp.OpPauseAllPrintersAfterCurrentJob,
		goipp.OpResumeAllPrinters,
		goipp.OpRestartSystem,
		goipp.OpSetPrinterAttributes,
		goipp.OpRenewSubscription,
		goipp.OpCancelSubscription,
		goipp.OpPurgeJobs:
		return true
	default:
		return false
	}
}

func (s *Server) handleGetPrinterAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	if dest.IsClass {
		var members []model.Printer
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			var err error
			members, err = s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			return err
		})
		addClassAttributes(resp, dest.Class, members, r, req)
	} else {
		addPrinterAttributes(ctx, resp, dest.Printer, r, req, s.Store)
	}
	return resp, nil
}

func (s *Server) handleGetPrinterSupportedValues(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)

	isClass := dest.IsClass
	printer := dest.Printer
	if isClass {
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			members, err := s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			if err != nil {
				return err
			}
			if len(members) > 0 {
				printer = members[0]
			}
			return nil
		})
		printer = applyClassDefaultsToPrinter(printer, dest.Class)
	}
	attrs := supportedValueAttributes(printer, isClass)
	requested, all := requestedAttributes(req)
	if all {
		for _, attr := range attrs {
			resp.Printer.Add(attr)
		}
		return resp, nil
	}
	for _, attr := range attrs {
		if requested[attr.Name] {
			resp.Printer.Add(attr)
		}
	}
	return resp, nil
}

func (s *Server) handleSetPrinterAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	printer, err := s.resolvePrinter(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	infoVal, infoOk := attrValue(req.Printer, "printer-info")
	locVal, locOk := attrValue(req.Printer, "printer-location")
	geoVal, geoOk := attrValue(req.Printer, "printer-geo-location")
	orgVal, orgOk := attrValue(req.Printer, "printer-organization")
	orgUnitVal, orgUnitOk := attrValue(req.Printer, "printer-organizational-unit")

	var infoPtr, locPtr, geoPtr, orgPtr, orgUnitPtr *string
	if infoOk {
		infoPtr = &infoVal
	}
	if locOk {
		locPtr = &locVal
	}
	if geoOk {
		geoPtr = &geoVal
	}
	if orgOk {
		orgPtr = &orgVal
	}
	if orgUnitOk {
		orgUnitPtr = &orgUnitVal
	}

	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		return s.Store.UpdatePrinterAttributes(ctx, tx, printer.ID, infoPtr, locPtr, geoPtr, orgPtr, orgUnitPtr)
	})
	if err != nil {
		return nil, err
	}

	updated, _ := s.resolvePrinter(ctx, r, req)
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addPrinterAttributes(ctx, resp, updated, r, req, s.Store)
	return resp, nil
}

func (s *Server) handleCupsGetPrinters(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var printers []model.Printer
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		printers, err = s.Store.ListPrinters(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}

	groups := make(goipp.Groups, 0, len(printers)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, p := range printers {
		attrs := buildPrinterAttributes(ctx, p, r, req, s.Store)
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetClasses(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var classes []model.Class
	memberMap := map[int64][]model.Printer{}
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		classes, err = s.Store.ListClasses(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range classes {
			members, err := s.Store.ListClassMembers(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			memberMap[c.ID] = members
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	groups := make(goipp.Groups, 0, len(classes)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, c := range classes {
		attrs := classAttributesWithMembers(c, memberMap[c.ID], r, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetDevices(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var printers []model.Printer
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		printers, err = s.Store.ListPrinters(ctx, tx)
		return err
	})

	devices := []Device{}
	devices = append(devices, discoverLocalDevices()...)
	devices = append(devices, discoverNetworkIPP()...)
	devices = append(devices, discoverMDNSIPP()...)

	groups := make(goipp.Groups, 0, len(printers)+len(devices)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, p := range printers {
		attrs := goipp.Attributes{}
		attrs.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(p.URI)))
		attrs.Add(goipp.MakeAttribute("device-info", goipp.TagText, goipp.String(p.Info)))
		attrs.Add(goipp.MakeAttribute("device-make-and-model", goipp.TagText, goipp.String("CUPS-Golang")))
		attrs.Add(goipp.MakeAttribute("device-class", goipp.TagKeyword, goipp.String("file")))
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
	}
	for _, d := range devices {
		attrs := goipp.Attributes{}
		attrs.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(d.URI)))
		attrs.Add(goipp.MakeAttribute("device-info", goipp.TagText, goipp.String(d.Info)))
		attrs.Add(goipp.MakeAttribute("device-make-and-model", goipp.TagText, goipp.String(d.Make)))
		attrs.Add(goipp.MakeAttribute("device-class", goipp.TagKeyword, goipp.String(d.Class)))
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetPpds(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	ppdDir := s.Config.PPDDir
	names := listPPDNames(ppdDir)
	if len(names) == 0 {
		names = []string{model.DefaultPPDName}
	}

	requested, all := requestedAttributes(req)
	groups := make(goipp.Groups, 0, len(names)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, name := range names {
		attrs := goipp.Attributes{}
		makeAttr := func(attrName string, tag goipp.Tag, value string) {
			if !all && !requested[attrName] {
				return
			}
			if strings.TrimSpace(value) == "" {
				return
			}
			attrs.Add(goipp.MakeAttribute(attrName, tag, goipp.String(value)))
		}
		makeAttr("ppd-name", goipp.TagName, name)

		ppd, err := config.LoadPPD(filepath.Join(ppdDir, name))
		if err == nil && ppd != nil {
			makeAttr("ppd-make", goipp.TagText, ppd.Make)
			modelName := firstNonEmpty(ppd.NickName, ppd.Model, ppd.Make)
			if modelName != "" {
				makeAttr("ppd-make-and-model", goipp.TagText, modelName)
			}
		} else {
			makeAttr("ppd-make", goipp.TagText, "CUPS-Golang")
			makeAttr("ppd-make-and-model", goipp.TagText, "CUPS-Golang Generic Printer")
		}
		if len(attrs) == 0 {
			continue
		}
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetPpd(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, []byte, error) {
	ppdName := attrString(req.Operation, "ppd-name")
	if ppdName == "" {
		if uri := attrString(req.Operation, "printer-uri"); uri != "" {
			if dest, err := s.resolveDestination(ctx, r, req); err == nil && !dest.IsClass {
				ppdName = dest.Printer.PPDName
			}
		}
	}
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}

	ppdPath := safePPDPath(s.Config.PPDDir, ppdName)
	ppd, err := os.ReadFile(ppdPath)
	if err != nil && ppdName != model.DefaultPPDName {
		ppdPath = safePPDPath(s.Config.PPDDir, model.DefaultPPDName)
		ppd, err = os.ReadFile(ppdPath)
	}
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil, nil
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, ppd, nil
}

func (s *Server) handleCupsMoveJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	destURI := attrString(req.Operation, "printer-uri")
	if destURI == "" {
		destURI = attrString(req.Operation, "printer-uri-destination")
	}
	if destURI == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var target model.Printer
	var job model.Job
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		target, err = s.printerFromURI(ctx, tx, destURI)
		if err != nil {
			return err
		}
		if !target.Accepting {
			return fmt.Errorf("not-accepting")
		}
		job, err = s.Store.MoveJob(ctx, tx, jobID, target.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		if errors.Is(err, store.ErrJobCompleted) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID), nil
		}
		if err.Error() == "not-accepting" {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, target, r, model.Document{}, req)
	return resp, nil
}

func (s *Server) handleCupsAddModifyPrinter(ctx context.Context, r *http.Request, req *goipp.Message, payload *bytes.Buffer) (*goipp.Message, error) {
	name := attrString(req.Operation, "printer-name")
	if name == "" {
		name = printerNameFromURI(attrString(req.Operation, "printer-uri"))
	}
	if name == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	location := attrString(req.Printer, "printer-location")
	info := attrString(req.Printer, "printer-info")
	uri := attrString(req.Printer, "device-uri")
	ppdName := attrString(req.Printer, "ppd-name")
	if ppdName == "" {
		ppdName = attrString(req.Operation, "ppd-name")
	}
	ppdName = strings.TrimSpace(ppdName)
	geo := attrString(req.Printer, "printer-geo-location")
	org := attrString(req.Printer, "printer-organization")
	orgUnit := attrString(req.Printer, "printer-organizational-unit")
	sharedVal, sharedOk := attrBoolPresent(req.Printer, "printer-is-shared")
	jobSheetsDefault := ""
	jobSheetsOk := false
	if vals := attrStrings(req.Printer, "job-sheets-default"); len(vals) > 0 {
		jobSheetsDefault = strings.Join(vals, ",")
		jobSheetsOk = true
	}
	if !jobSheetsOk {
		for _, attr := range req.Printer {
			if attr.Name != "job-sheets-col-default" || len(attr.Values) == 0 {
				continue
			}
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				if v := collectionString(col, "job-sheets"); v != "" {
					jobSheetsDefault = v
					jobSheetsOk = true
					break
				}
			}
		}
	}
	if uri == "" {
		uri = attrString(req.Operation, "printer-uri")
	}
	if uri == "" {
		uri = "file:///dev/null"
	}
	var printer model.Printer
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.UpsertPrinter(ctx, tx, name, uri, location, info, true)
		if err != nil {
			return err
		}
		// Optional inline PPD payload (only when ppd-name isn't explicitly set).
		if ppdName == "" && payload != nil && payload.Len() > 0 {
			ppdData, err := io.ReadAll(payload)
			if err != nil {
				return err
			}
			if len(ppdData) > 0 {
				targetName := name + ".ppd"
				ppdPath := safePPDPath(s.Config.PPDDir, targetName)
				if err := os.MkdirAll(filepath.Dir(ppdPath), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(ppdPath, ppdData, 0o644); err != nil {
					return err
				}
				ppdName = filepath.Base(ppdPath)
			}
		}
		if ppdName != "" {
			if err := s.Store.UpdatePrinterPPDName(ctx, tx, printer.ID, ppdName); err != nil {
				return err
			}
			printer.PPDName = ppdName
		}
		var geoPtr, orgPtr, orgUnitPtr *string
		if geo != "" {
			geoPtr = &geo
		}
		if org != "" {
			orgPtr = &org
		}
		if orgUnit != "" {
			orgUnitPtr = &orgUnit
		}
		if geoPtr != nil || orgPtr != nil || orgUnitPtr != nil {
			if err := s.Store.UpdatePrinterAttributes(ctx, tx, printer.ID, nil, nil, geoPtr, orgPtr, orgUnitPtr); err != nil {
				return err
			}
		}
		if sharedOk {
			if err := s.Store.UpdatePrinterSharing(ctx, tx, printer.ID, sharedVal); err != nil {
				return err
			}
			printer.Shared = sharedVal
		}
		if jobSheetsOk {
			if err := s.Store.UpdatePrinterJobSheetsDefault(ctx, tx, printer.ID, jobSheetsDefault); err != nil {
				return err
			}
			printer.JobSheetsDefault = jobSheetsDefault
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addPrinterAttributes(ctx, resp, printer, r, req, s.Store)
	return resp, nil
}

func (s *Server) handleCupsDeletePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	name := attrString(req.Operation, "printer-name")
	if name == "" {
		name = printerNameFromURI(attrString(req.Operation, "printer-uri"))
	}
	if name == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(ctx, tx, name)
		if err != nil {
			return err
		}
		return s.Store.DeletePrinter(ctx, tx, p.ID)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsAcceptJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			return s.Store.UpdateClassAccepting(ctx, tx, dest.Class.ID, true)
		}
		return s.Store.UpdatePrinterAccepting(ctx, tx, dest.Printer.ID, true)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsRejectJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			return s.Store.UpdateClassAccepting(ctx, tx, dest.Class.ID, false)
		}
		return s.Store.UpdatePrinterAccepting(ctx, tx, dest.Printer.ID, false)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsAddModifyClass(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	name := attrString(req.Operation, "printer-name")
	if name == "" {
		name = printerNameFromURI(attrString(req.Operation, "printer-uri"))
	}
	if name == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	location := attrString(req.Printer, "printer-location")
	info := attrString(req.Printer, "printer-info")
	memberURIs := attrStrings(req.Printer, "member-uris")
	jobSheetsDefault := ""
	jobSheetsOk := false
	if vals := attrStrings(req.Printer, "job-sheets-default"); len(vals) > 0 {
		jobSheetsDefault = strings.Join(vals, ",")
		jobSheetsOk = true
	}
	if !jobSheetsOk {
		for _, attr := range req.Printer {
			if attr.Name != "job-sheets-col-default" || len(attr.Values) == 0 {
				continue
			}
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				if v := collectionString(col, "job-sheets"); v != "" {
					jobSheetsDefault = v
					jobSheetsOk = true
					break
				}
			}
		}
	}

	var class model.Class
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		memberIDs := make([]int64, 0, len(memberURIs))
		for _, uri := range memberURIs {
			memberName := printerNameFromURI(uri)
			if memberName == "" {
				memberName = uri
			}
			if p, err := s.Store.GetPrinterByName(ctx, tx, memberName); err == nil {
				memberIDs = append(memberIDs, p.ID)
			}
		}
		class, err = s.Store.UpsertClass(ctx, tx, name, location, info, true, memberIDs)
		if err != nil {
			return err
		}
		if jobSheetsOk {
			if err := s.Store.UpdateClassJobSheetsDefault(ctx, tx, class.ID, jobSheetsDefault); err != nil {
				return err
			}
			class.JobSheetsDefault = jobSheetsDefault
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	var members []model.Printer
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		members, err = s.Store.ListClassMembers(ctx, tx, class.ID)
		return err
	})
	addClassAttributes(resp, class, members, r, req)
	return resp, nil
}

func (s *Server) handleCupsDeleteClass(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	name := attrString(req.Operation, "printer-name")
	if name == "" {
		name = printerNameFromURI(attrString(req.Operation, "printer-uri"))
	}
	if name == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(ctx, tx, name)
		if err != nil {
			return err
		}
		return s.Store.DeleteClass(ctx, tx, c.ID)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsGetDefault(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var dest destination
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		dest, err = s.defaultDestination(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	if dest.IsClass {
		var members []model.Printer
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			var err error
			members, err = s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			return err
		})
		addClassAttributes(resp, dest.Class, members, r, req)
	} else {
		addPrinterAttributes(ctx, resp, dest.Printer, r, req, s.Store)
	}
	return resp, nil
}

func (s *Server) handleCupsSetDefault(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	name := attrString(req.Operation, "printer-name")
	if name == "" {
		name = printerNameFromURI(attrString(req.Operation, "printer-uri"))
	}
	if name == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if p, err := s.Store.GetPrinterByName(ctx, tx, name); err == nil {
			return s.Store.SetDefaultPrinter(ctx, tx, p.ID)
		}
		if c, err := s.Store.GetClassByName(ctx, tx, name); err == nil {
			return s.Store.SetDefaultClass(ctx, tx, c.ID)
		}
		return sql.ErrNoRows
	})
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCreatePrinterSubscription(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil || dest.IsClass {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	events, lease, recipient, pullMethod, interval, userData, err := parseSubscriptionRequest(req)
	if err != nil {
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	owner := requestingUserName(req, r)
	var sub model.Subscription
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		sub, err = s.Store.CreateSubscription(ctx, tx, &dest.Printer.ID, nil, events, lease, owner, recipient, pullMethod, interval, userData)
		return err
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	resp.Subscription.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(sub.ID)))
	return resp, nil
}

func (s *Server) handleCreateJobSubscription(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	events, lease, recipient, pullMethod, interval, userData, err := parseSubscriptionRequest(req)
	if err != nil {
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}
	owner := requestingUserName(req, r)
	var sub model.Subscription
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !s.canManageJob(ctx, r, req, authType, job, false) {
			return errNotAuthorized
		}
		sub, err = s.Store.CreateSubscription(ctx, tx, nil, &jobID, events, lease, owner, recipient, pullMethod, interval, userData)
		return err
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	resp.Subscription.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(sub.ID)))
	return resp, nil
}

func (s *Server) handleGetNotifications(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	subIDs := attrInts(req.Operation, "notify-subscription-ids")
	if len(subIDs) == 0 {
		if subID := attrInt(req.Operation, "notify-subscription-id"); subID != 0 {
			subIDs = []int64{subID}
		}
	}
	if len(subIDs) == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	seqNums := attrInts(req.Operation, "notify-sequence-numbers")
	if len(seqNums) == 0 {
		if seq := attrInt(req.Operation, "notify-sequence-number"); seq != 0 {
			seqNums = []int64{seq}
		}
	}

	limit := clampLimit(attrInt(req.Operation, "notify-limit"), 0, 100, 1000)
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}

	type subWithNotes struct {
		sub   model.Subscription
		notes []model.Notification
		min   int64
	}
	collected := []subWithNotes{}
	interval := 60

	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		for i, id := range subIDs {
			sub, err := s.Store.GetSubscription(ctx, tx, id)
			if err != nil {
				return err
			}
			if !s.canManageSubscription(ctx, r, req, authType, sub.Owner) {
				return errNotAuthorized
			}
			minSeq := int64(1)
			if i < len(seqNums) && seqNums[i] > 0 {
				minSeq = seqNums[i]
			}
			notes, err := s.Store.ListNotifications(ctx, tx, id, limit)
			if err != nil {
				return err
			}
			filtered := make([]model.Notification, 0, len(notes))
			for _, n := range notes {
				if int64(n.ID) >= minSeq {
					filtered = append(filtered, n)
				}
			}
			collected = append(collected, subWithNotes{sub: sub, notes: filtered, min: minSeq})

			if sub.JobID.Valid {
				job, err := s.Store.GetJob(ctx, tx, sub.JobID.Int64)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil {
					if job.State >= 6 {
						interval = 0
					} else if job.State == 5 && interval > 10 {
						interval = 10
					}
				}
			} else if sub.PrinterID.Valid {
				printer, err := s.Store.GetPrinterByID(ctx, tx, sub.PrinterID.Int64)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil && printer.State == 4 && interval > 30 {
					interval = 30
				}
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	opAttrs := buildOperationDefaults()
	opAttrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	if interval > 0 {
		opAttrs.Add(goipp.MakeAttribute("notify-get-interval", goipp.TagInteger, goipp.Integer(interval)))
	}

	groups := goipp.Groups{{
		Tag:   goipp.TagOperationGroup,
		Attrs: opAttrs,
	}}
	for _, item := range collected {
		sub := item.sub
		for _, n := range item.notes {
			attrs := goipp.Attributes{}
			attrs.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(sub.ID)))
			attrs.Add(goipp.MakeAttribute("notify-event", goipp.TagKeyword, goipp.String(n.Event)))
			attrs.Add(goipp.MakeAttribute("notify-time-interval", goipp.TagInteger, goipp.Integer(sub.TimeInterval)))
			attrs.Add(goipp.MakeAttribute("notify-lease-duration", goipp.TagInteger, goipp.Integer(sub.LeaseSecs)))
			if sub.LeaseSecs > 0 {
				attrs.Add(goipp.MakeAttribute("notify-lease-expiration-time", goipp.TagInteger, goipp.Integer(sub.CreatedAt.Add(time.Duration(sub.LeaseSecs)*time.Second).Unix())))
			}
			attrs.Add(goipp.MakeAttribute("printer-state-change-time", goipp.TagInteger, goipp.Integer(n.CreatedAt.Unix())))
			user := strings.TrimSpace(sub.Owner)
			if user == "" {
				user = "anonymous"
			}
			attrs.Add(goipp.MakeAttribute("notify-subscriber-user-name", goipp.TagName, goipp.String(user)))
			attrs.Add(goipp.MakeAttribute("notify-sequence-number", goipp.TagInteger, goipp.Integer(n.ID)))
			groups = append(groups, goipp.Group{Tag: goipp.TagEventNotificationGroup, Attrs: attrs})
		}
	}
	status := goipp.StatusOk
	if interval == 0 {
		status = goipp.StatusOkEventsComplete
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(status), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleGetSubscriptionAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	subID := attrInt(req.Operation, "notify-subscription-id")
	if subID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var sub model.Subscription
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		sub, err = s.Store.GetSubscription(ctx, tx, subID)
		if err != nil {
			return err
		}
		if !s.canManageSubscription(ctx, r, req, authType, sub.Owner) {
			return errNotAuthorized
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	attrs := buildSubscriptionAttributes(sub, r, s, req)
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	for _, attr := range attrs {
		resp.Subscription.Add(attr)
	}
	return resp, nil
}

func (s *Server) handleGetSubscriptions(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var printerID *int64
	var jobID *int64

	if uri := attrString(req.Operation, "job-uri"); uri != "" {
		if id := jobIDFromURI(uri); id != 0 {
			jobID = &id
		}
	}
	if jobID == nil {
		if id := attrInt(req.Subscription, "notify-job-id"); id != 0 {
			jobID = &id
		} else if id := attrInt(req.Operation, "notify-job-id"); id != 0 {
			jobID = &id
		} else if id := attrInt(req.Operation, "job-id"); id != 0 {
			jobID = &id
		}
	}

	if jobID != nil {
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			_, err := s.Store.GetJob(ctx, tx, *jobID)
			return err
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
			}
			return nil, err
		}
	}

	if printerID == nil && jobID == nil {
		if uri := attrString(req.Operation, "printer-uri"); uri != "" {
			if u, err := url.Parse(uri); err == nil {
				if strings.HasPrefix(u.Path, "/printers/") {
					name := strings.TrimPrefix(u.Path, "/printers/")
					if name != "" {
						err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
							p, err := s.Store.GetPrinterByName(ctx, tx, name)
							if err != nil {
								return err
							}
							id := p.ID
							printerID = &id
							return nil
						})
						if err != nil {
							if errors.Is(err, sql.ErrNoRows) {
								return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
							}
							return nil, err
						}
					}
				}
			}
		}
	}

	limit := clampLimit(attrInt(req.Operation, "limit"), 0, 0, 1000000)
	owner := ""
	if attrBool(req.Operation, "my-subscriptions") {
		owner = requestingUserName(req, r)
		authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
		if authType == "" {
			authType = "basic"
		}
		if !s.canActAsUser(ctx, r, req, authType, owner) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
	}

	var subs []model.Subscription
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		subs, err = s.Store.ListSubscriptions(ctx, tx, printerID, jobID, owner, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	groups := goipp.Groups{{
		Tag:   goipp.TagOperationGroup,
		Attrs: buildOperationDefaults(),
	}}

	for _, sub := range subs {
		attrs := buildSubscriptionAttributes(sub, r, s, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagSubscriptionGroup, Attrs: attrs})
	}

	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleRenewSubscription(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	subID := attrInt(req.Operation, "notify-subscription-id")
	if subID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var updated model.Subscription
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		sub, err := s.Store.GetSubscription(ctx, tx, subID)
		if err != nil {
			return err
		}
		if !s.canManageSubscription(ctx, r, req, authType, sub.Owner) {
			return errNotAuthorized
		}
		if sub.JobID.Valid {
			return errNotPossible
		}
		lease, ok := attrIntPresent(req.Subscription, "notify-lease-duration")
		if !ok {
			lease, ok = attrIntPresent(req.Operation, "notify-lease-duration")
		}
		if !ok {
			lease = sub.LeaseSecs
		}
		updated, err = s.Store.UpdateSubscriptionLease(ctx, tx, subID, lease)
		return err
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, errNotPossible) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	attrs := buildSubscriptionAttributes(updated, r, s, req)
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	for _, attr := range attrs {
		resp.Subscription.Add(attr)
	}
	return resp, nil
}

func (s *Server) handleCancelSubscription(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	subID := attrInt(req.Operation, "notify-subscription-id")
	if subID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		sub, err := s.Store.GetSubscription(ctx, tx, subID)
		if err != nil {
			return err
		}
		if !s.canManageSubscription(ctx, r, req, authType, sub.Owner) {
			return errNotAuthorized
		}
		return s.Store.CancelSubscription(ctx, tx, subID)
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handlePrintJob(ctx context.Context, r *http.Request, req *goipp.Message, docReader io.Reader) (*goipp.Message, error) {
	jobName := attrString(req.Operation, "job-name")
	if jobName == "" {
		jobName = "Untitled"
	}
	userName := attrString(req.Operation, "requesting-user-name")
	if userName == "" {
		userName = "anonymous"
	}
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	if dest.IsClass && !s.userAllowedForClass(ctx, dest.Class, userName) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	printer := dest.Printer
	if dest.IsClass {
		if !dest.Class.Accepting {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer, err = s.selectClassMember(ctx, dest.Class.ID)
		if err != nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer = applyClassDefaultsToPrinter(printer, dest.Class)
	}
	if !printer.Accepting {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
	}
	if !s.userAllowedForPrinter(ctx, printer, userName) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	documentFormat := attrString(req.Operation, "document-format")
	if documentFormat == "" {
		documentFormat = "application/octet-stream"
	}
	if !isDocumentFormatSupported(documentFormat) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorDocumentUnprintable, req.RequestID), nil
	}

	var job model.Job
	var doc model.Document
	if err := validateRequestOptions(req, printer); err != nil {
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	options := serializeJobOptions(req, printer)
	if dest.IsClass {
		options = addClassInternalOptions(options, dest.Class)
	}
	holdRequested := jobHoldRequested(options)
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, jobName, userName, options)
		if err != nil {
			return err
		}
		sp := spool.Spool{Dir: s.Spool.Dir, OutputDir: s.Spool.OutputDir}
		path, size, err := sp.Save(job.ID, jobName, docReader)
		if err != nil {
			return err
		}
		doc, err = s.Store.AddDocument(ctx, tx, job.ID, jobName, documentFormat, path, size)
		if err != nil {
			return err
		}
		if holdRequested {
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-hold-until-specified", nil); err != nil {
				return err
			}
			job.State = 4
			job.StateReason = "job-hold-until-specified"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, doc, req)
	return resp, nil
}

func (s *Server) handleCreateJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobName := attrString(req.Operation, "job-name")
	if jobName == "" {
		jobName = "Untitled"
	}
	userName := attrString(req.Operation, "requesting-user-name")
	if userName == "" {
		userName = "anonymous"
	}
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	if dest.IsClass && !s.userAllowedForClass(ctx, dest.Class, userName) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	printer := dest.Printer
	if dest.IsClass {
		if !dest.Class.Accepting {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer, err = s.selectClassMember(ctx, dest.Class.ID)
		if err != nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer = applyClassDefaultsToPrinter(printer, dest.Class)
	}
	if !printer.Accepting {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
	}
	if !s.userAllowedForPrinter(ctx, printer, userName) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}

	var job model.Job
	if err := validateRequestOptions(req, printer); err != nil {
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	options := serializeJobOptions(req, printer)
	if dest.IsClass {
		options = addClassInternalOptions(options, dest.Class)
	}
	holdRequested := jobHoldRequested(options)
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, jobName, userName, options)
		if err != nil {
			return err
		}
		if holdRequested {
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-hold-until-specified", nil); err != nil {
				return err
			}
			job.State = 4
			job.StateReason = "job-hold-until-specified"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, model.Document{}, req)
	return resp, nil
}

func (s *Server) handleSendDocument(ctx context.Context, r *http.Request, req *goipp.Message, docReader io.Reader) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	documentFormat := attrString(req.Operation, "document-format")
	if documentFormat == "" {
		documentFormat = "application/octet-stream"
	}
	if !isDocumentFormatSupported(documentFormat) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorDocumentUnprintable, req.RequestID), nil
	}

	var job model.Job
	var printer model.Printer
	var doc model.Document
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}
		sp := spool.Spool{Dir: s.Spool.Dir, OutputDir: s.Spool.OutputDir}
		path, size, err := sp.Save(job.ID, job.Name, docReader)
		if err != nil {
			return err
		}
		doc, err = s.Store.AddDocument(ctx, tx, job.ID, job.Name, documentFormat, path, size)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, doc, req)
	return resp, nil
}

func (s *Server) handleGetJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	limit := clampLimit(attrInt(req.Operation, "limit"), 50, 1, 1000)
	queryLimit := limit
	if queryLimit < 200 {
		queryLimit = 200
	}
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	var jobs []model.Job
	printerMap := map[int64]model.Printer{}
	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		if dest.IsClass {
			members, err := s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			if err != nil {
				return err
			}
			for _, p := range members {
				printerMap[p.ID] = p
				rows, err := s.Store.ListJobsByPrinter(ctx, tx, p.ID, queryLimit)
				if err != nil {
					return err
				}
				jobs = append(jobs, rows...)
			}
			return nil
		}
		printerMap[dest.Printer.ID] = dest.Printer
		jobs, err = s.Store.ListJobsByPrinter(ctx, tx, dest.Printer.ID, queryLimit)
		return err
	})
	if err != nil {
		return nil, err
	}

	jobs = filterJobs(jobs, req, r)
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}

	groups := make(goipp.Groups, 0, len(jobs)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, job := range jobs {
		printer := printerMap[job.PrinterID]
		attrs := buildJobAttributes(job, printer, r, model.Document{}, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagJobGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleGetJobAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var job model.Job
	var printer model.Printer
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, model.Document{}, req)
	return resp, nil
}

func (s *Server) handleGetDocuments(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := int64(0)
	if uri := attrString(req.Operation, "job-uri"); uri != "" {
		jobID = jobIDFromURI(uri)
	} else if uri := attrString(req.Operation, "printer-uri"); uri != "" {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var job model.Job
	var printer model.Printer
	var docs []model.Document
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}
		docs, err = s.Store.ListDocumentsByJob(ctx, tx, job.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	allDocs := appendBannerDocs(job, printer, docs)

	firstDoc := attrInt(req.Operation, "first-document-number")
	if firstDoc <= 0 {
		firstDoc = 1
	}
	limit := clampLimit(attrInt(req.Operation, "limit"), len(allDocs), 1, 1000)
	if limit > len(allDocs) {
		limit = len(allDocs)
	}
	start := int(firstDoc - 1)
	if start < 0 {
		start = 0
	}
	if start > len(allDocs) {
		start = len(allDocs)
	}
	end := start + limit
	if end > len(allDocs) {
		end = len(allDocs)
	}

	groups := make(goipp.Groups, 0, 1+(end-start))
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for i := start; i < end; i++ {
		docNum := int64(i + 1)
		attrs := buildDocumentAttributes(allDocs[i], docNum, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagDocumentGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleGetDocumentAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := int64(0)
	if uri := attrString(req.Operation, "job-uri"); uri != "" {
		jobID = jobIDFromURI(uri)
	} else if uri := attrString(req.Operation, "printer-uri"); uri != "" {
		jobID = attrInt(req.Operation, "job-id")
		if jobID == 0 {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
	}
	if jobID == 0 {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	docNum := attrInt(req.Operation, "document-number")
	if docNum <= 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	var job model.Job
	var printer model.Printer
	var docs []model.Document
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}
		docs, err = s.Store.ListDocumentsByJob(ctx, tx, job.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}
	allDocs := appendBannerDocs(job, printer, docs)
	if int(docNum) > len(allDocs) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	doc := allDocs[docNum-1]

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	for _, attr := range buildDocumentAttributes(doc, int64(docNum), req) {
		resp.Document.Add(attr)
	}
	return resp, nil
}

func buildDocumentAttributes(doc model.Document, docNum int64, req *goipp.Message) goipp.Attributes {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("document-number", goipp.TagInteger, goipp.Integer(docNum)))
	if doc.MimeType != "" {
		attrs.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(doc.MimeType)))
	}
	if doc.FileName != "" {
		attrs.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(doc.FileName)))
	}
	if doc.SizeBytes > 0 {
		attrs.Add(goipp.MakeAttribute("document-size", goipp.TagInteger, goipp.Integer(doc.SizeBytes)))
	}
	return filterAttributesForRequest(attrs, req)
}

func (s *Server) handleSetJobAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	updates, jobName := collectJobOptionUpdates(req)
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())

	var job model.Job
	var printer model.Printer
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !s.canManageJob(ctx, r, req, authType, job, false) {
			return errNotAuthorized
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}

		mergedOptions := job.Options
		if len(updates) > 0 {
			mergedOptions = mergeJobOptions(job.Options, updates)
		}
		if err := validatePPDConstraintsForPrinter(printer, mergedOptions); err != nil {
			return err
		}

		var optionsPtr *string
		if len(updates) > 0 {
			optionsPtr = &mergedOptions
			job.Options = mergedOptions
		}
		if optionsPtr != nil || jobName != nil {
			if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, jobName, optionsPtr); err != nil {
				return err
			}
			if jobName != nil {
				job.Name = *jobName
			}
		}

		if hold, ok := updates["job-hold-until"]; ok {
			if hold != "" && hold != "no-hold" && job.State == 3 {
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-hold-until-specified", nil); err != nil {
					return err
				}
				job.State = 4
				job.StateReason = "job-hold-until-specified"
			} else if hold == "no-hold" && job.State == 4 {
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-incoming", nil); err != nil {
					return err
				}
				job.State = 3
				job.StateReason = "job-incoming"
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, model.Document{}, req)
	return resp, nil
}

func (s *Server) handleCancelJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !s.canManageJob(ctx, r, req, authType, job, true) {
			return errNotAuthorized
		}
		completed := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, jobID, 7, "job-canceled-by-user", &completed)
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCancelMyJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	user := strings.TrimSpace(attrString(req.Operation, "requesting-user-name"))
	if user == "" && r != nil {
		if authUser, _, ok := r.BasicAuth(); ok {
			user = authUser
		}
	}
	if user == "" {
		user = "anonymous"
	}
	if !s.canActAsUser(ctx, r, req, authType, user) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}

	which := strings.ToLower(strings.TrimSpace(attrString(req.Operation, "which-jobs")))
	if which == "" {
		which = "not-completed"
	}
	reason := "job-canceled-by-user"
	if attrBool(req.Operation, "purge-jobs") {
		reason = "job-purged"
	}

	var printerID *int64
	if uri := attrString(req.Operation, "printer-uri"); uri != "" {
		var pid int64
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			p, err := s.printerFromURI(ctx, tx, uri)
			if err != nil {
				return err
			}
			pid = p.ID
			return nil
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
			}
			return nil, err
		}
		printerID = &pid
	}

	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		jobs, err := s.Store.ListJobsByUser(ctx, tx, user, printerID, 1000)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if !matchWhichJobs(which, job.State) {
				continue
			}
			if job.State >= 7 {
				continue
			}
			completed := time.Now().UTC()
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 7, reason, &completed); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleValidateJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	userName := attrString(req.Operation, "requesting-user-name")
	if userName == "" {
		userName = "anonymous"
	}
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	if dest.IsClass && !s.userAllowedForClass(ctx, dest.Class, userName) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	printer := dest.Printer
	if dest.IsClass {
		if !dest.Class.Accepting {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer, err = s.selectClassMember(ctx, dest.Class.ID)
		if err != nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAcceptingJobs, req.RequestID), nil
		}
		printer = applyClassDefaultsToPrinter(printer, dest.Class)
	}
	if err := validateRequestOptions(req, printer); err != nil {
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleValidateDocument(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	docFormat := attrString(req.Operation, "document-format")
	if docFormat == "" {
		docFormat = "application/octet-stream"
	}
	if !isDocumentFormatSupported(docFormat) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorDocumentUnprintable, req.RequestID), nil
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsAuthenticateJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID != 0 {
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			_, err := s.Store.GetJob(ctx, tx, jobID)
			return err
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
			}
			return nil, err
		}
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCupsGetDocument(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, io.ReadCloser, error) {
	jobID := int64(0)
	if uri := attrString(req.Operation, "job-uri"); uri != "" {
		jobID = jobIDFromURI(uri)
	} else if uri := attrString(req.Operation, "printer-uri"); uri != "" {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil, nil
	}

	docNum := attrInt(req.Operation, "document-number")
	if docNum <= 0 {
		docNum = 1
	}

	var job model.Job
	var doc model.Document
	var printer model.Printer
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}
		docs, err := s.Store.ListDocumentsByJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		allDocs := appendBannerDocs(job, printer, docs)
		if docNum > int64(len(allDocs)) {
			return sql.ErrNoRows
		}
		doc = allDocs[int(docNum)-1]
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil, nil
		}
		return nil, nil, err
	}

	var reader io.ReadCloser
	if doc.MimeType == "application/vnd.cups-banner" || doc.Path == "" {
		content := renderBannerTemplateContent(doc, job, printer)
		reader = io.NopCloser(strings.NewReader(content))
	} else {
		f, err := os.Open(doc.Path)
		if err != nil {
			if os.IsNotExist(err) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil, nil
			}
			return nil, nil, err
		}
		reader = f
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, reader, nil
}

func (s *Server) handleHoldJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateJobStateFromRequest(ctx, r, req, 4, "job-held-by-user", nil, false)
}

func (s *Server) handleReleaseJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateJobStateFromRequest(ctx, r, req, 3, "job-queued", nil, false)
}

func (s *Server) handleRestartJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateJobStateFromRequest(ctx, r, req, 3, "job-restart", nil, false)
}

func (s *Server) handleResumeJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateJobStateFromRequest(ctx, r, req, 3, "job-queued", nil, false)
}

func (s *Server) handleCloseJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())

	var job model.Job
	var printer model.Printer
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !s.canManageJob(ctx, r, req, authType, job, false) {
			return errNotAuthorized
		}
		printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
		if err != nil {
			return err
		}
		if job.State < 7 {
			return s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-queued", nil)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, model.Document{}, req)
	return resp, nil
}

func (s *Server) updateJobStateFromRequest(ctx context.Context, r *http.Request, req *goipp.Message, state int, reason string, completedAt *time.Time, allowCancelAny bool) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !s.canManageJob(ctx, r, req, authType, job, allowCancelAny) {
			return errNotAuthorized
		}
		return s.Store.UpdateJobState(ctx, tx, jobID, state, reason, completedAt)
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handlePausePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 5, false, "printer-stopped")
}

func (s *Server) handlePausePrinterAfterCurrentJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 5, false, "printer-stopping")
}

func (s *Server) handleResumePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 3, true, "printer-resumed")
}

func (s *Server) handleEnablePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationAccepting(ctx, r, req, true)
}

func (s *Server) handleDisablePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationAccepting(ctx, r, req, false)
}

func (s *Server) handleHoldNewJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 3, false, "printer-hold-new-jobs")
}

func (s *Server) handleReleaseHeldNewJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 3, true, "printer-release-held-new-jobs")
}

func (s *Server) handleRestartPrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateDestinationState(ctx, r, req, 3, true, "printer-restarted")
}

func (s *Server) updateDestinationState(ctx context.Context, r *http.Request, req *goipp.Message, state int, accepting bool, reason string) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			if err := s.Store.UpdateClassState(ctx, tx, dest.Class.ID, state); err != nil {
				return err
			}
			return s.Store.UpdateClassAccepting(ctx, tx, dest.Class.ID, accepting)
		}
		if err := s.Store.UpdatePrinterState(ctx, tx, dest.Printer.ID, state); err != nil {
			return err
		}
		return s.Store.UpdatePrinterAccepting(ctx, tx, dest.Printer.ID, accepting)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) updateDestinationAccepting(ctx context.Context, r *http.Request, req *goipp.Message, accepting bool) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			return s.Store.UpdateClassAccepting(ctx, tx, dest.Class.ID, accepting)
		}
		return s.Store.UpdatePrinterAccepting(ctx, tx, dest.Printer.ID, accepting)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) handleCancelJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	// If my-jobs is set this behaves like Cancel-My-Jobs for the target destination.
	if attrBool(req.Operation, "my-jobs") {
		return s.handleCancelMyJobs(ctx, r, req)
	}
	return s.cancelJobsForDestination(ctx, r, req, "job-canceled-by-user")
}

func (s *Server) handlePurgeJobs(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	deleteFiles := !s.preserveJobFiles(ctx)
	if err := s.purgeDestinationJobs(ctx, dest, deleteFiles); err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) cancelJobsForDestination(ctx context.Context, r *http.Request, req *goipp.Message, reason string) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			members, err := s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			if err != nil {
				return err
			}
			for _, p := range members {
				if err := s.Store.CancelJobsByPrinter(ctx, tx, p.ID, reason); err != nil {
					return err
				}
			}
			return nil
		}
		return s.Store.CancelJobsByPrinter(ctx, tx, dest.Printer.ID, reason)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func (s *Server) purgeDestinationJobs(ctx context.Context, dest destination, deleteFiles bool) error {
	if s == nil || s.Store == nil {
		return nil
	}
	paths := []string{}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		collect := func(printerID int64) error {
			jobIDs, err := s.Store.ListJobIDsByPrinter(ctx, tx, printerID)
			if err != nil {
				return err
			}
			for _, jobID := range jobIDs {
				if deleteFiles {
					docs, err := s.Store.ListDocumentsByJob(ctx, tx, jobID)
					if err != nil {
						return err
					}
					for _, d := range docs {
						if strings.TrimSpace(d.Path) != "" {
							paths = append(paths, d.Path)
						}
					}
				}
				if err := s.Store.DeleteJob(ctx, tx, jobID); err != nil {
					return err
				}
			}
			return nil
		}

		if dest.IsClass {
			members, err := s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			if err != nil {
				return err
			}
			for _, p := range members {
				if err := collect(p.ID); err != nil {
					return err
				}
			}
			return nil
		}
		return collect(dest.Printer.ID)
	})
	if err != nil {
		return err
	}
	if deleteFiles {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
	return nil
}

func (s *Server) preserveJobFiles(ctx context.Context) bool {
	if s == nil || s.Store == nil {
		return false
	}
	enabled := false
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		val, err := s.Store.GetSetting(ctx, tx, "_preserve_job_files", "0")
		if err != nil {
			return err
		}
		enabled = isTruthy(val)
		return nil
	})
	return enabled
}

func (s *Server) handlePauseAllPrinters(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateAllPrinters(ctx, req, 5, false)
}

func (s *Server) handlePauseAllPrintersAfterCurrentJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateAllPrinters(ctx, req, 5, false)
}

func (s *Server) handleResumeAllPrinters(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateAllPrinters(ctx, req, 3, true)
}

func (s *Server) handleRestartSystem(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	return s.updateAllPrinters(ctx, req, 3, true)
}

func (s *Server) updateAllPrinters(ctx context.Context, req *goipp.Message, state int, accepting bool) (*goipp.Message, error) {
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if err := s.Store.UpdateAllPrintersState(ctx, tx, state); err != nil {
			return err
		}
		if err := s.Store.UpdateAllPrintersAccepting(ctx, tx, accepting); err != nil {
			return err
		}
		if err := s.Store.UpdateAllClassesState(ctx, tx, state); err != nil {
			return err
		}
		return s.Store.UpdateAllClassesAccepting(ctx, tx, accepting)
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	return resp, nil
}

func addOperationDefaults(resp *goipp.Message) {
	resp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	resp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
}

func buildOperationDefaults() goipp.Attributes {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	return attrs
}

func addPrinterAttributes(ctx context.Context, resp *goipp.Message, printer model.Printer, r *http.Request, req *goipp.Message, st *store.Store) {
	for _, attr := range buildPrinterAttributes(ctx, printer, r, req, st) {
		resp.Printer.Add(attr)
	}
}

func addClassAttributes(resp *goipp.Message, class model.Class, members []model.Printer, r *http.Request, req *goipp.Message) {
	for _, attr := range classAttributesWithMembers(class, members, r, req) {
		resp.Printer.Add(attr)
	}
}

func buildPrinterAttributes(ctx context.Context, printer model.Printer, r *http.Request, req *goipp.Message, st *store.Store) goipp.Attributes {
	uri := printerURIFor(printer, r)
	attrs := goipp.Attributes{}
	ppd, _ := loadPPDForPrinter(printer)
	documentFormats := supportedDocumentFormats()
	shareServer := sharingEnabled(r, st)
	share := shareServer && printer.Shared
	jobSheetsDefault := strings.TrimSpace(printer.JobSheetsDefault)
	if jobSheetsDefault == "" {
		jobSheetsDefault = "none"
	}
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	ppdName := strings.TrimSpace(printer.PPDName)
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}
	modelName := "CUPS-Golang"
	if ppd != nil {
		switch {
		case ppd.NickName != "":
			modelName = ppd.NickName
		case ppd.Model != "":
			modelName = ppd.Model
		case ppd.Make != "":
			modelName = ppd.Make
		}
	}

	attrs.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(printer.Name)))
	attrs.Add(goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String(uri)))
	attrs.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(printer.State)))
	attrs.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(printer.Accepting)))
	attrs.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(printerStateReason(printer))))
	if printer.Location != "" {
		attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(printer.Location)))
	}
	if printer.Info != "" {
		attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(printer.Info)))
	}
	attrs.Add(goipp.MakeAttribute("ppd-name", goipp.TagName, goipp.String(ppdName)))
	attrs.Add(goipp.MakeAttribute("printer-make-and-model", goipp.TagText, goipp.String(modelName)))
	attrs.Add(makeMimeTypesAttr("document-format-supported", documentFormats))
	attrs.Add(goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String("application/octet-stream")))
	attrs.Add(goipp.MakeAttribute("document-format-preferred", goipp.TagMimeType, goipp.String("application/octet-stream")))
	attrs.Add(goipp.MakeAttribute("charset-configured", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("natural-language-configured", goipp.TagLanguage, goipp.String("en-US")))
	attrs.Add(goipp.MakeAttribute("natural-language-supported", goipp.TagLanguage, goipp.String("en-US")))
	attrs.Add(makeKeywordsAttr("ipp-versions-supported", []string{"1.0", "1.1", "2.0", "2.1"}))
	attrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	attrs.Add(goipp.MakeAttribute("queued-job-count", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("printer-uuid", goipp.TagURI, goipp.String("urn:uuid:00000000-0000-0000-0000-000000000000")))
	attrs.Add(goipp.MakeAttribute("cups-version", goipp.TagText, goipp.String("2.4.16")))
	attrs.Add(goipp.MakeAttribute("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en-US")))
	attrs.Add(goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(printer.ID)))
	geo := strings.TrimSpace(printer.Geo)
	if geo == "" {
		geo = strings.TrimSpace(os.Getenv("CUPS_GEO_LOCATION"))
	}
	if geo != "" {
		attrs.Add(goipp.MakeAttribute("printer-geo-location", goipp.TagURI, goipp.String(geo)))
	} else {
		attrs.Add(goipp.MakeAttribute("printer-geo-location", goipp.TagUnknown, goipp.Void{}))
	}
	org := strings.TrimSpace(printer.Org)
	if org == "" {
		org = strings.TrimSpace(os.Getenv("CUPS_ORGANIZATION"))
	}
	orgUnit := strings.TrimSpace(printer.OrgUnit)
	if orgUnit == "" {
		orgUnit = strings.TrimSpace(os.Getenv("CUPS_ORGANIZATIONAL_UNIT"))
	}
	attrs.Add(goipp.MakeAttribute("printer-organization", goipp.TagText, goipp.String(org)))
	attrs.Add(goipp.MakeAttribute("printer-organizational-unit", goipp.TagText, goipp.String(orgUnit)))
	attrs.Add(goipp.MakeAttribute("printer-strings-languages-supported", goipp.TagLanguage, goipp.String("en-US")))
	attrs.Add(goipp.MakeAttribute("printer-device-id", goipp.TagText, goipp.String("MFG:CUPS-Golang;MDL:Generic;")))
	attrs.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(printer.URI)))
	attrs.Add(goipp.MakeAttribute("destination-uri", goipp.TagURI, goipp.String(uri)))
	attrs.Add(goipp.MakeAttribute("multiple-destination-uris-supported", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(makeKeywordsAttr("uri-security-supported", []string{"none"}))
	attrs.Add(makeKeywordsAttr("uri-authentication-supported", []string{"none"}))
	attrs.Add(makeEnumsAttr("operations-supported", supportedOperations()))
	attrs.Add(goipp.MakeAttribute("printer-is-shared", goipp.TagBoolean, goipp.Boolean(share)))
	attrs.Add(goipp.MakeAttribute("printer-state-message", goipp.TagText, goipp.String(printerStateReason(printer))))
	attrs.Add(makeKeywordsAttr("multiple-document-handling-supported", []string{
		"separate-documents-uncollated-copies", "separate-documents-collated-copies",
	}))
	attrs.Add(goipp.MakeAttribute("multiple-document-jobs-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeKeywordsAttr("compression-supported", []string{"none", "gzip"}))
	attrs.Add(goipp.MakeAttribute("ippget-event-life", goipp.TagInteger, goipp.Integer(15)))
	attrs.Add(goipp.MakeAttribute("job-ids-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("job-priority-supported", goipp.TagInteger, goipp.Integer(100)))
	attrs.Add(goipp.MakeAttribute("job-priority-default", goipp.TagInteger, goipp.Integer(50)))
	attrs.Add(goipp.MakeAttribute("job-account-id-supported", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(goipp.MakeAttribute("job-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("pdf-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(makeKeywordsAttr("pdf-versions-supported", []string{
		"adobe-1.2", "adobe-1.3", "adobe-1.4", "adobe-1.5", "adobe-1.6", "adobe-1.7",
		"iso-19005-1_2005", "iso-32000-1_2008", "pwg-5102.3",
	}))
	attrs.Add(makeKeywordsAttr("ipp-features-supported", []string{
		"ipp-everywhere", "ipp-everywhere-server", "subscription-object",
	}))
	attrs.Add(makeKeywordsAttr("notify-attributes-supported", []string{
		"printer-state-change-time", "notify-lease-expiration-time", "notify-subscriber-user-name",
	}))
	attrs.Add(makeKeywordsAttr("notify-events-supported", []string{
		"job-completed", "job-config-changed", "job-created", "job-progress", "job-state-changed", "job-stopped",
		"printer-added", "printer-changed", "printer-config-changed", "printer-deleted",
		"printer-finishings-changed", "printer-media-changed", "printer-modified", "printer-restarted",
		"printer-shutdown", "printer-state-changed", "printer-stopped",
		"server-audit", "server-restarted", "server-started", "server-stopped",
	}))
	attrs.Add(makeKeywordsAttr("notify-schemes-supported", []string{"ippget"}))
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-default", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("notify-max-events-supported", goipp.TagInteger, goipp.Integer(100)))
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(makeKeywordsAttr("printer-get-attributes-supported", []string{"document-format"}))
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out", goipp.TagInteger, goipp.Integer(60)))
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out-action", goipp.TagKeyword, goipp.String("process-job")))
	attrs.Add(goipp.MakeAttribute("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 999}))
	attrs.Add(goipp.MakeAttribute("copies-default", goipp.TagInteger, goipp.Integer(1)))
	attrs.Add(makeKeywordsAttr("job-sheets-supported", jobSheetsSupported()))
	attrs.Add(makeJobSheetsAttr("job-sheets-default", jobSheetsDefault))
	attrs.Add(makeKeywordsAttr("job-sheets-col-supported", []string{"job-sheets", "media", "media-col"}))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-default", jobSheetsDefault))
	attrs.Add(goipp.MakeAttribute("print-as-raster-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("print-as-raster-default", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(makeKeywordsAttr("job-hold-until-supported", []string{
		"no-hold", "indefinite", "day-time", "evening", "night", "second-shift", "third-shift", "weekend",
	}))
	attrs.Add(makeKeywordsAttr("page-delivery-supported", []string{"reverse-order", "same-order"}))
	attrs.Add(makeKeywordsAttr("print-scaling-supported", []string{"auto", "auto-fit", "fill", "fit", "none"}))
	attrs.Add(makeEnumsAttr("print-quality-supported", []int{3, 4, 5}))
	attrs.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, goipp.Integer(4)))
	attrs.Add(goipp.MakeAttribute("page-ranges-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeEnumsAttr("finishings-supported", []int{3}))
	attrs.Add(goipp.MakeAttribute("finishings-default", goipp.TagEnum, goipp.Integer(3)))
	attrs.Add(makeIntsAttr("number-up-supported", []int{1, 2, 4, 6, 9, 16}))
	attrs.Add(goipp.MakeAttribute("number-up-default", goipp.TagInteger, goipp.Integer(1)))
	attrs.Add(makeKeywordsAttr("number-up-layout-supported", []string{
		"btlr", "btrl", "lrbt", "lrtb", "rlbt", "rltb", "tblr", "tbrl",
	}))
	attrs.Add(makeEnumsAttr("orientation-requested-supported", []int{3, 4, 5, 6}))
	attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagEnum, goipp.Integer(3)))
	attrs.Add(makeKeywordsAttr("job-settable-attributes-supported", []string{
		"copies", "finishings", "job-hold-until", "job-name", "job-priority", "media",
		"media-col", "multiple-document-handling", "number-up", "output-bin",
		"orientation-requested", "page-ranges", "print-color-mode", "print-quality",
		"printer-resolution", "sides",
	}))
	attrs.Add(makeKeywordsAttr("job-creation-attributes-supported", []string{
		"copies", "finishings", "finishings-col", "ipp-attribute-fidelity", "job-hold-until",
		"job-name", "job-priority", "job-sheets", "media", "media-col",
		"multiple-document-handling", "number-up", "number-up-layout", "orientation-requested",
		"output-bin", "page-delivery", "page-ranges", "print-color-mode", "print-quality",
		"print-scaling", "printer-resolution", "sides",
	}))
	attrs.Add(makeKeywordsAttr("printer-settable-attributes-supported", []string{
		"printer-geo-location", "printer-info", "printer-location", "printer-organization",
		"printer-organizational-unit",
	}))
	errorPolicies := printerErrorPolicySupported(false)
	opPolicies := supportedOpPolicies()
	portMonitors := portMonitorSupported(ppd)
	errorPolicyDefault := choiceOrDefault(defaultOpts["printer-error-policy"], errorPolicies, defaultPrinterErrorPolicy(false))
	opPolicyDefault := choiceOrDefault(defaultOpts["printer-op-policy"], opPolicies, defaultPrinterOpPolicy())
	portMonitorDefault := choiceOrDefault(defaultOpts["port-monitor"], portMonitors, defaultPortMonitor())
	attrs.Add(makeNamesAttr("printer-error-policy-supported", errorPolicies))
	attrs.Add(goipp.MakeAttribute("printer-error-policy", goipp.TagName, goipp.String(errorPolicyDefault)))
	attrs.Add(makeNamesAttr("printer-op-policy-supported", opPolicies))
	attrs.Add(goipp.MakeAttribute("printer-op-policy", goipp.TagName, goipp.String(opPolicyDefault)))
	attrs.Add(makeNamesAttr("port-monitor-supported", portMonitors))
	attrs.Add(goipp.MakeAttribute("port-monitor", goipp.TagName, goipp.String(portMonitorDefault)))
	attrs.Add(goipp.MakeAttribute("job-cancel-after-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("jpeg-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("jpeg-x-dimension-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 65535}))
	attrs.Add(goipp.MakeAttribute("jpeg-y-dimension-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 65535}))
	attrs.Add(makeKeywordsAttr("media-col-supported", []string{
		"media-size", "media-type", "media-source", "media-bottom-margin", "media-left-margin",
		"media-right-margin", "media-top-margin",
	}))
	supplyState, supplyDetails := loadPrinterSupplies(ctx, st, printer)
	supplyVals, supplyDesc, markerMsg := buildSupplyAttributes(supplyState, supplyDetails)
	attrs.Add(makeTextsAttr("marker-message", []string{markerMsg}))
	attrs.Add(makeStringsAttr("printer-supply", supplyVals))
	attrs.Add(makeTextsAttr("printer-supply-description", supplyDesc))
	attrs.Add(makeJobPresetsSupportedAttr())
	attrs.Add(makeKeywordsAttr("which-jobs-supported", []string{
		"completed", "not-completed", "aborted", "all", "canceled", "pending", "pending-held",
		"processing", "processing-stopped",
	}))
	attrs.Add(goipp.MakeAttribute("server-is-sharing-printers", goipp.TagBoolean, goipp.Boolean(shareServer)))
	attrs.Add(goipp.MakeAttribute("mopria-certified", goipp.TagText, goipp.String("1.3")))

	caps := computePrinterCaps(ppd, defaultOpts)
	mediaSupported := caps.mediaSupported
	sidesSupported := caps.sidesSupported
	mediaDefault := caps.mediaDefault
	sidesDefault := caps.sidesDefault
	colorModes := caps.colorModes
	colorDefault := caps.colorDefault
	rasterTypes := caps.rasterTypes
	resolutions := caps.resolutions
	resDefault := caps.resDefault
	mediaSources := caps.mediaSources
	mediaSourceDefault := caps.mediaSourceDefault
	mediaTypes := caps.mediaTypes
	mediaTypeDefault := caps.mediaTypeDefault
	outputBins := caps.outputBins
	outputBinDefault := caps.outputBinDefault
	attrs.Add(makeKeywordsAttr("media-supported", mediaSupported))
	attrs.Add(makeKeywordsAttr("media-ready", mediaSupported))
	attrs.Add(goipp.MakeAttribute("media-default", goipp.TagKeyword, goipp.String(mediaDefault)))
	attrs.Add(makeMediaColAttr("media-col-default", mediaDefault))
	attrs.Add(makeMediaColAttr("media-col-ready", mediaDefault))
	attrs.Add(makeKeywordsAttr("media-source-supported", mediaSources))
	attrs.Add(goipp.MakeAttribute("media-source-default", goipp.TagKeyword, goipp.String(mediaSourceDefault)))
	attrs.Add(makeKeywordsAttr("media-type-supported", mediaTypes))
	attrs.Add(goipp.MakeAttribute("media-type-default", goipp.TagKeyword, goipp.String(mediaTypeDefault)))
	attrs.Add(makeMediaColDatabaseAttr("media-col-database", mediaSupported, mediaTypes, mediaSources))
	attrs.Add(makeKeywordsAttr("sides-supported", sidesSupported))
	attrs.Add(goipp.MakeAttribute("sides-default", goipp.TagKeyword, goipp.String(sidesDefault)))
	attrs.Add(makeKeywordsAttr("print-color-mode-supported", colorModes))
	attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String(colorDefault)))
	attrs.Add(makeKeywordsAttr("pwg-raster-document-type-supported", rasterTypes))
	attrs.Add(makeResolutionsAttr("pwg-raster-document-resolution-supported", resolutions))
	attrs.Add(makeResolutionsAttr("printer-resolution-supported", resolutions))
	attrs.Add(goipp.MakeAttribute("printer-resolution-default", goipp.TagResolution, resDefault))
	attrs.Add(makeKeywordsAttr("output-bin-supported", outputBins))
	attrs.Add(goipp.MakeAttribute("output-bin-default", goipp.TagKeyword, goipp.String(outputBinDefault)))
	attrs.Add(makeFinishingsColDatabaseAttr("finishings-col-database", []int{3}))
	attrs.Add(makeKeywordsAttr("urf-supported", urfSupported(resolutions, colorModes, sidesSupported)))
	return filterAttributesForRequest(attrs, req)
}

func buildClassAttributes(class model.Class, r *http.Request) goipp.Attributes {
	uri := classURIFor(class, r)
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(class.Name)))
	attrs.Add(goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String(uri)))
	attrs.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(class.State)))
	attrs.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(class.Accepting)))
	attrs.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(printerStateReason(model.Printer{Accepting: class.Accepting}))))
	if class.Location != "" {
		attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(class.Location)))
	}
	if class.Info != "" {
		attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(class.Info)))
	}
	attrs.Add(makeCharsetsAttr("charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("natural-language-supported", goipp.TagLanguage, goipp.String("en-US")))
	attrs.Add(makeMimeTypesAttr("document-format-supported", supportedDocumentFormats()))
	attrs.Add(goipp.MakeAttribute("printer-make-and-model", goipp.TagText, goipp.String("CUPS-Golang")))
	attrs.Add(makeEnumsAttr("operations-supported", supportedOperations()))
	defaultOpts := parseJobOptions(class.DefaultOptions)
	errorPolicies := printerErrorPolicySupported(true)
	opPolicies := supportedOpPolicies()
	portMonitors := portMonitorSupported(nil)
	errorPolicyDefault := choiceOrDefault(defaultOpts["printer-error-policy"], errorPolicies, defaultPrinterErrorPolicy(true))
	opPolicyDefault := choiceOrDefault(defaultOpts["printer-op-policy"], opPolicies, defaultPrinterOpPolicy())
	portMonitorDefault := choiceOrDefault(defaultOpts["port-monitor"], portMonitors, defaultPortMonitor())
	attrs.Add(makeNamesAttr("printer-error-policy-supported", errorPolicies))
	attrs.Add(goipp.MakeAttribute("printer-error-policy", goipp.TagName, goipp.String(errorPolicyDefault)))
	attrs.Add(makeNamesAttr("printer-op-policy-supported", opPolicies))
	attrs.Add(goipp.MakeAttribute("printer-op-policy", goipp.TagName, goipp.String(opPolicyDefault)))
	attrs.Add(makeNamesAttr("port-monitor-supported", portMonitors))
	attrs.Add(goipp.MakeAttribute("port-monitor", goipp.TagName, goipp.String(portMonitorDefault)))
	jobSheetsDefault := strings.TrimSpace(class.JobSheetsDefault)
	if jobSheetsDefault == "" {
		jobSheetsDefault = "none"
	}
	attrs.Add(makeKeywordsAttr("job-sheets-supported", jobSheetsSupported()))
	attrs.Add(makeJobSheetsAttr("job-sheets-default", jobSheetsDefault))
	attrs.Add(makeKeywordsAttr("job-sheets-col-supported", []string{"job-sheets", "media", "media-col"}))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-default", jobSheetsDefault))
	return attrs
}

func classAttributesWithMembers(class model.Class, members []model.Printer, r *http.Request, req *goipp.Message) goipp.Attributes {
	attrs := buildClassAttributes(class, r)
	if len(members) > 0 {
		names := make([]string, 0, len(members))
		uris := make([]string, 0, len(members))
		for _, p := range members {
			names = append(names, p.Name)
			uris = append(uris, printerURIFor(p, r))
		}
		attrs.Add(makeNamesAttr("member-names", names))
		attrs.Add(makeURIAttr("member-uris", uris))
	}
	return filterAttributesForRequest(attrs, req)
}

func buildSubscriptionAttributes(sub model.Subscription, r *http.Request, s *Server, req *goipp.Message) goipp.Attributes {
	attrs := goipp.Attributes{}
	events := strings.Split(strings.TrimSpace(sub.Events), ",")
	clean := make([]string, 0, len(events))
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		clean = append(clean, e)
	}
	if len(clean) == 0 {
		clean = []string{"all"}
	}
	attrs.Add(makeKeywordsAttr("notify-events", clean))
	if !sub.JobID.Valid {
		attrs.Add(goipp.MakeAttribute("notify-lease-duration", goipp.TagInteger, goipp.Integer(sub.LeaseSecs)))
	}
	if strings.TrimSpace(sub.RecipientURI) != "" {
		attrs.Add(goipp.MakeAttribute("notify-recipient-uri", goipp.TagURI, goipp.String(sub.RecipientURI)))
	} else {
		pull := strings.TrimSpace(sub.PullMethod)
		if pull == "" {
			pull = "ippget"
		}
		attrs.Add(goipp.MakeAttribute("notify-pull-method", goipp.TagKeyword, goipp.String(pull)))
	}
	user := strings.TrimSpace(sub.Owner)
	if user == "" {
		user = "anonymous"
	}
	attrs.Add(goipp.MakeAttribute("notify-subscriber-user-name", goipp.TagName, goipp.String(user)))
	attrs.Add(goipp.MakeAttribute("notify-time-interval", goipp.TagInteger, goipp.Integer(sub.TimeInterval)))
	if len(sub.UserData) > 0 {
		attrs.Add(goipp.MakeAttribute("notify-user-data", goipp.TagString, goipp.Binary(sub.UserData)))
	}
	if sub.JobID.Valid {
		attrs.Add(goipp.MakeAttribute("notify-job-id", goipp.TagInteger, goipp.Integer(sub.JobID.Int64)))
	}
	if sub.PrinterID.Valid && s != nil && r != nil {
		var uri string
		ctx := r.Context()
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			p, err := s.Store.GetPrinterByID(ctx, tx, sub.PrinterID.Int64)
			if err != nil {
				return err
			}
			uri = printerURIFor(p, r)
			return nil
		})
		if uri != "" {
			attrs.Add(goipp.MakeAttribute("notify-printer-uri", goipp.TagURI, goipp.String(uri)))
		}
	}
	attrs.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(sub.ID)))
	return filterAttributesForRequest(attrs, req)
}

func addJobAttributes(resp *goipp.Message, job model.Job, printer model.Printer, r *http.Request, doc model.Document, req *goipp.Message) {
	for _, attr := range buildJobAttributes(job, printer, r, doc, req) {
		resp.Job.Add(attr)
	}
}

func buildJobAttributes(job model.Job, printer model.Printer, r *http.Request, doc model.Document, req *goipp.Message) goipp.Attributes {
	jobURI := jobURIFor(job, r)
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	attrs.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI)))
	attrs.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(job.Name)))
	attrs.Add(goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(job.State)))
	if job.StateReason != "" {
		attrs.Add(goipp.MakeAttribute("job-state-reasons", goipp.TagKeyword, goipp.String(job.StateReason)))
	}
	attrs.Add(goipp.MakeAttribute("job-state-message", goipp.TagText, goipp.String(job.StateReason)))
	attrs.Add(goipp.MakeAttribute("job-originating-user-name", goipp.TagName, goipp.String(job.UserName)))
	if r != nil && r.RemoteAddr != "" {
		attrs.Add(goipp.MakeAttribute("job-originating-host-name", goipp.TagName, goipp.String(r.RemoteAddr)))
	}
	attrs.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String(printerURIFor(printer, r))))
	attrs.Add(goipp.MakeAttribute("job-printer-name", goipp.TagName, goipp.String(printer.Name)))
	attrs.Add(goipp.MakeAttribute("time-at-creation", goipp.TagInteger, goipp.Integer(job.SubmittedAt.Unix())))
	attrs.Add(goipp.MakeAttribute("time-at-processing", goipp.TagInteger, goipp.Integer(job.SubmittedAt.Unix())))
	attrs.Add(goipp.MakeAttribute("job-impressions", goipp.TagInteger, goipp.Integer(job.Impressions)))
	attrs.Add(goipp.MakeAttribute("job-impressions-completed", goipp.TagInteger, goipp.Integer(job.Impressions)))
	if job.CompletedAt != nil {
		attrs.Add(goipp.MakeAttribute("time-at-completed", goipp.TagInteger, goipp.Integer(job.CompletedAt.Unix())))
	}
	if priority := getJobOption(job.Options, "job-priority"); priority != "" {
		if n, err := strconv.Atoi(priority); err == nil {
			attrs.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(n)))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(50)))
	}
	if account := getJobOption(job.Options, "job-account-id"); account != "" {
		attrs.Add(goipp.MakeAttribute("job-account-id", goipp.TagName, goipp.String(account)))
	}
	if holdUntil := getJobOption(job.Options, "job-hold-until"); holdUntil != "" {
		attrs.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String(holdUntil)))
	} else {
		attrs.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String("no-hold")))
	}
	if media := getJobOption(job.Options, "media"); media != "" {
		attrs.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(media)))
		attrs.Add(makeMediaColAttr("media-col", media))
		attrs.Add(makeMediaColAttr("media-col-actual", media))
	}
	if mediaType := getJobOption(job.Options, "media-type"); mediaType != "" {
		attrs.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(mediaType)))
	}
	if mediaSource := getJobOption(job.Options, "media-source"); mediaSource != "" {
		attrs.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(mediaSource)))
	}
	if sides := getJobOption(job.Options, "sides"); sides != "" {
		attrs.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String(sides)))
	}
	if copies := getJobOption(job.Options, "copies"); copies != "" {
		if n, err := strconv.Atoi(copies); err == nil {
			attrs.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	sheets := getJobOption(job.Options, "job-sheets")
	if sheets == "" {
		sheets = printer.JobSheetsDefault
	}
	if strings.TrimSpace(sheets) == "" {
		sheets = "none"
	}
	attrs.Add(makeJobSheetsAttr("job-sheets", sheets))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col", sheets))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-actual", sheets))
	if cancelAfter := getJobOption(job.Options, "job-cancel-after"); cancelAfter != "" {
		if n, err := strconv.Atoi(cancelAfter); err == nil {
			attrs.Add(goipp.MakeAttribute("job-cancel-after", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if quality := getJobOption(job.Options, "print-quality"); quality != "" {
		if n, err := strconv.Atoi(quality); err == nil {
			attrs.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(n)))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))
	}
	if numberUp := getJobOption(job.Options, "number-up"); numberUp != "" {
		if n, err := strconv.Atoi(numberUp); err == nil {
			attrs.Add(goipp.MakeAttribute("number-up", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if layout := getJobOption(job.Options, "number-up-layout"); layout != "" {
		attrs.Add(goipp.MakeAttribute("number-up-layout", goipp.TagKeyword, goipp.String(layout)))
	}
	if scaling := getJobOption(job.Options, "print-scaling"); scaling != "" {
		attrs.Add(goipp.MakeAttribute("print-scaling", goipp.TagKeyword, goipp.String(scaling)))
	}
	if orientation := getJobOption(job.Options, "orientation-requested"); orientation != "" {
		if n, err := strconv.Atoi(orientation); err == nil {
			attrs.Add(goipp.MakeAttribute("orientation-requested", goipp.TagEnum, goipp.Integer(n)))
		}
	}
	if delivery := getJobOption(job.Options, "page-delivery"); delivery != "" {
		attrs.Add(goipp.MakeAttribute("page-delivery", goipp.TagKeyword, goipp.String(delivery)))
	}
	if finishings := getJobOption(job.Options, "finishings"); finishings != "" {
		if n, err := strconv.Atoi(finishings); err == nil {
			attrs.Add(goipp.MakeAttribute("finishings", goipp.TagEnum, goipp.Integer(n)))
		}
	}
	if retries := getJobOption(job.Options, "number-of-retries"); retries != "" {
		if n, err := strconv.Atoi(retries); err == nil {
			attrs.Add(goipp.MakeAttribute("number-of-retries", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if interval := getJobOption(job.Options, "retry-interval"); interval != "" {
		if n, err := strconv.Atoi(interval); err == nil {
			attrs.Add(goipp.MakeAttribute("retry-interval", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if timeout := getJobOption(job.Options, "retry-time-out"); timeout != "" {
		if n, err := strconv.Atoi(timeout); err == nil {
			attrs.Add(goipp.MakeAttribute("retry-time-out", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if confirm := getJobOption(job.Options, "confirmation-sheet-print"); confirm != "" {
		attrs.Add(goipp.MakeAttribute("confirmation-sheet-print", goipp.TagKeyword, goipp.String(confirm)))
	}
	if cover := getJobOption(job.Options, "cover-sheet-info"); cover != "" {
		attrs.Add(goipp.MakeAttribute("cover-sheet-info", goipp.TagKeyword, goipp.String(cover)))
	}
	if bin := getJobOption(job.Options, "output-bin"); bin != "" {
		attrs.Add(goipp.MakeAttribute("output-bin", goipp.TagKeyword, goipp.String(bin)))
	}
	if ranges := getJobOption(job.Options, "page-ranges"); ranges != "" {
		if r, ok := parsePageRanges(ranges); ok {
			attrs.Add(goipp.MakeAttribute("page-ranges", goipp.TagRange, r))
		}
	}
	if colorMode := getJobOption(job.Options, "print-color-mode"); colorMode != "" {
		attrs.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String(colorMode)))
	}
	if raster := getJobOption(job.Options, "print-as-raster"); raster != "" {
		attrs.Add(goipp.MakeAttribute("print-as-raster", goipp.TagBoolean, goipp.Boolean(isTruthy(raster))))
	}
	if doc.ID != 0 {
		attrs.Add(goipp.MakeAttribute("number-of-documents", goipp.TagInteger, goipp.Integer(1)))
		attrs.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(doc.MimeType)))
		if doc.SizeBytes > 0 {
			attrs.Add(goipp.MakeAttribute("job-k-octets", goipp.TagInteger, goipp.Integer((doc.SizeBytes+1023)/1024)))
		}
	}
	return filterAttributesForRequest(attrs, req)
}

func attrString(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return ""
		}
		return attr.Values[0].V.String()
	}
	return ""
}

func attrValue(attrs goipp.Attributes, name string) (string, bool) {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return "", true
		}
		return attr.Values[0].V.String(), true
	}
	return "", false
}

func attrIntPresent(attrs goipp.Attributes, name string) (int64, bool) {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return 0, true
		}
		if v, ok := attr.Values[0].V.(goipp.Integer); ok {
			return int64(v), true
		}
		if v, err := strconv.ParseInt(attr.Values[0].V.String(), 10, 64); err == nil {
			return v, true
		}
		return 0, true
	}
	return 0, false
}

func attrInt(attrs goipp.Attributes, name string) int64 {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return 0
		}
		if v, ok := attr.Values[0].V.(goipp.Integer); ok {
			return int64(v)
		}
		if v, ok := attr.Values[0].V.(goipp.String); ok {
			n, _ := strconv.ParseInt(string(v), 10, 64)
			return n
		}
	}
	return 0
}

func attrBool(attrs goipp.Attributes, name string) bool {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return false
		}
		switch v := attr.Values[0].V.(type) {
		case goipp.Boolean:
			return bool(v)
		case goipp.Integer:
			return v != 0
		case goipp.String:
			s := strings.ToLower(strings.TrimSpace(string(v)))
			return s == "true" || s == "1" || s == "yes"
		default:
			return false
		}
	}
	return false
}

func attrBoolPresent(attrs goipp.Attributes, name string) (bool, bool) {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return false, true
		}
		switch v := attr.Values[0].V.(type) {
		case goipp.Boolean:
			return bool(v), true
		case goipp.Integer:
			return v != 0, true
		default:
			s := strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
			return s == "true" || s == "1" || s == "yes" || s == "on", true
		}
	}
	return false, false
}

func attrStrings(attrs goipp.Attributes, name string) []string {
	out := []string{}
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		for _, v := range attr.Values {
			out = append(out, v.V.String())
		}
	}
	return out
}

func attrBinary(attrs goipp.Attributes, name string) ([]byte, bool) {
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		if len(attr.Values) == 0 {
			return nil, true
		}
		switch v := attr.Values[0].V.(type) {
		case goipp.Binary:
			return []byte(v), true
		case goipp.String:
			return []byte(v), true
		default:
			return []byte(attr.Values[0].V.String()), true
		}
	}
	return nil, false
}

func attrInts(attrs goipp.Attributes, name string) []int64 {
	out := []int64{}
	for _, attr := range attrs {
		if attr.Name != name {
			continue
		}
		for _, v := range attr.Values {
			switch vv := v.V.(type) {
			case goipp.Integer:
				out = append(out, int64(vv))
			case goipp.String:
				if n, err := strconv.ParseInt(string(vv), 10, 64); err == nil {
					out = append(out, n)
				}
			default:
				if s := strings.TrimSpace(v.V.String()); s != "" {
					if n, err := strconv.ParseInt(s, 10, 64); err == nil {
						out = append(out, n)
					}
				}
			}
		}
	}
	return out
}

func parseSubscriptionRequest(req *goipp.Message) (events string, lease int64, recipient string, pullMethod string, interval int64, userData []byte, err error) {
	if req == nil {
		return "", 0, "", "ippget", 0, nil, nil
	}
	events = strings.Join(attrStrings(req.Subscription, "notify-events"), ",")
	lease = int64(attrInt(req.Subscription, "notify-lease-duration"))
	interval = int64(attrInt(req.Subscription, "notify-time-interval"))
	if interval < 0 {
		interval = 0
	}
	recipient = strings.TrimSpace(attrString(req.Subscription, "notify-recipient-uri"))
	pullMethod = strings.TrimSpace(attrString(req.Subscription, "notify-pull-method"))
	if data, ok := attrBinary(req.Subscription, "notify-user-data"); ok {
		userData = data
	}

	if recipient != "" {
		u, parseErr := url.Parse(recipient)
		if parseErr != nil || !strings.EqualFold(u.Scheme, "ippget") {
			return events, lease, recipient, pullMethod, interval, userData, errUnsupported
		}
		pullMethod = ""
		return events, lease, recipient, pullMethod, interval, userData, nil
	}
	if pullMethod == "" {
		pullMethod = "ippget"
	}
	if !strings.EqualFold(pullMethod, "ippget") {
		return events, lease, recipient, pullMethod, interval, userData, errUnsupported
	}
	return events, lease, recipient, pullMethod, interval, userData, nil
}

func joinAttrValues(values goipp.Values, max int) string {
	if len(values) == 0 {
		return ""
	}
	if max <= 0 || max > len(values) {
		max = len(values)
	}
	parts := make([]string, 0, max)
	for i := 0; i < max; i++ {
		parts = append(parts, values[i].V.String())
	}
	return strings.Join(parts, ",")
}

func requestingUserName(req *goipp.Message, r *http.Request) string {
	name := strings.TrimSpace(attrString(req.Operation, "requesting-user-name"))
	if name == "" && r != nil {
		if user, _, ok := r.BasicAuth(); ok {
			name = user
		}
	}
	if name == "" {
		name = "anonymous"
	}
	return name
}

func serializeJobOptions(req *goipp.Message, printer model.Printer) string {
	opts := collectJobOptions(req)
	ppd, _ := loadPPDForPrinter(printer)
	opts = applyPPDDefaults(opts, ppd)
	opts = applyPrinterDefaults(opts, printer)
	b, _ := json.Marshal(opts)
	return string(b)
}

func addClassInternalOptions(optionsJSON string, class model.Class) string {
	defaults := parseJobOptions(class.DefaultOptions)
	policy := strings.TrimSpace(defaults["printer-error-policy"])
	if policy == "" {
		return optionsJSON
	}
	opts := parseJobOptions(optionsJSON)
	if opts == nil {
		opts = map[string]string{}
	}
	opts["cups-error-policy"] = policy
	b, _ := json.Marshal(opts)
	return string(b)
}

func collectJobOptions(req *goipp.Message) map[string]string {
	opts := map[string]string{}
	apply := func(attrs goipp.Attributes, allowAll bool) {
		for _, attr := range attrs {
			if len(attr.Values) == 0 {
				continue
			}
			if attr.Name == "job-sheets" {
				opts["job-sheets"] = joinAttrValues(attr.Values, 2)
				continue
			}
			if attr.Name == "job-sheets-col" {
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if v := collectionString(col, "job-sheets"); v != "" {
						opts["job-sheets"] = v
					}
				}
				continue
			}
			if attr.Name == "media-col" {
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if name := mediaSizeNameFromCollection(col); name != "" {
						opts["media"] = name
					}
					if v := collectionString(col, "media-type"); v != "" {
						opts["media-type"] = v
					}
					if v := collectionString(col, "media-source"); v != "" {
						opts["media-source"] = v
					}
				}
				continue
			}
			if attr.Name == "print-as-raster" {
				opts["print-as-raster"] = attr.Values[0].V.String()
				continue
			}
			if allowAll || strings.HasPrefix(attr.Name, "job-") || attr.Name == "sides" || attr.Name == "media" ||
				attr.Name == "print-as-raster" || attr.Name == "print-quality" || attr.Name == "number-up" ||
				attr.Name == "number-up-layout" || attr.Name == "print-scaling" || attr.Name == "orientation-requested" ||
				attr.Name == "page-delivery" || attr.Name == "print-color-mode" || attr.Name == "finishings" ||
				attr.Name == "output-bin" || attr.Name == "page-ranges" || attr.Name == "number-of-retries" ||
				attr.Name == "retry-interval" || attr.Name == "retry-time-out" || attr.Name == "confirmation-sheet-print" ||
				attr.Name == "cover-sheet-info" || attr.Name == "printer-resolution" {
				opts[attr.Name] = attr.Values[0].V.String()
			}
		}
	}
	if req != nil {
		apply(req.Job, true)
		apply(req.Operation, false)
	}
	return opts
}

func parseJobOptions(optionsJSON string) map[string]string {
	if optionsJSON == "" {
		return map[string]string{}
	}
	var opts map[string]string
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil || opts == nil {
		return map[string]string{}
	}
	return opts
}

func mergeJobOptions(existing string, updates map[string]string) string {
	opts := parseJobOptions(existing)
	for k, v := range updates {
		opts[k] = v
	}
	b, _ := json.Marshal(opts)
	return string(b)
}

func collectJobOptionUpdates(req *goipp.Message) (map[string]string, *string) {
	updates := map[string]string{}
	var namePtr *string

	apply := func(attrs goipp.Attributes) {
		for _, attr := range attrs {
			if attr.Name == "" {
				continue
			}
			if attr.Name == "job-name" {
				if len(attr.Values) > 0 {
					v := attr.Values[0].V.String()
					namePtr = &v
				}
				continue
			}
			if len(attr.Values) == 0 {
				continue
			}
			switch attr.Name {
			case "job-sheets":
				updates["job-sheets"] = joinAttrValues(attr.Values, 2)
			case "media-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if name := mediaSizeNameFromCollection(col); name != "" {
						updates["media"] = name
					}
					if v := collectionString(col, "media-type"); v != "" {
						updates["media-type"] = v
					}
					if v := collectionString(col, "media-source"); v != "" {
						updates["media-source"] = v
					}
				}
			case "page-ranges":
				if r, ok := attr.Values[0].V.(goipp.Range); ok {
					if r.Lower > 0 {
						if r.Upper > r.Lower {
							updates["page-ranges"] = fmt.Sprintf("%d-%d", r.Lower, r.Upper)
						} else {
							updates["page-ranges"] = fmt.Sprintf("%d", r.Lower)
						}
					}
				} else {
					updates["page-ranges"] = attr.Values[0].V.String()
				}
			default:
				switch attr.Name {
				case "copies", "finishings", "job-hold-until", "job-priority", "media", "media-type", "media-source", "multiple-document-handling",
					"number-up", "output-bin", "orientation-requested", "print-color-mode", "print-quality",
					"printer-resolution", "sides", "number-up-layout", "page-delivery", "print-scaling",
					"print-as-raster", "job-sheets", "job-sheets-col", "job-cancel-after",
					"number-of-retries", "retry-interval", "retry-time-out", "confirmation-sheet-print",
					"cover-sheet-info":
					updates[attr.Name] = attr.Values[0].V.String()
				}
			}
		}
	}

	if req != nil {
		apply(req.Job)
		apply(req.Operation)
	}

	return updates, namePtr
}

func applyPPDDefaults(opts map[string]string, ppd *config.PPD) map[string]string {
	if ppd == nil {
		return opts
	}
	if _, ok := opts["media"]; !ok {
		if def, ok := ppd.Defaults["PageSize"]; ok && def != "" {
			opts["media"] = def
		}
	}
	if _, ok := opts["sides"]; !ok {
		if def, ok := ppd.Defaults["Duplex"]; ok && def != "" {
			if strings.Contains(strings.ToLower(def), "tumble") {
				opts["sides"] = "two-sided-short-edge"
			} else if strings.Contains(strings.ToLower(def), "none") {
				opts["sides"] = "one-sided"
			} else {
				opts["sides"] = "two-sided-long-edge"
			}
		}
	}
	if _, ok := opts["media-source"]; !ok {
		if def, ok := ppd.Defaults["InputSlot"]; ok && def != "" {
			opts["media-source"] = def
		}
	}
	if _, ok := opts["media-type"]; !ok {
		if def, ok := ppd.Defaults["MediaType"]; ok && def != "" {
			opts["media-type"] = def
		}
	}
	if _, ok := opts["output-bin"]; !ok {
		if def, ok := ppd.Defaults["OutputBin"]; ok && def != "" {
			opts["output-bin"] = def
		}
	}
	return opts
}

func applyPrinterDefaults(opts map[string]string, printer model.Printer) map[string]string {
	defaults := parseJobOptions(printer.DefaultOptions)
	skip := map[string]bool{
		"printer-error-policy": true,
		"printer-op-policy":    true,
		"port-monitor":         true,
	}
	for k, v := range defaults {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if skip[k] {
			continue
		}
		if _, ok := opts[k]; !ok {
			opts[k] = v
		}
	}
	if _, ok := opts["job-sheets"]; !ok {
		if v := strings.TrimSpace(defaults["job-sheets"]); v != "" {
			opts["job-sheets"] = v
		} else if v := strings.TrimSpace(printer.JobSheetsDefault); v != "" {
			opts["job-sheets"] = v
		}
	}
	return opts
}

type printerCaps struct {
	mediaSupported                    []string
	mediaSources                      []string
	mediaTypes                        []string
	outputBins                        []string
	sidesSupported                    []string
	colorModes                        []string
	resolutions                       []goipp.Resolution
	rasterTypes                       []string
	mediaDefault                      string
	mediaSourceDefault                string
	mediaTypeDefault                  string
	outputBinDefault                  string
	sidesDefault                      string
	colorDefault                      string
	resDefault                        goipp.Resolution
	finishingsSupported               []int
	printQualitySupported             []int
	numberUpSupported                 []int
	orientationSupported              []int
	pageDeliverySupported             []string
	printScalingSupported             []string
	jobHoldUntilSupported             []string
	multipleDocumentHandlingSupported []string
	jobSheetsSupported                []string
}

func computePrinterCaps(ppd *config.PPD, defaultOpts map[string]string) printerCaps {
	caps := printerCaps{
		mediaSupported:                    []string{"A4"},
		sidesSupported:                    []string{"one-sided"},
		mediaDefault:                      "A4",
		sidesDefault:                      "one-sided",
		colorModes:                        []string{"monochrome"},
		colorDefault:                      "monochrome",
		rasterTypes:                       []string{"black_1", "sgray_8"},
		resolutions:                       []goipp.Resolution{{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}},
		mediaSources:                      []string{"auto"},
		mediaSourceDefault:                "auto",
		mediaTypes:                        []string{"auto"},
		mediaTypeDefault:                  "auto",
		outputBins:                        []string{"face-up"},
		outputBinDefault:                  "face-up",
		finishingsSupported:               []int{3},
		printQualitySupported:             []int{3, 4, 5},
		numberUpSupported:                 []int{1, 2, 4, 6, 9, 16},
		orientationSupported:              []int{3, 4, 5, 6},
		pageDeliverySupported:             []string{"reverse-order", "same-order"},
		printScalingSupported:             []string{"auto", "auto-fit", "fill", "fit", "none"},
		jobHoldUntilSupported:             []string{"no-hold", "indefinite", "day-time", "evening", "night", "second-shift", "third-shift", "weekend"},
		multipleDocumentHandlingSupported: []string{"separate-documents-uncollated-copies", "separate-documents-collated-copies"},
		jobSheetsSupported:                jobSheetsSupported(),
	}
	caps.resDefault = caps.resolutions[0]

	if ppd != nil {
		if opts, ok := ppd.Options["PageSize"]; ok && len(opts) > 0 {
			caps.mediaSupported = opts
		}
		if def, ok := ppd.Defaults["PageSize"]; ok && def != "" {
			caps.mediaDefault = def
		}
		if opts, ok := ppd.Options["InputSlot"]; ok && len(opts) > 0 {
			caps.mediaSources = opts
		}
		if def, ok := ppd.Defaults["InputSlot"]; ok && def != "" {
			caps.mediaSourceDefault = def
		}
		if opts, ok := ppd.Options["MediaType"]; ok && len(opts) > 0 {
			caps.mediaTypes = opts
		}
		if def, ok := ppd.Defaults["MediaType"]; ok && def != "" {
			caps.mediaTypeDefault = def
		}
		if opts, ok := ppd.Options["OutputBin"]; ok && len(opts) > 0 {
			caps.outputBins = opts
		}
		if def, ok := ppd.Defaults["OutputBin"]; ok && def != "" {
			caps.outputBinDefault = def
		}
		if _, ok := ppd.Options["Duplex"]; ok {
			caps.sidesSupported = []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}
		}
		if def, ok := ppd.Defaults["Duplex"]; ok {
			if strings.Contains(strings.ToLower(def), "tumble") {
				caps.sidesDefault = "two-sided-short-edge"
			} else if strings.Contains(strings.ToLower(def), "none") {
				caps.sidesDefault = "one-sided"
			} else if def != "" {
				caps.sidesDefault = "two-sided-long-edge"
			}
		}
		if ppd.ColorDevice || hasColorOptions(ppd) {
			caps.colorModes = []string{"monochrome", "color"}
			if ppd.DefaultColorSpace != "" && !strings.Contains(strings.ToLower(ppd.DefaultColorSpace), "gray") {
				caps.colorDefault = "color"
			}
			caps.rasterTypes = []string{"black_1", "sgray_8", "srgb_8"}
		}
		caps.resolutions = []goipp.Resolution{}
		for _, res := range ppd.Resolutions {
			if parsed, ok := parseResolution(res); ok {
				caps.resolutions = appendUniqueResolution(caps.resolutions, parsed)
			}
		}
		if def := firstNonEmpty(ppd.DefaultResolution, ppd.Defaults["Resolution"]); def != "" {
			if parsed, ok := parseResolution(def); ok {
				caps.resDefault = parsed
				caps.resolutions = appendUniqueResolution(caps.resolutions, parsed)
			}
		}
		if len(caps.resolutions) == 0 {
			caps.resolutions = []goipp.Resolution{{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}}
			caps.resDefault = caps.resolutions[0]
		} else if caps.resDefault.Xres == 0 {
			caps.resDefault = caps.resolutions[0]
		}
	}

	if v := strings.TrimSpace(defaultOpts["media"]); v != "" {
		caps.mediaDefault = v
	}
	if v := strings.TrimSpace(defaultOpts["media-source"]); v != "" {
		caps.mediaSourceDefault = v
	}
	if v := strings.TrimSpace(defaultOpts["media-type"]); v != "" {
		caps.mediaTypeDefault = v
	}
	if v := strings.TrimSpace(defaultOpts["output-bin"]); v != "" {
		caps.outputBinDefault = v
	}
	if v := strings.TrimSpace(defaultOpts["sides"]); v != "" {
		caps.sidesDefault = normalizePPDChoice("sides", v)
	}
	if v := strings.TrimSpace(defaultOpts["print-color-mode"]); v != "" {
		caps.colorDefault = normalizePPDChoice("print-color-mode", v)
	}
	if v := strings.TrimSpace(defaultOpts["printer-resolution"]); v != "" {
		if parsed, ok := parseResolution(v); ok {
			caps.resDefault = parsed
			caps.resolutions = appendUniqueResolution(caps.resolutions, parsed)
		}
	}

	caps.mediaSupported = ensureStringInList(caps.mediaSupported, caps.mediaDefault)
	caps.mediaSources = ensureStringInList(caps.mediaSources, caps.mediaSourceDefault)
	caps.mediaTypes = ensureStringInList(caps.mediaTypes, caps.mediaTypeDefault)
	caps.outputBins = ensureStringInList(caps.outputBins, caps.outputBinDefault)
	caps.sidesSupported = ensureStringInList(caps.sidesSupported, caps.sidesDefault)
	caps.colorModes = ensureStringInList(caps.colorModes, caps.colorDefault)
	if caps.resDefault.Xres > 0 {
		caps.resolutions = appendUniqueResolution(caps.resolutions, caps.resDefault)
	}
	return caps
}

func ensureStringInList(values []string, value string) []string {
	if strings.TrimSpace(value) == "" {
		return values
	}
	for _, v := range values {
		if strings.EqualFold(v, value) {
			return values
		}
	}
	return append(values, value)
}

func applyClassDefaultsToPrinter(printer model.Printer, class model.Class) model.Printer {
	if strings.TrimSpace(class.DefaultOptions) != "" {
		printer.DefaultOptions = mergeDefaultOptions(printer.DefaultOptions, class.DefaultOptions)
	}
	if strings.TrimSpace(class.JobSheetsDefault) != "" {
		printer.JobSheetsDefault = class.JobSheetsDefault
	}
	return printer
}

func mergeDefaultOptions(baseJSON, overrideJSON string) string {
	base := parseJobOptions(baseJSON)
	override := parseJobOptions(overrideJSON)
	if len(override) == 0 {
		return baseJSON
	}
	if base == nil {
		base = map[string]string{}
	}
	for k, v := range override {
		if strings.TrimSpace(v) == "" {
			continue
		}
		base[k] = v
	}
	if len(base) == 0 {
		return ""
	}
	merged, err := json.Marshal(base)
	if err != nil {
		return baseJSON
	}
	return string(merged)
}

func defaultPrinterErrorPolicy(isClass bool) string {
	if isClass {
		return "retry-current-job"
	}
	return "stop-printer"
}

func defaultPrinterOpPolicy() string {
	return "default"
}

func defaultPortMonitor() string {
	return "none"
}

func supportedOpPolicies() []string {
	policyNamesOnce.Do(func() {
		policy := config.LoadPolicy(appConfig().ConfDir)
		for _, name := range policy.Policies {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			seen := false
			for _, existing := range policyNames {
				if strings.EqualFold(existing, name) {
					seen = true
					break
				}
			}
			if !seen {
				policyNames = append(policyNames, name)
			}
		}
	})
	if len(policyNames) == 0 {
		return []string{"default"}
	}
	hasDefault := false
	for _, name := range policyNames {
		if strings.EqualFold(name, "default") {
			hasDefault = true
			break
		}
	}
	if hasDefault {
		return append([]string{}, policyNames...)
	}
	return append([]string{"default"}, policyNames...)
}

func printerErrorPolicySupported(isClass bool) []string {
	if isClass {
		return []string{"retry-current-job"}
	}
	return []string{"abort-job", "retry-current-job", "retry-job", "stop-printer"}
}

func portMonitorSupported(ppd *config.PPD) []string {
	seen := map[string]bool{"none": true}
	out := []string{"none"}
	if ppd != nil {
		for _, v := range ppd.PortMonitors {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			key := strings.ToLower(v)
			if !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
		}
		if hasProtocol(ppd.Protocols, "TBCP") {
			if !seen["tbcp"] {
				seen["tbcp"] = true
				out = append(out, "tbcp")
			}
		} else if hasProtocol(ppd.Protocols, "BCP") {
			if !seen["bcp"] {
				seen["bcp"] = true
				out = append(out, "bcp")
			}
		}
	}
	return out
}

func hasProtocol(protocols []string, value string) bool {
	for _, p := range protocols {
		if strings.EqualFold(p, value) {
			return true
		}
	}
	return false
}

func choiceOrDefault(value string, supported []string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	for _, s := range supported {
		if strings.EqualFold(s, value) {
			return s
		}
	}
	return fallback
}

func loadPPDForPrinter(printer model.Printer) (*config.PPD, error) {
	ppdName := strings.TrimSpace(printer.PPDName)
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}
	ppdPath := safePPDPath(appConfig().PPDDir, ppdName)
	ppd, err := config.LoadPPD(ppdPath)
	if err == nil {
		return ppd, nil
	}
	if ppdName != model.DefaultPPDName {
		fallback := safePPDPath(appConfig().PPDDir, model.DefaultPPDName)
		if ppd2, err2 := config.LoadPPD(fallback); err2 == nil {
			return ppd2, nil
		}
	}
	return nil, err
}

func listPPDNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), ".ppd") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func safePPDPath(baseDir, name string) string {
	clean := filepath.Clean(strings.TrimSpace(name))
	if clean == "" || clean == "." {
		clean = filepath.Base(name)
	}
	if filepath.IsAbs(clean) {
		clean = filepath.Base(clean)
	}
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(os.PathSeparator)) {
		clean = filepath.Base(clean)
	}
	full := filepath.Clean(filepath.Join(baseDir, clean))
	base := filepath.Clean(baseDir)
	baseLower := strings.ToLower(base)
	fullLower := strings.ToLower(full)
	if fullLower != baseLower && !strings.HasPrefix(fullLower, strings.ToLower(base+string(os.PathSeparator))) {
		return filepath.Clean(filepath.Join(baseDir, filepath.Base(name)))
	}
	return full
}

func validateRequestOptions(req *goipp.Message, printer model.Printer) error {
	if err := validateIppOptions(req, printer); err != nil {
		return err
	}
	ppd, err := loadPPDForPrinter(printer)
	if err != nil || ppd == nil || len(ppd.Constraints) == 0 {
		return nil
	}
	opts := collectJobOptions(req)
	opts = applyPPDDefaults(opts, ppd)
	opts = applyPrinterDefaults(opts, printer)
	return validatePPDConstraints(ppd, opts)
}

func validateIppOptions(req *goipp.Message, printer model.Printer) error {
	if req == nil {
		return nil
	}
	ppd, _ := loadPPDForPrinter(printer)
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)

	checkAttr := func(attr goipp.Attribute) error {
		if attr.Name == "" || len(attr.Values) == 0 {
			return nil
		}
		switch attr.Name {
		case "document-format":
			if !isDocumentFormatSupported(attr.Values[0].V.String()) {
				return errUnsupported
			}
		case "copies":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if n < 1 || n > 999 {
					return errUnsupported
				}
			}
		case "job-priority":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if n < 1 || n > 100 {
					return errUnsupported
				}
			}
		case "job-hold-until":
			if !stringInList(attr.Values[0].V.String(), caps.jobHoldUntilSupported) {
				return errUnsupported
			}
		case "job-sheets":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.jobSheetsSupported) {
					return errUnsupported
				}
			}
		case "job-sheets-col":
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				if v := collectionString(col, "job-sheets"); v != "" {
					parts := strings.Split(v, ",")
					for _, p := range parts {
						if !stringInList(strings.TrimSpace(p), caps.jobSheetsSupported) {
							return errUnsupported
						}
					}
				}
			}
		case "media":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.mediaSupported) {
					return errUnsupported
				}
			}
		case "media-source":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.mediaSources) {
					return errUnsupported
				}
			}
		case "media-type":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.mediaTypes) {
					return errUnsupported
				}
			}
		case "media-col":
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				if name := mediaSizeNameFromCollection(col); name != "" {
					if !stringInList(name, caps.mediaSupported) {
						return errUnsupported
					}
				}
				if v := collectionString(col, "media-source"); v != "" {
					if !stringInList(v, caps.mediaSources) {
						return errUnsupported
					}
				}
				if v := collectionString(col, "media-type"); v != "" {
					if !stringInList(v, caps.mediaTypes) {
						return errUnsupported
					}
				}
			}
		case "output-bin":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.outputBins) {
					return errUnsupported
				}
			}
		case "sides":
			if !stringInList(attr.Values[0].V.String(), caps.sidesSupported) {
				return errUnsupported
			}
		case "print-color-mode":
			if !stringInList(attr.Values[0].V.String(), caps.colorModes) {
				return errUnsupported
			}
		case "printer-resolution":
			if res, ok := attr.Values[0].V.(goipp.Resolution); ok {
				if !resolutionSupported(res, caps.resolutions) {
					return errUnsupported
				}
			} else if res, ok := parseResolution(attr.Values[0].V.String()); ok {
				if !resolutionSupported(res, caps.resolutions) {
					return errUnsupported
				}
			}
		case "finishings":
			for _, v := range attr.Values {
				if n, ok := valueInt(v.V); ok {
					if !intInList(n, caps.finishingsSupported) {
						return errUnsupported
					}
				}
			}
		case "print-quality":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if !intInList(n, caps.printQualitySupported) {
					return errUnsupported
				}
			}
		case "number-up":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if !intInList(n, caps.numberUpSupported) {
					return errUnsupported
				}
			}
		case "orientation-requested":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if !intInList(n, caps.orientationSupported) {
					return errUnsupported
				}
			}
		case "page-delivery":
			if !stringInList(attr.Values[0].V.String(), caps.pageDeliverySupported) {
				return errUnsupported
			}
		case "print-scaling":
			if !stringInList(attr.Values[0].V.String(), caps.printScalingSupported) {
				return errUnsupported
			}
		case "multiple-document-handling":
			if !stringInList(attr.Values[0].V.String(), caps.multipleDocumentHandlingSupported) {
				return errUnsupported
			}
		case "page-ranges":
			if _, ok := attr.Values[0].V.(goipp.Range); ok {
				return nil
			}
			if _, ok := parseRangeValue(attr.Values[0].V.String()); !ok {
				return errUnsupported
			}
		case "number-of-retries", "retry-interval", "retry-time-out", "job-cancel-after":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if n < 0 {
					return errUnsupported
				}
			}
		}
		return nil
	}

	apply := func(attrs goipp.Attributes) error {
		for _, attr := range attrs {
			if err := checkAttr(attr); err != nil {
				return err
			}
		}
		return nil
	}

	if err := apply(req.Job); err != nil {
		return err
	}
	if err := apply(req.Operation); err != nil {
		return err
	}
	return nil
}

func validatePPDConstraintsForPrinter(printer model.Printer, optionsJSON string) error {
	ppd, err := loadPPDForPrinter(printer)
	if err != nil || ppd == nil || len(ppd.Constraints) == 0 {
		return nil
	}
	opts := parseJobOptions(optionsJSON)
	opts = applyPPDDefaults(opts, ppd)
	opts = applyPrinterDefaults(opts, printer)
	return validatePPDConstraints(ppd, opts)
}

func validatePPDConstraints(ppd *config.PPD, opts map[string]string) error {
	if ppd == nil || len(ppd.Constraints) == 0 {
		return nil
	}
	for _, c := range ppd.Constraints {
		key1, choice1 := mapConstraintOption(c.Option1, c.Choice1)
		key2, choice2 := mapConstraintOption(c.Option2, c.Choice2)
		if key1 == "" || key2 == "" {
			continue
		}
		val1 := strings.TrimSpace(opts[key1])
		val2 := strings.TrimSpace(opts[key2])
		if constraintMatch(val1, choice1) && constraintMatch(val2, choice2) {
			return fmt.Errorf("ppd constraint %s %s %s %s: %w", c.Option1, c.Choice1, c.Option2, c.Choice2, errPPDConstraint)
		}
	}
	return nil
}

func mapConstraintOption(option, choice string) (string, string) {
	opt := strings.TrimPrefix(strings.TrimSpace(option), "*")
	key := ppdOptionToJobKey(opt)
	if key == "" {
		return "", ""
	}
	return key, normalizePPDChoice(key, choice)
}

func ppdOptionToJobKey(option string) string {
	switch strings.ToLower(strings.TrimSpace(option)) {
	case "pagesize":
		return "media"
	case "inputslot":
		return "media-source"
	case "mediatype":
		return "media-type"
	case "outputbin":
		return "output-bin"
	case "duplex":
		return "sides"
	case "resolution":
		return "printer-resolution"
	case "colormodel", "colormode", "colorspace":
		return "print-color-mode"
	default:
		return ""
	}
}

func normalizePPDChoice(optionKey, choice string) string {
	c := strings.TrimSpace(choice)
	if c == "" {
		return ""
	}
	switch optionKey {
	case "sides":
		switch strings.ToLower(c) {
		case "none", "off", "false", "simplex", "one-sided":
			return "one-sided"
		case "duplexnotumble", "duplexlongedge":
			return "two-sided-long-edge"
		case "duplextumble", "duplexshortedge":
			return "two-sided-short-edge"
		}
	case "print-color-mode":
		lc := strings.ToLower(c)
		if strings.Contains(lc, "gray") || strings.Contains(lc, "mono") || strings.Contains(lc, "black") {
			return "monochrome"
		}
		if lc == "none" || lc == "off" {
			return "monochrome"
		}
		return "color"
	}
	return c
}

func constraintMatch(value, choice string) bool {
	if choice == "" || strings.EqualFold(choice, "any") {
		return true
	}
	if value == "" && strings.EqualFold(choice, "none") {
		return true
	}
	return strings.EqualFold(value, choice)
}

func makeKeywordsAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagKeyword, vals[0], vals[1:]...)
}

func sharingEnabled(r *http.Request, st *store.Store) bool {
	if st == nil {
		return true
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	enabled := true
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		val, err := st.GetSetting(ctx, tx, "_share_printers", "1")
		if err != nil {
			return err
		}
		v := strings.ToLower(strings.TrimSpace(val))
		enabled = v == "1" || v == "true" || v == "yes" || v == "on"
		return nil
	})
	return enabled
}

func (s *Server) userAllowedForPrinter(ctx context.Context, printer model.Printer, user string) bool {
	if s == nil || s.Store == nil {
		return true
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return true
	}
	allowed := ""
	denied := ""
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		keyPrefix := "printer." + strconv.FormatInt(printer.ID, 10)
		allowed, err = s.Store.GetSetting(ctx, tx, keyPrefix+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(ctx, tx, keyPrefix+".denied_users", "")
		return err
	})
	allowedList := splitUserList(allowed)
	deniedList := splitUserList(denied)
	if len(allowedList) > 0 {
		return userInList(user, allowedList)
	}
	if len(deniedList) > 0 {
		return !userInList(user, deniedList)
	}
	return true
}

func (s *Server) userAllowedForClass(ctx context.Context, class model.Class, user string) bool {
	if s == nil || s.Store == nil {
		return true
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return true
	}
	allowed := ""
	denied := ""
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		keyPrefix := "class." + strconv.FormatInt(class.ID, 10)
		allowed, err = s.Store.GetSetting(ctx, tx, keyPrefix+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(ctx, tx, keyPrefix+".denied_users", "")
		return err
	})
	allowedList := splitUserList(allowed)
	deniedList := splitUserList(denied)
	if len(allowedList) > 0 {
		return userInList(user, allowedList)
	}
	if len(deniedList) > 0 {
		return !userInList(user, deniedList)
	}
	return true
}

func (s *Server) selectClassMember(ctx context.Context, classID int64) (model.Printer, error) {
	if s == nil || s.Store == nil {
		return model.Printer{}, sql.ErrNoRows
	}
	var selected model.Printer
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		members, err := s.Store.ListClassMembers(ctx, tx, classID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return sql.ErrNoRows
		}
		for _, p := range members {
			if p.Accepting {
				selected = p
				return nil
			}
		}
		return sql.ErrNoRows
	})
	return selected, err
}

func splitUserList(list string) []string {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil
	}
	parts := strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ';' || r == '\t' || r == '\n' || r == '\r' || r == ' '
	})
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func userInList(user string, list []string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), user) {
			return true
		}
	}
	return false
}

func makeCharsetsAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String("utf-8"))
	}
	return goipp.MakeAttr(name, goipp.TagCharset, vals[0], vals[1:]...)
}

func makeMimeTypesAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String("application/octet-stream"))
	}
	return goipp.MakeAttr(name, goipp.TagMimeType, vals[0], vals[1:]...)
}

func makeURIAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagURI, vals[0], vals[1:]...)
}

func makeNamesAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagName, vals[0], vals[1:]...)
}

func makeStringsAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagString, vals[0], vals[1:]...)
}

func makeTextsAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagText, vals[0], vals[1:]...)
}

func makeResolutionsAttr(name string, values []goipp.Resolution) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi})
	}
	return goipp.MakeAttr(name, goipp.TagResolution, vals[0], vals[1:]...)
}

func makeIntsAttr(name string, values []int) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.Integer(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.Integer(0))
	}
	return goipp.MakeAttr(name, goipp.TagInteger, vals[0], vals[1:]...)
}

func makeEnumsAttr(name string, values []int) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.Integer(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.Integer(0))
	}
	return goipp.MakeAttr(name, goipp.TagEnum, vals[0], vals[1:]...)
}

func makeJobSheetsColAttr(name string, sheets string) goipp.Attribute {
	col := goipp.Collection{}
	values := parseJobSheetsValues(sheets)
	sheetVal := "none"
	if len(values) > 0 {
		sheetVal = values[0]
	}
	col.Add(goipp.MakeAttribute("job-sheets", goipp.TagName, goipp.String(sheetVal)))
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, col)
}

func makeJobSheetsAttr(name string, sheets string) goipp.Attribute {
	values := parseJobSheetsValues(sheets)
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String("none"))
	}
	return goipp.MakeAttr(name, goipp.TagName, vals[0], vals[1:]...)
}

func parseJobSheetsValues(sheets string) []string {
	sheets = strings.TrimSpace(sheets)
	if sheets == "" {
		return []string{"none"}
	}
	parts := strings.Split(sheets, ",")
	out := make([]string, 0, 2)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	if len(out) == 1 {
		out = append(out, "none")
	}
	return out
}

func jobSheetsPair(options string) (string, string) {
	values := parseJobSheetsValues(getJobOption(options, "job-sheets"))
	start := "none"
	end := "none"
	if len(values) > 0 {
		start = values[0]
	}
	if len(values) > 1 {
		end = values[1]
	}
	return start, end
}

func appendBannerDocs(job model.Job, printer model.Printer, docs []model.Document) []model.Document {
	start, end := jobSheetsPair(job.Options)
	out := make([]model.Document, 0, len(docs)+2)
	if start != "" && start != "none" {
		out = append(out, buildBannerDoc(job, printer, "start", start))
	}
	out = append(out, docs...)
	if end != "" && end != "none" {
		out = append(out, buildBannerDoc(job, printer, "end", end))
	}
	return out
}

func buildBannerDoc(job model.Job, printer model.Printer, position, banner string) model.Document {
	content := renderBannerContent(job, printer, banner)
	return model.Document{
		FileName:  fmt.Sprintf("banner-%s-%s.txt", position, sanitizeBannerName(banner)),
		MimeType:  "application/vnd.cups-banner",
		SizeBytes: int64(len(content)),
		Path:      "",
	}
}

func renderBannerTemplateContent(doc model.Document, job model.Job, printer model.Printer) string {
	banner := bannerNameFromDoc(doc)
	if banner == "" {
		banner = "standard"
	}
	return renderBannerContent(job, printer, banner)
}

func renderBannerContent(job model.Job, printer model.Printer, banner string) string {
	if tpl, ok := loadBannerTemplate(banner); ok {
		return applyBannerTemplate(tpl, job, printer)
	}
	return defaultBannerText(job, printer, banner)
}

func loadBannerTemplate(banner string) (string, bool) {
	name := sanitizeBannerName(banner)
	if name == "" || name == "none" {
		return "", false
	}
	baseDir := filepath.Join(appConfig().DataDir, "banners")
	lang := strings.TrimSpace(os.Getenv("CUPS_LANG"))
	if lang == "" {
		lang = strings.TrimSpace(os.Getenv("LANG"))
	}
	lang = strings.Split(lang, ".")[0]
	paths := []string{}
	if lang != "" {
		paths = append(paths, filepath.Join(baseDir, lang, name))
		if idx := strings.Index(lang, "_"); idx > 0 {
			paths = append(paths, filepath.Join(baseDir, lang[:idx], name))
		}
	}
	paths = append(paths, filepath.Join(baseDir, name))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil && len(data) > 0 {
			return string(data), true
		}
	}
	return "", false
}

func applyBannerTemplate(tpl string, job model.Job, printer model.Printer) string {
	out := tpl
	out = strings.ReplaceAll(out, "{?printer-name}", printer.Name)
	out = strings.ReplaceAll(out, "{?job-id}", strconv.FormatInt(job.ID, 10))
	out = strings.ReplaceAll(out, "{?job-originating-user-name}", job.UserName)
	out = strings.ReplaceAll(out, "{?job-name}", job.Name)
	out = strings.ReplaceAll(out, "{?job-impressions}", strconv.Itoa(job.Impressions))
	return out
}

func defaultBannerText(job model.Job, printer model.Printer, banner string) string {
	ts := time.Now().Format(time.RFC3339)
	builder := strings.Builder{}
	builder.WriteString("CUPS-Golang Banner\n")
	builder.WriteString("------------------\n")
	builder.WriteString("Banner: ")
	builder.WriteString(banner)
	builder.WriteString("\n")
	builder.WriteString("Printer: ")
	builder.WriteString(printer.Name)
	builder.WriteString("\n")
	builder.WriteString("Job ID: ")
	builder.WriteString(strconv.FormatInt(job.ID, 10))
	builder.WriteString("\n")
	builder.WriteString("Job Name: ")
	builder.WriteString(job.Name)
	builder.WriteString("\n")
	builder.WriteString("User: ")
	builder.WriteString(job.UserName)
	builder.WriteString("\n")
	builder.WriteString("Time: ")
	builder.WriteString(ts)
	builder.WriteString("\n")
	return builder.String()
}

func bannerNameFromDoc(doc model.Document) string {
	name := doc.FileName
	if strings.HasPrefix(name, "banner-") {
		trim := strings.TrimPrefix(name, "banner-")
		if idx := strings.Index(trim, "-"); idx >= 0 {
			trim = trim[idx+1:]
		}
		trim = strings.TrimSuffix(trim, ".txt")
		return trim
	}
	return strings.TrimSuffix(name, ".txt")
}

func sanitizeBannerName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "banner"
	}
	clean := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			continue
		}
		clean = append(clean, r)
	}
	if len(clean) == 0 {
		return "banner"
	}
	return string(clean)
}

func makeMediaColAttr(name string, media string) goipp.Attribute {
	col := goipp.Collection{}
	if media == "" {
		media = "A4"
	}
	col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollection(media)))
	col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(media)))
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, col)
}

func makeMediaColDatabaseAttr(name string, media []string, mediaTypes []string, mediaSources []string) goipp.Attribute {
	cols := make([]goipp.Value, 0, len(media))
	defType := firstString(mediaTypes)
	defSource := firstString(mediaSources)
	for _, m := range media {
		col := goipp.Collection{}
		if m == "" {
			continue
		}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollection(m)))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(m)))
		if defType != "" {
			col.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(defType)))
		}
		if defSource != "" {
			col.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(defSource)))
		}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollection("A4")))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String("A4")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeFinishingsColDatabaseAttr(name string, finishings []int) goipp.Attribute {
	cols := []goipp.Value{}
	for _, f := range finishings {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishings", goipp.TagEnum, goipp.Integer(f)))
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishings", goipp.TagEnum, goipp.Integer(3)))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func mediaSizeCollection(media string) goipp.Collection {
	col := goipp.Collection{}
	x, y := mediaSizeDimensions(media)
	col.Add(goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(x)))
	col.Add(goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(y)))
	return col
}

func mediaSizeDimensions(media string) (int, int) {
	switch strings.ToLower(strings.TrimSpace(media)) {
	case "letter", "na_letter_8.5x11in":
		return 21590, 27940
	case "legal", "na_legal_8.5x14in":
		return 21590, 35560
	case "a5", "iso_a5_148x210mm":
		return 14800, 21000
	case "a4", "iso_a4_210x297mm":
		fallthrough
	default:
		return 21000, 29700
	}
}

func mediaSizeNameFromCollection(col goipp.Collection) string {
	if name := collectionString(col, "media-size-name"); name != "" {
		return name
	}
	for _, attr := range col {
		if attr.Name != "media-size" || len(attr.Values) == 0 {
			continue
		}
		if sizeCol, ok := attr.Values[0].V.(goipp.Collection); ok {
			x := collectionInt(sizeCol, "x-dimension")
			y := collectionInt(sizeCol, "y-dimension")
			if x > 0 && y > 0 {
				return mediaNameFromDimensions(x, y)
			}
		}
	}
	return ""
}

func mediaNameFromDimensions(x, y int) string {
	switch {
	case approxMedia(x, y, 21000, 29700):
		return "A4"
	case approxMedia(x, y, 21590, 27940):
		return "Letter"
	case approxMedia(x, y, 21590, 35560):
		return "Legal"
	case approxMedia(x, y, 14800, 21000):
		return "A5"
	default:
		return ""
	}
}

func approxMedia(x, y, targetX, targetY int) bool {
	const tol = 200
	return (abs(x-targetX) <= tol && abs(y-targetY) <= tol) || (abs(x-targetY) <= tol && abs(y-targetX) <= tol)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func collectionInt(col goipp.Collection, name string) int {
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		if v, ok := attr.Values[0].V.(goipp.Integer); ok {
			return int(v)
		}
	}
	return 0
}

func makeJobPresetsSupportedAttr() goipp.Attribute {
	defaultCol := goipp.Collection{}
	defaultCol.Add(goipp.MakeAttribute("preset-name", goipp.TagName, goipp.String("Default")))
	defaultCol.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))
	defaultCol.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("one-sided")))
	defaultCol.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("monochrome")))

	highCol := goipp.Collection{}
	highCol.Add(goipp.MakeAttribute("preset-name", goipp.TagName, goipp.String("High Quality")))
	highCol.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(5)))
	highCol.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("one-sided")))
	highCol.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("color")))

	return goipp.MakeAttr("job-presets-supported", goipp.TagBeginCollection, defaultCol, highCol)
}

func supportedDocumentFormats() []string {
	mimeOnce.Do(func() {
		db, err := config.LoadMimeDB(appConfig().ConfDir)
		if err != nil || db == nil || len(db.Types) == 0 {
			mimeTypes = []string{"application/octet-stream"}
			return
		}
		for mt := range db.Types {
			mimeTypes = append(mimeTypes, mt)
		}
		sort.Strings(mimeTypes)
		if len(mimeTypes) == 0 {
			mimeTypes = []string{"application/octet-stream"}
		}
	})
	return mimeTypes
}

func jobSheetsSupported() []string {
	names := []string{"none"}
	dir := filepath.Join(appConfig().DataDir, "banners")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names
	}
	seen := map[string]bool{"none": true}
	extra := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		extra = append(extra, name)
	}
	sort.Strings(extra)
	return append(names, extra...)
}

func supportedOperations() []int {
	ops := []goipp.Op{
		goipp.OpPrintJob,
		goipp.OpValidateJob,
		goipp.OpCreateJob,
		goipp.OpSendDocument,
		goipp.OpCloseJob,
		goipp.OpCancelJob,
		goipp.OpCancelMyJobs,
		goipp.OpGetJobAttributes,
		goipp.OpSetJobAttributes,
		goipp.OpGetJobs,
		goipp.OpGetPrinterAttributes,
		goipp.OpEnablePrinter,
		goipp.OpDisablePrinter,
		goipp.OpSetPrinterAttributes,
		goipp.OpGetPrinterSupportedValues,
		goipp.OpGetDocuments,
		goipp.OpGetDocumentAttributes,
		goipp.OpHoldJob,
		goipp.OpReleaseJob,
		goipp.OpRestartJob,
		goipp.OpResumeJob,
		goipp.OpPausePrinter,
		goipp.OpPausePrinterAfterCurrentJob,
		goipp.OpResumePrinter,
		goipp.OpHoldNewJobs,
		goipp.OpReleaseHeldNewJobs,
		goipp.OpRestartPrinter,
		goipp.OpPurgeJobs,
		goipp.OpCancelJobs,
		goipp.OpPauseAllPrinters,
		goipp.OpPauseAllPrintersAfterCurrentJob,
		goipp.OpResumeAllPrinters,
		goipp.OpRestartSystem,
		goipp.OpCreatePrinterSubscriptions,
		goipp.OpCreateJobSubscriptions,
		goipp.OpGetSubscriptionAttributes,
		goipp.OpGetSubscriptions,
		goipp.OpRenewSubscription,
		goipp.OpGetNotifications,
		goipp.OpCancelSubscription,
		goipp.OpValidateDocument,
		goipp.OpCupsAuthenticateJob,
		goipp.OpCupsGetDefault,
		goipp.OpCupsGetPrinters,
		goipp.OpCupsAddModifyPrinter,
		goipp.OpCupsAcceptJobs,
		goipp.OpCupsRejectJobs,
		goipp.OpCupsMoveJob,
		goipp.OpCupsDeletePrinter,
		goipp.OpCupsGetClasses,
		goipp.OpCupsAddModifyClass,
		goipp.OpCupsDeleteClass,
		goipp.OpCupsSetDefault,
		goipp.OpCupsGetDevices,
		goipp.OpCupsGetPpds,
		goipp.OpCupsGetPpd,
		goipp.OpCupsGetDocument,
	}
	out := make([]int, 0, len(ops))
	for _, op := range ops {
		out = append(out, int(op))
	}
	return out
}

func isDocumentFormatSupported(format string) bool {
	if format == "" || format == "application/octet-stream" {
		return true
	}
	for _, mt := range supportedDocumentFormats() {
		if strings.EqualFold(mt, format) {
			return true
		}
	}
	return false
}

func supportedValueAttributes(printer model.Printer, isClass bool) goipp.Attributes {
	ppd, _ := loadPPDForPrinter(printer)
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)

	attrs := goipp.Attributes{}
	attrs.Add(makeKeywordsAttr("job-sheets", caps.jobSheetsSupported))
	attrs.Add(makeKeywordsAttr("job-sheets-col", []string{"job-sheets", "media", "media-col"}))
	attrs.Add(makeKeywordsAttr("media", caps.mediaSupported))
	attrs.Add(makeKeywordsAttr("media-source", caps.mediaSources))
	attrs.Add(makeKeywordsAttr("media-type", caps.mediaTypes))
	attrs.Add(makeKeywordsAttr("output-bin", caps.outputBins))
	attrs.Add(makeKeywordsAttr("sides", caps.sidesSupported))
	attrs.Add(makeKeywordsAttr("print-color-mode", caps.colorModes))
	attrs.Add(makeResolutionsAttr("printer-resolution", caps.resolutions))
	attrs.Add(makeEnumsAttr("finishings", caps.finishingsSupported))
	attrs.Add(makeEnumsAttr("print-quality", caps.printQualitySupported))
	attrs.Add(makeIntsAttr("number-up", caps.numberUpSupported))
	attrs.Add(makeEnumsAttr("orientation-requested", caps.orientationSupported))
	attrs.Add(makeKeywordsAttr("page-delivery", caps.pageDeliverySupported))
	attrs.Add(makeKeywordsAttr("print-scaling", caps.printScalingSupported))
	attrs.Add(makeKeywordsAttr("job-hold-until", caps.jobHoldUntilSupported))
	attrs.Add(makeKeywordsAttr("multiple-document-handling", caps.multipleDocumentHandlingSupported))
	attrs.Add(makeMimeTypesAttr("document-format", supportedDocumentFormats()))
	attrs.Add(goipp.MakeAttribute("copies", goipp.TagRange, goipp.Range{Lower: 1, Upper: 999}))
	attrs.Add(goipp.MakeAttribute("page-ranges", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("job-priority", goipp.TagRange, goipp.Range{Lower: 1, Upper: 100}))
	attrs.Add(goipp.MakeAttribute("job-cancel-after", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("number-of-retries", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("retry-interval", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	attrs.Add(goipp.MakeAttribute("retry-time-out", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))

	errorPolicies := printerErrorPolicySupported(isClass)
	opPolicies := supportedOpPolicies()
	portMonitors := portMonitorSupported(ppd)
	attrs.Add(makeNamesAttr("printer-error-policy", errorPolicies))
	attrs.Add(makeNamesAttr("printer-op-policy", opPolicies))
	attrs.Add(makeNamesAttr("port-monitor", portMonitors))

	attrs.Add(goipp.MakeAttribute("printer-geo-location", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-organization", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-organizational-unit", goipp.TagAdminDefine, goipp.Void{}))

	return attrs
}

func filterAttributesForRequest(attrs goipp.Attributes, req *goipp.Message) goipp.Attributes {
	if req == nil {
		return attrs
	}
	requested, all := requestedAttributes(req)
	if all {
		return attrs
	}
	out := goipp.Attributes{}
	for _, attr := range attrs {
		if requested[attr.Name] {
			out.Add(attr)
		}
	}
	return out
}

func requestedAttributes(req *goipp.Message) (map[string]bool, bool) {
	values := attrStrings(req.Operation, "requested-attributes")
	if len(values) == 0 {
		return nil, true
	}
	set := map[string]bool{}
	for _, v := range values {
		name := strings.TrimSpace(v)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		switch lower {
		case "all", "printer-description", "printer-defaults", "printer-configuration", "printer-status",
			"job-description", "job-template", "job-status", "subscription-description", "subscription-template":
			return nil, true
		}
		set[name] = true
	}
	if len(set) == 0 {
		return nil, true
	}
	return set, false
}

func stringInList(value string, values []string) bool {
	value = strings.TrimSpace(value)
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), value) {
			return true
		}
	}
	return false
}

func intInList(value int, values []int) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func valueInt(v goipp.Value) (int, bool) {
	switch t := v.(type) {
	case goipp.Integer:
		return int(t), true
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(v.String())); err == nil {
			return n, true
		}
	}
	return 0, false
}

func resolutionSupported(res goipp.Resolution, values []goipp.Resolution) bool {
	for _, v := range values {
		if v.Xres == res.Xres && v.Yres == res.Yres && v.Units == res.Units {
			return true
		}
	}
	return false
}

func parseRangeValue(value string) (goipp.Range, bool) {
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

func appendUniqueResolution(list []goipp.Resolution, res goipp.Resolution) []goipp.Resolution {
	for _, existing := range list {
		if existing.Xres == res.Xres && existing.Yres == res.Yres && existing.Units == res.Units {
			return list
		}
	}
	return append(list, res)
}

func hasColorOptions(ppd *config.PPD) bool {
	if ppd == nil {
		return false
	}
	if len(ppd.ColorSpaces) > 0 {
		return true
	}
	if opts, ok := ppd.Options["ColorModel"]; ok && len(opts) > 0 {
		return true
	}
	if opts, ok := ppd.Options["ColorMode"]; ok && len(opts) > 0 {
		return true
	}
	return false
}

func urfSupported(resolutions []goipp.Resolution, colorModes []string, sides []string) []string {
	urf := []string{"W8"}
	color := false
	for _, c := range colorModes {
		if strings.EqualFold(c, "color") {
			color = true
			break
		}
	}
	if color {
		urf = append(urf, "SRGB24")
	} else {
		urf = append(urf, "SGRAY8")
	}
	maxRes := 0
	for _, r := range resolutions {
		if r.Xres > maxRes {
			maxRes = r.Xres
		}
		if r.Yres > maxRes {
			maxRes = r.Yres
		}
	}
	if maxRes > 0 {
		urf = append(urf, fmt.Sprintf("RS%d", maxRes))
	}
	if len(sides) > 1 {
		urf = append(urf, "DM1")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, v := range urf {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstString(values []string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clampLimit(value int64, fallback, min, max int) int {
	if value <= 0 {
		return fallback
	}
	if value < int64(min) {
		return min
	}
	if value > int64(max) {
		return max
	}
	return int(value)
}

func filterJobs(jobs []model.Job, req *goipp.Message, r *http.Request) []model.Job {
	if req == nil {
		return jobs
	}
	which := strings.ToLower(strings.TrimSpace(attrString(req.Operation, "which-jobs")))
	if which == "" {
		which = "not-completed"
	}
	requestingUser := strings.TrimSpace(attrString(req.Operation, "requesting-user-name"))
	if requestingUser == "" && r != nil {
		if user, _, ok := r.BasicAuth(); ok {
			requestingUser = user
		}
	}
	myJobs := attrBool(req.Operation, "my-jobs")
	filtered := make([]model.Job, 0, len(jobs))
	for _, job := range jobs {
		if myJobs && requestingUser != "" && job.UserName != requestingUser {
			continue
		}
		if !matchWhichJobs(which, job.State) {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

func matchWhichJobs(which string, state int) bool {
	switch which {
	case "all":
		return true
	case "completed":
		return state == 7 || state == 8 || state == 9
	case "pending-held":
		return state == 4
	case "aborted":
		return state == 8
	case "canceled":
		return state == 7
	case "pending":
		return state == 3 || state == 4
	case "processing":
		return state == 5
	case "processing-stopped":
		return state == 6
	case "not-completed":
		return state < 7
	default:
		return true
	}
}

func getJobOption(optionsJSON string, key string) string {
	if optionsJSON == "" {
		return ""
	}
	var opts map[string]string
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return ""
	}
	return opts[key]
}

func jobHoldRequested(optionsJSON string) bool {
	hold := strings.ToLower(strings.TrimSpace(getJobOption(optionsJSON, "job-hold-until")))
	switch hold {
	case "", "no-hold", "none", "resume":
		return false
	default:
		return true
	}
}

func collectionString(col goipp.Collection, name string) string {
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		return attr.Values[0].V.String()
	}
	return ""
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parsePageRanges(value string) (goipp.Range, bool) {
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

func jobIDFromURI(uri string) int64 {
	if uri == "" {
		return 0
	}
	u, err := url.Parse(uri)
	if err != nil {
		return 0
	}
	base := path.Base(u.Path)
	if base == "" {
		return 0
	}
	n, _ := strconv.ParseInt(base, 10, 64)
	return n
}

func (s *Server) resolvePrinter(ctx context.Context, r *http.Request, req *goipp.Message) (model.Printer, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil || dest.IsClass {
		return model.Printer{}, fmt.Errorf("printer not found")
	}
	return dest.Printer, nil
}

func (s *Server) defaultPrinter(ctx context.Context, tx *sql.Tx) (model.Printer, error) {
	var p model.Printer
	var accepting int
	var isDefault int
	var shared int
	var jobSheets string
	var defaultOptions string
	var ppdName string
	err := tx.QueryRowContext(ctx, `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        ORDER BY is_default DESC, id ASC
        LIMIT 1
    `).Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return model.Printer{}, err
	}
	p.Accepting = accepting != 0
	p.Shared = shared != 0
	p.IsDefault = isDefault != 0
	p.PPDName = strings.TrimSpace(ppdName)
	if strings.TrimSpace(jobSheets) == "" {
		jobSheets = "none"
	}
	p.JobSheetsDefault = jobSheets
	p.DefaultOptions = defaultOptions
	return p, nil
}

type destination struct {
	IsClass bool
	Printer model.Printer
	Class   model.Class
}

func (s *Server) resolveDestination(ctx context.Context, r *http.Request, req *goipp.Message) (destination, error) {
	printerName := ""
	className := ""
	printerURI := attrString(req.Operation, "printer-uri")
	if printerURI != "" {
		if u, err := url.Parse(printerURI); err == nil {
			if strings.HasPrefix(u.Path, "/printers/") {
				printerName = strings.TrimPrefix(u.Path, "/printers/")
			} else if strings.HasPrefix(u.Path, "/classes/") {
				className = strings.TrimPrefix(u.Path, "/classes/")
			}
		}
	}
	if printerName == "" && className == "" {
		if strings.HasPrefix(r.URL.Path, "/printers/") {
			printerName = strings.TrimPrefix(r.URL.Path, "/printers/")
		} else if strings.HasPrefix(r.URL.Path, "/classes/") {
			className = strings.TrimPrefix(r.URL.Path, "/classes/")
		}
	}
	var dest destination
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		if printerName != "" {
			dest.Printer, err = s.Store.GetPrinterByName(ctx, tx, printerName)
			return err
		}
		if className != "" {
			dest.Class, err = s.Store.GetClassByName(ctx, tx, className)
			dest.IsClass = true
			return err
		}
		dest, err = s.defaultDestination(ctx, tx)
		return err
	})
	return dest, err
}

func (s *Server) defaultDestination(ctx context.Context, tx *sql.Tx) (destination, error) {
	var dest destination
	row := tx.QueryRowContext(ctx, `
        SELECT 'printer' AS kind, id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        WHERE is_default = 1
        UNION ALL
        SELECT 'class' AS kind, id, name, '' AS uri, '' AS ppd_name, location, info, '' AS geo_location, '' AS organization, '' AS organizational_unit, state, accepting, 0 AS shared, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM classes
        WHERE is_default = 1
        LIMIT 1
    `)
	var kind string
	var accepting int
	var shared int
	var isDefault int
	var createdAt, updatedAt time.Time
	var uri string
	var ppdName string
	var jobSheets string
	var defaultOptions string
	err := row.Scan(&kind, &dest.Printer.ID, &dest.Printer.Name, &uri, &ppdName, &dest.Printer.Location, &dest.Printer.Info, &dest.Printer.Geo, &dest.Printer.Org, &dest.Printer.OrgUnit, &dest.Printer.State, &accepting, &shared, &isDefault, &jobSheets, &defaultOptions, &createdAt, &updatedAt)
	if err == nil && kind == "printer" {
		dest.Printer.URI = uri
		dest.Printer.Accepting = accepting != 0
		dest.Printer.Shared = shared != 0
		dest.Printer.IsDefault = isDefault != 0
		dest.Printer.PPDName = strings.TrimSpace(ppdName)
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		dest.Printer.JobSheetsDefault = jobSheets
		dest.Printer.DefaultOptions = defaultOptions
		dest.Printer.CreatedAt = createdAt
		dest.Printer.UpdatedAt = updatedAt
		dest.IsClass = false
		return dest, nil
	}
	if err == nil && kind == "class" {
		dest.Class.ID = dest.Printer.ID
		dest.Class.Name = dest.Printer.Name
		dest.Class.Location = dest.Printer.Location
		dest.Class.Info = dest.Printer.Info
		dest.Class.State = dest.Printer.State
		dest.Class.Accepting = accepting != 0
		dest.Class.IsDefault = isDefault != 0
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		dest.Class.JobSheetsDefault = jobSheets
		dest.Class.DefaultOptions = defaultOptions
		dest.Class.CreatedAt = createdAt
		dest.Class.UpdatedAt = updatedAt
		dest.IsClass = true
		return dest, nil
	}
	// fallback to first printer
	p, err2 := s.defaultPrinter(ctx, tx)
	if err2 != nil {
		return destination{}, err2
	}
	dest.Printer = p
	dest.IsClass = false
	return dest, nil
}

func printerURIFor(printer model.Printer, r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "localhost:631"
	}
	return fmt.Sprintf("ipp://%s/printers/%s", host, printer.Name)
}

func classURIFor(class model.Class, r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "localhost:631"
	}
	return fmt.Sprintf("ipp://%s/classes/%s", host, class.Name)
}

func jobURIFor(job model.Job, r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "localhost:631"
	}
	return fmt.Sprintf("ipp://%s/jobs/%d", host, job.ID)
}

func printerStateReason(printer model.Printer) string {
	if !printer.Accepting {
		return "paused"
	}
	return "none"
}

func (s *Server) canActAsUser(ctx context.Context, r *http.Request, req *goipp.Message, authType string, user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		user = "anonymous"
	}
	authType = strings.TrimSpace(authType)
	if strings.EqualFold(authType, "none") {
		return true
	}
	u, ok := s.authenticateUser(ctx, r, authType)
	if !ok {
		return false
	}
	if u.IsAdmin {
		return true
	}
	return strings.EqualFold(u.Username, user)
}

func (s *Server) canManageJob(ctx context.Context, r *http.Request, req *goipp.Message, authType string, job model.Job, allowCancelAny bool) bool {
	authType = strings.TrimSpace(authType)
	// When AuthType is none, we treat the requesting-user-name as the identity.
	if strings.EqualFold(authType, "none") {
		requestUser := strings.TrimSpace(attrString(req.Operation, "requesting-user-name"))
		if requestUser == "" {
			requestUser = "anonymous"
		}
		return strings.EqualFold(job.UserName, requestUser)
	}

	u, ok := s.authenticateUser(ctx, r, authType)
	if !ok {
		return false
	}
	if u.IsAdmin {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(u.Username), strings.TrimSpace(job.UserName)) {
		return true
	}
	if allowCancelAny && s.userCancelAnyEnabled(ctx) {
		return true
	}
	return false
}

func (s *Server) canManageSubscription(ctx context.Context, r *http.Request, req *goipp.Message, authType string, owner string) bool {
	authType = strings.TrimSpace(authType)
	if strings.EqualFold(authType, "none") {
		requestUser := strings.TrimSpace(attrString(req.Operation, "requesting-user-name"))
		if requestUser == "" {
			requestUser = "anonymous"
		}
		if owner == "" {
			owner = "anonymous"
		}
		return strings.EqualFold(owner, requestUser)
	}

	u, ok := s.authenticateUser(ctx, r, authType)
	if !ok {
		return false
	}
	if u.IsAdmin {
		return true
	}
	if strings.TrimSpace(owner) == "" {
		owner = "anonymous"
	}
	return strings.EqualFold(strings.TrimSpace(u.Username), strings.TrimSpace(owner))
}

func (s *Server) authenticateUser(ctx context.Context, r *http.Request, authType string) (model.User, bool) {
	if s == nil || s.Store == nil || r == nil {
		return model.User{}, false
	}
	u, ok := s.authenticate(r, authType)
	if !ok {
		return model.User{}, false
	}
	if strings.TrimSpace(u.Username) == "" {
		// AuthType=none returns empty user; treat as anonymous.
		u.Username = "anonymous"
	}
	return u, true
}

func (s *Server) userCancelAnyEnabled(ctx context.Context) bool {
	if s == nil || s.Store == nil {
		return false
	}
	enabled := false
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		val, err := s.Store.GetSetting(ctx, tx, "_user_cancel_any", "0")
		if err != nil {
			return err
		}
		enabled = isTruthy(val)
		return nil
	})
	return enabled
}

func loadPrinterSupplies(ctx context.Context, st *store.Store, printer model.Printer) (string, map[string]string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if st == nil || printer.ID == 0 || strings.TrimSpace(printer.URI) == "" {
		return "", nil
	}

	var cached store.PrinterSupplies
	var hasCached bool
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		cached, hasCached, err = st.GetPrinterSupplies(ctx, tx, printer.ID)
		return err
	})
	if hasCached && !cached.UpdatedAt.IsZero() && time.Since(cached.UpdatedAt) < supplyCacheTTL {
		return cached.State, cached.Details
	}

	b := backend.ForURI(printer.URI)
	if b == nil {
		if hasCached {
			return cached.State, cached.Details
		}
		return "", nil
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	status, err := b.QuerySupplies(qctx, printer)
	if err != nil {
		if hasCached {
			return cached.State, cached.Details
		}
		return "", nil
	}
	now := time.Now().UTC()
	_ = st.WithTx(ctx, false, func(tx *sql.Tx) error {
		return st.UpsertPrinterSupplies(ctx, tx, printer.ID, status.State, status.Details, now)
	})
	return status.State, status.Details
}

type supplyEntry struct {
	desc       string
	level      int
	max        int
	percent    int
	hasLevel   bool
	hasMax     bool
	hasPercent bool
}

func buildSupplyAttributes(state string, details map[string]string) ([]string, []string, string) {
	if details == nil {
		details = map[string]string{}
	}
	entries := map[string]*supplyEntry{}
	for key, val := range details {
		if !strings.HasPrefix(key, "supply.") {
			continue
		}
		rest := strings.TrimPrefix(key, "supply.")
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			continue
		}
		idx := parts[0]
		field := parts[1]
		if idx == "" || field == "" {
			continue
		}
		entry := entries[idx]
		if entry == nil {
			entry = &supplyEntry{}
			entries[idx] = entry
		}
		switch field {
		case "desc":
			entry.desc = val
		case "level":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				entry.level = n
				entry.hasLevel = true
			}
		case "max":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				entry.max = n
				entry.hasMax = true
			}
		case "percent":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				entry.percent = n
				entry.hasPercent = true
			}
		}
	}

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	supplyVals := []string{}
	supplyDesc := []string{}
	for _, k := range keys {
		entry := entries[k]
		desc := strings.TrimSpace(entry.desc)
		if desc == "" {
			desc = "Supply " + k
		}
		supplyDesc = append(supplyDesc, desc)
		parts := []string{k}
		if entry.hasLevel {
			parts = append(parts, "level="+strconv.Itoa(entry.level))
		}
		if entry.hasMax {
			parts = append(parts, "max="+strconv.Itoa(entry.max))
		}
		if entry.hasPercent {
			parts = append(parts, "percent="+strconv.Itoa(entry.percent))
		}
		if entry.desc != "" {
			parts = append(parts, "desc="+entry.desc)
		}
		supplyVals = append(supplyVals, strings.Join(parts, ";"))
	}

	state = strings.TrimSpace(strings.ToLower(state))
	marker := ""
	switch state {
	case "low":
		marker = "Supply low"
	case "empty":
		marker = "Supply empty"
	case "ok":
		marker = ""
	case "":
		marker = ""
	default:
		marker = "Supply " + state
	}
	return supplyVals, supplyDesc, marker
}

func printerNameFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

func (s *Server) printerFromURI(ctx context.Context, tx *sql.Tx, uri string) (model.Printer, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return model.Printer{}, err
	}
	switch {
	case strings.HasPrefix(u.Path, "/printers/"):
		name := strings.TrimPrefix(u.Path, "/printers/")
		if name == "" {
			return model.Printer{}, sql.ErrNoRows
		}
		return s.Store.GetPrinterByName(ctx, tx, name)
	case strings.HasPrefix(u.Path, "/classes/"):
		name := strings.TrimPrefix(u.Path, "/classes/")
		if name == "" {
			return model.Printer{}, sql.ErrNoRows
		}
		class, err := s.Store.GetClassByName(ctx, tx, name)
		if err != nil {
			return model.Printer{}, err
		}
		members, err := s.Store.ListClassMembers(ctx, tx, class.ID)
		if err != nil {
			return model.Printer{}, err
		}
		if len(members) == 0 {
			return model.Printer{}, sql.ErrNoRows
		}
		return members[0], nil
	default:
		name := path.Base(u.Path)
		if name == "" {
			return model.Printer{}, sql.ErrNoRows
		}
		return s.Store.GetPrinterByName(ctx, tx, name)
	}
}
