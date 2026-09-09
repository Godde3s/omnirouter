// obridge — OpenCode Zen provider (opencode.ai/zen).
//
// This is the "OpenCode Free" feature popularized by 9router: OpenCode's
// Zen gateway serves an OpenAI-compatible endpoint whose FREE model tier
// works with NO API key at all — the client just needs a session id header
// (x-session-id / x-opencode-session, any UUID). Paid models become usable
// by setting OPENCODE_API_KEY.
//
// The bridge is therefore a thin enriching reverse-proxy:
//   GET  /v1/models             → auto-fetch of the live Zen model list
//   POST /v1/chat/completions   → passthrough + session header injection
//   POST /v1/messages           → Anthropic ⇄ OpenAI translation (below)
//
// Config (env): OPENCODE_BASE_URL (default https://opencode.ai/zen/v1),
// OPENCODE_API_KEY (optional, unlocks paid models), OPENCODE_SESSION_ID
// (optional stable session uuid; a random one is generated per process),
// AUTH_TOKEN (shared internal secret, default "opencode").

package obridge

import (
        "bytes"
        "context"
        "crypto/rand"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "os"
        "strings"
        "sync"
        "time"
)

const defaultBase = "https://opencode.ai/zen/v1"
const defaultToken = "opencode"

type oconfig struct {
        BaseURL   string
        APIKey    string
        SessionID string
        AuthToken string
}

var cfg oconfig

var httpClient = &http.Client{Timeout: 0} // streams run long; per-request ctx bounds non-stream

func envOr(key, def string) string {
        if v := strings.TrimSpace(os.Getenv(key)); v != "" {
                return v
        }
        return def
}

func newUUID() string {
        b := make([]byte, 16)
        _, _ = rand.Read(b)
        b[6] = (b[6] & 0x0f) | 0x40
        b[8] = (b[8] & 0x3f) | 0x80
        return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// initConfig reads env config. Called by the OmniRouter core at boot.
func initConfig() {
        cfg = oconfig{
                BaseURL:   strings.TrimRight(envOr("OPENCODE_BASE_URL", defaultBase), "/"),
                APIKey:    strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")),
                SessionID: strings.TrimSpace(os.Getenv("OPENCODE_SESSION_ID")),
                AuthToken: envOr("AUTH_TOKEN", defaultToken),
        }
        if cfg.SessionID == "" {
                cfg.SessionID = newUUID()
        }
        log.Printf("[OpenCode] bridge ready — base=%s mode=%s session=%s…",
                cfg.BaseURL, map[bool]string{true: "api-key", false: "FREE (no key)"}[cfg.APIKey != ""], cfg.SessionID[:8])
}

// sessionFor picks the session id for one request: the client's own header
// wins, otherwise the process-stable uuid (free tier requires "used in
// OpenCode" — any session uuid satisfies the check, as verified live).
func sessionFor(r *http.Request) string {
        for _, h := range []string{"x-opencode-session", "x-session-id"} {
                if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
                        return v
                }
        }
        return cfg.SessionID
}

func checkAuth(r *http.Request) bool {
        provided := r.Header.Get("Authorization")
        if len(provided) >= 7 && strings.EqualFold(provided[:7], "Bearer ") {
                provided = provided[7:]
        }
        if provided == "" {
                provided = r.Header.Get("x-api-key")
        }
        return provided == cfg.AuthToken
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(status)
        json.NewEncoder(w).Encode(v)
}

// ---------- live model cache (auto-fetch, 9router parity) ----------

var (
        modelsMu   sync.Mutex
        modelsList []map[string]interface{}
        modelsAt   time.Time
)

func fetchModels() []map[string]interface{} {
        modelsMu.Lock()
        defer modelsMu.Unlock()
        if len(modelsList) > 0 && time.Since(modelsAt) < 60*time.Second {
                return modelsList
        }
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "GET", cfg.BaseURL+"/models", nil)
        if err != nil {
                return modelsList
        }
        resp, err := httpClient.Do(req)
        if err != nil {
                log.Println("[OpenCode] models fetch:", err)
                return modelsList
        }
        defer resp.Body.Close()
        if resp.StatusCode != 200 {
                log.Println("[OpenCode] models fetch HTTP", resp.StatusCode)
                return modelsList
        }
        raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
        if err != nil {
                return modelsList
        }
        var parsed struct {
                Data []map[string]interface{} `json:"data"`
        }
        if json.Unmarshal(raw, &parsed) != nil || len(parsed.Data) == 0 {
                return modelsList
        }
        modelsList = parsed.Data
        modelsAt = time.Now()
        return modelsList
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
        data := fetchModels()
        if data == nil {
                writeJSON(w, 200, map[string]interface{}{"object": "list", "data": []interface{}{}})
                return
        }
        writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

// ---------- chat passthrough ----------

func chatHandler(w http.ResponseWriter, r *http.Request) {
        if r.Method != "POST" {
                http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                return
        }
        body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
        if err != nil {
                writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": "failed to read body"}})
                return
        }
        forwardChat(w, r, body, "/chat/completions")
}

func forwardChat(w http.ResponseWriter, r *http.Request, body []byte, path string) {
        ctx := r.Context()
        req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+path, bytes.NewReader(body))
        if err != nil {
                writeJSON(w, 502, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
                return
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "application/json")
        if r.Header.Get("Accept") == "text/event-stream" {
                req.Header.Set("Accept", "text/event-stream")
        }
        req.Header.Set("x-session-id", sessionFor(r))
        req.Header.Set("x-opencode-session", sessionFor(r))
        if cfg.APIKey != "" {
                req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
        }
        resp, err := httpClient.Do(req)
        if err != nil {
                writeJSON(w, 502, map[string]interface{}{"error": map[string]string{
                        "message": "OpenCode Zen upstream unreachable: " + err.Error()}})
                return
        }
        defer resp.Body.Close()

        ct := resp.Header.Get("Content-Type")
        if ct == "" {
                ct = "application/json"
        }
        for k, vv := range resp.Header {
                switch strings.ToLower(k) {
                case "content-type", "cache-control", "x-accel-buffering":
                        continue
                }
                for _, v := range vv {
                        w.Header().Add(k, v)
                }
        }
        w.Header().Set("Content-Type", ct)
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("X-Accel-Buffering", "no")
        w.WriteHeader(resp.StatusCode)

        flusher, _ := w.(http.Flusher)
        buf := make([]byte, 4096)
        for {
                n, rerr := resp.Body.Read(buf)
                if n > 0 {
                        if _, werr := w.Write(buf[:n]); werr != nil {
                                return
                        }
                        if flusher != nil {
                                flusher.Flush()
                        }
                }
                if rerr != nil {
                        return
                }
        }
}

// ---------- health / status ----------

func healthHandler(w http.ResponseWriter, r *http.Request) {
        mode := "free-no-key"
        if cfg.APIKey != "" {
                mode = "api-key"
        }
        writeJSON(w, 200, map[string]interface{}{
                "service":  "opencode-bridge",
                "connected": true,
                "mode":     mode,
                "base_url": cfg.BaseURL,
                "models":   len(fetchModels()),
        })
}

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
                w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, x-session-id, x-opencode-session")
                if r.Method == "OPTIONS" {
                        w.WriteHeader(200)
                        return
                }
                next.ServeHTTP(w, r)
        })
}
