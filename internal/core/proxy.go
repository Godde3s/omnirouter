// proxy.go — request forwarding with cross-provider failover, streaming,
// per-provider retry + cooldown, usage extraction and request logging.
// The heart of the router.
//
// Forwarding rules:
//   bridge providers → internal 127.0.0.1 listeners; Authorization is the
//                      shared internal AUTH_TOKEN (model already provider-
//                      local thanks to the resolve step).
//   custom providers → the real external endpoint; Authorization is the
//                      provider's own key.
//
// Failover: candidates are tried in order (RETRY_PER_PROVIDER attempts each);
// a candidate is skipped when it is on cooldown or when the HTTP status is
// retryable (429/401/403/5xx/transport error) AND nothing has been streamed
// to the client yet. Once the first byte is flushed the response is
// committed — mid-stream failures surface as an SSE error frame (written by
// the bridges themselves).
//
// Usage accounting: the router scans upstream responses (JSON body or the
// tail of an SSE stream) for OpenAI/Anthropic `usage` objects and records
// real token counts per key, provider, model and hour bucket.

package core

import (
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "regexp"
        "strconv"
        "strings"
        "sync"
        "time"
)

type LogEntry struct {
        Time      int64  `json:"time"`
        Key       string `json:"key"`
        Model     string `json:"model"`
        Provider  string `json:"provider"`
        Status    int    `json:"status"`
        Stream    bool   `json:"stream"`
        LatencyMS int64  `json:"latency_ms"`
        TokensIn  int64  `json:"tokens_in,omitempty"`
        TokensOut int64  `json:"tokens_out,omitempty"`
        Err       string `json:"error,omitempty"`
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

type forwarder struct {
        registry      *Registry
        store         *Store
        internalToken string
        client        *http.Client
        retryPer      int // RETRY_PER_PROVIDER
        cooldownSec   int64
        timeoutSec    int64 // non-stream upstream timeout (REQUEST_TIMEOUT)
}

func newForwarder(reg *Registry, st *Store, internalToken string, retryPer int, cooldownSec, timeoutSec int64) *forwarder {
        if retryPer < 1 {
                retryPer = 1
        }
        if cooldownSec <= 0 {
                cooldownSec = 20
        }
        if timeoutSec <= 0 {
                timeoutSec = 300
        }
        return &forwarder{
                registry:      reg,
                store:         st,
                internalToken: internalToken,
                client:        &http.Client{Timeout: 0}, // no global timeout: streams run long
                retryPer:      retryPer,
                cooldownSec:   cooldownSec,
                timeoutSec:    timeoutSec,
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
        case "gemini":
                return "Bearer gemini"
        }
        return ""
}

func retryable(status int) bool {
        // 401/403 are provider-scoped here (each bridge has its own credentials),
        // so they legitimately trigger provider failover; 429/5xx are transient.
        return status == 429 || status == 401 || status == 403 || status >= 500
}

// ---------- usage extraction ----------

var usageRe = regexp.MustCompile(`"usage"\s*:\s*(\{[^{}]*\})`)

type usageFields struct {
        PromptTokens     int64 `json:"prompt_tokens"`
        CompletionTokens int64 `json:"completion_tokens"`
        InputTokens      int64 `json:"input_tokens"`
        OutputTokens     int64 `json:"output_tokens"`
}

// extractUsage finds OpenAI/Anthropic usage objects. Anthropic streams emit
// input_tokens in message_start and output_tokens in message_delta, so the
// scan keeps the first non-zero input and the last non-zero output.
func extractUsage(b []byte) (in, out int64) {
        matches := usageRe.FindAllSubmatch(b, -1)
        for _, m := range matches {
                var u usageFields
                if len(m) < 2 || json.Unmarshal(m[1], &u) != nil {
                        continue
                }
                if u.PromptTokens > 0 {
                        in = u.PromptTokens
                }
                if u.InputTokens > 0 && in == 0 {
                        in = u.InputTokens
                }
                if u.CompletionTokens > 0 {
                        out = u.CompletionTokens
                }
                if u.OutputTokens > 0 {
                        out = u.OutputTokens
                }
        }
        return
}

// estimateTokens is the rough fallback (≈4 chars/token) when the upstream
// did not report usage.
func estimateTokens(n int) int64 {
        if n <= 0 {
                return 0
        }
        return int64(n / 4)
}

// tailTee keeps the last `keep` bytes of a stream so usage frames can be
// parsed after forwarding finishes — zero-copy while hot path streams.
type tailTee struct {
        r    io.Reader
        tail []byte
}

func (t *tailTee) Read(p []byte) (int, error) {
        n, err := t.r.Read(p)
        if n > 0 {
                t.tail = append(t.tail, p[:n]...)
                if len(t.tail) > 8192 {
                        t.tail = t.tail[len(t.tail)-8192:]
                }
        }
        return n, err
}

// ---------- per-key model allowlist ----------

// modelAllowed checks an APIKey's AllowedModels list: exact ids, "provider/*"
// wildcards (matching both "provider/model" and plain models owned by that
// provider), or "*" for everything.
func modelAllowed(rules []string, model string, reg *Registry) bool {
        if len(rules) == 0 {
                return true
        }
        for _, rule := range rules {
                rule = strings.TrimSpace(rule)
                if rule == "" || rule == "*" {
                        return true
                }
                if rule == model {
                        return true
                }
                prov := strings.TrimSuffix(rule, "/*")
                if prov != rule && strings.HasPrefix(model, prov+"/") {
                        return true // "qwen/*" matches "qwen/anything"
                }
                if !strings.Contains(model, "/") && !strings.Contains(prov, "/") {
                        // plain model vs provider rule → catalog owner check
                        if owner := reg.OwnerOf(model); owner != "" && strings.EqualFold(strings.TrimPrefix(owner, "custom:"), prov) {
                                return true
                        }
                }
        }
        return false
}

// ---------- chat completions ----------

// ForwardChat drives POST /v1/chat/completions.
func (f *forwarder) ForwardChat(w http.ResponseWriter, r *http.Request, body []byte, keyName string) {
        start := time.Now()
        var req struct {
                Model  string `json:"model"`
                Stream *bool  `json:"stream"`
        }
        _ = json.Unmarshal(body, &req)
        // OpenAI spec: stream defaults to false when omitted (clients like
        // OpenCode/Cline often drop the field entirely and expect JSON back).
        stream := req.Stream != nil && *req.Stream

        // per-key allowlist
        if k, ok := f.store.LookupKey(keyName); ok && !modelAllowed(k.AllowedModels, req.Model, f.registry) {
                writeJSON(w, 403, formatRouterError(
                        fmt.Sprintf("کلید «%s» مجاز به مدل «%s» نیست / key not allowed to use model «%s» — allowed: %v",
                                k.Name, req.Model, req.Model, k.AllowedModels),
                        "model_not_allowed"))
                return
        }

        chain := f.registry.Resolve(req.Model)
        if len(chain) == 0 {
                writeJSON(w, 404, formatRouterError(
                        fmt.Sprintf("مدل «%s» روی هیچ ارائه‌دهنده‌ای پیدا نشد / model not found on any provider — ببین /v1/models", req.Model),
                        "model_not_found"))
                return
        }

        // non-stream requests get a hard upstream timeout; streams run unbounded
        ctx := r.Context()
        var cancel context.CancelFunc = func() {}
        if !stream {
                ctx, cancel = context.WithTimeout(ctx, time.Duration(f.timeoutSec)*time.Second)
        }
        defer cancel()

        var lastStatus int
        var lastErr string
        tried := false

        // tryCandidate runs the retry loop against one provider; returns true when
        // the response was delivered to the client.
        tryCandidate := func(p *Provider, path string, consume func(*http.Response, *Provider, string)) bool {
                upModel := req.Model
                if len(p.Models) == 1 {
                        upModel = p.Models[0]
                }
                for attempt := 0; attempt < f.retryPer; attempt++ {
                        resp, err := f.openUpstream(ctx, r, p, path, upModel, body)
                        if err == nil && !retryable(resp.StatusCode) {
                                consume(resp, p, upModel)
                                return true
                        }
                        lastStatus, lastErr = failureInfo(resp, err)
                        log.Printf("[Router] provider %s attempt %d/%d failed for %s (HTTP %d): %s",
                                p.ID, attempt+1, f.retryPer, req.Model, lastStatus, lastErr)
                }
                // candidate exhausted → short cooldown so later requests prefer healthier providers
                f.registry.Cooldown(p.ID, f.cooldownSec)
                f.store.RecordStat(p.ID, req.Model, 0, 0, true)
                return false
        }

        // pass 1 — skip providers on cooldown (they just failed recently)
        for _, p := range chain {
                if f.registry.Cooling(p.ID) {
                        lastErr = "provider on cooldown (recent failures)"
                        log.Printf("[Router] skip %s for %s — %s", p.ID, req.Model, lastErr)
                        continue
                }
                if tryCandidate(p, "/v1/chat/completions", func(resp *http.Response, p *Provider, up string) {
                        f.consumeAndRecord(w, resp, stream, keyName, p.ID, req.Model, up, stream, body, start)
                }) {
                        return
                }
                tried = true
        }
        // pass 2 — every candidate was on cooldown: cooldown is a preference, not
        // a hard block, so try them anyway rather than failing the request.
        if !tried {
                for _, p := range chain {
                        if tryCandidate(p, "/v1/chat/completions", func(resp *http.Response, p *Provider, up string) {
                                f.consumeAndRecord(w, resp, stream, keyName, p.ID, req.Model, up, stream, body, start)
                        }) {
                                return
                        }
                }
        }

        // Every candidate failed.
        f.store.RecordStat("router", req.Model, 0, 0, true)
        f.store.RecordUsage(keyName, 0, 0, true)
        addLog(LogEntry{Time: time.Now().Unix(), Key: keyName, Model: req.Model,
                Provider: chain[0].ID, Status: lastStatus, Stream: stream,
                LatencyMS: time.Since(start).Milliseconds(), Err: lastErr})
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

// ---------- anthropic messages ----------

// ForwardMessages drives POST /v1/messages (Anthropic protocol).
func (f *forwarder) ForwardMessages(w http.ResponseWriter, r *http.Request, body []byte, keyName string) {
        start := time.Now()
        var req struct {
                Model  string `json:"model"`
                Stream bool   `json:"stream"`
        }
        _ = json.Unmarshal(body, &req)

        if k, ok := f.store.LookupKey(keyName); ok && !modelAllowed(k.AllowedModels, req.Model, f.registry) {
                writeJSON(w, 403, map[string]interface{}{
                        "type": "error",
                        "error": map[string]interface{}{
                                "type": "permission_error",
                                "message": fmt.Sprintf("کلید «%s» مجاز به مدل «%s» نیست / key not allowed to use model «%s» — allowed: %v",
                                        k.Name, req.Model, req.Model, k.AllowedModels),
                        },
                })
                return
        }

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

        ctx := r.Context()
        var cancel context.CancelFunc = func() {}
        if !req.Stream {
                ctx, cancel = context.WithTimeout(ctx, time.Duration(f.timeoutSec)*time.Second)
        }
        defer cancel()

        var lastStatus int
        var lastErr string
        tried := false
        tryCandidateM := func(p *Provider) bool {
                upModel := req.Model
                if len(p.Models) == 1 {
                        upModel = p.Models[0]
                }
                for attempt := 0; attempt < f.retryPer; attempt++ {
                        resp, err := f.openUpstream(ctx, r, p, "/v1/messages", upModel, body)
                        if err == nil && !retryable(resp.StatusCode) {
                                f.consumeMessages(w, resp, req.Stream, keyName, p.ID, req.Model, body, start)
                                return true
                        }
                        lastStatus, lastErr = failureInfo(resp, err)
                        log.Printf("[Router] provider %s attempt %d/%d failed for %s via /v1/messages (HTTP %d): %s",
                                p.ID, attempt+1, f.retryPer, req.Model, lastStatus, lastErr)
                }
                f.registry.Cooldown(p.ID, f.cooldownSec)
                f.store.RecordStat(p.ID, req.Model, 0, 0, true)
                return false
        }
        for _, p := range chain {
                if f.registry.Cooling(p.ID) {
                        lastErr = "provider on cooldown (recent failures)"
                        continue
                }
                if tryCandidateM(p) {
                        return
                }
                tried = true
        }
        if !tried {
                for _, p := range chain {
                        if tryCandidateM(p) {
                                return
                        }
                }
        }

        f.store.RecordStat("router", req.Model, 0, 0, true)
        f.store.RecordUsage(keyName, 0, 0, true)
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
        case 403:
                return "permission_error"
        case 404:
                return "not_found_error"
        default:
                return "api_error"
        }
}

// ---------- response consumption + usage recording ----------

// consumeAndRecord streams/copies a successful /v1/chat/completions response
// to the client and records usage stats. Returns the recorded token counts.
func (f *forwarder) consumeAndRecord(w http.ResponseWriter, resp *http.Response, stream bool,
        keyName, providerID, model, upModel string, isStream bool, reqBody []byte, start time.Time) (int64, int64) {

        var tin, tout int64
        if stream {
                tee := &tailTee{r: resp.Body}
                f.streamResponseBody(w, resp, tee, true)
                tin, tout = extractUsage(tee.tail)
        } else {
                raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
                resp.Body.Close()
                ct := resp.Header.Get("Content-Type")
                if ct == "" {
                        ct = "application/json"
                }
                w.Header().Set("Content-Type", ct)
                w.Header().Set("Cache-Control", "no-cache")
                w.WriteHeader(resp.StatusCode)
                w.Write(raw)
                tin, tout = extractUsage(raw)
        }
        if tin == 0 {
                tin = estimateTokens(len(reqBody))
        }
        f.recordSuccess(keyName, providerID, model, tin, tout, stream, start)
        return tin, tout
}

// consumeMessages is the Anthropic variant of consumeAndRecord.
func (f *forwarder) consumeMessages(w http.ResponseWriter, resp *http.Response, stream bool,
        keyName, providerID, model string, reqBody []byte, start time.Time) {

        var tin, tout int64
        if stream {
                tee := &tailTee{r: resp.Body}
                f.streamResponseBody(w, resp, tee, true)
                tin, tout = extractUsage(tee.tail)
        } else {
                raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
                resp.Body.Close()
                ct := resp.Header.Get("Content-Type")
                if ct == "" {
                        ct = "application/json"
                }
                w.Header().Set("Content-Type", ct)
                w.WriteHeader(resp.StatusCode)
                w.Write(raw)
                tin, tout = extractUsage(raw)
        }
        if tin == 0 {
                tin = estimateTokens(len(reqBody))
        }
        f.recordSuccess(keyName, providerID, model, tin, tout, stream, start)
}

func (f *forwarder) recordSuccess(keyName, providerID, model string, tin, tout int64, stream bool, start time.Time) {
        f.store.RecordStat(providerID, model, tin, tout, false)
        f.store.RecordUsage(keyName, tin, tout, false)
        addLog(LogEntry{Time: time.Now().Unix(), Key: keyName, Model: model,
                Provider: providerID, Status: 200, Stream: stream,
                LatencyMS: time.Since(start).Milliseconds(), TokensIn: tin, TokensOut: tout})
}

// ---------- upstream plumbing ----------

// openUpstream performs one attempt against one provider and returns the
// live response (caller owns the body). Failure info is extracted and the
// body closed by failureInfo when the caller decides to retry.
func (f *forwarder) openUpstream(ctx context.Context, r *http.Request, p *Provider, path, upModel string, body []byte) (*http.Response, error) {
        // Rewrite the model field to the provider-local id.
        var payload map[string]json.RawMessage
        if err := json.Unmarshal(body, &payload); err == nil {
                if _, ok := payload["model"]; ok {
                        payload["model"], _ = json.Marshal(upModel)
                        body, _ = json.Marshal(payload)
                }
        }

        req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+path, bytes.NewReader(body))
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

// streamResponseBody copies the upstream response to the client with true
// streaming: every read chunk is written and flushed immediately, so SSE
// clients see tokens as they arrive.
func (f *forwarder) streamResponseBody(w http.ResponseWriter, resp *http.Response, body io.Reader, stream bool) {
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
                n, err := body.Read(buf)
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

func statusOr(status, def int) int {
        if status >= 400 {
                return status
        }
        return def
}

// EstimateTokens exposes the rough char/4 estimator for count_tokens.
func EstimateTokens(s string) int64 { return estimateTokens(len(s)) }

var _ = strconv.Itoa
