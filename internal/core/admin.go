// admin.go — dashboard auth + admin management API.
//
//   POST /admin/login            {password} → session token (cookie + JSON)
//   GET  /admin/api/overview     providers + keys + version
//   GET  /admin/api/models       merged catalog
//   POST /admin/api/models/refresh
//   GET  /admin/api/keys         list (masked)
//   POST /admin/api/keys         {name} → full key (shown once)
//   DELETE /admin/api/keys       ?key=
//   POST /admin/api/keys/toggle  {key, enabled}
//   GET  /admin/api/providers    custom providers
//   POST /admin/api/providers    {name, base_url, api_key, models[], enabled}
//   DELETE /admin/api/providers  ?name=
//   POST /admin/api/providers/toggle {name, enabled}
//   GET  /admin/api/logs         recent request log (ring buffer)

package core

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type adminAuth struct {
	mu     sync.Mutex
	tokens map[string]int64 // token → expiry unix
}

var admin = &adminAuth{tokens: map[string]int64{}}

const adminSessionTTL = 12 * time.Hour

func (a *adminAuth) login(password, real string) (string, bool) {
	if subtle.ConstantTimeCompare([]byte(password), []byte(real)) != 1 {
		return "", false
	}
	tok := randomHex(24)
	a.mu.Lock()
	a.tokens[tok] = time.Now().Add(adminSessionTTL).Unix()
	a.mu.Unlock()
	return tok, true
}

func (a *adminAuth) check(tok string) bool {
	if tok == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tokens[tok]
	if !ok || time.Now().Unix() > exp {
		return false
	}
	return true
}

func (a *adminAuth) logout(tok string) {
	a.mu.Lock()
	delete(a.tokens, tok)
	a.mu.Unlock()
}

func adminTokenFrom(r *http.Request) string {
	if c, err := r.Cookie("omni_admin"); err == nil {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return auth[7:]
	}
	return ""
}

func adminAPI(next http.HandlerFunc, password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !admin.check(adminTokenFrom(r)) {
			writeJSON(w, 401, map[string]interface{}{"error": "unauthorized — /admin/login first"})
			return
		}
		next(w, r)
	}
}

func registerAdminRoutes(mux *http.ServeMux, st *Store, reg *Registry, fw *forwarder, cfg *Config) {
	mux.HandleFunc("/admin/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid json"})
			return
		}
		tok, ok := admin.login(body.Password, cfg.AdminPassword)
		if !ok {
			writeJSON(w, 401, map[string]interface{}{"error": "رمز اشتباه است / wrong password"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "omni_admin", Value: tok, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, 200, map[string]interface{}{"token": tok})
	})

	mux.HandleFunc("/admin/logout", func(w http.ResponseWriter, r *http.Request) {
		admin.logout(adminTokenFrom(r))
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})

	mux.HandleFunc("/admin/api/overview", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") == "1" {
			reg.RefreshModels(r.Context())
		}
		writeJSON(w, 200, map[string]interface{}{
			"service":    "omnirouter",
			"version":    "1.0.0",
			"time":       time.Now().Unix(),
			"providers":  reg.Providers(),
			"keys":       st.ListKeys(),
			"logs":       RecentLogs(),
			"custom_raw": st.ListProviders(),
		})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/models", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		models, _ := reg.Catalog()
		writeJSON(w, 200, map[string]interface{}{"object": "list", "data": models})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/models/refresh", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		reg.RefreshModels(r.Context())
		models, _ := reg.Catalog()
		writeJSON(w, 200, map[string]interface{}{"object": "list", "data": models})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/keys", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, 200, map[string]interface{}{"keys": st.ListKeys()})
		case "POST":
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			k := st.CreateKey(strings.TrimSpace(body.Name))
			writeJSON(w, 200, map[string]interface{}{"key": k})
		case "DELETE":
			key := r.URL.Query().Get("key")
			if !st.DeleteKey(key) {
				writeJSON(w, 404, map[string]interface{}{"error": "not found"})
				return
			}
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/keys/toggle", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid body"})
			return
		}
		st.ToggleKey(body.Key, body.Enabled)
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/providers", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, 200, map[string]interface{}{"providers": st.ListProviders()})
		case "POST":
			var p CustomProvider
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeJSON(w, 400, map[string]interface{}{"error": "invalid json"})
				return
			}
			p.Name = strings.TrimSpace(strings.ToLower(p.Name))
			p.BaseURL = strings.TrimSpace(p.BaseURL)
			p.Enabled = true
			if err := st.AddProvider(p); err != nil {
				writeJSON(w, 400, map[string]interface{}{"error": err.Error()})
				return
			}
			// Hot-register into the running registry + refresh catalog.
			reg.mu.Lock()
			reg.providers = append(reg.providers, &Provider{
				ID: "custom:" + p.Name, Kind: KindCustom,
				BaseURL: strings.TrimRight(p.BaseURL, "/"), APIKey: p.APIKey,
				Label: p.Name, Enabled: true, Models: p.Models,
			})
			reg.mu.Unlock()
			reg.RefreshModels(r.Context())
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		case "DELETE":
			name := r.URL.Query().Get("name")
			if !st.DeleteProvider(name) {
				writeJSON(w, 404, map[string]interface{}{"error": "not found"})
				return
			}
			reg.mu.Lock()
			for i := range reg.providers {
				if reg.providers[i].ID == "custom:"+name {
					reg.providers = append(reg.providers[:i], reg.providers[i+1:]...)
					break
				}
			}
			reg.mu.Unlock()
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/providers/toggle", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid body"})
			return
		}
		st.ToggleProvider(body.Name, body.Enabled)
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/logs", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]interface{}{"logs": RecentLogs()})
	}, cfg.AdminPassword))
}
