package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cupsgolang/internal/backend"
	"cupsgolang/internal/config"
	"cupsgolang/internal/logging"
	"cupsgolang/internal/model"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/store"
)

type Scheduler struct {
	Store    *store.Store
	Spool    spool.Spool
	Interval time.Duration
	StopChan chan struct{}
	Mime     *config.MimeDB
	Config   config.Config

	lastTempCleanup time.Time
}

func (s *Scheduler) Start(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 2 * time.Second
	}
	if s.StopChan == nil {
		s.StopChan = make(chan struct{})
	}

	// CUPS temporary queues are not persisted across scheduler restarts.
	s.cleanupTemporaryPrinters(ctx, true)

	ticker := time.NewTicker(s.Interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.processOnce(ctx)
			case <-s.StopChan:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	if s.StopChan != nil {
		close(s.StopChan)
	}
}

func (s *Scheduler) processOnce(ctx context.Context) {
	s.releaseHeldJobs(ctx)

	historySecs, filesSecs := s.preserveIntervals(ctx)

	var jobs []model.Job
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		jobs, err = s.Store.ListPendingJobs(ctx, tx, 50)
		return err
	})

	candidates := make([]jobCandidate, 0, len(jobs))
	for _, job := range jobs {
		opts := parseOptionsJSON(job.Options)
		candidates = append(candidates, jobCandidate{
			job:      job,
			options:  opts,
			priority: jobPriority(opts),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority == candidates[j].priority {
			return candidates[i].job.SubmittedAt.Before(candidates[j].job.SubmittedAt)
		}
		return candidates[i].priority > candidates[j].priority
	})

	now := time.Now()
	for _, candidate := range candidates {
		job := candidate.job
		opts := candidate.options
		if shouldCancelJob(job, opts, now, s.Config.MaxJobTime) {
			_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
				completed := time.Now().UTC()
				return s.Store.UpdateJobState(ctx, tx, job.ID, 7, "job-canceled-at-device", &completed)
			})
			continue
		}
		if holdReason := shouldHoldJob(job, opts, now); holdReason != "" {
			_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
				return s.Store.UpdateJobState(ctx, tx, job.ID, 4, holdReason, nil)
			})
			continue
		}
		claimed := false
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			var err error
			claimed, err = s.Store.ClaimPendingJob(ctx, tx, job.ID)
			return err
		})
		if !claimed {
			continue
		}

		var docs []model.Document
		var printer model.Printer
		_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
			var err error
			docs, err = s.Store.ListDocumentsByJob(ctx, tx, job.ID)
			if err != nil {
				return err
			}
			printer, err = s.Store.GetPrinterByID(ctx, tx, job.PrinterID)
			return err
		})

		failed := false
		failReason := "document-unprintable-error"
		docList, err := s.buildJobDocuments(ctx, job, printer, docs)
		if err != nil {
			failed = true
			failReason = "document-unprintable-error"
		}
		for _, doc := range docList {
			outPath := s.Spool.OutputPath(job.ID, doc.FileName)
			if outPath == "" {
				continue
			}
			if err := s.processDocument(ctx, job, printer, doc, outPath); err != nil {
				failed = true
				failReason = failureReasonForError(err)
				break
			}
		}

		finalState := 0
		pageResult := ""
		err = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			if failed {
				if handled, err := s.applyErrorPolicy(ctx, tx, job, printer, opts, failReason); err != nil {
					return err
				} else if handled {
					return nil
				}
				if retried, err := s.scheduleRetry(ctx, tx, job, opts); err == nil && retried {
					return nil
				}
				completed := time.Now().UTC()
				state := 8
				if failReason == "job-stopped" {
					state = 6
				}
				if err := s.Store.UpdateJobState(ctx, tx, job.ID, state, failReason, &completed); err != nil {
					return err
				}
				finalState = state
				pageResult = failReason
				return nil
			}
			completed := time.Now().UTC()
			if err := s.Store.UpdateJobState(ctx, tx, job.ID, 9, "job-completed-successfully", &completed); err != nil {
				return err
			}
			finalState = 9
			pageResult = "ok"
			return nil
		})
		if err == nil && finalState != 0 {
			copies := optionInt(opts, "copies")
			logging.Page(logging.PageLogLine(job.ID, job.UserName, printer.Name, job.Name, copies, pageResult))
		}
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			details := map[string]string{
				"printer": printer.Name,
				"result":  pageResult,
			}
			if failed {
				details["status"] = "failed"
			} else {
				details["status"] = "completed"
			}
			return s.Store.AddJobEvent(ctx, tx, job.ID, "job-processed", details)
		})
	}

	// Match CUPS behavior: completed jobs and/or job files are cleaned up based on
	// PreserveJobHistory/PreserveJobFiles time intervals.
	s.cleanTerminalJobs(ctx, historySecs, filesSecs)

	// Match CUPS behavior: temporary queues are removed when unused.
	s.cleanupTemporaryPrinters(ctx, false)
}

type jobCandidate struct {
	job      model.Job
	options  map[string]string
	priority int
}

func (s *Scheduler) releaseHeldJobs(ctx context.Context) {
	var jobs []model.Job
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		jobs, err = s.Store.ListHeldJobs(ctx, tx, 50)
		return err
	})
	if len(jobs) == 0 {
		return
	}
	now := time.Now()
	for _, job := range jobs {
		opts := parseOptionsJSON(job.Options)
		if shouldCancelJob(job, opts, now, s.Config.MaxJobTime) {
			_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
				completed := time.Now().UTC()
				return s.Store.UpdateJobState(ctx, tx, job.ID, 7, "job-canceled-at-device", &completed)
			})
			continue
		}
		if shouldHoldJob(job, opts, now) != "" {
			continue
		}
		optionsJSON := job.Options
		changed := false
		if normalizeRetryOptions(opts, now) {
			changed = true
		}
		if normalizeHoldOptions(opts, now) {
			changed = true
		}
		if changed {
			if b, err := json.Marshal(opts); err == nil {
				optionsJSON = string(b)
			}
		}
		_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
			if optionsJSON != job.Options {
				if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
					return err
				}
			}
			return s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-queued", nil)
		})
	}
}

func (s *Scheduler) scheduleRetry(ctx context.Context, tx *sql.Tx, job model.Job, opts map[string]string) (bool, error) {
	retries := optionInt(opts, "number-of-retries")
	if retries <= 0 {
		return false, nil
	}
	if timeout := optionInt(opts, "retry-time-out"); timeout > 0 {
		if time.Since(job.SubmittedAt) > time.Duration(timeout)*time.Second {
			return false, nil
		}
	}
	interval := optionInt(opts, "retry-interval")
	retries--
	if retries > 0 {
		opts["number-of-retries"] = strconv.Itoa(retries)
	} else {
		delete(opts, "number-of-retries")
	}
	if interval > 0 {
		opts["cups-retry-at"] = strconv.FormatInt(time.Now().Add(time.Duration(interval)*time.Second).Unix(), 10)
	} else {
		delete(opts, "cups-retry-at")
	}
	optionsJSON, err := marshalOptionsJSON(opts)
	if err != nil {
		return false, err
	}
	if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
		return false, err
	}
	if interval > 0 {
		if err := s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-retry", nil); err != nil {
			return false, err
		}
	} else {
		if err := s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-retry", nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

const (
	defaultJobRetryLimit    = 5
	defaultJobRetryInterval = 300
)

func (s *Scheduler) applyErrorPolicy(ctx context.Context, tx *sql.Tx, job model.Job, printer model.Printer, opts map[string]string, reason string) (bool, error) {
	if reason != "job-stopped" {
		return false, nil
	}
	policy := strings.ToLower(strings.TrimSpace(errorPolicyForJob(printer, opts)))
	switch policy {
	case "retry-current-job":
		return true, s.Store.UpdateJobState(ctx, tx, job.ID, 3, "job-retry", nil)
	case "retry-job":
		return true, s.retryJobPolicy(ctx, tx, job, opts)
	case "abort-job":
		completed := time.Now().UTC()
		return true, s.Store.UpdateJobState(ctx, tx, job.ID, 8, "aborted-by-system", &completed)
	case "stop-printer":
		_ = s.Store.UpdatePrinterState(ctx, tx, printer.ID, 5)
		return true, s.Store.UpdateJobState(ctx, tx, job.ID, 3, "printer-stopped", nil)
	default:
		return false, nil
	}
}

func errorPolicyForJob(printer model.Printer, opts map[string]string) string {
	if opts != nil {
		if v := strings.TrimSpace(opts["cups-error-policy"]); v != "" {
			return v
		}
	}
	defaults := parseOptionsJSON(printer.DefaultOptions)
	if v := strings.TrimSpace(defaults["printer-error-policy"]); v != "" {
		return v
	}
	return "stop-printer"
}

func (s *Scheduler) retryJobPolicy(ctx context.Context, tx *sql.Tx, job model.Job, opts map[string]string) error {
	limit := optionInt(opts, "cups-retry-limit")
	if limit <= 0 {
		if s != nil && s.Config.JobRetryLimit > 0 {
			limit = s.Config.JobRetryLimit
		} else {
			limit = defaultJobRetryLimit
		}
	}
	interval := optionInt(opts, "cups-retry-interval")
	if interval <= 0 {
		if s != nil && s.Config.JobRetryInterval > 0 {
			interval = s.Config.JobRetryInterval
		} else {
			interval = defaultJobRetryInterval
		}
	}
	count := optionInt(opts, "cups-retry-count")
	count++
	if limit > 0 && count > limit {
		completed := time.Now().UTC()
		return s.Store.UpdateJobState(ctx, tx, job.ID, 8, "aborted-by-system", &completed)
	}
	opts["cups-retry-count"] = strconv.Itoa(count)
	if interval > 0 {
		opts["cups-retry-at"] = strconv.FormatInt(time.Now().Add(time.Duration(interval)*time.Second).Unix(), 10)
	} else {
		delete(opts, "cups-retry-at")
	}
	optionsJSON, err := marshalOptionsJSON(opts)
	if err != nil {
		return err
	}
	if err := s.Store.UpdateJobAttributes(ctx, tx, job.ID, nil, &optionsJSON); err != nil {
		return err
	}
	return s.Store.UpdateJobState(ctx, tx, job.ID, 4, "job-retry", nil)
}

func (s *Scheduler) buildJobDocuments(ctx context.Context, job model.Job, printer model.Printer, docs []model.Document) ([]model.Document, error) {
	sheets := getJobOption(job.Options, "job-sheets")
	if strings.TrimSpace(sheets) == "" {
		sheets = printer.JobSheetsDefault
	}
	start, end := parseJobSheetsOption(sheets)
	out := make([]model.Document, 0, len(docs)+2)
	if start != "" && start != "none" {
		doc, err := s.createBannerDocument(ctx, job, printer, "start", start)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	out = append(out, docs...)
	if end != "" && end != "none" {
		doc, err := s.createBannerDocument(ctx, job, printer, "end", end)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}

func (s *Scheduler) createBannerDocument(ctx context.Context, job model.Job, printer model.Printer, position, banner string) (model.Document, error) {
	name := fmt.Sprintf("banner-%s-%s.txt", position, sanitizeJobSheetsName(banner))
	template, ok := s.loadBannerTemplate(banner)
	content := renderBannerText(job, printer, banner, position)
	if ok {
		content = applyBannerTemplate(template, job, printer)
	}
	path, size, err := s.Spool.Save(job.ID, name, strings.NewReader(content))
	if err != nil {
		return model.Document{}, err
	}
	return model.Document{
		JobID:     job.ID,
		FileName:  name,
		MimeType:  "application/vnd.cups-banner",
		SizeBytes: size,
		Path:      path,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func renderBannerText(job model.Job, printer model.Printer, banner, position string) string {
	ts := time.Now().Format(time.RFC3339)
	builder := strings.Builder{}
	builder.WriteString("CUPS-Golang Banner\n")
	builder.WriteString("------------------\n")
	builder.WriteString("Banner: ")
	builder.WriteString(banner)
	builder.WriteString("\n")
	builder.WriteString("Position: ")
	builder.WriteString(position)
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

func (s *Scheduler) loadBannerTemplate(banner string) (string, bool) {
	name := sanitizeJobSheetsName(banner)
	if name == "" || name == "none" {
		return "", false
	}
	baseDir := filepath.Join(s.Config.DataDir, "banners")
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

func parseJobSheetsOption(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none", "none"
	}
	parts := strings.Split(value, ",")
	start := strings.TrimSpace(parts[0])
	if start == "" {
		start = "none"
	}
	end := "none"
	if len(parts) > 1 {
		end = strings.TrimSpace(parts[1])
		if end == "" {
			end = "none"
		}
	}
	return start, end
}

func sanitizeJobSheetsName(name string) string {
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func (s *Scheduler) processDocument(ctx context.Context, job model.Job, printer model.Printer, doc model.Document, outPath string) error {
	docMime := resolveDocMime(s.Mime, doc)
	if docMime == "" {
		docMime = "application/octet-stream"
	}
	doc.MimeType = docMime
	if s.Mime == nil {
		if err := copyFile(doc.Path, outPath); err != nil {
			return err
		}
		return s.submitToBackend(ctx, printer, job, doc, outPath)
	}
	if strings.EqualFold(doc.MimeType, "application/vnd.cups-raw") || isRawJob(job.Options) {
		if err := copyFile(doc.Path, outPath); err != nil {
			return err
		}
		return s.submitToBackend(ctx, printer, job, doc, outPath)
	}
	finalType, err := s.runFilterPipeline(job, printer, doc, outPath)
	if err != nil {
		return err
	}
	if finalType != "" {
		doc.MimeType = finalType
	}
	return s.submitToBackend(ctx, printer, job, doc, outPath)
}

func (s *Scheduler) runFilterPipeline(job model.Job, printer model.Printer, doc model.Document, outPath string) (string, error) {
	if s.Mime == nil {
		return doc.MimeType, copyFile(doc.Path, outPath)
	}
	docMime := strings.TrimSpace(doc.MimeType)
	if docMime == "" {
		docMime = "application/octet-stream"
	}

	ppdPath := ppdPathForPrinter(s.Config, printer)
	ppd, _ := config.LoadPPD(ppdPath)
	extra := []config.MimeConv{}
	destSet := map[string]bool{}
	hasFilter2 := false
	if ppd != nil {
		for _, f := range ppd.Filters {
			if strings.TrimSpace(f.Dest) != "" {
				hasFilter2 = true
				break
			}
		}
		for _, f := range ppd.Filters {
			dest := strings.TrimSpace(f.Dest)
			if hasFilter2 && dest == "" {
				continue
			}
			if dest != "" {
				destSet[dest] = true
			}
			extra = append(extra, config.MimeConv{
				Source:  f.Source,
				Dest:    dest,
				Cost:    f.Cost,
				Program: f.Program,
			})
		}
	}

	convs, finalType := selectFilterPipeline(s.Mime, docMime, extra, destSet, isTruthy(getJobOption(job.Options, "print-as-raster")))
	if len(convs) == 0 {
		return docMime, copyFile(doc.Path, outPath)
	}
	cmds := make([][]string, 0, len(convs))
	for _, conv := range convs {
		if conv.Program == "" {
			continue
		}
		if strings.TrimSpace(conv.Program) == "-" {
			continue
		}
		parts := strings.Fields(conv.Program)
		if len(parts) == 0 {
			continue
		}
		cmds = append(cmds, parts)
	}
	if len(cmds) == 0 {
		return docMime, copyFile(doc.Path, outPath)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return docMime, err
	}
	in, err := os.Open(doc.Path)
	if err != nil {
		return docMime, err
	}
	defer in.Close()
	out, err := os.Create(outPath)
	if err != nil {
		return docMime, err
	}
	defer out.Close()

	if finalType == "" {
		finalType = docMime
	}
	env := buildFilterEnv(job, printer, doc, s.Config, finalType)
	filterArgs := func(includeFile bool) []string {
		user := strings.TrimSpace(job.UserName)
		if user == "" {
			user = "anonymous"
		}
		title := strings.TrimSpace(job.Name)
		if title == "" {
			if doc.FileName != "" {
				title = doc.FileName
			} else {
				title = "Untitled"
			}
		}
		copies := "1"
		if v := getJobOption(job.Options, "copies"); v != "" {
			copies = v
		}
		opts := buildCupsOptions(job.Options)
		args := []string{
			strconv.FormatInt(job.ID, 10),
			user,
			title,
			copies,
			opts,
		}
		if includeFile {
			args = append(args, doc.Path)
		}
		return args
	}
	var prev io.Reader = in
	cmdsRun := make([]*exec.Cmd, 0, len(cmds))
	pipeClosers := make([]io.Closer, 0, len(cmds))
	for i, parts := range cmds {
		args := append([]string{}, parts[1:]...)
		args = append(args, filterArgs(i == 0)...)
		cmd := exec.Command(parts[0], args...)
		cmd.Env = env
		cmd.Stdin = prev
		if i == len(cmds)-1 {
			cmd.Stdout = out
		} else {
			pipeR, pipeW := io.Pipe()
			cmd.Stdout = pipeW
			prev = pipeR
			pipeClosers = append(pipeClosers, pipeW)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return docMime, copyFile(doc.Path, outPath)
		}
		cmdsRun = append(cmdsRun, cmd)
		if i < len(pipeClosers) {
			_ = pipeClosers[i].Close()
		}
	}
	for _, cmd := range cmdsRun {
		if err := cmd.Wait(); err != nil {
			return docMime, copyFile(doc.Path, outPath)
		}
	}
	return finalType, out.Sync()
}

func (s *Scheduler) submitToBackend(ctx context.Context, printer model.Printer, job model.Job, doc model.Document, outPath string) error {
	if printer.URI == "" {
		return nil
	}
	b := backend.ForURI(printer.URI)
	if b == nil {
		return backend.ErrUnsupported
	}
	return b.SubmitJob(ctx, printer, job, doc, outPath)
}

func failureReasonForError(err error) string {
	if err == nil {
		return "job-completed-successfully"
	}
	if backend.IsUnsupported(err) || errors.Is(err, backend.ErrUnsupported) {
		return "document-unprintable-error"
	}
	if backend.IsTemporary(err) {
		return "job-stopped"
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "unsupported") || strings.Contains(msg, "unprintable") || strings.Contains(msg, "format") {
		return "document-unprintable-error"
	}
	return "job-stopped"
}

func buildFilterEnv(job model.Job, printer model.Printer, doc model.Document, cfg config.Config, finalType string) []string {
	cupsOptions := buildCupsOptions(job.Options)
	copies := "1"
	if v := getJobOption(job.Options, "copies"); v != "" {
		copies = v
	}
	fileType := doc.MimeType
	if doc.MimeType == "application/vnd.cups-banner" {
		fileType = "job-sheet"
	}
	ppdPath := ppdPathForPrinter(cfg, printer)
	if finalType == "" {
		finalType = fileType
	}
	return append(os.Environ(),
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"CHARSET=utf-8",
		"CUPS_COPIES="+copies,
		"CUPS_FILETYPE="+fileType,
		"CUPS_JOB_ID="+strconv.FormatInt(job.ID, 10),
		"CUPS_JOB_NAME="+doc.FileName,
		"CUPS_USER="+job.UserName,
		"CONTENT_TYPE="+doc.MimeType,
		"FINAL_CONTENT_TYPE="+finalType,
		"CUPS_FINAL_CONTENT_TYPE="+finalType,
		"CUPS_OPTIONS="+cupsOptions,
		"CUPS_PRINTER="+printer.Name,
		"PRINTER="+printer.Name,
		"CUPS_PRINTER_URI="+printer.URI,
		"DEVICE_URI="+printer.URI,
		"PRINTER_INFO="+printer.Info,
		"PRINTER_LOCATION="+printer.Location,
		"PPD="+ppdPath,
		"CUPS_PPD="+ppdPath,
		"TMPDIR="+os.TempDir(),
		"CUPS_SERVERROOT="+cfg.ConfDir,
		"CUPS_DATADIR="+cfg.DataDir,
		"CUPS_STATEDIR="+cfg.DataDir,
	)
}

func ppdPathForPrinter(cfg config.Config, printer model.Printer) string {
	name := strings.TrimSpace(printer.PPDName)
	if name == "" {
		name = model.DefaultPPDName
	}
	name = filepath.Base(name)
	return filepath.Join(cfg.PPDDir, name)
}

func isRawJob(optionsJSON string) bool {
	val := strings.ToLower(strings.TrimSpace(getJobOption(optionsJSON, "raw")))
	return val == "true" || val == "yes" || val == "1"
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

func buildCupsOptions(optionsJSON string) string {
	if optionsJSON == "" {
		return ""
	}
	var opts map[string]string
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return ""
	}
	template := strings.TrimSpace(opts["finishing-template"])
	useTemplate := template != "" && !strings.EqualFold(template, "none")
	parts := make([]string, 0, len(opts))
	for k, v := range opts {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "cups-") || strings.HasPrefix(lk, "custom.") || strings.HasSuffix(lk, "-supplied") || lk == "job-attribute-fidelity" {
			continue
		}
		if v == "" {
			continue
		}
		if useTemplate && strings.EqualFold(k, "finishings") {
			continue
		}
		if strings.EqualFold(k, "finishing-template") {
			k = "cupsFinishingTemplate"
		}
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func resolveDocMime(db *config.MimeDB, doc model.Document) string {
	docMime := strings.TrimSpace(doc.MimeType)
	if docMime != "" && !strings.EqualFold(docMime, "application/octet-stream") {
		return docMime
	}
	if db == nil {
		return docMime
	}
	if ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(doc.FileName)), "."); ext != "" {
		if mt := db.TypeForExtension(ext); mt != "" {
			return mt
		}
	}
	return docMime
}

func parseOptionsJSON(optionsJSON string) map[string]string {
	if optionsJSON == "" {
		return map[string]string{}
	}
	var opts map[string]string
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil || opts == nil {
		return map[string]string{}
	}
	return opts
}

func marshalOptionsJSON(opts map[string]string) (string, error) {
	if opts == nil {
		return "{}", nil
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func optionInt(opts map[string]string, key string) int {
	if opts == nil {
		return 0
	}
	val := strings.TrimSpace(opts[key])
	if val == "" {
		return 0
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return n
}

func optionInt64(opts map[string]string, key string) int64 {
	if opts == nil {
		return 0
	}
	val := strings.TrimSpace(opts[key])
	if val == "" {
		return 0
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func jobPriority(opts map[string]string) int {
	n := optionInt(opts, "job-priority")
	if n < 1 || n > 100 {
		return 50
	}
	return n
}

func shouldCancelJob(job model.Job, opts map[string]string, now time.Time, defaultAfter int) bool {
	after := optionInt(opts, "job-cancel-after")
	if after <= 0 {
		after = defaultAfter
	}
	if after <= 0 {
		return false
	}
	if job.ProcessingAt == nil || job.ProcessingAt.IsZero() {
		return false
	}
	return now.Sub(*job.ProcessingAt) > time.Duration(after)*time.Second
}

func shouldHoldJob(job model.Job, opts map[string]string, now time.Time) string {
	if retryAt := optionInt64(opts, "cups-retry-at"); retryAt > 0 {
		if now.Unix() < retryAt {
			return "job-retry"
		}
	}
	if holdUntil := optionInt64(opts, "cups-hold-until"); holdUntil > 0 {
		if now.Unix() < holdUntil {
			return "job-incoming"
		}
	}
	hold, _ := jobHoldStatus(opts["job-hold-until"], job.SubmittedAt, now)
	if hold {
		return "job-hold-until-specified"
	}
	return ""
}

func normalizeRetryOptions(opts map[string]string, now time.Time) bool {
	retryAt := optionInt64(opts, "cups-retry-at")
	if retryAt <= 0 {
		return false
	}
	if now.Unix() >= retryAt {
		delete(opts, "cups-retry-at")
		return true
	}
	return false
}

func normalizeHoldOptions(opts map[string]string, now time.Time) bool {
	holdUntil := optionInt64(opts, "cups-hold-until")
	if holdUntil <= 0 {
		return false
	}
	if now.Unix() >= holdUntil {
		delete(opts, "cups-hold-until")
		return true
	}
	return false
}

func jobHoldStatus(value string, submittedAt, now time.Time) (bool, time.Time) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "no-hold", "none", "resume":
		return false, time.Time{}
	case "indefinite", "hold", "forever":
		return true, time.Time{}
	case "auth-info-required":
		return true, time.Time{}
	case "day-time":
		return holdWindow(now.In(time.Local), 6, 18)
	case "night":
		return holdWindow(now.In(time.Local), 18, 6)
	case "second-shift":
		return holdWindow(now.In(time.Local), 16, 24)
	case "third-shift":
		return holdWindow(now.In(time.Local), 0, 8)
	case "weekend":
		return holdWeekend(now.In(time.Local))
	default:
		if submittedAt.IsZero() {
			submittedAt = now
		}
		if releaseAt, ok := parseHoldTimeUTC(value, submittedAt); ok {
			if now.UTC().Before(releaseAt) {
				return true, releaseAt
			}
			return false, releaseAt
		}
	}
	return false, time.Time{}
}

func holdWindow(now time.Time, startHour, endHour int) (bool, time.Time) {
	if startHour < 0 {
		startHour = 0
	}
	if endHour < 0 {
		endHour = 0
	}
	if startHour > 24 {
		startHour = 24
	}
	if endHour > 24 {
		endHour = 24
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), startHour, 0, 0, 0, now.Location())
	end := time.Date(now.Year(), now.Month(), now.Day(), endHour, 0, 0, 0, now.Location())
	if startHour == endHour {
		return false, time.Time{}
	}
	if startHour < endHour {
		if now.Before(start) {
			return true, start
		}
		if now.After(end) || now.Equal(end) {
			return true, start.Add(24 * time.Hour)
		}
		return false, time.Time{}
	}
	// Window crosses midnight.
	if now.After(start) || now.Equal(start) || now.Before(end) {
		return false, time.Time{}
	}
	return true, start
}

func holdWeekend(now time.Time) (bool, time.Time) {
	switch now.Weekday() {
	case time.Saturday, time.Sunday:
		return false, time.Time{}
	default:
		// Hold until next Saturday 00:00.
		daysUntil := (int(time.Saturday) - int(now.Weekday()) + 7) % 7
		if daysUntil == 0 {
			daysUntil = 7
		}
		release := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, daysUntil)
		return true, release
	}
}

func parseHoldTimeUTC(value string, base time.Time) (time.Time, bool) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return time.Time{}, false
	}
	hour, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	sec := 0
	var err3 error
	if len(parts) == 3 {
		sec, err3 = strconv.Atoi(parts[2])
	}
	if err1 != nil || err2 != nil || err3 != nil {
		return time.Time{}, false
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 || sec < 0 || sec > 59 {
		return time.Time{}, false
	}
	baseUTC := base.UTC()
	release := time.Date(baseUTC.Year(), baseUTC.Month(), baseUTC.Day(), hour, min, sec, 0, time.UTC)
	if !release.After(baseUTC) {
		release = release.Add(24 * time.Hour)
	}
	return release, true
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (s *Scheduler) preserveIntervals(ctx context.Context) (historySecs int, filesSecs int) {
	// CUPS defaults: PreserveJobHistory Yes, PreserveJobFiles 1d.
	const defaultFilesSecs = 24 * 60 * 60
	settings := map[string]string{}
	if s == nil || s.Store == nil {
		return intMax(), defaultFilesSecs
	}
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		settings, err = s.Store.ListSettings(ctx, tx)
		return err
	})

	historyVal := firstSetting(settings, "PreserveJobHistory", "_preserve_job_history", "preserve_job_history")
	if strings.TrimSpace(historyVal) == "" {
		historyVal = "Yes"
	}
	filesVal := firstSetting(settings, "PreserveJobFiles", "_preserve_job_files", "preserve_job_files")
	if strings.TrimSpace(filesVal) == "" {
		filesVal = "1d"
	}

	historySecs = parseTimeInterval(historyVal, intMax())
	filesSecs = parseTimeInterval(filesVal, defaultFilesSecs)
	return historySecs, filesSecs
}

func (s *Scheduler) cleanTerminalJobs(ctx context.Context, historySecs int, filesSecs int) {
	if s == nil || s.Store == nil {
		return
	}
	if historySecs == intMax() && filesSecs == intMax() {
		return
	}

	now := time.Now().UTC()
	type cleanupJob struct {
		id        int64
		deleteJob bool
		deleteDoc bool
		docPaths  []string
		outPaths  []string
	}
	cleanups := []cleanupJob{}

	// Decide which terminal jobs need cleanup. We do the decisions inside a
	// consistent read transaction and then do file IO + deletes outside.
	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		jobs, err := s.Store.ListTerminalJobs(ctx, tx, 200)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.CompletedAt == nil {
				continue
			}
			age := now.Sub(job.CompletedAt.UTC())
			ageSecs := int(age / time.Second)
			if ageSecs < 0 {
				continue
			}

			deleteJob := historySecs != intMax() && ageSecs >= historySecs
			deleteDoc := !deleteJob && filesSecs != intMax() && ageSecs >= filesSecs
			if !deleteJob && !deleteDoc {
				continue
			}

			docs, err := s.Store.ListDocumentsByJob(ctx, tx, job.ID)
			if err != nil {
				return err
			}
			docPaths := []string{}
			outPaths := []string{}
			for _, d := range docs {
				if p := strings.TrimSpace(d.Path); p != "" {
					docPaths = append(docPaths, p)
				}
				if s.Spool.OutputDir != "" {
					if op := strings.TrimSpace(s.Spool.OutputPath(job.ID, d.FileName)); op != "" {
						outPaths = append(outPaths, op)
					}
				}
			}

			cleanups = append(cleanups, cleanupJob{
				id:        job.ID,
				deleteJob: deleteJob,
				deleteDoc: deleteDoc,
				docPaths:  docPaths,
				outPaths:  outPaths,
			})
		}
		return nil
	})

	if len(cleanups) == 0 {
		return
	}

	// Best-effort file cleanup first; then remove DB rows.
	for _, c := range cleanups {
		for _, p := range c.docPaths {
			_ = os.Remove(p)
		}
		for _, p := range c.outPaths {
			_ = os.Remove(p)
		}
	}

	_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		for _, c := range cleanups {
			if c.deleteJob {
				if err := s.Store.DeleteJob(ctx, tx, c.id); err != nil {
					return err
				}
				continue
			}
			if c.deleteDoc {
				if err := s.Store.DeleteDocumentsByJob(ctx, tx, c.id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Scheduler) cleanupTemporaryPrinters(ctx context.Context, force bool) {
	if s == nil || s.Store == nil {
		return
	}

	if !force && !s.lastTempCleanup.IsZero() && time.Since(s.lastTempCleanup) < 30*time.Second {
		return
	}
	s.lastTempCleanup = time.Now().UTC()

	// CUPS keeps temporary queues around for 5 minutes after the last job
	// completes, unless forced (e.g. on startup/shutdown).
	unusedBefore := time.Now().UTC().Add(-5 * time.Minute)

	type cleanupPrinter struct {
		printerID int64
		docPaths  []string
		outPaths  []string
		ppdPath   string
	}
	cleanups := []cleanupPrinter{}
	var candidates []model.Printer

	_ = s.Store.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		candidates, err = s.Store.ListTemporaryPrinters(ctx, tx, force, unusedBefore, 100)
		if err != nil {
			return err
		}
		for _, p := range candidates {
			jobIDs, err := s.Store.ListJobIDsByPrinter(ctx, tx, p.ID)
			if err != nil {
				return err
			}
			docPaths := []string{}
			outPaths := []string{}
			for _, jobID := range jobIDs {
				docs, err := s.Store.ListDocumentsByJob(ctx, tx, jobID)
				if err != nil {
					return err
				}
				for _, d := range docs {
					if sp := strings.TrimSpace(d.Path); sp != "" {
						docPaths = append(docPaths, sp)
					}
					if s.Spool.OutputDir != "" {
						if op := strings.TrimSpace(s.Spool.OutputPath(jobID, d.FileName)); op != "" {
							outPaths = append(outPaths, op)
						}
					}
				}
			}
			ppdPath := ""
			if ppdName := strings.TrimSpace(p.PPDName); ppdName != "" && !strings.EqualFold(ppdName, model.DefaultPPDName) {
				base := filepath.Base(ppdName)
				if strings.EqualFold(base, p.Name+".ppd") {
					ppdPath = filepath.Join(s.Config.PPDDir, base)
				}
			}
			cleanups = append(cleanups, cleanupPrinter{printerID: p.ID, docPaths: docPaths, outPaths: outPaths, ppdPath: ppdPath})
		}
		return nil
	})

	if len(candidates) == 0 {
		return
	}

	// Best-effort file cleanup before deleting DB rows to avoid orphan spool data.
	for _, c := range cleanups {
		for _, p := range c.docPaths {
			_ = os.Remove(p)
		}
		for _, p := range c.outPaths {
			_ = os.Remove(p)
		}
		if strings.TrimSpace(c.ppdPath) != "" {
			_ = os.Remove(c.ppdPath)
		}
	}

	_ = s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		for _, candidate := range candidates {
			p, err := s.Store.GetPrinterByID(ctx, tx, candidate.ID)
			if err != nil {
				continue
			}
			if !p.IsTemporary {
				continue
			}
			if !force {
				if p.State == 4 {
					continue
				}
				if !p.UpdatedAt.Before(unusedBefore) {
					continue
				}
			}
			if err := s.Store.DeletePrinter(ctx, tx, p.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func firstSetting(settings map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := settings[k]; ok {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	return ""
}

func intMax() int {
	return int(^uint(0) >> 1)
}

func parseTimeInterval(value string, fallback int) int {
	v := strings.TrimSpace(value)
	if v == "" {
		return fallback
	}
	low := strings.ToLower(v)
	switch low {
	case "true", "on", "enabled", "yes":
		return intMax()
	case "false", "off", "disabled", "no":
		return 0
	}

	// CUPS syntax: <number>[w|d|h|m]
	n, unit := parseLeadingNumber(v)
	if n < 0 {
		return fallback
	}
	switch unit {
	case "", "s":
		// seconds
	case "w":
		n *= 7 * 24 * 60 * 60
	case "d":
		n *= 24 * 60 * 60
	case "h":
		n *= 60 * 60
	case "m":
		n *= 60
	default:
		return fallback
	}
	if n < 0 {
		return fallback
	}
	if n > float64(intMax()) {
		return intMax()
	}
	return int(n)
}

func parseLeadingNumber(value string) (num float64, unit string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return -1, ""
	}
	// Only accept a leading unsigned integer/float with optional unit suffix,
	// matching the simple formats used by CUPS ("1d", "3600", etc).
	i := 0
	dot := false
	for i < len(value) {
		ch := value[i]
		if ch >= '0' && ch <= '9' {
			i++
			continue
		}
		if ch == '.' && !dot {
			dot = true
			i++
			continue
		}
		break
	}
	if i == 0 {
		return -1, ""
	}
	nf, err := strconv.ParseFloat(value[:i], 64)
	if err != nil || nf < 0 {
		return -1, ""
	}
	unit = strings.ToLower(strings.TrimSpace(value[i:]))
	if len(unit) > 1 {
		// Allow a single-letter suffix, ignore trailing whitespace.
		return -1, ""
	}
	return nf, unit
}

func selectFilterPipeline(db *config.MimeDB, src string, extra []config.MimeConv, destSet map[string]bool, forceRaster bool) ([]config.MimeConv, string) {
	if db == nil || src == "" {
		return nil, ""
	}
	candidates := []string{}
	if forceRaster {
		candidates = append(candidates, "application/vnd.cups-raster", "image/pwg-raster")
	}
	for dest := range destSet {
		candidates = append(candidates, dest)
	}
	candidates = append(candidates, "application/octet-stream")

	var best []config.MimeConv
	bestCost := -1
	bestDest := ""
	seen := map[string]bool{}
	for _, dest := range candidates {
		if dest == "" || seen[dest] {
			continue
		}
		seen[dest] = true
		convs := findFilterPipeline(db, src, dest, extra)
		if len(convs) == 0 {
			continue
		}
		if forceRaster && (dest == "application/vnd.cups-raster" || dest == "image/pwg-raster") {
			return convs, dest
		}
		cost := pipelineCost(convs)
		if bestCost == -1 || cost < bestCost {
			bestCost = cost
			best = convs
			bestDest = dest
		}
	}
	return best, bestDest
}

func pipelineCost(convs []config.MimeConv) int {
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

func findFilterPipeline(db *config.MimeDB, src, dst string, extra []config.MimeConv) []config.MimeConv {
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
		dest := strings.TrimSpace(conv.Dest)
		if dest == "" {
			dest = dst
		}
		if conv.Source == "" || dest == "" {
			continue
		}
		convCopy := conv
		convCopy.Dest = dest
		graph[conv.Source] = append(graph[conv.Source], edge{to: dest, conv: convCopy})
	}
	dist := map[string]int{}
	prev := map[string]edge{}
	visited := map[string]bool{}
	queue := []string{src}
	dist[src] = 0
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
	for cur := dst; cur != src; {
		e, ok := prev[cur]
		if !ok {
			break
		}
		path = append(path, e.conv)
		cur = e.conv.Source
	}
	if len(path) == 0 {
		return nil
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}
