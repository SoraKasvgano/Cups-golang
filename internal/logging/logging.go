package logging

import (
	"io"
	"os"
	"strings"
	"sync"
)

type manager struct {
	errorLog       *RotatingFile
	accessLog      *RotatingFile
	pageLog        *RotatingFile
	accessLogLevel string
	pageLogFormat  string
}

var (
	globalMu sync.RWMutex
	global   = manager{}
)

func Configure(errorPath, accessPath, pagePath string, maxSize int64, accessLevel, pageFormat string) {
	globalMu.Lock()
	defer globalMu.Unlock()
	global.errorLog = NewRotatingFile(errorPath, maxSize)
	global.accessLog = NewRotatingFile(accessPath, maxSize)
	global.pageLog = NewRotatingFile(pagePath, maxSize)
	global.accessLogLevel = normalizeAccessLogLevel(accessLevel)
	global.pageLogFormat = pageFormat
}

func ErrorWriter() io.Writer {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if global.errorLog != nil && global.errorLog.Enabled() {
		return global.errorLog
	}
	return os.Stderr
}

func Access(line string) {
	globalMu.RLock()
	logger := global.accessLog
	globalMu.RUnlock()
	if logger != nil {
		_ = logger.WriteLine(line)
	}
}

func Page(line string) {
	globalMu.RLock()
	logger := global.pageLog
	globalMu.RUnlock()
	if line == "" {
		return
	}
	if logger != nil {
		_ = logger.WriteLine(line)
	}
}

func AccessLogLevel() string {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global.accessLogLevel
}

func PageLogFormat() string {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global.pageLogFormat
}

func normalizeAccessLogLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "all", "actions", "config":
		return level
	}
	return "actions"
}
