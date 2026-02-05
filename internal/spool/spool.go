package spool

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Spool struct {
	Dir       string
	OutputDir string
}

func (s Spool) Ensure() error {
	if err := os.MkdirAll(s.Dir, 0755); err != nil {
		return err
	}
	if s.OutputDir != "" {
		if err := os.MkdirAll(s.OutputDir, 0755); err != nil {
			return err
		}
	}
	return nil
}

func (s Spool) Save(jobID int64, fileName string, r io.Reader) (string, int64, error) {
	if err := s.Ensure(); err != nil {
		return "", 0, err
	}
	base := fmt.Sprintf("job-%d-%d", jobID, time.Now().UnixNano())
	if fileName != "" {
		base = base + "-" + sanitizeFileName(fileName)
	}
	path := filepath.Join(s.Dir, base)
	f, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, r)
	if err != nil {
		return "", 0, err
	}
	return path, n, nil
}

func (s Spool) OutputPath(jobID int64, fileName string) string {
	if s.OutputDir == "" {
		return ""
	}
	base := fmt.Sprintf("job-%d", jobID)
	if fileName != "" {
		base = base + "-" + sanitizeFileName(fileName)
	}
	return filepath.Join(s.OutputDir, base)
}

func sanitizeFileName(name string) string {
	clean := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			continue
		}
		clean = append(clean, r)
	}
	if len(clean) == 0 {
		return "document"
	}
	return string(clean)
}
