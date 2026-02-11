package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"cupsgolang/internal/model"
)

type Store struct {
	db        *sql.DB
	MaxEvents int
}

type PrinterSupplies struct {
	PrinterID int64
	State     string
	Details   map[string]string
	UpdatedAt time.Time
}

type DocumentStats struct {
	JobID     int64
	Count     int
	SizeBytes int64
	MimeType  string
}

var ErrJobCompleted = errors.New("job completed")

func Open(ctx context.Context, dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) WithTx(ctx context.Context, readOnly bool, fn func(tx *sql.Tx) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store not initialized")
	}
	opts := &sql.TxOptions{ReadOnly: readOnly}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) EnsureDefaultPrinter(ctx context.Context) error {
	return s.WithTx(ctx, false, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM printers").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `
            INSERT INTO printers (name, uri, ppd_name, location, info, state, accepting, is_default, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        `, "Default", "http://localhost:631/printers/Default", model.DefaultPPDName, "", "Default Printer", 3, 1, 1, now, now)
		return err
	})
}

func (s *Store) EnsureAdminUser(ctx context.Context) error {
	user := os.Getenv("CUPS_ADMIN_USER")
	pass := os.Getenv("CUPS_ADMIN_PASS")
	if user == "" {
		user = "admin"
	}
	if pass == "" {
		pass = "admin"
	}
	return s.WithTx(ctx, false, func(tx *sql.Tx) error {
		u, err := s.GetUserByUsername(ctx, tx, user)
		if err == nil {
			if u.DigestHA1 == "" && pass != "" && checkPassword(u.PasswordHash, pass) == nil {
				digest := digestHA1(user, pass)
				_, _ = tx.ExecContext(ctx, `UPDATE users SET digest_ha1 = ? WHERE username = ?`, digest, user)
			}
			return nil
		}
		return s.CreateUser(ctx, tx, user, pass, true)
	})
}

func (s *Store) ListPrinters(ctx context.Context, tx *sql.Tx) ([]model.Printer, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_temporary, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        ORDER BY name
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	printers := []model.Printer{}
	for rows.Next() {
		var p model.Printer
		var accepting int
		var shared int
		var temporary int
		var isDefault int
		var jobSheets string
		var defaultOptions string
		var ppdName string
		if err := rows.Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Accepting = accepting != 0
		p.Shared = shared != 0
		p.IsTemporary = temporary != 0
		p.IsDefault = isDefault != 0
		p.PPDName = strings.TrimSpace(ppdName)
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		p.JobSheetsDefault = jobSheets
		p.DefaultOptions = defaultOptions
		printers = append(printers, p)
	}
	return printers, rows.Err()
}

func (s *Store) ListTemporaryPrinters(ctx context.Context, tx *sql.Tx, force bool, unusedBefore time.Time, limit int) ([]model.Printer, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_temporary, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        WHERE is_temporary = 1
    `
	args := []any{}
	if !force {
		query += " AND state != 4 AND updated_at < ?"
		args = append(args, unusedBefore.UTC())
	}
	query += " ORDER BY updated_at LIMIT ?"
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Printer{}
	for rows.Next() {
		var p model.Printer
		var accepting int
		var shared int
		var temporary int
		var isDefault int
		var jobSheets string
		var defaultOptions string
		var ppdName string
		if err := rows.Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Accepting = accepting != 0
		p.Shared = shared != 0
		p.IsTemporary = temporary != 0
		p.IsDefault = isDefault != 0
		p.PPDName = strings.TrimSpace(ppdName)
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		p.JobSheetsDefault = jobSheets
		p.DefaultOptions = defaultOptions
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateClass(ctx context.Context, tx *sql.Tx, name, location, info string, accepting bool, isDefault bool, memberPrinterIDs []int64) (model.Class, error) {
	now := time.Now().UTC()
	acc := 0
	def := 0
	if accepting {
		acc = 1
	}
	if isDefault {
		def = 1
	}
	res, err := tx.ExecContext(ctx, `
        INSERT INTO classes (name, location, info, state, accepting, is_default, job_sheets_default, default_options, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, name, location, info, 3, acc, def, "none", "", now, now)
	if err != nil {
		return model.Class{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Class{}, err
	}
	for _, pid := range memberPrinterIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO class_members (class_id, printer_id) VALUES (?, ?)`, id, pid); err != nil {
			return model.Class{}, err
		}
	}
	return model.Class{
		ID:               id,
		Name:             name,
		Location:         location,
		Info:             info,
		State:            3,
		Accepting:        accepting,
		IsDefault:        isDefault,
		JobSheetsDefault: "none",
		DefaultOptions:   "",
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

func (s *Store) UpsertClass(ctx context.Context, tx *sql.Tx, name, location, info string, accepting bool, memberPrinterIDs []int64) (model.Class, error) {
	existing, err := s.GetClassByName(ctx, tx, name)
	if err == nil {
		acc := 0
		if accepting {
			acc = 1
		}
		_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET location = ?, info = ?, accepting = ?, updated_at = ?
        WHERE id = ?
    `, location, info, acc, time.Now().UTC(), existing.ID)
		if err != nil {
			return model.Class{}, err
		}
		if err := s.ReplaceClassMembers(ctx, tx, existing.ID, memberPrinterIDs); err != nil {
			return model.Class{}, err
		}
		existing.Location = location
		existing.Info = info
		existing.Accepting = accepting
		existing.UpdatedAt = time.Now().UTC()
		return existing, nil
	}
	return s.CreateClass(ctx, tx, name, location, info, accepting, false, memberPrinterIDs)
}

func (s *Store) ReplaceClassMembers(ctx context.Context, tx *sql.Tx, classID int64, memberPrinterIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM class_members WHERE class_id = ?`, classID); err != nil {
		return err
	}
	for _, pid := range memberPrinterIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO class_members (class_id, printer_id) VALUES (?, ?)`, classID, pid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListClasses(ctx context.Context, tx *sql.Tx) ([]model.Class, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, name, location, info, state, accepting, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM classes
        ORDER BY name
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	classes := []model.Class{}
	for rows.Next() {
		var c model.Class
		var accepting int
		var isDefault int
		var jobSheets string
		var defaultOptions string
		if err := rows.Scan(&c.ID, &c.Name, &c.Location, &c.Info, &c.State, &accepting, &isDefault, &jobSheets, &defaultOptions, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Accepting = accepting != 0
		c.IsDefault = isDefault != 0
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		c.JobSheetsDefault = jobSheets
		c.DefaultOptions = defaultOptions
		classes = append(classes, c)
	}
	return classes, rows.Err()
}

func (s *Store) GetClassByName(ctx context.Context, tx *sql.Tx, name string) (model.Class, error) {
	var c model.Class
	var accepting int
	var isDefault int
	var jobSheets string
	var defaultOptions string
	err := tx.QueryRowContext(ctx, `
        SELECT id, name, location, info, state, accepting, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM classes
        WHERE name = ?
    `, name).Scan(&c.ID, &c.Name, &c.Location, &c.Info, &c.State, &accepting, &isDefault, &jobSheets, &defaultOptions, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return model.Class{}, err
	}
	c.Accepting = accepting != 0
	c.IsDefault = isDefault != 0
	if strings.TrimSpace(jobSheets) == "" {
		jobSheets = "none"
	}
	c.JobSheetsDefault = jobSheets
	c.DefaultOptions = defaultOptions
	return c, nil
}

func (s *Store) ListClassMembers(ctx context.Context, tx *sql.Tx, classID int64) ([]model.Printer, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT p.id, p.name, p.uri, p.ppd_name, p.location, p.info, p.geo_location, p.organization, p.organizational_unit, p.state, p.accepting, p.shared, p.is_temporary, p.is_default, p.job_sheets_default, p.default_options, p.created_at, p.updated_at
        FROM class_members cm
        JOIN printers p ON p.id = cm.printer_id
        WHERE cm.class_id = ?
        ORDER BY p.name
    `, classID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	printers := []model.Printer{}
	for rows.Next() {
		var p model.Printer
		var accepting int
		var shared int
		var temporary int
		var isDefault int
		var jobSheets string
		var defaultOptions string
		var ppdName string
		if err := rows.Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Accepting = accepting != 0
		p.Shared = shared != 0
		p.IsTemporary = temporary != 0
		p.IsDefault = isDefault != 0
		p.PPDName = strings.TrimSpace(ppdName)
		if strings.TrimSpace(jobSheets) == "" {
			jobSheets = "none"
		}
		p.JobSheetsDefault = jobSheets
		p.DefaultOptions = defaultOptions
		printers = append(printers, p)
	}
	return printers, rows.Err()
}

func (s *Store) UpdateClassAccepting(ctx context.Context, tx *sql.Tx, id int64, accepting bool) error {
	acc := 0
	if accepting {
		acc = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET accepting = ?, updated_at = ?
        WHERE id = ?
    `, acc, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateClassState(ctx context.Context, tx *sql.Tx, id int64, state int) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET state = ?, updated_at = ?
        WHERE id = ?
    `, state, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateClassJobSheetsDefault(ctx context.Context, tx *sql.Tx, id int64, sheets string) error {
	if strings.TrimSpace(sheets) == "" {
		sheets = "none"
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET job_sheets_default = ?, updated_at = ?
        WHERE id = ?
    `, sheets, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateClassDefaultOptions(ctx context.Context, tx *sql.Tx, id int64, optionsJSON string) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET default_options = ?, updated_at = ?
        WHERE id = ?
    `, optionsJSON, time.Now().UTC(), id)
	return err
}

func (s *Store) DeleteClass(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM classes WHERE id = ?`, id)
	return err
}

func (s *Store) SetDefaultClass(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE printers SET is_default = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE classes SET is_default = 0`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET is_default = 1, updated_at = ?
        WHERE id = ?
    `, time.Now().UTC(), id)
	return err
}
func (s *Store) GetPrinterByName(ctx context.Context, tx *sql.Tx, name string) (model.Printer, error) {
	var p model.Printer
	var accepting int
	var isDefault int
	var shared int
	var temporary int
	var jobSheets string
	var defaultOptions string
	var ppdName string
	err := tx.QueryRowContext(ctx, `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_temporary, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        WHERE name = ?
    `, name).Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return model.Printer{}, err
	}
	p.Accepting = accepting != 0
	p.Shared = shared != 0
	p.IsTemporary = temporary != 0
	p.IsDefault = isDefault != 0
	p.PPDName = strings.TrimSpace(ppdName)
	if strings.TrimSpace(jobSheets) == "" {
		jobSheets = "none"
	}
	p.JobSheetsDefault = jobSheets
	p.DefaultOptions = defaultOptions
	return p, nil
}

func (s *Store) GetPrinterByID(ctx context.Context, tx *sql.Tx, id int64) (model.Printer, error) {
	var p model.Printer
	var accepting int
	var isDefault int
	var shared int
	var temporary int
	var jobSheets string
	var defaultOptions string
	var ppdName string
	err := tx.QueryRowContext(ctx, `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_temporary, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        WHERE id = ?
    `, id).Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return model.Printer{}, err
	}
	p.Accepting = accepting != 0
	p.Shared = shared != 0
	p.IsTemporary = temporary != 0
	p.IsDefault = isDefault != 0
	p.PPDName = strings.TrimSpace(ppdName)
	if strings.TrimSpace(jobSheets) == "" {
		jobSheets = "none"
	}
	p.JobSheetsDefault = jobSheets
	p.DefaultOptions = defaultOptions
	return p, nil
}

func (s *Store) GetPrinterByURI(ctx context.Context, tx *sql.Tx, uri string) (model.Printer, error) {
	var p model.Printer
	var accepting int
	var isDefault int
	var shared int
	var temporary int
	var jobSheets string
	var defaultOptions string
	var ppdName string
	err := tx.QueryRowContext(ctx, `
        SELECT id, name, uri, ppd_name, location, info, geo_location, organization, organizational_unit, state, accepting, shared, is_temporary, is_default, job_sheets_default, default_options, created_at, updated_at
        FROM printers
        WHERE uri = ?
    `, uri).Scan(&p.ID, &p.Name, &p.URI, &ppdName, &p.Location, &p.Info, &p.Geo, &p.Org, &p.OrgUnit, &p.State, &accepting, &shared, &temporary, &isDefault, &jobSheets, &defaultOptions, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return model.Printer{}, err
	}
	p.Accepting = accepting != 0
	p.Shared = shared != 0
	p.IsTemporary = temporary != 0
	p.IsDefault = isDefault != 0
	p.PPDName = strings.TrimSpace(ppdName)
	if strings.TrimSpace(jobSheets) == "" {
		jobSheets = "none"
	}
	p.JobSheetsDefault = jobSheets
	p.DefaultOptions = defaultOptions
	return p, nil
}

func (s *Store) TouchPrinter(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET updated_at = ?
        WHERE id = ?
    `, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdatePrinterTemporary(ctx context.Context, tx *sql.Tx, id int64, temporary bool) error {
	val := 0
	if temporary {
		val = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET is_temporary = ?, updated_at = ?
        WHERE id = ?
    `, val, time.Now().UTC(), id)
	return err
}

func (s *Store) DeleteTemporaryPrinters(ctx context.Context, tx *sql.Tx, force bool, unusedBefore time.Time) (int64, error) {
	stmt := `DELETE FROM printers WHERE is_temporary = 1`
	args := []any{}
	if !force {
		stmt += ` AND state != 4 AND updated_at < ?`
		args = append(args, unusedBefore.UTC())
	}
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (s *Store) CreateJob(ctx context.Context, tx *sql.Tx, printerID int64, name, user, originHost, options string) (model.Job, error) {
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
        INSERT INTO jobs (printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, printerID, name, user, originHost, options, 3, "job-incoming", 0, now)
	if err != nil {
		return model.Job{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Job{}, err
	}
	_ = s.addNotificationForJob(ctx, tx, id, "job-created")
	return model.Job{
		ID:          id,
		PrinterID:   printerID,
		Name:        name,
		UserName:    user,
		OriginHost:  originHost,
		Options:     options,
		State:       3,
		StateReason: "job-incoming",
		Impressions: 0,
		SubmittedAt: now,
	}, nil
}

func (s *Store) AddDocument(ctx context.Context, tx *sql.Tx, jobID int64, fileName, mimeType, path string, sizeBytes int64, nameSupplied, formatSupplied string) (model.Document, error) {
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
        INSERT INTO documents (job_id, file_name, mime_type, format_supplied, name_supplied, size_bytes, path, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
    `, jobID, fileName, mimeType, formatSupplied, nameSupplied, sizeBytes, path, now)
	if err != nil {
		return model.Document{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Document{}, err
	}
	return model.Document{
		ID:             id,
		JobID:          jobID,
		FileName:       fileName,
		MimeType:       mimeType,
		FormatSupplied: formatSupplied,
		NameSupplied:   nameSupplied,
		SizeBytes:      sizeBytes,
		Path:           path,
		CreatedAt:      now,
	}, nil
}

func (s *Store) GetJob(ctx context.Context, tx *sql.Tx, jobID int64) (model.Job, error) {
	var job model.Job
	var processing sql.NullTime
	var completed sql.NullTime
	err := tx.QueryRowContext(ctx, `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE id = ?
    `, jobID).Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed)
	if err != nil {
		return model.Job{}, err
	}
	if processing.Valid {
		job.ProcessingAt = &processing.Time
	}
	if completed.Valid {
		job.CompletedAt = &completed.Time
	}
	return job, nil
}

func (s *Store) ListJobsByPrinter(ctx context.Context, tx *sql.Tx, printerID int64, limit int) ([]model.Job, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE printer_id = ?
        ORDER BY id DESC
        LIMIT ?
    `, printerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []model.Job{}
	for rows.Next() {
		var job model.Job
		var processing sql.NullTime
		var completed sql.NullTime
		if err := rows.Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed); err != nil {
			return nil, err
		}
		if processing.Valid {
			job.ProcessingAt = &processing.Time
		}
		if completed.Valid {
			job.CompletedAt = &completed.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListJobsByUser(ctx context.Context, tx *sql.Tx, user string, printerID *int64, limit int) ([]model.Job, error) {
	query := `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE user_name = ?
    `
	args := []any{user}
	if printerID != nil {
		query += " AND printer_id = ?"
		args = append(args, *printerID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []model.Job{}
	for rows.Next() {
		var job model.Job
		var processing sql.NullTime
		var completed sql.NullTime
		if err := rows.Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed); err != nil {
			return nil, err
		}
		if processing.Valid {
			job.ProcessingAt = &processing.Time
		}
		if completed.Valid {
			job.CompletedAt = &completed.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) UpdateJobState(ctx context.Context, tx *sql.Tx, jobID int64, state int, reason string, completedAt *time.Time) error {
	var completed sql.NullTime
	if completedAt != nil {
		completed = sql.NullTime{Time: *completedAt, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE jobs
        SET state = ?, state_reason = ?, completed_at = ?
        WHERE id = ?
    `, state, reason, completed, jobID)
	if err == nil {
		_ = s.addNotificationForJob(ctx, tx, jobID, "job-state-changed")
		switch state {
		case 9:
			_ = s.addNotificationForJob(ctx, tx, jobID, "job-completed")
		case 5:
			_ = s.addNotificationForJob(ctx, tx, jobID, "job-progress")
		case 4, 6, 7, 8:
			_ = s.addNotificationForJob(ctx, tx, jobID, "job-stopped")
		}
	}
	return err
}

func (s *Store) UpdateJobAttributes(ctx context.Context, tx *sql.Tx, jobID int64, name *string, options *string) error {
	fields := []string{}
	args := []any{}
	if name != nil {
		fields = append(fields, "name = ?")
		args = append(args, *name)
	}
	if options != nil {
		fields = append(fields, "options = ?")
		args = append(args, *options)
	}
	if len(fields) == 0 {
		return nil
	}
	args = append(args, jobID)
	_, err := tx.ExecContext(ctx, `
        UPDATE jobs
        SET `+strings.Join(fields, ", ")+`
        WHERE id = ?
    `, args...)
	if err == nil {
		_ = s.addNotificationForJob(ctx, tx, jobID, "job-config-changed")
	}
	return err
}

func (s *Store) MoveJob(ctx context.Context, tx *sql.Tx, jobID int64, printerID int64) (model.Job, error) {
	job, err := s.GetJob(ctx, tx, jobID)
	if err != nil {
		return model.Job{}, err
	}
	if job.State >= 7 {
		return job, ErrJobCompleted
	}
	_, err = tx.ExecContext(ctx, `
        UPDATE jobs
        SET printer_id = ?, state = ?, state_reason = ?, completed_at = NULL
        WHERE id = ?
    `, printerID, 3, "job-moved", jobID)
	if err != nil {
		return model.Job{}, err
	}
	job.PrinterID = printerID
	job.State = 3
	job.StateReason = "job-moved"
	job.CompletedAt = nil
	_ = s.addNotificationForJob(ctx, tx, jobID, "job-config-changed")
	_ = s.addNotificationForJob(ctx, tx, jobID, "job-state-changed")
	return job, nil
}

func (s *Store) CreateUser(ctx context.Context, tx *sql.Tx, username, password string, admin bool) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	digest := digestHA1(username, password)
	now := time.Now().UTC()
	adminInt := 0
	if admin {
		adminInt = 1
	}
	_, err = tx.ExecContext(ctx, `
        INSERT INTO users (username, password_hash, digest_ha1, is_admin, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?)
    `, username, hash, digest, adminInt, now, now)
	return err
}

func (s *Store) GetUserByUsername(ctx context.Context, tx *sql.Tx, username string) (model.User, error) {
	var u model.User
	var isAdmin int
	err := tx.QueryRowContext(ctx, `
        SELECT id, username, password_hash, digest_ha1, is_admin, created_at, updated_at
        FROM users
        WHERE username = ?
    `, username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DigestHA1, &isAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return model.User{}, err
	}
	u.IsAdmin = isAdmin != 0
	return u, nil
}

func (s *Store) VerifyUser(ctx context.Context, tx *sql.Tx, username, password string) (model.User, error) {
	u, err := s.GetUserByUsername(ctx, tx, username)
	if err != nil {
		return model.User{}, err
	}
	if err := checkPassword(u.PasswordHash, password); err != nil {
		return model.User{}, err
	}
	return u, nil
}

func (s *Store) SetSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
        INSERT INTO settings (key, value) VALUES (?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value
    `, key, value)
	return err
}

func (s *Store) GetSetting(ctx context.Context, tx *sql.Tx, key, fallback string) (string, error) {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	return v, nil
}

func (s *Store) ListSettings(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) CreatePrinter(ctx context.Context, tx *sql.Tx, name, uri, location, info, ppdName string, accepting bool, isDefault bool, shared bool, jobSheetsDefault, defaultOptions string) (model.Printer, error) {
	now := time.Now().UTC()
	acc := 0
	def := 0
	sh := 0
	if accepting {
		acc = 1
	}
	if isDefault {
		def = 1
	}
	if shared {
		sh = 1
	}
	if strings.TrimSpace(jobSheetsDefault) == "" {
		jobSheetsDefault = "none"
	}
	ppdName = strings.TrimSpace(ppdName)
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}
	res, err := tx.ExecContext(ctx, `
        INSERT INTO printers (name, uri, ppd_name, location, info, state, accepting, shared, is_default, job_sheets_default, default_options, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, name, uri, ppdName, location, info, 3, acc, sh, def, jobSheetsDefault, defaultOptions, now, now)
	if err != nil {
		return model.Printer{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Printer{}, err
	}
	_ = s.addNotificationForPrinter(ctx, tx, id, "printer-added")
	return model.Printer{
		ID:               id,
		Name:             name,
		URI:              uri,
		PPDName:          ppdName,
		Location:         location,
		Info:             info,
		State:            3,
		Accepting:        accepting,
		Shared:           shared,
		IsDefault:        isDefault,
		JobSheetsDefault: jobSheetsDefault,
		DefaultOptions:   defaultOptions,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

func (s *Store) UpsertPrinter(ctx context.Context, tx *sql.Tx, name, uri, location, info string, accepting bool) (model.Printer, error) {
	existing, err := s.GetPrinterByName(ctx, tx, name)
	if err == nil {
		acc := 0
		if accepting {
			acc = 1
		}
		_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET uri = ?, location = ?, info = ?, accepting = ?, updated_at = ?
        WHERE id = ?
    `, uri, location, info, acc, time.Now().UTC(), existing.ID)
		if err != nil {
			return model.Printer{}, err
		}
		existing.URI = uri
		existing.Location = location
		existing.Info = info
		existing.Accepting = accepting
		existing.UpdatedAt = time.Now().UTC()
		_ = s.addNotificationForPrinter(ctx, tx, existing.ID, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, existing.ID, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, existing.ID, "printer-modified")
		return existing, nil
	}
	return s.CreatePrinter(ctx, tx, name, uri, location, info, "", accepting, false, true, "none", "")
}

func (s *Store) UpdatePrinterAttributes(ctx context.Context, tx *sql.Tx, id int64, info, location, geo, org, orgUnit *string) error {
	fields := []string{}
	args := []any{}
	if info != nil {
		fields = append(fields, "info = ?")
		args = append(args, *info)
	}
	if location != nil {
		fields = append(fields, "location = ?")
		args = append(args, *location)
	}
	if geo != nil {
		fields = append(fields, "geo_location = ?")
		args = append(args, *geo)
	}
	if org != nil {
		fields = append(fields, "organization = ?")
		args = append(args, *org)
	}
	if orgUnit != nil {
		fields = append(fields, "organizational_unit = ?")
		args = append(args, *orgUnit)
	}
	if len(fields) == 0 {
		return nil
	}
	fields = append(fields, "updated_at = ?")
	args = append(args, time.Now().UTC(), id)
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET `+strings.Join(fields, ", ")+`
        WHERE id = ?
    `, args...)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) UpdatePrinterSharing(ctx context.Context, tx *sql.Tx, id int64, shared bool) error {
	val := 0
	if shared {
		val = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET shared = ?, updated_at = ?
        WHERE id = ?
    `, val, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) UpdatePrinterPPDName(ctx context.Context, tx *sql.Tx, id int64, ppdName string) error {
	ppdName = strings.TrimSpace(ppdName)
	if ppdName == "" {
		ppdName = model.DefaultPPDName
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET ppd_name = ?, updated_at = ?
        WHERE id = ?
    `, ppdName, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) UpdatePrinterJobSheetsDefault(ctx context.Context, tx *sql.Tx, id int64, sheets string) error {
	if strings.TrimSpace(sheets) == "" {
		sheets = "none"
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET job_sheets_default = ?, updated_at = ?
        WHERE id = ?
    `, sheets, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) UpdatePrinterDefaultOptions(ctx context.Context, tx *sql.Tx, id int64, optionsJSON string) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET default_options = ?, updated_at = ?
        WHERE id = ?
    `, optionsJSON, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) GetPrinterSupplies(ctx context.Context, tx *sql.Tx, printerID int64) (PrinterSupplies, bool, error) {
	var state string
	var details string
	var updated time.Time
	err := tx.QueryRowContext(ctx, `
        SELECT state, details, updated_at
        FROM printer_supplies
        WHERE printer_id = ?
    `, printerID).Scan(&state, &details, &updated)
	if err == sql.ErrNoRows {
		return PrinterSupplies{}, false, nil
	}
	if err != nil {
		return PrinterSupplies{}, false, err
	}
	out := PrinterSupplies{
		PrinterID: printerID,
		State:     state,
		UpdatedAt: updated,
		Details:   map[string]string{},
	}
	if strings.TrimSpace(details) != "" {
		_ = json.Unmarshal([]byte(details), &out.Details)
	}
	return out, true, nil
}

func (s *Store) UpsertPrinterSupplies(ctx context.Context, tx *sql.Tx, printerID int64, state string, details map[string]string, updated time.Time) error {
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	raw := ""
	if details != nil {
		if b, err := json.Marshal(details); err == nil {
			raw = string(b)
		}
	}
	_, err := tx.ExecContext(ctx, `
        INSERT INTO printer_supplies (printer_id, state, details, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(printer_id) DO UPDATE SET state = excluded.state, details = excluded.details, updated_at = excluded.updated_at
    `, printerID, state, raw, updated)
	return err
}

func (s *Store) UpdatePrinterAccepting(ctx context.Context, tx *sql.Tx, id int64, accepting bool) error {
	acc := 0
	if accepting {
		acc = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET accepting = ?, updated_at = ?
        WHERE id = ?
    `, acc, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-config-changed")
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-modified")
	}
	return err
}

func (s *Store) UpdateAllPrintersAccepting(ctx context.Context, tx *sql.Tx, accepting bool) error {
	acc := 0
	if accepting {
		acc = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET accepting = ?, updated_at = ?
    `, acc, time.Now().UTC())
	return err
}

func (s *Store) UpdatePrinterState(ctx context.Context, tx *sql.Tx, id int64, state int) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET state = ?, updated_at = ?
        WHERE id = ?
    `, state, time.Now().UTC(), id)
	if err == nil {
		_ = s.addNotificationForPrinter(ctx, tx, id, "printer-state-changed")
		if state == 5 {
			_ = s.addNotificationForPrinter(ctx, tx, id, "printer-stopped")
		} else if state == 3 {
			_ = s.addNotificationForPrinter(ctx, tx, id, "printer-restarted")
		}
	}
	return err
}

func (s *Store) UpdateAllPrintersState(ctx context.Context, tx *sql.Tx, state int) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET state = ?, updated_at = ?
    `, state, time.Now().UTC())
	return err
}

func (s *Store) UpdateAllClassesAccepting(ctx context.Context, tx *sql.Tx, accepting bool) error {
	acc := 0
	if accepting {
		acc = 1
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET accepting = ?, updated_at = ?
    `, acc, time.Now().UTC())
	return err
}

func (s *Store) UpdateAllClassesState(ctx context.Context, tx *sql.Tx, state int) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE classes
        SET state = ?, updated_at = ?
    `, state, time.Now().UTC())
	return err
}

func (s *Store) SetDefaultPrinter(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE printers SET is_default = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE classes SET is_default = 0`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
        UPDATE printers
        SET is_default = 1, updated_at = ?
        WHERE id = ?
    `, time.Now().UTC(), id)
	return err
}

func (s *Store) DeletePrinter(ctx context.Context, tx *sql.Tx, id int64) error {
	_ = s.addNotificationForPrinter(ctx, tx, id, "printer-deleted")
	_, err := tx.ExecContext(ctx, `DELETE FROM printers WHERE id = ?`, id)
	return err
}

func (s *Store) CancelJobsByPrinter(ctx context.Context, tx *sql.Tx, printerID int64, reason string) error {
	completed := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
        UPDATE jobs
        SET state = ?, state_reason = ?, completed_at = ?
        WHERE printer_id = ? AND state < ?
    `, 7, reason, completed, printerID, 7)
	return err
}

func (s *Store) CreateSubscription(ctx context.Context, tx *sql.Tx, printerID *int64, jobID *int64, events string, leaseSecs int64, owner string, recipientURI string, pullMethod string, timeInterval int64, userData []byte) (model.Subscription, error) {
	now := time.Now().UTC()
	events = normalizeEvents(events)
	if owner == "" {
		owner = "anonymous"
	}
	if strings.TrimSpace(pullMethod) == "" {
		pullMethod = "ippget"
	}
	var pid sql.NullInt64
	var jid sql.NullInt64
	if printerID != nil {
		pid = sql.NullInt64{Int64: *printerID, Valid: true}
	}
	if jobID != nil {
		jid = sql.NullInt64{Int64: *jobID, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
        INSERT INTO subscriptions (printer_id, job_id, events, lease_seconds, owner, recipient_uri, pull_method, time_interval, user_data, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, pid, jid, events, leaseSecs, owner, recipientURI, pullMethod, timeInterval, userData, now)
	if err != nil {
		return model.Subscription{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Subscription{}, err
	}
	return model.Subscription{
		ID:           id,
		PrinterID:    pid,
		JobID:        jid,
		Events:       events,
		LeaseSecs:    leaseSecs,
		Owner:        owner,
		RecipientURI: recipientURI,
		PullMethod:   pullMethod,
		TimeInterval: timeInterval,
		UserData:     userData,
		CreatedAt:    now,
	}, nil
}

func (s *Store) GetSubscription(ctx context.Context, tx *sql.Tx, id int64) (model.Subscription, error) {
	var sub model.Subscription
	var printerID sql.NullInt64
	var jobID sql.NullInt64
	var userData []byte
	err := tx.QueryRowContext(ctx, `
        SELECT id, printer_id, job_id, events, lease_seconds, owner, recipient_uri, pull_method, time_interval, user_data, created_at
        FROM subscriptions
        WHERE id = ?
    `, id).Scan(&sub.ID, &printerID, &jobID, &sub.Events, &sub.LeaseSecs, &sub.Owner, &sub.RecipientURI, &sub.PullMethod, &sub.TimeInterval, &userData, &sub.CreatedAt)
	if err != nil {
		return model.Subscription{}, err
	}
	sub.PrinterID = printerID
	sub.JobID = jobID
	sub.UserData = userData
	return sub, nil
}

func (s *Store) UpdateSubscriptionLease(ctx context.Context, tx *sql.Tx, id int64, leaseSecs int64) (model.Subscription, error) {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
        UPDATE subscriptions
        SET lease_seconds = ?, created_at = ?
        WHERE id = ?
    `, leaseSecs, now, id)
	if err != nil {
		return model.Subscription{}, err
	}
	return s.GetSubscription(ctx, tx, id)
}

func (s *Store) ListSubscriptions(ctx context.Context, tx *sql.Tx, printerID *int64, jobID *int64, owner string, limit int) ([]model.Subscription, error) {
	query := `
        SELECT id, printer_id, job_id, events, lease_seconds, owner, recipient_uri, pull_method, time_interval, user_data, created_at
        FROM subscriptions
    `
	conds := []string{}
	args := []any{}
	if printerID != nil {
		conds = append(conds, "printer_id = ?")
		args = append(args, *printerID)
	}
	if jobID != nil {
		conds = append(conds, "job_id = ?")
		args = append(args, *jobID)
	}
	if owner != "" {
		conds = append(conds, "owner = ?")
		args = append(args, owner)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY id ASC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	subs := []model.Subscription{}
	for rows.Next() {
		var sub model.Subscription
		var pid sql.NullInt64
		var jid sql.NullInt64
		var userData []byte
		if err := rows.Scan(&sub.ID, &pid, &jid, &sub.Events, &sub.LeaseSecs, &sub.Owner, &sub.RecipientURI, &sub.PullMethod, &sub.TimeInterval, &userData, &sub.CreatedAt); err != nil {
			return nil, err
		}
		sub.PrinterID = pid
		sub.JobID = jid
		sub.UserData = userData
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) CancelSubscription(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

func (s *Store) CountSubscriptions(ctx context.Context, tx *sql.Tx) (int, error) {
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) CountSubscriptionsForPrinter(ctx context.Context, tx *sql.Tx, printerID int64) (int, error) {
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE printer_id = ?`, printerID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) CountSubscriptionsForJob(ctx context.Context, tx *sql.Tx, jobID int64) (int, error) {
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) CountSubscriptionsForUser(ctx context.Context, tx *sql.Tx, owner string) (int, error) {
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE owner = ?`, owner).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) ListNotifications(ctx context.Context, tx *sql.Tx, subscriptionID int64, limit int) ([]model.Notification, error) {
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
        SELECT id, subscription_id, event, created_at
        FROM notifications
        WHERE subscription_id = ?
        ORDER BY id ASC
        LIMIT ?
    `, subscriptionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Notification
	for rows.Next() {
		var n model.Notification
		if err := rows.Scan(&n.ID, &n.SubscriptionID, &n.Event, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) addNotificationForPrinter(ctx context.Context, tx *sql.Tx, printerID int64, event string) error {
	now := time.Now().UTC()
	if s.MaxEvents <= 0 {
		return nil
	}
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
        SELECT id, events, lease_seconds, created_at
        FROM subscriptions
        WHERE printer_id = ?
    `, printerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var events string
		var lease int64
		var createdAt time.Time
		if err := rows.Scan(&id, &events, &lease, &createdAt); err != nil {
			return err
		}
		if !subscriptionActive(createdAt, lease, now) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id); err != nil {
				return err
			}
			continue
		}
		if !eventAllowed(events, event) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO notifications (subscription_id, event, created_at)
            VALUES (?, ?, ?)
        `, id, event, now); err != nil {
			return err
		}
		if s.MaxEvents > 0 {
			if _, err := tx.ExecContext(ctx, `
                DELETE FROM notifications
                WHERE subscription_id = ?
                  AND id NOT IN (
                    SELECT id FROM notifications
                    WHERE subscription_id = ?
                    ORDER BY id DESC
                    LIMIT ?
                  )
            `, id, id, s.MaxEvents); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func (s *Store) addNotificationForJob(ctx context.Context, tx *sql.Tx, jobID int64, event string) error {
	now := time.Now().UTC()
	if s.MaxEvents <= 0 {
		return nil
	}
	if err := s.PruneExpiredSubscriptions(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
        SELECT id, events, lease_seconds, created_at
        FROM subscriptions
        WHERE job_id = ?
    `, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var events string
		var lease int64
		var createdAt time.Time
		if err := rows.Scan(&id, &events, &lease, &createdAt); err != nil {
			return err
		}
		if !subscriptionActive(createdAt, lease, now) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id); err != nil {
				return err
			}
			continue
		}
		if !eventAllowed(events, event) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO notifications (subscription_id, event, created_at)
            VALUES (?, ?, ?)
        `, id, event, now); err != nil {
			return err
		}
		if s.MaxEvents > 0 {
			if _, err := tx.ExecContext(ctx, `
                DELETE FROM notifications
                WHERE subscription_id = ?
                  AND id NOT IN (
                    SELECT id FROM notifications
                    WHERE subscription_id = ?
                    ORDER BY id DESC
                    LIMIT ?
                  )
            `, id, id, s.MaxEvents); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func (s *Store) PruneExpiredSubscriptions(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
        DELETE FROM subscriptions
        WHERE lease_seconds > 0
          AND (strftime('%s', created_at) + lease_seconds) <= strftime('%s', ?)
    `, now)
	return err
}

func normalizeEvents(events string) string {
	events = strings.TrimSpace(events)
	if events == "" {
		return "all"
	}
	parts := strings.Split(events, ",")
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
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
	if len(out) == 0 {
		return "all"
	}
	return strings.Join(out, ",")
}

func subscriptionActive(createdAt time.Time, leaseSecs int64, now time.Time) bool {
	if leaseSecs <= 0 {
		return true
	}
	expireAt := createdAt.Add(time.Duration(leaseSecs) * time.Second)
	return now.Before(expireAt)
}

func eventAllowed(events string, event string) bool {
	events = normalizeEvents(events)
	if events == "all" {
		return true
	}
	for _, e := range strings.Split(events, ",") {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}

func (s *Store) ClaimPendingJob(ctx context.Context, tx *sql.Tx, jobID int64) (bool, error) {
	processingAt := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
        UPDATE jobs
        SET state = ?, state_reason = ?, processing_at = COALESCE(processing_at, ?)
        WHERE id = ? AND state = ?
    `, 5, "job-printing", processingAt, jobID, 3)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) ListPendingJobs(ctx context.Context, tx *sql.Tx, limit int) ([]model.Job, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE state = ?
          AND printer_id IN (SELECT id FROM printers WHERE accepting = 1 AND state != 5)
        ORDER BY submitted_at
        LIMIT ?
    `, 3, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []model.Job{}
	for rows.Next() {
		var job model.Job
		var processing sql.NullTime
		var completed sql.NullTime
		if err := rows.Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed); err != nil {
			return nil, err
		}
		if processing.Valid {
			job.ProcessingAt = &processing.Time
		}
		if completed.Valid {
			job.CompletedAt = &completed.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListHeldJobs(ctx context.Context, tx *sql.Tx, limit int) ([]model.Job, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE state = ?
        ORDER BY submitted_at
        LIMIT ?
    `, 4, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []model.Job{}
	for rows.Next() {
		var job model.Job
		var processing sql.NullTime
		var completed sql.NullTime
		if err := rows.Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed); err != nil {
			return nil, err
		}
		if processing.Valid {
			job.ProcessingAt = &processing.Time
		}
		if completed.Valid {
			job.CompletedAt = &completed.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListTerminalJobs(ctx context.Context, tx *sql.Tx, limit int) ([]model.Job, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, printer_id, name, user_name, origin_host, options, state, state_reason, impressions, submitted_at, processing_at, completed_at
        FROM jobs
        WHERE state IN (7, 8, 9)
          AND completed_at IS NOT NULL
        ORDER BY completed_at
        LIMIT ?
    `, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []model.Job{}
	for rows.Next() {
		var job model.Job
		var processing sql.NullTime
		var completed sql.NullTime
		if err := rows.Scan(&job.ID, &job.PrinterID, &job.Name, &job.UserName, &job.OriginHost, &job.Options, &job.State, &job.StateReason, &job.Impressions, &job.SubmittedAt, &processing, &completed); err != nil {
			return nil, err
		}
		if processing.Valid {
			job.ProcessingAt = &processing.Time
		}
		if completed.Valid {
			job.CompletedAt = &completed.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListDocumentsByJob(ctx context.Context, tx *sql.Tx, jobID int64) ([]model.Document, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id, job_id, file_name, mime_type, format_supplied, name_supplied, size_bytes, path, created_at
        FROM documents
        WHERE job_id = ?
        ORDER BY id
    `, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	docs := []model.Document{}
	for rows.Next() {
		var doc model.Document
		if err := rows.Scan(&doc.ID, &doc.JobID, &doc.FileName, &doc.MimeType, &doc.FormatSupplied, &doc.NameSupplied, &doc.SizeBytes, &doc.Path, &doc.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) ListDocumentStatsByJobIDs(ctx context.Context, tx *sql.Tx, jobIDs []int64) (map[int64]DocumentStats, error) {
	stats := make(map[int64]DocumentStats, len(jobIDs))
	if len(jobIDs) == 0 {
		return stats, nil
	}
	placeholders := strings.Repeat("?,", len(jobIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(jobIDs))
	for _, id := range jobIDs {
		args = append(args, id)
	}
	query := fmt.Sprintf(`
        SELECT job_id,
               COUNT(1) AS doc_count,
               COALESCE(SUM(size_bytes), 0) AS total_size,
               MIN(mime_type) AS mime_type
        FROM documents
        WHERE job_id IN (%s)
        GROUP BY job_id
    `, placeholders)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row DocumentStats
		if err := rows.Scan(&row.JobID, &row.Count, &row.SizeBytes, &row.MimeType); err != nil {
			return nil, err
		}
		stats[row.JobID] = row
	}
	return stats, rows.Err()
}

func (s *Store) DeleteDocumentsByJob(ctx context.Context, tx *sql.Tx, jobID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE job_id = ?`, jobID)
	return err
}

func (s *Store) ListJobIDsByPrinter(ctx context.Context, tx *sql.Tx, printerID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT id
        FROM jobs
        WHERE printer_id = ?
        ORDER BY id
    `, printerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) CountQueuedJobsByPrinterIDs(ctx context.Context, tx *sql.Tx, printerIDs []int64) (int, error) {
	if len(printerIDs) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat("?,", len(printerIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(printerIDs))
	for _, id := range printerIDs {
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT COUNT(1) FROM jobs WHERE printer_id IN (%s) AND state IN (3, 4, 5, 6)`, placeholders)
	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) DeleteJob(ctx context.Context, tx *sql.Tx, jobID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, jobID)
	return err
}
