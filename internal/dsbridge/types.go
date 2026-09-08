// types.go — core types and globals for the DeepSeek bridge (package dsbridge).

package dsbridge

import (
        "encoding/json"
        "sync"
        "sync/atomic"
        "time"
)

// ---------- wire types ----------

// Message is one OpenAI-style chat message (content stays raw so string and
// content-part arrays both survive).
type Message struct {
        Role    string          `json:"role"`
        Content json.RawMessage `json:"content"`
}

// UpstreamResult is one event from the DeepSeek completion stream — the same
// contract the other bridges use, so the shared Anthropic formatter and the
// agent interceptor plug in unchanged.
type UpstreamResult struct {
        Chunk     string // incremental content delta (rune-safe)
        FullText  string // full-text rewrite (unused for DS; kept for parity)
        Reasoning string // DeepThink reasoning delta
        Err       error
}

// SendOptions carries everything one upstream call needs.
type SendOptions struct {
        // Model is DeepSeek's `model_type` wire value: "default" or "expert".
        Model string
        // Thinking enables DeepThink reasoning (thinking_enabled).
        Thinking *bool
        // WebSearch enables web search (search_enabled).
        WebSearch *bool
        // ClientMessagesRaw keeps the (possibly agent-transformed) raw messages
        // for parity with the other bridges' option struct.
        ClientMessagesRaw json.RawMessage
        // ChatID / ReasoningEffort exist for parity with the shared Anthropic
        // handler; DeepSeek runs stateless so ChatID is unused.
        ChatID          string
        ReasoningEffort string
        // Account pins the request to one pool account (already picked by the
        // handler). The failover layer may still switch accounts on 429.
        Account *Account
}

type ResponseResult struct {
        Content      string
        Text         string
        Prompt       string
        FinishReason string
        Reasoning    string
}

// ---------- session state (dashboard/status parity) ----------

type SessionState struct {
        mu           sync.Mutex
        Token        string
        UserID       string
        UserName     string
        Messages     []Message
        Features     Features
        Initialized  bool
        Initializing bool
}

type Features struct {
        WebSearch     bool `json:"webSearch"`
        AutoWebSearch bool `json:"autoWebSearch"`
        Thinking      bool `json:"thinking"`
        ImageGen      bool `json:"imageGen"`
        PreviewMode   bool `json:"previewMode"`
}

var session = &SessionState{
        UserName: "DeepSeek",
        Features: Features{Thinking: true},
}

var (
        verbose  bool
        gRunning atomic.Bool
)

// ---------- models ----------

type ModelInfo struct {
        ID           string
        Name         string
        Description  string
        Capabilities map[string]interface{}
        Created      int64
}

// dsModels is the static catalog: DeepSeek's web app exposes exactly two
// `model_type` values (Instant / Expert); DeepThink thinking and web search
// are per-request toggles, so `deepseek-reasoner` maps to Instant + thinking.
var dsModels = []ModelInfo{
        {ID: "deepseek-chat", Name: "DeepSeek Chat (Instant)", Description: "Fast default model — great default for agents and chat"},
        {ID: "deepseek-reasoner", Name: "DeepSeek DeepThink (R1-style reasoning)", Description: "Instant model with DeepThink reasoning enabled"},
        {ID: "deepseek-expert", Name: "DeepSeek Expert", Description: "Stronger, slower model — hard reasoning"},
}

// resolveModel maps a public model id to (model_type, thinking_enabled).
func resolveModel(name string) (string, bool) {
        switch name {
        case "deepseek-expert":
                return "expert", false
        case "deepseek-reasoner":
                return "default", true
        case "deepseek-chat", "":
                return "default", false
        }
        return "", false
}

func isKnownModel(name string) bool {
        mt, _ := resolveModel(name)
        return mt != ""
}

// ---------- per-model feature state (parity with other bridges) ----------

type ModelFeatureState struct {
        IncludeAll bool
        Overrides  map[string]interface{}
}

var (
        modelFeatureStates   = make(map[string]*ModelFeatureState)
        modelFeatureStatesMu sync.Mutex
)

var modelsCacheTime time.Time
