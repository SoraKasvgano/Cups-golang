package logging

import (
	"io"
	"os"
	"sync"
)

type manager struct {
	errorLog  *RotatingFile
	accessLog *RotatingFile
	pageLog   *RotatingFile
}

var (
	globalMu sync.RWMutex
	global   = manager{}
)

func Configure(errorPath, accessPath, pagePath string, maxSize int64) {
	globalMu.Lock()
	defer globalMu.Unlock()
	global.errorLog = NewRotatingFile(errorPath, maxSize)
	global.accessLog = NewRotatingFile(accessPath, maxSize)
	global.pageLog = NewRotatingFile(pagePath, maxSize)
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
	if logger != nil {
		_ = logger.WriteLine(line)
	}
}
