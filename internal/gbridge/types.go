// Core type definitions and global state for the Gemini bridge (package gbridge).

package gbridge

import (
        "encoding/json"
        "sync"
        "sync/atomic"
        "time"
)

// ============================================================================
// TYPE DEFINITIONS
// ============================================================================

type Message struct {
        Role    string          `json:"role"`
        Content json.RawMessage `json:"content"`
}

// SessionState is kept for handler/dashboard compatibility. The Gemini
// upstream is naturally stateless (temporary-chat flag), so the session only
// mirrors pool health.
type SessionState struct {
        mu           sync.Mutex
        UserName     string
        Initialized  bool
        Initializing bool
}

type UpstreamResult struct {
        Chunk     string
        FullText  string
        Reasoning string
        Err       error
}

type SendOptions struct {
        Model             string
        Thinking          *bool
        ChatID            string
        Messages          []Message
        ClientMessagesRaw json.RawMessage
        ReasoningEffort   string
        // WebSearch is accepted for API compatibility but Gemini web decides
        // grounding/search on its own — the flag is a no-op.
        WebSearch *bool
        // FileData carries uploaded-file references for vision requests
        // ([["/contrib_service/ttl_1d/..."]] shape from the push endpoint).
        FileData interface{}
        // Account pins this request to one Google account cookie pair
        // (multi-account pool). nil = guest flow (no cookies, at token empty).
        Account *Account
}

type ResponseResult struct {
        Content      string
        Text         string
        Prompt       string
        FinishReason string
        Reasoning    string
}

// ============================================================================
// GLOBAL STATE
// ============================================================================

var (
        verbose  bool
        gRunning atomic.Bool
        logMu    sync.Mutex
)

var session = &SessionState{
        UserName: "Guest",
}

type ModelInfo struct {
        ID           string
        Name         string
        Description  string
        Capabilities map[string]interface{}
        Created      int64 // upstream "created" unix seconds (0 = unknown)
}

var (
        modelsCache     []ModelInfo
        modelsCacheTime time.Time
        modelsCacheMu   sync.Mutex
)

const modelsCacheTTL = 5 * time.Minute

// Fallback if the discovery RPC is unreachable and the cache is empty.
// IDs follow the live web naming (category + version), verified against the
// account model registry; the registry itself is always fetched live first.
var fallbackModels = []ModelInfo{
        {ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash", Description: "Fast default model of the Gemini web app (flash tier)"},
        {ID: "gemini-3.5-flash-lite", Name: "Gemini 3.5 Flash-Lite", Description: "Lightweight fast model"},
        {ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro", Description: "Most capable Gemini web model (pro tier)"},
}

// ---------- Gemini globals ----------

var geminiUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
