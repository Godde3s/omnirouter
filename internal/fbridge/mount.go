// mount.go — exported integration surface for the OmniRouter core.
//
// The router embeds this bridge as a library: it calls Init() once at boot
// and Handler() to obtain the bridge's full HTTP surface, served on an
// internal loopback listener. Freebuff needs a per-account auth token, so
// the bridge answers with guided errors until one is connected (env, CLI
// file, or the dashboard Connect panel).

package fbridge

import "net/http"

// Init reads env config (no network calls — the catalog is fetched lazily).
func Init() {
	initConfig()
}

// Handler returns the bridge's complete HTTP surface (routes + auth + CORS).
func Handler() http.Handler { return NewHandler() }

// NewHandler assembles the bridge HTTP surface.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		healthHandler(w, r)
	})
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/v1/models", auth(modelsHandler))
	mux.HandleFunc("/models", auth(modelsHandler))
	mux.HandleFunc("/v1/chat/completions", auth(chatHandler))
	mux.HandleFunc("/v1/messages", auth(anthropicMessagesHandler))
	return cors(mux)
}

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r) {
			writeJSON(w, 401, map[string]interface{}{
				"error": map[string]string{"message": "invalid token (AUTH_TOKEN)",
					"type": "authentication_error"}})
			return
		}
		next(w, r)
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}
