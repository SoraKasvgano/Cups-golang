package logging

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RotatingFile writes UTF-8 log lines to a file and rotates to "<path>.O"
// when MaxSize is reached, matching CUPS-style single backup behavior.
type RotatingFile struct {
	path    string
	maxSize int64
	mu      sync.Mutex
	mode    targetMode
}

type targetMode int

const (
	targetFile targetMode = iota
	targetStderr
	targetStdout
	targetDiscard
)

func NewRotatingFile(path string, maxSize int64) *RotatingFile {
	r := &RotatingFile{path: strings.TrimSpace(path), maxSize: maxSize}
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "", "none", "off":
		r.mode = targetDiscard
	case "syslog":
		r.mode = targetDiscard
	case "stderr", "-":
		r.mode = targetStderr
	case "stdout":
		r.mode = targetStdout
	default:
		r.mode = targetFile
	}
	return r
}

func (r *RotatingFile) Enabled() bool {
	return r != nil && r.mode != targetDiscard
}

func (r *RotatingFile) WriteLine(line string) error {
	if r == nil {
		return nil
	}
	_, err := r.Write([]byte(line + "\n"))
	return err
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.mode {
	case targetDiscard:
		return len(p), nil
	case targetStderr:
		return os.Stderr.Write(p)
	case targetStdout:
		return os.Stdout.Write(p)
	default:
		if err := r.ensureDir(); err != nil {
			return 0, err
		}
		if err := r.rotateIfNeeded(int64(len(p))); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		return f.Write(p)
	}
}

func (r *RotatingFile) ensureDir() error {
	if r.mode != targetFile {
		return nil
	}
	dir := filepath.Dir(r.path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func (r *RotatingFile) rotateIfNeeded(next int64) error {
	if r.mode != targetFile || r.maxSize <= 0 {
		return nil
	}
	info, err := os.Stat(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size()+next <= r.maxSize {
		return nil
	}
	oldPath := r.path + ".O"
	_ = os.Remove(oldPath)
	if err := os.Rename(r.path, oldPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

var _ io.Writer = (*RotatingFile)(nil)
