package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/web"
)

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminOr401(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		web.RenderAdmin(w, r, s.Store, "", nil, nil)
		return
	case http.MethodPost:
		r.ParseForm()
		op := r.FormValue("OP")
		switch op {
		case "config-server":
			if r.FormValue("CHANGESETTINGS") != "" {
				if err := s.applyServerSettingsFromForm(r); err != nil {
					web.RenderAdmin(w, r, s.Store, "admin.tmpl", map[string]string{
						"SETTINGS_MESSAGE": "Unable to update server settings:",
						"SETTINGS_ERROR":   err.Error(),
					}, nil)
					return
				}
			}
			web.RenderAdmin(w, r, s.Store, "admin.tmpl", nil, nil)
			return
		case "find-new-printers":
			s.renderAvailablePrinters(w, r)
			return
		case "add-printer":
			web.RenderAdmin(w, r, s.Store, "add-printer.tmpl", map[string]string{
				"op":               "add-printer-confirm",
				"device_uri":       r.FormValue("DEVICE_URI"),
				"PRINTER_INFO":     r.FormValue("PRINTER_INFO"),
				"PRINTER_LOCATION": r.FormValue("PRINTER_LOCATION"),
				"template_name":    firstNonEmpty(r.FormValue("TEMPLATE_NAME"), r.FormValue("PRINTER_NAME")),
			}, nil)
			return
		case "add-class":
			memberData := s.memberOptions(r)
			web.RenderAdmin(w, r, s.Store, "add-class.tmpl", map[string]string{
				"op": "add-class-confirm",
			}, memberData)
			return
		case "add-printer-confirm":
			if err := s.createPrinterFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{
					"error": err.Error(),
				}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-added.tmpl", nil, nil)
			return
		case "add-class-confirm":
			if err := s.createClassFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{
					"error": err.Error(),
				}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "class-added.tmpl", nil, nil)
			return
		case "modify-printer":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing printer name"}, nil)
				return
			}
			if err := s.renderModifyPrinter(w, r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
			}
			return
		case "modify-printer-confirm":
			if err := s.modifyPrinterFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-modified.tmpl", map[string]string{
				"printer_name": r.FormValue("PRINTER_NAME"),
			}, nil)
			return
		case "modify-class":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing class name"}, nil)
				return
			}
			if err := s.renderModifyClass(w, r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
			}
			return
		case "modify-class-confirm":
			if err := s.modifyClassFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "class-modified.tmpl", map[string]string{
				"printer_name": r.FormValue("PRINTER_NAME"),
			}, nil)
			return
		case "set-printer-options":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing printer name"}, nil)
				return
			}
			if err := s.renderSetPrinterOptions(w, r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
			}
			return
		case "set-printer-options-confirm":
			if err := s.applyPrinterOptionsFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-configured.tmpl", map[string]string{
				"printer_name": r.FormValue("PRINTER_NAME"),
				"OP":           "set-printer-options",
			}, nil)
			return
		case "set-class-options":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing class name"}, nil)
				return
			}
			if err := s.renderSetClassOptions(w, r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
			}
			return
		case "set-class-options-confirm":
			if err := s.applyClassOptionsFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-configured.tmpl", map[string]string{
				"printer_name": r.FormValue("PRINTER_NAME"),
				"OP":           "set-class-options",
			}, nil)
			return
		case "set-as-default":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing printer name"}, nil)
				return
			}
			if r.FormValue("IS_CLASS") != "" {
				s.setDefaultClass(r, name)
				web.RenderAdmin(w, r, s.Store, "printer-default.tmpl", map[string]string{
					"printer_name": name,
					"is_class":     "1",
				}, nil)
				return
			}
			s.setDefaultPrinter(r, name)
			web.RenderAdmin(w, r, s.Store, "printer-default.tmpl", map[string]string{
				"printer_name": name,
				"is_class":     "",
			}, nil)
			return
		case "set-allowed-users":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing printer name"}, nil)
				return
			}
			if err := s.renderAllowedUsers(w, r, name, r.FormValue("IS_CLASS") != ""); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
			}
			return
		case "set-allowed-users-confirm":
			if err := s.applyAllowedUsersFromForm(r); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-modified.tmpl", map[string]string{
				"printer_name": r.FormValue("PRINTER_NAME"),
			}, nil)
			return
		case "delete-printer":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing printer name"}, nil)
				return
			}
			if err := s.deletePrinterByName(r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "printer-deleted.tmpl", map[string]string{"printer_name": name}, nil)
			return
		case "delete-class":
			name := r.FormValue("printer_name")
			if name == "" {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": "missing class name"}, nil)
				return
			}
			if err := s.deleteClassByName(r, name); err != nil {
				web.RenderAdmin(w, r, s.Store, "error.tmpl", map[string]string{"error": err.Error()}, nil)
				return
			}
			web.RenderAdmin(w, r, s.Store, "class-deleted.tmpl", map[string]string{"printer_name": name}, nil)
			return
		default:
			web.RenderAdmin(w, r, s.Store, "admin.tmpl", nil, nil)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePrinterPost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminOr401(w, r) {
		return
	}
	r.ParseForm()
	op := r.FormValue("OP")
	name := path.Base(r.URL.Path)
	switch op {
	case "print-test-page":
		_ = s.printTestPage(r, name)
	case "accept-jobs":
		s.updatePrinterAccepting(r, name, true)
	case "reject-jobs":
		s.updatePrinterAccepting(r, name, false)
	case "stop-printer":
		s.updatePrinterState(r, name, 5)
	case "start-printer":
		s.updatePrinterState(r, name, 3)
	case "cancel-jobs":
		s.cancelJobs(r, name)
	case "set-as-default":
		s.setDefaultPrinter(r, name)
	case "delete-printer":
		_ = s.deletePrinterByName(r, name)
	}
	http.Redirect(w, r, "/printers/"+name, http.StatusFound)
}

func (s *Server) handleClassPost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminOr401(w, r) {
		return
	}
	r.ParseForm()
	op := r.FormValue("OP")
	name := path.Base(r.URL.Path)
	switch op {
	case "print-test-page":
		_ = s.printTestPageForClass(r, name)
	case "accept-jobs":
		s.updateClassAccepting(r, name, true)
	case "reject-jobs":
		s.updateClassAccepting(r, name, false)
	case "stop-class":
		s.updateClassState(r, name, 5)
	case "start-class":
		s.updateClassState(r, name, 3)
	case "set-as-default":
		s.setDefaultClass(r, name)
	case "delete-class":
		_ = s.deleteClassByName(r, name)
	}
	http.Redirect(w, r, "/classes/"+name, http.StatusFound)
}

func (s *Server) handleJobsPost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminOr401(w, r) {
		return
	}
	r.ParseForm()
	op := r.FormValue("OP")
	jobID, _ := strconv.ParseInt(r.FormValue("job_id"), 10, 64)
	switch op {
	case "cancel-job":
		s.cancelJob(r, jobID)
	case "hold-job":
		s.updateJobState(r, jobID, 4, "job-hold-until-specified")
	case "release-job":
		s.updateJobState(r, jobID, 3, "job-incoming")
	}
	http.Redirect(w, r, "/jobs/", http.StatusFound)
}

func (s *Server) createPrinterFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing printer name")
	}
	info := r.FormValue("PRINTER_INFO")
	loc := r.FormValue("PRINTER_LOCATION")
	uri := r.FormValue("DEVICE_URI")
	if uri == "" {
		uri = "file:///dev/null"
	}
	ppdName := strings.TrimSpace(firstNonEmpty(r.FormValue("PPD_NAME"), r.FormValue("ppd_name")))
	shared := r.FormValue("PRINTER_IS_SHARED") != ""
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		_, err := s.Store.CreatePrinter(r.Context(), tx, name, uri, loc, info, ppdName, true, false, shared, "none", "")
		if err != nil {
			return err
		}
		return nil
	})
}

func (s *Server) createClassFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing class name")
	}
	info := r.FormValue("PRINTER_INFO")
	loc := r.FormValue("PRINTER_LOCATION")
	memberURIs := r.Form["MEMBER_URIS"]

	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		memberIDs := make([]int64, 0, len(memberURIs))
		for _, uri := range memberURIs {
			printerName := path.Base(uri)
			if printerName == "" {
				printerName = uri
			}
			p, err := s.Store.GetPrinterByName(r.Context(), tx, printerName)
			if err != nil {
				continue
			}
			memberIDs = append(memberIDs, p.ID)
		}
		_, err := s.Store.CreateClass(r.Context(), tx, name, loc, info, true, false, memberIDs)
		return err
	})
}

func (s *Server) deletePrinterByName(r *http.Request, name string) error {
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.DeletePrinter(r.Context(), tx, p.ID)
	})
}

func (s *Server) deleteClassByName(r *http.Request, name string) error {
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.DeleteClass(r.Context(), tx, c.ID)
	})
}

func (s *Server) updatePrinterAccepting(r *http.Request, name string, accepting bool) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.UpdatePrinterAccepting(r.Context(), tx, p.ID, accepting)
	})
}

func (s *Server) updatePrinterState(r *http.Request, name string, state int) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.UpdatePrinterState(r.Context(), tx, p.ID, state)
	})
}

func (s *Server) setDefaultPrinter(r *http.Request, name string) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.SetDefaultPrinter(r.Context(), tx, p.ID)
	})
}

func (s *Server) setDefaultClass(r *http.Request, name string) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.SetDefaultClass(r.Context(), tx, c.ID)
	})
}

func (s *Server) cancelJobs(r *http.Request, name string) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.CancelJobsByPrinter(r.Context(), tx, p.ID, "job-canceled-by-user")
	})
}

func (s *Server) updateClassAccepting(r *http.Request, name string, accepting bool) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.UpdateClassAccepting(r.Context(), tx, c.ID, accepting)
	})
}

func (s *Server) updateClassState(r *http.Request, name string, state int) {
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		return s.Store.UpdateClassState(r.Context(), tx, c.ID, state)
	})
}

func (s *Server) cancelJob(r *http.Request, jobID int64) {
	if jobID == 0 {
		return
	}
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		completed := time.Now().UTC()
		return s.Store.UpdateJobState(r.Context(), tx, jobID, 7, "job-canceled-by-user", &completed)
	})
}

func (s *Server) updateJobState(r *http.Request, jobID int64, state int, reason string) {
	if jobID == 0 {
		return
	}
	_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		return s.Store.UpdateJobState(r.Context(), tx, jobID, state, reason, nil)
	})
}

func (s *Server) memberOptions(r *http.Request) map[string][]string {
	var printers []model.Printer
	_ = s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printers, err = s.Store.ListPrinters(r.Context(), tx)
		return err
	})
	uris := make([]string, 0, len(printers))
	names := make([]string, 0, len(printers))
	for _, p := range printers {
		uris = append(uris, "/printers/"+p.Name)
		names = append(names, p.Name)
	}
	return map[string][]string{
		"member_uris":     uris,
		"member_names":    names,
		"member_selected": make([]string, len(uris)),
	}
}

func (s *Server) applyServerSettingsFromForm(r *http.Request) error {
	if s == nil || s.Store == nil {
		return nil
	}
	ctx := r.Context()
	return s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		setBool := func(formKey, settingKey string) error {
			val := "0"
			if strings.TrimSpace(r.FormValue(formKey)) != "" {
				val = "1"
			}
			return s.Store.SetSetting(ctx, tx, settingKey, val)
		}
		setString := func(formKey, settingKey, fallback string) error {
			val := strings.TrimSpace(r.FormValue(formKey))
			if val == "" {
				val = fallback
			}
			return s.Store.SetSetting(ctx, tx, settingKey, val)
		}

		if err := setBool("SHARE_PRINTERS", "_share_printers"); err != nil {
			return err
		}
		if err := setBool("REMOTE_ADMIN", "_remote_admin"); err != nil {
			return err
		}
		if err := setBool("REMOTE_ANY", "_remote_any"); err != nil {
			return err
		}
		if err := setBool("USER_CANCEL_ANY", "_user_cancel_any"); err != nil {
			return err
		}
		if err := setBool("BROWSE_WEB_IF", "_browse_web_if"); err != nil {
			return err
		}
		if err := setBool("DEBUG_LOGGING", "_debug_logging"); err != nil {
			return err
		}
		if err := setString("MAX_CLIENTS", "_max_clients", "100"); err != nil {
			return err
		}
		if err := setString("MAX_JOBS", "_max_jobs", "500"); err != nil {
			return err
		}
		if err := setString("MAX_LOG_SIZE", "_max_log_size", "1m"); err != nil {
			return err
		}

		if strings.TrimSpace(r.FormValue("PRESERVE_JOBS")) == "" {
			if err := s.Store.SetSetting(ctx, tx, "_preserve_job_history", "0"); err != nil {
				return err
			}
			if err := s.Store.SetSetting(ctx, tx, "_preserve_job_files", "0"); err != nil {
				return err
			}
			return nil
		}

		if err := setString("PRESERVE_JOB_HISTORY", "_preserve_job_history", "Yes"); err != nil {
			return err
		}
		if err := setString("PRESERVE_JOB_FILES", "_preserve_job_files", "1d"); err != nil {
			return err
		}
		return nil
	})
}

func (s *Server) renderAvailablePrinters(w http.ResponseWriter, r *http.Request) {
	devices := []Device{}
	devices = append(devices, discoverLocalDevices()...)
	devices = append(devices, discoverNetworkIPP()...)
	devices = append(devices, discoverMDNSIPP()...)

	uris := make([]string, 0, len(devices))
	infos := make([]string, 0, len(devices))
	makes := make([]string, 0, len(devices))
	templates := make([]string, 0, len(devices))
	for _, d := range devices {
		uris = append(uris, d.URI)
		infos = append(infos, d.Info)
		makes = append(makes, d.Make)
		templates = append(templates, sanitizePrinterName(firstNonEmpty(d.Info, d.Make, "Printer")))
	}
	web.RenderAdmin(w, r, s.Store, "list-available-printers.tmpl", nil, map[string][]string{
		"device_uri":            uris,
		"device_info":           infos,
		"device_make_and_model": makes,
		"template_name":         templates,
	})
}

func (s *Server) renderModifyPrinter(w http.ResponseWriter, r *http.Request, name string) error {
	var printer model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.GetPrinterByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		return err
	}
	web.RenderAdmin(w, r, s.Store, "modify-printer.tmpl", map[string]string{
		"op":               "modify-printer-confirm",
		"printer_name":     printer.Name,
		"printer_info":     printer.Info,
		"printer_location": printer.Location,
		"device_uri":       printer.URI,
		"PRINTER_IS_SHARED": func() string {
			if printer.Shared {
				return "1"
			}
			return "0"
		}(),
	}, nil)
	return nil
}

func (s *Server) modifyPrinterFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing printer name")
	}
	info := r.FormValue("PRINTER_INFO")
	loc := r.FormValue("PRINTER_LOCATION")
	uri := r.FormValue("DEVICE_URI")
	shared := r.FormValue("PRINTER_IS_SHARED") != ""
	ppdName := strings.TrimSpace(firstNonEmpty(r.FormValue("PPD_NAME"), r.FormValue("ppd_name")))
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		if uri == "" {
			uri = p.URI
		}
		if _, err := s.Store.UpsertPrinter(r.Context(), tx, name, uri, loc, info, p.Accepting); err != nil {
			return err
		}
		if ppdName != "" {
			if err := s.Store.UpdatePrinterPPDName(r.Context(), tx, p.ID, ppdName); err != nil {
				return err
			}
		}
		return s.Store.UpdatePrinterSharing(r.Context(), tx, p.ID, shared)
	})
}

func (s *Server) renderModifyClass(w http.ResponseWriter, r *http.Request, name string) error {
	var class model.Class
	var members []model.Printer
	var printers []model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		class, err = s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		members, err = s.Store.ListClassMembers(r.Context(), tx, class.ID)
		if err != nil {
			return err
		}
		printers, err = s.Store.ListPrinters(r.Context(), tx)
		return err
	})
	if err != nil {
		return err
	}

	memberSet := map[int64]bool{}
	for _, m := range members {
		memberSet[m.ID] = true
	}
	uris := make([]string, 0, len(printers))
	names := make([]string, 0, len(printers))
	selected := make([]string, 0, len(printers))
	for _, p := range printers {
		uris = append(uris, "/printers/"+p.Name)
		names = append(names, p.Name)
		if memberSet[p.ID] {
			selected = append(selected, "selected")
		} else {
			selected = append(selected, "")
		}
	}

	web.RenderAdmin(w, r, s.Store, "modify-class.tmpl", map[string]string{
		"op":               "modify-class-confirm",
		"printer_name":     class.Name,
		"printer_info":     class.Info,
		"printer_location": class.Location,
	}, map[string][]string{
		"member_uris":     uris,
		"member_names":    names,
		"member_selected": selected,
	})
	return nil
}

func (s *Server) modifyClassFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing class name")
	}
	info := r.FormValue("PRINTER_INFO")
	loc := r.FormValue("PRINTER_LOCATION")
	memberURIs := r.Form["MEMBER_URIS"]
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		c, err := s.Store.GetClassByName(r.Context(), tx, name)
		if err != nil {
			return err
		}
		memberIDs := make([]int64, 0, len(memberURIs))
		for _, uri := range memberURIs {
			printerName := path.Base(uri)
			if printerName == "" {
				printerName = uri
			}
			p, err := s.Store.GetPrinterByName(r.Context(), tx, printerName)
			if err != nil {
				continue
			}
			memberIDs = append(memberIDs, p.ID)
		}
		_, err = s.Store.UpsertClass(r.Context(), tx, name, loc, info, c.Accepting, memberIDs)
		return err
	})
}

func (s *Server) renderSetPrinterOptions(w http.ResponseWriter, r *http.Request, name string) error {
	var printer model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.GetPrinterByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		return err
	}

	ppd, _ := loadPPDForPrinter(printer)
	groups := []struct {
		ID      string
		Name    string
		Options []*config.PPDOption
	}{}
	if ppd != nil && len(ppd.Groups) > 0 {
		for _, g := range ppd.Groups {
			opts := filterPPDOptions(g.Options, ppd)
			if len(opts) == 0 {
				continue
			}
			name := g.Text
			if strings.TrimSpace(name) == "" {
				name = g.Name
			}
			groups = append(groups, struct {
				ID      string
				Name    string
				Options []*config.PPDOption
			}{
				ID:      g.Name,
				Name:    name,
				Options: opts,
			})
		}
	} else if ppd != nil {
		var opts []*config.PPDOption
		for _, opt := range ppd.OptionDetails {
			opts = append(opts, opt)
		}
		sort.Slice(opts, func(i, j int) bool {
			return opts[i].Keyword < opts[j].Keyword
		})
		opts = filterPPDOptions(opts, ppd)
		if len(opts) > 0 {
			groups = append(groups, struct {
				ID      string
				Name    string
				Options []*config.PPDOption
			}{
				ID:      "GENERAL",
				Name:    "Printer Options",
				Options: opts,
			})
		}
	}
	if len(jobSheetsSupported()) > 0 {
		groups = append(groups, struct {
			ID      string
			Name    string
			Options []*config.PPDOption
		}{
			ID:      "CUPS_BANNERS",
			Name:    "Banners",
			Options: nil,
		})
	}
	if len(printerErrorPolicySupported(false)) > 0 || len(s.policyNames()) > 0 {
		groups = append(groups, struct {
			ID      string
			Name    string
			Options []*config.PPDOption
		}{
			ID:      "CUPS_POLICIES",
			Name:    "Policies",
			Options: nil,
		})
	}
	if len(portMonitorSupported(ppd)) > 0 {
		groups = append(groups, struct {
			ID      string
			Name    string
			Options []*config.PPDOption
		}{
			ID:      "CUPS_PORT_MONITOR",
			Name:    "Port Monitor",
			Options: nil,
		})
	}

	ctx := web.NewTemplateContext()
	ctx.SetVar("title", "Set Printer Options")
	ctx.SetVar("SECTION", "admin")
	ctx.SetVar("org.cups.sid", "")
	ctx.SetVar("printer_name", printer.Name)
	ctx.SetVar("op", "set-printer-options-confirm")

	groupIDs := make([]string, 0, len(groups))
	groupNames := make([]string, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		groupNames = append(groupNames, g.Name)
	}
	ctx.SetArray("group_id", groupIDs)
	ctx.SetArray("group", groupNames)

	web.RenderTemplates(w, r, ctx, "header.tmpl.in", "set-printer-options-header.tmpl")

	defaults := parseJobOptions(printer.DefaultOptions)
	for _, g := range groups {
		ctx.SetVar("group_id", g.ID)
		ctx.SetVar("group", g.Name)
		web.RenderTemplates(w, r, ctx, "option-header.tmpl")

		if g.ID == "CUPS_BANNERS" {
			supported := jobSheetsSupported()
			start, end := jobSheetsPairFromDefault(printer.JobSheetsDefault)

			ctx.SetArray("choices", supported)
			ctx.SetArray("text", supported)
			ctx.SetArray("conflicted", []string{"0"})
			ctx.SetArray("iscustom", []string{"0"})
			ctx.SetArray("params", []string{})
			ctx.SetArray("paramtext", []string{})
			ctx.SetArray("paramvalue", []string{})
			ctx.SetArray("inputtype", []string{})

			ctx.SetArray("keyword", []string{"job_sheets_start"})
			ctx.SetArray("keytext", []string{"Starting Banner"})
			ctx.SetArray("defchoice", []string{start})
			web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")

			ctx.SetArray("keyword", []string{"job_sheets_end"})
			ctx.SetArray("keytext", []string{"Ending Banner"})
			ctx.SetArray("defchoice", []string{end})
			web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
		} else if g.ID == "CUPS_POLICIES" {
			errorPolicies := printerErrorPolicySupported(false)
			opPolicies := s.policyNames()
			errorDefault := choiceOrDefault(defaults["printer-error-policy"], errorPolicies, defaultPrinterErrorPolicy(false))
			opDefault := choiceOrDefault(defaults["printer-op-policy"], opPolicies, defaultPrinterOpPolicy())

			ctx.SetArray("conflicted", []string{"0"})
			ctx.SetArray("iscustom", []string{"0"})
			ctx.SetArray("params", []string{})
			ctx.SetArray("paramtext", []string{})
			ctx.SetArray("paramvalue", []string{})
			ctx.SetArray("inputtype", []string{})

			if len(errorPolicies) > 0 {
				ctx.SetArray("choices", errorPolicies)
				ctx.SetArray("text", errorPolicies)
				ctx.SetArray("keyword", []string{"printer_error_policy"})
				ctx.SetArray("keytext", []string{"Error Policy"})
				ctx.SetArray("defchoice", []string{errorDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
			if len(opPolicies) > 0 {
				ctx.SetArray("choices", opPolicies)
				ctx.SetArray("text", opPolicies)
				ctx.SetArray("keyword", []string{"printer_op_policy"})
				ctx.SetArray("keytext", []string{"Operation Policy"})
				ctx.SetArray("defchoice", []string{opDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
		} else if g.ID == "CUPS_PORT_MONITOR" {
			monitors := portMonitorSupported(ppd)
			if len(monitors) > 0 {
				portDefault := choiceOrDefault(defaults["port-monitor"], monitors, defaultPortMonitor())

				ctx.SetArray("conflicted", []string{"0"})
				ctx.SetArray("iscustom", []string{"0"})
				ctx.SetArray("params", []string{})
				ctx.SetArray("paramtext", []string{})
				ctx.SetArray("paramvalue", []string{})
				ctx.SetArray("inputtype", []string{})
				ctx.SetArray("choices", monitors)
				ctx.SetArray("text", monitors)
				ctx.SetArray("keyword", []string{"port_monitor"})
				ctx.SetArray("keytext", []string{"Port Monitor"})
				ctx.SetArray("defchoice", []string{portDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
		} else {
			for _, opt := range g.Options {
				choices := ppdOptionChoices(opt, ppd)
				if len(choices) < 2 {
					continue
				}
				selected := ppdDefaultSelections(opt, choices, defaults, ppd)
				choiceVals := make([]string, 0, len(choices))
				choiceTexts := make([]string, 0, len(choices))
				for _, c := range choices {
					choiceVals = append(choiceVals, c.Choice)
					choiceTexts = append(choiceTexts, c.Text)
				}
				defchoice := []string{}
				if strings.EqualFold(opt.UI, "pickmany") {
					selectedSet := map[string]bool{}
					for _, s := range selected {
						selectedSet[strings.ToLower(s)] = true
					}
					defchoice = make([]string, len(choiceVals))
					for i, v := range choiceVals {
						if selectedSet[strings.ToLower(v)] {
							defchoice[i] = v
						}
					}
				} else {
					ch := ""
					if len(selected) > 0 {
						ch = selected[0]
					}
					defchoice = []string{ch}
				}

				ctx.SetArray("keyword", []string{opt.Keyword})
				ctx.SetArray("keytext", []string{ppdOptionLabel(opt.Keyword)})
				ctx.SetArray("choices", choiceVals)
				ctx.SetArray("text", choiceTexts)
				ctx.SetArray("defchoice", defchoice)
				ctx.SetArray("conflicted", []string{"0"})

				if opt.Custom && len(opt.CustomParams) > 0 {
					ctx.SetArray("iscustom", []string{"1"})
					paramNames := make([]string, 0, len(opt.CustomParams))
					paramTexts := make([]string, 0, len(opt.CustomParams))
					paramValues := make([]string, 0, len(opt.CustomParams))
					inputTypes := make([]string, 0, len(opt.CustomParams))
					customDefaults := customParamDefaults(defaults, opt)
					unitHint := customUnitHint(selected)
					for _, p := range opt.CustomParams {
						paramNames = append(paramNames, p.Name)
						paramTexts = append(paramTexts, firstNonEmpty(p.Text, p.Name))
						value := strings.TrimSpace(customDefaults[p.Name])
						if value == "" && p.Type == "units" && unitHint != "" {
							value = unitHint
						}
						paramValues = append(paramValues, value)
						if p.Type == "password" {
							inputTypes = append(inputTypes, "password")
						} else {
							inputTypes = append(inputTypes, "text")
						}
					}
					ctx.SetArray("params", paramNames)
					ctx.SetArray("paramtext", paramTexts)
					ctx.SetArray("paramvalue", paramValues)
					ctx.SetArray("inputtype", inputTypes)
				} else {
					ctx.SetArray("iscustom", []string{"0"})
					ctx.SetArray("params", []string{})
					ctx.SetArray("paramtext", []string{})
					ctx.SetArray("paramvalue", []string{})
					ctx.SetArray("inputtype", []string{})
				}

				switch strings.ToLower(opt.UI) {
				case "boolean":
					web.RenderTemplates(w, r, ctx, "option-boolean.tmpl")
				case "pickmany":
					web.RenderTemplates(w, r, ctx, "option-pickmany.tmpl")
				default:
					web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
				}
			}
		}

		web.RenderTemplates(w, r, ctx, "option-trailer.tmpl")
	}

	web.RenderTemplates(w, r, ctx, "set-printer-options-trailer.tmpl", "trailer.tmpl")
	return nil
}

func (s *Server) applyPrinterOptionsFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing printer name")
	}

	var printer model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.GetPrinterByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		return err
	}

	ppd, _ := loadPPDForPrinter(printer)
	defaults := map[string]string{}
	if ppd != nil {
		for _, opt := range ppd.OptionDetails {
			choices := ppdOptionChoices(opt, ppd)
			if len(choices) < 2 || strings.EqualFold(opt.Keyword, "PageRegion") {
				continue
			}
			var vals []string
			if strings.EqualFold(opt.UI, "pickmany") {
				vals = r.Form[opt.Keyword]
				if len(vals) == 0 {
					continue
				}
				if opt.Custom && anyCustomSelected(vals) {
					customValues := map[string]string{}
					for _, p := range opt.CustomParams {
						field := opt.Keyword + "." + p.Name
						v, err := normalizeCustomParamValue(p, r.FormValue(field))
						if err != nil {
							return err
						}
						customValues[p.Name] = v
						defaults["custom."+opt.Keyword+"."+p.Name] = v
					}
					if len(customValues) > 0 {
						if customValue, err := buildCustomOptionValue(opt, customValues); err != nil {
							return err
						} else if customValue != "" {
							replaced := false
							for i, v := range vals {
								if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "custom") {
									vals[i] = customValue
									replaced = true
								}
							}
							if !replaced {
								vals = append(vals, customValue)
							}
						}
					}
				}
				val := strings.Join(vals, ",")
				if jobKey := ppdOptionToJobKey(opt.Keyword); jobKey != "" {
					defaults[jobKey] = normalizePPDChoice(jobKey, vals[0])
				} else {
					defaults[opt.Keyword] = val
				}
			} else {
				val := strings.TrimSpace(r.FormValue(opt.Keyword))
				if val == "" {
					continue
				}
				if opt.Custom && strings.HasPrefix(strings.ToLower(val), "custom") {
					customValues := map[string]string{}
					for _, p := range opt.CustomParams {
						field := opt.Keyword + "." + p.Name
						v, err := normalizeCustomParamValue(p, r.FormValue(field))
						if err != nil {
							return err
						}
						customValues[p.Name] = v
						defaults["custom."+opt.Keyword+"."+p.Name] = v
					}
					if len(customValues) > 0 {
						if customValue, err := buildCustomOptionValue(opt, customValues); err != nil {
							return err
						} else if customValue != "" {
							val = customValue
						}
					}
				}
				if jobKey := ppdOptionToJobKey(opt.Keyword); jobKey != "" {
					defaults[jobKey] = normalizePPDChoice(jobKey, val)
				} else {
					defaults[opt.Keyword] = val
				}
			}
		}
	}

	if val := strings.TrimSpace(r.FormValue("printer_error_policy")); val != "" {
		defaults["printer-error-policy"] = val
	}
	if val := strings.TrimSpace(r.FormValue("printer_op_policy")); val != "" {
		defaults["printer-op-policy"] = val
	}
	if val := strings.TrimSpace(r.FormValue("port_monitor")); val != "" {
		defaults["port-monitor"] = val
	}

	start := strings.TrimSpace(r.FormValue("job_sheets_start"))
	end := strings.TrimSpace(r.FormValue("job_sheets_end"))
	if start == "" {
		start = "none"
	}
	if end == "" {
		end = "none"
	}
	jobSheetsDefault := strings.Join(parseJobSheetsValues(start+","+end), ",")

	optsJSON, _ := json.Marshal(defaults)
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		if err := s.Store.UpdatePrinterDefaultOptions(r.Context(), tx, printer.ID, string(optsJSON)); err != nil {
			return err
		}
		return s.Store.UpdatePrinterJobSheetsDefault(r.Context(), tx, printer.ID, jobSheetsDefault)
	})
}

func (s *Server) renderSetClassOptions(w http.ResponseWriter, r *http.Request, name string) error {
	var class model.Class
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		class, err = s.Store.GetClassByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		return err
	}

	ctx := web.NewTemplateContext()
	ctx.SetVar("title", "Set Class Options")
	ctx.SetVar("SECTION", "admin")
	ctx.SetVar("org.cups.sid", "")
	ctx.SetVar("printer_name", class.Name)
	ctx.SetVar("op", "set-class-options-confirm")

	groups := []struct {
		ID   string
		Name string
	}{}
	if len(jobSheetsSupported()) > 0 {
		groups = append(groups, struct {
			ID   string
			Name string
		}{ID: "CUPS_BANNERS", Name: "Banners"})
	}
	if len(printerErrorPolicySupported(true)) > 0 || len(s.policyNames()) > 0 {
		groups = append(groups, struct {
			ID   string
			Name string
		}{ID: "CUPS_POLICIES", Name: "Policies"})
	}
	if len(portMonitorSupported(nil)) > 0 {
		groups = append(groups, struct {
			ID   string
			Name string
		}{ID: "CUPS_PORT_MONITOR", Name: "Port Monitor"})
	}
	groupIDs := make([]string, 0, len(groups))
	groupNames := make([]string, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		groupNames = append(groupNames, g.Name)
	}
	ctx.SetArray("group_id", groupIDs)
	ctx.SetArray("group", groupNames)

	web.RenderTemplates(w, r, ctx, "header.tmpl.in", "set-printer-options-header.tmpl")

	defaults := parseJobOptions(class.DefaultOptions)
	for _, g := range groups {
		ctx.SetVar("group_id", g.ID)
		ctx.SetVar("group", g.Name)
		web.RenderTemplates(w, r, ctx, "option-header.tmpl")

		if g.ID == "CUPS_BANNERS" {
			supported := jobSheetsSupported()
			start, end := jobSheetsPairFromDefault(class.JobSheetsDefault)

			ctx.SetArray("choices", supported)
			ctx.SetArray("text", supported)
			ctx.SetArray("conflicted", []string{"0"})
			ctx.SetArray("iscustom", []string{"0"})
			ctx.SetArray("params", []string{})
			ctx.SetArray("paramtext", []string{})
			ctx.SetArray("paramvalue", []string{})
			ctx.SetArray("inputtype", []string{})

			ctx.SetArray("keyword", []string{"job_sheets_start"})
			ctx.SetArray("keytext", []string{"Starting Banner"})
			ctx.SetArray("defchoice", []string{start})
			web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")

			ctx.SetArray("keyword", []string{"job_sheets_end"})
			ctx.SetArray("keytext", []string{"Ending Banner"})
			ctx.SetArray("defchoice", []string{end})
			web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
		} else if g.ID == "CUPS_POLICIES" {
			errorPolicies := printerErrorPolicySupported(true)
			opPolicies := s.policyNames()
			errorDefault := choiceOrDefault(defaults["printer-error-policy"], errorPolicies, defaultPrinterErrorPolicy(true))
			opDefault := choiceOrDefault(defaults["printer-op-policy"], opPolicies, defaultPrinterOpPolicy())

			ctx.SetArray("conflicted", []string{"0"})
			ctx.SetArray("iscustom", []string{"0"})
			ctx.SetArray("params", []string{})
			ctx.SetArray("paramtext", []string{})
			ctx.SetArray("paramvalue", []string{})
			ctx.SetArray("inputtype", []string{})

			if len(errorPolicies) > 0 {
				ctx.SetArray("choices", errorPolicies)
				ctx.SetArray("text", errorPolicies)
				ctx.SetArray("keyword", []string{"printer_error_policy"})
				ctx.SetArray("keytext", []string{"Error Policy"})
				ctx.SetArray("defchoice", []string{errorDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
			if len(opPolicies) > 0 {
				ctx.SetArray("choices", opPolicies)
				ctx.SetArray("text", opPolicies)
				ctx.SetArray("keyword", []string{"printer_op_policy"})
				ctx.SetArray("keytext", []string{"Operation Policy"})
				ctx.SetArray("defchoice", []string{opDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
		} else if g.ID == "CUPS_PORT_MONITOR" {
			monitors := portMonitorSupported(nil)
			if len(monitors) > 0 {
				portDefault := choiceOrDefault(defaults["port-monitor"], monitors, defaultPortMonitor())

				ctx.SetArray("conflicted", []string{"0"})
				ctx.SetArray("iscustom", []string{"0"})
				ctx.SetArray("params", []string{})
				ctx.SetArray("paramtext", []string{})
				ctx.SetArray("paramvalue", []string{})
				ctx.SetArray("inputtype", []string{})
				ctx.SetArray("choices", monitors)
				ctx.SetArray("text", monitors)
				ctx.SetArray("keyword", []string{"port_monitor"})
				ctx.SetArray("keytext", []string{"Port Monitor"})
				ctx.SetArray("defchoice", []string{portDefault})
				web.RenderTemplates(w, r, ctx, "option-pickone.tmpl")
			}
		}

		web.RenderTemplates(w, r, ctx, "option-trailer.tmpl")
	}

	web.RenderTemplates(w, r, ctx, "set-printer-options-trailer.tmpl", "trailer.tmpl")
	return nil
}

func (s *Server) applyClassOptionsFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing class name")
	}
	var class model.Class
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		class, err = s.Store.GetClassByName(r.Context(), tx, name)
		return err
	})
	if err != nil {
		return err
	}

	start := strings.TrimSpace(r.FormValue("job_sheets_start"))
	end := strings.TrimSpace(r.FormValue("job_sheets_end"))
	if start == "" {
		start = "none"
	}
	if end == "" {
		end = "none"
	}
	jobSheetsDefault := strings.Join(parseJobSheetsValues(start+","+end), ",")

	defaults := parseJobOptions(class.DefaultOptions)
	if val := strings.TrimSpace(r.FormValue("printer_error_policy")); val != "" {
		defaults["printer-error-policy"] = val
	}
	if val := strings.TrimSpace(r.FormValue("printer_op_policy")); val != "" {
		defaults["printer-op-policy"] = val
	}
	if val := strings.TrimSpace(r.FormValue("port_monitor")); val != "" {
		defaults["port-monitor"] = val
	}
	optsJSON, _ := json.Marshal(defaults)

	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		if err := s.Store.UpdateClassJobSheetsDefault(r.Context(), tx, class.ID, jobSheetsDefault); err != nil {
			return err
		}
		return s.Store.UpdateClassDefaultOptions(r.Context(), tx, class.ID, string(optsJSON))
	})
}

func (s *Server) renderAllowedUsers(w http.ResponseWriter, r *http.Request, name string, isClass bool) error {
	ctx := web.NewTemplateContext()
	ctx.SetVar("title", "Allowed Users")
	ctx.SetVar("SECTION", "admin")
	ctx.SetVar("org.cups.sid", "")
	ctx.SetVar("OP", "set-allowed-users-confirm")
	ctx.SetVar("printer_name", name)
	if isClass {
		ctx.SetVar("IS_CLASS", "1")
	}

	var allowed string
	var denied string
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		keyPrefix := "printer."
		if isClass {
			keyPrefix = "class."
		}
		id := int64(0)
		if isClass {
			c, err := s.Store.GetClassByName(r.Context(), tx, name)
			if err != nil {
				return err
			}
			id = c.ID
		} else {
			p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
			if err != nil {
				return err
			}
			id = p.ID
		}
		allowed, err = s.Store.GetSetting(r.Context(), tx, keyPrefix+strconv.FormatInt(id, 10)+".allowed_users", "")
		if err != nil {
			return err
		}
		denied, err = s.Store.GetSetting(r.Context(), tx, keyPrefix+strconv.FormatInt(id, 10)+".denied_users", "")
		return err
	})
	if err != nil {
		return err
	}

	if strings.TrimSpace(allowed) != "" {
		ctx.SetVar("requesting_user_name_allowed", allowed)
	} else if strings.TrimSpace(denied) != "" {
		ctx.SetVar("requesting_user_name_denied", denied)
	}

	web.RenderTemplates(w, r, ctx, "header.tmpl.in", "users.tmpl", "trailer.tmpl")
	return nil
}

func (s *Server) applyAllowedUsersFromForm(r *http.Request) error {
	name := r.FormValue("PRINTER_NAME")
	if name == "" {
		return fmt.Errorf("missing printer name")
	}
	isClass := r.FormValue("IS_CLASS") != ""
	users := strings.TrimSpace(r.FormValue("users"))
	policy := strings.TrimSpace(r.FormValue("type"))
	return s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
		keyPrefix := "printer."
		id := int64(0)
		if isClass {
			keyPrefix = "class."
			c, err := s.Store.GetClassByName(r.Context(), tx, name)
			if err != nil {
				return err
			}
			id = c.ID
		} else {
			p, err := s.Store.GetPrinterByName(r.Context(), tx, name)
			if err != nil {
				return err
			}
			id = p.ID
		}
		allowedKey := keyPrefix + strconv.FormatInt(id, 10) + ".allowed_users"
		deniedKey := keyPrefix + strconv.FormatInt(id, 10) + ".denied_users"
		switch policy {
		case "requesting-user-name-denied":
			if err := s.Store.SetSetting(r.Context(), tx, deniedKey, users); err != nil {
				return err
			}
			return s.Store.SetSetting(r.Context(), tx, allowedKey, "")
		default:
			if err := s.Store.SetSetting(r.Context(), tx, allowedKey, users); err != nil {
				return err
			}
			return s.Store.SetSetting(r.Context(), tx, deniedKey, "")
		}
	})
}

func (s *Server) policyNames() []string {
	names := []string{}
	if s != nil {
		for _, name := range s.Policy.Policies {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			seen := false
			for _, existing := range names {
				if strings.EqualFold(existing, name) {
					seen = true
					break
				}
			}
			if !seen {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return []string{"default"}
	}
	hasDefault := false
	for _, name := range names {
		if strings.EqualFold(name, "default") {
			hasDefault = true
			break
		}
	}
	if hasDefault {
		return names
	}
	return append([]string{"default"}, names...)
}

func sanitizePrinterName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Printer"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "Printer"
	}
	return out
}

func ppdOptionLabel(keyword string) string {
	switch strings.ToLower(keyword) {
	case "pagesize":
		return "Page Size"
	case "duplex":
		return "2-Sided Printing"
	case "resolution":
		return "Resolution"
	case "inputslot":
		return "Media Source"
	case "mediatype":
		return "Media Type"
	case "outputbin":
		return "Output Bin"
	case "colormodel", "colormode", "colorspace":
		return "Color Mode"
	default:
		return keyword
	}
}

func filterPPDOptions(options []*config.PPDOption, ppd *config.PPD) []*config.PPDOption {
	out := make([]*config.PPDOption, 0, len(options))
	for _, opt := range options {
		if opt == nil {
			continue
		}
		if strings.EqualFold(opt.Keyword, "PageRegion") {
			continue
		}
		if len(ppdOptionChoices(opt, ppd)) < 2 {
			continue
		}
		out = append(out, opt)
	}
	return out
}

func ppdOptionChoices(opt *config.PPDOption, ppd *config.PPD) []config.PPDChoice {
	if opt != nil && len(opt.Choices) > 0 {
		return opt.Choices
	}
	if ppd == nil || opt == nil {
		return nil
	}
	raw := ppd.Options[opt.Keyword]
	out := make([]config.PPDChoice, 0, len(raw))
	for _, c := range raw {
		out = append(out, config.PPDChoice{Choice: c, Text: c})
	}
	if opt.Custom {
		hasCustom := false
		for _, c := range out {
			if strings.HasPrefix(strings.ToLower(c.Choice), "custom") {
				hasCustom = true
				break
			}
		}
		if !hasCustom {
			out = append(out, config.PPDChoice{Choice: "Custom", Text: "Custom"})
		}
	}
	return out
}

func ppdDefaultSelections(opt *config.PPDOption, choices []config.PPDChoice, defaults map[string]string, ppd *config.PPD) []string {
	if opt == nil {
		return nil
	}
	desired := ""
	if jobKey := ppdOptionToJobKey(opt.Keyword); jobKey != "" {
		desired = strings.TrimSpace(defaults[jobKey])
		if desired != "" {
			if match := ppdChoiceFromJobValue(jobKey, choices, desired); match != "" {
				return []string{match}
			}
		}
	}
	if desired == "" {
		desired = strings.TrimSpace(defaults[opt.Keyword])
	}
	if desired == "" {
		if opt.Default != "" {
			desired = opt.Default
		} else if ppd != nil {
			desired = ppd.Defaults[opt.Keyword]
		}
	}
	if desired == "" && len(choices) > 0 {
		return []string{choices[0].Choice}
	}
	if strings.Contains(desired, ",") {
		values := splitList(desired)
		for i, v := range values {
			v = strings.TrimSpace(v)
			if strings.HasPrefix(strings.ToLower(v), "custom") || (strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}")) {
				values[i] = "Custom"
			}
		}
		return values
	}
	trimmed := strings.TrimSpace(desired)
	if strings.HasPrefix(strings.ToLower(trimmed), "custom") || (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) {
		return []string{"Custom"}
	}
	return []string{desired}
}

func ppdChoiceFromJobValue(jobKey string, choices []config.PPDChoice, desired string) string {
	for _, c := range choices {
		if strings.EqualFold(normalizePPDChoice(jobKey, c.Choice), desired) {
			return c.Choice
		}
	}
	for _, c := range choices {
		if strings.EqualFold(c.Choice, desired) {
			return c.Choice
		}
	}
	return ""
}

func splitList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
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

func customParamDefaults(defaults map[string]string, opt *config.PPDOption) map[string]string {
	out := map[string]string{}
	if opt == nil {
		return out
	}
	prefix := "custom." + opt.Keyword + "."
	for k, v := range defaults {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	if len(out) > 0 {
		return out
	}
	value := strings.TrimSpace(defaults[opt.Keyword])
	if jobKey := ppdOptionToJobKey(opt.Keyword); jobKey != "" {
		if v := strings.TrimSpace(defaults[jobKey]); v != "" {
			value = v
		}
	}
	if value == "" {
		return out
	}
	return parseCustomOptionValue(opt, value)
}

func anyCustomSelected(values []string) bool {
	for _, v := range values {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "custom") {
			return true
		}
	}
	return false
}

func customUnitHint(selected []string) string {
	for _, s := range selected {
		ls := strings.ToLower(s)
		switch {
		case strings.Contains(ls, "mm"):
			return "mm"
		case strings.Contains(ls, "cm"):
			return "cm"
		case strings.Contains(ls, "in"):
			return "in"
		case strings.Contains(ls, "ft"):
			return "ft"
		case strings.Contains(ls, "m"):
			return "m"
		}
	}
	return ""
}

func normalizeCustomParamValue(param config.PPDCustomParam, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		if param.Range && param.Min == param.Max {
			switch strings.ToLower(param.Type) {
			case "int":
				value = strconv.Itoa(int(param.Min))
			case "real", "points":
				value = strconv.FormatFloat(param.Min, 'f', -1, 64)
			case "units":
				value = "pt"
			}
		}
		if value == "" {
			return "", fmt.Errorf("missing custom parameter %s", param.Name)
		}
	}

	switch strings.ToLower(param.Type) {
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return "", fmt.Errorf("invalid integer for %s", param.Name)
		}
		if param.Range && (float64(n) < param.Min || float64(n) > param.Max) {
			return "", fmt.Errorf("value for %s out of range", param.Name)
		}
	case "real", "points":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", fmt.Errorf("invalid number for %s", param.Name)
		}
		if param.Range && (f < param.Min || f > param.Max) {
			return "", fmt.Errorf("value for %s out of range", param.Name)
		}
	case "string", "password":
		if param.Range {
			l := utf8.RuneCountInString(value)
			if l < int(param.Min) || l > int(param.Max) {
				return "", fmt.Errorf("value for %s out of range", param.Name)
			}
		}
	case "units":
		switch strings.ToLower(value) {
		case "pt", "mm", "cm", "in", "ft", "m":
		default:
			return "", fmt.Errorf("invalid units for %s", param.Name)
		}
	}
	return value, nil
}

func buildCustomOptionValue(opt *config.PPDOption, values map[string]string) (string, error) {
	if opt == nil || len(opt.CustomParams) == 0 {
		return "", fmt.Errorf("custom parameters not defined")
	}
	params := orderedCustomParams(opt.CustomParams)
	units := ""
	for _, p := range params {
		if strings.EqualFold(p.Name, "Units") || strings.EqualFold(p.Type, "units") {
			units = strings.TrimSpace(values[p.Name])
		}
	}
	if units == "" {
		units = "pt"
	}

	if strings.EqualFold(opt.Keyword, "PageSize") {
		width := strings.TrimSpace(values["Width"])
		height := strings.TrimSpace(values["Height"])
		if width == "" || height == "" {
			return "", fmt.Errorf("missing custom page size")
		}
		wNum, err := strconv.ParseFloat(width, 64)
		if err != nil {
			return "", fmt.Errorf("invalid custom page size width")
		}
		hNum, err := strconv.ParseFloat(height, 64)
		if err != nil {
			return "", fmt.Errorf("invalid custom page size height")
		}
		return fmt.Sprintf("Custom.%sx%s%s", formatCustomNumber(wNum), formatCustomNumber(hNum), units), nil
	}

	nonUnits := make([]config.PPDCustomParam, 0, len(params))
	for _, p := range params {
		if strings.EqualFold(p.Name, "Units") || strings.EqualFold(p.Type, "units") {
			continue
		}
		nonUnits = append(nonUnits, p)
	}
	if len(nonUnits) == 1 {
		p := nonUnits[0]
		val := strings.TrimSpace(values[p.Name])
		if val == "" {
			return "", fmt.Errorf("missing custom parameter %s", p.Name)
		}
		return "Custom." + formatCustomParamValue(p, val, units, false), nil
	}

	var sb strings.Builder
	sb.WriteString("{")
	first := true
	for _, p := range nonUnits {
		val := strings.TrimSpace(values[p.Name])
		if val == "" {
			return "", fmt.Errorf("missing custom parameter %s", p.Name)
		}
		if !first {
			sb.WriteString(" ")
		}
		first = false
		sb.WriteString(p.Name)
		sb.WriteString("=")
		sb.WriteString(formatCustomParamValue(p, val, units, true))
	}
	sb.WriteString("}")
	return sb.String(), nil
}

func orderedCustomParams(params []config.PPDCustomParam) []config.PPDCustomParam {
	if len(params) == 0 {
		return params
	}
	out := make([]config.PPDCustomParam, len(params))
	copy(out, params)
	hasOrder := false
	for _, p := range out {
		if p.Order > 0 {
			hasOrder = true
			break
		}
	}
	if !hasOrder {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := out[i].Order, out[j].Order
		if oi > 0 && oj > 0 {
			return oi < oj
		}
		if oi > 0 && oj == 0 {
			return true
		}
		if oi == 0 && oj > 0 {
			return false
		}
		return false
	})
	return out
}

func formatCustomParamValue(param config.PPDCustomParam, value, units string, quoted bool) string {
	switch strings.ToLower(param.Type) {
	case "points":
		return value + units
	case "string", "password":
		if !quoted {
			return value
		}
		return `"` + escapeCustomString(value) + `"`
	default:
		return value
	}
}

func escapeCustomString(value string) string {
	var sb strings.Builder
	for _, r := range value {
		if r == '\\' || r == '"' {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func formatCustomNumber(val float64) string {
	return strconv.FormatFloat(val, 'f', -1, 64)
}

var customPageSizeRe = regexp.MustCompile(`(?i)^custom\.([0-9.]+)x([0-9.]+)([a-z]+)?$`)

func parseCustomOptionValue(opt *config.PPDOption, value string) map[string]string {
	out := map[string]string{}
	if opt == nil || strings.TrimSpace(value) == "" {
		return out
	}
	raw := strings.TrimSpace(value)
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "custom.") {
		if strings.EqualFold(opt.Keyword, "PageSize") {
			if m := customPageSizeRe.FindStringSubmatch(raw); len(m) >= 3 {
				out["Width"] = m[1]
				out["Height"] = m[2]
				if len(m) >= 4 && m[3] != "" {
					out["Units"] = strings.ToLower(m[3])
				}
			}
			return out
		}
		rest := raw[len("Custom."):]
		rest = strings.TrimSpace(rest)
		if len(opt.CustomParams) == 1 {
			p := opt.CustomParams[0]
			if strings.EqualFold(p.Type, "points") {
				num, unit := splitNumericSuffix(rest)
				if unit != "" {
					out["Units"] = unit
				}
				out[p.Name] = num
			} else {
				out[p.Name] = rest
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		parsed := parseCustomDict(inner)
		for k, v := range parsed {
			out[k] = v
		}
		for _, p := range opt.CustomParams {
			if !strings.EqualFold(p.Type, "points") {
				continue
			}
			if v, ok := out[p.Name]; ok {
				if num, unit := splitNumericSuffix(v); num != "" {
					out[p.Name] = num
					if unit != "" {
						out["Units"] = unit
					}
				}
			}
		}
		return out
	}
	return out
}

func parseCustomDict(value string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(value) {
		for i < len(value) && value[i] == ' ' {
			i++
		}
		if i >= len(value) {
			break
		}
		start := i
		for i < len(value) && value[i] != '=' && value[i] != ' ' {
			i++
		}
		if i >= len(value) || value[i] != '=' {
			break
		}
		key := strings.TrimSpace(value[start:i])
		i++
		if i >= len(value) {
			out[key] = ""
			break
		}
		var val string
		if value[i] == '"' {
			i++
			var sb strings.Builder
			for i < len(value) {
				if value[i] == '\\' && i+1 < len(value) {
					i++
					sb.WriteByte(value[i])
					i++
					continue
				}
				if value[i] == '"' {
					i++
					break
				}
				sb.WriteByte(value[i])
				i++
			}
			val = sb.String()
		} else {
			startVal := i
			for i < len(value) && value[i] != ' ' {
				i++
			}
			val = strings.TrimSpace(value[startVal:i])
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func splitNumericSuffix(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	i := len(value)
	for i > 0 {
		r := value[i-1]
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			i--
			continue
		}
		break
	}
	num := strings.TrimSpace(value[:i])
	unit := strings.ToLower(strings.TrimSpace(value[i:]))
	return num, unit
}

func jobSheetsPairFromDefault(value string) (string, string) {
	values := parseJobSheetsValues(value)
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

func (s *Server) printTestPage(r *http.Request, printerName string) error {
	var printer model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		var err error
		printer, err = s.Store.GetPrinterByName(r.Context(), tx, printerName)
		return err
	})
	if err != nil {
		return err
	}
	return s.enqueueTestPage(r.Context(), printer)
}

func (s *Server) printTestPageForClass(r *http.Request, className string) error {
	var member model.Printer
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		class, err := s.Store.GetClassByName(r.Context(), tx, className)
		if err != nil {
			return err
		}
		members, err := s.Store.ListClassMembers(r.Context(), tx, class.ID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no class members")
		}
		member = members[0]
		return nil
	})
	if err != nil {
		return err
	}
	return s.enqueueTestPage(r.Context(), member)
}

func (s *Server) enqueueTestPage(ctx context.Context, printer model.Printer) error {
	opts := applyPrinterDefaults(map[string]string{}, printer)
	optionsJSON, _ := json.Marshal(opts)
	jobName := fmt.Sprintf("Test Page %s", time.Now().Format("2006-01-02 15:04:05"))
	content := strings.Builder{}
	content.WriteString("CUPS-Golang Test Page\n")
	content.WriteString("=====================\n")
	content.WriteString("Printer: ")
	content.WriteString(printer.Name)
	content.WriteString("\nTime: ")
	content.WriteString(time.Now().Format(time.RFC1123))
	content.WriteString("\n")
	sp := spool.Spool{Dir: s.Spool.Dir, OutputDir: s.Spool.OutputDir}
	return s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		job, err := s.Store.CreateJob(ctx, tx, printer.ID, jobName, "admin", string(optionsJSON))
		if err != nil {
			return err
		}
		path, size, err := sp.Save(job.ID, "test-page.txt", strings.NewReader(content.String()))
		if err != nil {
			return err
		}
		_, err = s.Store.AddDocument(ctx, tx, job.ID, "test-page.txt", "text/plain", path, size)
		return err
	})
}
