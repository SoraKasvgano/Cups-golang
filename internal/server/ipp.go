package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/backend"
	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/store"
	"cupsgolang/internal/web"
)

var (
	mimeOnce          sync.Once
	mimeDB            *config.MimeDB
	mimeTypes         []string
	mimeExt           map[string]string
	printerFormatOnce sync.Map
	errPPDConstraint  = errors.New("ppd-constraint-violation")
	errUnsupported    = errors.New("unsupported-attribute-value")
	errConflicting    = errors.New("conflicting-attributes")
	policyNamesOnce   sync.Once
	policyNames       []string
	stringsLangOnce   sync.Once
	stringsLangs      []string
	pwgMediaOnce      sync.Once
	pwgMediaByName    map[string]mediaSize
	pwgMediaByDims    map[string]string
	pwgMediaPPDByDims map[string]string
	finishingsOnce    sync.Once
	finishingsAll     []int
	errNotAuthorized  = errors.New("not-authorized")
	errNotPossible    = errors.New("not-possible")
	errBadRequest     = errors.New("bad-request")
	errTooManySubs    = errors.New("too-many-subscriptions")
)

var readOnlyJobAttrs = map[string]bool{
	"date-time-at-completed":        true,
	"date-time-at-creation":         true,
	"date-time-at-processing":       true,
	"job-detailed-status-messages":  true,
	"job-document-access-errors":    true,
	"job-id":                        true,
	"job-impressions-completed":     true,
	"job-k-octets-completed":        true,
	"job-k-octets-processed":        true,
	"job-media-sheets-completed":    true,
	"job-more-info":                 true,
	"job-account-id-actual":         true,
	"job-accounting-user-id-actual": true,
	"job-pages-completed":           true,
	"job-preserved":                 true,
	"job-printer-up-time":           true,
	"job-processing-time":           true,
	"job-printer-uri":               true,
	"job-state":                     true,
	"job-state-message":             true,
	"job-state-reasons":             true,
	"job-uri":                       true,
	"job-uuid":                      true,
	"number-of-documents":           true,
	"number-of-intervening-jobs":    true,
	"original-requesting-user-name": true,
	"output-device-assigned":        true,
	"time-at-completed":             true,
	"time-at-creation":              true,
	"time-at-processing":            true,
}

type ippHTTPError struct {
	status   int
	authType string
	message  string
}

func (e *ippHTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.message != "" {
		return e.message
	}
	if e.status > 0 {
		return http.StatusText(e.status)
	}
	return "http-error"
}

type ippWarning struct {
	status      goipp.Status
	unsupported goipp.Attributes
}

func mergeWarning(dst, src *ippWarning) *ippWarning {
	if src == nil {
		return dst
	}
	if dst == nil {
		return src
	}
	if src.status != 0 {
		dst.status = src.status
	}
	if len(src.unsupported) > 0 {
		dst.unsupported = append(dst.unsupported, src.unsupported...)
	}
	return dst
}

func applyWarning(resp *goipp.Message, warn *ippWarning) {
	if resp == nil || warn == nil {
		return
	}
	if warn.status != 0 {
		resp.Code = goipp.Code(warn.status)
	}
	if len(warn.unsupported) > 0 {
		resp.Unsupported = append(resp.Unsupported, warn.unsupported...)
	}
}

const (
	cupsPTypeClass      = 0x0001
	cupsPTypeRemote     = 0x0002
	cupsPTypeBW         = 0x0004
	cupsPTypeColor      = 0x0008
	cupsPTypeDuplex     = 0x0010
	cupsPTypeStaple     = 0x0020
	cupsPTypeCopies     = 0x0040
	cupsPTypeCollate    = 0x0080
	cupsPTypePunch      = 0x0100
	cupsPTypeCover      = 0x0200
	cupsPTypeBind       = 0x0400
	cupsPTypeSort       = 0x0800
	cupsPTypeSmall      = 0x1000
	cupsPTypeMedium     = 0x2000
	cupsPTypeLarge      = 0x4000
	cupsPTypeVariable   = 0x8000
	cupsPTypeDefault    = 0x20000
	cupsPTypeFax        = 0x40000
	cupsPTypeRejecting  = 0x80000
	cupsPTypeNotShared  = 0x200000
	cupsPTypeAuth       = 0x400000
	cupsPTypeCommands   = 0x800000
	cupsPTypeDiscovered = 0x1000000
	cupsPTypeScanner    = 0x2000000
	cupsPTypeMFP        = 0x4000000
	cupsPType3D         = 0x8000000
	cupsPTypeFold       = 0x10000000
)

const supplyCacheTTL = 60 * time.Second

type ippPolicyCheckContext struct {
	policyName string
	owner      string
}

func remoteIPForRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}
	return remoteIP
}

func isLocalhostRequest(r *http.Request) bool {
	ip := net.ParseIP(remoteIPForRequest(r))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) defaultPolicyName() string {
	if s == nil {
		return "default"
	}
	if v := strings.TrimSpace(s.Config.DefaultPolicy); v != "" {
		return v
	}
	return "default"
}

func (s *Server) policyNameForDefaultOptions(defaultOptionsJSON string) string {
	defaultOpts := parseJobOptions(defaultOptionsJSON)
	return choiceOrDefault(defaultOpts["printer-op-policy"], supportedOpPolicies(), s.defaultPolicyName())
}

func (s *Server) resolveClassOrPrinterPolicy(ctx context.Context, tx *sql.Tx, printerURI string) (string, error) {
	if s == nil || s.Store == nil || tx == nil {
		return s.defaultPolicyName(), nil
	}
	u, err := url.Parse(printerURI)
	if err != nil {
		return s.defaultPolicyName(), err
	}
	switch {
	case strings.HasPrefix(u.Path, "/classes/"):
		name := strings.TrimPrefix(u.Path, "/classes/")
		if strings.TrimSpace(name) == "" {
			return s.defaultPolicyName(), sql.ErrNoRows
		}
		class, err := s.Store.GetClassByName(ctx, tx, name)
		if err != nil {
			return s.defaultPolicyName(), err
		}
		return s.policyNameForDefaultOptions(class.DefaultOptions), nil
	case strings.HasPrefix(u.Path, "/printers/"):
		name := strings.TrimPrefix(u.Path, "/printers/")
		if strings.TrimSpace(name) == "" {
			return s.defaultPolicyName(), sql.ErrNoRows
		}
		printer, err := s.Store.GetPrinterByName(ctx, tx, name)
		if err != nil {
			return s.defaultPolicyName(), err
		}
		return s.policyNameForDefaultOptions(printer.DefaultOptions), nil
	default:
		// Unknown printer URI path - treat as default policy.
		return s.defaultPolicyName(), nil
	}
}

func (s *Server) ippPolicyCheckContexts(ctx context.Context, r *http.Request, req *goipp.Message) ([]ippPolicyCheckContext, error) {
	if s == nil || s.Store == nil || req == nil {
		return nil, nil
	}
	op := goipp.Op(req.Code)

	// Get-Notifications can reference multiple subscriptions and CUPS checks
	// policy for each one, stopping on the first failure.
	if op == goipp.OpGetNotifications {
		subIDs := attrInts(req.Operation, "notify-subscription-ids")
		if len(subIDs) == 0 {
			return nil, nil
		}
		out := make([]ippPolicyCheckContext, 0, len(subIDs))
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			for _, id := range subIDs {
				sub, err := s.Store.GetSubscription(ctx, tx, id)
				if err != nil {
					return err
				}
				policyName := s.defaultPolicyName()
				if sub.JobID.Valid {
					job, err := s.Store.GetJob(ctx, tx, sub.JobID.Int64)
					if err != nil {
						return err
					}
					if printer, err := s.Store.GetPrinterByID(ctx, tx, job.PrinterID); err == nil {
						policyName = s.policyNameForDefaultOptions(printer.DefaultOptions)
					}
				} else if sub.PrinterID.Valid {
					if printer, err := s.Store.GetPrinterByID(ctx, tx, sub.PrinterID.Int64); err == nil {
						policyName = s.policyNameForDefaultOptions(printer.DefaultOptions)
					}
				}
				out = append(out, ippPolicyCheckContext{policyName: policyName, owner: sub.Owner})
			}
			return nil
		})
		if err != nil {
			// Let the handler convert missing subscriptions/jobs to IPP not-found.
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		return out, nil
	}

	// Default case: resolve a single policy context (policy name + optional owner).
	ctxItem := ippPolicyCheckContext{policyName: s.defaultPolicyName()}

	// Some CUPS operations are always checked against the DefaultPolicyPtr even
	// when a printer-uri is present (see scheduler/ipp.c in CUPS 2.4.16).
	switch op {
	case goipp.OpCupsGetPpd, goipp.OpCupsDeletePrinter, goipp.OpCupsDeleteClass, goipp.OpCupsSetDefault:
		return []ippPolicyCheckContext{ctxItem}, nil
	}

	// Subscription-based operations.
	switch op {
	case goipp.OpGetSubscriptionAttributes, goipp.OpRenewSubscription, goipp.OpCancelSubscription:
		subID := attrInt(req.Operation, "notify-subscription-id")
		if subID == 0 {
			return []ippPolicyCheckContext{ctxItem}, nil
		}
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			sub, err := s.Store.GetSubscription(ctx, tx, subID)
			if err != nil {
				return err
			}
			ctxItem.owner = sub.Owner
			if sub.JobID.Valid {
				job, err := s.Store.GetJob(ctx, tx, sub.JobID.Int64)
				if err != nil {
					return err
				}
				if printer, err := s.Store.GetPrinterByID(ctx, tx, job.PrinterID); err == nil {
					ctxItem.policyName = s.policyNameForDefaultOptions(printer.DefaultOptions)
				}
			} else if sub.PrinterID.Valid {
				if printer, err := s.Store.GetPrinterByID(ctx, tx, sub.PrinterID.Int64); err == nil {
					ctxItem.policyName = s.policyNameForDefaultOptions(printer.DefaultOptions)
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		return []ippPolicyCheckContext{ctxItem}, nil
	}

	// Job-based operations.
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 && r != nil && strings.HasPrefix(r.URL.Path, "/jobs/") {
		if n, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/jobs/"), 10, 64); err == nil {
			jobID = n
		}
	}

	// CUPS-Move-Job is special: it checks the destination printer policy,
	// but uses the job owner for the @OWNER check.
	if op == goipp.OpCupsMoveJob {
		destURI := attrString(req.Operation, "printer-uri")
		if destURI == "" {
			destURI = attrString(req.Operation, "printer-uri-destination")
		}
		if jobID == 0 || strings.TrimSpace(destURI) == "" {
			return []ippPolicyCheckContext{ctxItem}, nil
		}
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			job, err := s.Store.GetJob(ctx, tx, jobID)
			if err != nil {
				return err
			}
			ctxItem.owner = job.UserName
			pol, err := s.resolveClassOrPrinterPolicy(ctx, tx, destURI)
			if err == nil {
				ctxItem.policyName = pol
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		return []ippPolicyCheckContext{ctxItem}, nil
	}

	if jobID != 0 {
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			job, err := s.Store.GetJob(ctx, tx, jobID)
			if err != nil {
				return err
			}
			ctxItem.owner = job.UserName
			// CUPS-Get-Document is checked against the DefaultPolicyPtr, not the
			// job's destination policy.
			if op != goipp.OpCupsGetDocument {
				if printer, err := s.Store.GetPrinterByID(ctx, tx, job.PrinterID); err == nil {
					ctxItem.policyName = s.policyNameForDefaultOptions(printer.DefaultOptions)
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		return []ippPolicyCheckContext{ctxItem}, nil
	}

	// Destination-based operations: printer-uri identifies printer/class op policy.
	if printerURI := strings.TrimSpace(attrString(req.Operation, "printer-uri")); printerURI != "" {
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			pol, err := s.resolveClassOrPrinterPolicy(ctx, tx, printerURI)
			if err == nil {
				ctxItem.policyName = pol
			} else if errors.Is(err, sql.ErrNoRows) {
				// Non-existent destination: keep default policy and let the handler decide.
				return nil
			} else {
				return err
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return []ippPolicyCheckContext{ctxItem}, nil
}

func (s *Server) enforceIPPOpPolicy(ctx context.Context, r *http.Request, req *goipp.Message) error {
	if s == nil || req == nil {
		return nil
	}
	// No parsed policies -> nothing to enforce.
	if len(s.Policy.PolicyLimits) == 0 {
		return nil
	}

	remoteIP := remoteIPForRequest(r)
	opName := goipp.Op(req.Code).String()
	contexts, err := s.ippPolicyCheckContexts(ctx, r, req)
	if err != nil || len(contexts) == 0 {
		return err
	}

	for _, c := range contexts {
		policyName := strings.TrimSpace(c.policyName)
		if policyName == "" {
			policyName = s.defaultPolicyName()
		}
		limit := s.Policy.PolicyLimitFor(policyName, opName)
		if limit == nil && !strings.EqualFold(policyName, s.defaultPolicyName()) {
			// Fallback to the default policy name when a queue references an unknown policy.
			limit = s.Policy.PolicyLimitFor(s.defaultPolicyName(), opName)
			policyName = s.defaultPolicyName()
		}
		if limit == nil {
			continue
		}
		if limit.DenyAll || !s.Policy.AllowedByLimit(limit, remoteIP) {
			return &ippHTTPError{status: http.StatusForbidden}
		}
		if !limitRequiresAuth(limit) {
			continue
		}

		authType := s.normalizeAuthType(limit.AuthType)
		if authType == "" {
			authType = strings.TrimSpace(s.Config.DefaultAuthType)
		}

		u, ok := s.authenticateUser(ctx, r, authType)
		if !ok {
			return &ippHTTPError{status: http.StatusUnauthorized, authType: authType}
		}
		if !userAllowedByLimit(u, c.owner, limit) {
			return &ippHTTPError{status: http.StatusForbidden}
		}
	}

	return nil
}

func (s *Server) enforceHTTPLocationPolicy(ctx context.Context, r *http.Request) error {
	if s == nil || r == nil {
		return nil
	}
	remoteIP := remoteIPForRequest(r)
	if rule := s.Policy.Match(r.URL.Path); rule != nil {
		if !s.Policy.Allowed(r.URL.Path, remoteIP) {
			return &ippHTTPError{status: http.StatusForbidden}
		}
		if limit := s.Policy.LimitFor(r.URL.Path, r.Method); limit != nil {
			if limit.DenyAll || !s.Policy.AllowedByLimit(limit, remoteIP) {
				return &ippHTTPError{status: http.StatusForbidden}
			}
			if limitRequiresAuth(limit) {
				authType := s.normalizeAuthType(limit.AuthType)
				if authType == "" {
					authType = strings.TrimSpace(s.Config.DefaultAuthType)
				}
				u, ok := s.authenticateUser(ctx, r, authType)
				if !ok {
					return &ippHTTPError{status: http.StatusUnauthorized, authType: authType}
				}
				if !userAllowedByLimit(u, "", limit) {
					return &ippHTTPError{status: http.StatusForbidden}
				}
			}
		} else if locationRequiresAuth(rule) {
			authType := s.normalizeAuthType(rule.AuthType)
			if authType == "" {
				authType = strings.TrimSpace(s.Config.DefaultAuthType)
			}
			u, ok := s.authenticateUser(ctx, r, authType)
			if !ok {
				return &ippHTTPError{status: http.StatusUnauthorized, authType: authType}
			}
			if !userAllowedByLocation(u, rule) {
				return &ippHTTPError{status: http.StatusForbidden}
			}
		}
	}
	return nil
}

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
	if err := s.enforceHTTPLocationPolicy(ctx, r); err != nil {
		var httpErr *ippHTTPError
		if errors.As(err, &httpErr) {
			if httpErr.status == http.StatusUnauthorized {
				setAuthChallenge(w, httpErr.authType)
			}
			msg := httpErr.message
			if msg == "" {
				msg = http.StatusText(httpErr.status)
			}
			http.Error(w, msg, httpErr.status)
			return nil
		}
		return err
	}

	// CUPS-Create-Local-Printer returns an IPP forbidden status for non-local
	// clients (it is not a policy-based HTTP 401/403).
	if op == goipp.OpCupsCreateLocalPrinter && !isLocalhostRequest(r) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorForbidden, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Only local users can create a local printer.")))
		w.Header().Set("Content-Type", goipp.ContentType)
		w.WriteHeader(http.StatusOK)
		_ = resp.Encode(w)
		return nil
	}

	if err := s.enforceIPPOpPolicy(ctx, r, &req); err != nil {
		var httpErr *ippHTTPError
		if errors.As(err, &httpErr) {
			if httpErr.status == http.StatusUnauthorized {
				setAuthChallenge(w, httpErr.authType)
			}
			msg := httpErr.message
			if msg == "" {
				msg = http.StatusText(httpErr.status)
			}
			http.Error(w, msg, httpErr.status)
			return nil
		}
		return err
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
	case goipp.OpCupsCreateLocalPrinter:
		resp, err = s.handleCupsCreateLocalPrinter(ctx, r, &req)
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
		var httpErr *ippHTTPError
		if errors.As(err, &httpErr) {
			if httpErr.status == http.StatusUnauthorized {
				setAuthChallenge(w, httpErr.authType)
			}
			msg := httpErr.message
			if msg == "" {
				msg = http.StatusText(httpErr.status)
			}
			http.Error(w, msg, httpErr.status)
			return nil
		}
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
		authInfo := s.authInfoRequiredForDestination(true, dest.Class.Name, dest.Class.DefaultOptions)
		addClassAttributes(ctx, resp, dest.Class, members, r, req, s.Store, s.Config, authInfo)
	} else {
		authInfo := s.authInfoRequiredForDestination(false, dest.Printer.Name, dest.Printer.DefaultOptions)
		addPrinterAttributes(ctx, resp, dest.Printer, r, req, s.Store, s.Config, authInfo)
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
	defaultOpts, jobSheetsDefault, jobSheetsOk, sharedPtr := collectPrinterDefaultOptions(req.Printer)

	// Match CUPS behavior: you cannot save (persistent) default values for a
	// temporary queue.
	if printer.IsTemporary && (sharedPtr != nil || jobSheetsOk || len(defaultOpts) > 0) {
		attrName := "printer-defaults"
		switch {
		case sharedPtr != nil:
			attrName = "printer-is-shared"
		case jobSheetsOk:
			attrName = "job-sheets-default"
		case len(defaultOpts) > 0:
			keys := make([]string, 0, len(defaultOpts))
			for k := range defaultOpts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				attrName = keys[0]
			}
		}
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(fmt.Sprintf("Unable to save value for \"%s\" with a temporary printer.", attrName))))
		return resp, nil
	}

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
		if err := s.Store.UpdatePrinterAttributes(ctx, tx, printer.ID, infoPtr, locPtr, geoPtr, orgPtr, orgUnitPtr); err != nil {
			return err
		}
		if sharedPtr != nil {
			if err := s.Store.UpdatePrinterSharing(ctx, tx, printer.ID, *sharedPtr); err != nil {
				return err
			}
		}
		if jobSheetsOk {
			if err := s.Store.UpdatePrinterJobSheetsDefault(ctx, tx, printer.ID, jobSheetsDefault); err != nil {
				return err
			}
		}
		if len(defaultOpts) > 0 {
			if jobSheetsOk {
				defaultOpts["job-sheets"] = jobSheetsDefault
			}
			merged := parseJobOptions(printer.DefaultOptions)
			applyDefaultOptionUpdates(merged, defaultOpts)
			if b, err := json.Marshal(merged); err == nil {
				if err := s.Store.UpdatePrinterDefaultOptions(ctx, tx, printer.ID, string(b)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	updated, _ := s.resolvePrinter(ctx, r, req)
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	authInfo := s.authInfoRequiredForDestination(false, updated.Name, updated.DefaultOptions)
	addPrinterAttributes(ctx, resp, updated, r, req, s.Store, s.Config, authInfo)
	return resp, nil
}

func (s *Server) handleCupsGetPrinters(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var printers []model.Printer
	var classes []model.Class
	memberMap := map[int64][]model.Printer{}
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		printers, err = s.Store.ListPrinters(ctx, tx)
		if err != nil {
			return err
		}
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
	if len(printers) == 0 && len(classes) == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	limit := int(attrInt(req.Operation, "limit"))
	if limit <= 0 {
		limit = 10000000
	}
	firstName, _ := attrValue(req.Operation, "first-printer-name")
	printerID, printerIDPresent := attrIntPresent(req.Operation, "printer-id")
	if printerIDPresent && printerID <= 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
	}
	printerType := int(attrInt(req.Operation, "printer-type"))
	printerMask := int(attrInt(req.Operation, "printer-type-mask"))
	location, _ := attrValue(req.Operation, "printer-location")
	location = strings.TrimSpace(location)
	username := requestUserForFilter(r, req)
	local := isLocalRequest(r)
	shareServer := sharingEnabled(r, s.Store)

	type destEntry struct {
		name    string
		isClass bool
		printer model.Printer
		class   model.Class
		members []model.Printer
	}
	entries := make([]destEntry, 0, len(printers)+len(classes))
	for _, p := range printers {
		entries = append(entries, destEntry{name: p.Name, printer: p})
	}
	for _, c := range classes {
		entries = append(entries, destEntry{name: c.Name, isClass: true, class: c, members: memberMap[c.ID]})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
	})

	start := 0
	if firstName != "" {
		found := false
		for i, e := range entries {
			if strings.EqualFold(e.name, firstName) {
				start = i
				found = true
				break
			}
		}
		if !found {
			start = 0
		}
	}

	groups := make(goipp.Groups, 0, len(entries)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	count := 0
	for i := start; i < len(entries) && count < limit; i++ {
		e := entries[i]
		effectiveShared := shareServer
		if !e.isClass {
			effectiveShared = shareServer && e.printer.Shared
		}
		if !local && !effectiveShared {
			continue
		}
		if printerID != 0 {
			if e.isClass {
				if e.class.ID != printerID {
					continue
				}
			} else if e.printer.ID != printerID {
				continue
			}
		}
		if location != "" {
			if e.isClass {
				if !strings.EqualFold(strings.TrimSpace(e.class.Location), location) {
					continue
				}
			} else if !strings.EqualFold(strings.TrimSpace(e.printer.Location), location) {
				continue
			}
		}
		if printerType != 0 || printerMask != 0 {
			if e.isClass {
				authInfo := s.authInfoRequiredForDestination(true, e.class.Name, e.class.DefaultOptions)
				ptype := computeClassType(e.class, shareServer, authInfo)
				if (ptype & printerMask) != printerType {
					continue
				}
			} else {
				authInfo := s.authInfoRequiredForDestination(false, e.printer.Name, e.printer.DefaultOptions)
				ppd, _ := loadPPDForPrinter(e.printer)
				caps := computePrinterCaps(ppd, parseJobOptions(e.printer.DefaultOptions))
				ptype := computePrinterType(e.printer, caps, ppd, false, authInfo)
				if (ptype & printerMask) != printerType {
					continue
				}
			}
		}
		if username != "" {
			if e.isClass {
				if !s.userAllowedForClass(ctx, e.class, username) {
					continue
				}
			} else if !s.userAllowedForPrinter(ctx, e.printer, username) {
				continue
			}
		}
		if e.isClass {
			authInfo := s.authInfoRequiredForDestination(true, e.class.Name, e.class.DefaultOptions)
			attrs := classAttributesWithMembers(ctx, e.class, e.members, r, req, s.Store, s.Config, authInfo)
			groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
			count++
			continue
		}
		authInfo := s.authInfoRequiredForDestination(false, e.printer.Name, e.printer.DefaultOptions)
		attrs := buildPrinterAttributes(ctx, e.printer, r, req, s.Store, s.Config, authInfo)
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
		count++
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetClasses(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	var classes []model.Class
	printerCount := 0
	memberMap := map[int64][]model.Printer{}
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		classes, err = s.Store.ListClasses(ctx, tx)
		if err != nil {
			return err
		}
		printers, err := s.Store.ListPrinters(ctx, tx)
		if err != nil {
			return err
		}
		printerCount = len(printers)
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
	if len(classes) == 0 && printerCount == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	limit := int(attrInt(req.Operation, "limit"))
	if limit <= 0 {
		limit = 10000000
	}
	firstName, _ := attrValue(req.Operation, "first-printer-name")
	classID, classIDPresent := attrIntPresent(req.Operation, "printer-id")
	if classIDPresent && classID <= 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
	}
	printerType := int(attrInt(req.Operation, "printer-type"))
	printerMask := int(attrInt(req.Operation, "printer-type-mask"))
	location, _ := attrValue(req.Operation, "printer-location")
	location = strings.TrimSpace(location)
	username := requestUserForFilter(r, req)
	local := isLocalRequest(r)
	shareServer := sharingEnabled(r, s.Store)
	start := 0
	if firstName != "" {
		for i, c := range classes {
			if c.Name == firstName {
				start = i
				break
			}
		}
	}

	groups := make(goipp.Groups, 0, len(classes)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	count := 0
	for i := start; i < len(classes) && count < limit; i++ {
		c := classes[i]
		if !local && !shareServer {
			continue
		}
		if classID != 0 && c.ID != classID {
			continue
		}
		if location != "" && !strings.EqualFold(strings.TrimSpace(c.Location), location) {
			continue
		}
		if printerType != 0 || printerMask != 0 {
			authInfo := s.authInfoRequiredForDestination(true, c.Name, c.DefaultOptions)
			ptype := computeClassType(c, shareServer, authInfo)
			if (ptype & printerMask) != printerType {
				continue
			}
		}
		if username != "" && !s.userAllowedForClass(ctx, c, username) {
			continue
		}
		authInfo := s.authInfoRequiredForDestination(true, c.Name, c.DefaultOptions)
		attrs := classAttributesWithMembers(ctx, c, memberMap[c.ID], r, req, s.Store, s.Config, authInfo)
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
		count++
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleCupsGetDevices(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	limit := int(attrInt(req.Operation, "limit"))
	if limit <= 0 {
		limit = 10000000
	}
	timeout := int(attrInt(req.Operation, "timeout"))
	if timeout <= 0 {
		timeout = 15
	}
	requested, all := requestedAttributes(req)
	includeSchemes := normalizeSchemeSet(attrStrings(req.Operation, "include-schemes"))
	excludeSchemes := normalizeSchemeSet(attrStrings(req.Operation, "exclude-schemes"))

	ctxDiscover := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		ctxDiscover, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	devices := discoverDevices(ctxDiscover, s.Store, true)
	sort.SliceStable(devices, func(i, j int) bool {
		leftInfo := strings.ToLower(strings.TrimSpace(devices[i].Info))
		rightInfo := strings.ToLower(strings.TrimSpace(devices[j].Info))
		if leftInfo != rightInfo {
			return leftInfo < rightInfo
		}
		leftClass := strings.ToLower(strings.TrimSpace(devices[i].Class))
		rightClass := strings.ToLower(strings.TrimSpace(devices[j].Class))
		if leftClass != rightClass {
			return leftClass < rightClass
		}
		leftURI := strings.ToLower(strings.TrimSpace(devices[i].URI))
		rightURI := strings.ToLower(strings.TrimSpace(devices[j].URI))
		return leftURI < rightURI
	})

	groups := make(goipp.Groups, 0, len(devices)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	count := 0
	for _, d := range devices {
		if count >= limit {
			break
		}
		if !schemeAllowed(d.URI, includeSchemes, excludeSchemes) {
			continue
		}
		attrs := goipp.Attributes{}
		addAttr := func(name string, tag goipp.Tag, value string) {
			if !all && !requested[name] {
				return
			}
			if strings.TrimSpace(value) == "" {
				return
			}
			attrs.Add(goipp.MakeAttribute(name, tag, goipp.String(value)))
		}
		addAttr("device-uri", goipp.TagURI, d.URI)
		addAttr("device-info", goipp.TagText, d.Info)
		addAttr("device-make-and-model", goipp.TagText, d.Make)
		addAttr("device-id", goipp.TagText, d.DeviceID)
		addAttr("device-location", goipp.TagText, d.Location)
		addAttr("device-class", goipp.TagKeyword, d.Class)
		if len(attrs) == 0 {
			continue
		}
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
		count++
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
	maxInt := int(^uint(0) >> 1)
	limitVal := attrInt(req.Operation, "limit")
	limit := maxInt
	if limitVal > 0 && limitVal < int64(maxInt) {
		limit = int(limitVal)
	}
	deviceID := strings.TrimSpace(attrString(req.Operation, "ppd-device-id"))
	language := strings.TrimSpace(attrString(req.Operation, "ppd-natural-language"))
	makeVal := strings.TrimSpace(attrString(req.Operation, "ppd-make"))
	makeModel := strings.TrimSpace(attrString(req.Operation, "ppd-make-and-model"))
	modelNumber := attrInt(req.Operation, "ppd-model-number")
	product := strings.TrimSpace(attrString(req.Operation, "ppd-product"))
	psversion := strings.TrimSpace(attrString(req.Operation, "ppd-psversion"))
	typeStr := strings.TrimSpace(attrString(req.Operation, "ppd-type"))
	if normType, ok := normalizePPDType(typeStr); ok {
		typeStr = normType
	} else {
		typeStr = ""
	}
	excludeSchemes := normalizeSchemeSet(attrStrings(req.Operation, "exclude-schemes"))
	includeSchemes := normalizeSchemeSet(attrStrings(req.Operation, "include-schemes"))

	hasFilters := deviceID != "" || language != "" || makeVal != "" || makeModel != "" || modelNumber != 0 || product != "" || psversion != "" || typeStr != ""
	deviceRe := compilePPDDeviceIDRegex(deviceID)
	makeModelRe := compilePPDStringRegex(makeModel)
	makeModelLen := len(makeModel)
	productLen := len(product)

	type ppdEntry struct {
		name    string
		ppd     *config.PPD
		matches int
	}
	entries := make([]ppdEntry, 0, len(names))
	for _, name := range names {
		ppd, err := loadPPDForListing(name, ppdDir)
		if err != nil || ppd == nil {
			if name == model.DefaultPPDName {
				ppd = &config.PPD{
					Make:         "CUPS-Golang",
					MakeAndModel: "CUPS-Golang Generic Printer",
					Languages:    []string{"en"},
					PPDType:      "postscript",
					Scheme:       "file",
				}
			} else {
				continue
			}
		}
		if !ppdValidForListing(ppd) && name != model.DefaultPPDName {
			continue
		}
		scheme := strings.TrimSpace(ppd.Scheme)
		if scheme == "" {
			scheme = "file"
		}
		if !schemeNameAllowed(scheme, includeSchemes, excludeSchemes) {
			continue
		}
		entry := ppdEntry{name: name, ppd: ppd}
		if hasFilters {
			entry.matches = scorePPDMatch(ppd, deviceRe, language, makeVal, makeModelRe, makeModelLen, modelNumber, product, productLen, psversion, typeStr)
			if entry.matches == 0 {
				continue
			}
		}
		entries = append(entries, entry)
	}
	if hasFilters {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].matches != entries[j].matches {
				return entries[i].matches > entries[j].matches
			}
			return strings.EqualFold(ppdMakeAndModel(entries[i].ppd), ppdMakeAndModel(entries[j].ppd))
		})
	} else {
		sort.SliceStable(entries, func(i, j int) bool {
			mi := strings.ToLower(ppdMake(entries[i].ppd))
			mj := strings.ToLower(ppdMake(entries[j].ppd))
			if mi != mj {
				return mi < mj
			}
			return strings.EqualFold(ppdMakeAndModel(entries[i].ppd), ppdMakeAndModel(entries[j].ppd))
		})
	}

	groups := make(goipp.Groups, 0, len(entries)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	sendName := all || requested["ppd-name"]
	sendMake := all || requested["ppd-make"]
	sendMakeModel := all || requested["ppd-make-and-model"]
	sendModelNumber := all || requested["ppd-model-number"]
	sendNaturalLang := all || requested["ppd-natural-language"]
	sendDeviceID := all || requested["ppd-device-id"]
	sendProduct := all || requested["ppd-product"]
	sendPSVersion := all || requested["ppd-psversion"]
	sendType := all || requested["ppd-type"]

	onlyMake := !all && len(requested) == 1 && requested["ppd-make"]
	count := 0
	lastMake := ""
	for _, entry := range entries {
		if count >= limit {
			break
		}
		ppd := entry.ppd
		if ppd == nil {
			continue
		}
		makeValue := ppdMake(ppd)
		if onlyMake {
			if strings.EqualFold(makeValue, lastMake) {
				continue
			}
			lastMake = makeValue
		}
		attrs := goipp.Attributes{}
		if sendName {
			attrs.Add(goipp.MakeAttribute("ppd-name", goipp.TagName, goipp.String(entry.name)))
		}
		if sendNaturalLang {
			attrs.Add(makeLanguagesAttr("ppd-natural-language", ppdLanguages(ppd)))
		}
		if sendMake {
			if strings.TrimSpace(makeValue) != "" {
				attrs.Add(goipp.MakeAttribute("ppd-make", goipp.TagText, goipp.String(makeValue)))
			}
		}
		if sendMakeModel {
			if mm := ppdMakeAndModel(ppd); strings.TrimSpace(mm) != "" {
				attrs.Add(goipp.MakeAttribute("ppd-make-and-model", goipp.TagText, goipp.String(mm)))
			}
		}
		if sendDeviceID && strings.TrimSpace(ppd.DeviceID) != "" {
			attrs.Add(goipp.MakeAttribute("ppd-device-id", goipp.TagText, goipp.String(ppd.DeviceID)))
		}
		if sendProduct && len(ppd.Products) > 0 {
			attrs.Add(makeTextsAttr("ppd-product", ppd.Products))
		}
		if sendPSVersion && len(ppd.PSVersions) > 0 {
			attrs.Add(makeTextsAttr("ppd-psversion", ppd.PSVersions))
		}
		if sendType && strings.TrimSpace(ppd.PPDType) != "" {
			attrs.Add(goipp.MakeAttribute("ppd-type", goipp.TagKeyword, goipp.String(ppd.PPDType)))
		}
		if sendModelNumber && ppd.ModelNumber != 0 {
			attrs.Add(goipp.MakeAttribute("ppd-model-number", goipp.TagInteger, goipp.Integer(ppd.ModelNumber)))
		}
		if len(attrs) == 0 {
			continue
		}
		groups = append(groups, goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: attrs})
		count++
	}
	status := goipp.StatusOk
	if len(groups) == 1 {
		status = goipp.StatusErrorNotFound
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(status), req.RequestID, groups)
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

func (s *Server) handleCupsCreateLocalPrinter(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	// Defense in depth: handleIPPRequest already blocks non-local callers.
	if !isLocalhostRequest(r) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorForbidden, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Only local users can create a local printer.")))
		return resp, nil
	}

	// Required attributes live in the printer group.
	rawNameAttr := attrByName(req.Printer, "printer-name")
	if rawNameAttr == nil {
		if attrByName(req.Operation, "printer-name") != nil {
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
			addOperationDefaults(resp)
			resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"printer-name\" is in the wrong group.")))
			return resp, nil
		}
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Missing required attribute \"printer-name\".")))
		return resp, nil
	}
	if len(rawNameAttr.Values) == 0 || rawNameAttr.Values[0].T != goipp.TagName {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"printer-name\" is the wrong value type.")))
		return resp, nil
	}

	rawName := strings.TrimSpace(rawNameAttr.Values[0].V.String())
	name := sanitizeLocalPrinterName(rawName)
	if strings.TrimSpace(name) == "" {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"printer-name\" has empty value.")))
		return resp, nil
	}

	deviceURIAttr := attrByName(req.Printer, "device-uri")
	if deviceURIAttr == nil {
		if attrByName(req.Operation, "device-uri") != nil {
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
			addOperationDefaults(resp)
			resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"device-uri\" is in the wrong group.")))
			return resp, nil
		}
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Missing required attribute \"device-uri\".")))
		return resp, nil
	}
	if len(deviceURIAttr.Values) == 0 || deviceURIAttr.Values[0].T != goipp.TagURI {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"device-uri\" is the wrong value type.")))
		return resp, nil
	}

	deviceURI := strings.TrimSpace(deviceURIAttr.Values[0].V.String())
	if deviceURI == "" {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Attribute \"device-uri\" has empty value.")))
		return resp, nil
	}

	// Optional attributes (ignore wrong type, matching CUPS ippFindAttribute behavior).
	geo := optionalStringAttr(req.Printer, "printer-geo-location", goipp.TagURI)
	info := optionalStringAttr(req.Printer, "printer-info", goipp.TagText)
	location := optionalStringAttr(req.Printer, "printer-location", goipp.TagText)

	localizedDeviceURI := localizeDeviceURIForLocalQueue(deviceURI, s.Config)

	var printer model.Printer
	alreadyExists := false
	existsName := ""
	createdNew := false
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		// Prefer name match (CUPS does name match first).
		if p, err := s.Store.GetPrinterByName(ctx, tx, name); err == nil {
			printer = p
			alreadyExists = true
			existsName = p.Name
			_ = s.Store.TouchPrinter(ctx, tx, p.ID)
			return nil
		}

		// Then check device URI match. Compare both raw and localized URI to avoid
		// duplicates when the queue has been localized to localhost.
		if p, err := s.Store.GetPrinterByURI(ctx, tx, deviceURI); err == nil {
			printer = p
			alreadyExists = true
			existsName = p.Name
			_ = s.Store.TouchPrinter(ctx, tx, p.ID)
			return nil
		}
		if localizedDeviceURI != deviceURI {
			if p, err := s.Store.GetPrinterByURI(ctx, tx, localizedDeviceURI); err == nil {
				printer = p
				alreadyExists = true
				existsName = p.Name
				_ = s.Store.TouchPrinter(ctx, tx, p.ID)
				return nil
			}
		}

		// Create the queue (temporary + not shared).
		p, err := s.Store.CreatePrinter(ctx, tx, name, localizedDeviceURI, location, info, model.DefaultPPDName, true, false, false, "none", "")
		if err != nil {
			return err
		}
		printer = p
		createdNew = true
		if err := s.Store.UpdatePrinterTemporary(ctx, tx, printer.ID, true); err != nil {
			return err
		}
		printer.IsTemporary = true

		// Persist optional metadata.
		var geoPtr *string
		if strings.TrimSpace(geo) != "" {
			geoPtr = &geo
			printer.Geo = geo
		}
		if geoPtr != nil {
			_ = s.Store.UpdatePrinterAttributes(ctx, tx, printer.ID, nil, nil, geoPtr, nil, nil)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Existing queues return immediately.
	if alreadyExists || !createdNew {
		resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
		addOperationDefaults(resp)
		if alreadyExists {
			msg := fmt.Sprintf("Printer \"%s\" already exists.", existsName)
			resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(msg)))
		}
		resp.Printer.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(printer.Accepting)))
		resp.Printer.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(printer.State)))
		resp.Printer.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(printerStateReason(printer))))
		resp.Printer.Add(goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String(printerURIFor(printer, r))))
		return resp, nil
	}

	// CUPS resolves mDNS URIs and generates a queue-specific PPD from the target
	// printer's IPP response. If this step fails, the temporary queue is removed.
	resolvedURI := strings.TrimSpace(printer.URI)
	if strings.Contains(resolvedURI, "._tcp") || strings.HasPrefix(strings.ToLower(resolvedURI), "dnssd://") {
		u, rerr := resolveMDNSURI(ctx, resolvedURI)
		if rerr != nil {
			_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
				p, err2 := s.Store.GetPrinterByID(ctx, tx, printer.ID)
				if err2 == nil && p.IsTemporary {
					_ = s.Store.DeletePrinter(ctx, tx, p.ID)
				}
				return nil
			})
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorDevice, req.RequestID)
			addOperationDefaults(resp)
			resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(rerr.Error())))
			return resp, nil
		}
		if strings.TrimSpace(u) != "" {
			resolvedURI = localizeDeviceURIForLocalQueue(u, s.Config)
		}
	}

	supported, err := getPrinterAttributesForLocalQueue(ctx, resolvedURI)
	if err != nil {
		// Best-effort cleanup of the temporary queue on failure.
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			p, err2 := s.Store.GetPrinterByID(ctx, tx, printer.ID)
			if err2 == nil && p.IsTemporary {
				_ = s.Store.DeletePrinter(ctx, tx, p.ID)
			}
			return nil
		})

		resp := goipp.NewResponse(req.Version, goipp.StatusErrorDevice, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(err.Error())))
		return resp, nil
	}

	ppdName, err := generatePPDFromIPP(s.Config.PPDDir, printer.Name, supported)
	if err != nil {
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			p, err2 := s.Store.GetPrinterByID(ctx, tx, printer.ID)
			if err2 == nil && p.IsTemporary {
				_ = s.Store.DeletePrinter(ctx, tx, p.ID)
			}
			return nil
		})
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorDevice, req.RequestID)
		addOperationDefaults(resp)
		resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(err.Error())))
		return resp, nil
	}

	remoteInfo := strings.TrimSpace(attrString(supported.Printer, "printer-info"))
	remoteLocation := strings.TrimSpace(attrString(supported.Printer, "printer-location"))
	remoteGeo := strings.TrimSpace(attrString(supported.Printer, "printer-geo-location"))
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByID(ctx, tx, printer.ID)
		if err != nil {
			return err
		}
		if !p.IsTemporary {
			return fmt.Errorf("printer no longer temporary")
		}
		if strings.TrimSpace(resolvedURI) != "" && !strings.EqualFold(strings.TrimSpace(resolvedURI), strings.TrimSpace(p.URI)) {
			if err := s.Store.UpdatePrinterURI(ctx, tx, p.ID, resolvedURI); err != nil {
				return err
			}
			p.URI = resolvedURI
		}
		if err := s.Store.UpdatePrinterPPDName(ctx, tx, p.ID, ppdName); err != nil {
			return err
		}
		p.PPDName = ppdName

		var infoPtr, locPtr, geoPtr *string
		if strings.TrimSpace(p.Info) == "" && remoteInfo != "" {
			infoPtr = &remoteInfo
			p.Info = remoteInfo
		}
		if strings.TrimSpace(p.Location) == "" && remoteLocation != "" {
			locPtr = &remoteLocation
			p.Location = remoteLocation
		}
		if strings.TrimSpace(p.Geo) == "" && remoteGeo != "" {
			geoPtr = &remoteGeo
			p.Geo = remoteGeo
		}
		if infoPtr != nil || locPtr != nil || geoPtr != nil {
			if err := s.Store.UpdatePrinterAttributes(ctx, tx, p.ID, infoPtr, locPtr, geoPtr, nil, nil); err != nil {
				return err
			}
		}
		printer = p
		return nil
	})
	if err != nil {
		_ = os.Remove(safePPDPath(s.Config.PPDDir, ppdName))
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			p, err2 := s.Store.GetPrinterByID(ctx, tx, printer.ID)
			if err2 == nil && p.IsTemporary {
				_ = s.Store.DeletePrinter(ctx, tx, p.ID)
			}
			return nil
		})
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String("Local printer created.")))
	resp.Printer.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(printer.Accepting)))
	resp.Printer.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(printer.State)))
	resp.Printer.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(printerStateReason(printer))))
	resp.Printer.Add(goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String(printerURIFor(printer, r))))
	return resp, nil
}

func optionalStringAttr(attrs goipp.Attributes, name string, tag goipp.Tag) string {
	attr := attrByName(attrs, name)
	if attr == nil || len(attr.Values) == 0 {
		return ""
	}
	actual := attr.Values[0].T
	if actual != tag {
		// Accept "*-with-language" values for optional text attributes.
		if !(tag == goipp.TagText && actual == goipp.TagTextLang) {
			return ""
		}
	}
	return strings.TrimSpace(attr.Values[0].V.String())
}

func sanitizeLocalPrinterName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	const maxLen = 127 // CUPS uses a 128-byte buffer including NUL terminator.
	var b strings.Builder
	b.Grow(len(raw))
	prevUnderscore := false
	for i := 0; i < len(raw) && b.Len() < maxLen; i++ {
		c := raw[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			prevUnderscore = false
			continue
		}
		if b.Len() == 0 || !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return b.String()
}

func localizeDeviceURIForLocalQueue(deviceURI string, cfg config.Config) string {
	u, err := url.Parse(deviceURI)
	if err != nil || u == nil {
		return deviceURI
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return deviceURI
	}

	serverHost := strings.TrimSpace(cfg.DNSSDHostName)
	fromServerName := false
	if serverHost == "" {
		serverHost = strings.TrimSpace(cfg.ServerName)
		fromServerName = true
	}
	serverHost = strings.TrimSpace(serverHost)
	if serverHost == "" {
		return deviceURI
	}
	if h, _, err := net.SplitHostPort(serverHost); err == nil {
		serverHost = h
	}
	serverHost = strings.Trim(serverHost, "[]")

	normalize := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(s, ".")
		return s
	}

	hostCmp := normalize(host)
	serverCmp := normalize(serverHost)

	// When we have only a ServerName (Browsing=Off), it may lack ".local" while
	// the requested device URI includes it. Match these like CUPS does.
	if fromServerName && strings.HasSuffix(hostCmp, ".local") && !strings.HasSuffix(serverCmp, ".local") {
		hostCmp = strings.TrimSuffix(hostCmp, ".local")
	}

	if hostCmp != serverCmp {
		return deviceURI
	}

	// Replace hostname with localhost while preserving port and userinfo/path.
	port := u.Port()
	if port != "" {
		u.Host = "localhost:" + port
	} else {
		u.Host = "localhost"
	}
	return u.String()
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
	addJobAttributes(resp, job, target, r, model.Document{}, 0, req)
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
	defaultOpts, jobSheetsDefault, jobSheetsOk, sharedPtr := collectPrinterDefaultOptions(req.Printer)

	// Match CUPS behavior: you cannot save (persistent) default values for a
	// temporary queue.
	if sharedPtr != nil || jobSheetsOk || len(defaultOpts) > 0 {
		var existing model.Printer
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			if p, err := s.Store.GetPrinterByName(ctx, tx, name); err == nil {
				existing = p
			}
			return nil
		})
		if existing.ID != 0 && existing.IsTemporary {
			attrName := "printer-defaults"
			switch {
			case sharedPtr != nil:
				attrName = "printer-is-shared"
			case jobSheetsOk:
				attrName = "job-sheets-default"
			case len(defaultOpts) > 0:
				keys := make([]string, 0, len(defaultOpts))
				for k := range defaultOpts {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if len(keys) > 0 {
					attrName = keys[0]
				}
			}
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID)
			addOperationDefaults(resp)
			resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(fmt.Sprintf("Unable to save value for \"%s\" with a temporary printer.", attrName))))
			return resp, nil
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
		if sharedPtr != nil {
			if err := s.Store.UpdatePrinterSharing(ctx, tx, printer.ID, *sharedPtr); err != nil {
				return err
			}
			printer.Shared = *sharedPtr
		}
		if jobSheetsOk {
			if err := s.Store.UpdatePrinterJobSheetsDefault(ctx, tx, printer.ID, jobSheetsDefault); err != nil {
				return err
			}
			printer.JobSheetsDefault = jobSheetsDefault
		}
		if len(defaultOpts) > 0 {
			if jobSheetsOk {
				defaultOpts["job-sheets"] = jobSheetsDefault
			}
			merged := parseJobOptions(printer.DefaultOptions)
			applyDefaultOptionUpdates(merged, defaultOpts)
			if b, err := json.Marshal(merged); err == nil {
				if err := s.Store.UpdatePrinterDefaultOptions(ctx, tx, printer.ID, string(b)); err != nil {
					return err
				}
				printer.DefaultOptions = string(b)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	authInfo := s.authInfoRequiredForDestination(false, printer.Name, printer.DefaultOptions)
	addPrinterAttributes(ctx, resp, printer, r, req, s.Store, s.Config, authInfo)
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

	var printer model.Printer
	docPaths := []string{}
	outPaths := []string{}
	ppdToRemove := ""

	// Collect document/output paths first so we don't orphan spool files after the
	// DB rows cascade-delete.
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.GetPrinterByName(ctx, tx, name)
		if err != nil {
			return err
		}
		jobIDs, err := s.Store.ListJobIDsByPrinter(ctx, tx, printer.ID)
		if err != nil {
			return err
		}
		for _, jobID := range jobIDs {
			docs, err := s.Store.ListDocumentsByJob(ctx, tx, jobID)
			if err != nil {
				return err
			}
			for _, d := range docs {
				if p := strings.TrimSpace(d.Path); p != "" {
					docPaths = append(docPaths, p)
				}
				if s.Spool.OutputDir != "" {
					if op := strings.TrimSpace(s.Spool.OutputPath(jobID, d.FileName)); op != "" {
						outPaths = append(outPaths, op)
					}
				}
			}
		}
		// Queue-specific PPD files are named like "<printer>.ppd" in CUPS.
		if ppdName := strings.TrimSpace(printer.PPDName); ppdName != "" && !strings.EqualFold(ppdName, model.DefaultPPDName) {
			base := filepath.Base(ppdName)
			if strings.EqualFold(base, printer.Name+".ppd") {
				ppdToRemove = base
			}
		}
		return nil
	})
	if err == nil {
		for _, p := range docPaths {
			_ = os.Remove(p)
		}
		for _, p := range outPaths {
			_ = os.Remove(p)
		}
		if ppdToRemove != "" {
			_ = os.Remove(safePPDPath(s.Config.PPDDir, ppdToRemove))
		}
		err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			// Re-check the printer still exists before deleting.
			p, err := s.Store.GetPrinterByID(ctx, tx, printer.ID)
			if err != nil {
				return err
			}
			return s.Store.DeletePrinter(ctx, tx, p.ID)
		})
	}
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
	defaultOpts, jobSheetsDefault, jobSheetsOk, _ := collectPrinterDefaultOptions(req.Printer)

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
		if len(defaultOpts) > 0 {
			if jobSheetsOk {
				defaultOpts["job-sheets"] = jobSheetsDefault
			}
			merged := parseJobOptions(class.DefaultOptions)
			applyDefaultOptionUpdates(merged, defaultOpts)
			if b, err := json.Marshal(merged); err == nil {
				if err := s.Store.UpdateClassDefaultOptions(ctx, tx, class.ID, string(b)); err != nil {
					return err
				}
				class.DefaultOptions = string(b)
			}
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
	authInfo := s.authInfoRequiredForDestination(true, class.Name, class.DefaultOptions)
	addClassAttributes(ctx, resp, class, members, r, req, s.Store, s.Config, authInfo)
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
		authInfo := s.authInfoRequiredForDestination(true, dest.Class.Name, dest.Class.DefaultOptions)
		addClassAttributes(ctx, resp, dest.Class, members, r, req, s.Store, s.Config, authInfo)
	} else {
		authInfo := s.authInfoRequiredForDestination(false, dest.Printer.Name, dest.Printer.DefaultOptions)
		addPrinterAttributes(ctx, resp, dest.Printer, r, req, s.Store, s.Config, authInfo)
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
	events, lease, leasePresent, recipient, pullMethod, interval, userData, err := parseSubscriptionRequest(req)
	if err != nil {
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	defaultEvents, defaultLease := subscriptionDefaultsForPrinter(dest.Printer, s.Config)
	if strings.TrimSpace(events) == "" {
		events = defaultEvents
	}
	if !leasePresent {
		lease = defaultLease
	}
	lease = clampLeaseDuration(lease, s.Config)
	if strings.TrimSpace(recipient) != "" {
		u, err := url.Parse(recipient)
		if err != nil || strings.TrimSpace(u.Scheme) == "" {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		if !strings.EqualFold(u.Scheme, "ippget") {
			if schemes := notifySchemesSupported(s.Config); !stringInList(u.Scheme, schemes) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
			}
		}
	}
	owner := requestingUserName(req, r)
	var sub model.Subscription
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if s.Config.MaxSubscriptions > 0 {
			if count, err := s.Store.CountSubscriptions(ctx, tx); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptions {
				return errTooManySubs
			}
		}
		if s.Config.MaxSubscriptionsPerPrinter > 0 {
			if count, err := s.Store.CountSubscriptionsForPrinter(ctx, tx, dest.Printer.ID); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptionsPerPrinter {
				return errTooManySubs
			}
		}
		if s.Config.MaxSubscriptionsPerUser > 0 {
			if count, err := s.Store.CountSubscriptionsForUser(ctx, tx, owner); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptionsPerUser {
				return errTooManySubs
			}
		}
		var err error
		sub, err = s.Store.CreateSubscription(ctx, tx, &dest.Printer.ID, nil, events, lease, owner, recipient, pullMethod, interval, userData)
		return err
	})
	if err != nil {
		if errors.Is(err, errTooManySubs) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorTooManySubscriptions, req.RequestID), nil
		}
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
	events, lease, leasePresent, recipient, pullMethod, interval, userData, err := parseSubscriptionRequest(req)
	if err != nil {
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	if !leasePresent {
		lease = int64(s.Config.DefaultLeaseDuration)
	}
	lease = clampLeaseDuration(lease, s.Config)
	if strings.TrimSpace(recipient) != "" {
		u, err := url.Parse(recipient)
		if err != nil || strings.TrimSpace(u.Scheme) == "" {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		if !strings.EqualFold(u.Scheme, "ippget") {
			if schemes := notifySchemesSupported(s.Config); !stringInList(u.Scheme, schemes) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
			}
		}
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
		defaultEvents := "job-completed"
		defaultLease := int64(s.Config.DefaultLeaseDuration)
		if printer, err := s.Store.GetPrinterByID(ctx, tx, job.PrinterID); err == nil {
			defaultEvents, defaultLease = subscriptionDefaultsForPrinter(printer, s.Config)
		}
		if strings.TrimSpace(events) == "" {
			events = defaultEvents
		}
		if !leasePresent {
			lease = defaultLease
		}
		if s.Config.MaxSubscriptions > 0 {
			if count, err := s.Store.CountSubscriptions(ctx, tx); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptions {
				return errTooManySubs
			}
		}
		if s.Config.MaxSubscriptionsPerJob > 0 {
			if count, err := s.Store.CountSubscriptionsForJob(ctx, tx, jobID); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptionsPerJob {
				return errTooManySubs
			}
		}
		if s.Config.MaxSubscriptionsPerUser > 0 {
			if count, err := s.Store.CountSubscriptionsForUser(ctx, tx, owner); err != nil {
				return err
			} else if count >= s.Config.MaxSubscriptionsPerUser {
				return errTooManySubs
			}
		}
		sub, err = s.Store.CreateSubscription(ctx, tx, nil, &jobID, events, lease, owner, recipient, pullMethod, interval, userData)
		return err
	})
	if err != nil {
		if errors.Is(err, errNotAuthorized) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
		if errors.Is(err, errTooManySubs) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorTooManySubscriptions, req.RequestID), nil
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
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	seqNums := attrInts(req.Operation, "notify-sequence-numbers")
	if len(seqNums) == 0 {
		if seq := attrInt(req.Operation, "notify-sequence-number"); seq != 0 {
			seqNums = []int64{seq}
		}
	}

	limit := s.Config.MaxEvents
	if limit <= 0 {
		limit = 10000000
	}
	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	if authType == "" {
		authType = "basic"
	}

	type subWithNotes struct {
		sub        model.Subscription
		notes      []model.Notification
		printerURI string
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
			item := subWithNotes{sub: sub, notes: filtered}

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
			}
			if sub.PrinterID.Valid {
				printer, err := s.Store.GetPrinterByID(ctx, tx, sub.PrinterID.Int64)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil {
					if printer.State == 4 && interval > 30 {
						interval = 30
					}
					if r != nil {
						item.printerURI = printerURIFor(printer, r)
					}
				}
			}
			collected = append(collected, item)
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

	now := time.Now().Unix()
	opAttrs := buildOperationDefaults()
	opAttrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(now)))
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
			attrs.Add(goipp.MakeAttribute("notify-printer-up-time", goipp.TagInteger, goipp.Integer(now)))
			attrs.Add(goipp.MakeAttribute("printer-state-change-time", goipp.TagInteger, goipp.Integer(n.CreatedAt.Unix())))
			if sub.JobID.Valid {
				attrs.Add(goipp.MakeAttribute("notify-job-id", goipp.TagInteger, goipp.Integer(sub.JobID.Int64)))
			}
			if item.printerURI != "" {
				attrs.Add(goipp.MakeAttribute("notify-printer-uri", goipp.TagURI, goipp.String(item.printerURI)))
			}
			if uuid := subscriptionUUIDFor(sub, r); uuid != "" {
				attrs.Add(goipp.MakeAttribute("notify-subscription-uuid", goipp.TagURI, goipp.String(uuid)))
			}
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

type subscriptionScopeKind int

const (
	subscriptionScopeAll subscriptionScopeKind = iota
	subscriptionScopePrinter
	subscriptionScopeClass
	subscriptionScopeJob
)

func parseSubscriptionScopeURI(raw string) (subscriptionScopeKind, string, int64, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return subscriptionScopeAll, "", 0, false
	}
	resource := strings.TrimSpace(u.Path)
	if resource == "" {
		resource = "/"
	}
	resource = path.Clean(resource)
	if resource == "." {
		resource = "/"
	}
	if !strings.HasPrefix(resource, "/") {
		resource = "/" + resource
	}

	switch resource {
	case "/", "/jobs", "/printers", "/classes":
		return subscriptionScopeAll, "", 0, true
	}

	if strings.HasPrefix(resource, "/jobs/") {
		rest := strings.TrimPrefix(resource, "/jobs/")
		if rest == "" || strings.Contains(rest, "/") {
			return subscriptionScopeAll, "", 0, false
		}
		jobID, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || jobID <= 0 {
			return subscriptionScopeAll, "", 0, false
		}
		return subscriptionScopeJob, "", jobID, true
	}

	if strings.HasPrefix(resource, "/printers/") {
		name := strings.TrimSpace(strings.TrimPrefix(resource, "/printers/"))
		if name == "" || strings.Contains(name, "/") {
			return subscriptionScopeAll, "", 0, false
		}
		return subscriptionScopePrinter, name, 0, true
	}

	if strings.HasPrefix(resource, "/classes/") {
		name := strings.TrimSpace(strings.TrimPrefix(resource, "/classes/"))
		if name == "" || strings.Contains(name, "/") {
			return subscriptionScopeAll, "", 0, false
		}
		return subscriptionScopeClass, name, 0, true
	}

	return subscriptionScopeAll, "", 0, false
}

func (s *Server) handleGetSubscriptions(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	scopeURI := strings.TrimSpace(attrString(req.Operation, "printer-uri"))
	if scopeURI == "" {
		scopeURI = strings.TrimSpace(attrString(req.Operation, "job-uri"))
	}
	if scopeURI == "" {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	scopeKind, scopeName, scopeJobID, ok := parseSubscriptionScopeURI(scopeURI)
	if !ok {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	var printerID *int64
	var jobID *int64
	classScoped := false
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		switch scopeKind {
		case subscriptionScopePrinter:
			p, err := s.Store.GetPrinterByName(ctx, tx, scopeName)
			if err != nil {
				return err
			}
			id := p.ID
			printerID = &id
		case subscriptionScopeClass:
			if _, err := s.Store.GetClassByName(ctx, tx, scopeName); err != nil {
				return err
			}
			classScoped = true
		case subscriptionScopeJob:
			id := scopeJobID
			jobID = &id
		}

		if scopeKind == subscriptionScopePrinter || scopeKind == subscriptionScopeClass {
			if id := attrInt(req.Operation, "notify-job-id"); id != 0 {
				v := id
				jobID = &v
			}
		}
		if jobID == nil {
			if id := attrInt(req.Subscription, "notify-job-id"); id != 0 {
				v := id
				jobID = &v
			} else if id := attrInt(req.Operation, "job-id"); id != 0 {
				v := id
				jobID = &v
			}
		}
		if jobID != nil {
			_, err := s.Store.GetJob(ctx, tx, *jobID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
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

	if classScoped {
		// Class-target subscriptions are not persisted separately yet.
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}

	var subs []model.Subscription
	err = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		if err := s.Store.PruneExpiredSubscriptions(ctx, tx); err != nil {
			return err
		}
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
		lease = clampLeaseDuration(lease, s.Config)
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
	jobName := sanitizeJobName(req)
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
	if err := s.enforceAuthInfo(r, req, goipp.OpPrintJob); err != nil {
		return nil, err
	}
	stripReadOnlyJobAttributes(req)
	originHost := jobOriginatingHostFromRequest(r, req)
	documentFormat := attrString(req.Operation, "document-format")
	documentFormatSupplied := strings.TrimSpace(documentFormat)
	if documentFormat == "" {
		documentFormat = "application/octet-stream"
	}
	documentNameSupplied := strings.TrimSpace(attrString(req.Operation, "document-name"))
	docName := documentNameSupplied
	if docName == "" {
		docName = jobName
	}
	ppdForFormats, _ := loadPPDForPrinter(printer)
	supportedFormats := supportedDocumentFormatsForPrinter(printer, ppdForFormats)
	if strings.EqualFold(documentFormat, "application/octet-stream") {
		if detected := detectDocumentFormat(docName); detected != "" && isDocumentFormatSupportedInList(supportedFormats, detected) {
			documentFormat = detected
		}
	}
	if !isDocumentFormatSupportedInList(supportedFormats, documentFormat) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorDocumentFormatNotSupported, req.RequestID)
		addOperationDefaults(resp)
		resp.Unsupported.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(documentFormat)))
		return resp, nil
	}

	var job model.Job
	var doc model.Document
	warn, err := validateRequestOptions(req, printer)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errConflicting) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorConflicting, req.RequestID), nil
		}
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	compression, err := compressionFromRequest(req)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	options := serializeJobOptions(req, printer)
	if dest.IsClass {
		options = addClassInternalOptions(options, dest.Class)
	}
	holdRequested := jobHoldRequested(options)
	reader := docReader
	if compression == "gzip" {
		gz, err := gzip.NewReader(docReader)
		if err != nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorDocumentUnprintable, req.RequestID), nil
		}
		defer gz.Close()
		reader = gz
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, jobName, userName, originHost, options)
		if err != nil {
			return err
		}
		if err := ensureJobUUID(ctx, tx, s, &job, printer, r); err != nil {
			return err
		}
		sp := spool.Spool{Dir: s.Spool.Dir, OutputDir: s.Spool.OutputDir}
		path, size, err := sp.Save(job.ID, jobName, reader)
		if err != nil {
			return err
		}
		doc, err = s.Store.AddDocument(ctx, tx, job.ID, docName, documentFormat, path, size, documentNameSupplied, documentFormatSupplied)
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
	applyWarning(resp, warn)
	addJobAttributes(resp, job, printer, r, doc, 1, req)
	return resp, nil
}

func (s *Server) handleCreateJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobName := sanitizeJobName(req)
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
	if err := s.enforceAuthInfo(r, req, goipp.OpCreateJob); err != nil {
		return nil, err
	}
	stripReadOnlyJobAttributes(req)
	originHost := jobOriginatingHostFromRequest(r, req)

	var job model.Job
	warn, err := validateRequestOptions(req, printer)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errConflicting) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorConflicting, req.RequestID), nil
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
	internalHold := false
	if !holdRequested {
		opts := parseJobOptions(options)
		timeout := s.Config.MultipleOperationTimeout
		if timeout <= 0 {
			timeout = 900
		}
		opts["cups-hold-until"] = strconv.FormatInt(time.Now().Add(time.Duration(timeout)*time.Second).Unix(), 10)
		if b, err := json.Marshal(opts); err == nil {
			options = string(b)
			internalHold = true
		}
	}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.CreateJob(ctx, tx, printer.ID, jobName, userName, originHost, options)
		if err != nil {
			return err
		}
		if err := ensureJobUUID(ctx, tx, s, &job, printer, r); err != nil {
			return err
		}
		if holdRequested || internalHold {
			reason := "job-hold-until-specified"
			if !holdRequested {
				if strings.TrimSpace(getJobOption(options, "job-password")) != "" {
					reason = "job-password-specified"
				} else {
					reason = "job-incoming"
				}
			}
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-hold-until-specified", nil); err != nil {
				return err
			}
			job.State = 4
			job.StateReason = reason
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	applyWarning(resp, warn)
	addJobAttributes(resp, job, printer, r, model.Document{}, 0, req)
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

	lastDoc, lastDocPresent := attrBoolPresent(req.Operation, "last-document")
	if !lastDocPresent {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	compression, err := compressionFromRequest(req)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	documentFormat := attrString(req.Operation, "document-format")
	documentFormatSupplied := strings.TrimSpace(documentFormat)
	if documentFormat == "" {
		documentFormat = "application/octet-stream"
	}
	documentNameSupplied := strings.TrimSpace(attrString(req.Operation, "document-name"))

	var job model.Job
	var printer model.Printer
	var doc model.Document
	var docs []model.Document
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
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
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	docName := documentNameSupplied
	if docName == "" {
		docName = job.Name
	}
	ppdForFormats, _ := loadPPDForPrinter(printer)
	supportedFormats := supportedDocumentFormatsForPrinter(printer, ppdForFormats)
	if strings.EqualFold(documentFormat, "application/octet-stream") {
		if detected := detectDocumentFormat(docName); detected != "" && isDocumentFormatSupportedInList(supportedFormats, detected) {
			documentFormat = detected
		}
	}
	if !isDocumentFormatSupportedInList(supportedFormats, documentFormat) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorDocumentFormatNotSupported, req.RequestID)
		addOperationDefaults(resp)
		resp.Unsupported.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(documentFormat)))
		return resp, nil
	}

	authType := s.authTypeForRequest(r, goipp.OpSendDocument.String())
	if !s.canManageJob(ctx, r, req, authType, job, false) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	if job.State >= 7 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID), nil
	}

	if buf, ok := docReader.(*bytes.Buffer); ok && buf.Len() == 0 {
		if lastDoc && len(docs) > 0 {
			resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
			addOperationDefaults(resp)
			addJobAttributes(resp, job, printer, r, model.Document{}, len(docs), req)
			return resp, nil
		}
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	reader := docReader
	if compression == "gzip" {
		gz, err := gzip.NewReader(docReader)
		if err != nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorDocumentUnprintable, req.RequestID), nil
		}
		defer gz.Close()
		reader = gz
	}

	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		sp := spool.Spool{Dir: s.Spool.Dir, OutputDir: s.Spool.OutputDir}
		path, size, err := sp.Save(job.ID, job.Name, reader)
		if err != nil {
			return err
		}
		doc, err = s.Store.AddDocument(ctx, tx, job.ID, docName, documentFormat, path, size, documentNameSupplied, documentFormatSupplied)
		if err != nil {
			return err
		}
		opts := parseJobOptions(job.Options)
		holdRequested := jobHoldRequested(job.Options)
		if !holdRequested {
			timeout := s.Config.MultipleOperationTimeout
			if timeout <= 0 {
				timeout = 900
			}
			if !lastDoc {
				opts["cups-hold-until"] = strconv.FormatInt(time.Now().Add(time.Duration(timeout)*time.Second).Unix(), 10)
				if b, err := json.Marshal(opts); err == nil {
					optionsJSON := string(b)
					if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
						return err
					}
					job.Options = optionsJSON
				}
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-incoming", nil); err != nil {
					return err
				}
				job.State = 4
				job.StateReason = "job-incoming"
				return nil
			}
			delete(opts, "cups-hold-until")
			if strings.TrimSpace(opts["job-password"]) != "" {
				opts["cups-hold-until"] = strconv.FormatInt(time.Now().Add(15*time.Second).Unix(), 10)
				if b, err := json.Marshal(opts); err == nil {
					optionsJSON := string(b)
					if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
						return err
					}
					job.Options = optionsJSON
				}
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-password-specified", nil); err != nil {
					return err
				}
				job.State = 4
				job.StateReason = "job-password-specified"
				return nil
			}
			if b, err := json.Marshal(opts); err == nil {
				optionsJSON := string(b)
				if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
					return err
				}
				job.Options = optionsJSON
			}
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 3, "none", nil); err != nil {
				return err
			}
			job.State = 3
			job.StateReason = "none"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, doc, len(docs)+1, req)
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

	docStats := map[int64]store.DocumentStats{}
	if len(jobs) > 0 {
		jobIDs := make([]int64, 0, len(jobs))
		for _, job := range jobs {
			jobIDs = append(jobIDs, job.ID)
		}
		if err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			var err error
			docStats, err = s.Store.ListDocumentStatsByJobIDs(ctx, tx, jobIDs)
			return err
		}); err != nil {
			return nil, err
		}
	}

	groups := make(goipp.Groups, 0, len(jobs)+1)
	groups = append(groups, goipp.Group{Tag: goipp.TagOperationGroup, Attrs: buildOperationDefaults()})
	for _, job := range jobs {
		printer := printerMap[job.PrinterID]
		stat := docStats[job.ID]
		docAgg := model.Document{SizeBytes: stat.SizeBytes, MimeType: stat.MimeType}
		attrs := buildJobAttributes(job, printer, r, docAgg, stat.Count, req)
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

	var docAgg model.Document
	if len(docs) == 1 {
		docAgg = docs[0]
	} else {
		var totalSize int64
		for _, d := range docs {
			totalSize += d.SizeBytes
		}
		docAgg = model.Document{SizeBytes: totalSize}
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	addJobAttributes(resp, job, printer, r, docAgg, len(docs), req)
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
		attrs := buildDocumentAttributes(allDocs[i], docNum, len(allDocs), job, printer, r, req)
		groups = append(groups, goipp.Group{Tag: goipp.TagDocumentGroup, Attrs: attrs})
	}
	resp := goipp.NewMessageWithGroups(req.Version, goipp.Code(goipp.StatusOk), req.RequestID, groups)
	return resp, nil
}

func (s *Server) handleGetDocumentAttributes(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := int64(0)
	docNum := 0
	docURI := strings.TrimSpace(attrString(req.Operation, "document-uri"))
	docNumberAttr := attrByName(req.Operation, "document-number")

	if docURI != "" {
		var ok bool
		if jobID, docNum, ok = parseDocumentURIStrict(docURI); !ok {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if docNumberAttr != nil {
			if len(docNumberAttr.Values) != 1 {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
			n, ok := valueInt(docNumberAttr.Values[0].V)
			if !ok || n <= 0 || n != docNum {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
		}
		if id := attrInt(req.Operation, "job-id"); id != 0 && id != jobID {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if uri := strings.TrimSpace(attrString(req.Operation, "job-uri")); uri != "" {
			if parsed := jobIDFromURI(uri); parsed == 0 || parsed != jobID {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
		}
	} else {
		if docNumberAttr == nil {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if len(docNumberAttr.Values) != 1 {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		n, ok := valueInt(docNumberAttr.Values[0].V)
		if !ok {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		docNum = n
		if docNum <= 0 {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		if uri := strings.TrimSpace(attrString(req.Operation, "job-uri")); uri != "" {
			jobID = jobIDFromURI(uri)
			if jobID == 0 {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
			if id := attrInt(req.Operation, "job-id"); id != 0 && id != jobID {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
		} else if strings.TrimSpace(attrString(req.Operation, "printer-uri")) != "" {
			jobID = attrInt(req.Operation, "job-id")
			if jobID == 0 {
				return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
			}
		} else {
			jobID = attrInt(req.Operation, "job-id")
		}
	}

	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}
	if docNum <= 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
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
	for _, attr := range buildDocumentAttributes(doc, int64(docNum), len(allDocs), job, printer, r, req) {
		resp.Document.Add(attr)
	}
	return resp, nil
}

func buildDocumentAttributes(doc model.Document, docNum int64, totalDocs int, job model.Job, printer model.Printer, r *http.Request, req *goipp.Message) goipp.Attributes {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("document-number", goipp.TagInteger, goipp.Integer(docNum)))
	attrs.Add(goipp.MakeAttribute("document-uri", goipp.TagURI, goipp.String(documentURIFor(job.ID, docNum, r))))
	attrs.Add(goipp.MakeAttribute("more-info", goipp.TagURI, goipp.String(documentURIFor(job.ID, docNum, r))))
	if uuid := documentUUIDFor(job, printer, docNum, r); uuid != "" {
		attrs.Add(goipp.MakeAttribute("document-uuid", goipp.TagURI, goipp.String(uuid)))
	}
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
	attrs.Add(goipp.MakeAttribute("document-state-message", goipp.TagText, goipp.String("")))
	attrs.Add(goipp.MakeAttribute("document-message", goipp.TagText, goipp.String("")))
	attrs.Add(goipp.MakeAttribute("document-access-errors", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("errors-count", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("warnings-count", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	attrs.Add(goipp.MakeAttribute("time-at-creation", goipp.TagInteger, goipp.Integer(job.SubmittedAt.Unix())))
	attrs.Add(goipp.MakeAttribute("date-time-at-creation", goipp.TagDateTime, goipp.Time{job.SubmittedAt}))
	if job.ProcessingAt != nil && !job.ProcessingAt.IsZero() {
		attrs.Add(goipp.MakeAttribute("time-at-processing", goipp.TagInteger, goipp.Integer(job.ProcessingAt.Unix())))
		attrs.Add(goipp.MakeAttribute("date-time-at-processing", goipp.TagDateTime, goipp.Time{*job.ProcessingAt}))
	} else {
		attrs.Add(goipp.MakeAttribute("time-at-processing", goipp.TagNoValue, goipp.Void{}))
		attrs.Add(goipp.MakeAttribute("date-time-at-processing", goipp.TagNoValue, goipp.Void{}))
	}
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		attrs.Add(goipp.MakeAttribute("time-at-completed", goipp.TagInteger, goipp.Integer(job.CompletedAt.Unix())))
		attrs.Add(goipp.MakeAttribute("date-time-at-completed", goipp.TagDateTime, goipp.Time{*job.CompletedAt}))
	} else {
		attrs.Add(goipp.MakeAttribute("time-at-completed", goipp.TagNoValue, goipp.Void{}))
		attrs.Add(goipp.MakeAttribute("date-time-at-completed", goipp.TagNoValue, goipp.Void{}))
	}
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
	if compression := strings.TrimSpace(getJobOption(job.Options, "compression-supplied")); compression != "" {
		attrs.Add(goipp.MakeAttribute("compression", goipp.TagKeyword, goipp.String(compression)))
	}
	if doc.MimeType != "" {
		attrs.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(doc.MimeType)))
		if shouldAddDocumentFormatDetected(doc, printer) {
			attrs.Add(goipp.MakeAttribute("document-format-detected", goipp.TagMimeType, goipp.String(doc.MimeType)))
		}
	}
	if doc.FileName != "" {
		attrs.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(doc.FileName)))
	}
	if totalDocs > 0 {
		isLast := docNum == int64(totalDocs)
		attrs.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(isLast)))
	}
	if doc.SizeBytes > 0 {
		attrs.Add(goipp.MakeAttribute("document-size", goipp.TagInteger, goipp.Integer(doc.SizeBytes)))
		kOctets := (doc.SizeBytes + 1023) / 1024
		attrs.Add(goipp.MakeAttribute("k-octets", goipp.TagInteger, goipp.Integer(kOctets)))
		processed := int64(0)
		if job.State >= 5 {
			processed = kOctets
		}
		attrs.Add(goipp.MakeAttribute("k-octets-processed", goipp.TagInteger, goipp.Integer(processed)))
	}
	appendDocumentActualAttributes(&attrs, job, printer)
	ensureDocumentDescriptionDefaults(&attrs, job, printer, doc, totalDocs, stateReason)
	ensureDocumentStatusDefaults(&attrs, job, stateReason)
	return filterAttributesForRequest(attrs, req)
}

func shouldAddDocumentFormatDetected(doc model.Document, printer model.Printer) bool {
	mime := strings.TrimSpace(doc.MimeType)
	if mime == "" || strings.EqualFold(mime, "application/octet-stream") {
		return false
	}
	supplied := strings.TrimSpace(doc.FormatSupplied)
	if supplied != "" && !strings.EqualFold(supplied, "application/octet-stream") {
		return false
	}
	if supplied == "" {
		def := ""
		if strings.TrimSpace(printer.DefaultOptions) != "" {
			opts := parseJobOptions(printer.DefaultOptions)
			def = strings.TrimSpace(opts["document-format"])
		}
		if def != "" && strings.EqualFold(def, mime) {
			return false
		}
	}
	return true
}

func appendDocumentActualAttributes(attrs *goipp.Attributes, job model.Job, printer model.Printer) {
	if attrs == nil {
		return
	}
	if media := getJobOption(job.Options, "media"); media != "" {
		attrs.Add(goipp.MakeAttribute("media-actual", goipp.TagKeyword, goipp.String(media)))
		mediaType := getJobOption(job.Options, "media-type")
		mediaSource := getJobOption(job.Options, "media-source")
		attrs.Add(makeMediaColAttrWithOptions("media-col-actual", media, mediaType, mediaSource, nil))
	}
	if sides := getJobOption(job.Options, "sides"); sides != "" {
		attrs.Add(goipp.MakeAttribute("sides-actual", goipp.TagKeyword, goipp.String(sides)))
	}
	copiesActual := 1
	if copies := getJobOption(job.Options, "copies"); copies != "" {
		if n, err := strconv.Atoi(copies); err == nil {
			copiesActual = n
		}
	}
	attrs.Add(goipp.MakeAttribute("copies-actual", goipp.TagInteger, goipp.Integer(copiesActual)))
	finishingTemplate := strings.TrimSpace(getJobOption(job.Options, "finishing-template"))
	if finishingTemplate != "" && !strings.EqualFold(finishingTemplate, "none") {
		attrs.Add(makeFinishingsColAttrWithTemplate("finishings-col-actual", finishingTemplate))
	} else if finishings := getJobOption(job.Options, "finishings"); finishings != "" {
		if vals := parseFinishingsList(finishings); len(vals) > 0 {
			attrs.Add(makeEnumsAttr("finishings-actual", vals))
			attrs.Add(makeFinishingsColAttr("finishings-col-actual", vals))
		}
	}
	if numberUp := getJobOption(job.Options, "number-up"); numberUp != "" {
		if n, err := strconv.Atoi(numberUp); err == nil {
			attrs.Add(goipp.MakeAttribute("number-up-actual", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if scaling := getJobOption(job.Options, "print-scaling"); scaling != "" {
		attrs.Add(goipp.MakeAttribute("print-scaling-actual", goipp.TagKeyword, goipp.String(scaling)))
	}
	if orientation := getJobOption(job.Options, "orientation-requested"); orientation != "" {
		if n, err := strconv.Atoi(orientation); err == nil {
			attrs.Add(goipp.MakeAttribute("orientation-requested-actual", goipp.TagEnum, goipp.Integer(n)))
		}
	}
	if delivery := getJobOption(job.Options, "page-delivery"); delivery != "" {
		attrs.Add(goipp.MakeAttribute("page-delivery-actual", goipp.TagKeyword, goipp.String(delivery)))
	}
	if res := getJobOption(job.Options, "printer-resolution"); res != "" {
		if parsed, ok := parseResolution(res); ok {
			attrs.Add(goipp.MakeAttribute("printer-resolution-actual", goipp.TagResolution, parsed))
		}
	}
	if bin := getJobOption(job.Options, "output-bin"); bin != "" {
		attrs.Add(goipp.MakeAttribute("output-bin-actual", goipp.TagKeyword, goipp.String(bin)))
	}
	if ranges := getJobOption(job.Options, "page-ranges"); ranges != "" {
		if parsed, ok := parsePageRangesList(ranges); ok {
			attrs.Add(makePageRangesAttr("page-ranges-actual", parsed))
		}
	}
	if colorMode := getJobOption(job.Options, "print-color-mode"); colorMode != "" {
		attrs.Add(goipp.MakeAttribute("print-color-mode-actual", goipp.TagKeyword, goipp.String(colorMode)))
	}
	quality := getJobOption(job.Options, "print-quality")
	if quality != "" {
		if n, err := strconv.Atoi(quality); err == nil {
			attrs.Add(goipp.MakeAttribute("print-quality-actual", goipp.TagEnum, goipp.Integer(n)))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("print-quality-actual", goipp.TagEnum, goipp.Integer(4)))
	}
}

func ensureDocumentDescriptionDefaults(attrs *goipp.Attributes, job model.Job, printer model.Printer, doc model.Document, totalDocs int, stateReason string) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	docCount := int64(0)
	if totalDocs <= 1 {
		docCount = int64(job.Impressions)
	}
	if docCount < 0 {
		docCount = 0
	}
	for name := range documentDescriptionAttrs {
		if existing[name] {
			continue
		}
		switch name {
		case "impressions", "impressions-completed", "pages", "pages-completed", "media-sheets", "media-sheets-completed":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(docCount)))
		case "impressions-completed-current-copy", "pages-completed-current-copy", "sheet-completed-copy-number":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(0)))
		case "k-octets", "k-octets-processed":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(0)))
		case "errors-count", "warnings-count":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(0)))
		case "output-device-assigned":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(printer.Name)))
		case "document-message":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagText, goipp.String(stateReason)))
		case "document-access-errors":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String("none")))
		case "document-metadata", "document-format-details", "detailed-status-messages":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		case "printer-up-time":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(time.Now().Unix())))
		default:
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		}
	}
}

func ensureJobDescriptionDefaults(attrs *goipp.Attributes, job model.Job, printer model.Printer, r *http.Request, doc model.Document, docCount int, jobURI, stateReason string) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	user := strings.TrimSpace(job.UserName)
	if user == "" {
		user = "anonymous"
	}
	host := strings.TrimSpace(job.OriginHost)
	if host == "" {
		host = requestHost(r)
	}
	priorityVal := 50
	if priority := getJobOption(job.Options, "job-priority"); priority != "" {
		if n, err := strconv.Atoi(priority); err == nil {
			priorityVal = n
		}
	}
	holdUntil := getJobOption(job.Options, "job-hold-until")
	if holdUntil == "" {
		holdUntil = "no-hold"
	}
	kOctets := int64(0)
	if doc.SizeBytes > 0 {
		kOctets = (doc.SizeBytes + 1023) / 1024
	}
	processingTime := int64(0)
	if job.ProcessingAt != nil && !job.ProcessingAt.IsZero() {
		end := time.Now()
		if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
			end = *job.CompletedAt
		}
		if end.Before(*job.ProcessingAt) {
			end = *job.ProcessingAt
		}
		processingTime = int64(end.Sub(*job.ProcessingAt).Seconds())
		if processingTime < 0 {
			processingTime = 0
		}
	}
	completedCount := int64(0)
	if job.CompletedAt != nil || job.State >= 5 {
		completedCount = int64(job.Impressions)
	}

	for name := range jobDescriptionAttrs {
		if existing[name] {
			continue
		}
		switch name {
		case "job-id":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.ID)))
		case "job-uri":
			if jobURI == "" {
				jobURI = jobURIFor(job, r)
			}
			attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(jobURI)))
		case "job-name":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(job.Name)))
		case "job-state":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagEnum, goipp.Integer(job.State)))
		case "job-state-reasons":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(stateReason)))
		case "job-state-message":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagText, goipp.String(stateReason)))
		case "job-originating-user-name", "original-requesting-user-name":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(user)))
		case "job-originating-user-uri":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String("urn:sub:"+user)))
		case "job-originating-host-name":
			if host != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(host)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-printer-uri":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(printerURIFor(printer, r))))
		case "job-printer-state-reasons":
			printerReason := printerStateReason(printer)
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(printerReason)))
		case "job-printer-state-message":
			printerReason := printerStateReason(printer)
			attrs.Add(goipp.MakeAttribute(name, goipp.TagText, goipp.String(printerReason)))
		case "job-printer-up-time":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(time.Now().Unix())))
		case "time-at-creation":
			if !job.SubmittedAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.SubmittedAt.Unix())))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "date-time-at-creation":
			if !job.SubmittedAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagDateTime, goipp.Time{job.SubmittedAt}))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "time-at-processing":
			if job.ProcessingAt != nil && !job.ProcessingAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.ProcessingAt.Unix())))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "date-time-at-processing":
			if job.ProcessingAt != nil && !job.ProcessingAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagDateTime, goipp.Time{*job.ProcessingAt}))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "time-at-completed":
			if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.CompletedAt.Unix())))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "date-time-at-completed":
			if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagDateTime, goipp.Time{*job.CompletedAt}))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-impressions":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.Impressions)))
		case "job-impressions-completed":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(completedCount)))
		case "job-priority-actual":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(priorityVal)))
		case "job-hold-until-actual":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(holdUntil)))
		case "job-account-id-actual":
			if account := getJobOption(job.Options, "job-account-id"); account != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(account)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-accounting-user-id-actual":
			if account := getJobOption(job.Options, "job-accounting-user-id"); account != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(account)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-k-octets":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(kOctets)))
		case "job-k-octets-processed":
			processed := int64(0)
			if job.State >= 5 {
				processed = kOctets
			}
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(processed)))
		case "job-pages":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.Impressions)))
		case "job-pages-completed":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(completedCount)))
		case "job-media-sheets":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(job.Impressions)))
		case "job-media-sheets-completed":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(completedCount)))
		case "job-more-info":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(jobMoreInfoURI(job, r))))
		case "job-uuid":
			if uuid := jobUUIDFor(job, printer, r); uuid != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(uuid)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "number-of-documents":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(docCount)))
		case "number-of-intervening-jobs":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(0)))
		case "output-device-assigned":
			if printer.Name != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(printer.Name)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-processing-time":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(processingTime)))
		case "document-format-supplied":
			if supplied := strings.TrimSpace(doc.FormatSupplied); supplied != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagMimeType, goipp.String(supplied)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "document-name-supplied":
			if supplied := strings.TrimSpace(doc.NameSupplied); supplied != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(supplied)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "document-charset-supplied":
			if supplied := strings.TrimSpace(getJobOption(job.Options, "document-charset-supplied")); supplied != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagCharset, goipp.String(supplied)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "document-natural-language-supplied":
			if supplied := strings.TrimSpace(getJobOption(job.Options, "document-natural-language-supplied")); supplied != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagLanguage, goipp.String(supplied)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "compression-supplied":
			if supplied := strings.TrimSpace(getJobOption(job.Options, "compression-supplied")); supplied != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(supplied)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "job-attribute-fidelity":
			if fidelity := strings.TrimSpace(getJobOption(job.Options, "job-attribute-fidelity")); fidelity != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagBoolean, goipp.Boolean(isTruthy(fidelity))))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "copies-actual":
			copiesActual := 1
			if copies := getJobOption(job.Options, "copies"); copies != "" {
				if n, err := strconv.Atoi(copies); err == nil {
					copiesActual = n
				}
			}
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(copiesActual)))
		case "job-sheets-actual":
			sheets := getJobOption(job.Options, "job-sheets")
			if sheets == "" {
				sheets = printer.JobSheetsDefault
			}
			if strings.TrimSpace(sheets) == "" {
				sheets = "none"
			}
			attrs.Add(makeJobSheetsAttr(name, sheets))
		case "job-sheets-col-actual":
			sheets := getJobOption(job.Options, "job-sheets")
			if sheets == "" {
				sheets = printer.JobSheetsDefault
			}
			if strings.TrimSpace(sheets) == "" {
				sheets = "none"
			}
			jobSheetsMedia := getJobOption(job.Options, "job-sheets-col-media")
			jobSheetsMediaType := getJobOption(job.Options, "job-sheets-col-media-type")
			jobSheetsMediaSource := getJobOption(job.Options, "job-sheets-col-media-source")
			attrs.Add(makeJobSheetsColAttr(name, sheets, jobSheetsMedia, jobSheetsMediaType, jobSheetsMediaSource))
		case "media-actual":
			if media := getJobOption(job.Options, "media"); media != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(media)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "media-col-actual":
			if media := getJobOption(job.Options, "media"); media != "" {
				mediaType := getJobOption(job.Options, "media-type")
				mediaSource := getJobOption(job.Options, "media-source")
				attrs.Add(makeMediaColAttrWithOptions(name, media, mediaType, mediaSource, nil))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "number-up-actual":
			if numberUp := getJobOption(job.Options, "number-up"); numberUp != "" {
				if n, err := strconv.Atoi(numberUp); err == nil {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(n)))
				} else {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
				}
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "orientation-requested-actual":
			if orientation := getJobOption(job.Options, "orientation-requested"); orientation != "" {
				if n, err := strconv.Atoi(orientation); err == nil {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagEnum, goipp.Integer(n)))
				} else {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
				}
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "output-bin-actual":
			if bin := getJobOption(job.Options, "output-bin"); bin != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(bin)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "page-delivery-actual":
			if delivery := getJobOption(job.Options, "page-delivery"); delivery != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(delivery)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "page-ranges-actual":
			if ranges := getJobOption(job.Options, "page-ranges"); ranges != "" {
				if parsed, ok := parsePageRangesList(ranges); ok {
					attrs.Add(makePageRangesAttr(name, parsed))
				} else {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
				}
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "print-color-mode-actual":
			if colorMode := getJobOption(job.Options, "print-color-mode"); colorMode != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(colorMode)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "print-quality-actual":
			if quality := getJobOption(job.Options, "print-quality"); quality != "" {
				if n, err := strconv.Atoi(quality); err == nil {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagEnum, goipp.Integer(n)))
				} else {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
				}
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagEnum, goipp.Integer(4)))
			}
		case "print-scaling-actual":
			if scaling := getJobOption(job.Options, "print-scaling"); scaling != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(scaling)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "printer-resolution-actual":
			if res := getJobOption(job.Options, "printer-resolution"); res != "" {
				if parsed, ok := parseResolution(res); ok {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagResolution, parsed))
				} else {
					attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
				}
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "sides-actual":
			if sides := getJobOption(job.Options, "sides"); sides != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(sides)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		default:
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		}
	}
}

func ensurePrinterDescriptionDefaults(attrs *goipp.Attributes, name string, r *http.Request, cfg config.Config, authInfo []string) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	host := hostForRequest(r)
	for attrName := range printerDescriptionAttrs {
		if existing[attrName] {
			continue
		}
		switch attrName {
		case "printer-icons":
			if strings.TrimSpace(name) != "" && strings.TrimSpace(host) != "" {
				uri := fmt.Sprintf("%s://%s/icons/%s.png", webScheme(r), host, name)
				attrs.Add(goipp.MakeAttribute(attrName, goipp.TagURI, goipp.String(uri)))
			}
		case "printer-dns-sd-name":
			if cfg.DNSSDHostName != "" {
				attrs.Add(goipp.MakeAttribute(attrName, goipp.TagName, goipp.String(cfg.DNSSDHostName)))
			}
		case "uri-security-supported":
			attrs.Add(goipp.MakeAttribute(attrName, goipp.TagKeyword, goipp.String(uriSecurityForRequest(r))))
		case "uri-authentication-supported":
			attrs.Add(goipp.MakeAttribute(attrName, goipp.TagKeyword, goipp.String(authSupportedForAuthInfo(authInfo))))
		case "printer-geo-location":
			attrs.Add(goipp.MakeAttribute(attrName, goipp.TagUnknown, goipp.Void{}))
		default:
			continue
		}
	}
}

func ensureJobTemplateDefaults(attrs *goipp.Attributes) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range jobTemplateAttrs {
		if existing[name] {
			continue
		}
		attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
	}
}

func ensureDocumentTemplateDefaults(attrs *goipp.Attributes) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range documentTemplateAttrs {
		if existing[name] {
			continue
		}
		attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
	}
}

func ensurePrinterDefaultsDefaults(attrs *goipp.Attributes) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range printerDefaultsAttrs {
		if existing[name] {
			continue
		}
		attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
	}
}

func ensurePrinterStatusDefaults(attrs *goipp.Attributes) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range printerStatusAttrs {
		if existing[name] {
			continue
		}
		attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
	}
}

func ensurePrinterConfigurationDefaults(attrs *goipp.Attributes) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range printerConfigurationAttrs {
		if existing[name] {
			continue
		}
		attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
	}
}

func ensureSubscriptionDescriptionDefaults(attrs *goipp.Attributes, sub model.Subscription, r *http.Request, s *Server) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	events := strings.Split(strings.TrimSpace(sub.Events), ",")
	clean := make([]string, 0, len(events))
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e != "" {
			clean = append(clean, e)
		}
	}
	if len(clean) == 0 {
		clean = []string{"all"}
	}
	user := strings.TrimSpace(sub.Owner)
	if user == "" {
		user = "anonymous"
	}
	pull := strings.TrimSpace(sub.PullMethod)
	if pull == "" {
		pull = "ippget"
	}
	var printerURI string
	if sub.PrinterID.Valid && s != nil && r != nil {
		_ = s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
			p, err := s.Store.GetPrinterByID(r.Context(), tx, sub.PrinterID.Int64)
			if err != nil {
				return err
			}
			printerURI = printerURIFor(p, r)
			return nil
		})
	}
	for name := range subscriptionDescriptionAttrs {
		if existing[name] {
			continue
		}
		switch name {
		case "notify-job-id":
			if sub.JobID.Valid {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(sub.JobID.Int64)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-lease-expiration-time":
			if sub.LeaseSecs > 0 && !sub.CreatedAt.IsZero() {
				exp := sub.CreatedAt.Add(time.Duration(sub.LeaseSecs) * time.Second).Unix()
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(exp)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-printer-up-time":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(time.Now().Unix())))
		case "notify-printer-uri":
			if printerURI != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(printerURI)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-resource-id", "notify-system-uri":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		case "notify-sequence-number":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		case "notify-subscriber-user-name":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagName, goipp.String(user)))
		case "notify-subscriber-user-uri":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String("urn:sub:"+user)))
		case "notify-subscription-id":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(sub.ID)))
		case "notify-subscription-uuid":
			if uuid := subscriptionUUIDFor(sub, r); uuid != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(uuid)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		default:
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		}
	}
}

func ensureSubscriptionTemplateDefaults(attrs *goipp.Attributes, sub model.Subscription, r *http.Request, s *Server) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	events := strings.Split(strings.TrimSpace(sub.Events), ",")
	clean := make([]string, 0, len(events))
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e != "" {
			clean = append(clean, e)
		}
	}
	if len(clean) == 0 {
		clean = []string{"all"}
	}
	pull := strings.TrimSpace(sub.PullMethod)
	if pull == "" {
		pull = "ippget"
	}
	for name := range subscriptionTemplateAttrs {
		if existing[name] {
			continue
		}
		switch name {
		case "notify-attributes":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		case "notify-attributes-supported":
			attrs.Add(makeKeywordsAttr(name, []string{
				"printer-state-change-time", "notify-lease-expiration-time", "notify-subscriber-user-name",
			}))
		case "notify-charset":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagCharset, goipp.String("utf-8")))
		case "notify-events":
			attrs.Add(makeKeywordsAttr(name, clean))
		case "notify-events-default":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String("job-completed")))
		case "notify-events-supported":
			attrs.Add(makeKeywordsAttr(name, []string{
				"job-completed", "job-config-changed", "job-created", "job-progress", "job-state-changed", "job-stopped",
				"printer-added", "printer-changed", "printer-config-changed", "printer-deleted",
				"printer-finishings-changed", "printer-media-changed", "printer-modified", "printer-restarted",
				"printer-shutdown", "printer-state-changed", "printer-stopped",
				"server-audit", "server-restarted", "server-started", "server-stopped",
			}))
		case "notify-lease-duration":
			if sub.LeaseSecs > 0 {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(sub.LeaseSecs)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-lease-duration-default":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(0)))
		case "notify-lease-duration-supported":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
		case "notify-max-events-supported":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(100)))
		case "notify-natural-language":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagLanguage, goipp.String("en")))
		case "notify-recipient-uri":
			if strings.TrimSpace(sub.RecipientURI) != "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagURI, goipp.String(sub.RecipientURI)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-pull-method":
			if strings.TrimSpace(sub.RecipientURI) == "" {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(pull)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-pull-method-supported":
			attrs.Add(makeKeywordsAttr(name, []string{"ippget"}))
		case "notify-schemes-supported":
			attrs.Add(makeKeywordsAttr(name, []string{"ippget"}))
		case "notify-time-interval":
			if sub.TimeInterval > 0 {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(sub.TimeInterval)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		case "notify-user-data":
			if len(sub.UserData) > 0 {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagString, goipp.Binary(sub.UserData)))
			} else {
				attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
			}
		default:
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		}
	}
}

func ensureDocumentStatusDefaults(attrs *goipp.Attributes, job model.Job, stateReason string) {
	if attrs == nil {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}
	for name := range documentStatusAttrs {
		if existing[name] {
			continue
		}
		switch name {
		case "document-state":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagEnum, goipp.Integer(documentStateForJob(job.State))))
		case "document-state-reasons":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(stateReason)))
		case "document-state-message":
			attrs.Add(goipp.MakeAttribute(name, goipp.TagText, goipp.String(stateReason)))
		default:
			attrs.Add(goipp.MakeAttribute(name, goipp.TagNoValue, goipp.Void{}))
		}
	}
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
	addJobAttributes(resp, job, printer, r, model.Document{}, 0, req)
	return resp, nil
}

func (s *Server) handleCancelJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	printerURI := attrString(req.Operation, "printer-uri")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 && printerURI != "" {
		var resolvedID int64
		err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			printer, err := s.printerFromURI(ctx, tx, printerURI)
			if err != nil {
				return err
			}
			id, err := s.findCurrentJobIDForPrinter(ctx, tx, printer.ID)
			if err != nil {
				return err
			}
			resolvedID = id
			return nil
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
			}
			return nil, err
		}
		jobID = resolvedID
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	authType := s.authTypeForRequest(r, goipp.Op(req.Code).String())
	purgeJob, purgeJobPresent := attrBoolPresent(req.Operation, "purge-job")
	if !purgeJobPresent {
		purgeJob = attrBool(req.Operation, "purge-jobs")
	}
	deleteFiles := purgeJob
	paths := []string{}
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		job, err := s.Store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if printerURI != "" {
			printer, err := s.printerFromURI(ctx, tx, printerURI)
			if err != nil {
				return err
			}
			if job.PrinterID != printer.ID {
				return sql.ErrNoRows
			}
		}
		if !s.canManageJob(ctx, r, req, authType, job, true) {
			return errNotAuthorized
		}
		if purgeJob {
			return s.purgeSingleJobWithPaths(ctx, tx, jobID, deleteFiles, &paths)
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

	if deleteFiles {
		for _, p := range paths {
			_ = os.Remove(p)
		}
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
	purge, purgePresent := attrBoolPresent(req.Operation, "purge-jobs")
	if !purgePresent {
		purge = attrBool(req.Operation, "purge-job")
	}
	reason := "job-canceled-by-user"
	if purge {
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

	paths := []string{}
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
			if purge {
				if err := s.purgeSingleJobWithPaths(ctx, tx, job.ID, true, &paths); err != nil {
					return err
				}
			} else {
				completed := time.Now().UTC()
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, 7, reason, &completed); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if purge {
		for _, p := range paths {
			_ = os.Remove(p)
		}
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
	if err := s.enforceAuthInfo(r, req, goipp.OpValidateJob); err != nil {
		return nil, err
	}
	stripReadOnlyJobAttributes(req)
	_ = sanitizeJobName(req)
	warn, err := validateRequestOptions(req, printer)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errConflicting) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorConflicting, req.RequestID), nil
		}
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	applyWarning(resp, warn)
	return resp, nil
}

func (s *Server) handleValidateDocument(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
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
	if err := s.enforceAuthInfo(r, req, goipp.OpValidateDocument); err != nil {
		return nil, err
	}
	stripReadOnlyJobAttributes(req)

	docFormat := attrString(req.Operation, "document-format")
	if docFormat == "" {
		docFormat = "application/octet-stream"
	}
	docName := strings.TrimSpace(attrString(req.Operation, "document-name"))
	supportedFormats := s.documentFormatsForRequest(ctx, r, req)
	if strings.EqualFold(docFormat, "application/octet-stream") {
		if detected := detectDocumentFormat(docName); detected != "" && isDocumentFormatSupportedInList(supportedFormats, detected) {
			docFormat = detected
		}
	}
	if !isDocumentFormatSupportedInList(supportedFormats, docFormat) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorDocumentFormatNotSupported, req.RequestID)
		addOperationDefaults(resp)
		resp.Unsupported.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(docFormat)))
		return resp, nil
	}

	warn, err := validateRequestOptions(req, printer)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errConflicting) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorConflicting, req.RequestID), nil
		}
		if errors.Is(err, errPPDConstraint) || errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	if _, err := compressionFromRequest(req); err != nil {
		if errors.Is(err, errBadRequest) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
		}
		if errors.Is(err, errUnsupported) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorAttributesOrValues, req.RequestID), nil
		}
		return nil, err
	}

	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	addOperationDefaults(resp)
	applyWarning(resp, warn)
	return resp, nil
}

func (s *Server) documentFormatsForRequest(ctx context.Context, r *http.Request, req *goipp.Message) []string {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return supportedDocumentFormats()
	}
	if dest.IsClass {
		// CUPS class destinations advertise and validate raw-only document formats.
		return []string{"application/octet-stream", "application/vnd.cups-raw"}
	}
	ppd, _ := loadPPDForPrinter(dest.Printer)
	return supportedDocumentFormatsForPrinter(dest.Printer, ppd)
}

func (s *Server) handleCupsAuthenticateJob(ctx context.Context, r *http.Request, req *goipp.Message) (*goipp.Message, error) {
	jobID := attrInt(req.Operation, "job-id")
	if jobID == 0 {
		jobID = jobIDFromURI(attrString(req.Operation, "job-uri"))
	}
	if jobID == 0 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorBadRequest, req.RequestID), nil
	}

	authInfo := authInfoFromRequest(req)
	if len(authInfo) > 0 && r != nil && r.TLS == nil && !isLocalRequest(r) {
		return nil, &ippHTTPError{status: http.StatusUpgradeRequired}
	}

	var job model.Job
	err := s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		job, err = s.Store.GetJob(ctx, tx, jobID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
		}
		return nil, err
	}

	authType := s.authTypeForRequest(r, goipp.OpCupsAuthenticateJob.String())
	if !s.canManageJob(ctx, r, req, authType, job, false) {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
	}
	if job.State != 4 {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotPossible, req.RequestID), nil
	}

	if len(authInfo) == 0 {
		if _, ok := s.authenticate(r, ""); !ok {
			required := authInfoRequiredForRequest(s, r)
			if len(required) == 1 && strings.EqualFold(required[0], "negotiate") {
				return nil, &ippHTTPError{
					status:   http.StatusUnauthorized,
					authType: authType,
				}
			}
			return goipp.NewResponse(req.Version, goipp.StatusErrorNotAuthorized, req.RequestID), nil
		}
	}

	options := mergeJobOptions(job.Options, map[string]string{
		"job-hold-until": "no-hold",
	})
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &options); err != nil {
			return err
		}
		return s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-incoming", nil)
	})
	if err != nil {
		return nil, err
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
	addJobAttributes(resp, job, printer, r, model.Document{}, 0, req)
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
	purge, purgePresent := attrBoolPresent(req.Operation, "purge-jobs")
	if !purgePresent {
		purge = true
	}
	return s.cancelJobsForDestination(ctx, r, req, "job-canceled-by-user", purge)
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

func (s *Server) cancelJobsForDestination(ctx context.Context, r *http.Request, req *goipp.Message, reason string, purge bool) (*goipp.Message, error) {
	dest, err := s.resolveDestination(ctx, r, req)
	if err != nil {
		return goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID), nil
	}
	paths := []string{}
	err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		if dest.IsClass {
			members, err := s.Store.ListClassMembers(ctx, tx, dest.Class.ID)
			if err != nil {
				return err
			}
			for _, p := range members {
				if err := s.cancelJobsForPrinter(ctx, tx, p.ID, reason, purge, &paths); err != nil {
					return err
				}
			}
			return nil
		}
		return s.cancelJobsForPrinter(ctx, tx, dest.Printer.ID, reason, purge, &paths)
	})
	if err != nil {
		return nil, err
	}
	if purge {
		for _, p := range paths {
			_ = os.Remove(p)
		}
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
				if err := s.purgeSingleJobWithPaths(ctx, tx, jobID, deleteFiles, &paths); err != nil {
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

func (s *Server) purgeSingleJobWithPaths(ctx context.Context, tx *sql.Tx, jobID int64, deleteFiles bool, paths *[]string) error {
	if deleteFiles {
		docs, err := s.Store.ListDocumentsByJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if strings.TrimSpace(d.Path) != "" && paths != nil {
				*paths = append(*paths, d.Path)
			}
		}
	}
	if err := s.Store.DeleteDocumentsByJob(ctx, tx, jobID); err != nil {
		return err
	}
	return s.Store.DeleteJob(ctx, tx, jobID)
}

func (s *Server) cancelJobsForPrinter(ctx context.Context, tx *sql.Tx, printerID int64, reason string, purge bool, paths *[]string) error {
	if !purge {
		return s.Store.CancelJobsByPrinter(ctx, tx, printerID, reason)
	}
	jobIDs, err := s.Store.ListJobIDsByPrinter(ctx, tx, printerID)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if err := s.purgeSingleJobWithPaths(ctx, tx, jobID, true, paths); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) findCurrentJobIDForPrinter(ctx context.Context, tx *sql.Tx, printerID int64) (int64, error) {
	jobs, err := s.Store.ListJobsByPrinter(ctx, tx, printerID, 50)
	if err != nil {
		return 0, err
	}
	var stoppedID int64
	for _, job := range jobs {
		if job.State > 0 && job.State <= 5 {
			return job.ID, nil
		}
		if stoppedID == 0 && job.State == 6 {
			stoppedID = job.ID
		}
	}
	return stoppedID, nil
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
	resp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
}

func buildOperationDefaults() goipp.Attributes {
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	return attrs
}

func addPrinterAttributes(ctx context.Context, resp *goipp.Message, printer model.Printer, r *http.Request, req *goipp.Message, st *store.Store, cfg config.Config, authInfo []string) {
	for _, attr := range buildPrinterAttributes(ctx, printer, r, req, st, cfg, authInfo) {
		resp.Printer.Add(attr)
	}
}

func addClassAttributes(ctx context.Context, resp *goipp.Message, class model.Class, members []model.Printer, r *http.Request, req *goipp.Message, st *store.Store, cfg config.Config, authInfo []string) {
	for _, attr := range classAttributesWithMembers(ctx, class, members, r, req, st, cfg, authInfo) {
		resp.Printer.Add(attr)
	}
}

func buildPrinterAttributes(ctx context.Context, printer model.Printer, r *http.Request, req *goipp.Message, st *store.Store, cfg config.Config, authInfo []string) goipp.Attributes {
	uri := printerURIFor(printer, r)
	attrs := goipp.Attributes{}
	ppd, _ := loadPPDForPrinter(printer)
	documentFormats := supportedDocumentFormatsForPrinter(printer, ppd)
	shareServer := serverIsSharingPrinters(cfg, st, r)
	share := printer.Shared
	jobSheetsDefault := strings.TrimSpace(printer.JobSheetsDefault)
	if jobSheetsDefault == "" {
		jobSheetsDefault = "none"
	}
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)
	kSupported := kOctetsSupported(cfg)
	finishingsSupported := caps.finishingsSupported
	if len(finishingsSupported) == 0 {
		finishingsSupported = []int{3}
	}
	finishingsDefault := 3
	finishingsTemplates := caps.finishingTemplates
	if len(finishingsTemplates) == 0 {
		finishingsTemplates = finishingsTemplatesFromEnums(finishingsSupported)
	}
	templateDefault := "none"
	printQualityDefault := 4
	qualityValue := strings.TrimSpace(defaultOpts["print-quality"])
	if qualityValue == "" && ppd != nil {
		qualityValue = strings.TrimSpace(ppd.Defaults["OutputMode"])
		if qualityValue == "" {
			qualityValue = strings.TrimSpace(ppd.Defaults["cupsPrintQuality"])
		}
	}
	if qualityValue != "" {
		if n, ok := parsePrintQualityValue(qualityValue); ok {
			printQualityDefault = n
		}
	}
	if len(caps.printQualitySupported) > 0 && !intInList(printQualityDefault, caps.printQualitySupported) {
		printQualityDefault = caps.printQualitySupported[0]
	}
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
	stateChange := printer.UpdatedAt
	if stateChange.IsZero() {
		stateChange = time.Now()
	}
	attrs.Add(goipp.MakeAttribute("printer-state-change-time", goipp.TagInteger, goipp.Integer(stateChange.Unix())))
	attrs.Add(goipp.MakeAttribute("printer-state-change-date-time", goipp.TagDateTime, goipp.Time{stateChange}))
	configChange := printer.UpdatedAt
	if configChange.IsZero() {
		configChange = stateChange
	}
	attrs.Add(goipp.MakeAttribute("printer-config-change-time", goipp.TagInteger, goipp.Integer(configChange.Unix())))
	attrs.Add(goipp.MakeAttribute("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{configChange}))
	attrs.Add(goipp.MakeAttribute("printer-current-time", goipp.TagDateTime, goipp.Time{time.Now()}))
	attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(printer.Location)))
	attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(printer.Info)))
	attrs.Add(goipp.MakeAttribute("ppd-name", goipp.TagName, goipp.String(ppdName)))
	attrs.Add(goipp.MakeAttribute("printer-make-and-model", goipp.TagText, goipp.String(modelName)))
	attrs.Add(goipp.MakeAttribute("printer-more-info", goipp.TagURI, goipp.String(printerMoreInfoURI(printer.Name, false, r))))
	attrs.Add(goipp.MakeAttribute("printer-is-temporary", goipp.TagBoolean, goipp.Boolean(printer.IsTemporary)))
	attrs.Add(goipp.MakeAttribute("printer-type", goipp.TagInteger, goipp.Integer(computePrinterType(printer, caps, ppd, false, authInfo))))
	colorSupported := false
	for _, mode := range caps.colorModes {
		if strings.EqualFold(mode, "color") {
			colorSupported = true
			break
		}
	}
	attrs.Add(goipp.MakeAttribute("color-supported", goipp.TagBoolean, goipp.Boolean(colorSupported)))
	attrs.Add(makeMimeTypesAttr("document-format-supported", documentFormats))
	attrs.Add(goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String("application/octet-stream")))
	attrs.Add(goipp.MakeAttribute("document-format-preferred", goipp.TagMimeType, goipp.String(preferredDocumentFormat(documentFormats))))
	attrs.Add(makeKeywordsAttr("document-creation-attributes-supported", []string{
		"compression", "document-charset", "document-format", "document-name", "document-natural-language",
	}))
	attrs.Add(goipp.MakeAttribute("document-format-details-supported", goipp.TagNoValue, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("document-format-varying-attributes", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-password-supported", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-privacy-attributes", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-privacy-scope", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-charset-default", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("document-charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("document-natural-language-default", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("document-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("charset-configured", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("natural-language-configured", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("pdl-override-supported", goipp.TagKeyword, goipp.String("attempted")))
	attrs.Add(makeKeywordsAttr("ipp-versions-supported", []string{"1.0", "1.1", "2.0", "2.1"}))
	attrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	attrs.Add(goipp.MakeAttribute("queued-job-count", goipp.TagInteger, goipp.Integer(queuedJobCountForPrinters(ctx, st, []int64{printer.ID}))))
	if uuid := printerUUIDFor(printer.Name, r); uuid != "" {
		attrs.Add(goipp.MakeAttribute("printer-uuid", goipp.TagURI, goipp.String(uuid)))
	}
	attrs.Add(goipp.MakeAttribute("cups-version", goipp.TagText, goipp.String("2.4.16")))
	attrs.Add(goipp.MakeAttribute("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
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
	stringsLangs := stringsLanguagesSupported()
	attrs.Add(makeLanguagesAttr("printer-strings-languages-supported", stringsLangs))
	attrs.Add(goipp.MakeAttribute("printer-strings-uri", goipp.TagURI, goipp.String(printerStringsURI(r, req))))
	deviceID := "MFG:CUPS-Golang;MDL:Generic;"
	if ppd != nil && strings.TrimSpace(ppd.DeviceID) != "" {
		deviceID = strings.TrimSpace(ppd.DeviceID)
	}
	attrs.Add(goipp.MakeAttribute("printer-device-id", goipp.TagText, goipp.String(deviceID)))
	attrs.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(printer.URI)))
	attrs.Add(goipp.MakeAttribute("destination-uri", goipp.TagURI, goipp.String(uri)))
	attrs.Add(goipp.MakeAttribute("multiple-destination-uris-supported", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(goipp.MakeAttribute("uri-security-supported", goipp.TagKeyword, goipp.String(uriSecurityForRequest(r))))
	attrs.Add(goipp.MakeAttribute("uri-authentication-supported", goipp.TagKeyword, goipp.String(authSupportedForAuthInfo(authInfo))))
	attrs.Add(makeURISchemesAttr("reference-uri-schemes-supported", referenceURISchemesSupported()))
	attrs.Add(makeEnumsAttr("operations-supported", supportedOperations()))
	attrs.Add(goipp.MakeAttribute("preferred-attributes-supported", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(goipp.MakeAttribute("printer-is-shared", goipp.TagBoolean, goipp.Boolean(share)))
	attrs.Add(goipp.MakeAttribute("printer-state-message", goipp.TagText, goipp.String("")))
	attrs.Add(makeKeywordsAttr("multiple-document-handling-supported", []string{
		"separate-documents-uncollated-copies", "separate-documents-collated-copies",
	}))
	attrs.Add(goipp.MakeAttribute("multiple-document-handling-default", goipp.TagKeyword, goipp.String("separate-documents-uncollated-copies")))
	attrs.Add(goipp.MakeAttribute("multiple-document-jobs-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeKeywordsAttr("compression-supported", []string{"none", "gzip"}))
	attrs.Add(goipp.MakeAttribute("ippget-event-life", goipp.TagInteger, goipp.Integer(15)))
	attrs.Add(goipp.MakeAttribute("job-ids-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("job-priority-supported", goipp.TagInteger, goipp.Integer(100)))
	jobPriorityDefault := 50
	if v := strings.TrimSpace(defaultOpts["job-priority"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			jobPriorityDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("job-priority-default", goipp.TagInteger, goipp.Integer(jobPriorityDefault)))
	accountSupported := ppd != nil && ppd.JobAccountID
	accountingUserSupported := ppd != nil && ppd.JobAccountingUser
	attrs.Add(goipp.MakeAttribute("job-account-id-supported", goipp.TagBoolean, goipp.Boolean(accountSupported)))
	attrs.Add(goipp.MakeAttribute("job-accounting-user-id-supported", goipp.TagBoolean, goipp.Boolean(accountingUserSupported)))
	if accountSupported {
		if v := strings.TrimSpace(defaultOpts["job-account-id"]); v != "" {
			attrs.Add(goipp.MakeAttribute("job-account-id-default", goipp.TagName, goipp.String(v)))
		} else {
			attrs.Add(goipp.MakeAttribute("job-account-id-default", goipp.TagNoValue, goipp.Void{}))
		}
	}
	if accountingUserSupported {
		if v := strings.TrimSpace(defaultOpts["job-accounting-user-id"]); v != "" {
			attrs.Add(goipp.MakeAttribute("job-accounting-user-id-default", goipp.TagName, goipp.String(v)))
		} else {
			attrs.Add(goipp.MakeAttribute("job-accounting-user-id-default", goipp.TagNoValue, goipp.Void{}))
		}
	}
	if ppd != nil && strings.TrimSpace(ppd.ChargeInfoURI) != "" {
		attrs.Add(goipp.MakeAttribute("printer-charge-info-uri", goipp.TagURI, goipp.String(ppd.ChargeInfoURI)))
	}
	if ppd != nil && strings.TrimSpace(ppd.JobPassword) != "" {
		attrs.Add(goipp.MakeAttribute("job-password-encryption-supported", goipp.TagKeyword, goipp.String("none")))
		attrs.Add(goipp.MakeAttribute("job-password-supported", goipp.TagInteger, goipp.Integer(len(ppd.JobPassword))))
	}
	attrs.Add(goipp.MakeAttribute("job-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
	attrs.Add(goipp.MakeAttribute("pdf-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
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
	notifyEventsDefault := strings.TrimSpace(defaultOpts["notify-events"])
	if notifyEventsDefault == "" {
		notifyEventsDefault = "job-completed"
	}
	attrs.Add(goipp.MakeAttribute("notify-events-default", goipp.TagKeyword, goipp.String(notifyEventsDefault)))
	attrs.Add(goipp.MakeAttribute("notify-pull-method-supported", goipp.TagKeyword, goipp.String("ippget")))
	if schemes := notifySchemesSupported(cfg); len(schemes) > 0 {
		attrs.Add(makeKeywordsAttr("notify-schemes-supported", schemes))
	}
	maxEvents := cfg.MaxEvents
	if maxEvents < 0 {
		maxEvents = 0
	}
	leaseDefault := cfg.DefaultLeaseDuration
	if v := strings.TrimSpace(defaultOpts["notify-lease-duration"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			leaseDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-default", goipp.TagInteger, goipp.Integer(leaseDefault)))
	attrs.Add(goipp.MakeAttribute("notify-max-events-supported", goipp.TagInteger, goipp.Integer(maxEvents)))
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: leaseDurationUpper(cfg)}))
	attrs.Add(makeKeywordsAttr("printer-get-attributes-supported", []string{"document-format"}))
	timeout := cfg.MultipleOperationTimeout
	if timeout <= 0 {
		timeout = 900
	}
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out", goipp.TagInteger, goipp.Integer(timeout)))
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out-action", goipp.TagKeyword, goipp.String("process-job")))
	maxCopies := caps.maxCopies
	if maxCopies <= 0 {
		maxCopies = 9999
	}
	attrs.Add(goipp.MakeAttribute("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: maxCopies}))
	attrs.Add(goipp.MakeAttribute("copies-default", goipp.TagInteger, goipp.Integer(1)))
	attrs.Add(goipp.MakeAttribute("job-quota-period", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("job-k-limit", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("job-page-limit", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(makeNamesAttr("job-sheets-supported", jobSheetsSupported()))
	attrs.Add(makeJobSheetsDefaultAttr("job-sheets-default", jobSheetsDefault))
	attrs.Add(makeKeywordsAttr("job-sheets-col-supported", []string{"job-sheets", "media", "media-col"}))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-default", jobSheetsDefault, "", "", ""))
	attrs.Add(goipp.MakeAttribute("print-as-raster-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("print-as-raster-default", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(makeKeywordsAttr("job-hold-until-supported", []string{
		"no-hold", "indefinite", "day-time", "evening", "night", "second-shift", "third-shift", "weekend",
	}))
	jobHoldDefault := strings.TrimSpace(defaultOpts["job-hold-until"])
	if jobHoldDefault == "" {
		jobHoldDefault = "no-hold"
	}
	attrs.Add(goipp.MakeAttribute("job-hold-until-default", goipp.TagKeyword, goipp.String(jobHoldDefault)))
	attrs.Add(makeKeywordsAttr("page-delivery-supported", []string{"reverse-order", "same-order"}))
	attrs.Add(goipp.MakeAttribute("page-delivery-default", goipp.TagKeyword, goipp.String("same-order")))
	attrs.Add(makeKeywordsAttr("print-scaling-supported", []string{"auto", "auto-fit", "fill", "fit", "none"}))
	attrs.Add(goipp.MakeAttribute("print-scaling-default", goipp.TagKeyword, goipp.String("auto")))
	attrs.Add(makeEnumsAttr("print-quality-supported", caps.printQualitySupported))
	attrs.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, goipp.Integer(printQualityDefault)))
	attrs.Add(goipp.MakeAttribute("page-ranges-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeEnumsAttr("finishings-supported", finishingsSupported))
	attrs.Add(goipp.MakeAttribute("finishings-default", goipp.TagEnum, goipp.Integer(finishingsDefault)))
	finishingsReady := uniqueInts(finishingsSupported)
	if len(finishingsReady) == 0 {
		finishingsReady = []int{3}
	}
	attrs.Add(makeEnumsAttr("finishings-ready", finishingsReady))
	attrs.Add(makeKeywordsAttr("finishing-template-supported", finishingsTemplates))
	attrs.Add(makeKeywordsAttr("finishings-col-supported", []string{"finishing-template"}))
	attrs.Add(makeFinishingsColAttrWithTemplate("finishings-col-default", templateDefault))
	attrs.Add(makeFinishingsColDatabaseFromTemplates("finishings-col-ready", finishingsTemplates))
	attrs.Add(makeIntsAttr("number-up-supported", []int{1, 2, 4, 6, 9, 16}))
	numUpDefault := 1
	if v := strings.TrimSpace(defaultOpts["number-up"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numUpDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("number-up-default", goipp.TagInteger, goipp.Integer(numUpDefault)))
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
	attrs.Add(makeKeywordsAttr("job-settable-attributes-supported", jobSettableAttributesSupported()))
	attrs.Add(makeKeywordsAttr("job-creation-attributes-supported", []string{
		"copies", "finishings", "finishings-col", "ipp-attribute-fidelity", "job-hold-until",
		"job-name", "job-priority", "job-sheets", "media", "media-col",
		"multiple-document-handling", "number-up", "number-up-layout", "orientation-requested",
		"output-bin", "page-delivery", "page-ranges", "print-color-mode", "print-quality",
		"print-scaling", "printer-resolution", "sides",
	}))
	if ppd != nil && len(ppd.Mandatory) > 0 {
		mandatory := make([]string, 0, len(ppd.Mandatory))
		seen := map[string]bool{}
		for _, m := range ppd.Mandatory {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			key := strings.ToLower(m)
			if seen[key] {
				continue
			}
			seen[key] = true
			mandatory = append(mandatory, m)
		}
		if len(mandatory) > 0 {
			attrs.Add(makeKeywordsAttr("printer-mandatory-job-attributes", mandatory))
		}
	}
	attrs.Add(makeKeywordsAttr("printer-settable-attributes-supported", printerSettableAttributesForDestination(false)))
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
	attrs.Add(makeKeywordsAttr("printer-commands", printerCommandsForPPD(ppd)))
	allowed, denied := loadUserAccessLists(ctx, st, "printer."+strconv.FormatInt(printer.ID, 10))
	if len(allowed) > 0 {
		attrs.Add(makeNamesAttr("requesting-user-name-allowed", allowed))
	} else if len(denied) > 0 {
		attrs.Add(makeNamesAttr("requesting-user-name-denied", denied))
	}
	if len(authInfo) > 0 {
		attrs.Add(makeKeywordsAttr("auth-info-required", authInfo))
	}
	attrs.Add(goipp.MakeAttribute("job-cancel-after-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 2147483647}))
	if v := strings.TrimSpace(defaultOpts["job-cancel-after"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagInteger, goipp.Integer(n)))
		} else {
			attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagNoValue, goipp.Void{}))
		}
	} else if cfg.MaxJobTime > 0 {
		attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagInteger, goipp.Integer(cfg.MaxJobTime)))
	} else {
		attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagNoValue, goipp.Void{}))
	}
	attrs.Add(goipp.MakeAttribute("jpeg-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
	attrs.Add(goipp.MakeAttribute("jpeg-x-dimension-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 65535}))
	attrs.Add(goipp.MakeAttribute("jpeg-y-dimension-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 65535}))
	attrs.Add(makeKeywordsAttr("media-col-supported", []string{
		"media-bottom-margin", "media-left-margin", "media-right-margin", "media-size",
		"media-source", "media-top-margin", "media-type",
	}))
	supplyState, supplyDetails := loadPrinterSupplies(ctx, st, printer)
	supplyVals, supplyDesc, markerMsg := buildSupplyAttributes(supplyState, supplyDetails)
	attrs.Add(makeTextsAttr("marker-message", []string{markerMsg}))
	attrs.Add(makeStringsAttr("printer-supply", supplyVals))
	attrs.Add(makeTextsAttr("printer-supply-description", supplyDesc))
	if presetAttr, ok := makeJobPresetsSupportedAttr(ppd); ok {
		attrs.Add(presetAttr)
	}
	ppm := 1
	if ppd != nil && ppd.Throughput > 0 {
		ppm = ppd.Throughput
	}
	attrs.Add(goipp.MakeAttribute("pages-per-minute", goipp.TagInteger, goipp.Integer(ppm)))
	if (ppd != nil && ppd.ColorDevice) || (ppd == nil && colorSupported) {
		attrs.Add(goipp.MakeAttribute("pages-per-minute-color", goipp.TagInteger, goipp.Integer(ppm)))
	}
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
	outputModes := []string{"monochrome"}
	if isColorModeSupported(colorModes) {
		outputModes = []string{"monochrome", "color"}
	}
	if colorDefault == "color" {
		attrs.Add(goipp.MakeAttribute("output-mode-default", goipp.TagKeyword, goipp.String("color")))
	} else {
		attrs.Add(goipp.MakeAttribute("output-mode-default", goipp.TagKeyword, goipp.String("monochrome")))
	}
	attrs.Add(makeKeywordsAttr("output-mode-supported", outputModes))
	attrs.Add(makeKeywordsAttr("pwg-raster-document-type-supported", rasterTypes))
	attrs.Add(makeResolutionsAttr("pwg-raster-document-resolution-supported", resolutions))
	if len(sidesSupported) > 1 {
		attrs.Add(goipp.MakeAttribute("pwg-raster-document-sheet-back", goipp.TagKeyword, goipp.String("normal")))
	}
	attrs.Add(makeResolutionsAttr("printer-resolution-supported", resolutions))
	attrs.Add(goipp.MakeAttribute("printer-resolution-default", goipp.TagResolution, resDefault))
	attrs.Add(makeKeywordsAttr("output-bin-supported", outputBins))
	attrs.Add(goipp.MakeAttribute("output-bin-default", goipp.TagKeyword, goipp.String(outputBinDefault)))
	if len(caps.finishingTemplates) > 0 {
		attrs.Add(makeFinishingsColDatabaseFromTemplates("finishings-col-database", caps.finishingTemplates))
	} else {
		attrs.Add(makeFinishingsColDatabaseAttr("finishings-col-database", finishingsSupported))
	}
	attrs.Add(makeKeywordsAttr("urf-supported", urfSupported(resolutions, colorModes, sidesSupported, finishingsSupported, caps.printQualitySupported)))
	ensureJobTemplateDefaults(&attrs)
	ensureDocumentTemplateDefaults(&attrs)
	ensurePrinterDefaultsDefaults(&attrs)
	ensurePrinterStatusDefaults(&attrs)
	ensurePrinterConfigurationDefaults(&attrs)
	ensurePrinterDescriptionDefaults(&attrs, printer.Name, r, cfg, authInfo)
	return filterAttributesForRequest(attrs, req)
}

func buildClassAttributes(ctx context.Context, class model.Class, r *http.Request, req *goipp.Message, st *store.Store, cfg config.Config, authInfo []string) goipp.Attributes {
	uri := classURIFor(class, r)
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(class.Name)))
	attrs.Add(goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String(uri)))
	attrs.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(class.State)))
	attrs.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(class.Accepting)))
	stateReason := printerStateReason(model.Printer{Accepting: class.Accepting})
	attrs.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(stateReason)))
	attrs.Add(goipp.MakeAttribute("printer-state-message", goipp.TagText, goipp.String("")))
	stateChange := class.UpdatedAt
	if stateChange.IsZero() {
		stateChange = time.Now()
	}
	attrs.Add(goipp.MakeAttribute("printer-state-change-time", goipp.TagInteger, goipp.Integer(stateChange.Unix())))
	attrs.Add(goipp.MakeAttribute("printer-state-change-date-time", goipp.TagDateTime, goipp.Time{stateChange}))
	configChange := class.UpdatedAt
	if configChange.IsZero() {
		configChange = stateChange
	}
	attrs.Add(goipp.MakeAttribute("printer-config-change-time", goipp.TagInteger, goipp.Integer(configChange.Unix())))
	attrs.Add(goipp.MakeAttribute("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{configChange}))
	attrs.Add(goipp.MakeAttribute("printer-current-time", goipp.TagDateTime, goipp.Time{time.Now()}))
	attrs.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(class.Location)))
	attrs.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(class.Info)))
	attrs.Add(goipp.MakeAttribute("printer-more-info", goipp.TagURI, goipp.String(printerMoreInfoURI(class.Name, true, r))))
	attrs.Add(goipp.MakeAttribute("printer-is-temporary", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(goipp.MakeAttribute("document-charset-default", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("document-charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("document-natural-language-default", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("document-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("charset-configured", goipp.TagCharset, goipp.String("utf-8")))
	attrs.Add(makeCharsetsAttr("charset-supported", []string{"us-ascii", "utf-8"}))
	attrs.Add(goipp.MakeAttribute("natural-language-configured", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(goipp.MakeAttribute("pdl-override-supported", goipp.TagKeyword, goipp.String("attempted")))
	stringsLangs := stringsLanguagesSupported()
	attrs.Add(makeLanguagesAttr("printer-strings-languages-supported", stringsLangs))
	attrs.Add(goipp.MakeAttribute("printer-strings-uri", goipp.TagURI, goipp.String(printerStringsURI(r, req))))
	attrs.Add(makeMimeTypesAttr("document-format-supported", []string{"application/octet-stream"}))
	attrs.Add(goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String("application/octet-stream")))
	defaultOpts := parseJobOptions(class.DefaultOptions)
	attrs.Add(makeKeywordsAttr("document-creation-attributes-supported", []string{
		"compression", "document-charset", "document-format", "document-name", "document-natural-language",
	}))
	attrs.Add(goipp.MakeAttribute("document-format-details-supported", goipp.TagNoValue, goipp.Void{}))
	attrs.Add(goipp.MakeAttribute("document-format-varying-attributes", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-password-supported", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-privacy-attributes", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("document-privacy-scope", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(makeKeywordsAttr("ipp-versions-supported", []string{"1.0", "1.1", "2.0", "2.1"}))
	shareServer := serverIsSharingPrinters(cfg, st, r)
	kSupported := kOctetsSupported(cfg)
	attrs.Add(goipp.MakeAttribute("printer-make-and-model", goipp.TagText, goipp.String("Local Printer Class")))
	attrs.Add(goipp.MakeAttribute("printer-type", goipp.TagInteger, goipp.Integer(computeClassType(class, shareServer, authInfo))))
	attrs.Add(goipp.MakeAttribute("printer-is-shared", goipp.TagBoolean, goipp.Boolean(shareServer)))
	attrs.Add(goipp.MakeAttribute("server-is-sharing-printers", goipp.TagBoolean, goipp.Boolean(shareServer)))
	attrs.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String("file:///dev/null")))
	attrs.Add(goipp.MakeAttribute("printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	if uuid := printerUUIDFor(class.Name, r); uuid != "" {
		attrs.Add(goipp.MakeAttribute("printer-uuid", goipp.TagURI, goipp.String(uuid)))
	}
	attrs.Add(goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(class.ID)))
	attrs.Add(goipp.MakeAttribute("cups-version", goipp.TagText, goipp.String("2.4.16")))
	attrs.Add(goipp.MakeAttribute("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	attrs.Add(makeKeywordsAttr("compression-supported", []string{"none", "gzip"}))
	attrs.Add(goipp.MakeAttribute("ippget-event-life", goipp.TagInteger, goipp.Integer(15)))
	attrs.Add(goipp.MakeAttribute("job-ids-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("job-priority-supported", goipp.TagInteger, goipp.Integer(100)))
	jobPriorityDefault := 50
	if v := strings.TrimSpace(defaultOpts["job-priority"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			jobPriorityDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("job-priority-default", goipp.TagInteger, goipp.Integer(jobPriorityDefault)))
	attrs.Add(goipp.MakeAttribute("job-cancel-after-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: ippIntMax}))
	if v := strings.TrimSpace(defaultOpts["job-cancel-after"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagInteger, goipp.Integer(n)))
		} else {
			attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagNoValue, goipp.Void{}))
		}
	} else if cfg.MaxJobTime > 0 {
		attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagInteger, goipp.Integer(cfg.MaxJobTime)))
	} else {
		attrs.Add(goipp.MakeAttribute("job-cancel-after-default", goipp.TagNoValue, goipp.Void{}))
	}
	attrs.Add(goipp.MakeAttribute("job-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
	attrs.Add(goipp.MakeAttribute("pdf-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
	attrs.Add(makeKeywordsAttr("pdf-versions-supported", []string{
		"adobe-1.2", "adobe-1.3", "adobe-1.4", "adobe-1.5", "adobe-1.6", "adobe-1.7",
		"iso-19005-1_2005", "iso-32000-1_2008", "pwg-5102.3",
	}))
	attrs.Add(goipp.MakeAttribute("jpeg-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: kSupported}))
	attrs.Add(goipp.MakeAttribute("jpeg-x-dimension-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 65535}))
	attrs.Add(goipp.MakeAttribute("jpeg-y-dimension-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 65535}))
	attrs.Add(makeKeywordsAttr("media-col-supported", []string{
		"media-bottom-margin", "media-left-margin", "media-right-margin", "media-size",
		"media-source", "media-top-margin", "media-type",
	}))
	attrs.Add(makeKeywordsAttr("multiple-document-handling-supported", []string{
		"separate-documents-uncollated-copies", "separate-documents-collated-copies",
	}))
	attrs.Add(goipp.MakeAttribute("multiple-document-handling-default", goipp.TagKeyword, goipp.String("separate-documents-uncollated-copies")))
	attrs.Add(goipp.MakeAttribute("multiple-document-jobs-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(makeIntsAttr("number-up-supported", []int{1, 2, 4, 6, 9, 16}))
	numUpDefault := 1
	if v := strings.TrimSpace(defaultOpts["number-up"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numUpDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("number-up-default", goipp.TagInteger, goipp.Integer(numUpDefault)))
	attrs.Add(makeKeywordsAttr("number-up-layout-supported", []string{
		"btlr", "btrl", "lrbt", "lrtb", "rlbt", "rltb", "tblr", "tbrl",
	}))
	attrs.Add(makeEnumsAttr("orientation-requested-supported", []int{3, 4, 5, 6}))
	attrs.Add(makeKeywordsAttr("page-delivery-supported", []string{"reverse-order", "same-order"}))
	attrs.Add(goipp.MakeAttribute("page-delivery-default", goipp.TagKeyword, goipp.String("same-order")))
	attrs.Add(makeKeywordsAttr("print-scaling-supported", []string{"auto", "auto-fit", "fill", "fit", "none"}))
	attrs.Add(goipp.MakeAttribute("print-scaling-default", goipp.TagKeyword, goipp.String("auto")))
	attrs.Add(goipp.MakeAttribute("print-as-raster-supported", goipp.TagBoolean, goipp.Boolean(true)))
	attrs.Add(goipp.MakeAttribute("print-as-raster-default", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(goipp.MakeAttribute("page-ranges-supported", goipp.TagBoolean, goipp.Boolean(true)))
	if v := strings.TrimSpace(defaultOpts["print-color-mode"]); v != "" {
		attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String(v)))
	} else if v := strings.TrimSpace(defaultOpts["output-mode"]); v != "" {
		if strings.EqualFold(v, "color") {
			attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String("color")))
		} else if strings.EqualFold(v, "monochrome") {
			attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String("monochrome")))
		}
	}
	printQualityDefault := 4
	if v := strings.TrimSpace(defaultOpts["print-quality"]); v != "" {
		if n, ok := parsePrintQualityValue(v); ok {
			printQualityDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, goipp.Integer(printQualityDefault)))
	attrs.Add(makeKeywordsAttr("printer-get-attributes-supported", []string{"document-format"}))
	attrs.Add(makeKeywordsAttr("printer-settable-attributes-supported", printerSettableAttributesForDestination(true)))
	attrs.Add(makeKeywordsAttr("job-settable-attributes-supported", jobSettableAttributesSupported()))
	attrs.Add(makeKeywordsAttr("job-creation-attributes-supported", []string{
		"copies", "finishings", "finishings-col", "ipp-attribute-fidelity", "job-hold-until",
		"job-name", "job-priority", "job-sheets", "media", "media-col",
		"multiple-document-handling", "number-up", "number-up-layout", "orientation-requested",
		"output-bin", "page-delivery", "page-ranges", "print-color-mode", "print-quality",
		"print-scaling", "printer-resolution", "sides",
	}))
	attrs.Add(makeKeywordsAttr("which-jobs-supported", []string{
		"completed", "not-completed", "aborted", "all", "canceled", "pending", "pending-held",
		"processing", "processing-stopped",
	}))
	attrs.Add(makeEnumsAttr("operations-supported", supportedOperations()))
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
	notifyEventsDefault := strings.TrimSpace(defaultOpts["notify-events"])
	if notifyEventsDefault == "" {
		notifyEventsDefault = "job-completed"
	}
	attrs.Add(goipp.MakeAttribute("notify-events-default", goipp.TagKeyword, goipp.String(notifyEventsDefault)))
	attrs.Add(goipp.MakeAttribute("notify-pull-method-supported", goipp.TagKeyword, goipp.String("ippget")))
	if schemes := notifySchemesSupported(cfg); len(schemes) > 0 {
		attrs.Add(makeKeywordsAttr("notify-schemes-supported", schemes))
	}
	maxEvents := cfg.MaxEvents
	if maxEvents < 0 {
		maxEvents = 0
	}
	leaseDefault := cfg.DefaultLeaseDuration
	if v := strings.TrimSpace(defaultOpts["notify-lease-duration"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			leaseDefault = n
		}
	}
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-default", goipp.TagInteger, goipp.Integer(leaseDefault)))
	attrs.Add(goipp.MakeAttribute("notify-max-events-supported", goipp.TagInteger, goipp.Integer(maxEvents)))
	attrs.Add(goipp.MakeAttribute("notify-lease-duration-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: leaseDurationUpper(cfg)}))
	attrs.Add(goipp.MakeAttribute("uri-security-supported", goipp.TagKeyword, goipp.String(uriSecurityForRequest(r))))
	attrs.Add(goipp.MakeAttribute("uri-authentication-supported", goipp.TagKeyword, goipp.String(authSupportedForAuthInfo(authInfo))))
	attrs.Add(makeURISchemesAttr("reference-uri-schemes-supported", referenceURISchemesSupported()))
	attrs.Add(goipp.MakeAttribute("preferred-attributes-supported", goipp.TagBoolean, goipp.Boolean(false)))
	attrs.Add(makeKeywordsAttr("job-hold-until-supported", []string{
		"no-hold", "indefinite", "day-time", "evening", "night", "second-shift", "third-shift", "weekend",
	}))
	jobHoldDefault := strings.TrimSpace(defaultOpts["job-hold-until"])
	if jobHoldDefault == "" {
		jobHoldDefault = "no-hold"
	}
	attrs.Add(goipp.MakeAttribute("job-hold-until-default", goipp.TagKeyword, goipp.String(jobHoldDefault)))
	if v := strings.TrimSpace(defaultOpts["orientation-requested"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagEnum, goipp.Integer(n)))
		} else {
			attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagNoValue, goipp.Void{}))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("orientation-requested-default", goipp.TagNoValue, goipp.Void{}))
	}
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
	attrs.Add(makeKeywordsAttr("printer-commands", printerCommandsForPPD(nil)))
	jobSheetsDefault := strings.TrimSpace(class.JobSheetsDefault)
	if jobSheetsDefault == "" {
		jobSheetsDefault = "none"
	}
	allowed, denied := loadUserAccessLists(ctx, st, "class."+strconv.FormatInt(class.ID, 10))
	if len(allowed) > 0 {
		attrs.Add(makeNamesAttr("requesting-user-name-allowed", allowed))
	} else if len(denied) > 0 {
		attrs.Add(makeNamesAttr("requesting-user-name-denied", denied))
	}
	if len(authInfo) > 0 {
		attrs.Add(makeKeywordsAttr("auth-info-required", authInfo))
	}
	attrs.Add(goipp.MakeAttribute("job-quota-period", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("job-k-limit", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(goipp.MakeAttribute("job-page-limit", goipp.TagInteger, goipp.Integer(0)))
	attrs.Add(makeNamesAttr("job-sheets-supported", jobSheetsSupported()))
	attrs.Add(makeJobSheetsDefaultAttr("job-sheets-default", jobSheetsDefault))
	attrs.Add(makeKeywordsAttr("job-sheets-col-supported", []string{"job-sheets", "media", "media-col"}))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-default", jobSheetsDefault, "", "", ""))
	timeout := cfg.MultipleOperationTimeout
	if timeout <= 0 {
		timeout = 900
	}
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out", goipp.TagInteger, goipp.Integer(timeout)))
	attrs.Add(goipp.MakeAttribute("multiple-operation-time-out-action", goipp.TagKeyword, goipp.String("process-job")))
	ensureJobTemplateDefaults(&attrs)
	ensureDocumentTemplateDefaults(&attrs)
	ensurePrinterDefaultsDefaults(&attrs)
	ensurePrinterStatusDefaults(&attrs)
	ensurePrinterConfigurationDefaults(&attrs)
	ensurePrinterDescriptionDefaults(&attrs, class.Name, r, cfg, authInfo)
	return attrs
}

func classAttributesWithMembers(ctx context.Context, class model.Class, members []model.Printer, r *http.Request, req *goipp.Message, st *store.Store, cfg config.Config, authInfo []string) goipp.Attributes {
	attrs := buildClassAttributes(ctx, class, r, req, st, cfg, authInfo)
	memberIDs := make([]int64, 0, len(members))
	for _, p := range members {
		memberIDs = append(memberIDs, p.ID)
	}
	attrs.Add(goipp.MakeAttribute("queued-job-count", goipp.TagInteger, goipp.Integer(queuedJobCountForPrinters(ctx, st, memberIDs))))
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
	addClassCapabilities(&attrs, members)
	return filterAttributesForRequest(attrs, req)
}

func addClassCapabilities(attrs *goipp.Attributes, members []model.Printer) {
	if attrs == nil || len(members) == 0 {
		return
	}
	existing := map[string]bool{}
	for _, attr := range *attrs {
		existing[attr.Name] = true
	}

	colorModes := []string{"monochrome"}
	colorDefault := "monochrome"
	printQualities := []int{}

	for _, p := range members {
		ppd, _ := loadPPDForPrinter(p)
		caps := computePrinterCaps(ppd, parseJobOptions(p.DefaultOptions))
		for _, q := range caps.printQualitySupported {
			if !intInList(q, printQualities) {
				printQualities = append(printQualities, q)
			}
		}
		if ppd != nil && ppd.ColorDevice {
			colorModes = []string{"monochrome", "color"}
			colorDefault = "color"
		}
	}

	if len(printQualities) == 0 {
		printQualities = []int{4}
	}
	sort.Ints(printQualities)

	if !existing["print-color-mode-supported"] {
		attrs.Add(makeKeywordsAttr("print-color-mode-supported", colorModes))
	}
	if !existing["print-color-mode-default"] {
		attrs.Add(goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String(colorDefault)))
	}
	if !existing["print-quality-supported"] {
		attrs.Add(makeEnumsAttr("print-quality-supported", printQualities))
	}
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
	ensureSubscriptionDescriptionDefaults(&attrs, sub, r, s)
	ensureSubscriptionTemplateDefaults(&attrs, sub, r, s)
	return filterAttributesForRequest(attrs, req)
}

func addJobAttributes(resp *goipp.Message, job model.Job, printer model.Printer, r *http.Request, doc model.Document, docCount int, req *goipp.Message) {
	for _, attr := range buildJobAttributes(job, printer, r, doc, docCount, req) {
		resp.Job.Add(attr)
	}
}

func ensureJobUUID(ctx context.Context, tx *sql.Tx, s *Server, job *model.Job, printer model.Printer, r *http.Request) error {
	if job == nil || job.ID == 0 || s == nil || s.Store == nil {
		return nil
	}
	opts := parseJobOptions(job.Options)
	if strings.TrimSpace(opts["job-uuid"]) != "" {
		return nil
	}
	host, port := serverHostPortForUUID(r)
	uuid := assembleUUID(host, port, printer.Name, job.ID, randomUint16(), randomUint16())
	opts["job-uuid"] = uuid
	b, _ := json.Marshal(opts)
	options := string(b)
	if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &options); err != nil {
		return err
	}
	job.Options = options
	return nil
}

func jobUUIDFor(job model.Job, printer model.Printer, r *http.Request) string {
	if val := strings.TrimSpace(getJobOption(job.Options, "job-uuid")); val != "" {
		return val
	}
	if job.ID == 0 {
		return ""
	}
	host, port := serverHostPortForUUID(r)
	return assembleUUID(host, port, printer.Name, job.ID, 0, 0)
}

func documentUUIDFor(job model.Job, printer model.Printer, docNum int64, r *http.Request) string {
	if job.ID == 0 || docNum <= 0 {
		return ""
	}
	host, port := serverHostPortForUUID(r)
	name := printer.Name
	if strings.TrimSpace(name) == "" {
		name = "document"
	}
	name = fmt.Sprintf("%s-doc-%d", name, docNum)
	return assembleUUID(host, port, name, job.ID, 0, 0)
}

func subscriptionUUIDFor(sub model.Subscription, r *http.Request) string {
	if sub.ID == 0 {
		return ""
	}
	host, port := serverHostPortForUUID(r)
	return assembleUUID(host, port, "subscription", sub.ID, 0, 0)
}

func buildJobAttributes(job model.Job, printer model.Printer, r *http.Request, doc model.Document, docCount int, req *goipp.Message) goipp.Attributes {
	jobURI := jobURIFor(job, r)
	attrs := goipp.Attributes{}
	attrs.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	attrs.Add(goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(jobURI)))
	if uuid := jobUUIDFor(job, printer, r); uuid != "" {
		attrs.Add(goipp.MakeAttribute("job-uuid", goipp.TagURI, goipp.String(uuid)))
	}
	attrs.Add(goipp.MakeAttribute("job-more-info", goipp.TagURI, goipp.String(jobMoreInfoURI(job, r))))
	attrs.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(job.Name)))
	attrs.Add(goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(job.State)))
	stateReason := strings.TrimSpace(job.StateReason)
	if stateReason == "" {
		stateReason = "none"
	}
	attrs.Add(goipp.MakeAttribute("job-state-reasons", goipp.TagKeyword, goipp.String(stateReason)))
	attrs.Add(goipp.MakeAttribute("job-state-message", goipp.TagText, goipp.String("")))
	attrs.Add(goipp.MakeAttribute("job-document-access-errors", goipp.TagKeyword, goipp.String("none")))
	attrs.Add(goipp.MakeAttribute("job-originating-user-name", goipp.TagName, goipp.String(job.UserName)))
	origUser := strings.TrimSpace(job.UserName)
	if origUser == "" {
		origUser = "anonymous"
	}
	attrs.Add(goipp.MakeAttribute("original-requesting-user-name", goipp.TagName, goipp.String(origUser)))
	attrs.Add(goipp.MakeAttribute("job-originating-user-uri", goipp.TagURI, goipp.String(jobOriginatingUserURI(job.UserName))))
	host := strings.TrimSpace(job.OriginHost)
	if host == "" {
		host = requestHost(r)
	}
	if host != "" {
		attrs.Add(goipp.MakeAttribute("job-originating-host-name", goipp.TagName, goipp.String(host)))
	}
	attrs.Add(goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String(printerURIForJob(printer, r))))
	attrs.Add(goipp.MakeAttribute("job-printer-name", goipp.TagName, goipp.String(printer.Name)))
	attrs.Add(goipp.MakeAttribute("output-device-assigned", goipp.TagName, goipp.String(printer.Name)))
	printerReason := printerStateReason(printer)
	attrs.Add(goipp.MakeAttribute("job-printer-state-reasons", goipp.TagKeyword, goipp.String(printerReason)))
	attrs.Add(goipp.MakeAttribute("job-printer-state-message", goipp.TagText, goipp.String("")))
	attrs.Add(goipp.MakeAttribute("job-printer-up-time", goipp.TagInteger, goipp.Integer(time.Now().Unix())))
	attrs.Add(goipp.MakeAttribute("time-at-creation", goipp.TagInteger, goipp.Integer(job.SubmittedAt.Unix())))
	attrs.Add(goipp.MakeAttribute("date-time-at-creation", goipp.TagDateTime, goipp.Time{job.SubmittedAt}))
	if job.State >= 5 && job.ProcessingAt != nil {
		attrs.Add(goipp.MakeAttribute("date-time-at-processing", goipp.TagDateTime, goipp.Time{*job.ProcessingAt}))
		attrs.Add(goipp.MakeAttribute("time-at-processing", goipp.TagInteger, goipp.Integer(job.ProcessingAt.Unix())))
	} else {
		attrs.Add(goipp.MakeAttribute("date-time-at-processing", goipp.TagNoValue, goipp.Void{}))
		attrs.Add(goipp.MakeAttribute("time-at-processing", goipp.TagNoValue, goipp.Void{}))
	}
	attrs.Add(goipp.MakeAttribute("job-impressions", goipp.TagInteger, goipp.Integer(job.Impressions)))
	attrs.Add(goipp.MakeAttribute("job-impressions-completed", goipp.TagInteger, goipp.Integer(job.Impressions)))
	if job.CompletedAt != nil {
		attrs.Add(goipp.MakeAttribute("date-time-at-completed", goipp.TagDateTime, goipp.Time{*job.CompletedAt}))
		attrs.Add(goipp.MakeAttribute("time-at-completed", goipp.TagInteger, goipp.Integer(job.CompletedAt.Unix())))
	} else {
		attrs.Add(goipp.MakeAttribute("date-time-at-completed", goipp.TagNoValue, goipp.Void{}))
		attrs.Add(goipp.MakeAttribute("time-at-completed", goipp.TagNoValue, goipp.Void{}))
	}
	kOctets := int64(0)
	if doc.SizeBytes > 0 {
		kOctets = (doc.SizeBytes + 1023) / 1024
	}
	attrs.Add(goipp.MakeAttribute("job-k-octets", goipp.TagInteger, goipp.Integer(kOctets)))
	if job.CompletedAt != nil || job.State >= 7 {
		attrs.Add(goipp.MakeAttribute("job-k-octets-completed", goipp.TagInteger, goipp.Integer(kOctets)))
	} else {
		attrs.Add(goipp.MakeAttribute("job-k-octets-completed", goipp.TagInteger, goipp.Integer(0)))
	}
	processed := int64(0)
	if job.State >= 5 {
		processed = kOctets
	}
	attrs.Add(goipp.MakeAttribute("job-k-octets-processed", goipp.TagInteger, goipp.Integer(processed)))
	if job.CompletedAt != nil || job.State >= 5 {
		attrs.Add(goipp.MakeAttribute("job-pages-completed", goipp.TagInteger, goipp.Integer(job.Impressions)))
		attrs.Add(goipp.MakeAttribute("job-media-sheets-completed", goipp.TagInteger, goipp.Integer(job.Impressions)))
	} else {
		attrs.Add(goipp.MakeAttribute("job-pages-completed", goipp.TagInteger, goipp.Integer(0)))
		attrs.Add(goipp.MakeAttribute("job-media-sheets-completed", goipp.TagInteger, goipp.Integer(0)))
	}
	attrs.Add(goipp.MakeAttribute("job-pages", goipp.TagInteger, goipp.Integer(job.Impressions)))
	attrs.Add(goipp.MakeAttribute("job-media-sheets", goipp.TagInteger, goipp.Integer(job.Impressions)))
	priorityVal := 50
	if priority := getJobOption(job.Options, "job-priority"); priority != "" {
		if n, err := strconv.Atoi(priority); err == nil {
			priorityVal = n
		}
	}
	attrs.Add(goipp.MakeAttribute("job-priority", goipp.TagInteger, goipp.Integer(priorityVal)))
	attrs.Add(goipp.MakeAttribute("job-priority-actual", goipp.TagInteger, goipp.Integer(priorityVal)))
	if account := getJobOption(job.Options, "job-account-id"); account != "" {
		attrs.Add(goipp.MakeAttribute("job-account-id", goipp.TagName, goipp.String(account)))
		attrs.Add(goipp.MakeAttribute("job-account-id-actual", goipp.TagName, goipp.String(account)))
	}
	if accountUser := getJobOption(job.Options, "job-accounting-user-id"); accountUser != "" {
		attrs.Add(goipp.MakeAttribute("job-accounting-user-id", goipp.TagName, goipp.String(accountUser)))
		attrs.Add(goipp.MakeAttribute("job-accounting-user-id-actual", goipp.TagName, goipp.String(accountUser)))
	}
	if fidelity := strings.TrimSpace(getJobOption(job.Options, "job-attribute-fidelity")); fidelity != "" {
		attrs.Add(goipp.MakeAttribute("job-attribute-fidelity", goipp.TagBoolean, goipp.Boolean(isTruthy(fidelity))))
	}
	holdUntil := getJobOption(job.Options, "job-hold-until")
	if holdUntil == "" {
		holdUntil = "no-hold"
	}
	attrs.Add(goipp.MakeAttribute("job-hold-until", goipp.TagKeyword, goipp.String(holdUntil)))
	attrs.Add(goipp.MakeAttribute("job-hold-until-actual", goipp.TagKeyword, goipp.String(holdUntil)))
	if supplied := strings.TrimSpace(getJobOption(job.Options, "document-charset-supplied")); supplied != "" {
		attrs.Add(goipp.MakeAttribute("document-charset-supplied", goipp.TagCharset, goipp.String(supplied)))
	}
	if supplied := strings.TrimSpace(getJobOption(job.Options, "document-natural-language-supplied")); supplied != "" {
		attrs.Add(goipp.MakeAttribute("document-natural-language-supplied", goipp.TagLanguage, goipp.String(supplied)))
	}
	if supplied := strings.TrimSpace(getJobOption(job.Options, "compression-supplied")); supplied != "" {
		attrs.Add(goipp.MakeAttribute("compression-supplied", goipp.TagKeyword, goipp.String(supplied)))
	}
	if media := getJobOption(job.Options, "media"); media != "" {
		attrs.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(media)))
		attrs.Add(goipp.MakeAttribute("media-actual", goipp.TagKeyword, goipp.String(media)))
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
		attrs.Add(goipp.MakeAttribute("sides-actual", goipp.TagKeyword, goipp.String(sides)))
	}
	if copies := getJobOption(job.Options, "copies"); copies != "" {
		if n, err := strconv.Atoi(copies); err == nil {
			attrs.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	copiesActual := 1
	if copies := getJobOption(job.Options, "copies"); copies != "" {
		if n, err := strconv.Atoi(copies); err == nil {
			copiesActual = n
		}
	}
	attrs.Add(goipp.MakeAttribute("copies-actual", goipp.TagInteger, goipp.Integer(copiesActual)))
	sheets := getJobOption(job.Options, "job-sheets")
	if sheets == "" {
		sheets = printer.JobSheetsDefault
	}
	if strings.TrimSpace(sheets) == "" {
		sheets = "none"
	}
	attrs.Add(makeJobSheetsAttr("job-sheets", sheets))
	attrs.Add(makeJobSheetsAttr("job-sheets-actual", sheets))
	jobSheetsMedia := getJobOption(job.Options, "job-sheets-col-media")
	jobSheetsMediaType := getJobOption(job.Options, "job-sheets-col-media-type")
	jobSheetsMediaSource := getJobOption(job.Options, "job-sheets-col-media-source")
	attrs.Add(makeJobSheetsColAttr("job-sheets-col", sheets, jobSheetsMedia, jobSheetsMediaType, jobSheetsMediaSource))
	attrs.Add(makeJobSheetsColAttr("job-sheets-col-actual", sheets, jobSheetsMedia, jobSheetsMediaType, jobSheetsMediaSource))
	if cancelAfter := getJobOption(job.Options, "job-cancel-after"); cancelAfter != "" {
		if n, err := strconv.Atoi(cancelAfter); err == nil {
			attrs.Add(goipp.MakeAttribute("job-cancel-after", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if quality := getJobOption(job.Options, "print-quality"); quality != "" {
		if n, err := strconv.Atoi(quality); err == nil {
			attrs.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(n)))
			attrs.Add(goipp.MakeAttribute("print-quality-actual", goipp.TagEnum, goipp.Integer(n)))
		}
	} else {
		attrs.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))
		attrs.Add(goipp.MakeAttribute("print-quality-actual", goipp.TagEnum, goipp.Integer(4)))
	}
	finishingTemplate := strings.TrimSpace(getJobOption(job.Options, "finishing-template"))
	if finishingTemplate != "" && !strings.EqualFold(finishingTemplate, "none") {
		attrs.Add(makeFinishingsColAttrWithTemplate("finishings-col", finishingTemplate))
		attrs.Add(makeFinishingsColAttrWithTemplate("finishings-col-actual", finishingTemplate))
	} else if finishings := getJobOption(job.Options, "finishings"); finishings != "" {
		if vals := parseFinishingsList(finishings); len(vals) > 0 {
			attrs.Add(makeEnumsAttr("finishings", vals))
			attrs.Add(makeEnumsAttr("finishings-actual", vals))
			attrs.Add(makeFinishingsColAttr("finishings-col", vals))
			attrs.Add(makeFinishingsColAttr("finishings-col-actual", vals))
		}
	}
	if numberUp := getJobOption(job.Options, "number-up"); numberUp != "" {
		if n, err := strconv.Atoi(numberUp); err == nil {
			attrs.Add(goipp.MakeAttribute("number-up", goipp.TagInteger, goipp.Integer(n)))
			attrs.Add(goipp.MakeAttribute("number-up-actual", goipp.TagInteger, goipp.Integer(n)))
		}
	}
	if layout := getJobOption(job.Options, "number-up-layout"); layout != "" {
		attrs.Add(goipp.MakeAttribute("number-up-layout", goipp.TagKeyword, goipp.String(layout)))
	}
	if scaling := getJobOption(job.Options, "print-scaling"); scaling != "" {
		attrs.Add(goipp.MakeAttribute("print-scaling", goipp.TagKeyword, goipp.String(scaling)))
		attrs.Add(goipp.MakeAttribute("print-scaling-actual", goipp.TagKeyword, goipp.String(scaling)))
	}
	if orientation := getJobOption(job.Options, "orientation-requested"); orientation != "" {
		if n, err := strconv.Atoi(orientation); err == nil {
			attrs.Add(goipp.MakeAttribute("orientation-requested", goipp.TagEnum, goipp.Integer(n)))
			attrs.Add(goipp.MakeAttribute("orientation-requested-actual", goipp.TagEnum, goipp.Integer(n)))
		}
	}
	if delivery := getJobOption(job.Options, "page-delivery"); delivery != "" {
		attrs.Add(goipp.MakeAttribute("page-delivery", goipp.TagKeyword, goipp.String(delivery)))
		attrs.Add(goipp.MakeAttribute("page-delivery-actual", goipp.TagKeyword, goipp.String(delivery)))
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
	if res := getJobOption(job.Options, "printer-resolution"); res != "" {
		if parsed, ok := parseResolution(res); ok {
			attrs.Add(goipp.MakeAttribute("printer-resolution", goipp.TagResolution, parsed))
			attrs.Add(goipp.MakeAttribute("printer-resolution-actual", goipp.TagResolution, parsed))
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
		attrs.Add(goipp.MakeAttribute("output-bin-actual", goipp.TagKeyword, goipp.String(bin)))
	}
	if ranges := getJobOption(job.Options, "page-ranges"); ranges != "" {
		if parsed, ok := parsePageRangesList(ranges); ok {
			attrs.Add(makePageRangesAttr("page-ranges", parsed))
			attrs.Add(makePageRangesAttr("page-ranges-actual", parsed))
		}
	}
	if colorMode := getJobOption(job.Options, "print-color-mode"); colorMode != "" {
		attrs.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String(colorMode)))
		attrs.Add(goipp.MakeAttribute("print-color-mode-actual", goipp.TagKeyword, goipp.String(colorMode)))
		if outMode := outputModeForColorMode(colorMode); outMode != "" {
			attrs.Add(goipp.MakeAttribute("output-mode", goipp.TagKeyword, goipp.String(outMode)))
		}
	} else if outMode := strings.TrimSpace(getJobOption(job.Options, "output-mode")); outMode != "" {
		attrs.Add(goipp.MakeAttribute("output-mode", goipp.TagKeyword, goipp.String(outMode)))
	}
	if raster := getJobOption(job.Options, "print-as-raster"); raster != "" {
		attrs.Add(goipp.MakeAttribute("print-as-raster", goipp.TagBoolean, goipp.Boolean(isTruthy(raster))))
	}
	attrs.Add(goipp.MakeAttribute("number-of-documents", goipp.TagInteger, goipp.Integer(docCount)))
	attrs.Add(goipp.MakeAttribute("number-of-intervening-jobs", goipp.TagInteger, goipp.Integer(0)))
	processingTime := int64(0)
	if job.ProcessingAt != nil && !job.ProcessingAt.IsZero() {
		end := time.Now()
		if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
			end = *job.CompletedAt
		}
		if end.Before(*job.ProcessingAt) {
			end = *job.ProcessingAt
		}
		processingTime = int64(end.Sub(*job.ProcessingAt).Seconds())
	}
	attrs.Add(goipp.MakeAttribute("job-processing-time", goipp.TagInteger, goipp.Integer(processingTime)))
	if job.State > 5 {
		attrs.Add(goipp.MakeAttribute("job-preserved", goipp.TagBoolean, goipp.Boolean(docCount > 0)))
	}
	if docCount <= 1 {
		if mime := strings.TrimSpace(doc.MimeType); mime != "" {
			attrs.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(mime)))
		}
		if shouldAddDocumentFormatDetected(doc, printer) {
			attrs.Add(goipp.MakeAttribute("document-format-detected", goipp.TagMimeType, goipp.String(doc.MimeType)))
		}
		if supplied := strings.TrimSpace(doc.FormatSupplied); supplied != "" {
			attrs.Add(goipp.MakeAttribute("document-format-supplied", goipp.TagMimeType, goipp.String(supplied)))
		}
		if supplied := strings.TrimSpace(doc.NameSupplied); supplied != "" {
			attrs.Add(goipp.MakeAttribute("document-name-supplied", goipp.TagName, goipp.String(supplied)))
		}
	}
	ensureJobDescriptionDefaults(&attrs, job, printer, r, doc, docCount, jobURI, stateReason)
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

func attrByName(attrs goipp.Attributes, name string) *goipp.Attribute {
	for i := range attrs {
		if attrs[i].Name == name {
			return &attrs[i]
		}
	}
	return nil
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

func parseSubscriptionRequest(req *goipp.Message) (events string, lease int64, leasePresent bool, recipient string, pullMethod string, interval int64, userData []byte, err error) {
	if req == nil {
		return "", 0, false, "", "ippget", 0, nil, nil
	}
	events = strings.Join(attrStrings(req.Subscription, "notify-events"), ",")
	if val, ok := attrIntPresent(req.Subscription, "notify-lease-duration"); ok {
		lease = int64(val)
		leasePresent = true
	}
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
		if _, parseErr := url.Parse(recipient); parseErr != nil {
			return events, lease, leasePresent, recipient, pullMethod, interval, userData, errUnsupported
		}
		pullMethod = ""
		return events, lease, leasePresent, recipient, pullMethod, interval, userData, nil
	}
	if pullMethod == "" {
		pullMethod = "ippget"
	}
	if !strings.EqualFold(pullMethod, "ippget") {
		return events, lease, leasePresent, recipient, pullMethod, interval, userData, errUnsupported
	}
	return events, lease, leasePresent, recipient, pullMethod, interval, userData, nil
}

func subscriptionDefaultsForPrinter(printer model.Printer, cfg config.Config) (string, int64) {
	opts := parseJobOptions(printer.DefaultOptions)
	events := strings.TrimSpace(opts["notify-events"])
	if events == "" {
		events = "job-completed"
	}
	lease := int64(cfg.DefaultLeaseDuration)
	if v := strings.TrimSpace(opts["notify-lease-duration"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			lease = int64(n)
		}
	}
	return events, lease
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

func requestUserForFilter(r *http.Request, req *goipp.Message) string {
	if r != nil {
		if user, _, ok := r.BasicAuth(); ok && strings.TrimSpace(user) != "" {
			return strings.TrimSpace(user)
		}
	}
	if req != nil {
		if val, ok := attrValue(req.Operation, "requested-user-name"); ok {
			return strings.TrimSpace(val)
		}
		if val, ok := attrValue(req.Operation, "requesting-user-name"); ok {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func authInfoRequiredForRequest(s *Server, r *http.Request) []string {
	if s == nil || r == nil {
		return nil
	}
	authType := s.authTypeForRequest(r, goipp.OpPrintJob.String())
	if authType == "" {
		authType = s.authTypeForRequest(r, goipp.OpCreateJob.String())
	}
	return authInfoRequiredFromAuthType(authType)
}

func authInfoRequiredFromAuthType(authType string) []string {
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "", "none":
		return nil
	case "negotiate", "kerberos":
		return []string{"negotiate"}
	case "domain":
		return []string{"domain", "username", "password"}
	case "basic", "digest":
		return []string{"username", "password"}
	default:
		return []string{"username", "password"}
	}
}

func authInfoFromRequest(req *goipp.Message) []string {
	if req == nil {
		return nil
	}
	if vals := attrStrings(req.Job, "auth-info"); len(vals) > 0 {
		return vals
	}
	if vals := attrStrings(req.Operation, "auth-info"); len(vals) > 0 {
		return vals
	}
	if vals := attrStrings(req.Printer, "auth-info"); len(vals) > 0 {
		return vals
	}
	if vals := attrStrings(req.Subscription, "auth-info"); len(vals) > 0 {
		return vals
	}
	return nil
}

func stripReadOnlyJobAttributes(req *goipp.Message) {
	if req == nil {
		return
	}
	filter := func(attrs goipp.Attributes) goipp.Attributes {
		if len(attrs) == 0 {
			return attrs
		}
		out := make(goipp.Attributes, 0, len(attrs))
		for _, attr := range attrs {
			if readOnlyJobAttrs[strings.ToLower(attr.Name)] {
				continue
			}
			out = append(out, attr)
		}
		return out
	}
	req.Job = filter(req.Job)
	req.Operation = filter(req.Operation)
}

func sanitizeJobName(req *goipp.Message) string {
	if req == nil {
		return "Untitled"
	}
	if name, found, valid := jobNameFromGroup(&req.Job); found {
		if valid {
			return name
		}
		return "Untitled"
	}
	if name, found, valid := jobNameFromGroup(&req.Operation); found {
		if valid {
			return name
		}
		return "Untitled"
	}
	return "Untitled"
}

func jobNameFromGroup(attrs *goipp.Attributes) (string, bool, bool) {
	if attrs == nil {
		return "", false, false
	}
	for i, attr := range *attrs {
		if !strings.EqualFold(attr.Name, "job-name") {
			continue
		}
		if len(attr.Values) != 1 {
			*attrs = append((*attrs)[:i], (*attrs)[i+1:]...)
			return "", true, false
		}
		tag := attr.Values[0].T
		if tag != goipp.TagName && tag != goipp.TagNameLang {
			*attrs = append((*attrs)[:i], (*attrs)[i+1:]...)
			return "", true, false
		}
		name := strings.TrimSpace(attr.Values[0].V.String())
		if name == "" || !utf8.ValidString(name) {
			*attrs = append((*attrs)[:i], (*attrs)[i+1:]...)
			return "", true, false
		}
		return name, true, true
	}
	return "", false, false
}

func jobOriginatingHostFromRequest(r *http.Request, req *goipp.Message) string {
	host := requestHost(r)
	local := isLocalRequest(r)
	if req == nil {
		return host
	}
	if value, found, valid := jobOriginatingHostFromGroup(&req.Job, local); found {
		if valid {
			return value
		}
		return host
	}
	if value, found, valid := jobOriginatingHostFromGroup(&req.Operation, local); found {
		if valid {
			return value
		}
		return host
	}
	return host
}

func jobOriginatingHostFromGroup(attrs *goipp.Attributes, local bool) (string, bool, bool) {
	if attrs == nil {
		return "", false, false
	}
	for i, attr := range *attrs {
		if !strings.EqualFold(attr.Name, "job-originating-host-name") {
			continue
		}
		*attrs = append((*attrs)[:i], (*attrs)[i+1:]...)
		if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagName || !local {
			return "", true, false
		}
		value := strings.TrimSpace(attr.Values[0].V.String())
		if value == "" || !utf8.ValidString(value) {
			return "", true, false
		}
		return value, true, true
	}
	return "", false, false
}

func requestHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if isLocalRequest(r) {
		return "localhost"
	}
	return strings.TrimSpace(host)
}

func jobOriginatingUserURI(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		user = "anonymous"
	}
	if looksLikeEmail(user) {
		return "mailto:" + user
	}
	return "urn:sub:" + user
}

func looksLikeEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, " ") || strings.Contains(value, "/") {
		return false
	}
	at := strings.Index(value, "@")
	if at <= 0 || at >= len(value)-1 {
		return false
	}
	return true
}

func compressionFromRequest(req *goipp.Message) (string, error) {
	if req == nil {
		return "", nil
	}
	for _, group := range []goipp.Attributes{req.Operation, req.Job} {
		for _, attr := range group {
			if !strings.EqualFold(attr.Name, "compression") {
				continue
			}
			if len(attr.Values) == 0 {
				return "", errBadRequest
			}
			if len(attr.Values) > 1 {
				return "", errBadRequest
			}
			value := strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
			switch value {
			case "", "none":
				return "none", nil
			case "gzip":
				return "gzip", nil
			default:
				return "", errUnsupported
			}
		}
	}
	return "", nil
}

func (s *Server) enforceAuthInfo(r *http.Request, req *goipp.Message, op goipp.Op) error {
	if s == nil || r == nil || req == nil {
		return nil
	}
	authInfo := authInfoFromRequest(req)
	if len(authInfo) > 0 && r.TLS == nil && !isLocalRequest(r) {
		return &ippHTTPError{status: http.StatusUpgradeRequired}
	}
	required := authInfoRequiredForRequest(s, r)
	if len(required) == 1 && strings.EqualFold(required[0], "negotiate") {
		if _, ok := s.authenticate(r, ""); !ok {
			return &ippHTTPError{
				status:   http.StatusUnauthorized,
				authType: s.authTypeForRequest(r, op.String()),
			}
		}
	}
	return nil
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
	sawJobSheetsCol := false
	apply := func(attrs goipp.Attributes, allowAll bool) {
		for _, attr := range attrs {
			if len(attr.Values) == 0 {
				continue
			}
			if attr.Name == "job-sheets" {
				if sawJobSheetsCol {
					continue
				}
				opts["job-sheets"] = joinAttrValues(attr.Values, 2)
				continue
			}
			if attr.Name == "job-sheets-col" {
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
						opts["job-sheets"] = joinStrings(vals, 2)
						sawJobSheetsCol = true
					}
					if media := collectionString(col, "media"); media != "" {
						opts["job-sheets-col-media"] = media
					}
					if mediaCol, ok := collectionCollection(col, "media-col"); ok {
						if name := mediaSizeNameFromCollection(mediaCol, nil); name != "" {
							opts["job-sheets-col-media"] = name
						}
						if v := collectionString(mediaCol, "media-type"); v != "" {
							opts["job-sheets-col-media-type"] = v
						}
						if v := collectionString(mediaCol, "media-source"); v != "" {
							opts["job-sheets-col-media-source"] = v
						}
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
					if templates := collectionStrings(col, "finishing-template"); len(templates) > 0 {
						opts["finishing-template"] = templates[0]
						delete(opts, "finishings")
					} else if vals := collectionInts(col, "finishings"); len(vals) > 0 {
						opts["finishings"] = joinInts(vals)
					}
				}
				continue
			}
			if attr.Name == "print-as-raster" {
				opts["print-as-raster"] = attr.Values[0].V.String()
				continue
			}
			if attr.Name == "output-mode" {
				mode := strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
				if mapped := outputModeForColorMode(mode); mapped != "" {
					mode = mapped
				}
				if mode != "" {
					opts["output-mode"] = mode
					if _, ok := opts["print-color-mode"]; !ok {
						opts["print-color-mode"] = mode
					}
				}
				continue
			}
			if attr.Name == "print-quality" {
				if n, ok := parsePrintQualityValue(attr.Values[0].V.String()); ok {
					opts["print-quality"] = strconv.Itoa(n)
				} else {
					opts["print-quality"] = attr.Values[0].V.String()
				}
				continue
			}
			if attr.Name == "compression" {
				opts["compression-supplied"] = strings.TrimSpace(attr.Values[0].V.String())
				continue
			}
			if attr.Name == "ipp-attribute-fidelity" {
				opts["job-attribute-fidelity"] = strings.TrimSpace(attr.Values[0].V.String())
				continue
			}
			if attr.Name == "attributes-charset" {
				opts["document-charset-supplied"] = strings.TrimSpace(attr.Values[0].V.String())
				continue
			}
			if attr.Name == "attributes-natural-language" {
				opts["document-natural-language-supplied"] = strings.TrimSpace(attr.Values[0].V.String())
				continue
			}
			if allowAll || strings.HasPrefix(attr.Name, "job-") || attr.Name == "sides" || attr.Name == "media" ||
				attr.Name == "print-as-raster" || attr.Name == "print-quality" || attr.Name == "number-up" ||
				attr.Name == "number-up-layout" || attr.Name == "print-scaling" || attr.Name == "orientation-requested" ||
				attr.Name == "page-delivery" || attr.Name == "print-color-mode" || attr.Name == "output-mode" || attr.Name == "finishings" ||
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

func collectPrinterDefaultOptions(attrs goipp.Attributes) (map[string]string, string, bool, *bool) {
	opts := map[string]string{}
	jobSheetsDefault := ""
	jobSheetsOk := false
	sawJobSheetsCol := false
	var sharedPtr *bool

	for _, attr := range attrs {
		if len(attr.Values) == 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(attr.Name))
		deleteAttr := attr.Values[0].T == goipp.TagDeleteAttr
		if name == "" {
			continue
		}
		switch name {
		case "printer-is-shared":
			if deleteAttr {
				sharedPtr = nil
				continue
			}
			val := isTruthy(attr.Values[0].V.String())
			sharedPtr = &val
			continue
		case "printer-error-policy", "printer-op-policy", "port-monitor":
			if deleteAttr {
				opts[name] = ""
				continue
			}
			opts[name] = strings.TrimSpace(attr.Values[0].V.String())
			continue
		case "job-sheets-default", "job-sheets":
			if deleteAttr {
				jobSheetsDefault = ""
				jobSheetsOk = true
				opts["job-sheets"] = ""
				continue
			}
			if sawJobSheetsCol {
				continue
			}
			jobSheetsDefault = strings.Join(parseJobSheetsValues(joinAttrValues(attr.Values, 2)), ",")
			jobSheetsOk = true
			opts["job-sheets"] = jobSheetsDefault
			continue
		case "job-sheets-col-default", "job-sheets-col":
			if deleteAttr {
				sawJobSheetsCol = true
				jobSheetsDefault = ""
				jobSheetsOk = true
				opts["job-sheets"] = ""
				opts["job-sheets-col-media"] = ""
				opts["job-sheets-col-media-type"] = ""
				opts["job-sheets-col-media-source"] = ""
				continue
			}
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				sawJobSheetsCol = true
				if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
					jobSheetsDefault = strings.Join(parseJobSheetsValues(strings.Join(vals, ",")), ",")
					jobSheetsOk = true
					opts["job-sheets"] = jobSheetsDefault
				}
				if media := collectionString(col, "media"); media != "" {
					opts["job-sheets-col-media"] = media
				}
				if mediaCol, ok := collectionCollection(col, "media-col"); ok {
					if name := mediaSizeNameFromCollection(mediaCol, nil); name != "" {
						opts["job-sheets-col-media"] = name
					}
					if v := collectionString(mediaCol, "media-type"); v != "" {
						opts["job-sheets-col-media-type"] = v
					}
					if v := collectionString(mediaCol, "media-source"); v != "" {
						opts["job-sheets-col-media-source"] = v
					}
				}
			}
			continue
		case "media-col-default", "media-col":
			if deleteAttr {
				opts["media"] = ""
				opts["media-type"] = ""
				opts["media-source"] = ""
				continue
			}
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
		case "finishings-col-default", "finishings-col":
			if deleteAttr {
				opts["finishing-template"] = ""
				opts["finishings"] = ""
				continue
			}
			if col, ok := attr.Values[0].V.(goipp.Collection); ok {
				if templates := collectionStrings(col, "finishing-template"); len(templates) > 0 {
					opts["finishing-template"] = templates[0]
					delete(opts, "finishings")
				} else if vals := collectionInts(col, "finishings"); len(vals) > 0 {
					opts["finishings"] = joinInts(vals)
				}
			}
			continue
		case "output-mode-default":
			if deleteAttr {
				opts["output-mode"] = ""
				opts["print-color-mode"] = ""
				continue
			}
			mode := strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
			if mapped := outputModeForColorMode(mode); mapped != "" {
				mode = mapped
			}
			if mode != "" {
				opts["output-mode"] = mode
				if _, ok := opts["print-color-mode"]; !ok {
					opts["print-color-mode"] = mode
				}
			}
			continue
		case "print-quality-default":
			if deleteAttr {
				opts["print-quality"] = ""
				continue
			}
			if n, ok := parsePrintQualityValue(attr.Values[0].V.String()); ok {
				opts["print-quality"] = strconv.Itoa(n)
			} else {
				opts["print-quality"] = attr.Values[0].V.String()
			}
			continue
		}
		if strings.HasSuffix(name, "-default") {
			base := strings.TrimSuffix(name, "-default")
			if base == "" {
				continue
			}
			if deleteAttr {
				opts[base] = ""
				continue
			}
			if len(attr.Values) > 1 {
				opts[base] = joinAttrValues(attr.Values, 0)
			} else {
				opts[base] = attr.Values[0].V.String()
			}
		}
	}
	return opts, jobSheetsDefault, jobSheetsOk, sharedPtr
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

func applyDefaultOptionUpdates(target map[string]string, updates map[string]string) {
	if len(updates) == 0 {
		return
	}
	for k, v := range updates {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if strings.TrimSpace(v) == "" {
			delete(target, key)
			continue
		}
		target[key] = v
	}
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
	sawJobSheetsCol := false

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
				if sawJobSheetsCol {
					continue
				}
				updates["job-sheets"] = joinAttrValues(attr.Values, 2)
			case "job-sheets-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if vals := collectionStrings(col, "job-sheets"); len(vals) > 0 {
						updates["job-sheets"] = joinStrings(vals, 2)
						sawJobSheetsCol = true
					}
					media := collectionString(col, "media")
					if media != "" {
						updates["job-sheets-col-media"] = media
					} else {
						updates["job-sheets-col-media"] = ""
					}
					updates["job-sheets-col-media-type"] = ""
					updates["job-sheets-col-media-source"] = ""
					if mediaCol, ok := collectionCollection(col, "media-col"); ok {
						if name := mediaSizeNameFromCollection(mediaCol, nil); name != "" {
							updates["job-sheets-col-media"] = name
						}
						if v := collectionString(mediaCol, "media-type"); v != "" {
							updates["job-sheets-col-media-type"] = v
						}
						if v := collectionString(mediaCol, "media-source"); v != "" {
							updates["job-sheets-col-media-source"] = v
						}
					}
				}
			case "finishings":
				updates["finishings"] = joinAttrValues(attr.Values, 0)
			case "finishings-col":
				if col, ok := attr.Values[0].V.(goipp.Collection); ok {
					if templates := collectionStrings(col, "finishing-template"); len(templates) > 0 {
						updates["finishing-template"] = templates[0]
						delete(updates, "finishings")
					} else if vals := collectionInts(col, "finishings"); len(vals) > 0 {
						updates["finishings"] = joinInts(vals)
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
				if len(attr.Values) == 0 {
					break
				}
				if _, ok := attr.Values[0].V.(goipp.Range); ok {
					parts := []string{}
					for _, v := range attr.Values {
						r, ok := v.V.(goipp.Range)
						if !ok {
							continue
						}
						if r.Lower <= 0 {
							continue
						}
						if r.Upper > r.Lower {
							parts = append(parts, fmt.Sprintf("%d-%d", r.Lower, r.Upper))
						} else {
							parts = append(parts, fmt.Sprintf("%d", r.Lower))
						}
					}
					if len(parts) > 0 {
						updates["page-ranges"] = strings.Join(parts, ",")
					}
				} else {
					updates["page-ranges"] = attr.Values[0].V.String()
				}
			case "output-mode":
				mode := strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
				if mapped := outputModeForColorMode(mode); mapped != "" {
					mode = mapped
				}
				if mode != "" {
					updates["output-mode"] = mode
					if _, ok := updates["print-color-mode"]; !ok {
						updates["print-color-mode"] = mode
					}
				}
			default:
				switch attr.Name {
				case "copies", "finishings", "job-hold-until", "job-priority", "media", "media-type", "media-source", "multiple-document-handling",
					"number-up", "output-bin", "orientation-requested", "print-color-mode", "output-mode", "print-quality",
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
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(k)), "custom.") {
			continue
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		if skip[k] {
			continue
		}
		if jobKey := ppdOptionToJobKey(k); jobKey != "" {
			if _, ok := opts[jobKey]; !ok {
				opts[jobKey] = normalizePPDChoice(jobKey, v)
			}
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
	if _, ok := opts["print-color-mode"]; !ok {
		if out := strings.TrimSpace(opts["output-mode"]); out != "" {
			if mapped := outputModeForColorMode(out); mapped != "" {
				opts["print-color-mode"] = mapped
			} else {
				opts["print-color-mode"] = out
			}
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
	if v, ok := opts["job-sheets-col-media"]; ok {
		if mapped := ppdMediaToPWG(ppd, v); mapped != "" {
			opts["job-sheets-col-media"] = mapped
		}
	}
	if v, ok := opts["media-source"]; ok {
		if _, mapping := pwgMediaSourceChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["media-source"] = mapped
			}
		}
	}
	if v, ok := opts["job-sheets-col-media-source"]; ok {
		if _, mapping := pwgMediaSourceChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["job-sheets-col-media-source"] = mapped
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
	if v, ok := opts["job-sheets-col-media-type"]; ok {
		if _, mapping := pwgMediaTypeChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, v); mapped != "" {
				opts["job-sheets-col-media-type"] = mapped
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
	maxCopies                         int
	finishingTemplates                []string
	finishingTemplateDefault          string
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
		printQualitySupported:             []int{4},
		numberUpSupported:                 []int{1, 2, 4, 6, 9, 16},
		orientationSupported:              []int{3, 4, 5, 6},
		pageDeliverySupported:             []string{"reverse-order", "same-order"},
		printScalingSupported:             []string{"auto", "auto-fit", "fill", "fit", "none"},
		jobHoldUntilSupported:             []string{"no-hold", "indefinite", "day-time", "evening", "night", "second-shift", "third-shift", "weekend"},
		multipleDocumentHandlingSupported: []string{"separate-documents-uncollated-copies", "separate-documents-collated-copies"},
		jobSheetsSupported:                jobSheetsSupported(),
		maxCopies:                         9999,
	}
	defaultOpts = mapJobOptionsToPWG(defaultOpts, ppd)
	caps.resDefault = caps.resolutions[0]
	caps.finishingsSupported = finishingsSupportedFromPPD(ppd)
	if len(caps.finishingsSupported) == 0 {
		caps.finishingsSupported = []int{3}
	}
	caps.finishingTemplates, caps.finishingTemplateDefault = finishingsTemplatesFromPPD(ppd)
	caps.printQualitySupported = printQualitySupportedFromPPD(ppd)

	if ppd != nil {
		if ppd.MaxCopies > 0 {
			caps.maxCopies = ppd.MaxCopies
		} else if ppd.ManualCopies {
			caps.maxCopies = 1
		}
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
			defColor := strings.TrimSpace(ppd.DefaultColorSpace)
			if defColor == "" {
				// IPP Everywhere PPDs generated by CUPS use DefaultColorModel.
				defColor = firstNonEmpty(ppd.Defaults["ColorModel"], ppd.Defaults["ColorMode"], ppd.Defaults["ColorSpace"])
			}
			if defColor != "" {
				lc := strings.ToLower(defColor)
				if strings.Contains(lc, "gray") || strings.Contains(lc, "mono") {
					caps.colorDefault = "monochrome"
				} else {
					caps.colorDefault = "color"
				}
			} else if ppd.ColorDevice {
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

	if isCustomSizeName(caps.mediaDefault) {
		key := strings.ToLower(strings.TrimSpace(caps.mediaDefault))
		if key != "" {
			if _, ok := caps.mediaSizes[key]; !ok {
				if size, ok := parseCustomMediaSize(caps.mediaDefault); ok {
					caps.mediaSizes[key] = mediaSize{
						X:      size.X,
						Y:      size.Y,
						Left:   caps.mediaCustomRange.Margins[0],
						Bottom: caps.mediaCustomRange.Margins[1],
						Right:  caps.mediaCustomRange.Margins[2],
						Top:    caps.mediaCustomRange.Margins[3],
					}
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

func computePrinterType(printer model.Printer, caps printerCaps, ppd *config.PPD, isClass bool, authInfo []string) int {
	typ := 0
	if isClass {
		typ |= cupsPTypeClass
	} else {
		typ |= cupsPTypeBW
		if isColorModeSupported(caps.colorModes) {
			typ |= cupsPTypeColor
		}
		if supportsDuplex(caps.sidesSupported) {
			typ |= cupsPTypeDuplex
		}
		if ppd == nil || !ppd.ManualCopies {
			if caps.maxCopies > 1 {
				typ |= cupsPTypeCopies
			}
		}
		if caps.mediaCustomRange.MinW > 0 || caps.mediaCustomRange.MaxW > 0 {
			typ |= cupsPTypeVariable
		}
		if sizeBits := mediaSizeBits(caps.mediaSizes); sizeBits != 0 {
			typ |= sizeBits
		}
		if finishingsHasPrefix(caps.finishingsSupported, "staple") {
			typ |= cupsPTypeStaple
		}
		if finishingsHasPrefix(caps.finishingsSupported, "punch") {
			typ |= cupsPTypePunch
		}
		if finishingsHasPrefix(caps.finishingsSupported, "cover") {
			typ |= cupsPTypeCover
		}
		if finishingsHasPrefix(caps.finishingsSupported, "bind") {
			typ |= cupsPTypeBind
		}
		if finishingsHasPrefix(caps.finishingsSupported, "fold") {
			typ |= cupsPTypeFold
		}
		if ppd != nil {
			if hasCommandFilter(ppd.Filters) {
				typ |= cupsPTypeCommands
			}
			if ppd.APICADriver {
				if ppd.APScannerOnly {
					typ |= cupsPTypeScanner
				} else {
					typ |= cupsPTypeMFP
				}
			}
		}
	}
	if printer.IsDefault {
		typ |= cupsPTypeDefault
	}
	if !printer.Accepting {
		typ |= cupsPTypeRejecting
	}
	if !isClass && !printer.Shared {
		typ |= cupsPTypeNotShared
	}
	if len(authInfo) > 0 && !(len(authInfo) == 1 && strings.EqualFold(authInfo[0], "none")) {
		typ |= cupsPTypeAuth
	}
	return typ
}

func computeClassType(class model.Class, shared bool, authInfo []string) int {
	typ := cupsPTypeClass | cupsPTypeBW
	if class.IsDefault {
		typ |= cupsPTypeDefault
	}
	if !class.Accepting {
		typ |= cupsPTypeRejecting
	}
	if !shared {
		typ |= cupsPTypeNotShared
	}
	if len(authInfo) > 0 && !(len(authInfo) == 1 && strings.EqualFold(authInfo[0], "none")) {
		typ |= cupsPTypeAuth
	}
	return typ
}

func isColorModeSupported(modes []string) bool {
	for _, m := range modes {
		if strings.EqualFold(m, "color") {
			return true
		}
	}
	return false
}

func supportsDuplex(sides []string) bool {
	for _, s := range sides {
		if strings.EqualFold(s, "two-sided-long-edge") || strings.EqualFold(s, "two-sided-short-edge") {
			return true
		}
	}
	return false
}

func finishingsHasPrefix(values []int, prefix string) bool {
	for _, v := range values {
		if name := finishingsEnumToName(v); name != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func mediaSizeBits(sizes map[string]mediaSize) int {
	if len(sizes) == 0 {
		return cupsPTypeSmall
	}
	const mediumLen = 35560 // 14 inches in PWG units (1/100 mm)
	const largeLen = 60960  // 24 inches in PWG units
	small := false
	medium := false
	large := false
	for _, size := range sizes {
		length := size.Y
		if size.X > length {
			length = size.X
		}
		if length > largeLen {
			large = true
		} else if length > mediumLen {
			medium = true
		} else {
			small = true
		}
	}
	bits := 0
	if small {
		bits |= cupsPTypeSmall
	}
	if medium {
		bits |= cupsPTypeMedium
	}
	if large {
		bits |= cupsPTypeLarge
	}
	return bits
}

func hasCommandFilter(filters []config.PPDFilter) bool {
	for _, filter := range filters {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(filter.Source)), "application/vnd.cups-command") {
			return true
		}
	}
	return false
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

func printerCommandsForPPD(ppd *config.PPD) []string {
	if ppd == nil {
		return []string{"none"}
	}
	if len(ppd.Commands) > 0 {
		out := make([]string, 0, len(ppd.Commands))
		for _, cmd := range ppd.Commands {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			out = append(out, cmd)
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"AutoConfigure", "Clean", "PrintSelfTestPage"}
}

func referenceURISchemesSupported() []string {
	return []string{"file", "ftp", "http", "https"}
}

func normalizeSchemeSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, raw := range values {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '	' || r == 10 || r == 13 || r == ' '
		}) {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func schemeAllowed(uri string, include, exclude map[string]bool) bool {
	scheme := strings.ToLower(strings.TrimSpace(uriScheme(uri)))
	if scheme == "" {
		return len(include) == 0
	}
	if len(include) > 0 && !include[scheme] {
		return false
	}
	if len(exclude) > 0 && exclude[scheme] {
		return false
	}
	return true
}

func uriScheme(uri string) string {
	if uri == "" {
		return ""
	}
	if u, err := url.Parse(uri); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	if idx := strings.Index(uri, ":"); idx > 0 {
		return uri[:idx]
	}
	return ""
}

func ippSchemeForRequest(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return "ipps"
	}
	return "ipp"
}

func uriSecurityForRequest(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return "tls"
	}
	return "none"
}

func authSupportedForAuthInfo(authInfo []string) string {
	auth := "requesting-user-name"
	for _, v := range authInfo {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "negotiate", "kerberos":
			return "negotiate"
		case "basic", "digest", "username", "password", "domain":
			auth = "basic"
		}
	}
	return auth
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
	refreshPPDCache(ppdName, ppdPath)
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

func loadPPDForListing(ppdName string, ppdDir string) (*config.PPD, error) {
	ppdName = strings.TrimSpace(ppdName)
	if ppdName == "" {
		return nil, os.ErrNotExist
	}
	ppdPath := safePPDPath(ppdDir, ppdName)
	refreshPPDCache(ppdName, ppdPath)
	return config.LoadPPD(ppdPath)
}

func refreshPPDCache(ppdName, ppdPath string) {
	st := appStore()
	if st == nil {
		return
	}
	ppdName = strings.TrimSpace(ppdName)
	if ppdName == "" || strings.TrimSpace(ppdPath) == "" {
		return
	}
	hash, size, mtime, ok := ppdFileCacheMetadata(ppdPath)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = st.WithTx(ctx, false, func(tx *sql.Tx) error {
		cachedHash, cachedAttrs, _, exists, err := st.GetPPDCache(ctx, tx, ppdName)
		if err != nil {
			return nil
		}
		payload := parsePPDCachePayload(cachedAttrs)
		if exists && cachedHash == hash && payload.Size == size && payload.MTime == mtime {
			if len(payload.Formats) > 0 {
				key := ppdFormatCacheKey(ppdName, hash)
				printerFormatOnce.Store(key, cloneStringSlice(payload.Formats))
			}
			return nil
		}
		next := ppdCachePayload{Size: size, MTime: mtime}
		if exists && cachedHash == hash && len(payload.Formats) > 0 {
			next.Formats = payload.Formats
		}
		raw := marshalPPDCachePayload(next)
		if err := st.SetPPDCache(ctx, tx, ppdName, hash, raw); err != nil {
			return nil
		}
		if len(next.Formats) > 0 {
			key := ppdFormatCacheKey(ppdName, hash)
			printerFormatOnce.Store(key, cloneStringSlice(next.Formats))
		}
		return nil
	})
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

func ppdValidForListing(ppd *config.PPD) bool {
	if ppd == nil {
		return false
	}
	if strings.TrimSpace(ppdMakeAndModel(ppd)) == "" {
		return false
	}
	if len(ppd.Products) == 0 || len(ppd.PSVersions) == 0 {
		return false
	}
	return true
}

func ppdMake(ppd *config.PPD) string {
	if ppd == nil {
		return ""
	}
	if makeVal := strings.TrimSpace(ppd.Make); makeVal != "" {
		return makeVal
	}
	return strings.TrimSpace(ppd.MakeAndModel)
}

func ppdMakeAndModel(ppd *config.PPD) string {
	if ppd == nil {
		return ""
	}
	if mm := strings.TrimSpace(ppd.MakeAndModel); mm != "" {
		return mm
	}
	return strings.TrimSpace(firstNonEmpty(ppd.NickName, ppd.Model, ppd.Make))
}

func ppdLanguages(ppd *config.PPD) []string {
	if ppd == nil {
		return []string{"en"}
	}
	if len(ppd.Languages) > 0 {
		return ppd.Languages
	}
	if lang := strings.TrimSpace(ppd.LanguageVersion); lang != "" {
		return []string{lang}
	}
	return []string{"en"}
}

func schemeNameAllowed(scheme string, include, exclude map[string]bool) bool {
	if scheme == "" {
		return len(include) == 0
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if len(include) > 0 && !include[scheme] {
		return false
	}
	if len(exclude) > 0 && exclude[scheme] {
		return false
	}
	return true
}

func normalizePPDType(value string) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "postscript", "pdf", "raster", "fax", "object", "object-direct", "object-storage", "unknown", "drv", "archive":
		return value, true
	default:
		return "", false
	}
}

func compilePPDStringRegex(value string) *regexp.Regexp {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	pattern := "(?i)" + regexp.QuoteMeta(value)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

func compilePPDDeviceIDRegex(deviceID string) *regexp.Regexp {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil
	}
	var b strings.Builder
	for len(deviceID) > 0 {
		cmd := hasPrefixFold(deviceID, "COMMAND SET:") || hasPrefixFold(deviceID, "CMD:")
		if cmd ||
			hasPrefixFold(deviceID, "MANUFACTURER:") ||
			hasPrefixFold(deviceID, "MFG:") ||
			hasPrefixFold(deviceID, "MFR:") ||
			hasPrefixFold(deviceID, "MODEL:") ||
			hasPrefixFold(deviceID, "MDL:") {
			if b.Len() > 0 {
				b.WriteString(".*")
			}
			b.WriteString("(")
			for len(deviceID) > 0 && deviceID[0] != ';' {
				ch := deviceID[0]
				if strings.ContainsRune("[]{}().*\\|", rune(ch)) {
					b.WriteByte('\\')
				}
				if ch == ':' {
					b.WriteByte(ch)
					b.WriteString(".*")
				} else {
					b.WriteByte(ch)
				}
				deviceID = deviceID[1:]
			}
			if len(deviceID) == 0 || deviceID[0] == ';' {
				b.WriteString(".*;")
			}
			b.WriteString(")")
			if cmd {
				b.WriteString("?")
			}
		} else if idx := strings.IndexByte(deviceID, ';'); idx >= 0 {
			deviceID = deviceID[idx+1:]
			continue
		} else {
			break
		}
		if len(deviceID) > 0 && deviceID[0] == ';' {
			deviceID = deviceID[1:]
		}
	}
	if b.Len() == 0 {
		return nil
	}
	re, err := regexp.Compile("(?i)" + b.String())
	if err != nil {
		return nil
	}
	return re
}

func scorePPDMatch(ppd *config.PPD, deviceRe *regexp.Regexp, language, makeVal string, makeModelRe *regexp.Regexp, makeModelLen int, modelNumber int64, product string, productLen int, psversion, typeStr string) int {
	if ppd == nil {
		return 0
	}
	score := 0
	if deviceRe != nil && strings.TrimSpace(ppd.DeviceID) != "" {
		if loc := deviceRe.FindStringSubmatchIndex(ppd.DeviceID); loc != nil {
			for i := 2; i+1 < len(loc); i += 2 {
				if loc[i] >= 0 {
					score++
				}
			}
		}
	}
	if language != "" {
		for _, lang := range ppdLanguages(ppd) {
			if lang == language {
				score++
				break
			}
		}
	}
	if makeVal != "" && strings.EqualFold(ppdMake(ppd), makeVal) {
		score++
	}
	if makeModelRe != nil {
		if loc := makeModelRe.FindStringIndex(ppdMakeAndModel(ppd)); loc != nil {
			if loc[0] == 0 {
				if loc[1] == makeModelLen {
					score += 3
				} else {
					score += 2
				}
			} else {
				score++
			}
		}
	}
	if modelNumber != 0 && int64(ppd.ModelNumber) == modelNumber {
		score++
	}
	if product != "" && productLen > 0 {
		for _, p := range ppd.Products {
			if strings.EqualFold(p, product) {
				score += 3
				break
			}
			if len(p) >= productLen && strings.EqualFold(p[:productLen], product) {
				score += 2
				break
			}
		}
	}
	if psversion != "" {
		for _, v := range ppd.PSVersions {
			if strings.EqualFold(v, psversion) {
				score++
				break
			}
		}
	}
	if typeStr != "" && strings.EqualFold(ppd.PPDType, typeStr) {
		score++
	}
	return score
}

func hasPrefixFold(value, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	return strings.EqualFold(value[:len(prefix)], prefix)
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

func validateRequestOptions(req *goipp.Message, printer model.Printer) (*ippWarning, error) {
	warn, err := validateIppOptions(req, printer)
	if err != nil {
		return nil, err
	}
	ppd, err := loadPPDForPrinter(printer)
	if err != nil || ppd == nil || len(ppd.Constraints) == 0 {
		return warn, nil
	}
	opts := collectJobOptions(req)
	opts = applyPPDDefaults(opts, ppd)
	opts = applyPrinterDefaults(opts, printer)
	opts = mapJobOptionsToPWG(opts, ppd)
	if err := validatePPDConstraints(ppd, opts); err != nil {
		return nil, err
	}
	return warn, nil
}

func validateIppOptions(req *goipp.Message, printer model.Printer) (*ippWarning, error) {
	if req == nil {
		return nil, nil
	}
	hasAttr := func(attrs goipp.Attributes, name string) bool {
		for _, attr := range attrs {
			if strings.EqualFold(attr.Name, name) {
				return true
			}
		}
		return false
	}
	hasMedia := hasAttr(req.Job, "media") || hasAttr(req.Operation, "media")
	hasMediaCol := hasAttr(req.Job, "media-col") || hasAttr(req.Operation, "media-col")
	if hasMedia && hasMediaCol {
		return nil, errBadRequest
	}
	hasFinishings := hasAttr(req.Job, "finishings") || hasAttr(req.Operation, "finishings")
	hasFinishingsCol := hasAttr(req.Job, "finishings-col") || hasAttr(req.Operation, "finishings-col")
	if hasFinishings && hasFinishingsCol {
		return nil, errBadRequest
	}
	if _, err := compressionFromRequest(req); err != nil {
		return nil, err
	}

	ppd, _ := loadPPDForPrinter(printer)
	documentFormats := supportedDocumentFormatsForPrinter(printer, ppd)
	defaultOpts := parseJobOptions(printer.DefaultOptions)
	caps := computePrinterCaps(ppd, defaultOpts)
	finishingsTemplates := caps.finishingTemplates
	if len(finishingsTemplates) == 0 {
		finishingsTemplates = finishingsTemplatesFromEnums(caps.finishingsSupported)
	}
	allowCustomMedia := supportsCustomPageSize(ppd)
	accountSupported := ppd != nil && ppd.JobAccountID
	accountingUserSupported := ppd != nil && ppd.JobAccountingUser
	passwordSupported := ppd != nil && strings.TrimSpace(ppd.JobPassword) != ""
	numberUpLayouts := numberUpLayoutSupported()
	if ppd != nil && len(ppd.Mandatory) > 0 {
		for _, mandatory := range ppd.Mandatory {
			if strings.TrimSpace(mandatory) == "" {
				continue
			}
			if !hasAttr(req.Job, mandatory) && !hasAttr(req.Operation, mandatory) {
				return nil, errConflicting
			}
		}
	}
	var sourceMap map[string]string
	var typeMap map[string]string
	var binMap map[string]string
	if ppd != nil {
		_, sourceMap = pwgMediaSourceChoices(ppd)
		_, typeMap = pwgMediaTypeChoices(ppd)
		_, binMap = pwgOutputBinChoices(ppd)
	}

	var warn *ippWarning
	checkAttr := func(attr goipp.Attribute) error {
		if attr.Name == "" || len(attr.Values) == 0 {
			return nil
		}
		switch attr.Name {
		case "document-format":
			if !isDocumentFormatSupportedInList(documentFormats, attr.Values[0].V.String()) {
				return errUnsupported
			}
		case "copies":
			if n, ok := valueInt(attr.Values[0].V); ok {
				maxCopies := caps.maxCopies
				if maxCopies <= 0 {
					maxCopies = 9999
				}
				if n < 1 || n > maxCopies {
					return errUnsupported
				}
			}
		case "job-priority":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if n < 1 || n > 100 {
					return errUnsupported
				}
			}
		case "job-account-id":
			if !accountSupported {
				return errUnsupported
			}
		case "job-accounting-user-id":
			if !accountingUserSupported {
				return errUnsupported
			}
		case "job-password":
			if !passwordSupported {
				return errUnsupported
			}
		case "job-hold-until":
			if len(attr.Values) != 1 {
				return errUnsupported
			}
			tag := attr.Values[0].T
			if tag != goipp.TagKeyword && tag != goipp.TagName && tag != goipp.TagNameLang {
				return errUnsupported
			}
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
		case "ipp-attribute-fidelity":
			if len(attr.Values) != 1 {
				return errUnsupported
			}
			if _, ok := attr.Values[0].V.(goipp.Boolean); !ok {
				switch strings.ToLower(strings.TrimSpace(attr.Values[0].V.String())) {
				case "true", "false", "1", "0", "yes", "no", "on", "off":
				default:
					return errUnsupported
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
			if w := mediaColMarginWarning(col, caps, ppd, allowCustomMedia); w != nil {
				warn = mergeWarning(warn, w)
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
		case "output-mode":
			mode := outputModeForColorMode(attr.Values[0].V.String())
			if mode == "" {
				mode = strings.ToLower(strings.TrimSpace(attr.Values[0].V.String()))
			}
			if !stringInList(mode, caps.colorModes) {
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
			} else if n, ok := parsePrintQualityValue(attr.Values[0].V.String()); ok {
				if !intInList(n, caps.printQualitySupported) {
					return errUnsupported
				}
			} else {
				return errUnsupported
			}
		case "number-up":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if !intInList(n, caps.numberUpSupported) {
					return errUnsupported
				}
			}
		case "number-up-layout":
			if !stringInList(attr.Values[0].V.String(), numberUpLayouts) {
				return errUnsupported
			}
		case "orientation-requested":
			if n, ok := valueInt(attr.Values[0].V); ok {
				if !intInList(n, caps.orientationSupported) {
					return errUnsupported
				}
			}
		case "print-as-raster":
			if !boolStringValid(attr.Values[0].V.String()) {
				return errUnsupported
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
			ranges := []goipp.Range{}
			if len(attr.Values) == 0 {
				return nil
			}
			if _, ok := attr.Values[0].V.(goipp.Range); ok {
				for _, v := range attr.Values {
					r, ok := v.V.(goipp.Range)
					if !ok {
						return errUnsupported
					}
					ranges = append(ranges, r)
				}
			} else {
				parsed, ok := parsePageRangesList(attr.Values[0].V.String())
				if !ok {
					return errUnsupported
				}
				ranges = parsed
			}
			if !validatePageRanges(ranges) {
				return errBadRequest
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
		return nil, err
	}
	if err := apply(req.Operation); err != nil {
		return nil, err
	}
	return warn, nil
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
		if matchDimsToSizes(x, y, caps.mediaSizes) == "" && allowCustom {
			if caps.mediaCustomRange.MinW > 0 && caps.mediaCustomRange.MaxW > 0 &&
				caps.mediaCustomRange.MinL > 0 && caps.mediaCustomRange.MaxL > 0 {
				if x < caps.mediaCustomRange.MinW || x > caps.mediaCustomRange.MaxW ||
					y < caps.mediaCustomRange.MinL || y > caps.mediaCustomRange.MaxL {
					return errUnsupported
				}
			}
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

func mediaColMarginWarning(col goipp.Collection, caps printerCaps, ppd *config.PPD, allowCustom bool) *ippWarning {
	expected, ok := expectedMarginsForMediaCol(col, caps, ppd, allowCustom)
	if !ok {
		return nil
	}
	margins, present := mediaColRequestedMargins(col)
	hasAny := false
	mismatch := false
	for i := 0; i < len(margins); i++ {
		if !present[i] {
			continue
		}
		hasAny = true
		if margins[i] != expected[i] {
			mismatch = true
		}
	}
	if !hasAny || !mismatch {
		return nil
	}
	unsupCol := goipp.Collection{}
	if present[1] {
		unsupCol.Add(goipp.MakeAttribute("media-bottom-margin", goipp.TagInteger, goipp.Integer(margins[1])))
	}
	if present[0] {
		unsupCol.Add(goipp.MakeAttribute("media-left-margin", goipp.TagInteger, goipp.Integer(margins[0])))
	}
	if present[2] {
		unsupCol.Add(goipp.MakeAttribute("media-right-margin", goipp.TagInteger, goipp.Integer(margins[2])))
	}
	if present[3] {
		unsupCol.Add(goipp.MakeAttribute("media-top-margin", goipp.TagInteger, goipp.Integer(margins[3])))
	}
	if len(unsupCol) == 0 {
		return nil
	}
	attr := goipp.MakeAttribute("media-col", goipp.TagBeginCollection, unsupCol)
	return &ippWarning{
		status:      goipp.StatusOkIgnoredOrSubstituted,
		unsupported: goipp.Attributes{attr},
	}
}

func expectedMarginsForMediaCol(col goipp.Collection, caps printerCaps, ppd *config.PPD, allowCustom bool) ([4]int, bool) {
	name := mediaSizeNameFromCollection(col, caps.mediaSizes)
	if name != "" && ppd != nil {
		if mapped := ppdMediaToPWG(ppd, name); mapped != "" {
			name = mapped
		}
	}
	if name != "" {
		if size, ok := caps.mediaSizes[name]; ok {
			return [4]int{size.Left, size.Bottom, size.Right, size.Top}, true
		}
		if allowCustom && isCustomSizeName(name) && caps.mediaCustomRange.MinW > 0 {
			return caps.mediaCustomRange.Margins, true
		}
	}
	return [4]int{}, false
}

func mediaColRequestedMargins(col goipp.Collection) ([4]int, [4]bool) {
	var margins [4]int
	var present [4]bool
	if v, ok := collectionIntOk(col, "media-left-margin"); ok {
		margins[0] = v
		present[0] = true
	}
	if v, ok := collectionIntOk(col, "media-bottom-margin"); ok {
		margins[1] = v
		present[1] = true
	}
	if v, ok := collectionIntOk(col, "media-right-margin"); ok {
		margins[2] = v
		present[2] = true
	}
	if v, ok := collectionIntOk(col, "media-top-margin"); ok {
		margins[3] = v
		present[3] = true
	}
	return margins, present
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
	case "outputmode", "cupsprintquality":
		return "print-quality"
	case "cupsfinishingtemplate":
		return "finishing-template"
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
	case "print-quality":
		switch strings.ToLower(c) {
		case "draft", "fast", "low":
			return "3"
		case "high", "best":
			return "5"
		case "normal":
			return "4"
		}
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
	values := []string{}
	for _, v := range attr.Values {
		val := strings.TrimSpace(v.V.String())
		if val == "" {
			continue
		}
		if isPickMany {
			if looksLikeCustomDict(val) {
				values = append(values, val)
				continue
			}
			if strings.ContainsAny(val, ",; \t\n\r") {
				values = append(values, splitList(val)...)
				continue
			}
		}
		values = append(values, val)
	}
	if !isPickMany {
		if len(attr.Values) > 1 || len(values) > 1 {
			return errUnsupported
		}
	}
	for _, val := range values {
		if val == "" {
			continue
		}
		if opt.Custom {
			lower := strings.ToLower(val)
			if strings.HasPrefix(lower, "custom") || looksLikeCustomDict(val) {
				if err := validateCustomOptionValue(opt, val); err != nil {
					return errUnsupported
				}
				continue
			}
			if strings.EqualFold(val, "custom") {
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

func validateCustomOptionValue(opt *config.PPDOption, raw string) error {
	if opt == nil {
		return nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	lower := strings.ToLower(value)
	if strings.EqualFold(lower, "custom") {
		return nil
	}
	isDict := looksLikeCustomDict(value)
	if !strings.HasPrefix(lower, "custom.") && !isDict {
		return errUnsupported
	}
	if len(opt.CustomParams) == 0 {
		return errUnsupported
	}
	params := parseCustomOptionValue(opt, value)
	units := strings.TrimSpace(params["Units"])
	if units == "" {
		units = "pt"
	}
	nonUnits := make([]config.PPDCustomParam, 0, len(opt.CustomParams))
	for _, p := range opt.CustomParams {
		if isUnitsParam(p) {
			continue
		}
		nonUnits = append(nonUnits, p)
	}
	for _, p := range opt.CustomParams {
		if isUnitsParam(p) {
			unitVal := strings.TrimSpace(params[p.Name])
			if unitVal == "" {
				unitVal = units
			}
			if _, err := normalizeCustomParamValue(p, unitVal); err != nil {
				return errUnsupported
			}
			continue
		}
		val := strings.TrimSpace(params[p.Name])
		if val == "" {
			if isDict {
				return errUnsupported
			}
			if strings.HasPrefix(lower, "custom.") && len(nonUnits) == 1 {
				return errUnsupported
			}
			continue
		}
		if err := validateCustomParamValue(p, val, units); err != nil {
			return errUnsupported
		}
	}
	return nil
}

func validateCustomParamValue(param config.PPDCustomParam, raw, units string) error {
	value := strings.TrimSpace(raw)
	switch strings.ToLower(param.Type) {
	case "points":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return errUnsupported
		}
		points := f
		if units == "" {
			units = "pt"
		}
		if !strings.EqualFold(units, "pt") {
			converted, ok := customUnitsToPoints(f, units)
			if !ok {
				return errUnsupported
			}
			points = converted
		}
		if param.Range && (points < param.Min || points > param.Max) {
			return errUnsupported
		}
	case "real", "curve", "invcurve":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return errUnsupported
		}
		if param.Range && (f < param.Min || f > param.Max) {
			return errUnsupported
		}
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return errUnsupported
		}
		if param.Range && (float64(n) < param.Min || float64(n) > param.Max) {
			return errUnsupported
		}
	case "string", "password":
		if param.Range {
			l := utf8.RuneCountInString(value)
			if l < int(param.Min) || l > int(param.Max) {
				return errUnsupported
			}
		}
	}
	return nil
}

func customUnitsToPoints(value float64, units string) (float64, bool) {
	if units == "" {
		return value, true
	}
	if strings.EqualFold(units, "pt") {
		return value, true
	}
	scale, ok := unitsToHundredthMM(units)
	if !ok {
		return 0, false
	}
	mm := value * scale / 100.0
	return mm / 25.4 * 72.0, true
}

func isUnitsParam(param config.PPDCustomParam) bool {
	return strings.EqualFold(param.Name, "Units") || strings.EqualFold(param.Type, "units")
}

func looksLikeCustomDict(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")
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

func makeURISchemesAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String(""))
	}
	return goipp.MakeAttr(name, goipp.TagURIScheme, vals[0], vals[1:]...)
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

func serverIsSharingPrinters(cfg config.Config, st *store.Store, r *http.Request) bool {
	if !cfg.BrowseLocal {
		return false
	}
	if len(cfg.BrowseLocalProtocols) == 0 {
		return false
	}
	return sharingEnabled(r, st)
}

func isLocalRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if i := strings.Index(host, "%"); i != -1 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
	err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		members, err := s.Store.ListClassMembers(ctx, tx, classID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return sql.ErrNoRows
		}

		eligible := make([]model.Printer, 0, len(members))
		for _, p := range members {
			// Match CUPS behavior: choose an accepting/enabled queue.
			if p.Accepting && p.State != 5 {
				eligible = append(eligible, p)
			}
		}
		if len(eligible) == 0 {
			return sql.ErrNoRows
		}

		// Round-robin between class members (persistent across restarts).
		key := fmt.Sprintf("class.%d.last_printer_id", classID)
		last, err := s.Store.GetSetting(ctx, tx, key, "")
		if err != nil {
			return err
		}
		start := 0
		if lastID, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64); err == nil && lastID > 0 {
			for i, p := range eligible {
				if p.ID == lastID {
					start = (i + 1) % len(eligible)
					break
				}
			}
		}
		selected = eligible[start]
		_ = s.Store.SetSetting(ctx, tx, key, strconv.FormatInt(selected.ID, 10))
		return nil
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

func loadUserAccessLists(ctx context.Context, st *store.Store, keyPrefix string) ([]string, []string) {
	if st == nil || keyPrefix == "" {
		return nil, nil
	}
	allowed := ""
	denied := ""
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		allowed, err = st.GetSetting(ctx, tx, keyPrefix+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = st.GetSetting(ctx, tx, keyPrefix+".denied_users", "")
		return err
	})
	return splitUserList(allowed), splitUserList(denied)
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

func makeLanguagesAttr(name string, values []string) goipp.Attribute {
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
	}
	if len(vals) == 0 {
		vals = append(vals, goipp.String("en"))
	}
	return goipp.MakeAttr(name, goipp.TagLanguage, vals[0], vals[1:]...)
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

func makeJobSheetsColAttr(name, sheets, media, mediaType, mediaSource string) goipp.Attribute {
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
	if strings.TrimSpace(media) != "" {
		col.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(media)))
		col.Add(makeMediaColAttrWithOptions("media-col", media, mediaType, mediaSource, nil))
	}
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

func makeJobSheetsDefaultAttr(name string, sheets string) goipp.Attribute {
	values := parseJobSheetsValues(sheets)
	switch len(values) {
	case 0:
		values = []string{"none", "none"}
	case 1:
		values = append(values, "none")
	default:
		if len(values) > 2 {
			values = values[:2]
		}
	}
	vals := make([]goipp.Value, 0, len(values))
	for _, v := range values {
		vals = append(vals, goipp.String(v))
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
	addMediaMargins(&col, media, sizes)
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
		addMediaMargins(&col, m, sizes)
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
		addMediaMargins(&col, "A4", sizes)
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
		addMediaMargins(&col, m, sizes)
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
		addMediaMargins(&col, "A4", sizes)
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func addMediaMargins(col *goipp.Collection, media string, sizes map[string]mediaSize) {
	if col == nil {
		return
	}
	if size, ok := mediaSizeLookup(media, sizes); ok {
		col.Add(goipp.MakeAttribute("media-bottom-margin", goipp.TagInteger, goipp.Integer(size.Bottom)))
		col.Add(goipp.MakeAttribute("media-left-margin", goipp.TagInteger, goipp.Integer(size.Left)))
		col.Add(goipp.MakeAttribute("media-right-margin", goipp.TagInteger, goipp.Integer(size.Right)))
		col.Add(goipp.MakeAttribute("media-top-margin", goipp.TagInteger, goipp.Integer(size.Top)))
	}
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
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag("none"), goipp.String("none")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeFinishingsColDatabaseFromTemplates(name string, templates []string) goipp.Attribute {
	templates = normalizeFinishingsTemplates(templates)
	cols := make([]goipp.Value, 0, len(templates))
	for _, template := range templates {
		if strings.TrimSpace(template) == "" {
			continue
		}
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag("none"), goipp.String("none")))
		cols = append(cols, col)
	}
	return goipp.MakeAttr(name, goipp.TagBeginCollection, cols[0], cols[1:]...)
}

func makeFinishingsColAttr(name string, finishings []int) goipp.Attribute {
	col := goipp.Collection{}
	template := finishingsTemplateForValues(finishings)
	col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, col)
}

func makeFinishingsColAttrWithTemplate(name, template string) goipp.Attribute {
	col := goipp.Collection{}
	if strings.TrimSpace(template) == "" {
		template = "none"
	}
	col.Add(goipp.MakeAttribute("finishing-template", finishingTemplateTag(template), goipp.String(template)))
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

func numberUpLayoutSupported() []string {
	return []string{
		"btlr", "btrl", "lrbt", "lrtb", "rlbt", "rltb", "tblr", "tbrl",
	}
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
	fraction := ((val%2540)*1000 + 1270) / 2540
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

func pwgMediaSourceFromPPDValue(value string) string {
	if value == "" {
		return ""
	}
	if mapped := pwgMediaSourceFromPPD(value, value); mapped != "" {
		return mapped
	}
	return pwgUnppdizeName(value, "_")
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

func pwgMediaTypeFromPPD(ppd *config.PPD, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if ppd != nil {
		if _, mapping := pwgMediaTypeChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, value); mapped != "" {
				return mapped
			}
		}
	}
	return pwgUnppdizeName(value, "_")
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

func pwgOutputBinFromPPD(ppd *config.PPD, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if ppd != nil {
		if _, mapping := pwgOutputBinChoices(ppd); len(mapping) > 0 {
			if mapped := mappedPPDChoice(mapping, value); mapped != "" {
				return mapped
			}
		}
	}
	return pwgUnppdizeName(value, "_")
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

func lookupMediaPPDNameByDims(x, y int) string {
	pwgMediaOnce.Do(loadPWGMediaTable)
	if len(pwgMediaPPDByDims) == 0 {
		return ""
	}
	if name, ok := pwgMediaPPDByDims[mediaDimsKey(x, y)]; ok {
		return name
	}
	if name, ok := pwgMediaPPDByDims[mediaDimsKey(y, x)]; ok {
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
	pwgMediaPPDByDims = map[string]string{}
	if path := findPWGMediaFile(); path != "" {
		_ = parsePWGMediaFile(path)
	}
	if len(pwgMediaByName) == 0 {
		addPWGMediaName("A4", 21000, 29700, true)
		addPWGMediaPPDName("A4", 21000, 29700)
		addPWGMediaName("Letter", 21590, 27940, true)
		addPWGMediaPPDName("Letter", 21590, 27940)
		addPWGMediaName("Legal", 21590, 35560, true)
		addPWGMediaPPDName("Legal", 21590, 35560)
		addPWGMediaName("A5", 14800, 21000, true)
		addPWGMediaPPDName("A5", 14800, 21000)
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
		addPWGMediaPPDName(ppd, x, y)
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

func addPWGMediaPPDName(name string, x, y int) {
	if strings.TrimSpace(name) == "" {
		return
	}
	dimsKey := mediaDimsKey(x, y)
	if pwgMediaPPDByDims[dimsKey] == "" {
		pwgMediaPPDByDims[dimsKey] = name
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

func collectionIntOk(col goipp.Collection, name string) (int, bool) {
	for _, attr := range col {
		if attr.Name != name || len(attr.Values) == 0 {
			continue
		}
		if v, ok := attr.Values[0].V.(goipp.Integer); ok {
			return int(v), true
		}
	}
	return 0, false
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

func finishingsTemplatesFromPPD(ppd *config.PPD) ([]string, string) {
	if ppd == nil || len(ppd.OptionDetails) == 0 {
		return nil, ""
	}
	var opt *config.PPDOption
	for key, o := range ppd.OptionDetails {
		if strings.EqualFold(key, "cupsFinishingTemplate") {
			opt = o
			break
		}
	}
	if opt == nil {
		return nil, ""
	}
	templates := []string{}
	appendTemplate := func(val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		templates = append(templates, val)
	}
	if len(opt.Choices) > 0 {
		for _, c := range opt.Choices {
			appendTemplate(c.Choice)
		}
	} else if vals, ok := ppd.Options[opt.Keyword]; ok {
		for _, v := range vals {
			appendTemplate(v)
		}
	}
	def := strings.TrimSpace(opt.Default)
	if def == "" {
		def = strings.TrimSpace(ppd.Defaults[opt.Keyword])
	}
	return normalizeFinishingsTemplates(templates), def
}

func printQualitySupportedFromPPD(ppd *config.PPD) []int {
	if ppd == nil {
		return []int{4}
	}
	if opts, ok := ppd.Options["OutputMode"]; ok && len(opts) > 0 {
		return printQualityFromOptions(opts)
	}
	if opts, ok := ppd.Options["cupsPrintQuality"]; ok && len(opts) > 0 {
		return printQualityFromOptions(opts)
	}
	if len(ppd.Presets) > 0 {
		qualities := []int{}
		hasDraft := false
		for _, preset := range ppd.Presets {
			if strings.Contains(strings.ToLower(preset.Name), "draft") {
				hasDraft = true
				break
			}
		}
		if hasDraft {
			qualities = append(qualities, 3)
		}
		qualities = append(qualities, 4, 5)
		return qualities
	}
	return []int{4}
}

func printQualityFromOptions(opts []string) []int {
	qualities := []int{}
	hasDraft := false
	hasHigh := false
	for _, opt := range opts {
		switch strings.ToLower(strings.TrimSpace(opt)) {
		case "draft", "fast":
			hasDraft = true
		case "high", "best":
			hasHigh = true
		}
	}
	if hasDraft {
		qualities = append(qualities, 3)
	}
	qualities = append(qualities, 4)
	if hasHigh {
		qualities = append(qualities, 5)
	}
	return qualities
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

func parsePrintQualityValue(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(value); err == nil {
		if n == 3 || n == 4 || n == 5 {
			return n, true
		}
	}
	switch strings.ToLower(value) {
	case "draft", "low", "fast":
		return 3, true
	case "high", "best":
		return 5, true
	case "normal":
		return 4, true
	}
	return 0, false
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

func finishingsEnumFromPPD(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if n := finishingsNameToEnum(value); n > 0 {
		return n
	}
	if n := finishingsNameToEnum(pwgUnppdizeName(value, "_")); n > 0 {
		return n
	}
	return 0
}

func hasUpperASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch >= 'A' && ch <= 'Z' {
			return true
		}
	}
	return false
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
	for _, v := range finishings {
		if name := finishingsEnumToName(v); name != "" {
			templates = append(templates, name)
		}
	}
	return normalizeFinishingsTemplates(templates)
}

func finishingsTemplateForValues(finishings []int) string {
	template := ""
	for _, v := range finishings {
		if name := finishingsEnumToName(v); name != "" {
			if !strings.EqualFold(name, "none") {
				return name
			}
			if template == "" {
				template = name
			}
		}
	}
	if template == "" {
		template = "none"
	}
	return template
}

func finishingTemplateTag(value string) goipp.Tag {
	value = strings.TrimSpace(value)
	if value == "" {
		return goipp.TagKeyword
	}
	if strings.Contains(value, " ") || hasUpperASCII(value) {
		return goipp.TagName
	}
	return goipp.TagKeyword
}

func normalizeFinishingsTemplates(templates []string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		key := strings.ToLower(val)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, val)
	}
	add("none")
	for _, t := range templates {
		add(t)
	}
	return out
}

func lookupFinishings() {
	finishingsOnce.Do(func() {
		finishingsAll = []int{}
		for i := range ippFinishingsNames {
			finishingsAll = append(finishingsAll, i+3)
		}
	})
}

func makeJobPresetsSupportedAttr(ppd *config.PPD) (goipp.Attribute, bool) {
	presets := jobPresetsSupportedFromPPD(ppd)
	if len(presets) == 0 {
		return goipp.Attribute{}, false
	}
	vals := make([]goipp.Value, 0, len(presets))
	for _, col := range presets {
		vals = append(vals, col)
	}
	return goipp.MakeAttr("job-presets-supported", goipp.TagBeginCollection, vals[0], vals[1:]...), true
}

func jobPresetsSupportedFromPPD(ppd *config.PPD) []goipp.Collection {
	if ppd == nil || len(ppd.Presets) == 0 {
		return nil
	}
	out := make([]goipp.Collection, 0, len(ppd.Presets))
	for _, preset := range ppd.Presets {
		name := strings.TrimSpace(preset.Name)
		if name == "" {
			continue
		}
		col := goipp.Collection{}
		col.Add(goipp.MakeAttribute("preset-name", goipp.TagName, goipp.String(name)))

		var finishings []int
		var mediaName string
		var presetMediaSize mediaSize
		var hasMediaSize bool
		var mediaSource string
		var mediaType string

		for _, opt := range preset.Options {
			key := strings.TrimSpace(opt.Option)
			value := strings.TrimSpace(opt.Value)
			if key == "" {
				continue
			}
			switch key {
			case "*Booklet":
				if n := finishingsNameToEnum("booklet-maker"); n > 0 {
					finishings = append(finishings, n)
				}
			case "*ColorModel":
				if strings.EqualFold(value, "Gray") {
					col.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("monochrome")))
				} else if value != "" {
					col.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("color")))
				}
			case "*FoldType", "*PunchMedia", "*StapleLocation":
				if n := finishingsEnumFromPPD(value); n > 0 {
					finishings = append(finishings, n)
				}
			case "*cupsFinishingTemplate":
				if value != "" {
					finishingsCol := goipp.Collection{}
					tag := goipp.TagKeyword
					if strings.Contains(value, " ") || hasUpperASCII(value) {
						tag = goipp.TagName
					}
					finishingsCol.Add(goipp.MakeAttribute("finishing-template", tag, goipp.String(value)))
					col.Add(goipp.MakeAttribute("finishings-col", goipp.TagBeginCollection, finishingsCol))
				}
			case "*OutputBin":
				if mapped := pwgOutputBinFromPPD(ppd, value); mapped != "" {
					col.Add(goipp.MakeAttribute("output-bin", goipp.TagKeyword, goipp.String(mapped)))
				}
			case "*InputSlot":
				mediaSource = pwgMediaSourceFromPPDValue(value)
			case "*MediaType":
				mediaType = pwgMediaTypeFromPPD(ppd, value)
			case "*PageSize":
				if value != "" {
					if mapped := ppdMediaToPWG(ppd, value); mapped != "" {
						mediaName = mapped
					} else {
						mediaName = pwgUnppdizeName(value, "_.")
					}
					if size, ok := ppdPageSizeForName(ppd, value); ok && size.Width > 0 && size.Length > 0 {
						presetMediaSize = mediaSize{X: size.Width, Y: size.Length}
						hasMediaSize = true
					} else if mediaName != "" {
						if size, ok := lookupMediaSize(mediaName); ok && size.X > 0 && size.Y > 0 {
							presetMediaSize = size
							hasMediaSize = true
						}
					}
				}
			case "*cupsPrintQuality":
				if value != "" {
					switch strings.ToLower(value) {
					case "draft":
						col.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(3)))
					case "high":
						col.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(5)))
					default:
						col.Add(goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))
					}
				}
			case "*Duplex":
				switch strings.ToLower(value) {
				case "none":
					col.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("one-sided")))
				case "duplexnotumble":
					col.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("two-sided-long-edge")))
				case "duplextumble":
					col.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("two-sided-short-edge")))
				}
			default:
				key = strings.TrimPrefix(key, "*")
				if key != "" && value != "" {
					col.Add(goipp.MakeAttribute(key, goipp.TagKeyword, goipp.String(value)))
				}
			}
		}

		if len(finishings) > 0 {
			col.Add(makeEnumsAttr("finishings", finishings))
		}

		if mediaName != "" || mediaSource != "" || mediaType != "" {
			if mediaName != "" && mediaSource == "" && mediaType == "" {
				col.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(mediaName)))
			} else if hasMediaSize {
				mediaCol := goipp.Collection{}
				sizeCol := goipp.Collection{}
				sizeCol.Add(goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(presetMediaSize.X)))
				sizeCol.Add(goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(presetMediaSize.Y)))
				mediaCol.Add(goipp.MakeAttribute("media-size", goipp.TagBeginCollection, sizeCol))
				if mediaSource != "" {
					mediaCol.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(mediaSource)))
				}
				if mediaType != "" {
					mediaCol.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(mediaType)))
				}
				col.Add(goipp.MakeAttribute("media-col", goipp.TagBeginCollection, mediaCol))
			} else if mediaName != "" {
				col.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(mediaName)))
			}
		}

		out = append(out, col)
	}
	return out
}

type ppdCachePayload struct {
	Size    string   `json:"size,omitempty"`
	MTime   string   `json:"mtime,omitempty"`
	Formats []string `json:"formats,omitempty"`
}

func supportedMimeDB() *config.MimeDB {
	_ = supportedDocumentFormats()
	return mimeDB
}

func supportedDocumentFormats() []string {
	mimeOnce.Do(func() {
		mimeTypes = nil
		mimeExt = map[string]string{}
		db, err := config.LoadMimeDB(appConfig().ConfDir)
		if err != nil || db == nil {
			mimeDB = nil
			mimeTypes = []string{"application/octet-stream", "application/vnd.cups-raw"}
			return
		}
		mimeDB = db
		for ext, mt := range db.ExtToType {
			ext = strings.TrimSpace(ext)
			mt = strings.TrimSpace(mt)
			if ext == "" || mt == "" {
				continue
			}
			mimeExt[strings.ToLower(ext)] = strings.ToLower(mt)
		}
		all := make([]string, 0, len(db.Types)+2)
		for mt := range db.Types {
			all = append(all, mt)
		}
		mimeTypes = normalizeDocumentFormats(all)
	})
	return cloneStringSlice(mimeTypes)
}

func normalizeDocumentFormats(formats []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(formats)+2)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, mt := range formats {
		add(mt)
	}
	add("application/octet-stream")
	add("application/vnd.cups-raw")
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"application/octet-stream", "application/vnd.cups-raw"}
	}
	return out
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func ppdFormatCacheKey(ppdName, ppdHash string) string {
	return strings.ToLower(strings.TrimSpace(ppdName)) + "|" + strings.TrimSpace(ppdHash)
}

func parsePPDCachePayload(raw string) ppdCachePayload {
	payload := ppdCachePayload{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return payload
	}
	if err := json.Unmarshal([]byte(raw), &payload); err == nil {
		payload.Size = strings.TrimSpace(payload.Size)
		payload.MTime = strings.TrimSpace(payload.MTime)
		if len(payload.Formats) > 0 {
			payload.Formats = normalizeDocumentFormats(payload.Formats)
		}
		return payload
	}
	legacy := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return ppdCachePayload{}
	}
	if size, ok := legacy["size"]; ok {
		payload.Size = strings.TrimSpace(fmt.Sprint(size))
	}
	if mtime, ok := legacy["mtime"]; ok {
		payload.MTime = strings.TrimSpace(fmt.Sprint(mtime))
	}
	if formats, ok := legacy["formats"]; ok {
		switch v := formats.(type) {
		case []any:
			items := make([]string, 0, len(v))
			for _, item := range v {
				items = append(items, fmt.Sprint(item))
			}
			if len(items) > 0 {
				payload.Formats = normalizeDocumentFormats(items)
			}
		case []string:
			if len(v) > 0 {
				payload.Formats = normalizeDocumentFormats(v)
			}
		}
	}
	return payload
}

func marshalPPDCachePayload(payload ppdCachePayload) string {
	payload.Size = strings.TrimSpace(payload.Size)
	payload.MTime = strings.TrimSpace(payload.MTime)
	if len(payload.Formats) > 0 {
		payload.Formats = normalizeDocumentFormats(payload.Formats)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(raw)
}

func ppdFileCacheMetadata(ppdPath string) (hash, size, mtime string, ok bool) {
	info, err := os.Stat(ppdPath)
	if err != nil {
		return "", "", "", false
	}
	raw, err := os.ReadFile(ppdPath)
	if err != nil {
		return "", "", "", false
	}
	hash = fmt.Sprintf("%x", md5.Sum(raw))
	size = strconv.FormatInt(info.Size(), 10)
	mtime = info.ModTime().UTC().Format(time.RFC3339Nano)
	return hash, size, mtime, true
}

func ppdFilterConvs(ppd *config.PPD) ([]config.MimeConv, map[string]bool, bool) {
	extra := []config.MimeConv{}
	destSet := map[string]bool{}
	if ppd == nil {
		return extra, destSet, false
	}
	hasFilter2 := false
	for _, f := range ppd.Filters {
		if strings.TrimSpace(f.Dest) != "" {
			hasFilter2 = true
			break
		}
	}
	hasFilters := false
	for _, f := range ppd.Filters {
		source := strings.TrimSpace(f.Source)
		dest := strings.TrimSpace(f.Dest)
		if source == "" {
			continue
		}
		if hasFilter2 && dest == "" {
			continue
		}
		extra = append(extra, config.MimeConv{
			Source:  source,
			Dest:    dest,
			Cost:    f.Cost,
			Program: f.Program,
		})
		hasFilters = true
		if dest != "" {
			destSet[dest] = true
		}
	}
	return extra, destSet, hasFilters
}

func selectMimePipeline(db *config.MimeDB, src string, extra []config.MimeConv, destSet map[string]bool) ([]config.MimeConv, string) {
	if db == nil || strings.TrimSpace(src) == "" {
		return nil, ""
	}
	candidates := make([]string, 0, len(destSet)+1)
	for dest := range destSet {
		candidates = append(candidates, dest)
	}
	candidates = append(candidates, "application/octet-stream")
	seen := map[string]bool{}
	var best []config.MimeConv
	bestCost := -1
	bestDest := ""
	for _, dest := range candidates {
		dest = strings.TrimSpace(dest)
		if dest == "" || seen[dest] {
			continue
		}
		seen[dest] = true
		convs := findMimePipeline(db, src, dest, extra)
		if len(convs) == 0 {
			continue
		}
		cost := mimePipelineCost(convs)
		if bestCost == -1 || cost < bestCost {
			bestCost = cost
			best = convs
			bestDest = dest
		}
	}
	return best, bestDest
}

func mimePipelineCost(convs []config.MimeConv) int {
	total := 0
	for _, conv := range convs {
		if conv.Cost > 0 {
			total += conv.Cost
		} else {
			total++
		}
	}
	return total
}

func filterProgramAvailable(program string) bool {
	program = strings.TrimSpace(program)
	if program == "" || program == "-" {
		return true
	}
	parts := strings.Fields(program)
	if len(parts) == 0 {
		return true
	}
	cmd := parts[0]
	if strings.Contains(cmd, "/") || strings.Contains(cmd, "\\\\") || filepath.IsAbs(cmd) {
		return executablePathExists(cmd)
	}
	if dir := strings.TrimSpace(appConfig().FilterDir); dir != "" {
		if candidate, ok := lookupExecutableInDir(dir, cmd); ok {
			return executablePathExists(candidate)
		}
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

func lookupExecutableInDir(dir, name string) (string, bool) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(name) == "" {
		return "", false
	}
	candidates := []string{filepath.Join(dir, name)}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
			candidates = append(candidates, filepath.Join(dir, name+ext))
		}
	}
	for _, candidate := range candidates {
		if executablePathExists(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func executablePathExists(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".exe", ".cmd", ".bat", ".com":
			return true
		default:
			return false
		}
	}
	return info.Mode()&0111 != 0
}

func findMimePipeline(db *config.MimeDB, src, dst string, extra []config.MimeConv) []config.MimeConv {
	if db == nil || src == "" || dst == "" {
		return nil
	}
	if src == dst {
		return nil
	}
	type edge struct {
		to   string
		conv config.MimeConv
	}
	graph := map[string][]edge{}
	allConvs := append([]config.MimeConv{}, db.Convs...)
	if len(extra) > 0 {
		allConvs = append(allConvs, extra...)
	}
	for _, conv := range allConvs {
		source := strings.TrimSpace(conv.Source)
		dest := strings.TrimSpace(conv.Dest)
		if source == "" {
			continue
		}
		if !filterProgramAvailable(conv.Program) {
			continue
		}
		if dest == "" {
			dest = dst
		}
		if dest == "" {
			continue
		}
		convCopy := conv
		convCopy.Source = source
		convCopy.Dest = dest
		graph[source] = append(graph[source], edge{to: dest, conv: convCopy})
	}
	dist := map[string]int{src: 0}
	prev := map[string]edge{}
	visited := map[string]bool{}
	queue := []string{src}
	for len(queue) > 0 {
		sort.Slice(queue, func(i, j int) bool { return dist[queue[i]] < dist[queue[j]] })
		node := queue[0]
		queue = queue[1:]
		if visited[node] {
			continue
		}
		visited[node] = true
		if node == dst {
			break
		}
		for _, e := range graph[node] {
			if visited[e.to] {
				continue
			}
			cost := dist[node] + e.conv.Cost
			if cur, ok := dist[e.to]; !ok || cost < cur {
				dist[e.to] = cost
				prev[e.to] = e
				queue = append(queue, e.to)
			}
		}
	}
	if _, ok := dist[dst]; !ok {
		return nil
	}
	path := []config.MimeConv{}
	cur := dst
	for cur != src {
		e, ok := prev[cur]
		if !ok {
			return nil
		}
		path = append([]config.MimeConv{e.conv}, path...)
		cur = e.conv.Source
	}
	return path
}

func canAcceptDocumentFormat(format string, db *config.MimeDB, extra []config.MimeConv, destSet map[string]bool) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" || format == "application/octet-stream" || format == "application/vnd.cups-raw" {
		return true
	}
	for dest := range destSet {
		if strings.EqualFold(dest, format) {
			return true
		}
	}
	convs, _ := selectMimePipeline(db, format, extra, destSet)
	return len(convs) > 0
}

func deriveDocumentFormatsForPPD(base []string, db *config.MimeDB, ppd *config.PPD) []string {
	base = normalizeDocumentFormats(base)
	extra, destSet, hasFilters := ppdFilterConvs(ppd)
	if !hasFilters {
		return normalizeDocumentFormats(nil)
	}
	if db == nil {
		fallback := make([]string, 0, len(extra))
		for _, conv := range extra {
			fallback = append(fallback, conv.Source)
		}
		return normalizeDocumentFormats(fallback)
	}
	formats := make([]string, 0, len(base))
	for _, format := range base {
		if canAcceptDocumentFormat(format, db, extra, destSet) {
			formats = append(formats, format)
		}
	}
	return normalizeDocumentFormats(formats)
}

func loadCachedDocumentFormats(ppdName, ppdHash string) []string {
	ppdName = strings.TrimSpace(ppdName)
	ppdHash = strings.TrimSpace(ppdHash)
	if ppdName == "" || ppdHash == "" {
		return nil
	}
	st := appStore()
	if st == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	formats := []string{}
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		cachedHash, cachedAttrs, _, ok, err := st.GetPPDCache(ctx, tx, ppdName)
		if err != nil || !ok || cachedHash != ppdHash {
			return nil
		}
		payload := parsePPDCachePayload(cachedAttrs)
		if len(payload.Formats) == 0 {
			return nil
		}
		formats = normalizeDocumentFormats(payload.Formats)
		return nil
	})
	return formats
}

func storeCachedDocumentFormats(ppdName, ppdHash, size, mtime string, formats []string) {
	ppdName = strings.TrimSpace(ppdName)
	ppdHash = strings.TrimSpace(ppdHash)
	if ppdName == "" || ppdHash == "" {
		return
	}
	st := appStore()
	if st == nil {
		return
	}
	payload := ppdCachePayload{
		Size:    strings.TrimSpace(size),
		MTime:   strings.TrimSpace(mtime),
		Formats: normalizeDocumentFormats(formats),
	}
	raw := marshalPPDCachePayload(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = st.WithTx(ctx, false, func(tx *sql.Tx) error {
		return st.SetPPDCache(ctx, tx, ppdName, ppdHash, raw)
	})
}

func supportedDocumentFormatsForPPD(ppdName, ppdPath string, ppd *config.PPD) []string {
	base := supportedDocumentFormats()
	if ppd == nil {
		return base
	}
	hash, size, mtime, ok := ppdFileCacheMetadata(ppdPath)
	if ok {
		cacheKey := ppdFormatCacheKey(ppdName, hash)
		if cached, found := printerFormatOnce.Load(cacheKey); found {
			if values, valid := cached.([]string); valid && len(values) > 0 {
				return cloneStringSlice(values)
			}
		}
		if cached := loadCachedDocumentFormats(ppdName, hash); len(cached) > 0 {
			printerFormatOnce.Store(cacheKey, cloneStringSlice(cached))
			return cached
		}
	}
	derived := deriveDocumentFormatsForPPD(base, supportedMimeDB(), ppd)
	if ok {
		cacheKey := ppdFormatCacheKey(ppdName, hash)
		printerFormatOnce.Store(cacheKey, cloneStringSlice(derived))
		storeCachedDocumentFormats(ppdName, hash, size, mtime, derived)
	}
	return derived
}

func supportedDocumentFormatsForPrinter(printer model.Printer, ppd *config.PPD) []string {
	ppdName := strings.TrimSpace(printer.PPDName)
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}
	ppdPath := safePPDPath(appConfig().PPDDir, ppdName)
	if _, err := os.Stat(ppdPath); err != nil && ppdName != model.DefaultPPDName {
		ppdName = model.DefaultPPDName
		ppdPath = safePPDPath(appConfig().PPDDir, ppdName)
	}
	return supportedDocumentFormatsForPPD(ppdName, ppdPath, ppd)
}

func preferredDocumentFormat(formats []string) string {
	if stringInList("application/pdf", formats) {
		return "application/pdf"
	}
	if stringInList("image/urf", formats) {
		return "image/urf"
	}
	if len(formats) > 0 {
		return formats[0]
	}
	return "application/octet-stream"
}

func detectDocumentFormat(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	_ = supportedDocumentFormats()
	if len(mimeExt) == 0 {
		return ""
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return ""
	}
	if mt := mimeExt[ext]; mt != "" {
		return mt
	}
	return ""
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
		goipp.OpCancelJob,
		goipp.OpGetJobAttributes,
		goipp.OpGetJobs,
		goipp.OpGetPrinterAttributes,
		goipp.OpHoldJob,
		goipp.OpReleaseJob,
		goipp.OpPausePrinter,
		goipp.OpResumePrinter,
		goipp.OpPurgeJobs,
		goipp.OpSetPrinterAttributes,
		goipp.OpSetJobAttributes,
		goipp.OpGetPrinterSupportedValues,
		goipp.OpCreatePrinterSubscriptions,
		goipp.OpCreateJobSubscriptions,
		goipp.OpGetSubscriptionAttributes,
		goipp.OpGetSubscriptions,
		goipp.OpRenewSubscription,
		goipp.OpCancelSubscription,
		goipp.OpGetNotifications,
		goipp.OpEnablePrinter,
		goipp.OpDisablePrinter,
		goipp.OpHoldNewJobs,
		goipp.OpReleaseHeldNewJobs,
		goipp.OpCancelJobs,
		goipp.OpCancelMyJobs,
		goipp.OpCloseJob,
		goipp.OpCupsGetDefault,
		goipp.OpCupsGetPrinters,
		goipp.OpCupsAddModifyPrinter,
		goipp.OpCupsDeletePrinter,
		goipp.OpCupsGetClasses,
		goipp.OpCupsAddModifyClass,
		goipp.OpCupsDeleteClass,
		goipp.OpCupsAcceptJobs,
		goipp.OpCupsRejectJobs,
		goipp.OpCupsSetDefault,
		goipp.OpCupsGetDevices,
		goipp.OpCupsGetPpds,
		goipp.OpCupsMoveJob,
		goipp.OpCupsAuthenticateJob,
		goipp.OpCupsGetPpd,
		goipp.OpCupsGetDocument,
		goipp.OpRestartJob,
	}
	out := make([]int, 0, len(ops))
	for _, op := range ops {
		out = append(out, int(op))
	}
	return out
}

const ippIntMax = 2147483647

func kOctetsSupported(cfg config.Config) int {
	path := strings.TrimSpace(cfg.RequestRoot)
	if path == "" {
		path = strings.TrimSpace(cfg.SpoolDir)
	}
	if path == "" {
		path = strings.TrimSpace(cfg.DataDir)
	}
	if path == "" {
		return ippIntMax
	}
	if size, ok := diskKOctets(path); ok && size > 0 {
		if size > ippIntMax {
			return ippIntMax
		}
		return int(size)
	}
	return ippIntMax
}

func notifySchemesSupported(cfg config.Config) []string {
	base := strings.TrimSpace(cfg.ServerBin)
	if base == "" {
		return nil
	}
	dir := filepath.Join(base, "notifier")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if !isNotifierExecutable(info, name) {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func isNotifierExecutable(info os.FileInfo, name string) bool {
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".exe", ".bat", ".cmd", ".com":
			return true
		default:
			return false
		}
	}
	return info.Mode()&0111 != 0
}

func leaseDurationUpper(cfg config.Config) int {
	if cfg.MaxLeaseDuration > 0 {
		return cfg.MaxLeaseDuration
	}
	return ippIntMax
}

func clampLeaseDuration(lease int64, cfg config.Config) int64 {
	if cfg.MaxLeaseDuration > 0 && (lease == 0 || lease > int64(cfg.MaxLeaseDuration)) {
		return int64(cfg.MaxLeaseDuration)
	}
	return lease
}

func isDocumentFormatSupportedInList(formats []string, format string) bool {
	format = strings.TrimSpace(format)
	if format == "" || strings.EqualFold(format, "application/octet-stream") || strings.EqualFold(format, "application/vnd.cups-raw") {
		return true
	}
	for _, mt := range formats {
		if strings.EqualFold(mt, format) {
			return true
		}
	}
	return false
}

func isDocumentFormatSupportedForPrinter(printer model.Printer, format string) bool {
	ppd, _ := loadPPDForPrinter(printer)
	return isDocumentFormatSupportedInList(supportedDocumentFormatsForPrinter(printer, ppd), format)
}

func isDocumentFormatSupported(format string) bool {
	return isDocumentFormatSupportedInList(supportedDocumentFormats(), format)
}

func jobSettableAttributesSupported() []string {
	return []string{
		"copies", "finishings", "job-cancel-after", "job-hold-until", "job-name", "job-priority",
		"job-sheets", "job-sheets-col", "media", "media-col", "multiple-document-handling",
		"number-up", "number-up-layout", "orientation-requested", "output-bin", "page-delivery",
		"page-ranges", "print-as-raster", "print-color-mode", "print-quality", "print-scaling",
		"printer-resolution", "sides",
	}
}

func printerSettableAttributesSupported() []string {
	return []string{
		"job-cancel-after-default", "job-hold-until-default", "job-priority-default", "job-sheets-default",
		"media-default", "media-col-default", "media-source-default", "media-type-default",
		"number-up-default", "orientation-requested-default", "output-bin-default", "output-mode-default",
		"port-monitor", "print-color-mode-default", "print-quality-default", "printer-error-policy",
		"printer-geo-location", "printer-info", "printer-is-shared", "printer-location",
		"printer-op-policy", "printer-organization", "printer-organizational-unit", "printer-resolution-default",
	}
}

func printerSettableAttributesForDestination(isClass bool) []string {
	keys := make([]string, 0, len(printerSettableAttributesSupported()))
	for _, key := range printerSettableAttributesSupported() {
		if isClass && strings.EqualFold(key, "printer-is-shared") {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func supportedValueAttributes(printer model.Printer, isClass bool) goipp.Attributes {
	_ = printer
	attrs := goipp.Attributes{}
	// CUPS returns admin-defined integer values (0) to indicate settable
	// attributes for Set-Printer-Attributes.
	for _, key := range printerSettableAttributesForDestination(isClass) {
		attrs.Add(goipp.MakeAttribute(key, goipp.TagAdminDefine, goipp.Integer(0)))
	}
	return attrs
}

var (
	jobDescriptionAttrs = map[string]bool{
		"job-id": true, "job-uri": true, "job-name": true, "job-state": true,
		"job-state-reasons": true, "job-state-message": true, "job-originating-user-name": true,
		"job-originating-user-uri": true, "job-originating-host-name": true,
		"job-printer-uri": true, "job-printer-state-message": true, "job-printer-state-reasons": true,
		"job-printer-up-time": true,
		"time-at-creation":    true, "time-at-processing": true, "time-at-completed": true,
		"date-time-at-creation": true, "date-time-at-processing": true, "date-time-at-completed": true,
		"job-impressions": true, "job-impressions-completed": true,
		"job-priority-actual": true, "job-hold-until-actual": true,
		"job-account-id-actual": true, "job-accounting-user-id-actual": true,
		"job-k-octets": true, "job-k-octets-processed": true,
		"job-pages": true, "job-pages-completed": true,
		"job-media-sheets": true, "job-media-sheets-completed": true, "job-more-info": true,
		"job-uuid": true, "number-of-documents": true, "number-of-intervening-jobs": true,
		"output-device-assigned": true, "original-requesting-user-name": true, "job-processing-time": true,
		"document-format-supplied": true, "document-name-supplied": true,
		"document-charset-supplied": true, "document-natural-language-supplied": true, "compression-supplied": true,
		"job-attribute-fidelity": true,
		"copies-actual":          true, "finishings-actual": true, "finishings-col-actual": true,
		"job-sheets-actual": true, "job-sheets-col-actual": true, "media-actual": true,
		"media-col-actual": true, "number-up-actual": true, "orientation-requested-actual": true,
		"output-bin-actual": true, "page-delivery-actual": true, "page-ranges-actual": true,
		"print-color-mode-actual": true, "print-quality-actual": true, "print-scaling-actual": true,
		"printer-resolution-actual": true, "sides-actual": true,
	}
	jobTemplateAttrs = map[string]bool{
		"accuracy-units-supported":                   true,
		"baling-type-supported":                      true,
		"baling-when-supported":                      true,
		"binding-reference-edge-supported":           true,
		"binding-type-supported":                     true,
		"chamber-humidity":                           true,
		"chamber-humidity-default":                   true,
		"chamber-humidity-supported":                 true,
		"chamber-temperature":                        true,
		"chamber-temperature-default":                true,
		"chamber-temperature-supported":              true,
		"coating-sides-supported":                    true,
		"coating-type-supported":                     true,
		"confirmation-sheet-print":                   true,
		"confirmation-sheet-print-default":           true,
		"copies":                                     true,
		"copies-default":                             true,
		"copies-supported":                           true,
		"cover-back":                                 true,
		"cover-back-default":                         true,
		"cover-back-supported":                       true,
		"cover-front":                                true,
		"cover-front-default":                        true,
		"cover-front-supported":                      true,
		"cover-sheet-info":                           true,
		"cover-sheet-info-default":                   true,
		"cover-sheet-info-supported":                 true,
		"covering-name-supported":                    true,
		"destination-uri-schemes-supported":          true,
		"destination-uris":                           true,
		"destination-uris-supported":                 true,
		"feed-orientation":                           true,
		"feed-orientation-default":                   true,
		"feed-orientation-supported":                 true,
		"finishings":                                 true,
		"finishings-col":                             true,
		"finishings-col-database":                    true,
		"finishings-col-default":                     true,
		"finishings-col-ready":                       true,
		"finishings-col-supported":                   true,
		"finishings-default":                         true,
		"finishings-ready":                           true,
		"finishings-supported":                       true,
		"folding-direction-supported":                true,
		"folding-offset-supported":                   true,
		"folding-reference-edge-supported":           true,
		"force-front-side":                           true,
		"force-front-side-default":                   true,
		"force-front-side-supported":                 true,
		"imposition-template":                        true,
		"imposition-template-default":                true,
		"imposition-template-supported":              true,
		"insert-count-supported":                     true,
		"insert-sheet":                               true,
		"insert-sheet-default":                       true,
		"insert-sheet-supported":                     true,
		"job-account-id":                             true,
		"job-account-id-default":                     true,
		"job-account-id-supported":                   true,
		"job-accounting-output-bin-supported":        true,
		"job-accounting-sheets":                      true,
		"job-accounting-sheets-default":              true,
		"job-accounting-sheets-supported":            true,
		"job-accounting-sheets-type-supported":       true,
		"job-accounting-user-id":                     true,
		"job-accounting-user-id-default":             true,
		"job-accounting-user-id-supported":           true,
		"job-cancel-after":                           true,
		"job-cancel-after-default":                   true,
		"job-cancel-after-supported":                 true,
		"job-complete-before":                        true,
		"job-complete-before-supported":              true,
		"job-complete-before-time":                   true,
		"job-complete-before-time-supported":         true,
		"job-delay-output-until":                     true,
		"job-delay-output-until-default":             true,
		"job-delay-output-until-supported":           true,
		"job-delay-output-until-time":                true,
		"job-delay-output-until-time-default":        true,
		"job-delay-output-until-time-supported":      true,
		"job-error-action":                           true,
		"job-error-action-default":                   true,
		"job-error-action-supported":                 true,
		"job-error-sheet":                            true,
		"job-error-sheet-default":                    true,
		"job-error-sheet-supported":                  true,
		"job-error-sheet-type-supported":             true,
		"job-error-sheet-when-supported":             true,
		"job-hold-until":                             true,
		"job-hold-until-default":                     true,
		"job-hold-until-supported":                   true,
		"job-hold-until-time":                        true,
		"job-hold-until-time-supported":              true,
		"job-message-to-operator":                    true,
		"job-message-to-operator-supported":          true,
		"job-phone-number":                           true,
		"job-phone-number-default":                   true,
		"job-phone-number-supported":                 true,
		"job-priority":                               true,
		"job-priority-default":                       true,
		"job-priority-supported":                     true,
		"job-recipient-name":                         true,
		"job-recipient-name-supported":               true,
		"job-retain-until":                           true,
		"job-retain-until-default":                   true,
		"job-retain-until-interval":                  true,
		"job-retain-until=interval-default":          true,
		"job-retain-until-interval-supported":        true,
		"job-retain-until-supported":                 true,
		"job-retain-until-time":                      true,
		"job-retain-until-time-supported":            true,
		"job-sheet-message":                          true,
		"job-sheet-message-supported":                true,
		"job-sheets":                                 true,
		"job-sheets-col":                             true,
		"job-sheets-col-default":                     true,
		"job-sheets-col-supported":                   true,
		"job-sheets-default":                         true,
		"job-sheets-supported":                       true,
		"laminating-sides-supported":                 true,
		"laminating-type-supported":                  true,
		"logo-uri-schemes-supported":                 true,
		"material-amount-units-supported":            true,
		"material-diameter-supported":                true,
		"material-purpose-supported":                 true,
		"material-rate-supported":                    true,
		"material-rate-units-supported":              true,
		"material-shell-thickness-supported":         true,
		"material-temperature-supported":             true,
		"material-type-supported":                    true,
		"materials-col":                              true,
		"materials-col-database":                     true,
		"materials-col-default":                      true,
		"materials-col-ready":                        true,
		"materials-col-supported":                    true,
		"max-materials-col-supported":                true,
		"max-page-ranges-supported":                  true,
		"max-stitching-locations-supported":          true,
		"media":                                      true,
		"media-back-coating-supported":               true,
		"media-bottom-margin-supported":              true,
		"media-col":                                  true,
		"media-col-default":                          true,
		"media-col-ready":                            true,
		"media-col-supported":                        true,
		"media-color-supported":                      true,
		"media-default":                              true,
		"media-front-coating-supported":              true,
		"media-grain-supported":                      true,
		"media-hole-count-supported":                 true,
		"media-info-supported":                       true,
		"media-input-tray-check":                     true,
		"media-input-tray-check-default":             true,
		"media-input-tray-check-supported":           true,
		"media-key-supported":                        true,
		"media-left-margin-supported":                true,
		"media-order-count-supported":                true,
		"media-overprint":                            true,
		"media-overprint-distance-supported":         true,
		"media-overprint-method-supported":           true,
		"media-overprint-supported":                  true,
		"media-pre-printed-supported":                true,
		"media-ready":                                true,
		"media-recycled-supported":                   true,
		"media-right-margin-supported":               true,
		"media-size-supported":                       true,
		"media-source-supported":                     true,
		"media-supported":                            true,
		"media-thickness-supported":                  true,
		"media-top-margin-supported":                 true,
		"media-type-supported":                       true,
		"media-weight-metric-supported":              true,
		"multiple-document-handling":                 true,
		"multiple-document-handling-default":         true,
		"multiple-document-handling-supported":       true,
		"multiple-object-handling":                   true,
		"multiple-object-handling-default":           true,
		"multiple-object-handling-supported":         true,
		"number-of-retries":                          true,
		"number-of-retries-default":                  true,
		"number-of-retries-supported":                true,
		"number-up":                                  true,
		"number-up-default":                          true,
		"number-up-supported":                        true,
		"orientation-requested":                      true,
		"orientation-requested-default":              true,
		"orientation-requested-supported":            true,
		"output-bin":                                 true,
		"output-bin-default":                         true,
		"output-bin-supported":                       true,
		"output-device":                              true,
		"output-device-supported":                    true,
		"output-mode":                                true,
		"output-mode-default":                        true,
		"output-mode-supported":                      true,
		"overrides":                                  true,
		"overrides-supported":                        true,
		"page-delivery":                              true,
		"page-delivery-default":                      true,
		"page-delivery-supported":                    true,
		"page-ranges":                                true,
		"page-ranges-supported":                      true,
		"platform-temperature":                       true,
		"platform-temperature-default":               true,
		"platform-temperature-supported":             true,
		"preferred-attributes-supported":             true,
		"presentation-direction-number-up":           true,
		"presentation-direction-number-up-default":   true,
		"presentation-direction-number-up-supported": true,
		"print-accuracy":                             true,
		"print-accuracy-default":                     true,
		"print-accuracy-supported":                   true,
		"print-base":                                 true,
		"print-base-default":                         true,
		"print-base-supported":                       true,
		"print-color-mode":                           true,
		"print-color-mode-default":                   true,
		"print-color-mode-supported":                 true,
		"print-content-optimize":                     true,
		"print-content-optimize-default":             true,
		"print-content-optimize-supported":           true,
		"print-objects":                              true,
		"print-objects-default":                      true,
		"print-objects-supported":                    true,
		"print-processing-attributes-supported":      true,
		"print-quality":                              true,
		"print-quality-default":                      true,
		"print-quality-supported":                    true,
		"print-rendering-intent":                     true,
		"print-rendering-intent-default":             true,
		"print-rendering-intent-supported":           true,
		"print-scaling":                              true,
		"print-scaling-default":                      true,
		"print-scaling-supported":                    true,
		"print-supports":                             true,
		"print-supports-default":                     true,
		"print-supports-supported":                   true,
		"printer-resolution":                         true,
		"printer-resolution-default":                 true,
		"printer-resolution-supported":               true,
		"proof-copies":                               true,
		"proof-copies-supported":                     true,
		"proof-print":                                true,
		"proof-print-default":                        true,
		"proof-print-supported":                      true,
		"punching-hole-diameter-configured":          true,
		"punching-locations-supported":               true,
		"punching-offset-supported":                  true,
		"punching-reference-edge-supported":          true,
		"retry-interval":                             true,
		"retry-interval-default":                     true,
		"retry-interval-supported":                   true,
		"retry-timeout":                              true,
		"retry-timeout-default":                      true,
		"retry-timeout-supported":                    true,
		"separator-sheets":                           true,
		"separator-sheets-default":                   true,
		"separator-sheets-supported":                 true,
		"separator-sheets-type-supported":            true,
		"sides":                                      true,
		"sides-default":                              true,
		"sides-supported":                            true,
		"stitching-angle-supported":                  true,
		"stitching-locations-supported":              true,
		"stitching-method-supported":                 true,
		"stitching-offset-supported":                 true,
		"stitching-reference-edge-supported":         true,
		"x-image-position":                           true,
		"x-image-position-default":                   true,
		"x-image-position-supported":                 true,
		"x-image-shift":                              true,
		"x-image-shift-default":                      true,
		"x-image-shift-supported":                    true,
		"x-side1-image-shift":                        true,
		"x-side1-image-shift-default":                true,
		"x-side1-image-shift-supported":              true,
		"x-side2-image-shift":                        true,
		"x-side2-image-shift-default":                true,
		"x-side2-image-shift-supported":              true,
		"y-image-position":                           true,
		"y-image-position-default":                   true,
		"y-image-position-supported":                 true,
		"y-image-shift":                              true,
		"y-image-shift-default":                      true,
		"y-image-shift-supported":                    true,
		"y-side1-image-shift":                        true,
		"y-side1-image-shift-default":                true,
		"y-side1-image-shift-supported":              true,
		"y-side2-image-shift":                        true,
		"y-side2-image-shift-default":                true,
		"y-side2-image-shift-supported":              true,
	}
	jobStatusAttrs = map[string]bool{
		"job-state": true, "job-state-reasons": true, "job-state-message": true,
		"time-at-processing": true, "time-at-completed": true, "job-printer-up-time": true,
		"job-impressions": true, "job-impressions-completed": true, "job-k-octets-completed": true,
		"job-pages-completed": true, "job-media-sheets-completed": true,
	}
	documentDescriptionAttrs = map[string]bool{
		"compression":                             true,
		"copies-actual":                           true,
		"cover-back-actual":                       true,
		"cover-front-actual":                      true,
		"current-page-order":                      true,
		"date-time-at-completed":                  true,
		"date-time-at-creation":                   true,
		"date-time-at-processing":                 true,
		"detailed-status-messages":                true,
		"document-access-errors":                  true,
		"document-charset":                        true,
		"document-format":                         true,
		"document-format-details":                 true,
		"document-format-detected":                true,
		"document-job-id":                         true,
		"document-job-uri":                        true,
		"document-message":                        true,
		"document-metadata":                       true,
		"document-name":                           true,
		"document-natural-language":               true,
		"document-number":                         true,
		"document-printer-uri":                    true,
		"document-state":                          true,
		"document-state-message":                  true,
		"document-state-reasons":                  true,
		"document-uri":                            true,
		"document-uuid":                           true,
		"errors-count":                            true,
		"finishings-actual":                       true,
		"finishings-col-actual":                   true,
		"force-front-side-actual":                 true,
		"imposition-template-actual":              true,
		"impressions":                             true,
		"impressions-col":                         true,
		"impressions-completed":                   true,
		"impressions-completed-col":               true,
		"impressions-completed-current-copy":      true,
		"insert-sheet-actual":                     true,
		"k-octets":                                true,
		"k-octets-processed":                      true,
		"last-document":                           true,
		"materials-col-actual":                    true,
		"media-actual":                            true,
		"media-col-actual":                        true,
		"media-input-tray-check-actual":           true,
		"media-sheets":                            true,
		"media-sheets-col":                        true,
		"media-sheets-completed":                  true,
		"media-sheets-completed-col":              true,
		"more-info":                               true,
		"multiple-object-handling-actual":         true,
		"number-up-actual":                        true,
		"orientation-requested-actual":            true,
		"output-bin-actual":                       true,
		"output-device-assigned":                  true,
		"overrides-actual":                        true,
		"page-delivery-actual":                    true,
		"page-order-received-actual":              true,
		"page-ranges-actual":                      true,
		"pages":                                   true,
		"pages-col":                               true,
		"pages-completed":                         true,
		"pages-completed-col":                     true,
		"pages-completed-current-copy":            true,
		"platform-temperature-actual":             true,
		"presentation-direction-number-up-actual": true,
		"print-accuracy-actual":                   true,
		"print-base-actual":                       true,
		"print-color-mode-actual":                 true,
		"print-content-optimize-actual":           true,
		"print-objects-actual":                    true,
		"print-quality-actual":                    true,
		"print-rendering-intent-actual":           true,
		"print-scaling-actual":                    true,
		"print-supports-actual":                   true,
		"printer-resolution-actual":               true,
		"printer-up-time":                         true,
		"separator-sheets-actual":                 true,
		"sheet-completed-copy-number":             true,
		"sides-actual":                            true,
		"time-at-completed":                       true,
		"time-at-creation":                        true,
		"time-at-processing":                      true,
		"warnings-count":                          true,
		"x-image-position-actual":                 true,
		"x-image-shift-actual":                    true,
		"x-side1-image-shift-actual":              true,
		"x-side2-image-shift-actual":              true,
		"y-image-position-actual":                 true,
		"y-image-shift-actual":                    true,
		"y-side1-image-shift-actual":              true,
		"y-side2-image-shift-actual":              true,
	}
	documentTemplateAttrs = map[string]bool{
		"baling-type-supported":                      true,
		"baling-when-supported":                      true,
		"binding-reference-edge-supported":           true,
		"binding-type-supported":                     true,
		"chamber-humidity":                           true,
		"chamber-humidity-default":                   true,
		"chamber-humidity-supported":                 true,
		"chamber-temperature":                        true,
		"chamber-temperature-default":                true,
		"chamber-temperature-supported":              true,
		"coating-sides-supported":                    true,
		"coating-type-supported":                     true,
		"copies":                                     true,
		"copies-default":                             true,
		"copies-supported":                           true,
		"cover-back":                                 true,
		"cover-back-default":                         true,
		"cover-back-supported":                       true,
		"cover-front":                                true,
		"cover-front-default":                        true,
		"cover-front-supported":                      true,
		"covering-name-supported":                    true,
		"feed-orientation":                           true,
		"feed-orientation-default":                   true,
		"feed-orientation-supported":                 true,
		"finishing-template-supported":               true,
		"finishings":                                 true,
		"finishings-col":                             true,
		"finishings-col-database":                    true,
		"finishings-col-default":                     true,
		"finishings-col-ready":                       true,
		"finishings-col-supported":                   true,
		"finishings-default":                         true,
		"finishings-ready":                           true,
		"finishings-supported":                       true,
		"folding-direction-supported":                true,
		"folding-offset-supported":                   true,
		"folding-reference-edge-supported":           true,
		"force-front-side":                           true,
		"force-front-side-default":                   true,
		"force-front-side-supported":                 true,
		"imposition-template":                        true,
		"imposition-template-default":                true,
		"imposition-template-supported":              true,
		"insert-count-supported":                     true,
		"insert-sheet":                               true,
		"insert-sheet-default":                       true,
		"insert-sheet-supported":                     true,
		"laminating-sides-supported":                 true,
		"laminating-type-supported":                  true,
		"material-amount-units-supported":            true,
		"material-diameter-supported":                true,
		"material-purpose-supported":                 true,
		"material-rate-supported":                    true,
		"material-rate-units-supported":              true,
		"material-shell-thickness-supported":         true,
		"material-temperature-supported":             true,
		"material-type-supported":                    true,
		"materials-col":                              true,
		"materials-col-database":                     true,
		"materials-col-default":                      true,
		"materials-col-ready":                        true,
		"materials-col-supported":                    true,
		"max-materials-col-supported":                true,
		"max-page-ranges-supported":                  true,
		"max-stitching-locations-supported":          true,
		"media":                                      true,
		"media-back-coating-supported":               true,
		"media-bottom-margin-supported":              true,
		"media-col":                                  true,
		"media-col-default":                          true,
		"media-col-ready":                            true,
		"media-col-supported":                        true,
		"media-color-supported":                      true,
		"media-default":                              true,
		"media-front-coating-supported":              true,
		"media-grain-supported":                      true,
		"media-hole-count-supported":                 true,
		"media-info-supported":                       true,
		"media-input-tray-check":                     true,
		"media-input-tray-check-default":             true,
		"media-input-tray-check-supported":           true,
		"media-key-supported":                        true,
		"media-left-margin-supported":                true,
		"media-order-count-supported":                true,
		"media-overprint":                            true,
		"media-overprint-distance-supported":         true,
		"media-overprint-method-supported":           true,
		"media-overprint-supported":                  true,
		"media-pre-printed-supported":                true,
		"media-ready":                                true,
		"media-recycled-supported":                   true,
		"media-right-margin-supported":               true,
		"media-size-supported":                       true,
		"media-source-supported":                     true,
		"media-supported":                            true,
		"media-thickness-supported":                  true,
		"media-top-margin-supported":                 true,
		"media-type-supported":                       true,
		"media-weight-metric-supported":              true,
		"multiple-document-handling":                 true,
		"multiple-document-handling-default":         true,
		"multiple-document-handling-supported":       true,
		"multiple-object-handling":                   true,
		"multiple-object-handling-default":           true,
		"multiple-object-handling-supported":         true,
		"number-up":                                  true,
		"number-up-default":                          true,
		"number-up-supported":                        true,
		"orientation-requested":                      true,
		"orientation-requested-default":              true,
		"orientation-requested-supported":            true,
		"output-device":                              true,
		"output-device-supported":                    true,
		"output-mode":                                true,
		"output-mode-default":                        true,
		"output-mode-supported":                      true,
		"overrides":                                  true,
		"overrides-supported":                        true,
		"page-delivery":                              true,
		"page-delivery-default":                      true,
		"page-delivery-supported":                    true,
		"page-ranges":                                true,
		"page-ranges-supported":                      true,
		"platform-temperature":                       true,
		"platform-temperature-default":               true,
		"platform-temperature-supported":             true,
		"preferred-attributes-supported":             true,
		"presentation-direction-number-up":           true,
		"presentation-direction-number-up-default":   true,
		"presentation-direction-number-up-supported": true,
		"print-accuracy":                             true,
		"print-accuracy-default":                     true,
		"print-accuracy-supported":                   true,
		"print-base":                                 true,
		"print-base-default":                         true,
		"print-base-supported":                       true,
		"print-color-mode":                           true,
		"print-color-mode-default":                   true,
		"print-color-mode-supported":                 true,
		"print-content-optimize":                     true,
		"print-content-optimize-default":             true,
		"print-content-optimize-supported":           true,
		"print-objects":                              true,
		"print-objects-default":                      true,
		"print-objects-supported":                    true,
		"print-processing-attributes-supported":      true,
		"print-quality":                              true,
		"print-quality-default":                      true,
		"print-quality-supported":                    true,
		"print-rendering-intent":                     true,
		"print-rendering-intent-default":             true,
		"print-rendering-intent-supported":           true,
		"print-scaling":                              true,
		"print-scaling-default":                      true,
		"print-scaling-supported":                    true,
		"print-supports":                             true,
		"print-supports-default":                     true,
		"print-supports-supported":                   true,
		"printer-resolution":                         true,
		"printer-resolution-default":                 true,
		"printer-resolution-supported":               true,
		"punching-hole-diameter-configured":          true,
		"punching-locations-supported":               true,
		"punching-offset-supported":                  true,
		"punching-reference-edge-supported":          true,
		"separator-sheets":                           true,
		"separator-sheets-default":                   true,
		"separator-sheets-supported":                 true,
		"separator-sheets-type-supported":            true,
		"sides":                                      true,
		"sides-default":                              true,
		"sides-supported":                            true,
		"stitching-angle-supported":                  true,
		"stitching-locations-supported":              true,
		"stitching-method-supported":                 true,
		"stitching-offset-supported":                 true,
		"stitching-reference-edge-supported":         true,
		"x-image-position":                           true,
		"x-image-position-default":                   true,
		"x-image-position-supported":                 true,
		"x-image-shift":                              true,
		"x-image-shift-default":                      true,
		"x-image-shift-supported":                    true,
		"x-side1-image-shift":                        true,
		"x-side1-image-shift-default":                true,
		"x-side1-image-shift-supported":              true,
		"x-side2-image-shift":                        true,
		"x-side2-image-shift-default":                true,
		"x-side2-image-shift-supported":              true,
		"y-image-position":                           true,
		"y-image-position-default":                   true,
		"y-image-position-supported":                 true,
		"y-image-shift":                              true,
		"y-image-shift-default":                      true,
		"y-image-shift-supported":                    true,
		"y-side1-image-shift":                        true,
		"y-side1-image-shift-default":                true,
		"y-side1-image-shift-supported":              true,
		"y-side2-image-shift":                        true,
		"y-side2-image-shift-default":                true,
		"y-side2-image-shift-supported":              true,
	}
	documentStatusAttrs = map[string]bool{
		"document-state": true, "document-state-reasons": true, "document-state-message": true,
	}
	subscriptionDescriptionAttrs = map[string]bool{
		"notify-job-id": true, "notify-lease-expiration-time": true, "notify-printer-up-time": true,
		"notify-printer-uri": true, "notify-resource-id": true, "notify-system-uri": true,
		"notify-sequence-number": true, "notify-subscriber-user-name": true, "notify-subscriber-user-uri": true,
		"notify-subscription-id": true, "notify-subscription-uuid": true,
	}
	subscriptionTemplateAttrs = map[string]bool{
		"notify-attributes": true, "notify-attributes-supported": true, "notify-charset": true,
		"notify-events": true, "notify-events-default": true, "notify-events-supported": true,
		"notify-lease-duration": true, "notify-lease-duration-default": true, "notify-lease-duration-supported": true,
		"notify-max-events-supported": true, "notify-natural-language": true, "notify-pull-method": true,
		"notify-pull-method-supported": true, "notify-recipient-uri": true, "notify-schemes-supported": true,
		"notify-time-interval": true, "notify-user-data": true,
	}
	printerStatusAttrs = map[string]bool{
		"printer-state": true, "printer-state-reasons": true, "printer-state-message": true,
		"printer-is-accepting-jobs": true, "printer-up-time": true, "queued-job-count": true,
		"printer-state-change-time": true,
		"marker-message":            true, "printer-supply": true, "printer-supply-description": true,
	}
	printerDefaultsAttrs = map[string]bool{
		"copies-default": true, "document-format-default": true, "finishings-default": true,
		"finishings-col-default": true, "job-sheets-col-default": true, "media-default": true,
		"media-col-default": true, "media-source-default": true, "media-type-default": true,
		"output-bin-default": true, "printer-resolution-default": true, "sides-default": true,
		"page-delivery-default": true, "print-scaling-default": true, "print-as-raster-default": true,
		"multiple-document-handling-default": true, "print-color-mode-default": true, "print-quality-default": true,
		"job-account-id-default": true, "job-accounting-user-id-default": true,
		"job-cancel-after-default": true, "job-hold-until-default": true, "job-priority-default": true,
		"job-sheets-default": true, "notify-lease-duration-default": true, "notify-events-default": true,
		"number-up-default": true, "orientation-requested-default": true,
	}
	printerConfigurationAttrs = map[string]bool{
		"printer-is-shared": true, "device-uri": true, "ppd-name": true,
		"printer-error-policy": true, "printer-op-policy": true, "port-monitor": true,
		"requesting-user-name-allowed": true, "requesting-user-name-denied": true,
		"auth-info-required":             true,
		"printer-error-policy-supported": true, "printer-op-policy-supported": true,
		"port-monitor-supported": true, "printer-settable-attributes-supported": true,
		"job-settable-attributes-supported": true, "job-creation-attributes-supported": true,
		"printer-get-attributes-supported": true, "server-is-sharing-printers": true,
	}
	printerDescriptionAttrs = map[string]bool{
		"auth-info-required":                        true,
		"chamber-humidity-current":                  true,
		"chamber-temperature-current":               true,
		"charset-configured":                        true,
		"charset-supported":                         true,
		"client-info-supported":                     true,
		"color-supported":                           true,
		"compression-supported":                     true,
		"device-service-count":                      true,
		"device-uri":                                true,
		"device-uuid":                               true,
		"document-charset-default":                  true,
		"document-charset-supported":                true,
		"document-creation-attributes-supported":    true,
		"document-format-default":                   true,
		"document-format-details-supported":         true,
		"document-format-preferred":                 true,
		"document-format-supported":                 true,
		"document-format-varying-attributes":        true,
		"document-natural-language-default":         true,
		"document-natural-language-supported":       true,
		"document-password-supported":               true,
		"document-privacy-attributes":               true,
		"document-privacy-scope":                    true,
		"generated-natural-language-supported":      true,
		"identify-actions-default":                  true,
		"identify-actions-supported":                true,
		"input-source-supported":                    true,
		"ipp-features-supported":                    true,
		"ipp-versions-supported":                    true,
		"ippget-event-life":                         true,
		"job-authorization-uri-supported":           true,
		"job-constraints-supported":                 true,
		"job-creation-attributes-supported":         true,
		"job-history-attributes-configured":         true,
		"job-history-attributes-supported":          true,
		"job-history-interval-configured":           true,
		"job-history-interval-supported":            true,
		"job-ids-supported":                         true,
		"job-impressions-supported":                 true,
		"job-k-limit":                               true,
		"job-k-octets-supported":                    true,
		"job-mandatory-attributes-supported":        true,
		"job-media-sheets-supported":                true,
		"job-page-limit":                            true,
		"job-pages-per-set-supported":               true,
		"job-password-encryption-supported":         true,
		"job-password-length-supported":             true,
		"job-password-repertoire-configured":        true,
		"job-password-repertoire-supported":         true,
		"job-password-supported":                    true,
		"job-presets-supported":                     true,
		"job-privacy-attributes":                    true,
		"job-privacy-scope":                         true,
		"job-quota-period":                          true,
		"job-release-action-default":                true,
		"job-release-action-supported":              true,
		"job-resolvers-supported":                   true,
		"job-settable-attributes-supported":         true,
		"job-spooling-supported":                    true,
		"job-storage-access-supported":              true,
		"job-storage-disposition-supported":         true,
		"job-storage-group-supported":               true,
		"job-storage-supported":                     true,
		"job-triggers-supported":                    true,
		"jpeg-features-supported":                   true,
		"jpeg-k-octets-supported":                   true,
		"jpeg-x-dimension-supported":                true,
		"jpeg-y-dimension-supported":                true,
		"landscape-orientation-requested-preferred": true,
		"marker-change-time":                        true,
		"marker-colors":                             true,
		"marker-high-levels":                        true,
		"marker-levels":                             true,
		"marker-low-levels":                         true,
		"marker-message":                            true,
		"marker-names":                              true,
		"marker-types":                              true,
		"max-client-info-supported":                 true,
		"member-names":                              true,
		"member-uris":                               true,
		"mopria-certified":                          true,
		"multiple-destination-uris-supported":       true,
		"multiple-document-jobs-supported":          true,
		"multiple-operation-time-out":               true,
		"multiple-operation-time-out-action":        true,
		"natural-language-configured":               true,
		"operations-supported":                      true,
		"output-device-uuid-supported":              true,
		"pages-per-minute":                          true,
		"pages-per-minute-color":                    true,
		"pdf-k-octets-supported":                    true,
		"pdf-features-supported":                    true,
		"pdf-versions-supported":                    true,
		"pdl-override-supported":                    true,
		"platform-shape":                            true,
		"pkcs7-document-format-supported":           true,
		"port-monitor":                              true,
		"port-monitor-supported":                    true,
		"preferred-attributes-supported":            true,
		"printer-alert":                             true,
		"printer-alert-description":                 true,
		"printer-camera-image-uri":                  true,
		"printer-charge-info":                       true,
		"printer-charge-info-uri":                   true,
		"printer-commands":                          true,
		"printer-config-change-date-time":           true,
		"printer-config-change-time":                true,
		"printer-config-changes":                    true,
		"printer-contact-col":                       true,
		"printer-current-time":                      true,
		"printer-detailed-status-messages":          true,
		"printer-device-id":                         true,
		"printer-dns-sd-name":                       true,
		"printer-driver-installer":                  true,
		"printer-fax-log-uri":                       true,
		"printer-fax-modem-info":                    true,
		"printer-fax-modem-name":                    true,
		"printer-fax-modem-number":                  true,
		"printer-finisher":                          true,
		"printer-finisher-description":              true,
		"printer-finisher-supplies":                 true,
		"printer-finisher-supplies-description":     true,
		"printer-firmware-name":                     true,
		"printer-firmware-patches":                  true,
		"printer-firmware-string-version":           true,
		"printer-firmware-version":                  true,
		"printer-geo-location":                      true,
		"printer-get-attributes-supported":          true,
		"printer-icc-profiles":                      true,
		"printer-icons":                             true,
		"printer-id":                                true,
		"printer-info":                              true,
		"printer-input-tray":                        true,
		"printer-is-accepting-jobs":                 true,
		"printer-is-shared":                         true,
		"printer-is-temporary":                      true,
		"printer-kind":                              true,
		"printer-location":                          true,
		"printer-make-and-model":                    true,
		"printer-mandatory-job-attributes":          true,
		"printer-message-date-time":                 true,
		"printer-message-from-operator":             true,
		"printer-message-time":                      true,
		"printer-more-info":                         true,
		"printer-more-info-manufacturer":            true,
		"printer-name":                              true,
		"printer-organization":                      true,
		"printer-organizational-unit":               true,
		"printer-output-tray":                       true,
		"printer-pkcs7-public-key":                  true,
		"printer-pkcs7-repertoire-configured":       true,
		"printer-pkcs7-repertoire-supported":        true,
		"printer-requested-client-type":             true,
		"printer-service-type":                      true,
		"printer-settable-attributes-supported":     true,
		"printer-service-contact-col":               true,
		"printer-state":                             true,
		"printer-state-change-date-time":            true,
		"printer-state-change-time":                 true,
		"printer-state-message":                     true,
		"printer-state-reasons":                     true,
		"printer-storage":                           true,
		"printer-storage-description":               true,
		"printer-strings-languages-supported":       true,
		"printer-strings-uri":                       true,
		"printer-supply":                            true,
		"printer-supply-description":                true,
		"printer-supply-info-uri":                   true,
		"printer-type":                              true,
		"printer-up-time":                           true,
		"printer-uri-supported":                     true,
		"printer-uuid":                              true,
		"printer-wifi-ssid":                         true,
		"printer-wifi-state":                        true,
		"printer-xri-supported":                     true,
		"proof-copies-supported":                    true,
		"proof-print-copies-supported":              true,
		"pwg-raster-document-resolution-supported":  true,
		"pwg-raster-document-sheet-back":            true,
		"pwg-raster-document-type-supported":        true,
		"queued-job-count":                          true,
		"reference-uri-schemes-supported":           true,
		"repertoire-supported":                      true,
		"requesting-user-name-allowed":              true,
		"requesting-user-name-denied":               true,
		"requesting-user-uri-supported":             true,
		"smi2699-auth-print-group":                  true,
		"smi2699-auth-proxy-group":                  true,
		"smi2699-device-command":                    true,
		"smi2699-device-format":                     true,
		"smi2699-device-name":                       true,
		"smi2699-device-uri":                        true,
		"subordinate-printers-supported":            true,
		"subscription-privacy-attributes":           true,
		"subscription-privacy-scope":                true,
		"trimming-offset-supported":                 true,
		"trimming-reference-edge-supported":         true,
		"trimming-type-supported":                   true,
		"trimming-when-supported":                   true,
		"urf-supported":                             true,
		"uri-authentication-supported":              true,
		"uri-security-supported":                    true,
		"which-jobs-supported":                      true,
		"xri-authentication-supported":              true,
		"xri-security-supported":                    true,
		"xri-uri-scheme-supported":                  true,
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
		switch goipp.Op(req.Code) {
		case goipp.OpGetDocuments:
			return map[string]bool{"document-number": true}, false
		case goipp.OpGetJobs:
			return map[string]bool{"job-id": true, "job-uri": true}, false
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
		case "document-description":
			mergeAttributeSet(out, documentDescriptionAttrs)
		case "document-template":
			mergeAttributeSet(out, documentTemplateAttrs)
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

func makePageRangesAttr(name string, ranges []goipp.Range) goipp.Attribute {
	if len(ranges) == 0 {
		return goipp.MakeAttribute(name, goipp.TagRange, goipp.Range{Lower: 1, Upper: 1})
	}
	values := make([]goipp.Value, 0, len(ranges))
	for _, r := range ranges {
		values = append(values, r)
	}
	return goipp.MakeAttr(name, goipp.TagRange, values[0], values[1:]...)
}

func parsePageRangesList(value string) ([]goipp.Range, bool) {
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
		r, ok := parseRangeValue(part)
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

func validatePageRanges(ranges []goipp.Range) bool {
	if len(ranges) == 0 {
		return false
	}
	lower := 1
	for _, r := range ranges {
		if r.Lower < lower || r.Lower > r.Upper {
			return false
		}
		lower = r.Upper + 1
	}
	return true
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

func boolStringValid(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "0", "true", "false", "yes", "no", "on", "off":
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
	return fmt.Sprintf("%s://%s/printers/%s", ippSchemeForRequest(r), host, printer.Name)
}

func printerURIForJob(printer model.Printer, r *http.Request) string {
	host := hostForRequest(r)
	return fmt.Sprintf("ipp://%s/printers/%s", host, printer.Name)
}

func classURIFor(class model.Class, r *http.Request) string {
	host := hostForRequest(r)
	return fmt.Sprintf("%s://%s/classes/%s", ippSchemeForRequest(r), host, class.Name)
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
	jobID, docNum, ok := parseDocumentURIStrict(uri)
	if !ok {
		return 0, 0
	}
	return jobID, docNum
}

func parseDocumentURIStrict(uri string) (int64, int, bool) {
	if strings.TrimSpace(uri) == "" {
		return 0, 0, false
	}
	u, err := url.Parse(uri)
	if err != nil {
		return 0, 0, false
	}
	resource := path.Clean(strings.TrimSpace(u.Path))
	if resource == "." {
		resource = "/"
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) != 4 {
		return 0, 0, false
	}
	if parts[0] != "jobs" || parts[2] != "documents" {
		return 0, 0, false
	}
	jobID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || jobID <= 0 {
		return 0, 0, false
	}
	docNum, err := strconv.Atoi(parts[3])
	if err != nil || docNum <= 0 {
		return 0, 0, false
	}
	return jobID, docNum, true
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

func serverHostPortForUUID(r *http.Request) (string, int) {
	host := ""
	if r != nil && strings.TrimSpace(r.Host) != "" {
		host = r.Host
	}
	if strings.TrimSpace(host) == "" {
		host = appConfig().ServerName
	}
	host = ensureHostPort(host)
	parsedHost, portStr, err := net.SplitHostPort(host)
	if err != nil {
		return strings.Trim(host, "[]"), 631
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		port = 631
	}
	return strings.Trim(parsedHost, "[]"), port
}

func randomUint16() uint16 {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return binary.BigEndian.Uint16(buf[:])
	}
	return uint16(time.Now().UnixNano())
}

func assembleUUID(server string, port int, name string, number int64, rand1, rand2 uint16) string {
	if strings.TrimSpace(name) == "" {
		name = server
	}
	data := fmt.Sprintf("%s:%d:%s:%d:%04x:%04x", server, port, name, number, rand1, rand2)
	sum := md5.Sum([]byte(data))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x40
	return fmt.Sprintf("urn:uuid:%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		sum[0], sum[1], sum[2], sum[3], sum[4], sum[5], sum[6], sum[7],
		sum[8], sum[9], sum[10], sum[11], sum[12], sum[13], sum[14], sum[15])
}

func jobMoreInfoURI(job model.Job, r *http.Request) string {
	host := hostForRequest(r)
	return fmt.Sprintf("%s://%s/jobs/%d", webScheme(r), host, job.ID)
}

func printerUUIDFor(name string, r *http.Request) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	host, port := serverHostPortForUUID(r)
	return assembleUUID(host, port, name, 0, 0, 0)
}

func outputModeForColorMode(colorMode string) string {
	switch strings.ToLower(strings.TrimSpace(colorMode)) {
	case "monochrome", "auto-monochrome":
		return "monochrome"
	case "color", "auto":
		return "color"
	default:
		return ""
	}
}

func webScheme(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return "https"
	}
	return "http"
}

func printerMoreInfoURI(name string, isClass bool, r *http.Request) string {
	pathName := "printers"
	if isClass {
		pathName = "classes"
	}
	host := hostForRequest(r)
	return fmt.Sprintf("%s://%s/%s/%s", webScheme(r), host, pathName, name)
}

func printerStringsURI(r *http.Request, req *goipp.Message) string {
	host := hostForRequest(r)
	lang := selectStringsLanguage(req, r)
	if lang == "" {
		lang = "en"
	}
	return fmt.Sprintf("%s://%s/strings/%s.strings", webScheme(r), host, lang)
}

func stringsLanguagesSupported() []string {
	stringsLangOnce.Do(func() {
		langs := web.CupsStringsLanguages()
		if len(langs) == 0 {
			langs = []string{"en"}
		}
		seen := map[string]bool{}
		out := make([]string, 0, len(langs))
		for _, lang := range langs {
			lang = strings.TrimSpace(lang)
			if lang == "" {
				continue
			}
			if seen[lang] {
				continue
			}
			seen[lang] = true
			out = append(out, lang)
		}
		if len(out) == 0 {
			out = []string{"en"}
		}
		stringsLangs = out
	})
	return append([]string{}, stringsLangs...)
}

func selectStringsLanguage(req *goipp.Message, r *http.Request) string {
	supported := stringsLanguagesSupported()
	if len(supported) == 0 {
		return "en"
	}
	requested := ""
	if req != nil {
		requested = strings.TrimSpace(attrString(req.Operation, "attributes-natural-language"))
	}
	if requested == "" && r != nil {
		requested = parseAcceptLanguage(r.Header.Get("Accept-Language"))
	}
	requested = normalizeLangTag(requested)
	if requested != "" {
		for _, lang := range supported {
			if normalizeLangTag(lang) == requested {
				return lang
			}
		}
		if base := langBase(requested); base != "" {
			for _, lang := range supported {
				if normalizeLangTag(lang) == base {
					return lang
				}
			}
		}
	}
	for _, lang := range supported {
		if normalizeLangTag(lang) == "en" {
			return lang
		}
	}
	return supported[0]
}

func parseAcceptLanguage(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	part := header
	if idx := strings.Index(part, ","); idx >= 0 {
		part = part[:idx]
	}
	if idx := strings.Index(part, ";"); idx >= 0 {
		part = part[:idx]
	}
	return strings.TrimSpace(part)
}

func normalizeLangTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	tag = strings.ReplaceAll(tag, "_", "-")
	return strings.ToLower(tag)
}

func langBase(tag string) string {
	if tag == "" {
		return ""
	}
	if idx := strings.Index(tag, "-"); idx > 0 {
		return tag[:idx]
	}
	return tag
}

func queuedJobCountForPrinters(ctx context.Context, st *store.Store, printerIDs []int64) int {
	if st == nil || len(printerIDs) == 0 {
		return 0
	}
	count := 0
	if err := st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		count, err = st.CountQueuedJobsByPrinterIDs(ctx, tx, printerIDs)
		return err
	}); err != nil {
		return 0
	}
	return count
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
