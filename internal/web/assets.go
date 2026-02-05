package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var embeddedAssets embed.FS

func AssetHandler() http.Handler {
	sub, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

func ServeSPA(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "text/html") && accept != "" {
		return false
	}

	f, err := embeddedAssets.Open("assets/index.html")
	if err != nil {
		return false
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
	return true
}
