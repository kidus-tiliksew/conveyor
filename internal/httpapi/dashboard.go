package httpapi

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed dashboard/*
var dashboardAssets embed.FS

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	assetPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	// Extensionless paths are SPA routes (/activity, /tasks/<id>, …): the
	// client router owns them, so they all serve the app shell.
	if assetPath == "." || assetPath == "" || !strings.Contains(path.Base(assetPath), ".") {
		assetPath = "index.html"
	}
	data, err := fs.ReadFile(dashboardAssets, "dashboard/"+assetPath)
	if err != nil {
		if assetPath != "index.html" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if contentType := mime.TypeByExtension(path.Ext(assetPath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if strings.HasPrefix(assetPath, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(data)
}
