// handlers.go — public router surface: /v1/models, /v1/chat/completions,
// /v1/messages (Anthropic), /v1/messages/count_tokens (Anthropic parity,
// local estimator), /health. Every /v1 request is authenticated with a
// router key (sk-…); the embedded bridges then receive the internal secret.

package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (rt *Router) registerV1(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", rt.keyAuth(rt.modelsHandler))
	mux.HandleFunc("/v1/chat/completions", rt.keyAuth(rt.chatHandler))
	mux.HandleFunc("/v1/messages/count_tokens", rt.keyAuth(rt.countTokensHandler))
	mux.HandleFunc("/v1/messages", rt.keyAuth(rt.messagesHandler))
	mux.HandleFunc("/models", rt.keyAuth(rt.modelsHandler))
}

func (rt *Router) keyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerKey(r)
		rec, ok := rt.store.LookupKey(key)
		if !ok || !rt.store.ValidateKey(key) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"کلید API نامعتبر است — از داشبورد کلید بگیر / invalid API key — create one in the dashboard","type":"authentication_error","code":"invalid_api_key"}}`))
			return
		}
		// per-key quota (MaxRequests caps the key's lifetime request count)
		if rec.MaxRequests > 0 && rec.Requests >= rec.MaxRequests {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			fmt.Fprintf(w, `{"error":{"message":"سهمیه‌ی کلید «%s» تمام شد / key quota exhausted (%d requests) — تازه‌اش کن یا سقف را در داشبورد بالا ببر / create a new key or raise the limit in the dashboard","type":"rate_limit_error","code":"key_quota_exceeded"}}`, rec.Name, rec.MaxRequests)
			return
		}
		next(w, r)
	}
}

func bearerKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

func (rt *Router) modelsHandler(w http.ResponseWriter, r *http.Request) {
	models, _ := rt.registry.Catalog()
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": models})
}

func (rt *Router) chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, 400, formatRouterError("failed to read body", "invalid_request_error"))
		return
	}
	rt.forwarder.ForwardChat(w, r, body, bearerKey(r))
}

func (rt *Router) messagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, 400, formatRouterError("failed to read body", "invalid_request_error"))
		return
	}
	rt.forwarder.ForwardMessages(w, r, body, bearerKey(r))
}

// countTokensHandler answers Anthropic's /v1/messages/count_tokens locally
// with a rough estimate (≈4 chars/token over messages+system+tools JSON).
// Good enough for client-side budgeting; no upstream round-trip.
func (rt *Router) countTokensHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil || json.Unmarshal(raw, &req) != nil {
		writeJSON(w, 400, map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": "invalid request body",
			},
		})
		return
	}
	// count over the payload sections (model id overhead is noise)
	payload := len(req.Messages) + len(req.System) + len(req.Tools)
	if payload == 0 {
		payload = len(raw)
	}
	writeJSON(w, 200, map[string]interface{}{
		"input_tokens": EstimateTokens(string(make([]byte, payload))),
	})
}
