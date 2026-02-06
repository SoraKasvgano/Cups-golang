package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	pwgMediaOnce     sync.Once
	pwgMediaByName   map[string]mediaSize
	pwgMediaByDims   map[string]string
	finishingsOnce   sync.Once
	finishingsAll    []int
	errNotAuthorized = errors.New("not-authorized")
	errNotPossible   = errors.New("not-possible")
	errBadRequest    = errors.New("bad-request")
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
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
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
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
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
		if uri := attrString(req.Operation, "document-uri"); uri != "" {
			jobID, _ = parseDocumentURI(uri)
		}
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
		attrs := buildDocumentAttributes(allDocs[i], docNum, job, printer, r, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagDocumentGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleGetDocumentAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := int64(0)
	docNum := int(attrInt(req.Operation, "document-number"))
	if uri := attrString(req.Operation, "document-uri"); uri != "" {
		jobID, docNum = parseDocumentURI(uri)
	}
	if jobID == 0 {
		if uri := attrString(req.Operation, "job-uri"); uri != "" {
			jobID = jobIDFromURI(uri)
		} else if uri := attrString(req.Operation, "printer-uri"); uri != "" {
			jobID = attrInt(req.Operation, "job-id")
			if jobID == 0 {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
		}
	}
	if jobID == 0 {
		jobID = attrInt(req.Operation, "job-id")
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
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
	if docNum > len(allDocs) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	doc := allDocs[docNum-1]

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	for _, attr := range buildDocumentAttributes(doc, int64(docNum), job, printer, r, req) {
		resp.Document.Add(attr)
	}
	return resp, nil
}

func buildDocumentAttributes(doc model.Document, docNum int64, job model.Job, printer model.Printer, r *http.Request, req *goipp.Message) goipp.Attributes {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("document-number", goipp.TagInteger, goipp.Integer(docNum)))
	attrs.Add(goipp.MakeAttribute("document-uri", goipp.TagURI, goipp.String(documentURIFor(job.ID, docNum, r))))
	if job.ID != 0 {
		attrs.Add(goipp.MakeAttribute("document-job-id", goipp.TagInteger, goipp.Integer(job.ID)))
		attrs.Add(goipp.MakeAttribute("document-job-uri", goipp.TagURI, goipp.String(jobURIFor(job, r))))
	}
	if printer.ID != 0 {
		attrs.Add(goipp.MakeAttribute("document-printer-uri", goipp.TagURI, goipp.String(printerURIFor(printer, r))))
	}
	attrs.Add(goipp.MakeAttribute("document-state", goipp.TagEnum, goipp.Integer(documentStateForJob(job.State))))
	stateReason := strings.TrimSpace(job.StateReason)
	if stateReason == "" {
		stateReason = "none"
	}
	attrs.Add(goipp.MakeAttribute("document-state-reasons", goipp.TagKeyword, goipp.String(stateReason)))
	attrs.Add(goipp.MakeAttribute("document-state-message", goipp.TagText, goipp.String(stateReason)))
	charset := "utf-8"
	naturalLanguage := "en"
	if req != nil {
		if v := strings.TrimSpace(attrString(req.Operation, "attributes-charset")); v != "" {
			charset = v
		}
		if v := strings.TrimSpace(attrString(req.Operation, "attributes-natural-language")); v != "" {
			naturalLanguage = v
		}
	}
	attrs.Add(goipp.MakeAttribute("document-charset", goipp.TagCharset, goipp.String(charset)))
	attrs.Add(goipp.MakeAttribute("document-natural-language", goipp.TagLanguage, goipp.String(naturalLanguage)))
	if doc.MimeType != "" {
		attrs.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(doc.MimeType)))
		attrs.Add(goipp.MakeAttribute("document-format-detected", goipp.TagMimeType, goipp.String(doc.MimeType)))
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
		ppd, _ := loadPPDForPrinter(printer)
		updates = mapJobOptionsToPWG(updates, ppd)

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
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
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
	caps := computePrinterCaps(ppd, defaultOpts)
	finishingsSupported := caps.finishingsSupported
	if len(finishingsSupported) == 0 {
		finishingsSupported = []int{3}
	}
	finishingsDefault := 3
	if v := strings.TrimSpace(defaultOpts["finishings"]); v != "" {
		if vals := parseFinishingsList(v); len(vals) > 0 {
			finishingsDefault = vals[0]
		}
	}
	if !intInList(finishingsDefault, finishingsSupported) && len(finishingsSupported) > 0 {
		finishingsDefault = finishingsSupported[0]
	}
	finishingsTemplates := finishingsTemplatesFromEnums(finishingsSupported)
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
	attrs.Add(goipp.MakeAttribute("document-charset-default", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("document-charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("document-natural-language-default", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("document-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
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
	attrs.Add(goipp.MakeAttribute("notify-events-default", goipp.TagKeyword, goipp.String("job-completed")))
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
	attrs.Add(goipp.MakeAttribute("job-hold-until-default", goipp.TagKeyword, goipp.String("no-hold")))
	attrs.Add(makeKeywordsAttr("page-delivery-supported", []string{"reverse-order", "same-order"}))
	attrs.Add(makeKeywordsAttr("print-scaling-supported", []string{"auto", "auto-fit", "fill", "fit", "none"}))
	attrs.Add(makeEnumsAttr("print-quality-supported", caps.printQualitySupported))
	attrs.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, goipp.Integer(4)))
	attrs.Add(goipp.MakeAttribute("page-ranges-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeEnumsAttr("finishings-supported", finishingsSupported))
	attrs.Add(goipp.MakeAttribute("finishings-default", goipp.TagEnum, goipp.Integer(finishingsDefault)))
	attrs.Add(goipp.MakeAttribute("finishings-ready", goipp.TagEnum, goipp.Integer(finishingsDefault)))
	attrs.Add(makeKeywordsAttr("finishing-template-supported", finishingsTemplates))
	attrs.Add(makeKeywordsAttr("finishings-col-supported", []string{"finishing-template"}))
	attrs.Add(makeFinishingsColAttr("finishings-col-default", []int{finishingsDefault}))
	attrs.Add(makeFinishingsColAttr("finishings-col-ready", []int{finishingsDefault}))
	attrs.Add(makeIntsAttr("number-up-supported", []int{1, 2, 4, 6, 9, 16}))
	attrs.Add(goipp.MakeAttribute("number-up-default", goipp.TagInteger, goipp.Integer(1)))
	attrs.Add(makeKeywordsAttr("number-up-layout-supported", []string{
		"btlr", "btrl", "lrbt", "lrtb", "rlbt", "rltb", "tblr", "tbrl",
	}))
	attrs.Add(makeEnumsAttr("orientation-requested-supported", []int{3, 4, 5, 6}))
	if v := strings.TrimSpace(defaultOpts["orientation-requested"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagEnum, goipp.Integer(n)))
		} else {
			attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagNoValue, goipp.Void{}))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagNoValue, goipp.Void{}))
	}
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

	mediaDefault := caps.mediaDefault
	mediaSupported := caps.mediaSupported
	if caps.mediaCustomMin != "" {
		mediaSupported = append(mediaSupported, caps.mediaCustomMin)
	}
	if caps.mediaCustomMax != "" {
		mediaSupported = append(mediaSupported, caps.mediaCustomMax)
	}
	mediaFixed := fixedMediaNames(mediaSupported)
	if isCustomSizeName(mediaDefault) && !stringInList(mediaDefault, mediaFixed) {
		mediaFixed = append(mediaFixed, mediaDefault)
	}
	mediaReady := reorderMediaReady(mediaFixed, mediaDefault)
	sidesSupported := caps.sidesSupported
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
	attrs.Add(makeKeywordsAttr("media-ready", mediaReady))
	attrs.Add(goipp.MakeAttribute("media-default", goipp.TagKeyword, goipp.String(mediaDefault)))
	attrs.Add(makeMediaColAttrWithOptions("media-col-default", mediaDefault, mediaTypeDefault, mediaSourceDefault, caps.mediaSizes))
	attrs.Add(makeMediaColReadyAttr("media-col-ready", mediaReady, mediaTypeDefault, caps.mediaSizes))
	attrs.Add(makeKeywordsAttr("media-source-supported", mediaSources))
	attrs.Add(goipp.MakeAttribute("media-source-default", goipp.TagKeyword, goipp.String(mediaSourceDefault)))
	attrs.Add(makeKeywordsAttr("media-type-supported", mediaTypes))
	attrs.Add(goipp.MakeAttribute("media-type-default", goipp.TagKeyword, goipp.String(mediaTypeDefault)))
	attrs.Add(makeMediaColDatabaseAttr("media-col-database", mediaFixed, caps.mediaSizes, caps.mediaCustomRange))
	attrs.Add(makeMediaSizeSupportedAttr("media-size-supported", mediaFixed, caps.mediaSizes, caps.mediaCustomRange))
	hwMargins := []int{0, 0, 0, 0}
	if ppd != nil {
		hwMargins = []int{ppd.HWMargins[0], ppd.HWMargins[1], ppd.HWMargins[2], ppd.HWMargins[3]}
	}
	attrs.Add(makeIntsAttr("media-bottom-margin-supported", mediaMarginValues(caps.mediaSizes, func(s mediaSize) int { return s.Bottom }, hwMargins[1])))
	attrs.Add(makeIntsAttr("media-left-margin-supported", mediaMarginValues(caps.mediaSizes, func(s mediaSize) int { return s.Left }, hwMargins[0])))
	attrs.Add(makeIntsAttr("media-right-margin-supported", mediaMarginValues(caps.mediaSizes, func(s mediaSize) int { return s.Right }, hwMargins[2])))
	attrs.Add(makeIntsAttr("media-top-margin-supported", mediaMarginValues(caps.mediaSizes, func(s mediaSize) int { return s.Top }, hwMargins[3])))
	attrs.Add(makeKeywordsAttr("sides-supported", sidesSupported))
	attrs.Add(goipp.MakeAttribute("sides-default", goipp.TagKeyword, goipp.String(sidesDefault)))
	attrs.Add(makeKeywordsAttr("print-color-mode-supported", colorModes))
	attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String(colorDefault)))
	attrs.Add(makeKeywordsAttr("pwg-raster-document-type-supported", rasterTypes))
	attrs.Add(makeResolutionsAttr("pwg-raster-document-resolution-supported", resolutions))
	if len(sidesSupported) > 1 {
		attrs.Add(goipp.MakeAttribute("pwg-raster-document-sheet-back", goipp.TagKeyword, goipp.String("normal")))
	}
	attrs.Add(makeResolutionsAttr("printer-resolution-supported", resolutions))
	attrs.Add(goipp.MakeAttribute("printer-resolution-default", goipp.TagResolution, resDefault))
	attrs.Add(makeKeywordsAttr("output-bin-supported", outputBins))
	attrs.Add(goipp.MakeAttribute("output-bin-default", goipp.TagKeyword, goipp.String(outputBinDefault)))
	attrs.Add(makeFinishingsColDatabaseAttr("finishings-col-database", finishingsSupported))
	attrs.Add(makeKeywordsAttr("urf-supported", urfSupported(resolutions, colorModes, sidesSupported, finishingsSupported, caps.printQualitySupported)))
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
	stateReason := strings.TrimSpace(job.StateReason)
	if stateReason == "" {
		stateReason = "none"
	}
	attrs.Add(goipp.MakeAttribute("job-state-reasons", goipp.TagKeyword, goipp.String(stateReason)))
	attrs.Add(goipp.MakeAttribute("job-state-message", goipp.TagText, goipp.String(stateReason)))
	attrs.Add(goipp.MakeAttribute("job-originating-user-name", goipp.TagName, goipp.String(job.UserName)))
	if r != nil && r.RemoteAddr != "" {
		host := r.RemoteAddr
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		attrs.Add(goipp.MakeAttribute("job-originating-host-name", goipp.TagName, goipp.String(host)))
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
		mediaType := getJobOption(job.Options, "media-type")
		mediaSource := getJobOption(job.Options, "media-source")
		attrs.Add(makeMediaColAttrWithOptions("media-col", media, mediaType, mediaSource, nil))
		attrs.Add(makeMediaColAttrWithOptions("media-col-actual", media, mediaType, mediaSource, nil))
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
	if finishings := getJobOption(job.Options, "finishings"); finishings != "" {
		if vals := parseFinishingsList(finishings); len(vals) > 0 {
			attrs.Add(makeEnumsAttr("finishings", vals))
			attrs.Add(makeFinishingsColAttr("finishings-col", vals))
			attrs.Add(makeFinishingsColAttr("finishings-col-actual", vals))
		}
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

func joinStrings(values []string, max int) string {
	if len(values) == 0 {
		return ""
	}
	if max <= 0 || max > len(values) {
		max = len(values)
	}
	return strings.Join(values[:max], ",")
}

func joinInts(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
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
	opts = mapJobOptionsToPWG(opts, ppd)
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
					if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
						opts["job-sheets"] = joinStrings(vals, 2)
					}
				}
				continue
			}
			if attr.Name == "media-col" {
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if name := mediaSizeNameFromCollection(col, nil); name != "" {
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
			if attr.Name == "finishings" {
				opts["finishings"] = joinAttrValues(attr.Values, 0)
				continue
			}
			if attr.Name == "finishings-col" {
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if vals := collectionInts(col, "finishings"); len(vals) > 0 {
						opts["finishings"] = joinInts(vals)
					} else if templates := collectionStrings(col, "finishing-template"); len(templates) > 0 {
						enums := make([]int, 0, len(templates))
						for _, t := range templates {
							if n := finishingsNameToEnum(t); n > 0 {
								enums = append(enums, n)
							}
						}
						if len(enums) > 0 {
							opts["finishings"] = joinInts(enums)
						}
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
				if len(attr.Values) > 1 {
					opts[attr.Name] = joinAttrValues(attr.Values, 0)
				} else {
					opts[attr.Name] = attr.Values[0].V.String()
				}
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
			case "job-sheets-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
						updates["job-sheets"] = joinStrings(vals, 2)
					}
				}
			case "finishings":
				updates["finishings"] = joinAttrValues(attr.Values, 0)
			case "finishings-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if vals := collectionInts(col, "finishings"); len(vals) > 0 {
						updates["finishings"] = joinInts(vals)
					} else if templates := collectionStrings(col, "finishing-template"); len(templates) > 0 {
						enums := make([]int, 0, len(templates))
						for _, t := range templates {
							if n := finishingsNameToEnum(t); n > 0 {
								enums = append(enums, n)
							}
						}
						if len(enums) > 0 {
							updates["finishings"] = joinInts(enums)
						}
					}
				}
			case "media-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if name := mediaSizeNameFromCollection(col, nil); name != "" {
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
					if len(attr.Values) > 1 {
						updates[attr.Name] = joinAttrValues(attr.Values, 0)
					} else {
						updates[attr.Name] = attr.Values[0].V.String()
					}
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
	for key, def := range ppd.Defaults {
		def = strings.TrimSpace(def)
		if def == "" {
			continue
		}
		if jobKey := ppdOptionToJobKey(key); jobKey != "" {
			if _, ok := opts[jobKey]; !ok {
				opts[jobKey] = normalizePPDChoice(jobKey, def)
			}
			continue
		}
		if _, ok := opts[key]; !ok {
			opts[key] = def
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

func mapJobOptionsToPWG(opts map[string]string, ppd *config.PPD) map[string]string {
	if ppd == nil || opts == nil {
		return opts
	}
	if v, ok := opts["media"]; ok {
		if mapped := ppdMediaToPWG(ppd, v); mapped != "" {
			opts["media"] = mapped
		}
	}
	if v, ok := opts["media-source"]; ok {
		if _, mapping := pwgMediaSourceChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["media-source"] = mapped
			}
		}
	}
	if v, ok := opts["media-type"]; ok {
		if _, mapping := pwgMediaTypeChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["media-type"] = mapped
			}
		}
	}
	if v, ok := opts["output-bin"]; ok {
		if _, mapping := pwgOutputBinChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["output-bin"] = mapped
			}
		}
	}
	return opts
}

func mapSupportedValue(value string, supported []string, mapping map[string]string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if stringInList(value, supported) {
		return value, true
	}
	if len(mapping) > 0 {
		if mapped := mappedPPDChoice(mapping, value); mapped != "" {
			if stringInList(mapped, supported) {
				return mapped, true
			}
		}
	}
	return "", false
}

func supportsCustomPageSize(ppd *config.PPD) bool {
	if ppd == nil {
		return false
	}
	if opt, ok := ppd.OptionDetails["PageSize"]; ok {
		if opt.Custom || len(opt.CustomParams) > 0 {
			return true
		}
	}
	if opts, ok := ppd.Options["PageSize"]; ok {
		for _, v := range opts {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "custom") {
				return true
			}
		}
	}
	return false
}

type printerCaps struct {
	mediaSupported                    []string
	mediaSizes                        map[string]mediaSize
	mediaCustomMin                    string
	mediaCustomMax                    string
	mediaCustomRange                  mediaCustomRange
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

type mediaSize struct {
	X      int
	Y      int
	Left   int
	Bottom int
	Right  int
	Top    int
}

type mediaCustomRange struct {
	MinW    int
	MinL    int
	MaxW    int
	MaxL    int
	Margins [4]int
}

var ippFinishingsNames = []string{
	"none",
	"staple",
	"punch",
	"cover",
	"bind",
	"saddle-stitch",
	"edge-stitch",
	"fold",
	"trim",
	"bale",
	"booklet-maker",
	"jog-offset",
	"coat",
	"laminate",
	"17",
	"18",
	"19",
	"staple-top-left",
	"staple-bottom-left",
	"staple-top-right",
	"staple-bottom-right",
	"edge-stitch-left",
	"edge-stitch-top",
	"edge-stitch-right",
	"edge-stitch-bottom",
	"staple-dual-left",
	"staple-dual-top",
	"staple-dual-right",
	"staple-dual-bottom",
	"staple-triple-left",
	"staple-triple-top",
	"staple-triple-right",
	"staple-triple-bottom",
	"36",
	"37",
	"38",
	"39",
	"40",
	"41",
	"42",
	"43",
	"44",
	"45",
	"46",
	"47",
	"48",
	"49",
	"bind-left",
	"bind-top",
	"bind-right",
	"bind-bottom",
	"54",
	"55",
	"56",
	"57",
	"58",
	"59",
	"trim-after-pages",
	"trim-after-documents",
	"trim-after-copies",
	"trim-after-job",
	"64",
	"65",
	"66",
	"67",
	"68",
	"69",
	"punch-top-left",
	"punch-bottom-left",
	"punch-top-right",
	"punch-bottom-right",
	"punch-dual-left",
	"punch-dual-top",
	"punch-dual-right",
	"punch-dual-bottom",
	"punch-triple-left",
	"punch-triple-top",
	"punch-triple-right",
	"punch-triple-bottom",
	"punch-quad-left",
	"punch-quad-top",
	"punch-quad-right",
	"punch-quad-bottom",
	"punch-multiple-left",
	"punch-multiple-top",
	"punch-multiple-right",
	"punch-multiple-bottom",
	"fold-accordion",
	"fold-double-gate",
	"fold-gate",
	"fold-half",
	"fold-half-z",
	"fold-left-gate",
	"fold-letter",
	"fold-parallel",
	"fold-poster",
	"fold-right-gate",
	"fold-z",
	"fold-engineering-z",
}

func computePrinterCaps(ppd *config.PPD, defaultOpts map[string]string) printerCaps {
	caps := printerCaps{
		mediaSupported:                    []string{"A4"},
		mediaSizes:                        map[string]mediaSize{},
		mediaCustomMin:                    "",
		mediaCustomMax:                    "",
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
	defaultOpts = mapJobOptionsToPWG(defaultOpts, ppd)
	caps.resDefault = caps.resolutions[0]
	caps.finishingsSupported = finishingsSupportedFromPPD(ppd)
	if len(caps.finishingsSupported) == 0 {
		caps.finishingsSupported = []int{3}
	}

	if ppd != nil {
		if opts, ok := ppd.Options["PageSize"]; ok && len(opts) > 0 {
			mapped := make([]string, 0, len(opts))
			seen := map[string]bool{}
			for _, opt := range opts {
				name := ppdMediaToPWG(ppd, opt)
				if name == "" {
					continue
				}
				key := strings.ToLower(strings.TrimSpace(name))
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				mapped = append(mapped, name)
			}
			if len(mapped) > 0 {
				caps.mediaSupported = mapped
			}
		}
		if len(ppd.PageSizes) > 0 {
			for name, size := range ppd.PageSizes {
				if size.Width > 0 && size.Length > 0 {
					pwgName := pwgMediaNameForSize(name, size.Width, size.Length)
					if pwgName == "" {
						continue
					}
					caps.mediaSizes[strings.ToLower(pwgName)] = mediaSize{
						X:      size.Width,
						Y:      size.Length,
						Left:   size.Left,
						Bottom: size.Bottom,
						Right:  size.Right,
						Top:    size.Top,
					}
				}
			}
		}
		if ppd.CustomMinSize[0] > 0 && ppd.CustomMinSize[1] > 0 {
			caps.mediaCustomMin = formatCustomMediaKeyword("custom_min", ppd.CustomMinSize[0], ppd.CustomMinSize[1])
			caps.mediaCustomRange.MinW = ppd.CustomMinSize[0]
			caps.mediaCustomRange.MinL = ppd.CustomMinSize[1]
		}
		if ppd.CustomMaxSize[0] > 0 && ppd.CustomMaxSize[1] > 0 {
			caps.mediaCustomMax = formatCustomMediaKeyword("custom_max", ppd.CustomMaxSize[0], ppd.CustomMaxSize[1])
			caps.mediaCustomRange.MaxW = ppd.CustomMaxSize[0]
			caps.mediaCustomRange.MaxL = ppd.CustomMaxSize[1]
		}
		if ppd.HWMargins != [4]int{} {
			caps.mediaCustomRange.Margins = ppd.HWMargins
		}
		if def, ok := ppd.Defaults["PageSize"]; ok && def != "" {
			if mapped := ppdMediaToPWG(ppd, def); mapped != "" {
				caps.mediaDefault = mapped
			}
		}
		if opts, ok := ppd.Options["InputSlot"]; ok && len(opts) > 0 {
			if mapped, mapping := pwgMediaSourceChoices(ppd); len(mapped) > 0 {
				caps.mediaSources = mapped
				if def, ok := ppd.Defaults["InputSlot"]; ok && def != "" {
					if mappedDef := mappedPPDChoice(mapping, def); mappedDef != "" {
						caps.mediaSourceDefault = mappedDef
					}
				}
			} else {
				caps.mediaSources = opts
			}
		}
		if def, ok := ppd.Defaults["InputSlot"]; ok && def != "" {
			if caps.mediaSourceDefault == "auto" || caps.mediaSourceDefault == "" {
				if mappedDef := pwgMediaSourceFromPPD(def, def); mappedDef != "" {
					caps.mediaSourceDefault = mappedDef
				} else {
					caps.mediaSourceDefault = def
				}
			}
		}
		if opts, ok := ppd.Options["MediaType"]; ok && len(opts) > 0 {
			if mapped, mapping := pwgMediaTypeChoices(ppd); len(mapped) > 0 {
				caps.mediaTypes = mapped
				if def, ok := ppd.Defaults["MediaType"]; ok && def != "" {
					if mappedDef := mappedPPDChoice(mapping, def); mappedDef != "" {
						caps.mediaTypeDefault = mappedDef
					}
				}
			} else {
				caps.mediaTypes = opts
			}
		}
		if def, ok := ppd.Defaults["MediaType"]; ok && def != "" {
			if caps.mediaTypeDefault == "auto" || caps.mediaTypeDefault == "" {
				mappedDef := pwgUnppdizeName(def, "_")
				if mappedDef != "" {
					caps.mediaTypeDefault = mappedDef
				} else {
					caps.mediaTypeDefault = def
				}
			}
		}
		if opts, ok := ppd.Options["OutputBin"]; ok && len(opts) > 0 {
			if mapped, mapping := pwgOutputBinChoices(ppd); len(mapped) > 0 {
				caps.outputBins = mapped
				if def, ok := ppd.Defaults["OutputBin"]; ok && def != "" {
					if mappedDef := mappedPPDChoice(mapping, def); mappedDef != "" {
						caps.outputBinDefault = mappedDef
					}
				}
			} else {
				caps.outputBins = opts
			}
		}
		if def, ok := ppd.Defaults["OutputBin"]; ok && def != "" {
			if caps.outputBinDefault == "face-up" || caps.outputBinDefault == "" {
				mappedDef := pwgUnppdizeName(def, "_")
				if mappedDef != "" {
					caps.outputBinDefault = mappedDef
				} else {
					caps.outputBinDefault = def
				}
			}
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

	for _, media := range caps.mediaSupported {
		key := strings.ToLower(strings.TrimSpace(media))
		if key == "" {
			continue
		}
		if _, ok := caps.mediaSizes[key]; ok {
			continue
		}
		if size, ok := lookupMediaSize(media); ok {
			caps.mediaSizes[key] = size
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

	if v := strings.TrimSpace(defaultOpts["finishings"]); v != "" {
		if vals := parseFinishingsList(v); len(vals) > 0 {
			for _, n := range vals {
				if !intInList(n, caps.finishingsSupported) {
					caps.finishingsSupported = append(caps.finishingsSupported, n)
				}
			}
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
	if v := strings.TrimSpace(appConfig().ErrorPolicy); v != "" {
		return v
	}
	return "stop-printer"
}

func defaultPrinterOpPolicy() string {
	if v := strings.TrimSpace(appConfig().DefaultPolicy); v != "" {
		return v
	}
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
	opts = mapJobOptionsToPWG(opts, ppd)
	return validatePPDConstraints(ppd, opts)
}

func validateIppOptions(req *goipp.Message, printer model.Printer) error {
	if req == nil {
		return nil
	}
	hasAttr := func(attrs goipp.Attributes, name string) bool {
		for _, attr := range attrs {
			if attr.Name == name {
				return true
			}
		}
		return false
	}
	hasMedia := hasAttr(req.Job, "media") || hasAttr(req.Operation, "media")
	hasMediaCol := hasAttr(req.Job, "media-col") || hasAttr(req.Operation, "media-col")
	if hasMedia && hasMediaCol {
		return errBadRequest
	}
	hasFinishings := hasAttr(req.Job, "finishings") || hasAttr(req.Operation, "finishings")
	hasFinishingsCol := hasAttr(req.Job, "finishings-col") || hasAttr(req.Operation, "finishings-col")
	if hasFinishings && hasFinishingsCol {
		return errBadRequest
	}

	ppd, _ := loadPPDForPrinter(printer)
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)
	finishingsTemplates := finishingsTemplatesFromEnums(caps.finishingsSupported)
	allowCustomMedia := supportsCustomPageSize(ppd)
	var sourceMap map[string]string
	var typeMap map[string]string
	var binMap map[string]string
	if ppd != nil {
		_, sourceMap = pwgMediaSourceChoices(ppd)
		_, typeMap = pwgMediaTypeChoices(ppd)
		_, binMap = pwgOutputBinChoices(ppd)
	}

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
			hold := strings.TrimSpace(attr.Values[0].V.String())
			if strings.EqualFold(hold, "resume") || strings.EqualFold(hold, "none") {
				return nil
			}
			if !stringInList(hold, caps.jobHoldUntilSupported) && !isHoldUntilTime(hold) {
				return errUnsupported
			}
		case "job-sheets":
			for _, v := range attr.Values {
				if !stringInList(v.V.String(), caps.jobSheetsSupported) {
					return errUnsupported
				}
			}
		case "job-sheets-col":
			for _, val := range attr.Values {
				col, ok := val.V.(goipp.Collection)
				if !ok {
					return errUnsupported
				}
				if len(attr.Values) != 1 {
					return errUnsupported
				}
				if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
					if len(vals) > 2 {
						return errUnsupported
					}
					for _, v := range vals {
						if !stringInList(strings.TrimSpace(v), caps.jobSheetsSupported) {
							return errUnsupported
						}
					}
				}
				if v := collectionString(col, "media"); v != "" {
					mapped := v
					if ppd != nil {
						if m := ppdMediaToPWG(ppd, v); m != "" {
							mapped = m
						}
					}
					if !stringInList(mapped, caps.mediaSupported) {
						if allowCustomMedia && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mapped)), "custom") {
							continue
						}
						return errUnsupported
					}
				}
				if mediaCol, ok := collectionCollection(col, "media-col"); ok {
					if err := validateMediaCol(mediaCol, caps, ppd, allowCustomMedia, sourceMap, typeMap); err != nil {
						return err
					}
				}
			}
		case "media":
			for _, v := range attr.Values {
				mediaVal := v.V.String()
				mapped := mediaVal
				if ppd != nil {
					if m := ppdMediaToPWG(ppd, mediaVal); m != "" {
						mapped = m
					}
				}
				if !stringInList(mapped, caps.mediaSupported) {
					if allowCustomMedia && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mapped)), "custom") {
						continue
					}
					return errUnsupported
				}
			}
		case "media-source":
			for _, v := range attr.Values {
				if _, ok := mapSupportedValue(v.V.String(), caps.mediaSources, sourceMap); !ok {
					return errUnsupported
				}
			}
		case "media-type":
			for _, v := range attr.Values {
				if _, ok := mapSupportedValue(v.V.String(), caps.mediaTypes, typeMap); !ok {
					return errUnsupported
				}
			}
		case "media-col":
			if len(attr.Values) != 1 {
				return errUnsupported
			}
			col, ok := attr.Values[0].V.(goipp.Collection)
			if !ok {
				return errUnsupported
			}
			if err := validateMediaCol(col, caps, ppd, allowCustomMedia, sourceMap, typeMap); err != nil {
				return err
			}
		case "output-bin":
			for _, v := range attr.Values {
				if _, ok := mapSupportedValue(v.V.String(), caps.outputBins, binMap); !ok {
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
		case "finishings-col":
			if len(attr.Values) != 1 {
				return errUnsupported
			}
			for _, val := range attr.Values {
				col, ok := val.V.(goipp.Collection)
				if !ok {
					return errUnsupported
				}
				for _, n := range collectionInts(col, "finishings") {
					if !intInList(n, caps.finishingsSupported) {
						return errUnsupported
					}
				}
				for _, t := range collectionStrings(col, "finishing-template") {
					if !stringInList(t, finishingsTemplates) {
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
		if ppd != nil {
			if opt, ok := ppd.OptionDetails[attr.Name]; ok {
				if err := validatePPDOptionValues(ppd, opt, attr); err != nil {
					return err
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

func validateMediaCol(col goipp.Collection, caps printerCaps, ppd *config.PPD, allowCustom bool, sourceMap, typeMap map[string]string) error {
	for _, attr := range col {
		if attr.Name != "media-size" || len(attr.Values) == 0 {
			continue
		}
		sizeCol, ok := attr.Values[0].V.(goipp.Collection)
		if !ok {
			return errUnsupported
		}
		x := collectionInt(sizeCol, "x-dimension")
		y := collectionInt(sizeCol, "y-dimension")
		if x <= 0 || y <= 0 {
			return errUnsupported
		}
	}
	if name := mediaSizeNameFromCollection(col, caps.mediaSizes); name != "" {
		mapped := name
		if ppd != nil {
			if m := ppdMediaToPWG(ppd, name); m != "" {
				mapped = m
			}
		}
		if !stringInList(mapped, caps.mediaSupported) {
			if allowCustom && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mapped)), "custom") {
				// Accept custom media size names when supported by PPD.
			} else {
				return errUnsupported
			}
		}
	}
	if v := collectionString(col, "media-source"); v != "" {
		if _, ok := mapSupportedValue(v, caps.mediaSources, sourceMap); !ok {
			return errUnsupported
		}
	}
	if v := collectionString(col, "media-type"); v != "" {
		if _, ok := mapSupportedValue(v, caps.mediaTypes, typeMap); !ok {
			return errUnsupported
		}
	}
	for _, key := range []string{"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin"} {
		if v := collectionInt(col, key); v < 0 {
			return errUnsupported
		}
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
	opts = mapJobOptionsToPWG(opts, ppd)
	return validatePPDConstraints(ppd, opts)
}

func validatePPDConstraints(ppd *config.PPD, opts map[string]string) error {
	if ppd == nil || len(ppd.Constraints) == 0 {
		return nil
	}
	for _, c := range ppd.Constraints {
		key1, choice1 := mapConstraintOption(ppd, c.Option1, c.Choice1)
		key2, choice2 := mapConstraintOption(ppd, c.Option2, c.Choice2)
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

func mapConstraintOption(ppd *config.PPD, option, choice string) (string, string) {
	opt := strings.TrimPrefix(strings.TrimSpace(option), "*")
	key := ppdOptionToJobKey(opt)
	if key == "" {
		key = opt
	}
	if key == "" {
		return "", ""
	}
	value := normalizePPDChoice(key, choice)
	if value == "" {
		return key, value
	}
	switch key {
	case "media":
		if ppd != nil {
			if mapped := ppdMediaToPWG(ppd, value); mapped != "" {
				value = mapped
			}
		}
	case "media-source":
		if ppd != nil {
			if _, mapping := pwgMediaSourceChoices(ppd); len(mapping) > 0 {
				if mapped := mappedPPDChoice(mapping, value); mapped != "" {
					value = mapped
				}
			}
		}
	case "media-type":
		if ppd != nil {
			if _, mapping := pwgMediaTypeChoices(ppd); len(mapping) > 0 {
				if mapped := mappedPPDChoice(mapping, value); mapped != "" {
					value = mapped
				}
			}
		}
	case "output-bin":
		if ppd != nil {
			if _, mapping := pwgOutputBinChoices(ppd); len(mapping) > 0 {
				if mapped := mappedPPDChoice(mapping, value); mapped != "" {
					value = mapped
				}
			}
		}
	}
	return key, value
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
	if strings.ContainsAny(value, ",; \t\n\r") {
		for _, v := range splitList(value) {
			if strings.EqualFold(v, choice) {
				return true
			}
		}
		return false
	}
	return strings.EqualFold(value, choice)
}

func validatePPDOptionValues(ppd *config.PPD, opt *config.PPDOption, attr goipp.Attribute) error {
	if ppd == nil || opt == nil || len(attr.Values) == 0 {
		return nil
	}
	choices := ppdOptionValues(ppd, opt)
	allowed := map[string]bool{}
	for _, c := range choices {
		allowed[strings.ToLower(c)] = true
	}
	isPickMany := strings.EqualFold(opt.UI, "pickmany")
	if !isPickMany && len(attr.Values) > 1 {
		return errUnsupported
	}
	for _, v := range attr.Values {
		val := strings.TrimSpace(v.V.String())
		if val == "" {
			continue
		}
		if opt.Custom {
			lower := strings.ToLower(val)
			if strings.HasPrefix(lower, "custom") || (strings.HasPrefix(val, "{") && strings.HasSuffix(val, "}")) {
				continue
			}
		}
		if len(allowed) > 0 {
			if !allowed[strings.ToLower(val)] {
				return errUnsupported
			}
		}
	}
	return nil
}

func ppdOptionValues(ppd *config.PPD, opt *config.PPDOption) []string {
	if ppd == nil || opt == nil {
		return nil
	}
	if len(opt.Choices) > 0 {
		out := make([]string, 0, len(opt.Choices))
		for _, c := range opt.Choices {
			out = append(out, c.Choice)
		}
		return out
	}
	if vals, ok := ppd.Options[opt.Keyword]; ok {
		return vals
	}
	return nil
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
	if len(values) == 0 {
		values = []string{"none", "none"}
	} else if len(values) == 1 {
		values = append(values, "none")
	} else if len(values) > 2 {
		values = values[:2]
	}
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	col.Add(goipp.MakeAttr("job-sheets", goipp.TagName, vals[0], vals[1:]...))
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

func makeMediaColAttr(name string, media string, sizes map[string]mediaSize) goipp.Attribute {
	return makeMediaColAttrWithOptions(name, media, "", "", sizes)
}

func makeMediaColAttrWithOptions(name, media, mediaType, mediaSource string, sizes map[string]mediaSize) goipp.Attribute {
	col := goipp.Collection{}
	if media == "" {
		media = "A4"
	}
	col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollectionFor(media, sizes)))
	col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(media)))
	if strings.TrimSpace(mediaType) != "" {
		col.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(mediaType)))
	}
	if strings.TrimSpace(mediaSource) != "" {
		col.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(mediaSource)))
	}
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, col)
}

func makeMediaColReadyAttr(name string, mediaReady []string, mediaType string, sizes map[string]mediaSize) goipp.Attribute {
	cols := make([]goipp.Value, 0, len(mediaReady))
	if len(mediaReady) == 0 {
		mediaReady = []string{"A4"}
	}
	for i, m := range mediaReady {
		if strings.TrimSpace(m) == "" {
			continue
		}
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollectionFor(m, sizes)))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(m)))
		source := "auto"
		if i > 0 {
			source = fmt.Sprintf("auto.%d", i+1)
		}
		col.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(source)))
		if i == 0 && strings.TrimSpace(mediaType) != "" {
			col.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(mediaType)))
		}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollectionFor("A4", sizes)))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String("A4")))
		col.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String("auto")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeMediaColDatabaseAttr(name string, media []string, sizes map[string]mediaSize, custom mediaCustomRange) goipp.Attribute {
	cols := make([]goipp.Value, 0, len(media))
	for _, m := range media {
		col := goipp.Collection{}
		if m == "" {
			continue
		}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollectionFor(m, sizes)))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(m)))
		cols = append(cols, col)
	}
	if custom.MinW > 0 && custom.MaxW > 0 && custom.MinL > 0 && custom.MaxL > 0 {
		mediaCol := goipp.Collection{}
		mediaSize := goipp.Collection{}
		mediaSize.Add(goipp.MakeAttribute("x-dimension", goipp.TagRange, goipp.Range{Lower: custom.MinW, Upper: custom.MaxW}))
		mediaSize.Add(goipp.MakeAttribute("y-dimension", goipp.TagRange, goipp.Range{Lower: custom.MinL, Upper: custom.MaxL}))
		mediaCol.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSize))
		mediaCol.Add(goipp.MakeAttribute("media-bottom-margin", goipp.TagInteger, goipp.Integer(custom.Margins[1])))
		mediaCol.Add(goipp.MakeAttribute("media-left-margin", goipp.TagInteger, goipp.Integer(custom.Margins[0])))
		mediaCol.Add(goipp.MakeAttribute("media-right-margin", goipp.TagInteger, goipp.Integer(custom.Margins[2])))
		mediaCol.Add(goipp.MakeAttribute("media-top-margin", goipp.TagInteger, goipp.Integer(custom.Margins[3])))
		cols = append(cols, mediaCol)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, mediaSizeCollectionFor("A4", sizes)))
		col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String("A4")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeMediaSizeSupportedAttr(name string, media []string, sizes map[string]mediaSize, custom mediaCustomRange) goipp.Attribute {
	cols := make([]goipp.Value, 0, len(media))
	for _, m := range media {
		if strings.TrimSpace(m) == "" {
			continue
		}
		col := goipp.Collection{}
		x, y := mediaSizeDimensionsFor(m, sizes)
		col.Add(goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(x)))
		col.Add(goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(y)))
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(21000)))
		col.Add(goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(29700)))
		cols = append(cols, col)
	}
	if custom.MinW > 0 && custom.MaxW > 0 && custom.MinL > 0 && custom.MaxL > 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("x-dimension", goipp.TagRange, goipp.Range{Lower: custom.MinW, Upper: custom.MaxW}))
		col.Add(goipp.MakeAttribute("y-dimension", goipp.TagRange, goipp.Range{Lower: custom.MinL, Upper: custom.MaxL}))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func mediaMarginValues(sizes map[string]mediaSize, selectValue func(mediaSize) int, fallback int) []int {
	seen := map[int]bool{}
	values := []int{}
	for _, size := range sizes {
		v := selectValue(size)
		if v == 0 && fallback > 0 {
			v = fallback
		}
		if v < 0 {
			continue
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		values = append(values, v)
	}
	if len(values) == 0 {
		return []int{0}
	}
	sort.Ints(values)
	return values
}

func makeFinishingsColDatabaseAttr(name string, finishings []int) goipp.Attribute {
	templates := finishingsTemplatesFromEnums(finishings)
	cols := make([]goipp.Value, 0, len(templates))
	for _, template := range templates {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", goipp.TagKeyword, goipp.String(template)))
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", goipp.TagKeyword, goipp.String("none")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeFinishingsColAttr(name string, finishings []int) goipp.Attribute {
	col := goipp.Collection{}
	templates := finishingsTemplatesFromEnums(finishings)
	template := "none"
	if len(templates) > 0 {
		template = templates[0]
	}
	col.Add(goipp.MakeAttribute("finishing-template", goipp.TagKeyword, goipp.String(template)))
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, col)
}

func mediaSizeCollectionFor(media string, sizes map[string]mediaSize) goipp.Collection {
	col := goipp.Collection{}
	x, y := mediaSizeDimensionsFor(media, sizes)
	col.Add(goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(x)))
	col.Add(goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(y)))
	return col
}

func mediaSizeDimensionsFor(media string, sizes map[string]mediaSize) (int, int) {
	if size, ok := parseCustomMediaSize(media); ok {
		return size.X, size.Y
	}
	if size, ok := mediaSizeLookup(media, sizes); ok {
		return size.X, size.Y
	}
	if size, ok := lookupMediaSize(media); ok {
		return size.X, size.Y
	}
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

func reorderMediaReady(values []string, def string) []string {
	if len(values) == 0 {
		if strings.TrimSpace(def) == "" {
			return values
		}
		return []string{def}
	}
	def = strings.TrimSpace(def)
	if def == "" {
		return values
	}
	out := []string{}
	found := false
	for _, v := range values {
		if strings.EqualFold(v, def) {
			found = true
			continue
		}
		out = append(out, v)
	}
	if found {
		return append([]string{def}, out...)
	}
	return append([]string{def}, values...)
}

func formatCustomMediaKeyword(prefix string, width, length int) string {
	if width <= 0 || length <= 0 {
		return ""
	}
	wmm := float64(width) / 100.0
	lmm := float64(length) / 100.0
	return fmt.Sprintf("%s_%sx%smm", prefix, formatCustomNumber(wmm), formatCustomNumber(lmm))
}

func isCustomRangeKeyword(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "custom_min_") || strings.HasPrefix(lower, "custom_max_")
}

func isCustomSizeName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(lower, "custom.") {
		return true
	}
	if strings.HasPrefix(lower, "custom_") && !isCustomRangeKeyword(lower) {
		return true
	}
	return false
}

func fixedMediaNames(media []string) []string {
	out := make([]string, 0, len(media))
	for _, m := range media {
		if strings.TrimSpace(m) == "" {
			continue
		}
		if isCustomRangeKeyword(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func mediaSizeNameFromCollection(col goipp.Collection, sizes map[string]mediaSize) string {
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
				if name := matchDimsToSizes(x, y, sizes); name != "" {
					return name
				}
				if name := mediaNameFromDimensions(x, y); name != "" {
					return name
				}
				return customMediaNameFromDimensions(x, y)
			}
		}
	}
	return ""
}

var pwgCustomSizeRe = regexp.MustCompile(`(?i)^custom_([0-9.]+)x([0-9.]+)(mm|cm|in|ft|m)?$`)

func parseCustomMediaSize(name string) (mediaSize, bool) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return mediaSize{}, false
	}
	lower := strings.ToLower(raw)
	if isCustomRangeKeyword(lower) {
		return mediaSize{}, false
	}
	matches := customPageSizeRe.FindStringSubmatch(raw)
	if len(matches) >= 3 {
		width, err := strconv.ParseFloat(matches[1], 64)
		if err != nil {
			return mediaSize{}, false
		}
		height, err := strconv.ParseFloat(matches[2], 64)
		if err != nil {
			return mediaSize{}, false
		}
		units := ""
		if len(matches) >= 4 {
			units = strings.ToLower(strings.TrimSpace(matches[3]))
		}
		if units == "" {
			units = "pt"
		}
		scale, ok := unitsToHundredthMM(units)
		if !ok {
			return mediaSize{}, false
		}
		x := int(math.Round(width * scale))
		y := int(math.Round(height * scale))
		if x <= 0 || y <= 0 {
			return mediaSize{}, false
		}
		return mediaSize{X: x, Y: y}, true
	}
	matches = pwgCustomSizeRe.FindStringSubmatch(raw)
	if len(matches) < 3 {
		return mediaSize{}, false
	}
	width, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return mediaSize{}, false
	}
	height, err := strconv.ParseFloat(matches[2], 64)
	if err != nil {
		return mediaSize{}, false
	}
	units := ""
	if len(matches) >= 4 {
		units = strings.ToLower(strings.TrimSpace(matches[3]))
	}
	if units == "" {
		units = "mm"
	}
	scale, ok := unitsToHundredthMM(units)
	if !ok {
		return mediaSize{}, false
	}
	x := int(math.Round(width * scale))
	y := int(math.Round(height * scale))
	if x <= 0 || y <= 0 {
		return mediaSize{}, false
	}
	return mediaSize{X: x, Y: y}, true
}

func unitsToHundredthMM(units string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(units)) {
	case "mm":
		return 100.0, true
	case "cm":
		return 1000.0, true
	case "m":
		return 100000.0, true
	case "in":
		return 25.4 * 100.0, true
	case "ft":
		return 12.0 * 25.4 * 100.0, true
	case "pt":
		return 25.4 * 100.0 / 72.0, true
	default:
		return 0, false
	}
}

func customMediaNameFromDimensions(x, y int) string {
	if x <= 0 || y <= 0 {
		return ""
	}
	mmX := float64(x) / 100.0
	mmY := float64(y) / 100.0
	return fmt.Sprintf("Custom.%sx%smm", formatCustomNumber(mmX), formatCustomNumber(mmY))
}

func mediaNameFromDimensions(x, y int) string {
	if name := lookupMediaNameByDims(x, y); name != "" {
		return name
	}
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

func ppdMediaToPWG(ppd *config.PPD, name string) string {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return ""
	}
	if strings.EqualFold(raw, "Custom") {
		return ""
	}
	if ppd != nil {
		if size, ok := ppdPageSizeForName(ppd, raw); ok && size.Width > 0 && size.Length > 0 {
			return pwgMediaNameForSize(raw, size.Width, size.Length)
		}
	}
	if size, ok := lookupMediaSize(raw); ok {
		if pwg := lookupMediaNameByDims(size.X, size.Y); pwg != "" {
			return pwg
		}
		return raw
	}
	if size, ok := parseCustomMediaSize(raw); ok && size.X > 0 && size.Y > 0 {
		return pwgFormatSizeName("custom", "", size.X, size.Y, "")
	}
	return raw
}

func ppdPageSizeForName(ppd *config.PPD, name string) (config.PPDPageSize, bool) {
	if ppd == nil || len(ppd.PageSizes) == 0 || strings.TrimSpace(name) == "" {
		return config.PPDPageSize{}, false
	}
	if size, ok := ppd.PageSizes[name]; ok {
		return size, true
	}
	for key, size := range ppd.PageSizes {
		if strings.EqualFold(key, name) {
			return size, true
		}
	}
	return config.PPDPageSize{}, false
}

func pwgMediaNameForSize(ppdName string, width, length int) string {
	if width <= 0 || length <= 0 {
		return ""
	}
	if name := lookupMediaNameByDims(width, length); name != "" {
		return name
	}
	ppdName = strings.TrimSpace(ppdName)
	if ppdName == "" {
		return pwgFormatSizeName("", "", width, length, "")
	}
	normalized := pwgUnppdizeName(ppdName, "_.")
	return pwgFormatSizeName("", normalized, width, length, "")
}

func pwgUnppdizeName(ppd string, dashchars string) string {
	if ppd == "" {
		return ""
	}
	if isLower(ppd[0]) {
		valid := true
		for i := 1; i < len(ppd); i++ {
			ch := ppd[i]
			if isUpper(ch) || strings.ContainsRune(dashchars, rune(ch)) ||
				(ch == '-' && (ppd[i-1] == '-' || i == len(ppd)-1)) {
				valid = false
				break
			}
		}
		if valid {
			return ppd
		}
	}
	var sb strings.Builder
	sb.Grow(len(ppd))
	nodash := true
	for i := 0; i < len(ppd); i++ {
		ch := ppd[i]
		if isAlphaNum(ch) || ch == '.' || ch == '_' {
			sb.WriteByte(toLowerASCII(ch))
			nodash = false
		} else if ch == '-' || strings.ContainsRune(dashchars, rune(ch)) {
			if !nodash {
				sb.WriteByte('-')
				nodash = true
			}
		}
		if !nodash && i+1 < len(ppd) {
			next := ppd[i+1]
			if !isUpper(ch) && isAlphaNum(ch) && isUpper(next) {
				sb.WriteByte('-')
				nodash = true
			} else if !isDigit(ch) && isDigit(next) {
				sb.WriteByte('-')
				nodash = true
			}
		}
	}
	out := sb.String()
	out = strings.TrimRight(out, "-")
	return out
}

func pwgFormatSizeName(prefix, name string, width, length int, units string) string {
	if width < 0 || length < 0 {
		return ""
	}
	if units != "" && units != "in" && units != "mm" {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = ""
	} else {
		for i := 0; i < len(name); i++ {
			ch := name[i]
			if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '.' && ch != '-' {
				return ""
			}
		}
	}
	if prefix == "disc" {
		width = 4000
	}
	if units == "" {
		if width%635 == 0 && length%635 == 0 {
			units = "in"
		} else {
			units = "mm"
		}
	}
	if prefix == "" {
		if units == "in" {
			prefix = "oe"
		} else {
			prefix = "om"
		}
	}
	var format func(int) string
	if units == "in" {
		format = pwgFormatInches
	} else {
		format = pwgFormatMillimeters
	}
	size := format(width) + "x" + format(length) + units
	if name == "" {
		name = size
	}
	return prefix + "_" + name + "_" + size
}

func pwgFormatInches(val int) string {
	integer := val / 2540
	fraction := ((val % 2540) * 1000 + 1270) / 2540
	if fraction >= 1000 {
		integer++
		fraction -= 1000
	}
	switch {
	case fraction == 0:
		return strconv.Itoa(integer)
	case fraction%10 != 0:
		return fmt.Sprintf("%d.%03d", integer, fraction)
	case fraction%100 != 0:
		return fmt.Sprintf("%d.%02d", integer, fraction/10)
	default:
		return fmt.Sprintf("%d.%01d", integer, fraction/100)
	}
}

func pwgFormatMillimeters(val int) string {
	integer := val / 100
	fraction := val % 100
	switch {
	case fraction == 0:
		return strconv.Itoa(integer)
	case fraction%10 != 0:
		return fmt.Sprintf("%d.%02d", integer, fraction)
	default:
		return fmt.Sprintf("%d.%01d", integer, fraction/10)
	}
}

func isLower(b byte) bool {
	return b >= 'a' && b <= 'z'
}

func isUpper(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlphaNum(b byte) bool {
	return isLower(b) || isUpper(b) || isDigit(b)
}

func toLowerASCII(b byte) byte {
	if isUpper(b) {
		return b + ('a' - 'A')
	}
	return b
}

func ppdOptionChoiceList(ppd *config.PPD, keyword string) []config.PPDChoice {
	if ppd == nil || strings.TrimSpace(keyword) == "" {
		return nil
	}
	if opt, ok := ppd.OptionDetails[keyword]; ok && len(opt.Choices) > 0 {
		return opt.Choices
	}
	if vals, ok := ppd.Options[keyword]; ok && len(vals) > 0 {
		out := make([]config.PPDChoice, 0, len(vals))
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			out = append(out, config.PPDChoice{Choice: v, Text: v})
		}
		return out
	}
	return nil
}

func mappedPPDChoice(mapping map[string]string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(mapping) == 0 {
		return ""
	}
	if mapped, ok := mapping[value]; ok {
		return mapped
	}
	for k, v := range mapping {
		if strings.EqualFold(k, value) {
			return v
		}
	}
	return ""
}

func pwgMediaSourceChoices(ppd *config.PPD) ([]string, map[string]string) {
	choices := ppdOptionChoiceList(ppd, "InputSlot")
	if len(choices) == 0 {
		return nil, nil
	}
	mapped := make([]string, 0, len(choices))
	mapping := map[string]string{}
	seen := map[string]bool{}
	for _, choice := range choices {
		pwg := pwgMediaSourceFromPPD(choice.Choice, choice.Text)
		if pwg == "" {
			pwg = pwgUnppdizeName(choice.Choice, "_")
		}
		mapping[choice.Choice] = pwg
		key := strings.ToLower(strings.TrimSpace(pwg))
		if key != "" && !seen[key] {
			seen[key] = true
			mapped = append(mapped, pwg)
		}
	}
	return mapped, mapping
}

func pwgMediaSourceFromPPD(choice, text string) string {
	choice = strings.TrimSpace(choice)
	text = strings.TrimSpace(text)
	choiceLower := strings.ToLower(choice)
	textLower := strings.ToLower(text)
	if strings.HasPrefix(choiceLower, "auto") || strings.HasPrefix(textLower, "auto") ||
		strings.EqualFold(choice, "Default") || strings.EqualFold(text, "Default") {
		return "auto"
	}
	switch {
	case strings.EqualFold(choice, "Cassette"):
		return "main"
	case strings.EqualFold(choice, "PhotoTray"):
		return "photo"
	case strings.EqualFold(choice, "CDTray"):
		return "disc"
	case strings.HasPrefix(choiceLower, "multipurpose") || strings.EqualFold(choice, "MP") || strings.EqualFold(choice, "MPTray"):
		return "by-pass-tray"
	case strings.EqualFold(choice, "LargeCapacity"):
		return "large-capacity"
	case strings.HasPrefix(choiceLower, "lower"):
		return "bottom"
	case strings.HasPrefix(choiceLower, "middle"):
		return "middle"
	case strings.HasPrefix(choiceLower, "upper"):
		return "top"
	case strings.HasPrefix(choiceLower, "side"):
		return "side"
	case strings.EqualFold(choice, "Roll"):
		return "main-roll"
	case choice == "0":
		return "tray-1"
	case choice == "1":
		return "tray-2"
	case choice == "2":
		return "tray-3"
	case choice == "3":
		return "tray-4"
	case choice == "4":
		return "tray-5"
	case choice == "5":
		return "tray-6"
	case choice == "6":
		return "tray-7"
	case choice == "7":
		return "tray-8"
	case choice == "8":
		return "tray-9"
	case choice == "9":
		return "tray-10"
	default:
		return ""
	}
}

type pwgMediaTypeMatch struct {
	ppd      string
	matchLen int
	pwg      string
}

func pwgMediaTypeChoices(ppd *config.PPD) ([]string, map[string]string) {
	choices := ppdOptionChoiceList(ppd, "MediaType")
	if len(choices) == 0 {
		return nil, nil
	}
	standard := []pwgMediaTypeMatch{
		{ppd: "Auto", matchLen: 4, pwg: "auto"},
		{ppd: "Any", matchLen: -1, pwg: "auto"},
		{ppd: "Default", matchLen: -1, pwg: "auto"},
		{ppd: "Card", matchLen: 4, pwg: "cardstock"},
		{ppd: "Env", matchLen: 3, pwg: "envelope"},
		{ppd: "Gloss", matchLen: 5, pwg: "photographic-glossy"},
		{ppd: "HighGloss", matchLen: -1, pwg: "photographic-high-gloss"},
		{ppd: "Matte", matchLen: -1, pwg: "photographic-matte"},
		{ppd: "Plain", matchLen: 5, pwg: "stationery"},
		{ppd: "Coated", matchLen: 6, pwg: "stationery-coated"},
		{ppd: "Inkjet", matchLen: -1, pwg: "stationery-inkjet"},
		{ppd: "Letterhead", matchLen: -1, pwg: "stationery-letterhead"},
		{ppd: "Preprint", matchLen: 8, pwg: "stationery-preprinted"},
		{ppd: "Recycled", matchLen: -1, pwg: "stationery-recycled"},
		{ppd: "Transparen", matchLen: 10, pwg: "transparency"},
	}
	matchCounts := make([]int, len(standard))
	type entry struct {
		choice string
		pwg    string
	}
	entries := make([]entry, 0, len(choices))
	for _, choice := range choices {
		pwg := ""
		for i, st := range standard {
			if matchPPDType(choice.Choice, st.ppd, st.matchLen) {
				pwg = st.pwg
				matchCounts[i]++
			}
		}
		if pwg == "" {
			pwg = pwgUnppdizeName(choice.Choice, "_")
		}
		entries = append(entries, entry{choice: choice.Choice, pwg: pwg})
	}
	if len(matchCounts) >= 3 {
		autoCount := matchCounts[0] + matchCounts[1] + matchCounts[2]
		matchCounts[0], matchCounts[1], matchCounts[2] = autoCount, autoCount, autoCount
	}
	for i := range entries {
		for j, st := range standard {
			if matchCounts[j] > 1 && entries[i].pwg == st.pwg {
				entries[i].pwg = pwgUnppdizeName(entries[i].choice, "_")
				break
			}
		}
	}
	mapped := make([]string, 0, len(entries))
	mapping := map[string]string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		mapping[entry.choice] = entry.pwg
		key := strings.ToLower(strings.TrimSpace(entry.pwg))
		if key != "" && !seen[key] {
			seen[key] = true
			mapped = append(mapped, entry.pwg)
		}
	}
	return mapped, mapping
}

func matchPPDType(choice, ppd string, matchLen int) bool {
	choice = strings.TrimSpace(choice)
	ppd = strings.TrimSpace(ppd)
	if choice == "" || ppd == "" {
		return false
	}
	if matchLen <= 0 {
		return strings.EqualFold(choice, ppd)
	}
	if len(ppd) > matchLen {
		ppd = ppd[:matchLen]
	}
	if len(choice) < len(ppd) {
		return false
	}
	return strings.EqualFold(choice[:len(ppd)], ppd)
}

func pwgOutputBinChoices(ppd *config.PPD) ([]string, map[string]string) {
	choices := ppdOptionChoiceList(ppd, "OutputBin")
	if len(choices) == 0 {
		return nil, nil
	}
	mapped := make([]string, 0, len(choices))
	mapping := map[string]string{}
	seen := map[string]bool{}
	for _, choice := range choices {
		pwg := pwgUnppdizeName(choice.Choice, "_")
		mapping[choice.Choice] = pwg
		key := strings.ToLower(strings.TrimSpace(pwg))
		if key != "" && !seen[key] {
			seen[key] = true
			mapped = append(mapped, pwg)
		}
	}
	return mapped, mapping
}

func mediaSizeLookup(name string, sizes map[string]mediaSize) (mediaSize, bool) {
	if sizes == nil {
		return mediaSize{}, false
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return mediaSize{}, false
	}
	size, ok := sizes[key]
	return size, ok
}

func matchDimsToSizes(x, y int, sizes map[string]mediaSize) string {
	if len(sizes) == 0 || x <= 0 || y <= 0 {
		return ""
	}
	for name, size := range sizes {
		if approxMedia(x, y, size.X, size.Y) {
			return name
		}
	}
	return ""
}

func lookupMediaSize(name string) (mediaSize, bool) {
	pwgMediaOnce.Do(loadPWGMediaTable)
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return mediaSize{}, false
	}
	size, ok := pwgMediaByName[key]
	return size, ok
}

func lookupMediaNameByDims(x, y int) string {
	pwgMediaOnce.Do(loadPWGMediaTable)
	if len(pwgMediaByDims) == 0 {
		return ""
	}
	if name, ok := pwgMediaByDims[mediaDimsKey(x, y)]; ok {
		return name
	}
	if name, ok := pwgMediaByDims[mediaDimsKey(y, x)]; ok {
		return name
	}
	return ""
}

func mediaDimsKey(x, y int) string {
	return strconv.Itoa(x) + "x" + strconv.Itoa(y)
}

func loadPWGMediaTable() {
	pwgMediaByName = map[string]mediaSize{}
	pwgMediaByDims = map[string]string{}
	if path := findPWGMediaFile(); path != "" {
		_ = parsePWGMediaFile(path)
	}
	if len(pwgMediaByName) == 0 {
		addPWGMediaName("A4", 21000, 29700, true)
		addPWGMediaName("Letter", 21590, 27940, true)
		addPWGMediaName("Legal", 21590, 35560, true)
		addPWGMediaName("A5", 14800, 21000, true)
	}
}

func findPWGMediaFile() string {
	if env := strings.TrimSpace(os.Getenv("CUPS_PWG_MEDIA_FILE")); env != "" {
		if fileExists(env) {
			return env
		}
	}
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "cups2.4.16", "cups", "pwg-media.c"),
			filepath.Join(cwd, "cups", "pwg-media.c"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "cups2.4.16", "cups", "pwg-media.c"),
			filepath.Join(exeDir, "cups", "pwg-media.c"),
		)
	}
	for _, path := range candidates {
		if fileExists(path) {
			return path
		}
	}
	return ""
}

var pwgMediaLineRe = regexp.MustCompile(`_PWG_MEDIA_(IN|MM)`)

func parsePWGMediaFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		unit, pwg, legacy, ppd, w, h, ok := parsePWGMediaLine(line)
		if !ok {
			continue
		}
		scale := 100.0
		if unit == "IN" {
			scale = 2540.0
		}
		x := int(math.Round(w * scale))
		y := int(math.Round(h * scale))
		if x <= 0 || y <= 0 {
			continue
		}
		addPWGMediaName(pwg, x, y, true)
		addPWGMediaName(legacy, x, y, false)
		addPWGMediaName(ppd, x, y, false)
	}
	return sc.Err()
}

func parsePWGMediaLine(line string) (string, string, string, string, float64, float64, bool) {
	match := pwgMediaLineRe.FindStringSubmatch(line)
	if len(match) < 2 {
		return "", "", "", "", 0, 0, false
	}
	unit := match[1]
	start := strings.Index(line, "(")
	end := strings.LastIndex(line, ")")
	if start < 0 || end <= start {
		return "", "", "", "", 0, 0, false
	}
	args := splitPWGArgs(line[start+1 : end])
	if len(args) < 5 {
		return "", "", "", "", 0, 0, false
	}
	pwg := parsePWGArgString(args[0])
	legacy := parsePWGArgString(args[1])
	ppd := parsePWGArgString(args[2])
	w, err1 := strconv.ParseFloat(strings.TrimSpace(args[3]), 64)
	h, err2 := strconv.ParseFloat(strings.TrimSpace(args[4]), 64)
	if err1 != nil || err2 != nil {
		return "", "", "", "", 0, 0, false
	}
	return unit, pwg, legacy, ppd, w, h, true
}

func splitPWGArgs(value string) []string {
	out := []string{}
	var buf strings.Builder
	inQuotes := false
	for _, r := range value {
		switch r {
		case '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case ',':
			if inQuotes {
				buf.WriteRune(r)
			} else {
				out = append(out, strings.TrimSpace(buf.String()))
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

func parsePWGArgString(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	if strings.EqualFold(arg, "NULL") {
		return ""
	}
	return strings.Trim(arg, "\"")
}

func addPWGMediaName(name string, x, y int, preferDims bool) {
	if strings.TrimSpace(name) == "" {
		return
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := pwgMediaByName[key]; !ok {
		pwgMediaByName[key] = mediaSize{X: x, Y: y}
	}
	dimsKey := mediaDimsKey(x, y)
	if preferDims || pwgMediaByDims[dimsKey] == "" {
		pwgMediaByDims[dimsKey] = name
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

func collectionInts(col goipp.Collection, name string) []int {
	out := []int{}
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		for _, v := range attr.Values {
			if n, ok := v.V.(goipp.Integer); ok {
				out = append(out, int(n))
			}
		}
	}
	return out
}

func collectionCollection(col goipp.Collection, name string) (goipp.Collection, bool) {
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		if c, ok := attr.Values[0].V.(goipp.Collection); ok {
			return c, true
		}
	}
	return goipp.Collection{}, false
}

func finishingsSupportedFromPPD(ppd *config.PPD) []int {
	if ppd == nil {
		return nil
	}
	values := []int{3}
	add := func(v int) {
		if v <= 0 {
			return
		}
		if !intInList(v, values) {
			values = append(values, v)
		}
	}
	for key, opts := range ppd.Options {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || len(opts) == 0 {
			continue
		}
		if fin := finishingsForPPDOption(key, opts); len(fin) > 0 {
			for _, v := range fin {
				add(v)
			}
		}
	}
	if len(values) == 1 {
		return values
	}
	sort.Ints(values)
	return values
}

func finishingsForPPDOption(option string, choices []string) []int {
	if len(choices) == 0 {
		return nil
	}
	switch option {
	case "staplelocation":
		return finishingsForStaple(choices)
	case "foldtype":
		return finishingsForFold(choices)
	case "punchmedia":
		return finishingsForPunch(choices)
	case "bindedge":
		return finishingsForBind(choices)
	case "booklet":
		return finishingsForBooklet(choices)
	case "cutmedia":
		return finishingsForCut(choices)
	case "ripuncht":
		fallthrough
	case "ripunch":
		return finishingsForPunch(choices)
	case "rifoldtype":
		return finishingsForFold(choices)
	default:
		if strings.HasPrefix(option, "staple") {
			return finishingsForStaple(choices)
		}
		if strings.HasPrefix(option, "fold") {
			return finishingsForFold(choices)
		}
		if strings.HasPrefix(option, "punch") {
			return finishingsForPunch(choices)
		}
		if strings.HasPrefix(option, "bind") {
			return finishingsForBind(choices)
		}
		if strings.HasPrefix(option, "cut") {
			return finishingsForCut(choices)
		}
	}
	return nil
}

func finishingsForStaple(choices []string) []int {
	out := []int{}
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		switch key {
		case "upperleft", "topleft", "singleportrait", "single":
			out = append(out, 20)
		case "upperright", "topright":
			out = append(out, 22)
		case "lowerleft", "bottomleft", "singlelandscape":
			out = append(out, 21)
		case "lowerright", "bottomright":
			out = append(out, 23)
		case "dualportrait", "dual":
			out = append(out, 25)
		case "dualtop":
			out = append(out, 25)
		case "duallandscape":
			out = append(out, 24)
		case "tripletop":
			out = append(out, 29)
		case "tripleleft":
			out = append(out, 28)
		case "tripleright":
			out = append(out, 30)
		case "triplebottom":
			out = append(out, 31)
		default:
			out = append(out, 4)
		}
	}
	return out
}

func finishingsForPunch(choices []string) []int {
	out := []int{}
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		switch key {
		case "left2", "dual-left", "dualleft":
			out = append(out, 73)
		case "right2", "dual-right", "dualright":
			out = append(out, 75)
		case "upper2", "top2", "dual-top", "dualtop":
			out = append(out, 74)
		case "left3", "triple-left", "tripleleft":
			out = append(out, 77)
		case "right3", "triple-right", "tripleright":
			out = append(out, 79)
		case "upper3", "top3", "triple-top", "tripletop":
			out = append(out, 78)
		case "left4", "quad-left", "quadleft":
			out = append(out, 81)
		case "right4", "quad-right", "quadright":
			out = append(out, 83)
		case "upper4", "top4", "quad-top", "quadtop":
			out = append(out, 82)
		default:
			out = append(out, 5)
		}
	}
	return out
}

func finishingsForFold(choices []string) []int {
	out := []int{}
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		switch key {
		case "zfold", "z-fold", "fold-z":
			out = append(out, 99)
		case "saddle", "fold-half", "half":
			out = append(out, 97)
		case "doublegate", "double-gate":
			out = append(out, 90)
		case "leftgate", "left-gate":
			out = append(out, 95)
		case "rightgate", "right-gate":
			out = append(out, 99)
		case "letter":
			out = append(out, 96)
		case "xfold", "poster":
			out = append(out, 98)
		default:
			out = append(out, 8)
		}
	}
	return out
}

func finishingsForBind(choices []string) []int {
	out := []int{}
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		switch key {
		case "left":
			out = append(out, 50)
		case "top":
			out = append(out, 51)
		case "right":
			out = append(out, 52)
		case "bottom":
			out = append(out, 53)
		default:
			out = append(out, 5)
		}
	}
	return out
}

func finishingsForBooklet(choices []string) []int {
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		return []int{13}
	}
	return nil
}

func finishingsForCut(choices []string) []int {
	for _, raw := range choices {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" || key == "none" || key == "off" || key == "false" {
			continue
		}
		return []int{9}
	}
	return nil
}

func parseFinishingsList(value string) []int {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	out := []int{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
			continue
		}
		if n := finishingsNameToEnum(p); n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func finishingsNameToEnum(name string) int {
	lookupFinishings()
	key := strings.ToLower(strings.TrimSpace(name))
	for i, v := range ippFinishingsNames {
		if v == key {
			return i + 3
		}
	}
	return 0
}

func finishingsEnumToName(value int) string {
	idx := value - 3
	if idx < 0 || idx >= len(ippFinishingsNames) {
		return ""
	}
	return ippFinishingsNames[idx]
}

func finishingsTemplatesFromEnums(finishings []int) []string {
	templates := []string{}
	seen := map[string]struct{}{}
	for _, v := range finishings {
		if name := finishingsEnumToName(v); name != "" {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			templates = append(templates, name)
		}
	}
	if _, ok := seen["none"]; !ok {
		templates = append(templates, "none")
	}
	return templates
}

func lookupFinishings() {
	finishingsOnce.Do(func() {
		finishingsAll = []int{}
		for i := range ippFinishingsNames {
			finishingsAll = append(finishingsAll, i+3)
		}
	})
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
			mimeTypes = []string{"application/octet-stream", "application/vnd.cups-raw"}
			return
		}
		for mt := range db.Types {
			mimeTypes = append(mimeTypes, mt)
		}
		if !stringInList("application/vnd.cups-raw", mimeTypes) {
			mimeTypes = append(mimeTypes, "application/vnd.cups-raw")
		}
		sort.Strings(mimeTypes)
		if len(mimeTypes) == 0 {
			mimeTypes = []string{"application/octet-stream", "application/vnd.cups-raw"}
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
	if strings.EqualFold(format, "application/vnd.cups-raw") {
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
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("printer-geo-location", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-organization", goipp.TagAdminDefine, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("printer-organizational-unit", goipp.TagAdminDefine, goipp.Void{}))

	return attrs
}

var (
	jobDescriptionAttrs = map[string]bool{
		"job-id": true, "job-uri": true, "job-name": true, "job-state": true,
		"job-state-reasons": true, "job-state-message": true, "job-originating-user-name": true,
		"job-originating-host-name": true, "job-printer-uri": true, "job-printer-name": true,
		"time-at-creation": true, "time-at-processing": true, "time-at-completed": true,
		"job-impressions": true, "job-impressions-completed": true, "job-priority": true,
		"job-account-id": true, "job-hold-until": true, "job-k-octets": true,
		"number-of-documents": true, "document-format": true,
	}
	jobTemplateAttrs = map[string]bool{
		"copies": true, "finishings": true, "finishings-col": true, "job-hold-until": true,
		"job-sheets": true, "job-sheets-col": true, "media": true, "media-col": true,
		"media-type": true, "media-source": true, "multiple-document-handling": true,
		"number-up": true, "number-up-layout": true, "orientation-requested": true,
		"output-bin": true, "page-delivery": true, "page-ranges": true,
		"print-color-mode": true, "print-quality": true, "print-scaling": true,
		"print-as-raster": true, "printer-resolution": true, "sides": true,
		"job-cancel-after": true, "number-of-retries": true, "retry-interval": true,
		"retry-time-out": true, "confirmation-sheet-print": true, "cover-sheet-info": true,
	}
	jobStatusAttrs = map[string]bool{
		"job-state": true, "job-state-reasons": true, "job-state-message": true,
		"time-at-processing": true, "time-at-completed": true,
		"job-impressions": true, "job-impressions-completed": true,
	}
	documentDescriptionAttrs = map[string]bool{
		"document-number": true, "document-uri": true, "document-printer-uri": true,
		"document-job-id": true, "document-job-uri": true, "document-state": true,
		"document-state-reasons": true, "document-state-message": true, "document-name": true,
		"document-format": true, "document-format-detected": true, "document-size": true,
		"document-charset": true, "document-natural-language": true,
	}
	documentStatusAttrs = map[string]bool{
		"document-state": true, "document-state-reasons": true, "document-state-message": true,
	}
	subscriptionDescriptionAttrs = map[string]bool{
		"notify-events": true, "notify-lease-duration": true, "notify-recipient-uri": true,
		"notify-pull-method": true, "notify-subscriber-user-name": true, "notify-time-interval": true,
		"notify-user-data": true, "notify-job-id": true, "notify-printer-uri": true,
		"notify-subscription-id": true,
	}
	subscriptionTemplateAttrs = map[string]bool{
		"notify-events": true, "notify-lease-duration": true, "notify-recipient-uri": true,
		"notify-pull-method": true, "notify-time-interval": true, "notify-user-data": true,
	}
	printerStatusAttrs = map[string]bool{
		"printer-state": true, "printer-state-reasons": true, "printer-state-message": true,
		"printer-is-accepting-jobs": true, "printer-up-time": true, "queued-job-count": true,
		"marker-message": true, "printer-supply": true, "printer-supply-description": true,
	}
	printerDefaultsAttrs = map[string]bool{
		"copies-default": true, "document-format-default": true, "finishings-default": true,
		"job-account-id-default": true, "job-accounting-user-id-default": true,
		"job-cancel-after-default": true, "job-hold-until-default": true, "job-priority-default": true,
		"job-sheets-default": true, "media-col-default": true, "notify-lease-duration-default": true,
		"notify-events-default": true, "number-up-default": true, "orientation-requested-default": true,
		"print-color-mode-default": true, "print-quality-default": true,
	}
	printerConfigurationAttrs = map[string]bool{
		"printer-is-shared": true, "device-uri": true, "ppd-name": true,
		"printer-error-policy": true, "printer-op-policy": true, "port-monitor": true,
		"printer-error-policy-supported": true, "printer-op-policy-supported": true,
		"port-monitor-supported": true, "printer-settable-attributes-supported": true,
		"job-settable-attributes-supported": true, "job-creation-attributes-supported": true,
		"printer-get-attributes-supported": true, "server-is-sharing-printers": true,
	}
	printerDescriptionAttrs = map[string]bool{
		"printer-name": true, "printer-uri-supported": true, "printer-state": true,
		"printer-is-accepting-jobs": true, "printer-state-reasons": true,
		"printer-location": true, "printer-info": true, "ppd-name": true,
		"printer-make-and-model": true, "document-format-supported": true,
		"document-format-default": true, "document-format-preferred": true,
		"document-charset-default": true, "document-charset-supported": true,
		"document-natural-language-default": true, "document-natural-language-supported": true,
		"charset-configured": true, "charset-supported": true, "natural-language-configured": true,
		"natural-language-supported": true, "ipp-versions-supported": true, "printer-up-time": true,
		"queued-job-count": true, "printer-uuid": true, "cups-version": true,
		"generated-natural-language-supported": true, "printer-id": true, "printer-geo-location": true,
		"printer-organization": true, "printer-organizational-unit": true,
		"printer-strings-languages-supported": true, "printer-device-id": true, "device-uri": true,
		"destination-uri": true, "multiple-destination-uris-supported": true,
		"uri-security-supported": true, "uri-authentication-supported": true,
		"operations-supported": true, "printer-is-shared": true, "printer-state-message": true,
		"multiple-document-handling-supported": true, "multiple-document-jobs-supported": true,
		"compression-supported": true, "ippget-event-life": true, "job-ids-supported": true,
		"job-priority-supported": true, "job-priority-default": true, "job-account-id-supported": true,
		"job-k-octets-supported": true, "pdf-k-octets-supported": true, "pdf-versions-supported": true,
		"ipp-features-supported": true, "notify-attributes-supported": true, "notify-events-supported": true,
		"notify-events-default": true, "notify-schemes-supported": true,
		"notify-lease-duration-default": true, "notify-max-events-supported": true,
		"notify-lease-duration-supported": true, "printer-get-attributes-supported": true,
		"multiple-operation-time-out": true, "multiple-operation-time-out-action": true,
		"copies-supported": true, "copies-default": true, "job-sheets-supported": true,
		"job-sheets-default": true, "job-sheets-col-supported": true, "job-sheets-col-default": true,
		"print-as-raster-supported": true, "print-as-raster-default": true,
		"job-hold-until-supported": true, "job-hold-until-default": true, "page-delivery-supported": true,
		"print-scaling-supported": true, "print-quality-supported": true, "print-quality-default": true,
		"page-ranges-supported": true, "finishings-supported": true, "finishings-default": true,
		"finishings-ready": true, "finishing-template-supported": true, "finishings-col-supported": true,
		"finishings-col-default": true, "finishings-col-ready": true, "number-up-supported": true,
		"number-up-default": true, "number-up-layout-supported": true, "orientation-requested-supported": true,
		"orientation-requested-default": true, "job-settable-attributes-supported": true,
		"job-creation-attributes-supported": true, "printer-settable-attributes-supported": true,
		"printer-error-policy-supported": true, "printer-error-policy": true, "printer-op-policy-supported": true,
		"printer-op-policy": true, "port-monitor-supported": true, "port-monitor": true,
		"job-cancel-after-supported": true, "job-cancel-after-default": true,
		"jpeg-k-octets-supported": true, "jpeg-x-dimension-supported": true, "jpeg-y-dimension-supported": true,
		"media-col-supported": true, "marker-message": true, "printer-supply": true,
		"printer-supply-description": true, "job-presets-supported": true, "which-jobs-supported": true,
		"server-is-sharing-printers": true, "mopria-certified": true, "media-supported": true,
		"media-ready": true, "media-default": true, "media-col-default": true, "media-col-ready": true,
		"media-source-supported": true, "media-source-default": true, "media-type-supported": true,
		"media-type-default": true, "media-col-database": true, "media-size-supported": true,
		"media-bottom-margin-supported": true, "media-left-margin-supported": true,
		"media-right-margin-supported": true, "media-top-margin-supported": true,
		"sides-supported": true,
		"sides-default":   true, "print-color-mode-supported": true, "print-color-mode-default": true,
		"pwg-raster-document-type-supported": true, "pwg-raster-document-resolution-supported": true,
		"pwg-raster-document-sheet-back": true,
		"printer-resolution-supported":   true, "printer-resolution-default": true, "output-bin-supported": true,
		"output-bin-default": true, "finishings-col-database": true, "urf-supported": true,
	}
)

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
		if requested[strings.ToLower(attr.Name)] {
			out.Add(attr)
		}
	}
	return out
}

func requestedAttributes(req *goipp.Message) (map[string]bool, bool) {
	if req == nil {
		return nil, true
	}
	values := attrStrings(req.Operation, "requested-attributes")
	if len(values) == 0 {
		if goipp.Op(req.Code) == goipp.OpGetDocuments {
			return map[string]bool{"document-number": true}, false
		}
		return nil, true
	}
	set := map[string]bool{}
	groups := map[string]bool{}
	for _, v := range values {
		name := strings.TrimSpace(v)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		switch lower {
		case "all":
			return nil, true
		case "printer-description", "printer-defaults", "printer-configuration", "printer-status",
			"job-description", "job-template", "job-status", "subscription-description", "subscription-template",
			"document-description", "document-status", "document-template":
			groups[lower] = true
			continue
		}
		set[lower] = true
	}
	if len(groups) > 0 {
		expanded, all := expandRequestedGroups(groups)
		if all {
			return nil, true
		}
		for k := range expanded {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return nil, true
	}
	return set, false
}

func expandRequestedGroups(groups map[string]bool) (map[string]bool, bool) {
	out := map[string]bool{}
	all := false
	for g := range groups {
		switch g {
		case "job-description":
			mergeAttributeSet(out, jobDescriptionAttrs)
		case "job-template":
			mergeAttributeSet(out, jobTemplateAttrs)
		case "job-status":
			mergeAttributeSet(out, jobStatusAttrs)
		case "printer-status":
			mergeAttributeSet(out, printerStatusAttrs)
		case "printer-defaults":
			mergeAttributeSet(out, printerDefaultsAttrs)
		case "printer-configuration":
			mergeAttributeSet(out, printerConfigurationAttrs)
		case "printer-description":
			mergeAttributeSet(out, printerDescriptionAttrs)
		case "document-description", "document-template":
			mergeAttributeSet(out, documentDescriptionAttrs)
		case "document-status":
			mergeAttributeSet(out, documentStatusAttrs)
		case "subscription-description":
			mergeAttributeSet(out, subscriptionDescriptionAttrs)
		case "subscription-template":
			mergeAttributeSet(out, subscriptionTemplateAttrs)
		default:
			all = true
		}
	}
	return out, all
}

func mergeAttributeSet(dst, src map[string]bool) {
	for k := range src {
		dst[k] = true
	}
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

func isHoldUntilTime(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return false
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || min < 0 || min > 59 {
		return false
	}
	if len(parts) == 3 {
		sec, err := strconv.Atoi(strings.TrimSpace(parts[2]))
		if err != nil || sec < 0 || sec > 59 {
			return false
		}
	}
	return true
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

func urfSupported(resolutions []goipp.Resolution, colorModes []string, sides []string, finishings []int, qualities []int) []string {
	urf := []string{"V1.4", "CP1", "W8"}
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
	if len(qualities) == 0 {
		qualities = []int{4}
	}
	pqVals := uniqueInts(qualities)
	sort.Ints(pqVals)
	pq := make([]string, 0, len(pqVals))
	for _, q := range pqVals {
		pq = append(pq, strconv.Itoa(q))
	}
	if len(pq) > 0 {
		urf = append(urf, "PQ"+strings.Join(pq, "-"))
	}
	rsVals := uniqueResolutionsForURF(resolutions)
	if len(rsVals) > 0 {
		parts := make([]string, 0, len(rsVals))
		for _, v := range rsVals {
			parts = append(parts, strconv.Itoa(v))
		}
		urf = append(urf, "RS"+strings.Join(parts, "-"))
	}
	if len(sides) > 1 {
		urf = append(urf, "DM1")
	}
	if len(finishings) == 0 {
		urf = append(urf, "FN3")
	} else {
		fnVals := uniqueInts(finishings)
		sort.Ints(fnVals)
		parts := make([]string, 0, len(fnVals))
		for _, f := range fnVals {
			parts = append(parts, strconv.Itoa(f))
		}
		urf = append(urf, "FN"+strings.Join(parts, "-"))
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

func uniqueInts(values []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func uniqueResolutionsForURF(resolutions []goipp.Resolution) []int {
	if len(resolutions) == 0 {
		return nil
	}
	seen := map[int]bool{}
	out := []int{}
	for _, r := range resolutions {
		val := r.Xres
		if r.Yres < val {
			val = r.Yres
		}
		if val <= 0 || seen[val] {
			continue
		}
		seen[val] = true
		out = append(out, val)
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

func collectionStrings(col goipp.Collection, name string) []string {
	out := []string{}
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		for _, v := range attr.Values {
			out = append(out, v.V.String())
		}
	}
	return out
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
	host := hostForRequest(r)
	return fmt.Sprintf("ipp://%s/printers/%s", host, printer.Name)
}

func classURIFor(class model.Class, r *http.Request) string {
	host := hostForRequest(r)
	return fmt.Sprintf("ipp://%s/classes/%s", host, class.Name)
}

func jobURIFor(job model.Job, r *http.Request) string {
	host := hostForRequest(r)
	return fmt.Sprintf("ipp://%s/jobs/%d", host, job.ID)
}

func documentURIFor(jobID int64, docNum int64, r *http.Request) string {
	host := hostForRequest(r)
	if jobID <= 0 {
		jobID = 0
	}
	if docNum <= 0 {
		docNum = 0
	}
	return fmt.Sprintf("ipp://%s/jobs/%d/documents/%d", host, jobID, docNum)
}

func parseDocumentURI(uri string) (int64, int) {
	if uri == "" {
		return 0, 0
	}
	u, err := url.Parse(uri)
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 {
		return 0, 0
	}
	if parts[0] != "jobs" || parts[2] != "documents" {
		return 0, 0
	}
	jobID, _ := strconv.ParseInt(parts[1], 10, 64)
	docNum, _ := strconv.Atoi(parts[3])
	return jobID, docNum
}

func documentStateForJob(jobState int) int {
	switch jobState {
	case 7:
		return 7
	case 8:
		return 8
	case 9:
		return 9
	case 6:
		return 6
	case 5:
		return 5
	default:
		return 3
	}
}

func hostForRequest(r *http.Request) string {
	if r != nil && strings.TrimSpace(r.Host) != "" {
		return r.Host
	}
	return ensureHostPort(appConfig().ServerName)
}

func ensureHostPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "localhost"
	}
	if strings.HasPrefix(host, "[") {
		if _, _, err := net.SplitHostPort(host); err == nil {
			return host
		}
		if strings.HasSuffix(host, "]") {
			return host + ":631"
		}
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if strings.Count(host, ":") > 1 {
		return "[" + host + "]:631"
	}
	if strings.Contains(host, ":") {
		return host
	}
	return host + ":631"
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
