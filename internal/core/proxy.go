// proxy.go — request forwarding with cross-provider failover, streaming and
// request logging. The heart of the router.
//
// Forwarding rules:
//   bridge providers → internal 127.0.0.1 listeners; Authorization is the
//                      shared internal AUTH_TOKEN (model already provider-
//                      local thanks to the resolve step).
//   custom providers → the real external endpoint; Authorization is the
//                      provider's own key.
//
// Failover: candidates are tried in order; a candidate is skipped when the
// HTTP status is retryable (429/5xx/transport error) AND nothing has been
// streamed to the client yet. Once the first byte is flushed the response is
// committed — mid-stream failures surface as an SSE error frame (written by
// the bridges themselves).

package core

import (
        "bytes"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "strings"
        "sync"
        "time"
)

type LogEntry struct {
        Time     int64  `json:"time"`
        Key      string `json:"key"`
        Model    string `json:"model"`
        Provider string `json:"provider"`
        Status   int    `json:"status"`
        Stream   bool   `json:"stream"`
        LatencyMS int64 `json:"latency_ms"`
        Err      string `json:"error,omitempty"`
}

type logRing struct {
        mu  sync.Mutex
        buf []LogEntry
}

var logs = &logRing{buf: make([]LogEntry, 0, 512)}

func addLog(e LogEntry) {
        logs.mu.Lock()
        defer logs.mu.Unlock()
        logs.buf = append(logs.buf, e)
        if len(logs.buf) > 500 {
                logs.buf = logs.buf[len(logs.buf)-500:]
        }
}

func RecentLogs() []LogEntry {
        logs.mu.Lock()
        defer logs.mu.Unlock()
        out := make([]LogEntry, len(logs.buf))
        copy(out, logs.buf)
        // newest first
        for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
                out[i], out[j] = out[j], out[i]
        }
        return out
}

var _ = json.Marshal

type forwarder struct {
        registry      *Registry
        internalToken string
        client        *http.Client
}

func newForwarder(reg *Registry, internalToken string) *forwarder {
        return &forwarder{
                registry:      reg,
                internalToken: internalToken,
                client:        &http.Client{Timeout: 0}, // no global timeout: streams run long
        }
}

// bridgeDefaultTokens are used only when AUTH_TOKEN is unset in the env (the
// embedded bridges fall back to their own defaults in exactly the same case).
func (f *forwarder) bridgeAuthHeader(providerID string) string {
        if f.internalToken != "" {
                return "Bearer " + f.internalToken
        }
        switch providerID {
        case "glm":
                return "Bearer Waguri"
        case "qwen":
                return "Bearer qwen"
        case "ds":
                return "Bearer deepseek"
        }
        return ""
}

func retryable(status int) bool {
        // 401/403 are provider-scoped here (each bridge has its own credentials),
        // so they legitimately trigger provider failover; 429/5xx are transient.
        return status == 429 || status == 401 || status == 403 || status >= 500
}

// ForwardChat drives POST /v1/chat/completions.
func (f *forwarder) ForwardChat(w http.ResponseWriter, r *http.Request, body []byte, keyName string) {
        start := time.Now()
        var req struct {
                Model  string `json:"model"`
                Stream *bool  `json:"stream"`
        }
        _ = json.Unmarshal(body, &req)
        stream := req.Stream == nil || *req.Stream

        chain := f.registry.Resolve(req.Model)
        if len(chain) == 0 {
                writeJSON(w, 404, formatRouterError(
                        fmt.Sprintf("مدل «%s» روی هیچ ارائه‌دهنده‌ای پیدا نشد / model not found on any provider — ببین /v1/models", req.Model),
                        "model_not_found"))
                return
        }

        var lastStatus int
        var lastErr string
        for _, p := range chain {
                upModel := req.Model
                if len(p.Models) == 1 {
                        upModel = p.Models[0]
                }
                resp, err := f.openUpstream(r, p, "/v1/chat/completions", upModel, body)
                if err == nil && !retryable(resp.StatusCode) {
                        entry := LogEntry{Time: time.Now().Unix(), Key: keyName, Model: req.Model,
                                Provider: p.ID, Status: resp.StatusCode, Stream: stream, LatencyMS: time.Since(start).Milliseconds()}
                        addLog(entry)
                        f.streamResponse(w, resp, stream)
                        return
                }
                lastStatus, lastErr = failureInfo(resp, err)
                log.Printf("[Router] provider %s failed for %s (HTTP %d): %s — trying next candidate", p.ID, req.Model, lastStatus, lastErr)
        }

        // Every candidate failed.
        entry := LogEntry{Time: time.Now().Unix(), Key: keyName, Model: req.Model,
                Provider: chain[0].ID, Status: lastStatus, Stream: stream,
                LatencyMS: time.Since(start).Milliseconds(), Err: lastErr}
        addLog(entry)
        if stream {
                w.Header().Set("Content-Type", "text/event-stream")
                w.Header().Set("Cache-Control", "no-cache")
                fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
                        "error": map[string]interface{}{
                                "message": fmt.Sprintf("همه‌ی ارائه‌دهنده‌ها برای «%s» خطا دادند / all providers failed: %s", req.Model, lastErr),
                                "type":    "api_error",
                                "code":    "all_providers_failed",
                        },
                }))
                fmt.Fprint(w, "data: [DONE]\n\n")
                return
        }
        writeJSON(w, statusOr(lastStatus, 502), formatRouterError(
                fmt.Sprintf("همه‌ی ارائه‌دهنده‌ها برای «%s» خطا دادند / all providers failed: %s", req.Model, lastErr),
                "api_error"))
}

// ForwardMessages drives POST /v1/messages (Anthropic protocol).
func (f *forwarder) ForwardMessages(w http.ResponseWriter, r *http.Request, body []byte, keyName string) {
        start := time.Now()
        var req struct {
                Model  string `json:"model"`
                Stream bool   `json:"stream"`
        }
        _ = json.Unmarshal(body, &req)

        chain := f.registry.Resolve(req.Model)
        if len(chain) == 0 {
                writeJSON(w, 404, map[string]interface{}{
                        "type": "error",
                        "error": map[string]interface{}{
                                "type":    "not_found_error",
                                "message": fmt.Sprintf("مدل «%s» پیدا نشد / model not found — ببین /v1/models", req.Model),
                        },
                })
                return
        }

        var lastStatus int
        var lastErr string
        for _, p := range chain {
                upModel := req.Model
                if len(p.Models) == 1 {
                        upModel = p.Models[0]
                }
                resp, err := f.openUpstream(r, p, "/v1/messages", upModel, body)
                if err == nil && !retryable(resp.StatusCode) {
                        addLog(LogEntry{Time: time.Now().Unix(), Key: keyName, Model: req.Model,
                                Provider: p.ID, Status: resp.StatusCode, Stream: req.Stream, LatencyMS: time.Since(start).Milliseconds()})
                        f.streamResponse(w, resp, req.Stream)
                        return
                }
                lastStatus, lastErr = failureInfo(resp, err)
                log.Printf("[Router] provider %s failed for %s via /v1/messages (HTTP %d): %s", p.ID, req.Model, lastStatus, lastErr)
        }

        addLog(LogEntry{Time: time.Now().Unix(), Key: keyName, Model: req.Model,
                Provider: chain[0].ID, Status: lastStatus, Stream: req.Stream,
                LatencyMS: time.Since(start).Milliseconds(), Err: lastErr})
        status := statusOr(lastStatus, 502)
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(status)
        json.NewEncoder(w).Encode(map[string]interface{}{
                "type": "error",
                "error": map[string]interface{}{
                        "type":    anthropicErrType(status),
                        "message": fmt.Sprintf("همه‌ی ارائه‌دهنده‌ها خطا دادند / all providers failed: %s", lastErr),
                },
        })
}

func anthropicErrType(status int) string {
        switch status {
        case 429:
                return "rate_limit_error"
        case 401:
                return "authentication_error"
        case 404:
                return "not_found_error"
        default:
                return "api_error"
        }
}

// openUpstream performs one attempt against one provider and returns the
// live response (caller owns the body). Failure info is extracted and the
// body closed by failureInfo when the caller decides to retry.
func (f *forwarder) openUpstream(r *http.Request, p *Provider, path, upModel string, body []byte) (*http.Response, error) {
        // Rewrite the model field to the provider-local id.
        var payload map[string]json.RawMessage
        if err := json.Unmarshal(body, &payload); err == nil {
                if _, ok := payload["model"]; ok {
                        payload["model"], _ = json.Marshal(upModel)
                        body, _ = json.Marshal(payload)
                }
        }

        req, err := http.NewRequestWithContext(r.Context(), "POST", p.BaseURL+path, bytes.NewReader(body))
        if err != nil {
                return nil, err
        }
        req.Header.Set("Content-Type", "application/json")
        switch p.Kind {
        case KindBridge:
                req.Header.Set("Authorization", f.bridgeAuthHeader(p.ID))
        case KindCustom:
                if p.APIKey != "" {
                        req.Header.Set("Authorization", "Bearer "+p.APIKey)
                }
        }
        // Anthropic-style clients may arrive with x-api-key / anthropic-version.
        if v := r.Header.Get("anthropic-version"); v != "" {
                req.Header.Set("anthropic-version", v)
        }

        return f.client.Do(req)
}

// failureInfo drains a failed attempt for diagnostics and closes its body.
func failureInfo(resp *http.Response, err error) (int, string) {
        if err != nil {
                return 0, err.Error()
        }
        defer resp.Body.Close()
        snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        s := strings.TrimSpace(string(snippet))
        if len(s) > 300 {
                s = s[:300] + "…"
        }
        if s == "" {
                return resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode)
        }
        return resp.StatusCode, s
}

// streamResponse copies the upstream response to the client with true
// streaming: every read chunk is written and flushed immediately, so SSE
// clients see tokens as they arrive.
func (f *forwarder) streamResponse(w http.ResponseWriter, resp *http.Response, stream bool) {
        defer resp.Body.Close()
        ct := resp.Header.Get("Content-Type")
        if ct == "" {
                if stream {
                        ct = "text/event-stream"
                } else {
                        ct = "application/json"
                }
        }
        w.Header().Set("Content-Type", ct)
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("X-Accel-Buffering", "no")
        w.WriteHeader(resp.StatusCode)

        flusher, _ := w.(http.Flusher)
        buf := make([]byte, 4096)
        for {
                n, err := resp.Body.Read(buf)
                if n > 0 {
                        if _, werr := w.Write(buf[:n]); werr != nil {
                                return // client gone
                        }
                        if flusher != nil {
                                flusher.Flush()
                        }
                }
                if err != nil {
                        return
                }
        }
}

func formatRouterError(message, code string) map[string]interface{} {
        return map[string]interface{}{
                "error": map[string]interface{}{
                        "message": message,
                        "type":    code,
                        "code":    code,
                        "param":   nil,
                },
        }
}

func errString(err error, status int, body []byte) string {
        if err != nil {
                return err.Error()
        }
        s := strings.TrimSpace(string(body))
        if len(s) > 300 {
                return s[:300] + "…"
        }
        if s == "" {
                return fmt.Sprintf("HTTP %d", status)
        }
        return s
}

func statusOr(status, def int) int {
        if status >= 400 {
                return status
        }
        return def
}
