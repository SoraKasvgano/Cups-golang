package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type MimeType struct {
	Type string
	Exts []string
}

type MimeConv struct {
	Source  string
	Dest    string
	Cost    int
	Program string
}

type MimeDB struct {
	Types     map[string]MimeType
	ExtToType map[string]string
	Convs     []MimeConv
}

func LoadMimeDB(confDir string) (*MimeDB, error) {
	db := &MimeDB{
		Types:     map[string]MimeType{},
		ExtToType: map[string]string{},
		Convs:     []MimeConv{},
	}
	if err := ensureDefaultConf(confDir); err != nil {
		return db, err
	}
	if err := db.readMimeTypes(filepath.Join(confDir, "mime.types")); err != nil {
		return db, err
	}
	_ = db.readMimeTypes(filepath.Join(confDir, "local.types"))
	if err := db.readMimeConvs(filepath.Join(confDir, "mime.convs")); err != nil {
		return db, err
	}
	_ = db.readMimeConvs(filepath.Join(confDir, "local.convs"))
	return db, nil
}

func ensureDefaultConf(confDir string) error {
	if confDir == "" {
		return nil
	}
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(confDir, "mime.types")); os.IsNotExist(err) {
		if err := writeEmbedded("defaults/mime.types", filepath.Join(confDir, "mime.types")); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(confDir, "mime.convs")); os.IsNotExist(err) {
		if err := writeEmbedded("defaults/mime.convs", filepath.Join(confDir, "mime.convs")); err != nil {
			return err
		}
	}
	return nil
}

func writeEmbedded(path string, dest string) error {
	f, err := defaultConf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, f)
	return err
}

func (db *MimeDB) readMimeTypes(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		mt := parts[0]
		exts := parts[1:]
		db.Types[mt] = MimeType{Type: mt, Exts: exts}
		for _, e := range exts {
			db.ExtToType[strings.ToLower(e)] = mt
		}
	}
	return sc.Err()
}

func (db *MimeDB) readMimeConvs(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		cost := 0
		fmt.Sscanf(parts[2], "%d", &cost)
		db.Convs = append(db.Convs, MimeConv{
			Source:  parts[0],
			Dest:    parts[1],
			Cost:    cost,
			Program: strings.Join(parts[3:], " "),
		})
	}
	return sc.Err()
}

func (db *MimeDB) TypeForExtension(ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	if ext == "" {
		return ""
	}
	return db.ExtToType[ext]
}

func (db *MimeDB) FindConvs(src, dst string) []MimeConv {
	out := []MimeConv{}
	for _, c := range db.Convs {
		if c.Source == src && c.Dest == dst {
			out = append(out, c)
		}
	}
	return out
}
