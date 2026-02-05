package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"cupsgolang/internal/model"
	"cupsgolang/internal/store"
)

func RenderPrinters(w http.ResponseWriter, r *http.Request, st *store.Store) {
	var printers []model.Printer
	_ = st.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printers, err = st.ListPrinters(context.Background(), tx)
		return err
	})

	shareServer := serverSharingEnabled(r.Context(), st)
	ctx := NewTemplateContext()
	ctx.SetVar("title", "Printers")
	ctx.SetVar("SECTION", "printers")
	ctx.SetVar("total", strconv.Itoa(len(printers)))
	ctx.SetArray("printer_name", mapPrinters(printers, func(p model.Printer) string { return p.Name }))
	ctx.SetArray("printer_info", mapPrinters(printers, func(p model.Printer) string { return p.Info }))
	ctx.SetArray("printer_location", mapPrinters(printers, func(p model.Printer) string { return p.Location }))
	ctx.SetArray("printer_make_and_model", mapPrinters(printers, func(p model.Printer) string { return "CUPS-Golang" }))
	ctx.SetArray("printer_state", mapPrinters(printers, func(p model.Printer) string { return strconv.Itoa(p.State) }))
	ctx.SetArray("printer_state_message", mapPrinters(printers, func(p model.Printer) string { return "" }))
	ctx.SetArray("printer_uri_supported", mapPrinters(printers, func(p model.Printer) string {
		return "/printers/" + p.Name
	}))
	ctx.SetVar("server_is_sharing_printers", boolInt(shareServer))
	ctx.SetVar("default_name", defaultPrinterName(printers))

	renderCupsPage(w, r, ctx, "header.tmpl.in", "printers-header.tmpl", "printers.tmpl", "trailer.tmpl")
}

func RenderPrinter(w http.ResponseWriter, r *http.Request, st *store.Store) {
	name := strings.TrimPrefix(r.URL.Path, "/printers/")
	name = path.Base(name)
	if name == "" || name == "/" {
		http.Redirect(w, r, "/printers/", http.StatusFound)
		return
	}

	var printer model.Printer
	err := st.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printer, err = st.GetPrinterByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}

	shareServer := serverSharingEnabled(r.Context(), st)
	sharePrinter := shareServer && printer.Shared
	defaults := parseDefaultOptions(printer.DefaultOptions)
	mediaDefault := firstNonEmptyString(defaults["media"], "A4")
	sidesDefault := firstNonEmptyString(defaults["sides"], "one-sided")
	ctx := NewTemplateContext()
	ctx.SetVar("title", printer.Name)
	ctx.SetVar("SECTION", "printers")
	ctx.SetVar("printer_name", printer.Name)
	ctx.SetVar("printer_uri_supported", "/printers/"+printer.Name)
	ctx.SetVar("printer_state", strconv.Itoa(printer.State))
	ctx.SetVar("printer_is_accepting_jobs", boolInt(printer.Accepting))
	ctx.SetVar("server_is_sharing_printers", boolInt(shareServer))
	ctx.SetVar("printer_is_shared", boolInt(sharePrinter))
	ctx.SetVar("default_name", printer.Name)
	ctx.SetVar("admin_uri", "/admin")
	ctx.SetVar("printer_info", printer.Info)
	ctx.SetVar("printer_location", printer.Location)
	ctx.SetVar("printer_make_and_model", "CUPS-Golang")
	ctx.SetVar("color_supported", "1")
	ctx.SetVar("sides_supported", "one-sided")
	ctx.SetVar("device_uri", printer.URI)
	ctx.SetVar("job_sheets_default", printer.JobSheetsDefault)
	ctx.SetVar("media_default", mediaDefault)
	ctx.SetVar("sides_default", sidesDefault)
	ctx.SetVar("printer_commands", "")

	renderCupsPage(w, r, ctx, "header.tmpl.in", "printer.tmpl", "trailer.tmpl")
}

func RenderJobs(w http.ResponseWriter, r *http.Request, st *store.Store) {
	var jobs []model.Job
	var printers []model.Printer
	_ = st.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printers, err = st.ListPrinters(r.Context(), tx)
		if err != nil || len(printers) == 0 {
			return err
		}
		jobs, err = st.ListJobsByPrinter(r.Context(), tx, printers[0].ID, 50)
		return err
	})

	ctx := NewTemplateContext()
	ctx.SetVar("title", "Jobs")
	ctx.SetVar("SECTION", "jobs")
	ctx.SetVar("total", strconv.Itoa(len(jobs)))
	ctx.SetArray("job_id", mapJobs(jobs, func(j model.Job) string { return strconv.FormatInt(j.ID, 10) }))
	ctx.SetArray("job_name", mapJobs(jobs, func(j model.Job) string { return j.Name }))
	ctx.SetArray("job_originating_user_name", mapJobs(jobs, func(j model.Job) string { return j.UserName }))
	ctx.SetArray("job_k_octets", mapJobs(jobs, func(j model.Job) string { return "0" }))
	ctx.SetArray("job_impressions_completed", mapJobs(jobs, func(j model.Job) string { return strconv.Itoa(j.Impressions) }))
	ctx.SetArray("job_state", mapJobs(jobs, func(j model.Job) string { return strconv.Itoa(j.State) }))
	ctx.SetArray("job_printer_name", mapJobs(jobs, func(j model.Job) string { return printerNameFor(j, printers) }))
	ctx.SetArray("job_printer_uri", mapJobs(jobs, func(j model.Job) string { return "/printers/" + printerNameFor(j, printers) }))
	ctx.SetArray("job_printer_state_message", mapJobs(jobs, func(j model.Job) string { return "" }))
	ctx.SetVar("org.cups.sid", "")

	renderCupsPage(w, r, ctx, "header.tmpl.in", "jobs-header.tmpl", "jobs.tmpl", "trailer.tmpl")
}

func RenderJob(w http.ResponseWriter, r *http.Request, st *store.Store) {
	// CUPS uses job list; fall back to /jobs/ for now.
	http.Redirect(w, r, "/jobs/", http.StatusFound)
}

func RenderAdmin(w http.ResponseWriter, r *http.Request, st *store.Store, msgTemplate string, msgVars map[string]string, arrayVars map[string][]string) {
	settings := loadServerSettings(r.Context(), st)
	shareServer := settingBool(settings, "_share_printers", "1")
	remoteAdmin := settingBool(settings, "_remote_admin", "0")
	remoteAny := settingBool(settings, "_remote_any", "0")
	userCancelAny := settingBool(settings, "_user_cancel_any", "0")
	browseWebIF := settingBool(settings, "_browse_web_if", "0")
	debugLogging := settingBool(settings, "_debug_logging", "0")
	maxClients := settingValue(settings, "_max_clients", "100")
	maxJobs := settingValue(settings, "_max_jobs", "500")
	maxLogSize := settingValue(settings, "_max_log_size", "1m")

	preserveHistory := settingValue(settings, "_preserve_job_history", "Yes")
	preserveFiles := settingValue(settings, "_preserve_job_files", "1d")
	preserveJobs := !isDisabledSetting(preserveHistory)
	if !preserveJobs {
		preserveHistory = "0"
		preserveFiles = "0"
	}

	advanced := strings.TrimSpace(r.FormValue("ADVANCEDSETTINGS"))
	advancedEnabled := advanced != "" && !strings.EqualFold(advanced, "no")

	ctx := NewTemplateContext()
	ctx.SetVar("title", "Administration")
	ctx.SetVar("SECTION", "admin")
	ctx.SetVar("org.cups.sid", "")
	ctx.SetVar("ADVANCEDSETTINGS", boolInt(advancedEnabled))
	ctx.SetVar("share_printers", checked(shareServer))
	ctx.SetVar("remote_admin", checked(remoteAdmin))
	ctx.SetVar("remote_any", checked(remoteAny))
	ctx.SetVar("browse_web_if", checked(browseWebIF))
	ctx.SetVar("user_cancel_any", checked(userCancelAny))
	ctx.SetVar("debug_logging", checked(debugLogging))
	ctx.SetVar("max_clients", maxClients)
	ctx.SetVar("max_jobs", maxJobs)
	ctx.SetVar("preserve_jobs", checked(preserveJobs))
	ctx.SetVar("preserve_job_history", preserveHistory)
	ctx.SetVar("preserve_job_files", preserveFiles)
	ctx.SetVar("max_log_size", maxLogSize)

	if msgVars != nil {
		for k, v := range msgVars {
			ctx.SetVar(k, v)
		}
	}
	if arrayVars != nil {
		for k, v := range arrayVars {
			ctx.SetArray(k, v)
		}
	}

	if msgTemplate != "" {
		renderCupsPage(w, r, ctx, "header.tmpl.in", msgTemplate, "trailer.tmpl")
		return
	}
	renderCupsPage(w, r, ctx, "header.tmpl.in", "admin.tmpl", "trailer.tmpl")
}

func RenderClasses(w http.ResponseWriter, r *http.Request, st *store.Store) {
	var classes []model.Class
	_ = st.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		classes, err = st.ListClasses(r.Context(), tx)
		return err
	})

	ctx := NewTemplateContext()
	ctx.SetVar("title", "Classes")
	ctx.SetVar("SECTION", "classes")
	ctx.SetVar("total", strconv.Itoa(len(classes)))
	ctx.SetArray("printer_name", mapClasses(classes, func(c model.Class) string { return c.Name }))
	ctx.SetArray("printer_info", mapClasses(classes, func(c model.Class) string { return c.Info }))
	ctx.SetArray("printer_location", mapClasses(classes, func(c model.Class) string { return c.Location }))
	ctx.SetArray("member_uris", mapClasses(classes, func(c model.Class) string { return "" }))
	ctx.SetArray("printer_state", mapClasses(classes, func(c model.Class) string { return strconv.Itoa(c.State) }))
	ctx.SetArray("printer_state_message", mapClasses(classes, func(c model.Class) string { return "" }))
	ctx.SetArray("printer_uri_supported", mapClasses(classes, func(c model.Class) string {
		return "/classes/" + c.Name
	}))
	renderCupsPage(w, r, ctx, "header.tmpl.in", "classes-header.tmpl", "classes.tmpl", "trailer.tmpl")
}

func RenderHelp(w http.ResponseWriter, r *http.Request) {
	ctx := NewTemplateContext()
	ctx.SetVar("title", "Help")
	ctx.SetVar("SECTION", "help")
	ctx.SetVar("HELPTITLE", "Online Help")
	ctx.SetVar("HELPFILE", "")
	ctx.SetVar("TOPIC", "")
	ctx.SetVar("QUERY", "")
	ctx.SetArray("BMTEXT", []string{})
	ctx.SetArray("BMLINK", []string{})
	ctx.SetArray("BMINDENT", []string{})
	ctx.SetArray("QTEXT", []string{})
	ctx.SetArray("QLINK", []string{})
	ctx.SetArray("QPTEXT", []string{})
	ctx.SetArray("QPLINK", []string{})

	if r.URL.Query().Get("PRINTABLE") == "YES" {
		renderCupsPage(w, r, ctx, "help-printable.tmpl")
		return
	}
	renderCupsPage(w, r, ctx, "header.tmpl.in", "help-header.tmpl", "help-trailer.tmpl")
}

func RenderClass(w http.ResponseWriter, r *http.Request, st *store.Store) {
	name := strings.TrimPrefix(r.URL.Path, "/classes/")
	name = path.Base(name)
	if name == "" || name == "/" {
		http.Redirect(w, r, "/classes/", http.StatusFound)
		return
	}

	var class model.Class
	var members []model.Printer
	err := st.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		class, err = st.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		members, err = st.ListClassMembers(r.Context(), tx, class.ID)
		return err
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}

	shareServer := serverSharingEnabled(r.Context(), st)
	defaults := parseDefaultOptions(class.DefaultOptions)
	mediaDefault := firstNonEmptyString(defaults["media"], "A4")
	sidesDefault := firstNonEmptyString(defaults["sides"], "one-sided")
	ctx := NewTemplateContext()
	ctx.SetVar("title", class.Name)
	ctx.SetVar("SECTION", "classes")
	ctx.SetVar("printer_name", class.Name)
	ctx.SetVar("printer_uri_supported", "/classes/"+class.Name)
	ctx.SetVar("printer_state", strconv.Itoa(class.State))
	ctx.SetVar("printer_is_accepting_jobs", boolInt(class.Accepting))
	ctx.SetVar("server_is_sharing_printers", boolInt(shareServer))
	ctx.SetVar("printer_is_shared", boolInt(shareServer))
	ctx.SetVar("default_name", class.Name)
	ctx.SetVar("admin_uri", "/admin")
	ctx.SetVar("printer_info", class.Info)
	ctx.SetVar("printer_location", class.Location)
	ctx.SetVar("job_sheets_default", class.JobSheetsDefault)
	ctx.SetVar("media_default", mediaDefault)
	ctx.SetVar("sides_default", sidesDefault)

	memberURIs := make([]string, 0, len(members))
	for _, m := range members {
		memberURIs = append(memberURIs, m.Name)
	}
	ctx.SetVar("member_uris", strings.Join(memberURIs, ", "))

	renderCupsPage(w, r, ctx, "header.tmpl.in", "class.tmpl", "trailer.tmpl")
}

func renderCupsPage(w http.ResponseWriter, r *http.Request, ctx *TemplateContext, files ...string) {
	tfs := cupsTemplateFS()
	if tfs == nil {
		http.Error(w, "templates not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, name := range files {
		pathName := selectTemplatePath(tfs, name, r)
		tpl, err := loadCupsTemplate(tfs, pathName)
		if err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		rendered, err := tpl.Render(ctx, 0)
		if err != nil {
			http.Error(w, "template render error", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(rewriteVersion(rendered)))
	}
}

// RenderTemplates renders a sequence of CUPS templates with a shared context.
func RenderTemplates(w http.ResponseWriter, r *http.Request, ctx *TemplateContext, files ...string) {
	renderCupsPage(w, r, ctx, files...)
}

func rewriteVersion(s string) string {
	s = strings.ReplaceAll(s, "@CUPS_VERSION@", "CUPS-Golang")
	s = strings.ReplaceAll(s, "@CUPS_REVISION@", "")
	return s
}

func selectTemplatePath(tfs fs.FS, name string, r *http.Request) string {
	for _, cand := range langCandidates(r) {
		if cand == "" {
			continue
		}
		path := cand + "/" + name
		if _, err := fs.Stat(tfs, path); err == nil {
			return path
		}
	}
	return name
}

func langCandidates(r *http.Request) []string {
	header := r.Header.Get("Accept-Language")
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	seen := map[string]bool{}
	out := []string{}
	for _, part := range parts {
		tag := strings.TrimSpace(strings.Split(part, ";")[0])
		if tag == "" {
			continue
		}
		cand := normalizeLang(tag)
		if cand != "" && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
		primary := strings.Split(cand, "_")[0]
		if primary != "" && !seen[primary] {
			seen[primary] = true
			out = append(out, primary)
		}
	}
	return out
}

func normalizeLang(tag string) string {
	tag = strings.ReplaceAll(tag, "-", "_")
	parts := strings.Split(tag, "_")
	if len(parts) == 1 {
		return strings.ToLower(parts[0])
	}
	lang := strings.ToLower(parts[0])
	region := strings.ToUpper(parts[1])
	return lang + "_" + region
}

func mapPrinters(printers []model.Printer, fn func(model.Printer) string) []string {
	out := make([]string, 0, len(printers))
	for _, p := range printers {
		out = append(out, fn(p))
	}
	return out
}

func mapJobs(jobs []model.Job, fn func(model.Job) string) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, fn(j))
	}
	return out
}

func mapClasses(classes []model.Class, fn func(model.Class) string) []string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, fn(c))
	}
	return out
}

func defaultPrinterName(printers []model.Printer) string {
	if len(printers) == 0 {
		return ""
	}
	for _, p := range printers {
		if p.IsDefault {
			return p.Name
		}
	}
	return printers[0].Name
}

func printerNameFor(job model.Job, printers []model.Printer) string {
	for _, p := range printers {
		if p.ID == job.PrinterID {
			return p.Name
		}
	}
	return ""
}

func boolInt(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func checked(v bool) string {
	if v {
		return "CHECKED"
	}
	return ""
}

func parseDefaultOptions(options string) map[string]string {
	options = strings.TrimSpace(options)
	if options == "" {
		return map[string]string{}
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(options), &out); err != nil || out == nil {
		return map[string]string{}
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func serverSharingEnabled(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
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

func loadServerSettings(ctx context.Context, st *store.Store) map[string]string {
	if st == nil {
		return map[string]string{}
	}
	settings := map[string]string{}
	_ = st.WithTx(ctx, true, func(tx *sql.Tx) error {
		var err error
		settings, err = st.ListSettings(ctx, tx)
		return err
	})
	return settings
}

func settingValue(settings map[string]string, key string, fallback string) string {
	if settings == nil {
		return fallback
	}
	if v, ok := settings[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func settingBool(settings map[string]string, key string, fallback string) bool {
	val := strings.ToLower(strings.TrimSpace(settingValue(settings, key, fallback)))
	switch val {
	case "1", "true", "yes", "on", "checked":
		return true
	default:
		return false
	}
}

func isDisabledSetting(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "no", "off", "false", "disabled":
		return true
	default:
		return false
	}
}
