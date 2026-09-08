// client.go — pure-HTTP DeepSeek chat client (package dsbridge).
//
// Port of the reference client (deepseek/client.py). For every request it:
//   1. creates a chat session   (POST /api/v0/chat_session/create)
//   2. fetches a PoW challenge   (POST /api/v0/chat/create_pow_challenge)
//   3. solves it with DeepSeek's own WASM (pow.go)
//   4. POSTs the completion with the x-ds-pow-response header
//   5. parses the SSE stream into content/reasoning events
//
// Every request runs on a THROWAWAY chat session (stateless bridge, same
// contract as the other bridges).

package dsbridge

import (
        "bufio"
        "bytes"
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "io"
        "net/http"
        "regexp"
        "strconv"
        "strings"
        "time"
)

const completionPath = "/api/v0/chat/completion"

func baseHeaders(token string) map[string]string {
        return map[string]string{
                "authorization":         "Bearer " + token,
                "accept":                "*/*",
                "content-type":          "application/json",
                "user-agent":            defaultUserAgent,
                "origin":                BASE_URL,
                "referer":               BASE_URL + "/",
                "x-app-version":         "2.0.0",
                "x-client-version":      "2.0.0",
                "x-client-platform":     "web",
                "x-client-locale":       "en_US",
                "x-client-bundle-id":    "com.deepseek.chat",
                "x-client-timezone-offset": "0",
        }
}

var dsHTTP = &http.Client{Timeout: 300 * time.Second}

// ---------- protocol steps ----------

func createChatSession(ctx context.Context, acc *Account) (string, error) {
        body, status, err := dsPost(ctx, acc, "/api/v0/chat_session/create", map[string]interface{}{})
        if err != nil {
                return "", err
        }
        if status != 200 {
                return "", classifyUpstreamError(status, body)
        }
        var env struct {
                Code int `json:"code"`
                Msg  string `json:"msg"`
                Data struct {
                        BizData struct {
                                ChatSession struct {
                                        ID string `json:"id"`
                                } `json:"chat_session"`
                        } `json:"biz_data"`
                } `json:"data"`
        }
        if err := json.Unmarshal(body, &env); err != nil {
                return "", fmt.Errorf("chat_session/create: unexpected response shape: %s", truncate(body, 200))
        }
        if env.Code != 0 {
                return "", classifyAPIError(env.Code, env.Msg)
        }
        if env.Data.BizData.ChatSession.ID == "" {
                return "", fmt.Errorf("chat_session/create: empty session id: %s", truncate(body, 200))
        }
        return env.Data.BizData.ChatSession.ID, nil
}

func powHeader(ctx context.Context, acc *Account, targetPath string) (string, error) {
        body, status, err := dsPost(ctx, acc, "/api/v0/chat/create_pow_challenge",
                map[string]interface{}{"target_path": targetPath})
        if err != nil {
                return "", err
        }
        if status != 200 {
                return "", classifyUpstreamError(status, body)
        }
        var env struct {
                Code int `json:"code"`
                Msg  string `json:"msg"`
                Data struct {
                        BizData struct {
                                Challenge dsChallenge `json:"challenge"`
                        } `json:"biz_data"`
                } `json:"data"`
        }
        if err := json.Unmarshal(body, &env); err != nil {
                return "", fmt.Errorf("create_pow_challenge: unexpected response shape: %s", truncate(body, 200))
        }
        if env.Code != 0 {
                return "", classifyAPIError(env.Code, env.Msg)
        }
        p, err := getPow()
        if err != nil {
                return "", err
        }
        return p.makeHeader(env.Data.BizData.Challenge)
}

func dsPost(ctx context.Context, acc *Account, path string, payload interface{}) ([]byte, int, error) {
        raw, _ := json.Marshal(payload)
        req, err := http.NewRequestWithContext(ctx, "POST", BASE_URL+path, bytes.NewReader(raw))
        if err != nil {
                return nil, 0, err
        }
        for k, v := range baseHeaders(acc.Token) {
                req.Header.Set(k, v)
        }
        resp, err := dsHTTP.Do(req)
        if err != nil {
                return nil, 0, fmt.Errorf("DeepSeek request failed: %w", err)
        }
        defer resp.Body.Close()
        data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if err != nil {
                return nil, resp.StatusCode, err
        }
        return data, resp.StatusCode, nil
}

// ---------- error classification ----------

func classifyUpstreamError(status int, body []byte) error {
        msg := truncate(body, 300)
        switch status {
        case 401, 403:
                return fmt.Errorf("%d — DeepSeek token rejected (expired? re-login): %s | توکن رد شد (منقضی؟ دوباره ds-login بزن)", status, msg)
        case 429:
                return fmt.Errorf("429 — DeepSeek rate limit | سقف نرخ دیپ‌سیک (%s)", msg)
        default:
                return fmt.Errorf("%d — DeepSeek upstream error: %s", status, msg)
        }
}

func classifyAPIError(code int, msg string) error {
        switch code {
        case 403:
                return fmt.Errorf("DeepSeek account error (%s) — login again | خطای اکانت — دوباره ورود کن", msg)
        default:
                return fmt.Errorf("DeepSeek API error %d: %s", code, msg)
        }
}

func truncate(b []byte, n int) string {
        s := strings.TrimSpace(string(b))
        if len(s) > n {
                return s[:n] + "…"
        }
        return s
}

func statusFromError(errMsg string) int {
        switch {
        case strings.Contains(errMsg, "401"), strings.Contains(errMsg, "403"),
                strings.Contains(errMsg, "token rejected"), strings.Contains(errMsg, "توکن رد شد"),
                strings.Contains(errMsg, "No DeepSeek account"), strings.Contains(errMsg, "هیچ اکانت"):
                return 401
        case strings.Contains(errMsg, "429"), strings.Contains(errMsg, "rate limit"), strings.Contains(errMsg, "سقف نرخ"),
                strings.Contains(errMsg, "cooling down"), strings.Contains(errMsg, "cooldown"):
                return 429
        default:
                return 502
        }
}

// ---------- upstream send (handler-facing) ----------

// sendUpstreamWithFailover runs one completion on a throwaway session and
// streams events back. On a pre-first-chunk failure (429/expiry) it retries
// on another pool account.
func sendUpstreamWithFailover(prompt string, opts SendOptions) (<-chan UpstreamResult, error) {
        if accounts == nil || accounts.Len() == 0 {
                return nil, errors.New(noAccountsMessage())
        }

        first := opts.Account
        if first == nil {
                var err error
                first, err = accounts.Pick(context.Background())
                if err != nil {
                        return nil, err
                }
        }

        tried := map[int]bool{first.ID: true}
        attempts := 0
        maxAttempts := minInt(3, accounts.Len())

        var lastErr error
        acc := first
        for {
                attempts++
                ch, err := streamCompletion(context.Background(), acc, prompt, opts)
                if err == nil {
                        return ch, nil
                }
                lastErr = err
                logError(fmt.Sprintf("account %s failed (attempt %d/%d): %v", acc.Label, attempts, maxAttempts, err))
                if isAuthError(err) {
                        acc.ReportAuthFail("rejected")
                } else if isRateError(err) {
                        acc.Report429()
                } else {
                        acc.ReportError()
                }
                if attempts >= maxAttempts {
                        return nil, lastErr
                }
                next := accounts.pickOther(acc)
                if next == nil {
                        return nil, lastErr
                }
                acc = next
                tried[acc.ID] = true
        }
}

func isAuthError(err error) bool {
        s := err.Error()
        return strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "rejected")
}

func isRateError(err error) bool {
        return strings.Contains(err.Error(), "429")
}

// streamCompletion performs the full protocol for one request and returns a
// channel of stream events (closed when the upstream stream ends).
func streamCompletion(ctx context.Context, acc *Account, prompt string, opts SendOptions) (<-chan UpstreamResult, error) {
        sessionID, err := createChatSession(ctx, acc)
        if err != nil {
                return nil, err
        }
        pow, err := powHeader(ctx, acc, completionPath)
        if err != nil {
                return nil, err
        }

        body := map[string]interface{}{
                "chat_session_id":  sessionID,
                "parent_message_id": nil,
                "prompt":           prompt,
                "ref_file_ids":     []interface{}{},
                "thinking_enabled": opts.Thinking != nil && *opts.Thinking,
                "search_enabled":   opts.WebSearch != nil && *opts.WebSearch,
                "action":           nil,
                "preempt":          false,
        }
        if opts.Model != "" {
                body["model_type"] = opts.Model
        }
        raw, _ := json.Marshal(body)

        req, err := http.NewRequestWithContext(ctx, "POST", BASE_URL+completionPath, bytes.NewReader(raw))
        if err != nil {
                return nil, err
        }
        for k, v := range baseHeaders(acc.Token) {
                req.Header.Set(k, v)
        }
        req.Header.Set("x-ds-pow-response", pow)

        resp, err := dsHTTP.Do(req)
        if err != nil {
                return nil, fmt.Errorf("DeepSeek request failed: %w", err)
        }
        if resp.StatusCode != 200 {
                b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
                resp.Body.Close()
                return nil, classifyUpstreamError(resp.StatusCode, b)
        }

        ch := make(chan UpstreamResult, 64)
        go func() {
                defer close(ch)
                defer resp.Body.Close()
                acc.ReportOK()
                parseDSStream(resp.Body, ch)
        }()
        return ch, nil
}

// ---------- SSE parsing (port of deepseek/client.py::_parse_sse) ----------
//
// The stream sends an initial snapshot frame (v = full response object with
// fragments[].content/type), then a series of append frames:
//   {"p":"response/fragments/-1/content","o":"APPEND","v":" what"}  (set path)
//   {"v":"'s"}                                                      (append)
// Fragment typing: explicit indices resolve via the last snapshot's type
// table; "-1" (ambiguous) falls back to "content". Paths mentioning thinking
// or a THINK-typed fragment yield reasoning events.

type dsFragTypes map[int]string

func parseDSStream(r io.Reader, ch chan<- UpstreamResult) {
        sc := bufio.NewScanner(r)
        sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

        activePath := ""
        activeKind := "content"
        fragTypes := dsFragTypes{}

        emit := func(kind, text string) {
                if text == "" {
                        return
                }
                if kind == "reasoning" {
                        ch <- UpstreamResult{Reasoning: text}
                } else {
                        ch <- UpstreamResult{Chunk: text}
                }
        }

        for sc.Scan() {
                line := strings.TrimSpace(sc.Text())
                if line == "" || !strings.HasPrefix(line, "data:") {
                        continue
                }
                payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
                if payload == "" || payload == "[DONE]" {
                        continue
                }
                var obj map[string]interface{}
                if err := json.Unmarshal([]byte(payload), &obj); err != nil {
                        continue
                }

                // API-level error frame?
                if code, ok := obj["code"].(float64); ok && code != 0 {
                        msg, _ := obj["msg"].(string)
                        ch <- UpstreamResult{Err: classifyAPIError(int(code), msg)}
                        return
                }

                v := obj["v"]

                // Snapshot frame: full response object.
                if m, ok := v.(map[string]interface{}); ok {
                        if respObj, ok := m["response"].(map[string]interface{}); ok {
                                if frags, ok := respObj["fragments"].([]interface{}); ok {
                                        for idx, f := range frags {
                                                fm, ok := f.(map[string]interface{})
                                                if !ok {
                                                        continue
                                                }
                                                if t, ok := fm["type"].(string); ok && t != "" {
                                                        fragTypes[idx] = t
                                                }
                                                if t, _ := fm["type"].(string); t == "RESPONSE" {
                                                        if content, ok := fm["content"].(string); ok && content != "" {
                                                                activePath = "response/fragments/-1/content"
                                                                activeKind = "content"
                                                                emit("content", content)
                                                        }
                                                }
                                                // THINKING fragments carry their text in the snapshot
                                                // too — emit it as reasoning so nothing is lost.
                                                if t, _ := fm["type"].(string); strings.EqualFold(t, "THINKING") {
                                                        if content, ok := fm["content"].(string); ok && content != "" {
                                                                emit("reasoning", content)
                                                        }
                                                }
                                        }
                                }
                        }
                        continue
                }

                // Path-setting append frame.
                if p, ok := obj["p"].(string); ok && p != "" {
                        activePath = p
                        if strings.HasSuffix(activePath, "message_id") {
                                if _, ok := v.(float64); ok {
                                        // message_id captured but unused for stateless bridge
                                        _ = v
                                }
                        }
                        if o, _ := obj["o"].(string); o == "APPEND" {
                                if s, ok := v.(string); ok && strings.HasSuffix(activePath, "content") {
                                        activeKind = kindForPath(activePath, fragTypes)
                                        emit(activeKind, s)
                                }
                        }
                        continue
                }

                // Bare append to the current path.
                if s, ok := v.(string); ok && activePath != "" && strings.HasSuffix(activePath, "content") {
                        emit(activeKind, s)
                }
        }
}

var fragIdxRe = regexp.MustCompile(`fragments/(\d+)/`)

func kindForPath(path string, fragTypes dsFragTypes) string {
        low := strings.ToLower(path)
        if strings.Contains(low, "thinking") || strings.Contains(low, "reason") {
                return "reasoning"
        }
        if m := fragIdxRe.FindStringSubmatch(path); m != nil {
                idx, _ := strconv.Atoi(m[1])
                if t, ok := fragTypes[idx]; ok && strings.Contains(strings.ToUpper(t), "THINK") {
                        return "reasoning"
                }
        }
        return "content"
}
