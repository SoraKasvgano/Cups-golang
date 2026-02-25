package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCupsdConfAccessLogLevelAndPageLogFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cupsd.conf")
	content := strings.Join([]string{
		`AccessLogLevel all`,
		`PageLogFormat "%p %u %j %T %P %C %{job-billing}"`,
		`LiStEn 127.0.0.1:8631`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cupsd.conf: %v", err)
	}

	cfg := Config{ConfDir: dir}
	parseCupsdConf(path, &cfg, nil)

	if cfg.AccessLogLevel != "all" {
		t.Fatalf("AccessLogLevel = %q, want all", cfg.AccessLogLevel)
	}
	if cfg.PageLogFormat != "%p %u %j %T %P %C %{job-billing}" {
		t.Fatalf("PageLogFormat = %q", cfg.PageLogFormat)
	}
	if len(cfg.ListenHTTP) != 1 || cfg.ListenHTTP[0] != "127.0.0.1:8631" {
		t.Fatalf("ListenHTTP = %#v, want [127.0.0.1:8631]", cfg.ListenHTTP)
	}
}

func TestParseCupsFilesConfCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cups-files.conf")
	content := strings.Join([]string{
		`AccessLog logs/access_log`,
		`ErrorLog logs/error_log`,
		`PageLog logs/page_log`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cups-files.conf: %v", err)
	}

	cfg := Config{ConfDir: dir}
	parseCupsFilesConf(path, &cfg, nil)

	if !strings.HasSuffix(cfg.AccessLogPath, filepath.Join("logs", "access_log")) {
		t.Fatalf("AccessLogPath = %q", cfg.AccessLogPath)
	}
	if !strings.HasSuffix(cfg.ErrorLogPath, filepath.Join("logs", "error_log")) {
		t.Fatalf("ErrorLogPath = %q", cfg.ErrorLogPath)
	}
	if !strings.HasSuffix(cfg.PageLogPath, filepath.Join("logs", "page_log")) {
		t.Fatalf("PageLogPath = %q", cfg.PageLogPath)
	}
}

func TestParseCupsFilesConfBlankPageLogDisables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cups-files.conf")
	content := strings.Join([]string{
		`AccessLog logs/access_log`,
		`PageLog`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cups-files.conf: %v", err)
	}

	cfg := Config{ConfDir: dir}
	parseCupsFilesConf(path, &cfg, nil)

	if cfg.AccessLogPath == "" {
		t.Fatalf("expected AccessLogPath to be set")
	}
	if cfg.PageLogPath != "" {
		t.Fatalf("PageLogPath = %q, want empty", cfg.PageLogPath)
	}
}
