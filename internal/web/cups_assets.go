package web

import (
	"embed"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed cups_assets/** cups_templates/**
var cupsEmbedded embed.FS

func CupsAssetHandler() http.Handler {
	sub, err := fs.Sub(cupsEmbedded, "cups_assets")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

func CupsHelpHandler() http.Handler {
	sub, err := fs.Sub(cupsEmbedded, "cups_assets/help")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// CupsStringsLanguages returns available language tags from embedded strings.
func CupsStringsLanguages() []string {
	sub, err := fs.Sub(cupsEmbedded, "cups_assets/strings")
	if err != nil {
		return nil
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	langs := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if !strings.HasSuffix(name, ".strings") {
			continue
		}
		tag := strings.TrimSuffix(name, ".strings")
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		langs = append(langs, tag)
	}
	if len(langs) == 0 {
		return nil
	}
	sort.Slice(langs, func(i, j int) bool {
		return strings.ToLower(langs[i]) < strings.ToLower(langs[j])
	})
	return langs
}

func cupsTemplateFS() fs.FS {
	sub, err := fs.Sub(cupsEmbedded, "cups_templates")
	if err != nil {
		return nil
	}
	return sub
}
