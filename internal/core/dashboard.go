// dashboard.go — the embedded Persian-RTL web UI (single HTML file).

package core

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var dashboardHTML []byte

func (rt *Router) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}
