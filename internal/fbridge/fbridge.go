// fbridge — Freebuff (freebuff.com) free-tier provider bridge.
//
// Freebuff is the free product built on the open Codebuff agent framework
// (github.com/CodebuffAI/freebuff). Its free tier serves a curated model set
// — GLM 5.3 Flash, DeepSeek V4.1 Flash, MiMo 2.6 Flash, Solar Mini 4 and
// friends, several of them unmetered — through an OpenAI-compatible chat
// endpoint guarded by three bookkeeping pieces:
//
//  1. a per-token "free session"   POST /api/v1/freebuff/session
//  2. a per-agent "agent run"      POST /api/v1/agent-runs (START/FINISH)
//  3. codebuff_metadata on every   POST /api/v1/chat/completions body
//
// This bridge implements that protocol and exposes a clean OpenAI surface
// to the router core (and to anything proxied at /freebuff/):
//
//      GET  /v1/models            → live free-agent catalog (auto-refreshed)
//      POST /v1/chat/completions  → session+run lifecycle, metadata injection
//      POST /v1/messages          → Anthropic ⇄ OpenAI translation (anthropic.go)
//      GET  /health               → status incl. connected token count
//
// Credentials, in priority order (first source that yields ≥1 token wins):
//
//      FREEBUFF_TOKENS env      — comma/newline separated auth tokens
//      Dashboard (persisted)    — applied live via SetTokens (core admin API)
//      Freebuff CLI login file  — ~/.config/manicode/credentials.json → authToken
//
// With zero tokens the bridge still boots: /v1/models answers from the
// static catalog and chat returns guided 401s, so "auto" routing simply
// skips Freebuff until a token is connected — same philosophy as the other
// bridges.

package fbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultBase = "https://codebuff.com"
	// The UA the official TS SDK sends; required shape, not a cosmetic.
	upstreamUA = "ai-sdk/openai-compatible/1.0.25/codebuff"

	sessionReuseMargin = 5 * time.Second  // refresh sessions shortly before expiry
	runRotation        = 6 * time.Hour    // rotate agent-run ids like the reference impl
	modelRefreshEvery  = 6 * time.Hour    // free-agent catalog refresh
	cooldownOnAuthFail = 30 * time.Minute // bench a token the backend rejects
	maxChainAttempts   = 4                // session/run invalidation retries
)

// freeAgentsSource is the upstream file the live catalog is parsed from.
const freeAgentsSource = "https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/free-agents.ts"

// fallbackAgentModels is the verified free-tier catalog (model → root agent).
// Unmetered models come first — they cost no session and are the defaults.
var fallbackAgentModels = []struct{ Model, Agent string }{
	{"z-ai/glm-5.3-flash", "base2-free-glm-5-3-flash"},
	{"deepseek/deepseek-v4.1-flash", "base2-free-deepseek-v4-1-flash"},
	{"mimo/mimo-v2.5", "base2-free-mimo"}, // MiMo 2.6 Flash serves under the v2.5 wire id
	{"upstage/solar-mini4", "base2-free-solar-mini4"},
	{"z-ai/glm-5.2", "base2-free-glm"},
	{"z-ai/glm-5.3", "base2-free-glm-5-3"},
	{"deepseek/deepseek-v4-pro", "base2-free-deepseek"},
	{"deepseek/deepseek-v4-flash", "base2-free-deepseek-flash"},
	{"deepseek/deepseek-v4.1-pro", "base2-free-deepseek-v4-1-pro"},
	{"openai/gpt-5.6-luna", "base2-free-luna"},
	{"openai/gpt-6-luna", "base2-free-luna-6"},
	{"openai/gpt-5.6-luna-es", "base2-free-luna-es"},
	{"crof/kimi-k3-eco", "base2-free-kimi-k3-eco"},
	{"stealth/space-bunny-alpha", "base2-free-space-bunny-alpha"},
}

type fbconfig struct {
	BaseURL string
	AuthTok string // shared internal secret (AUTH_TOKEN), like every bridge
}

var cfg fbconfig

var httpClient = &http.Client{Timeout: 0} // streams run long; per-request ctx bounds everything else

// ---------- token pool ----------

type fbSession struct {
	status     string // active | queued | disabled | ""
	instanceID string
	expiresAt  time.Time
}

type managedRun struct {
	id        string
	agentID   string
	startedAt time.Time
}

type tokenPool struct {
	name  string
	token string

	mu       sync.Mutex
	session  fbSession
	runs     map[string]*managedRun // agentID → live run
	lastErr  string
	coolTill time.Time
}

type poolSet struct {
	mu    sync.Mutex
	pools []*tokenPool
	next  uint64
}

var pools = &poolSet{}

func (ps *poolSet) snapshot() []*tokenPool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]*tokenPool, len(ps.pools))
	copy(out, ps.pools)
	return out
}

// SetTokens replaces the pool. Env-configured tokens always win when set;
// otherwise the dashboard/store list is used; an empty list keeps the CLI
// file as the last resort (resolved lazily per request).
func SetTokens(tokens []string) {
	ps := pools
	ps.mu.Lock()
	defer ps.mu.Unlock()
	clean := make([]string, 0, len(tokens))
	seen := map[string]bool{}
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	next := make([]*tokenPool, 0, len(clean))
	for i, t := range clean {
		next = append(next, &tokenPool{
			name:  fmt.Sprintf("token-%d", i+1),
			token: t,
			runs:  map[string]*managedRun{},
		})
	}
	ps.pools = next
	if len(clean) > 0 {
		log.Printf("[Freebuff] %d token(s) connected — %d model(s) exposed", len(clean), len(fallbackAgentModels))
	} else {
		log.Printf("[Freebuff] no tokens connected — set FREEBUFF_TOKENS, use the dashboard Connect panel, or run `npm i -g freebuff && freebuff` once")
	}
}

// cliToken scans the standard Freebuff CLI credential files and returns the
// first authToken found ("" when none).
func cliToken() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".config", "manicode", "credentials.json"),
		filepath.Join(home, ".config", "codebuff", "credentials.json"),
	}
	for _, path := range candidates {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var profiles map[string]map[string]interface{}
		if json.Unmarshal(raw, &profiles) == nil {
			for _, prof := range profiles {
				if tok, ok := prof["authToken"].(string); ok && tok != "" {
					return tok
				}
			}
		}
		// Some builds write a flat object instead of profiles.
		var flat map[string]interface{}
		if json.Unmarshal(raw, &flat) == nil {
			if tok, ok := flat["authToken"].(string); ok && tok != "" {
				return tok
			}
		}
	}
	return ""
}

// CLIFileStatus reports the CLI credential path + whether a usable token was
// found (dashboard "Auto-detected CLI credentials" chip).
func CLIFileStatus() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	path := filepath.Join(home, ".config", "manicode", "credentials.json")
	if tok := cliToken(); tok != "" {
		return path, true
	}
	if _, err := os.Stat(path); err == nil {
		return path, false
	}
	return path, false
}

// pick round-robins across healthy (non-cooling) pools.
func (ps *poolSet) pick() *tokenPool {
	all := ps.snapshot()
	if len(all) == 0 {
		return nil
	}
	now := time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i := 0; i < len(all); i++ {
		p := all[int((ps.next+uint64(i))%uint64(len(all)))]
		p.mu.Lock()
		ok := now.After(p.coolTill)
		p.mu.Unlock()
		if ok {
			ps.next += uint64(i) + 1
			return p
		}
	}
	return nil
}

// ---------- upstream calls ----------

func doJSON(ctx context.Context, method, path, token string, body []byte, extra map[string]string) (int, []byte, error) {
	url := strings.TrimRight(cfg.BaseURL, "/") + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", upstreamUA)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// ensureSession returns a live free-session instance id ("" when the session
// feature reports disabled — chat still works without one).
func (p *tokenPool) ensureSession(ctx context.Context) (string, error) {
	p.mu.Lock()
	s := p.session
	if s.status == "active" && s.instanceID != "" &&
		(s.expiresAt.IsZero() || time.Now().Before(s.expiresAt.Add(-sessionReuseMargin))) {
		p.mu.Unlock()
		return s.instanceID, nil
	}
	p.mu.Unlock()

	status, raw, err := doJSON(ctx, "POST", "/api/v1/freebuff/session", p.token, []byte("{}"), nil)
	if err != nil {
		return "", fmt.Errorf("free session: %w", err)
	}
	if status == http.StatusNotFound {
		// Session gating not enabled for this account/backend — proceed.
		p.mu.Lock()
		p.session = fbSession{status: "disabled"}
		p.mu.Unlock()
		return "", nil
	}
	if status == http.StatusUnauthorized {
		return "", errAuthRejected
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("free session HTTP %d: %s", status, clip(raw))
	}
	var parsed struct {
		Status     string `json:"status"`
		InstanceID string `json:"instanceId"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("free session decode: %w", err)
	}
	switch st := strings.TrimSpace(parsed.Status); st {
	case "disabled", "":
		p.mu.Lock()
		p.session = fbSession{status: "disabled"}
		p.mu.Unlock()
		return "", nil
	case "queued":
		p.mu.Lock()
		p.session = fbSession{status: "queued"}
		p.lastErr = "waiting room: queued"
		p.mu.Unlock()
		return "", &waitingRoomError{detail: clip(raw)}
	case "active":
		p.mu.Lock()
		p.session = fbSession{status: "active", instanceID: parsed.InstanceID, expiresAt: parseTime(parsed.ExpiresAt)}
		p.lastErr = ""
		p.mu.Unlock()
		return parsed.InstanceID, nil
	default:
		return "", fmt.Errorf("free session status %q", st)
	}
}

// getRun returns a live agent-run id for the model's root agent, starting
// one via the agent-runs API when needed.
func (p *tokenPool) getRun(ctx context.Context, agentID string) (string, error) {
	p.mu.Lock()
	if run := p.runs[agentID]; run != nil && time.Since(run.startedAt) < runRotation {
		id := run.id
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	status, raw, err := doJSON(ctx, "POST", "/api/v1/agent-runs", p.token,
		[]byte(fmt.Sprintf(`{"action":"START","agentId":%q}`, agentID)), nil)
	if err != nil {
		return "", fmt.Errorf("start run: %w", err)
	}
	if status == http.StatusUnauthorized {
		return "", errAuthRejected
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("start run HTTP %d: %s", status, clip(raw))
	}
	var parsed struct {
		RunID string `json:"runId"`
	}
	if json.Unmarshal(raw, &parsed) != nil || strings.TrimSpace(parsed.RunID) == "" {
		return "", fmt.Errorf("start run: missing runId (%s)", clip(raw))
	}
	p.mu.Lock()
	p.runs[agentID] = &managedRun{id: parsed.RunID, agentID: agentID, startedAt: time.Now()}
	p.mu.Unlock()
	return parsed.RunID, nil
}

// finishRun best-effort FINISH — bookkeeping only, never blocks the response.
func (p *tokenPool) finishRun(agentID, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := fmt.Sprintf(`{"action":"FINISH","runId":%q,"status":"completed","totalSteps":0,"directCredits":0,"totalCredits":0}`, runID)
	_, _, _ = doJSON(ctx, "POST", "/api/v1/agent-runs", p.token, []byte(body), nil)
	p.mu.Lock()
	if cur := p.runs[agentID]; cur != nil && cur.id == runID {
		delete(p.runs, agentID)
	}
	p.mu.Unlock()
}

func (p *tokenPool) invalidateSession(reason string) {
	p.mu.Lock()
	p.session = fbSession{}
	if reason != "" {
		p.lastErr = reason
	}
	p.mu.Unlock()
}

func (p *tokenPool) invalidateRun(agentID, reason string) {
	p.mu.Lock()
	delete(p.runs, agentID)
	if reason != "" {
		p.lastErr = reason
	}
	p.mu.Unlock()
}

func (p *tokenPool) cooldown(d time.Duration, reason string) {
	p.mu.Lock()
	p.coolTill = time.Now().Add(d)
	p.lastErr = reason
	p.mu.Unlock()
}

type waitingRoomError struct{ detail string }

func (e *waitingRoomError) Error() string {
	if e != nil && e.detail != "" {
		return "freebuff waiting room queued (" + e.detail + ")"
	}
	return "freebuff waiting room queued"
}

var errAuthRejected = errors.New("freebuff auth rejected token")

// ---------- model catalog ----------

var (
	catMu       sync.Mutex
	modelAgents [][2]string // model → agent pairs, unmetered first
	catAt       time.Time
	catFetching bool
)

func catalog() [][2]string {
	catMu.Lock()
	if len(modelAgents) > 0 && time.Since(catAt) < modelRefreshEvery {
		out := modelAgents
		catMu.Unlock()
		return out
	}
	if !catFetching {
		catFetching = true
		go refreshCatalog()
	}
	catMu.Unlock()

	if len(modelAgents) == 0 {
		fb := make([][2]string, 0, len(fallbackAgentModels))
		for _, e := range fallbackAgentModels {
			fb = append(fb, [2]string{e.Model, e.Agent})
		}
		return fb
	}
	return modelAgents
}

// ModelList returns the catalog's model ids in display order (unmetered first).
func ModelList() []string {
	cat := catalog()
	out := make([]string, 0, len(cat))
	seen := map[string]bool{}
	for _, e := range cat {
		if !seen[e[0]] {
			seen[e[0]] = true
			out = append(out, e[0])
		}
	}
	return out
}

// agentForModel resolves the root agent id serving a model.
func agentForModel(model string) (string, bool) {
	for _, e := range catalog() {
		if e[0] == model {
			return e[1], true
		}
	}
	return "", false
}

// refreshCatalog parses the upstream free-agents.ts, resolving FREEBUFF_*
// model-id identifiers against the constants files so the catalog tracks
// upstream edits without a router release.
func refreshCatalog() {
	defer func() {
		catMu.Lock()
		catFetching = false
		catMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	agentsSrc, err := httpGet(ctx, freeAgentsSource)
	if err != nil {
		return
	}
	ids := map[string]string{}
	for _, url := range []string{
		"https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/freebuff-models.ts",
		"https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/freebuff-model-entitlements.ts",
	} {
		if src, err := httpGet(ctx, url); err == nil {
			ids = mergeIDLiterals(ids, string(src))
		}
	}

	out := make([][2]string, 0, len(fallbackAgentModels)+8)
	seen := map[string]bool{}
	for _, e := range fallbackAgentModels { // unmetered-first anchor order
		seen[e.Model] = true
		out = append(out, [2]string{e.Model, e.Agent})
	}
	for _, m := range findAgentModelPairs(string(agentsSrc), ids) {
		if !seen[m[0]] {
			seen[m[0]] = true
			out = append(out, m)
		}
	}
	if len(out) <= len(fallbackAgentModels) {
		return // nothing new learned
	}
	catMu.Lock()
	modelAgents = out
	catAt = time.Now()
	catMu.Unlock()
	log.Printf("[Freebuff] live catalog: %d models (fallback %d)", len(out), len(fallbackAgentModels))
}

// findAgentModelPairs extracts extra (model, agent) pairs from the
// FREE_MODE_AGENT_MODELS block that are not already in the fallback table,
// resolving FREEBUFF_* identifiers through `ids`.
func findAgentModelPairs(src string, ids map[string]string) [][2]string {
	var out [][2]string
	blocks := regexp.MustCompile(`'(base[23]-free[^']*)':\s*new\s+Set\(\[([^\]]*)\]`).FindAllStringSubmatch(src, -1)
	ident := regexp.MustCompile(`[A-Z][A-Z0-9_]{6,}`)
	literal := regexp.MustCompile(`'([^']+)'`)
	for _, b := range blocks {
		agent := b[1]
		for _, item := range strings.Split(b[2], ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			var model string
			if s := literal.FindStringSubmatch(item); s != nil {
				model = s[1]
			} else if c := ident.FindString(item); c != "" {
				if lit, ok := ids[c]; ok {
					model = lit
				}
			}
			if model != "" && !strings.HasPrefix(model, "FREEBUFF_") {
				out = append(out, [2]string{model, agent})
			}
		}
	}
	return out
}

// mergeIDLiterals scrapes `export const FREEBUFF_X_MODEL_ID = 'lit'` pairs.
func mergeIDLiterals(ids map[string]string, src string) map[string]string {
	out := ids
	for _, m := range regexp.MustCompile(`(FREEBUFF_[A-Z0-9_]+_MODEL_ID)\s*=\s*'([^']+)'`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// ---------- HTTP surface ----------

func checkAuth(r *http.Request) bool {
	provided := r.Header.Get("Authorization")
	if len(provided) >= 7 && strings.EqualFold(provided[:7], "Bearer ") {
		provided = provided[7:]
	}
	if provided == "" {
		provided = r.Header.Get("x-api-key")
	}
	return provided == cfg.AuthTok
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oaiErr(w http.ResponseWriter, status int, msg, errType string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{"message": msg, "type": errType, "code": status},
	})
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	models := ModelList()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "freebuff",
		})
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

type httpUpstreamError struct {
	status  int
	message string
}

func (e *httpUpstreamError) Error() string { return e.message }

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		oaiErr(w, 400, "failed to read body", "invalid_request_error")
		return
	}
	var payload map[string]interface{}
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		oaiErr(w, 400, "invalid JSON body", "invalid_request_error")
		return
	}

	if countPools() == 0 {
		// Last resort: a CLI login on this machine can still unlock Freebuff.
		if tok := cliToken(); tok != "" {
			SetTokens([]string{tok})
		}
	}
	if countPools() == 0 {
		oaiErr(w, 401, "Freebuff is not connected yet. Connect it in the dashboard (Connect → Freebuff) by pasting your Freebuff auth token, set FREEBUFF_TOKENS, or run `npm i -g freebuff && freebuff` once on this machine — the CLI login is detected automatically.", "authentication_error")
		return
	}

	stream := false
	if s, ok := payload["stream"].(bool); ok {
		stream = s
	}

	lastErr := "no token available"
	for attempt := 0; attempt < maxChainAttempts; attempt++ {
		p := pools.pick()
		if p == nil {
			break
		}
		done, retryable, err := p.chatOnce(r.Context(), w, payload, stream)
		if done {
			return
		}
		if err != nil {
			lastErr = err.Error()
			if !retryable {
				if he, ok := err.(*httpUpstreamError); ok {
					oaiErr(w, he.status, he.message, "upstream_error")
				} else {
					oaiErr(w, 502, "Freebuff upstream error: "+err.Error(), "api_error")
				}
				return
			}
		}
	}
	oaiErr(w, 503, lastErr, "service_unavailable")
}

// chatOnce drives one full attempt: session → run → metadata → upstream chat.
// It writes the successful response itself (SSE passthrough when streaming)
// and reports done=true. retryable errors keep the token chain going.
func (p *tokenPool) chatOnce(ctx context.Context, w http.ResponseWriter, payload map[string]interface{}, stream bool) (done, retryable bool, err error) {
	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)
	agent, ok := agentForModel(model)
	if !ok {
		// Unknown model: fall back to the default unmetered root so the
		// request still completes (the backend normalizes the catalog id).
		agent = fallbackAgentModels[0].Agent
		model = fallbackAgentModels[0].Model
	}

	instanceID, err := p.ensureSession(ctx)
	if err != nil {
		if errors.Is(err, errAuthRejected) {
			p.cooldown(cooldownOnAuthFail, "auth rejected")
			return false, true, err
		}
		var wr *waitingRoomError
		if errors.As(err, &wr) {
			w.Header().Set("Content-Type", "application/json")
			return false, false, &httpUpstreamError{status: 503, message: err.Error()}
		}
		return false, true, err
	}

	runID, err := p.getRun(ctx, agent)
	if err != nil {
		if errors.Is(err, errAuthRejected) {
			p.cooldown(cooldownOnAuthFail, "auth rejected")
			return false, true, err
		}
		return false, true, err
	}

	payload["model"] = model
	meta, _ := payload["codebuff_metadata"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
	}
	meta["run_id"] = runID
	meta["cost_mode"] = "free"
	meta["client_id"] = clientSessionID()
	if instanceID != "" {
		meta["freebuff_instance_id"] = instanceID
	}
	payload["codebuff_metadata"] = meta
	body, err := json.Marshal(payload)
	if err != nil {
		return false, false, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(cfg.BaseURL, "/")+"/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", upstreamUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		p.cooldown(cooldownOnAuthFail, "auth rejected")
		return false, true, errAuthRejected
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := string(raw)
		switch {
		case strings.Contains(msg, "session_expired"), strings.Contains(msg, "session_superseded"),
			strings.Contains(msg, "waiting_room"), strings.Contains(msg, "freebuff_update_required"):
			p.invalidateSession(msg)
			return false, true, errors.New("session invalid — refreshed")
		case strings.Contains(msg, "run_id"), strings.Contains(msg, "runId"), strings.Contains(msg, "agent-run"):
			p.invalidateRun(agent, msg)
			return false, true, errors.New("run invalid — rotating")
		default:
			return false, false, &httpUpstreamError{status: resp.StatusCode, message: clip(raw)}
		}
	}

	// Success — pipe the upstream response straight through.
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
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true, false, nil // client went away — done
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	return true, false, nil
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	all := pools.snapshot()
	type tok struct {
		Name    string `json:"name"`
		Cooling bool   `json:"cooling"`
		Session string `json:"session"`
		LastErr string `json:"last_error,omitempty"`
	}
	toks := make([]tok, 0, len(all))
	now := time.Now()
	for _, p := range all {
		p.mu.Lock()
		t := tok{Name: p.name, Cooling: now.Before(p.coolTill), Session: p.session.status, LastErr: p.lastErr}
		p.mu.Unlock()
		toks = append(toks, t)
	}
	cliPath, cliOK := CLIFileStatus()
	writeJSON(w, 200, map[string]interface{}{
		"service":   "freebuff-bridge",
		"connected": len(all) > 0,
		"tokens":    toks,
		"models":    len(ModelList()),
		"cli_file":  cliPath,
		"cli_found": cliOK,
		"base_url":  cfg.BaseURL,
	})
}

func countPools() int { return len(pools.snapshot()) }

// ---------- admin surface (called by core admin.go) ----------

// Status summarizes the bridge for the dashboard Connect panel.
func Status() map[string]interface{} {
	all := pools.snapshot()
	cliPath, cliOK := CLIFileStatus()
	items := make([]map[string]interface{}, 0, len(all))
	now := time.Now()
	for _, p := range all {
		p.mu.Lock()
		items = append(items, map[string]interface{}{
			"name": p.name, "cooling": now.Before(p.coolTill), "session": p.session.status, "last_error": p.lastErr,
		})
		p.mu.Unlock()
	}
	return map[string]interface{}{
		"connected": len(all) > 0,
		"tokens":    len(all),
		"detail":    items,
		"models":    ModelList(),
		"cli_file":  cliPath,
		"cli_found": cliOK,
		"how_to": []string{
			"Web: open freebuff.com → sign in → copy your auth token",
			"CLI: npm i -g freebuff && freebuff → the token lands in ~/.config/manicode/credentials.json",
			"Then paste it in Dashboard → Connect → Freebuff (or set FREEBUFF_TOKENS)",
		},
	}
}

// TokensFromEnv reads FREEBUFF_TOKENS (comma/newline/semicolon separated).
func TokensFromEnv() []string {
	raw := os.Getenv("FREEBUFF_TOKENS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	split := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	out := make([]string, 0, len(split))
	for _, t := range split {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// clientSessionID mirrors the official SDK's per-request id
// (Math.random().toString(36).substring(2, 15) — 13 base-36 chars).
func clientSessionID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 13)
	for i := range out {
		n, _ := rand.Int(rand.Reader, big.NewInt(36))
		out[i] = alphabet[n.Int64()]
	}
	return string(out)
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// initConfig reads env config. Called by the OmniRouter core at boot.
func initConfig() {
	cfg = fbconfig{
		BaseURL: strings.TrimRight(envOr("FREEBUFF_BASE_URL", defaultBase), "/"),
		AuthTok: envOr("AUTH_TOKEN", "freebuff"),
	}
	// Env tokens win; otherwise the dashboard/store list arrives later via
	// SetTokens; otherwise the CLI file is used lazily on first chat.
	if toks := TokensFromEnv(); len(toks) > 0 {
		SetTokens(toks)
	} else {
		log.Printf("[Freebuff] bridge ready — no token yet (dashboard Connect → Freebuff, FREEBUFF_TOKENS, or CLI login)")
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
