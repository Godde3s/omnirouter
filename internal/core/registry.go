// registry.go — provider registry + unified model catalog + routing.
//
// Providers come in two kinds:
//   bridge — the embedded bridges (glm / qwen / ds / gemini), each served by
//            the core on an internal 127.0.0.1 listener; core forwards /v1/*
//            to them with the shared internal AUTH_TOKEN.
//   custom — any OpenAI-compatible endpoint (Gemini AI Studio, OpenRouter,
//            your own gateways…). Managed live from the dashboard.
//
// The catalog is refreshed from every provider's /v1/models (60s TTL) and
// supports explicit "provider/model" addressing, public model aliases
// (per-provider map: public name → upstream id) and an "auto" chain with
// cross-provider failover. A provider that just failed is put on a short
// cooldown so the chain prefers healthier candidates immediately.

package core

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "sort"
        "strings"
        "sync"
        "time"
)

type ProviderKind string

const (
        KindBridge ProviderKind = "bridge"
        KindCustom ProviderKind = "custom"
)

type Provider struct {
        ID      string       `json:"id"`   // "glm" | "qwen" | "ds" | "gemini" | "custom:<name>"
        Kind    ProviderKind `json:"kind"`
        BaseURL string       `json:"base_url"` // internal listener or external root
        APIKey  string       `json:"-"`        // custom only
        Label   string       `json:"label"`
        Enabled bool         `json:"enabled"`
        Models  []string     `json:"models"`
        Healthy bool         `json:"healthy"`
        LastErr string       `json:"last_error,omitempty"`
        // Aliases: public model name → upstream model id for THIS provider.
        Aliases map[string]string `json:"aliases,omitempty"`
}

type catalogEntry struct {
        Model    string
        Upstream string // set when entry is an alias
        Provider string
}

type Registry struct {
        mu        sync.RWMutex
        providers []*Provider
        catalog   map[string]catalogEntry // public id → provider (first enabled wins)
        catTime   time.Time
        client    *http.Client

        coolMu    sync.Mutex
        coolUntil map[string]int64 // provider id → cooldown expiry (unix)
}

const catalogTTL = 60 * time.Second

func NewRegistry(internalToken string, customs []CustomProvider) *Registry {
        r := &Registry{
                catalog:   map[string]catalogEntry{},
                client:    &http.Client{Timeout: 15 * time.Second},
                coolUntil: map[string]int64{},
        }
        // Bridge defaults; enablement is decided by the caller per config.
        r.providers = append(r.providers,
                &Provider{ID: "qwen", Kind: KindBridge, Label: "Qwen (chat.qwen.ai)", Enabled: true},
                &Provider{ID: "glm", Kind: KindBridge, Label: "GLM (chat.z.ai)", Enabled: true},
                &Provider{ID: "ds", Kind: KindBridge, Label: "DeepSeek (chat.deepseek.com)", Enabled: true},
                &Provider{ID: "gemini", Kind: KindBridge, Label: "Gemini (gemini.google.com)", Enabled: true},
        )
        for _, c := range customs {
                if !c.Enabled {
                        continue
                }
                r.providers = append(r.providers, &Provider{
                        ID:      "custom:" + c.Name,
                        Kind:    KindCustom,
                        BaseURL: strings.TrimRight(c.BaseURL, "/"),
                        APIKey:  c.APIKey,
                        Label:   c.Name,
                        Enabled: true,
                        Models:  c.Models,
                        Aliases: c.ModelMap,
                })
        }
        return r
}

func (r *Registry) Providers() []Provider {
        r.mu.RLock()
        defer r.mu.RUnlock()
        out := make([]Provider, 0, len(r.providers))
        for _, p := range r.providers {
                cp := *p
                cp.APIKey = ""
                out = append(out, cp)
        }
        return out
}

// SetBridgeURL assigns the internal listener address of an embedded bridge.
func (r *Registry) SetBridgeURL(id, baseURL string) {
        r.mu.Lock()
        defer r.mu.Unlock()
        for _, p := range r.providers {
                if p.ID == id {
                        p.BaseURL = baseURL
                }
        }
}

// SetBridgeEnabled marks an embedded bridge enabled/disabled (e.g. no tokens).
func (r *Registry) SetBridgeEnabled(id string, enabled bool) {
        r.mu.Lock()
        defer r.mu.Unlock()
        for _, p := range r.providers {
                if p.ID == id {
                        p.Enabled = enabled
                }
        }
}

// SetAliases installs/updates the public→upstream alias map of a provider
// (bridge id or custom name) and rebuilds the catalog immediately.
func (r *Registry) SetAliases(providerID string, m map[string]string) bool {
        r.mu.Lock()
        defer r.mu.Unlock()
        found := false
        for _, p := range r.providers {
                pid := p.ID
                if p.Kind == KindCustom {
                        pid = strings.TrimPrefix(p.ID, "custom:")
                }
                if strings.EqualFold(pid, providerID) {
                        p.Aliases = m
                        found = true
                }
        }
        if found {
                r.rebuildCatalogLocked()
        }
        return found
}

// OwnerOf returns the provider id that owns a model id in the catalog
// (used by per-key allowlist checks).
func (r *Registry) OwnerOf(model string) string {
        r.mu.RLock()
        defer r.mu.RUnlock()
        if e, ok := r.catalog[model]; ok {
                return e.Provider
        }
        return ""
}

// Cooldown marks a provider unhealthy for the next `seconds` so failover
// prefers other candidates.
func (r *Registry) Cooldown(providerID string, seconds int64) {
        r.coolMu.Lock()
        r.coolUntil[providerID] = time.Now().UnixMilli() + seconds*1000
        r.coolMu.Unlock()
}

// Cooling reports whether a provider is currently on cooldown.
func (r *Registry) Cooling(providerID string) bool {
        r.coolMu.Lock()
        defer r.coolMu.Unlock()
        until, ok := r.coolUntil[providerID]
        if !ok {
                return false
        }
        if time.Now().UnixMilli() > until {
                delete(r.coolUntil, providerID)
                return false
        }
        return true
}

// RefreshModels re-fetches /v1/models from every enabled provider.
func (r *Registry) RefreshModels(ctx context.Context) {
        r.mu.Lock()
        defer r.mu.Unlock()
        for _, p := range r.providers {
                if !p.Enabled || p.BaseURL == "" {
                        continue
                }
                models, err := fetchModels(ctx, r.client, p)
                if err != nil {
                        p.Healthy = false
                        p.LastErr = err.Error()
                        if p.Kind == KindBridge && len(p.Models) == 0 {
                                p.Models = fallbackBridgeModels(p.ID)
                        }
                        continue
                }
                p.Healthy = true
                p.LastErr = ""
                if len(models) > 0 {
                        p.Models = models
                }
        }
        r.rebuildCatalogLocked()
        r.catTime = time.Now()
}

func fetchModels(ctx context.Context, client *http.Client, p *Provider) ([]string, error) {
        req, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/v1/models", nil)
        if err != nil {
                return nil, err
        }
        if p.Kind == KindCustom && p.APIKey != "" {
                req.Header.Set("Authorization", "Bearer "+p.APIKey)
        }
        resp, err := client.Do(req)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if resp.StatusCode != 200 {
                return nil, fmt.Errorf("models: HTTP %d", resp.StatusCode)
        }
        var parsed struct {
                Data []struct {
                        ID string `json:"id"`
                } `json:"data"`
        }
        if err := json.Unmarshal(body, &parsed); err != nil {
                return nil, err
        }
        var out []string
        for _, m := range parsed.Data {
                if m.ID != "" {
                        out = append(out, m.ID)
                }
        }
        return out, nil
}

func fallbackBridgeModels(id string) []string {
        switch id {
        case "qwen":
                return []string{"qwen3.8-max", "qwen3.7-plus"}
        case "glm":
                return []string{"GLM-5.1", "GLM-5"}
        case "ds":
                return []string{"deepseek-chat", "deepseek-reasoner"}
        case "gemini":
                return []string{"gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro"}
        }
        return nil
}

func (r *Registry) rebuildCatalogLocked() {
        r.catalog = map[string]catalogEntry{}
        for _, p := range r.providers {
                if !p.Enabled {
                        continue
                }
                for _, m := range p.Models {
                        if _, exists := r.catalog[m]; !exists {
                                r.catalog[m] = catalogEntry{Model: m, Provider: p.ID}
                        }
                }
                // aliases appear as public model ids owned by this provider
                for public, upstream := range p.Aliases {
                        if public == "" || upstream == "" {
                                continue
                        }
                        if _, exists := r.catalog[public]; !exists {
                                r.catalog[public] = catalogEntry{Model: public, Upstream: upstream, Provider: p.ID}
                        }
                }
        }
}

// Catalog returns the merged model list (sorted) with owning provider ids.
func (r *Registry) Catalog() ([]map[string]interface{}, bool) {
        r.mu.RLock()
        fresh := time.Since(r.catTime) < catalogTTL && len(r.catalog) > 0
        r.mu.RUnlock()
        if !fresh {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                r.RefreshModels(ctx)
                cancel()
        }
        r.mu.RLock()
        defer r.mu.RUnlock()
        ids := make([]string, 0, len(r.catalog))
        for id := range r.catalog {
                ids = append(ids, id)
        }
        sort.Strings(ids)
        out := make([]map[string]interface{}, 0, len(ids))
        for _, id := range ids {
                e := r.catalog[id]
                entry := map[string]interface{}{
                        "id":        id,
                        "object":    "model",
                        "created":   time.Now().Unix(),
                        "owned_by":  e.Provider,
                }
                if e.Upstream != "" {
                        entry["alias_for"] = e.Upstream
                }
                out = append(out, entry)
        }
        return out, true
}

// Resolve maps a client model id to an ordered candidate chain:
//   "auto"            → AUTO chain (env AUTO_CHAIN or a sane default across
//                       enabled providers, best-first)
//   "provider/model"  → exactly that provider (single candidate)
//   "model"           → catalog owner + (optionally) other providers serving it
// Each candidate's Models[0] is the upstream model id to send (alias applied).
func (r *Registry) Resolve(model string) []*Provider {
        r.mu.RLock()
        defer r.mu.RUnlock()

        var pick func(id string) *Provider
        pick = func(id string) *Provider {
                for _, p := range r.providers {
                        if p.ID == id && p.Enabled && p.BaseURL != "" {
                                return p
                        }
                }
                return nil
        }

        aliasOf := func(p *Provider, mid string) string {
                if p == nil {
                        return mid
                }
                if up, ok := p.Aliases[mid]; ok && up != "" {
                        return up
                }
                return mid
        }

        var chain []*Provider

        if model == "" || model == "auto" || model == "omni-auto" {
                for _, entry := range autoChainIDs(r) {
                        if i := strings.Index(entry, "/"); i > 0 {
                                chain = append(chain, r.resolvePrefixLocked(entry[:i], entry[i+1:])...)
                        }
                }
                return chain
        }

        // explicit prefix form: "qwen/qwen3.8-max" or "gemini/gemini-2.5-flash"
        if i := strings.Index(model, "/"); i > 0 {
                return r.resolvePrefixLocked(model[:i], model[i+1:])
        }

        // plain model id → catalog owner first, then any other provider listing it
        if e, ok := r.catalog[model]; ok {
                if p := pick(e.Provider); p != nil {
                        up := model
                        if e.Upstream != "" {
                                up = e.Upstream
                        } else {
                                up = aliasOf(p, model)
                        }
                        cp := *p
                        cp.Models = []string{up}
                        chain = append(chain, &cp)
                }
        }
        for _, p := range r.providers {
                if !p.Enabled || p.BaseURL == "" {
                        continue
                }
                if len(chain) > 0 && p.ID == chain[0].ID {
                        continue
                }
                up := aliasOf(p, model)
                for _, m := range p.Models {
                        if m == up || m == model {
                                cp := *p
                                cp.Models = []string{up}
                                chain = append(chain, &cp)
                                break
                        }
                }
        }
        return chain
}

// resolvePrefixLocked resolves "provider/model" — bridges by id ("qwen",
// "glm", "ds", "gemini") and custom providers by their slug ("gemini", …).
// Public aliases are translated to the upstream model id.
func (r *Registry) resolvePrefixLocked(pid, mid string) []*Provider {
        if mid == "" {
                return nil
        }
        var p *Provider
        for _, q := range r.providers {
                if !q.Enabled || q.BaseURL == "" {
                        continue
                }
                if q.ID == pid || (q.Kind == KindCustom && strings.EqualFold(strings.TrimPrefix(q.ID, "custom:"), pid)) {
                        p = q
                        break
                }
        }
        if p == nil {
                return nil
        }
        up := mid
        if up2, ok := p.Aliases[mid]; ok && up2 != "" {
                up = up2
        }
        cp := *p
        cp.Models = []string{up}
        return []*Provider{&cp}
}

// autoChainIDs builds the "auto" failover order: AUTO_CHAIN env when set,
// else the strongest model of every enabled bridge, best-first.
func autoChainIDs(r *Registry) []string {
        if env := strings.TrimSpace(os.Getenv("AUTO_CHAIN")); env != "" {
                var out []string
                for _, part := range strings.Split(env, ",") {
                        if p := strings.TrimSpace(part); p != "" {
                                out = append(out, p)
                        }
                }
                return out
        }
        preferred := []struct{ id, model string }{
                {"qwen", "qwen3.8-max"},
                {"ds", "deepseek-chat"},
                {"gemini", "gemini-3.6-flash"},
                {"glm", "GLM-5.1"},
        }
        var out []string
        for _, pf := range preferred {
                for _, p := range r.providers {
                        if p.ID == pf.id && p.Enabled && p.BaseURL != "" {
                                // use the provider's real strongest model if listed
                                chosen := pf.model
                                for _, m := range p.Models {
                                        if m == pf.model {
                                                chosen = m
                                                break
                                        }
                                }
                                out = append(out, p.ID+"/"+chosen)
                                break
                        }
                }
        }
        return out
}
