// Multi-account cookie pool for the Gemini bridge (Godde3s edition).
//
// Turns a list of Google cookie pairs (GEMINI_COOKIES) into a round-robin
// pool with GhostBrain-style resilience:
//
//   - 429 rate limit  -> the account enters an exponential cooldown and the
//     in-flight request transparently fails over to the next healthy account
//     BEFORE any byte of the response has been streamed to the client.
//   - 401/403         -> one RotateCookies refresh attempt, then the account
//     is marked dead (expired cookies) if rotation cannot save it.
//   - all accounts cooling down -> new requests wait in a bounded queue
//     (ACCOUNT_QUEUE_TIMEOUT seconds, default 120) instead of erroring.
//
// An EMPTY pool (no GEMINI_COOKIES) disables the pool entirely and the bridge
// falls back to the guest flow (verified live: StreamGenerate accepts
// unauthenticated calls with an empty at token).

package gbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// ACCOUNT
// ============================================================================

// Account is one Google identity: the __Secure-1PSID cookie pair plus live
// health state.
type Account struct {
	ID     int
	PSID   string
	PSIDTS string
	Label_ string

	mu            sync.Mutex
	cooldownUntil time.Time
	rateLimited   int   // consecutive 429s (reset on success)
	dead          bool  // auth failure that rotation could not fix
	lastError     string
	lastUsed      time.Time

	requests atomic.Int64
	errors   atomic.Int64
	rateHits atomic.Int64
}

func newAccount(id int, psid, psidts string) *Account {
	a := &Account{ID: id, PSID: strings.TrimSpace(psid), PSIDTS: strings.TrimSpace(psidts)}
	if a.PSIDTS == "" {
		a.PSIDTS = defaultPSIDTSIfAny(a.PSID)
	}
	if a.Label_ == "" {
		a.Label_ = fmt.Sprintf("account-%d", id)
	}
	return a
}

// defaultPSIDTSIfAny lets callers pass "psid|psidts" packed values.
func defaultPSIDTSIfAny(psid string) string {
	if i := strings.Index(psid, "|"); i > 0 {
		return strings.TrimSpace(psid[i+1:])
	}
	return ""
}

// Label returns a short human-readable identifier for logs.
func (a *Account) Label() string {
	name := a.Label_
	if len(name) > 18 {
		name = name[:18]
	}
	return fmt.Sprintf("#%d(%s)", a.ID, name)
}

// Available reports whether the account can serve a request right now.
func (a *Account) Available() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.dead && time.Now().After(a.cooldownUntil)
}

// CooldownLeft returns how long until the account leaves cooldown (0 if now).
func (a *Account) CooldownLeft() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := time.Until(a.cooldownUntil)
	if d < 0 {
		d = 0
	}
	return d
}

// ReportOK marks a successful request: cooldown and 429 streak are reset.
func (a *Account) ReportOK() {
	a.requests.Add(1)
	a.mu.Lock()
	a.rateLimited = 0
	a.lastError = ""
	a.lastUsed = time.Now()
	a.mu.Unlock()
}

// Report429 puts the account into an exponential cooldown:
// base * 2^(streak-1), capped at 30 minutes.
func (a *Account) Report429() {
	a.errors.Add(1)
	a.rateHits.Add(1)
	a.mu.Lock()
	a.rateLimited++
	streak := a.rateLimited
	base := time.Duration(config.AccountCooldownBase) * time.Second
	if base <= 0 {
		base = 60 * time.Second
	}
	d := base
	for i := 1; i < streak && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	a.cooldownUntil = time.Now().Add(d)
	a.lastError = fmt.Sprintf("rate limited, cooling down %s", d.Truncate(time.Second))
	a.lastUsed = time.Now()
	a.mu.Unlock()
	logAlways(fmt.Sprintf("[Accounts] %s hit 429 — cooldown %s (streak %d)",
		a.Label(), d.Truncate(time.Second), streak))
}

// ReportAuthFail marks the account dead (expired cookies).
func (a *Account) ReportAuthFail(reason string) {
	a.errors.Add(1)
	a.mu.Lock()
	if !a.dead {
		a.dead = true
		a.lastError = "auth failed: " + reason
		a.lastUsed = time.Now()
	}
	a.mu.Unlock()
	logAlways(fmt.Sprintf("[Accounts] %s marked DEAD (auth failure): %s", a.Label(), reason))
}

// ReportError records a generic (non-fatal) upstream failure.
func (a *Account) ReportError(err string) {
	a.errors.Add(1)
	a.mu.Lock()
	a.lastError = err
	a.lastUsed = time.Now()
	a.mu.Unlock()
}

// updatePSIDTS stores a Google-issued refresh and persists the pool cache.
func (a *Account) updatePSIDTS(psidts string) {
	a.mu.Lock()
	changed := psidts != "" && psidts != a.PSIDTS
	if changed {
		a.PSIDTS = psidts
	}
	a.mu.Unlock()
	if changed {
		saveCookieCache()
		logAlways(fmt.Sprintf("[Accounts] %s __Secure-1PSIDTS refreshed automatically", a.Label()))
	}
}

// Snapshot is the JSON-friendly status view of one account.
type AccountSnapshot struct {
	ID        int    `json:"id"`
	UserName  string `json:"userName"`
	UserID    string `json:"userId"`
	Healthy   bool   `json:"healthy"`
	Dead      bool   `json:"dead"`
	Cooldown  string `json:"cooldown"`
	Requests  int64  `json:"requests"`
	Errors    int64  `json:"errors"`
	RateHits  int64  `json:"rateLimited"`
	LastError string `json:"lastError,omitempty"`
}

func (a *Account) Snapshot() AccountSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	label := a.Label_
	snap := AccountSnapshot{
		ID:        a.ID,
		UserName:  label,
		Healthy:   !a.dead && time.Now().After(a.cooldownUntil),
		Dead:      a.dead,
		Requests:  a.requests.Load(),
		Errors:    a.errors.Load(),
		RateHits:  a.rateHits.Load(),
		LastError: a.lastError,
	}
	if d := time.Until(a.cooldownUntil); d > 0 {
		snap.Cooldown = d.Truncate(time.Second).String()
	}
	return snap
}

// ============================================================================
// POOL
// ============================================================================

// AccountPool hands out accounts round-robin, skipping dead / cooling ones.
type AccountPool struct {
	mu       sync.Mutex
	accounts []*Account
	cursor   int
}

// accounts is the process-wide pool; nil means "no pool" (guest flow).
var accounts *AccountPool

// cookiePair is one GEMINI_COOKIES entry.
type cookiePair struct {
	PSID   string `json:"psid"`
	PSIDTS string `json:"psidts"`
	Label  string `json:"label"`
}

// ParseCookiesEnv reads GEMINI_COOKIES in three accepted shapes:
//
//  1. JSON list:   [{"psid":"...","psidts":"..."}]  (label optional)
//  2. Cookie header: "__Secure-1PSID=x; __Secure-1PSIDTS=y" (pairs separated
//     by newlines/commas for multi-account)
//  3. Bare PSID value
//
// Multiple accounts in header form: separate entries with ", " — but PSID
// values never contain commas, so splitting on ',' then parsing each chunk
// as a cookie header is safe. JSON may also pack as "psid|psidts".
func ParseCookiesEnv(raw string) []cookiePair {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Shape 1: JSON
	if strings.HasPrefix(raw, "[") {
		var list []cookiePair
		if json.Unmarshal([]byte(raw), &list) == nil {
			if cleaned := cleanPairs(list); len(cleaned) > 0 {
				return cleaned
			}
		}
		// JSON of shape [{"__Secure-1PSID": "..."}] (browser export style)
		var generic []map[string]string
		if json.Unmarshal([]byte(raw), &generic) == nil && len(generic) > 0 {
			var out []cookiePair
			for _, m := range generic {
				p := cookiePair{}
				for k, v := range m {
					lk := strings.ToLower(k)
					switch {
					case strings.Contains(lk, "1psidts"):
						p.PSIDTS = v
					case strings.Contains(lk, "1psid"):
						p.PSID = v
					case lk == "label":
						p.Label = v
					}
				}
				out = append(out, p)
			}
			return cleanPairs(out)
		}
		return nil
	}
	if strings.HasPrefix(raw, "{") {
		var one cookiePair
		if json.Unmarshal([]byte(raw), &one) == nil && one.PSID != "" {
			return cleanPairs([]cookiePair{one})
		}
		return nil
	}

	// Shape 2/3: cookie header(s) or bare values, comma/newline separated.
	var out []cookiePair
	for _, chunk := range splitPairString(raw) {
		var psid, psidts string
		for _, seg := range strings.Split(chunk, ";") {
			seg = strings.TrimSpace(seg)
			if eq := strings.Index(seg, "="); eq > 0 {
				name := strings.TrimSpace(strings.ToLower(seg[:eq]))
				val := strings.TrimSpace(seg[eq+1:])
				switch {
				case strings.Contains(name, "1psidts"):
					psidts = val
				case strings.Contains(name, "1psid"):
					psid = val
				}
			}
		}
		if psid == "" {
			// bare value form
			cand := strings.TrimSpace(chunk)
			if cand == "" || strings.HasPrefix(strings.ToLower(cand), "__secure-") {
				continue
			}
			psid = cand
		}
		out = append(out, cookiePair{PSID: psid, PSIDTS: psidts})
	}
	return out
}

func splitPairString(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	var out []string
	for _, f := range fields {
		if strings.TrimSpace(f) != "" {
			out = append(out, strings.TrimSpace(f))
		}
	}
	if len(out) == 0 {
		out = []string{raw}
	}
	return out
}

func cleanPairs(list []cookiePair) []cookiePair {
	seen := map[string]bool{}
	var out []cookiePair
	for _, p := range list {
		p.PSID = strings.TrimSpace(p.PSID)
		p.PSIDTS = strings.TrimSpace(p.PSIDTS)
		if p.PSID == "" || seen[p.PSID] {
			continue
		}
		seen[p.PSID] = true
		out = append(out, p)
	}
	return out
}

// NewAccountPool builds a pool from cookie pairs. Returns nil for empty.
func NewAccountPool(pairs []cookiePair) *AccountPool {
	if len(pairs) == 0 {
		return nil
	}
	p := &AccountPool{}
	for i, c := range pairs {
		a := newAccount(i+1, c.PSID, c.PSIDTS)
		if c.Label != "" {
			a.Label_ = c.Label
		}
		p.accounts = append(p.accounts, a)
	}
	return p
}

// Len reports the number of accounts in the pool.
func (p *AccountPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// loadCookieFile reads GEMINI_COOKIES_FILE (or ./gemini-cookies.json).
func loadCookieFile() []cookiePair {
	path := config.GeminiCookiesFle
	if path == "" {
		path = "gemini-cookies.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseCookiesEnv(string(raw))
}

// initAccounts wires the process-wide pool from configuration. Priority:
// GEMINI_COOKIES_FILE > GEMINI_COOKIES > guest flow. After loading, the
// persisted PSIDTS cache (gemini-cookies-cache.json) is layered on top so
// rotated cookies survive restarts.
func initAccounts() {
	pairs := loadCookieFile()
	if len(pairs) == 0 {
		pairs = ParseCookiesEnv(config.GeminiCookies)
	}
	accounts = NewAccountPool(pairs)

	if accounts == nil {
		logInfo("[Accounts] no GEMINI_COOKIES configured — guest mode (no cookies needed)")
		return
	}
	loadPSIDTSCache(accounts)
	healthy := 0
	for _, a := range accounts.accounts {
		if a.PSID != "" {
			healthy++
		}
		logInfo(fmt.Sprintf("[Accounts] %s loaded (psid=%s...)", a.Label(), shortID(a.PSID)))
	}
	logInfo(fmt.Sprintf("[Accounts] pool ready: %d account(s), round-robin + 429 failover active", healthy))
}

// ============================================================================
// PSIDTS PERSISTENCE — rotated cookies survive restarts
// ============================================================================

type psidtsCacheFile struct {
	Accounts []psidtsEntry `json:"accounts"`
}

type psidtsEntry struct {
	PSID   string `json:"psid"`
	PSIDTS string `json:"psidts"`
}

func cacheFilePath() string {
	if p := os.Getenv("GEMINI_CACHE_FILE"); p != "" {
		return p
	}
	return "gemini-cookies-cache.json"
}

func loadPSIDTSCache(p *AccountPool) {
	raw, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return
	}
	var cf psidtsCacheFile
	if json.Unmarshal(raw, &cf) != nil {
		return
	}
	latest := map[string]string{}
	for _, e := range cf.Accounts {
		latest[e.PSID] = e.PSIDTS
	}
	p.mu.Lock()
	for _, a := range p.accounts {
		if fresh, ok := latest[a.PSID]; ok && fresh != "" && fresh != a.PSIDTS {
			a.PSIDTS = fresh
			logInfo(fmt.Sprintf("[Accounts] %s restored rotated PSIDTS from cache", a.Label()))
		}
	}
	p.mu.Unlock()
}

// saveCookieCache writes the current pool cookies to disk (atomic rename).
func saveCookieCache() {
	if accounts == nil {
		return
	}
	accounts.mu.Lock()
	cf := psidtsCacheFile{}
	for _, a := range accounts.accounts {
		cf.Accounts = append(cf.Accounts, psidtsEntry{PSID: a.PSID, PSIDTS: a.PSIDTS})
	}
	accounts.mu.Unlock()

	raw, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return
	}
	path := cacheFilePath()
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ============================================================================
// POOL OPERATIONS (same semantics as the Qwen edition)
// ============================================================================

// Pick blocks until an available account shows up, the request context is
// done, or the queue timeout expires. No-op (nil, nil) when pool disabled.
func (p *AccountPool) Pick(ctx context.Context) (*Account, error) {
	if p == nil {
		return nil, nil
	}
	timeout := time.Duration(config.AccountQueueTimeout) * time.Second
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		if acc := p.pickNow(); acc != nil {
			return acc, nil
		}
		if p.allDead() {
			return nil, errors.New("all accounts are dead (expired cookies) — refresh GEMINI_COOKIES")
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, errors.New("all accounts are rate-limited or dead; try again shortly or add more GEMINI_COOKIES")
		}
	}
}

// allDead reports whether every account is dead (auth failure).
func (p *AccountPool) allDead() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return false
	}
	for _, a := range p.accounts {
		if !a.dead {
			return false
		}
	}
	return true
}

// pickNow returns the next available account in round-robin order, or nil.
func (p *AccountPool) pickNow() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		a := p.accounts[idx]
		if a.Available() {
			p.cursor = (idx + 1) % n
			return a
		}
	}
	return nil
}

// pickOther returns any available account except `exclude` (failover path).
func (p *AccountPool) pickOther(exclude *Account) *Account {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accounts)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		a := p.accounts[idx]
		if a != exclude && a.Available() {
			p.cursor = (idx + 1) % n
			return a
		}
	}
	return nil
}

// Report categorizes an upstream error onto the account that served it.
// Auth failures get one RotateCookies rescue attempt before the account is
// marked dead — Google rotates PSIDTS aggressively and a single stale PSIDTS
// is usually recoverable.
func (p *AccountPool) Report(acc *Account, err error) {
	if acc == nil || err == nil {
		return
	}
	msg := err.Error()
	switch statusFromError(msg) {
	case 429:
		acc.Report429()
	case 401, 403:
		if rotateAccountCookies(acc) {
			acc.ReportError("auth recovered after PSIDTS rotation")
			return
		}
		invalidateInitTokens(acc)
		acc.ReportAuthFail(msg)
	default:
		acc.ReportError(msg)
	}
}

// StatusJSON returns the per-account snapshot list (for /status & dashboard).
func (p *AccountPool) StatusJSON() []AccountSnapshot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountSnapshot, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, a.Snapshot())
	}
	return out
}

// HealthyCount reports how many accounts can serve right now.
func (p *AccountPool) HealthyCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accounts {
		if a.Available() {
			n++
		}
	}
	return n
}

// acquireAccountForRequest is the handler-side entry point.
func acquireAccountForRequest(ctx context.Context) (*Account, error) {
	if accounts == nil {
		return nil, nil
	}
	return accounts.Pick(ctx)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
