// admin.go — dashboard auth + admin management API.
//
//   POST /admin/login            {password} → session token (cookie + JSON)
//   GET  /admin/api/overview     providers + keys + logs + version
//   GET  /admin/api/stats        usage statistics (totals, per provider/model, hourly)
//   GET  /admin/api/models       merged catalog (incl. aliases)
//   POST /admin/api/models/refresh
//   GET  /admin/api/keys         list (full records)
//   POST /admin/api/keys         {name, max_requests?, allowed_models?} → full key (shown once)
//   PATCH/POST /admin/api/keys/update  {key, max_requests?, allowed_models?}
//   DELETE /admin/api/keys       ?key=
//   POST /admin/api/keys/toggle  {key, enabled}
//   GET  /admin/api/providers    custom providers (raw, incl. model_map)
//   POST /admin/api/providers    {name, base_url, api_key, models[], model_map?} (upsert)
//   DELETE /admin/api/providers  ?name=
//   POST /admin/api/providers/toggle {name, enabled}
//   POST /admin/api/aliases      {provider, map:{public: upstream}} (bridges + customs)
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

const routerVersion = "1.3.1"

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

// upsertProvider adds or replaces a custom provider in the registry, and
// keeps the store in sync.
func upsertProvider(r *http.Request, st *Store, reg *Registry, p CustomProvider) error {
	p.Name = strings.TrimSpace(strings.ToLower(p.Name))
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	exists := false
	for _, e := range st.ListProviders() {
		if strings.EqualFold(e.Name, p.Name) {
			exists = true
			break
		}
	}
	if exists {
		if err := st.UpdateProvider(p); err != nil {
			return err
		}
	} else {
		p.Enabled = true
		if err := st.AddProvider(p); err != nil {
			return err
		}
	}
	// Hot-register into the running registry.
	reg.mu.Lock()
	replaced := false
	for i := range reg.providers {
		if reg.providers[i].ID == "custom:"+p.Name {
			reg.providers[i] = &Provider{
				ID: "custom:" + p.Name, Kind: KindCustom,
				BaseURL: strings.TrimRight(p.BaseURL, "/"), APIKey: p.APIKey,
				Label: p.Name, Enabled: true, Models: p.Models, Aliases: p.ModelMap,
			}
			replaced = true
			break
		}
	}
	if !replaced {
		reg.providers = append(reg.providers, &Provider{
			ID: "custom:" + p.Name, Kind: KindCustom,
			BaseURL: strings.TrimRight(p.BaseURL, "/"), APIKey: p.APIKey,
			Label: p.Name, Enabled: true, Models: p.Models, Aliases: p.ModelMap,
		})
	}
	reg.mu.Unlock()
	reg.RefreshModels(r.Context())
	return nil
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
		stats := st.SnapshotStats()
		writeJSON(w, 200, map[string]interface{}{
			"service":    "omnirouter",
			"version":    routerVersion,
			"saver":      SaverSnapshot(),
			"combos":     reg.Combos(),
			"time":       time.Now().Unix(),
			"uptime_sec": time.Now().Unix() - stats.StartedAt,
			"providers":  reg.Providers(),
			"keys":       st.ListKeys(),
			"logs":       RecentLogs(),
			"stats":      stats,
			"custom_raw": st.ListProviders(),
		})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/stats", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, st.SnapshotStats())
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
				Name          string   `json:"name"`
				MaxRequests   int64    `json:"max_requests"`
				AllowedModels []string `json:"allowed_models"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			k := st.CreateKey(strings.TrimSpace(body.Name), body.MaxRequests, body.AllowedModels)
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

	mux.HandleFunc("/admin/api/keys/update", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key           string    `json:"key"`
			MaxRequests   *int64    `json:"max_requests"`
			AllowedModels *[]string `json:"allowed_models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid body"})
			return
		}
		if !st.UpdateKey(body.Key, body.MaxRequests, body.AllowedModels) {
			writeJSON(w, 404, map[string]interface{}{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
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
			if err := upsertProvider(r, st, reg, p); err != nil {
				writeJSON(w, 400, map[string]interface{}{"error": err.Error()})
				return
			}
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
		// hot-toggle in the registry too
		reg.mu.Lock()
		for _, p := range reg.providers {
			if strings.EqualFold(strings.TrimPrefix(p.ID, "custom:"), body.Name) {
				p.Enabled = body.Enabled
			}
		}
		reg.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/aliases", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			out := map[string]map[string]string{}
			for _, p := range reg.Providers() {
				if p.Aliases != nil {
					out[p.ID] = p.Aliases
				}
			}
			writeJSON(w, 200, map[string]interface{}{"aliases": out})
		case "POST":
			var body struct {
				Provider string            `json:"provider"`
				Map      map[string]string `json:"map"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Provider == "" {
				writeJSON(w, 400, map[string]interface{}{"error": "invalid body — {provider, map}"})
				return
			}
			if body.Map == nil {
				body.Map = map[string]string{}
			}
			// persist for customs in the store too
			for _, cp := range st.ListProviders() {
				if strings.EqualFold(cp.Name, body.Provider) {
					cp.ModelMap = body.Map
					_ = st.UpdateProvider(cp)
					break
				}
			}
			if !reg.SetAliases(body.Provider, body.Map) {
				writeJSON(w, 404, map[string]interface{}{"error": "provider not found"})
				return
			}
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/saver", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, 200, SaverSnapshot())
		case "POST":
			var body struct {
				RTK        *bool   `json:"rtk"`
				PromptMode *string `json:"prompt_mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]interface{}{"error": "invalid json"})
				return
			}
			if body.RTK != nil {
				saver.rtk.Store(*body.RTK)
			}
			if body.PromptMode != nil {
				mode := strings.ToLower(strings.TrimSpace(*body.PromptMode))
				if !ValidPromptMode(mode) {
					writeJSON(w, 400, map[string]interface{}{"error": "invalid prompt_mode (off|caveman|ponytail-lite|ponytail-full|ponytail-ultra)"})
					return
				}
				saver.promptMode.Store(mode)
			}
			writeJSON(w, 200, SaverSnapshot())
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/combos", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, 200, map[string]interface{}{"combos": reg.Combos()})
		case "POST":
			var body struct {
				Name  string   `json:"name"`
				Chain []string `json:"chain"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				writeJSON(w, 400, map[string]interface{}{"error": "invalid json — need {name, chain:[provider/model,...]}"})
				return
			}
			clean := make([]string, 0, len(body.Chain))
			for _, c := range body.Chain {
				if c = strings.TrimSpace(c); c != "" {
					clean = append(clean, c)
				}
			}
			if len(clean) == 0 {
				writeJSON(w, 400, map[string]interface{}{"error": "chain must list at least one provider/model"})
				return
			}
			reg.SetCombo(body.Name, clean)
			st.SetCombo(body.Name, clean)
			writeJSON(w, 200, map[string]interface{}{"ok": true, "name": strings.ToLower(strings.TrimSpace(strings.TrimPrefix(body.Name, "combo:"))), "chain": clean})
		case "DELETE":
			name := r.URL.Query().Get("name")
			if !reg.DeleteCombo(name) {
				st.DeleteCombo(name)
			}
			st.DeleteCombo(name)
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, cfg.AdminPassword))

	mux.HandleFunc("/admin/api/logs", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]interface{}{"logs": RecentLogs()})
	}, cfg.AdminPassword))

	// OAuth connections: connected clients + pending device grants.
	mux.HandleFunc("/admin/api/connections", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		conns := st.ConnectionKeys()
		type connView struct {
			Key         string `json:"key"` // masked
			FullKey     string `json:"key_full"`
			Name        string `json:"name"`
			Client      string `json:"client"`
			UserCode    string `json:"user_code"`
			Requests    int64  `json:"requests"`
			TokensIn    int64  `json:"tokens_in"`
			TokensOut   int64  `json:"tokens_out"`
			ConnectedAt int64  `json:"connected_at"`
			LastUsedAt  int64  `json:"last_used_at"`
			Enabled     bool   `json:"enabled"`
		}
		views := make([]connView, 0, len(conns))
		for _, k := range conns {
			masked := k.Key
			if len(masked) > 14 {
				masked = masked[:10] + "..." + masked[len(masked)-4:]
			}
			views = append(views, connView{
				Key: masked, FullKey: k.Key, Name: k.Name, Client: k.Client,
				UserCode: k.UserCode, Requests: k.Requests, TokensIn: k.TokensIn,
				TokensOut: k.TokensOut, ConnectedAt: k.CreatedAt,
				LastUsedAt: k.LastUsedAt, Enabled: k.Enabled,
			})
		}
		writeJSON(w, 200, map[string]interface{}{
			"connections": views,
			"pending":     st.PendingGrants(),
		})
	}, cfg.AdminPassword))

	// Approve/deny a pending device grant from the dashboard panel.
	mux.HandleFunc("/admin/api/oauth/decision", adminAPI(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserCode string `json:"user_code"`
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserCode == "" {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid body"})
			return
		}
		decision := "approve"
		if strings.EqualFold(body.Decision, "deny") {
			decision = "deny"
		}
		g, ok := st.ResolveDeviceGrant(strings.ToUpper(strings.TrimSpace(body.UserCode)), decision)
		if !ok {
			writeJSON(w, 404, map[string]interface{}{"error": "کد فعال پیدا نشد"})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": g.Status, "client": g.Client})
	}, cfg.AdminPassword))
}
