package store

import (
	"context"
	"database/sql"
	"strings"
)

func (s *Store) migrate(ctx context.Context) error {
	return s.WithTx(ctx, false, func(tx *sql.Tx) error {
		stmts := []string{
			`CREATE TABLE IF NOT EXISTS printers (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL UNIQUE,
                uri TEXT NOT NULL UNIQUE,
                ppd_name TEXT NOT NULL DEFAULT '',
                location TEXT NOT NULL DEFAULT '',
                info TEXT NOT NULL DEFAULT '',
                geo_location TEXT NOT NULL DEFAULT '',
                organization TEXT NOT NULL DEFAULT '',
                organizational_unit TEXT NOT NULL DEFAULT '',
                state INTEGER NOT NULL DEFAULT 3,
                accepting INTEGER NOT NULL DEFAULT 1,
                shared INTEGER NOT NULL DEFAULT 1,
                is_default INTEGER NOT NULL DEFAULT 0,
                job_sheets_default TEXT NOT NULL DEFAULT 'none',
                default_options TEXT NOT NULL DEFAULT '',
                created_at DATETIME NOT NULL,
                updated_at DATETIME NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS classes (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL UNIQUE,
                location TEXT NOT NULL DEFAULT '',
                info TEXT NOT NULL DEFAULT '',
                state INTEGER NOT NULL DEFAULT 3,
                accepting INTEGER NOT NULL DEFAULT 1,
                is_default INTEGER NOT NULL DEFAULT 0,
                job_sheets_default TEXT NOT NULL DEFAULT 'none',
                default_options TEXT NOT NULL DEFAULT '',
                created_at DATETIME NOT NULL,
                updated_at DATETIME NOT NULL
            )`,
			`CREATE TABLE IF NOT EXISTS class_members (
                class_id INTEGER NOT NULL,
                printer_id INTEGER NOT NULL,
                PRIMARY KEY (class_id, printer_id),
                FOREIGN KEY (class_id) REFERENCES classes(id) ON DELETE CASCADE,
                FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE
            )`,
			`CREATE TABLE IF NOT EXISTS jobs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                printer_id INTEGER NOT NULL,
                name TEXT NOT NULL DEFAULT '',
                user_name TEXT NOT NULL DEFAULT '',
                options TEXT NOT NULL DEFAULT '',
                state INTEGER NOT NULL,
                state_reason TEXT NOT NULL DEFAULT '',
                impressions INTEGER NOT NULL DEFAULT 0,
                submitted_at DATETIME NOT NULL,
                completed_at DATETIME,
                FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE
            )`,
			`CREATE TABLE IF NOT EXISTS documents (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                job_id INTEGER NOT NULL,
                file_name TEXT NOT NULL DEFAULT '',
                mime_type TEXT NOT NULL DEFAULT 'application/octet-stream',
                size_bytes INTEGER NOT NULL DEFAULT 0,
                path TEXT NOT NULL,
                created_at DATETIME NOT NULL,
                FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
            )`,
			`CREATE TABLE IF NOT EXISTS users (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                username TEXT NOT NULL UNIQUE,
                password_hash TEXT NOT NULL,
                digest_ha1 TEXT NOT NULL DEFAULT '',
                is_admin INTEGER NOT NULL DEFAULT 0,
                created_at DATETIME NOT NULL,
                updated_at DATETIME NOT NULL
            )`,
			`CREATE TABLE IF NOT EXISTS settings (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )`,
			`CREATE TABLE IF NOT EXISTS subscriptions (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                printer_id INTEGER,
                job_id INTEGER,
                events TEXT NOT NULL DEFAULT '',
                lease_seconds INTEGER NOT NULL DEFAULT 0,
                owner TEXT NOT NULL DEFAULT '',
                recipient_uri TEXT NOT NULL DEFAULT '',
                pull_method TEXT NOT NULL DEFAULT 'ippget',
                time_interval INTEGER NOT NULL DEFAULT 0,
                user_data BLOB,
                created_at DATETIME NOT NULL,
                FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE,
                FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
            )`,
			`CREATE TABLE IF NOT EXISTS notifications (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                subscription_id INTEGER NOT NULL,
                event TEXT NOT NULL,
                created_at DATETIME NOT NULL,
                FOREIGN KEY (subscription_id) REFERENCES subscriptions(id) ON DELETE CASCADE
            )`,
			`CREATE TABLE IF NOT EXISTS printer_supplies (
                printer_id INTEGER PRIMARY KEY,
                state TEXT NOT NULL DEFAULT '',
                details TEXT NOT NULL DEFAULT '',
                updated_at DATETIME NOT NULL,
                FOREIGN KEY (printer_id) REFERENCES printers(id) ON DELETE CASCADE
            )`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_printer_id ON jobs(printer_id)`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state)`,
			`CREATE INDEX IF NOT EXISTS idx_documents_job_id ON documents(job_id)`,
			`CREATE INDEX IF NOT EXISTS idx_class_members_class_id ON class_members(class_id)`,
			`CREATE INDEX IF NOT EXISTS idx_class_members_printer_id ON class_members(printer_id)`,
			`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,
			`CREATE INDEX IF NOT EXISTS idx_subscriptions_printer_id ON subscriptions(printer_id)`,
			`CREATE INDEX IF NOT EXISTS idx_subscriptions_job_id ON subscriptions(job_id)`,
			`CREATE INDEX IF NOT EXISTS idx_notifications_subscription_id ON notifications(subscription_id)`,
			`CREATE INDEX IF NOT EXISTS idx_printer_supplies_printer_id ON printer_supplies(printer_id)`,
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		if err := ensureColumn(ctx, tx, "printers", "geo_location", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "ppd_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "organization", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "organizational_unit", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "shared", "INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "job_sheets_default", "TEXT NOT NULL DEFAULT 'none'"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printers", "default_options", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "users", "digest_ha1", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "subscriptions", "owner", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "subscriptions", "recipient_uri", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "subscriptions", "pull_method", "TEXT NOT NULL DEFAULT 'ippget'"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "subscriptions", "time_interval", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "subscriptions", "user_data", "BLOB"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "classes", "job_sheets_default", "TEXT NOT NULL DEFAULT 'none'"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "classes", "default_options", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printer_supplies", "state", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printer_supplies", "details", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumn(ctx, tx, "printer_supplies", "updated_at", "DATETIME NOT NULL"); err != nil {
			return err
		}
		return nil
	})
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}
