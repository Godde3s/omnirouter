// Gemini web upstream client (package gbridge).
//
// Talks to gemini.google.com's internal StreamGenerate endpoint the same way
// the web app does:
//
//      GET  /app                 -> SNlM0e (at token), cfb2h (bl), FdrFJe (f.sid)
//      POST StreamGenerate       -> length-prefixed frames with cumulative snapshots
//
// Two auth modes:
//   - Guest:   no cookies at all, `at` sent empty (verified live: the endpoint
//     accepts unauthenticated generate calls; model registry restricted).
//   - Account: __Secure-1PSID (+ __Secure-1PSIDTS) cookie pair from the
//     user's browser; PSIDTS is auto-rotated and persisted when Google
//     refreshes it (RotateCookies / Set-Cookie).
//
// Conversation state: requests run with the temporary-chat flag set
// (inner[45]=1) so nothing lands in the account history — the bridge is
// stateless by construction, mirroring the Qwen/GLM throwaway-session flow.

package gbridge

import (
        "bufio"
        "bytes"
        "context"
        crand "crypto/rand"
        "encoding/json"
        "fmt"
        "io"
        "math/big"
        "net/http"
        "os"
        "regexp"
        "strconv"
        "strings"
        "sync"
        "time"
)

// ============================================================================
// INIT TOKENS (SNlM0e / bl / f.sid) — cached per identity
// ============================================================================

type initTokens struct {
        AT         string // SNlM0e — empty for guest sessions (still valid)
        BL         string // cfb2h build label
        SessionID  string // FdrFJe
        Language   string
        PushID     string
        psidts     string // refreshed __Secure-1PSIDTS from Set-Cookie (if any)
        guest      bool
        fetchedAt  time.Time
        statusCode int
        note       string // human-readable failure note when unusable
}

var (
        initMu       sync.Mutex
        initCache    = map[string]*initTokens{} // key: psid or "guest"
        initInflight = map[string]*sync.Once{}
)

var (
        reSNlM0e  = regexp.MustCompile(`"SNlM0e"\s*:\s*"([^"]+)"`)
        reBL      = regexp.MustCompile(`"cfb2h"\s*:\s*"([^"]+)"`)
        reSession = regexp.MustCompile(`"FdrFJe"\s*:\s*"([^"]+)"`)
        reLang    = regexp.MustCompile(`"TuX5cc"\s*:\s*"([^"]+)"`)
        rePushID  = regexp.MustCompile(`"qKIAYe"\s*:\s*"([^"]+)"`)
)

func initKey(acc *Account) string {
        if acc == nil || acc.PSID == "" {
                return "guest"
        }
        return acc.PSID
}

// getInitTokens returns cached init tokens for the identity, refreshing them
// when the TTL lapses. On failure the error is humanized Persian.
func getInitTokens(acc *Account) (*initTokens, error) {
        key := initKey(acc)
        ttl := time.Duration(config.InitTokenTTL) * time.Second

        initMu.Lock()
        if t, ok := initCache[key]; ok && t.statusCode == 200 && time.Since(t.fetchedAt) < ttl {
                initMu.Unlock()
                return t, nil
        }
        // Coalesce concurrent refreshes per identity.
        once, inflight := initInflight[key]
        if !inflight {
                once = &sync.Once{}
                initInflight[key] = once
        }
        initMu.Unlock()

        var result *initTokens
        var err error
        once.Do(func() {
                result, err = fetchInitTokens(acc)
                initMu.Lock()
                if result != nil && result.statusCode == 200 {
                        initCache[key] = result
                } else {
                        delete(initCache, key) // failed refresh — retry next request
                }
                delete(initInflight, key)
                initMu.Unlock()
        })
        if err != nil {
                return nil, err
        }
        if result == nil {
                // Another goroutine's fetch already cached tokens — reuse them.
                initMu.Lock()
                result = initCache[key]
                initMu.Unlock()
                if result == nil {
                        return nil, &upstreamErr{502, "Gemini init failed — no cached tokens"}
                }
        }
        if result.statusCode != 200 {
                return nil, &upstreamErr{statusFromError(result.note), result.note}
        }
        return result, nil
}

// invalidateInitTokens drops cached tokens for an identity (after auth fails).
func invalidateInitTokens(acc *Account) {
        initMu.Lock()
        delete(initCache, initKey(acc))
        initMu.Unlock()
}

func fetchInitTokens(acc *Account) (*initTokens, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()

        req, err := http.NewRequestWithContext(ctx, "GET", URL_INIT, nil)
        if err != nil {
                return nil, err
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
        req.Header.Set("Accept-Language", "en-US,en;q=0.9")
        req.Header.Set("Referer", "https://gemini.google.com/")
        if acc != nil && acc.PSID != "" {
                req.Header.Set("Cookie", accountCookieHeader(acc))
        }

        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return nil, &upstreamErr{502, "خطا در اتصال به gemini.google.com: " + err.Error()}
        }
        defer resp.Body.Close()

        t := &initTokens{statusCode: resp.StatusCode, guest: acc == nil || acc.PSID == ""}
        if cookie := extractSetCookie(resp.Header.Get("Set-Cookie"), "__Secure-1PSIDTS"); cookie != "" {
                t.psidts = cookie
        }

        if resp.StatusCode == 429 {
                t.note = "IP شما موقتاً توسط گوگل محدود شده است (HTTP 429). چند دقیقه صبر کنید یا از پروکسی استفاده کنید."
                return t, nil
        }
        if resp.StatusCode == 401 || resp.StatusCode == 403 {
                t.note = "کوکی‌های اکانت گوگل رد شدند (HTTP " + strconv.Itoa(resp.StatusCode) + "). کوکی‌های __Secure-1PSID را تازه‌سازی کنید."
                return t, nil
        }
        if resp.StatusCode != 200 {
                t.note = "صفحه init جمنای وضعیت غیرعادی برگرداند (HTTP " + strconv.Itoa(resp.StatusCode) + "). بعداً تلاش کنید."
                return t, nil
        }

        body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
        if err != nil {
                t.note = "خواندن صفحه init جمنای ناموفق بود: " + err.Error()
                return t, nil
        }
        html := string(body)

        if m := reSNlM0e.FindStringSubmatch(html); m != nil {
                t.AT = m[1]
        }
        if m := reBL.FindStringSubmatch(html); m != nil {
                t.BL = m[1]
        }
        if m := reSession.FindStringSubmatch(html); m != nil {
                t.SessionID = m[1]
        }
        if m := reLang.FindStringSubmatch(html); m != nil && m[1] != "" {
                t.Language = m[1]
        }
        if m := rePushID.FindStringSubmatch(html); m != nil && m[1] != "" {
                t.PushID = m[1]
        }
        if t.Language == "" {
                t.Language = "en"
        }
        if t.PushID == "" {
                t.PushID = "feeds/mcudyrk2a4khkz"
        }

        if t.AT == "" {
                if t.guest {
                        // Guest sessions legitimately have no SNlM0e (at=""); the page
                        // itself must still look like the Gemini app.
                        if !strings.Contains(html, "WIZ_global_data") {
                                t.note = "پاسخ صفحه init جمنای قابل تشخیص نبود — احتمالاً IP مسدود است."
                        } else {
                                t.statusCode = 200
                        }
                        return t, nil
                }
                t.note = "کوکی‌های اکانت گوگل منقضی شده‌اند (SNlM0e یافت نشد). وارد gemini.google.com شوید و کوکی‌های __Secure-1PSID و __Secure-1PSIDTS را دوباره کپی کنید."
                return t, nil
        }

        // Google may rotate __Secure-1PSIDTS on this response — persist it.
        if t.psidts != "" && acc != nil && acc.PSID != "" && t.psidts != acc.PSIDTS {
                acc.updatePSIDTS(t.psidts)
        }
        return t, nil
}

// extractSetCookie pulls one cookie value out of a raw Set-Cookie header.
func extractSetCookie(setCookie, name string) string {
        for _, part := range strings.Split(setCookie, ";") {
                part = strings.TrimSpace(part)
                if eq := strings.Index(part, "="); eq > 0 {
                        if strings.EqualFold(strings.TrimSpace(part[:eq]), name) {
                                return strings.TrimSpace(part[eq+1:])
                        }
                }
        }
        return ""
}

// ============================================================================
// MODEL HEADERS — x-goog-ext-525001261-jspb selector
// ============================================================================

const (
        modelHeaderKey  = "x-goog-ext-525001261-jspb"
        tierHeaderA     = "x-goog-ext-73010989-jspb"
        tierHeaderB     = "x-goog-ext-73010990-jspb"
        uuidHeaderKey   = "x-goog-ext-525005358-jspb"
        STREAMING_INDEX = 7
        TEMPCHAT_INDEX  = 45
)

// buildModelHeader assembles the model selector array:
// [1,null,null,null,"<id>",null,null,0,[4,5,6,8],null,null,<capacity>,
//  null,null,<number>,<thinking 1|2>,"<session uuid>"]
func buildModelHeader(modelID string, capacity, modelNumber int, thinking bool, sessionUUID string) string {
        think := 1
        if thinking {
                think = 2
        }
        if capacity < 1 {
                capacity = 1
        }
        arr := []interface{}{
                1, nil, nil, nil, modelID, nil, nil, 0,
                []int{4, 5, 6, 8}, nil, nil, capacity, nil, nil, modelNumber, think, sessionUUID,
        }
        raw, _ := json.Marshal(arr)
        return string(raw)
}

// ============================================================================
// REQUEST BUILDING — inner f.req list (81 slots, web-app layout)
// ============================================================================

func buildInnerReqList(prompt string, fileData interface{}, language string, thinking bool, modelNumber int) []interface{} {
        inner := make([]interface{}, 81)
        inner[0] = []interface{}{prompt, 0, nil, fileData, nil, nil, 0}
        inner[1] = []string{language}
        inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
        inner[6] = []int{1}
        inner[STREAMING_INDEX] = 1
        inner[10] = 1
        inner[11] = 0
        inner[17] = [][]int{{0}}
        inner[18] = 0
        inner[27] = 1
        inner[30] = []int{4}
        inner[41] = []int{1}
        inner[TEMPCHAT_INDEX] = 1 // temporary chat: nothing saved server-side
        inner[53] = 0
        inner[61] = []interface{}{}
        inner[68] = 1
        inner[79] = modelNumber
        if thinking {
                inner[80] = 2 // extended thinking
        } else {
                inner[80] = 1
        }
        return inner
}

func buildGenerateRequest(prompt string, fileData interface{}, thinking bool, it *initTokens, mh *geminiModel, acc *Account) (*http.Request, context.CancelFunc, error) {
        language := it.Language
        if language == "" {
                language = "en"
        }
        sessionUUID := strings.ToUpper(generateUUID())
        inner := buildInnerReqList(prompt, fileData, language, thinking, mh.ModelNumber)
        inner[59] = sessionUUID

        innerJSON, err := json.Marshal(inner)
        if err != nil {
                return nil, nil, err
        }
        freq, err := json.Marshal([]interface{}{nil, string(innerJSON)})
        if err != nil {
                return nil, nil, err
        }

        ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.Timeouts.Default)*time.Millisecond)
        req, err := http.NewRequestWithContext(ctx, "POST", URL_GENERATE, strings.NewReader("at="+urlEncode(it.AT, "")+"&f.req="+urlEncode(string(freq), "")))
        if err != nil {
                cancel()
                return nil, nil, err
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
        req.Header.Set("Origin", "https://gemini.google.com")
        req.Header.Set("Referer", "https://gemini.google.com/")
        req.Header.Set("X-Same-Domain", "1")
        req.Header.Set(modelHeaderKey, buildModelHeader(mh.ModelID, mh.Capacity, mh.ModelNumber, thinking, sessionUUID))
        req.Header.Set(uuidHeaderKey, fmt.Sprintf(`["%s",1]`, sessionUUID))
        req.Header.Set(tierHeaderA, "[0]")
        req.Header.Set(tierHeaderB, "[0,0,0]")

        q := req.URL.Query()
        q.Set("hl", language)
        q.Set("_reqid", strconv.Itoa(10000+randIntn(89999)))
        q.Set("rt", "c")
        if it.BL != "" {
                q.Set("bl", it.BL)
        }
        if it.SessionID != "" {
                q.Set("f.sid", it.SessionID)
        }
        req.URL.RawQuery = q.Encode()

        if it.guest {
                // Guest preflight cookies (NID/AEC consent) ride along when present.
                if cc := guestConsentCookie(); cc != "" {
                        req.Header.Set("Cookie", cc)
                }
        } else {
                req.Header.Set("Cookie", accountCookieHeader(acc))
        }
        return req, cancel, nil
}

// guestConsentCookie returns cached consent cookies from the guest preflight
// (google.com NID/AEC), improving guest acceptance on flagged IPs.
func guestConsentCookie() string {
        consentMu.Lock()
        defer consentMu.Unlock()
        return consentCookieHeader
}

var (
        consentMu           sync.Mutex
        consentCookieHeader string
)

// preflightGuestConsent fetches google.com once to collect consent cookies.
func preflightGuestConsent() {
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "GET", URL_GOOGLE, nil)
        if err != nil {
                return
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return
        }
        defer resp.Body.Close()
        io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

        var b strings.Builder
        for _, c := range resp.Cookies() {
                if c.Name == "" || c.Value == "" {
                        continue
                }
                if b.Len() > 0 {
                        b.WriteString("; ")
                }
                b.WriteString(c.Name + "=" + c.Value)
        }
        consentMu.Lock()
        consentCookieHeader = b.String()
        consentMu.Unlock()
}

// ============================================================================
// SEND UPSTREAM — stream StreamGenerate into UpstreamResult deltas
// ============================================================================

// upstreamErr carries an HTTP-ish status for handlers/pool reporting.
type upstreamErr struct {
        status int
        msg    string
}

func (e *upstreamErr) Error() string { return e.msg }

// sendUpstream runs one generate call and streams text/thought deltas.
func sendUpstream(prompt string, opts SendOptions) (<-chan UpstreamResult, error) {
        out := make(chan UpstreamResult, 64)

        model := opts.Model
        mh := resolveGeminiModel(model)

        acc := opts.Account
        it, err := getInitTokens(acc)
        if err != nil {
                return nil, err
        }

        thinking := config.Thinking
        if opts.Thinking != nil {
                thinking = *opts.Thinking
        }

        req, cancel, err := buildGenerateRequest(prompt, opts.FileData, thinking, it, mh, acc)
        if err != nil {
                return nil, &upstreamErr{502, "ساخت درخواست generate ناموفق بود: " + err.Error()}
        }

        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                cancel()
                return nil, &upstreamErr{502, "خطای شبکه در generate: " + err.Error()}
        }

        if dumpPath := os.Getenv("GEMINI_DEBUG_DUMP"); dumpPath != "" {
                dbgBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
                _ = os.WriteFile(dumpPath, dbgBody, 0o600)
                // Rewind: serve the dumped bytes to the stream reader below.
                resp.Body = io.NopCloser(bytes.NewReader(dbgBody))
                logAlways("[Debug] raw StreamGenerate body dumped to " + dumpPath)
        }

        if resp.StatusCode != 200 {
                body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
                resp.Body.Close()
                cancel()
                return nil, classifyGeminiFail(resp.StatusCode, string(body))
        }

        go func() {
                defer close(out)
                defer resp.Body.Close()
                defer cancel()

                parser := newFrameParser()
                lastText := map[string]string{}   // rcid -> text sent so far
                lastThought := map[string]string{} // rcid -> thoughts sent so far
                sawCandidate := false

                reader := bufio.NewReaderSize(resp.Body, 64<<10)
                buf := make([]byte, 32<<10)
                eofSeen := false
                for {
                        n, rerr := reader.Read(buf)
                        if n > 0 {
                                frames, perr := parser.feed(buf[:n])
                                if perr != nil {
                                        select {
                                        case out <- UpstreamResult{Err: &upstreamErr{502, "پارس پاسخ جمنای ناموفق بود: " + perr.Error()}}:
                                        default:
                                        }
                                        return
                                }
                                for _, fr := range frames {
                                        errOut, done := processFrame(fr, lastText, lastThought, &sawCandidate, out)
                                        if errOut != nil {
                                                select {
                                                case out <- UpstreamResult{Err: errOut}:
                                                default:
                                                }
                                                return
                                        }
                                        if done {
                                                return
                                        }
                                }
                        }
                        if rerr != nil {
                                // EOF: flush any pending final frame, then decide.
                                if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
                                        if !eofSeen {
                                                eofSeen = true
                                                tail, perr := parser.flush()
                                                if perr == nil {
                                                        for _, fr := range tail {
                                                                errOut, done := processFrame(fr, lastText, lastThought, &sawCandidate, out)
                                                                if errOut != nil {
                                                                        select {
                                                                        case out <- UpstreamResult{Err: errOut}:
                                                                        default:
                                                                        }
                                                                        return
                                                                }
                                                                if done {
                                                                        return
                                                                }
                                                        }
                                                }
                                        }
                                        if !sawCandidate {
                                                select {
                                                case out <- UpstreamResult{Err: &upstreamErr{502, "جمنای پاسخی برنگرداند — لطفاً دوباره تلاش کنید"}}:
                                                default:
                                                }
                                        }
                                        return
                                }
                                select {
                                case out <- UpstreamResult{Err: &upstreamErr{502, "خطای استریم جمنای: " + rerr.Error()}}:
                                default:
                                }
                                return
                        }
                }
        }()

        return out, nil
}

// processFrame extracts deltas from one parsed frame. Returns (err, done).
func processFrame(fr json.RawMessage, lastText, lastThought map[string]string, sawCandidate *bool, out chan<- UpstreamResult) (error, bool) {
        var envelope []interface{}
        if err := json.Unmarshal(fr, &envelope); err != nil {
                return nil, false
        }

        for _, entry := range envelope {
                part, ok := entry.([]interface{})
                if !ok || len(part) < 3 {
                        continue
                }
                kind, _ := part[0].(string)
                if kind != "wrb.fr" {
                        continue // [["di",...]], [["e",...]] terminators — ignore
                }
                innerStr, ok := part[2].(string)
                if !ok {
                        continue
                }
                var inner []interface{}
                if json.Unmarshal([]byte(innerStr), &inner) != nil {
                        continue
                }

                // Fatal API error codes: part[5][2][0][1][0]
                if code, found := nestedInt(part, 5, 2, 0, 1, 0); found && code != 0 {
                        return classifyGeminiAPICode(code), true
                }

                // Conversation ids: inner[1][0] (cid), inner[1][1] (rid) — logged only.
                if len(inner) > 4 {
                        cands, ok := inner[4].([]interface{})
                        if !ok {
                                continue
                        }
                        for _, c := range cands {
                                cand, ok := c.([]interface{})
                                if !ok {
                                        continue
                                }
                                rcid := ""
                                if s, ok := cand[0].(string); ok {
                                        rcid = s
                                }
                                *sawCandidate = true

                                // Thoughts: candidate[37][0][0]
                                if thought, found := nestedString(cand, 37, 0, 0); found && thought != "" {
                                        if delta := snapshotDelta(lastThought, rcid, thought); delta != "" {
                                                out <- UpstreamResult{Reasoning: delta}
                                        }
                                }

                                // Text: candidate[1][0]
                                if text, found := nestedString(cand, 1, 0); found && text != "" {
                                        text = cleanGeminiText(text)
                                        if delta := snapshotDelta(lastText, rcid, text); delta != "" {
                                                out <- UpstreamResult{Chunk: delta}
                                        }
                                }
                        }
                }
        }
        return nil, false
}

// snapshotDelta diffs a cumulative snapshot against the last-sent text and
// returns the new suffix (rune-safe). Empty when nothing new.
func snapshotDelta(store map[string]string, key, snap string) string {
        last := store[key]
        if snap == last {
                return ""
        }
        if len(snap) < len(last) || (last != "" && !strings.HasPrefix(snap, last)) {
                // Rewrite: find the longest common rune prefix and resend the tail.
                common := commonPrefixLen(last, snap)
                store[key] = snap
                return runeSlice(snap, common)
        }
        store[key] = snap
        return snap[len(last):]
}

func commonPrefixLen(a, b string) int {
        ra, rb := []rune(a), []rune(b)
        n := len(ra)
        if len(rb) < n {
                n = len(rb)
        }
        for i := 0; i < n; i++ {
                if ra[i] != rb[i] {
                        return i
                }
        }
        return n
}

func runeSlice(s string, from int) string {
        if from <= 0 {
                return s
        }
        r := []rune(s)
        if from >= len(r) {
                return ""
        }
        return string(r[from:])
}

// cleanGeminiText strips web-UI artifacts from reply text.
func cleanGeminiText(s string) string {
        s = strings.TrimSuffix(s, "\n```")
        if i := strings.Index(s, "<FollowUp"); i >= 0 {
                s = s[:i]
        }
        return s
}

// nestedString navigates (list-only) nested paths safely.
func nestedString(v interface{}, path ...int) (string, bool) {
        cur := v
        for _, idx := range path {
                arr, ok := cur.([]interface{})
                if !ok || idx < 0 || idx >= len(arr) {
                        return "", false
                }
                cur = arr[idx]
        }
        s, ok := cur.(string)
        return s, ok
}

func nestedInt(v interface{}, path ...int) (int, bool) {
        cur := v
        for _, idx := range path {
                arr, ok := cur.([]interface{})
                if !ok || idx < 0 || idx >= len(arr) {
                        return 0, false
                }
                cur = arr[idx]
        }
        if f, ok := cur.(float64); ok {
                return int(f), true
        }
        return 0, false
}

// ============================================================================
// ERROR CLASSIFICATION — humanized Persian for every known failure mode
// ============================================================================

func classifyGeminiFail(status int, body string) error {
        switch status {
        case 401, 403:
                return &upstreamErr{401, "کوکی‌های اکانت گوگل رد شدند. وارد gemini.google.com شوید و کوکی‌های __Secure-1PSID / __Secure-1PSIDTS را تازه کنید. (" + strconv.Itoa(status) + ")"}
        case 429:
                return &upstreamErr{429, "سقف درخواست گوگل برای این IP/اکانت پر شده است (HTTP 429). چند دقیقه دیگر تلاش کنید."}
        }
        if status >= 500 {
                return &upstreamErr{502, fmt.Sprintf("سرور جمنای خطای %d داد — بعداً تلاش کنید", status)}
        }
         snippet := body
         if len(snippet) > 160 {
                snippet = snippet[:160]
         }
        return &upstreamErr{502, fmt.Sprintf("خطای جمنای (HTTP %d): %s", status, snippet)}
}

// classifyGeminiAPICode maps in-band error codes observed by the Gemini web
// client (gemini_webapi constants) to humanized bridge errors.
func classifyGeminiAPICode(code int) error {
        switch code {
        case 1013:
                return &upstreamErr{502, "جمنای خطای موقت ۱۰۱۳ داد — چند لحظه دیگر دوباره تلاش کنید."}
        case 1037:
                return &upstreamErr{429, "سقف استفاده این مدل پر شده است (کد ۱۰۳۷). مدل دیگری انتخاب کنید یا صبر کنید تا سهمیه ریست شود."}
        case 1050:
                return &upstreamErr{400, "مدل انتخابی با تاریخچه مکالمه ناسازگار است (کد ۱۰۵۰). مدل را در طول مکالمه ثابت نگه دارید."}
        case 1052:
                return &upstreamErr{400, "این مدل در حال حاضر در دسترس نیست یا ساختار درخواست قدیمی شده (کد ۱۰۵۲). مدل دیگری امتحان کنید."}
        case 1060:
                return &upstreamErr{403, "IP شما موقتاً توسط گوگل فلگ شده (کد ۱۰۶۰). با پروکسی یا شبکه دیگر تلاش کنید."}
        }
        return &upstreamErr{502, fmt.Sprintf("خطای ناشناخته جمنای (کد %d) — احتمالاً موقتی است؛ دوباره تلاش کنید.", code)}
}

// statusFromError extracts an HTTP-ish status from an error message so
// handlers can shape HTTP responses from string errors.
func statusFromError(errMsg string) int {
        switch {
        case strings.Contains(errMsg, "HTTP 429"), strings.Contains(errMsg, "(۴۲۹)"),
                strings.Contains(errMsg, "سقف درخواست"), strings.Contains(errMsg, "سقف استفاده"),
                strings.Contains(errMsg, "rate limited"):
                return 429
        case strings.Contains(errMsg, "HTTP 401"), strings.Contains(errMsg, "HTTP 403"),
                strings.Contains(errMsg, "کوکی‌های اکانت گوگل رد شدند"),
                strings.Contains(errMsg, "کوکی‌های اکانت Google"),
                strings.Contains(errMsg, "unauthorized"):
                return 401
        case strings.Contains(errMsg, "IP شما موقتاً"), strings.Contains(errMsg, "کد ۱۰۶۰"):
                return 403
        case strings.Contains(errMsg, "مدل انتخابی"), strings.Contains(errMsg, "کد ۱۰۵۰"),
                strings.Contains(errMsg, "کد ۱۰۵۲"), strings.Contains(errMsg, "مدل ناشناخته"):
                return 400
        }
        return 502
}

// randIntn returns a cryptographically-seeded random int in [0,n).
func randIntn(n int) int {
        if n <= 0 {
                return 0
        }
        idx, err := crand.Int(crand.Reader, big.NewInt(int64(n)))
        if err != nil {
                return 0
        }
        return int(idx.Int64())
}

// sendUpstreamWithFailover tries accounts round-robin until one produces a
// stream. Failover only happens BEFORE the first byte reaches the client.
func sendUpstreamWithFailover(prompt string, opts SendOptions) (<-chan UpstreamResult, error) {
        acc := opts.Account
        maxTries := 1
        if accounts != nil {
                maxTries = accounts.Len()
                if maxTries > 4 {
                        maxTries = 4
                }
        }
        var lastErr error
        for try := 0; try < maxTries; try++ {
                if try > 0 {
                        if accounts == nil {
                                break
                        }
                        next := accounts.pickOther(acc)
                        if next == nil {
                                break
                        }
                        acc = next
                        opts.Account = acc
                        logInfo(fmt.Sprintf("[Failover] retrying with %s", acc.Label()))
                }
                ch, err := sendUpstream(prompt, opts)
                if err == nil {
                        if acc != nil {
                                acc.ReportOK()
                        }
                        return ch, nil
                }
                lastErr = err
                if accounts != nil {
                        accounts.Report(acc, err)
                }
                // Non-retryable statuses: do not burn another account.
                if ue, ok := err.(*upstreamErr); ok && (ue.status == 400 || ue.status == 404) {
                        break
                }
        }
        if lastErr == nil {
                lastErr = &upstreamErr{502, "no upstream attempt was made"}
        }
        return nil, lastErr
}

// ============================================================================
// HTTP CLIENTS — no shared cookie jar (per-account headers are explicit)
// ============================================================================

var geminiHTTPClient = &http.Client{
        Transport: &http.Transport{
                MaxIdleConns:          100,
                MaxIdleConnsPerHost:   20,
                MaxConnsPerHost:       20,
                IdleConnTimeout:       90 * time.Second,
                TLSHandshakeTimeout:   15 * time.Second,
                ExpectContinueTimeout: 1 * time.Second,
                ForceAttemptHTTP2:     true,
        },
        Timeout: 0, // streams outlive any fixed client timeout; per-request ctx governs
}

// initializeSession is the startup warm-up: preflight consent cookies for
// guest mode and warm the model registry. Never fatal — the bridge serves
// requests regardless (guest needs no state).
func initializeSession() error {
        if accounts == nil {
                preflightGuestConsent()
        }
        fetchModelsFromGemini()
        session.mu.Lock()
        session.Initialized = true
        session.mu.Unlock()
        return nil
}

// accountCookieHeader builds the explicit Cookie header for an account.
func accountCookieHeader(acc *Account) string {
        if acc == nil || acc.PSID == "" {
                return ""
        }
        h := "__Secure-1PSID=" + acc.PSID
        if acc.PSIDTS != "" {
                h += "; __Secure-1PSIDTS=" + acc.PSIDTS
        }
        return h
}

// rotateAccountCookies calls Google's RotateCookies endpoint to refresh
// __Secure-1PSIDTS for an account (called on auth failures before giving up).
func rotateAccountCookies(acc *Account) bool {
        if acc == nil || acc.PSID == "" {
                return false
        }
        ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "POST", URL_ROTATE,
                bytes.NewReader([]byte(`[000,"-0000000000000000000"]`)))
        if err != nil {
                return false
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Origin", "https://accounts.google.com")
        req.Header.Set("Cookie", accountCookieHeader(acc))

        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return false
        }
        defer resp.Body.Close()
        io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
        if resp.StatusCode != 200 {
                return false
        }
        if fresh := extractSetCookie(resp.Header.Get("Set-Cookie"), "__Secure-1PSIDTS"); fresh != "" {
                if fresh != acc.PSIDTS {
                        acc.updatePSIDTS(fresh)
                        logAlways(fmt.Sprintf("[Accounts] %s PSIDTS rotated via RotateCookies", acc.Label()))
                }
                return true
        }
        return false
}
