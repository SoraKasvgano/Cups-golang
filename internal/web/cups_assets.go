package web

import (
	"embed"
	"io/fs"
	"net/http"
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

func cupsTemplateFS() fs.FS {
	sub, err := fs.Sub(cupsEmbedded, "cups_templates")
	if err != nil {
		return nil
	}
	return sub
}
