// store.go — persistent router state: client API keys (with quota + model
// allowlist), custom providers (with model alias map), and live usage
// statistics. A single JSON file (data/omnirouter.json), atomically rewritten.
// Stats are flushed at most every ~5s (dirty-flag) so per-request accounting
// stays cheap.

package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type APIKey struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	Enabled    bool   `json:"enabled"`
	Requests   int64  `json:"requests"`
	Errors     int64  `json:"errors"`
	TokensIn   int64  `json:"tokens_in"`
	TokensOut  int64  `json:"tokens_out"`
	LastUsedAt int64  `json:"last_used_at"`
	// MaxRequests caps total requests for this key (0 = unlimited).
	MaxRequests int64 `json:"max_requests,omitempty"`
	// AllowedModels restricts which models this key may request.
	// Entries are exact model ids or provider wildcards like "qwen/*".
	// Empty = all models allowed.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

type CustomProvider struct {
	Name    string `json:"name"`      // unique slug, used as prefix "name/model"
	BaseURL string `json:"base_url"`  // OpenAI-compatible root (…/v1)
	APIKey  string `json:"api_key"`   // upstream key (Bearer)
	Models  []string `json:"models"`  // advertised models (or fetched live)
	Enabled bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	// ModelMap maps public model names → the upstream model id actually
	// sent to this provider (aliasing, yolorouter-style).
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// ---------- usage statistics ----------

type ProviderStat struct {
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

type ModelStat struct {
	Requests  int64 `json:"requests"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

type HourBucket struct {
	Hour      int64 `json:"hour"` // unix hour
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

type Stats struct {
	TotalRequests int64                     `json:"total_requests"`
	TotalErrors   int64                     `json:"total_errors"`
	TokensIn      int64                     `json:"tokens_in"`
	TokensOut     int64                     `json:"tokens_out"`
	Providers     map[string]*ProviderStat  `json:"providers"`
	Models        map[string]*ModelStat     `json:"models"`
	Hourly        []HourBucket              `json:"hourly"`
	StartedAt     int64                     `json:"started_at"`
}

const hourlyKeep = 48 // last 48 hours

func newStats() *Stats {
	return &Stats{
		Providers: map[string]*ProviderStat{},
		Models:    map[string]*ModelStat{},
		Hourly:    []HourBucket{},
		StartedAt: time.Now().Unix(),
	}
}

func (s *Stats) record(provider, model string, in, out int64, failed bool) {
	s.TotalRequests++
	if failed {
		s.TotalErrors++
	}
	s.TokensIn += in
	s.TokensOut += out

	ps, ok := s.Providers[provider]
	if !ok {
		ps = &ProviderStat{}
		s.Providers[provider] = ps
	}
	ps.Requests++
	if failed {
		ps.Errors++
	}
	ps.TokensIn += in
	ps.TokensOut += out

	if model != "" && model != "auto" {
		ms, ok := s.Models[model]
		if !ok {
			ms = &ModelStat{}
			s.Models[model] = ms
		}
		ms.Requests++
		ms.TokensIn += in
		ms.TokensOut += out
	}

	h := time.Now().Unix() / 3600 * 3600
	// find-or-append the bucket (normally the last one)
	bucket := -1
	for i := len(s.Hourly) - 1; i >= 0 && i >= len(s.Hourly)-4; i-- {
		if s.Hourly[i].Hour == h {
			bucket = i
			break
		}
	}
	if bucket < 0 {
		s.Hourly = append(s.Hourly, HourBucket{Hour: h})
		bucket = len(s.Hourly) - 1
	}
	s.Hourly[bucket].Requests++
	if failed {
		s.Hourly[bucket].Errors++
	}
	s.Hourly[bucket].TokensIn += in
	s.Hourly[bucket].TokensOut += out
	// trim old buckets
	if len(s.Hourly) > hourlyKeep {
		s.Hourly = s.Hourly[len(s.Hourly)-hourlyKeep:]
	}
}

// ---------- store ----------

type storeData struct {
	Keys            []APIKey         `json:"keys"`
	CustomProviders []CustomProvider `json:"custom_providers"`
	Stats           *Stats           `json:"stats,omitempty"`
}

type Store struct {
	mu       sync.RWMutex
	path     string
	data     storeData
	dirty    bool
	lastSave time.Time
}

func NewStore(dir string) *Store {
	s := &Store{path: filepath.Join(dir, "omnirouter.json")}
	raw, err := os.ReadFile(s.path)
	if err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	if s.data.Keys == nil {
		s.data.Keys = []APIKey{}
	}
	if s.data.CustomProviders == nil {
		s.data.CustomProviders = []CustomProvider{}
	}
	if s.data.Stats == nil {
		s.data.Stats = newStats()
	}
	return s
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Flush persists dirty state (stats) at most once per 5 seconds. Called by a
// background ticker and before shutdown.
func (s *Store) Flush(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty && !force {
		return
	}
	if !force && time.Since(s.lastSave) < 5*time.Second {
		return
	}
	if err := s.saveLocked(); err == nil {
		s.dirty = false
		s.lastSave = time.Now()
	}
}

// EnsureSeedKey creates the first key if none exists: ROUTER_KEY when set,
// else a random sk- key. Returns the key material for the boot banner.
func (s *Store) EnsureSeedKey(routerKey string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.data.Keys {
		if k.Enabled {
			return k.Key, false
		}
	}
	key := routerKey
	if key == "" {
		key = "sk-omni-" + randomHex(24)
	}
	s.data.Keys = append(s.data.Keys, APIKey{
		Key: key, Name: "default", CreatedAt: time.Now().Unix(), Enabled: true,
	})
	_ = s.saveLocked()
	return key, true
}

func (s *Store) ListKeys() []APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]APIKey, len(s.data.Keys))
	copy(out, s.data.Keys)
	return out
}

// LookupKey returns a copy of the key record (quota + allowlist checks).
func (s *Store) LookupKey(key string) (APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.data.Keys {
		if k.Key == key && k.Enabled {
			return k, true
		}
	}
	return APIKey{}, false
}

// ValidateKey checks a client key and bumps its request counter.
func (s *Store) ValidateKey(key string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Keys {
		if s.data.Keys[i].Key == key && s.data.Keys[i].Enabled {
			s.data.Keys[i].Requests++
			s.data.Keys[i].LastUsedAt = time.Now().Unix()
			s.dirty = true
			return true
		}
	}
	return false
}

func (s *Store) CreateKey(name string, maxReq int64, allowedModels []string) APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := APIKey{
		Key:           "sk-omni-" + randomHex(24),
		Name:          name,
		CreatedAt:     time.Now().Unix(),
		Enabled:       true,
		MaxRequests:   maxReq,
		AllowedModels: allowedModels,
	}
	s.data.Keys = append(s.data.Keys, k)
	_ = s.saveLocked()
	return k
}

func (s *Store) DeleteKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Keys {
		if s.data.Keys[i].Key == key {
			s.data.Keys = append(s.data.Keys[:i], s.data.Keys[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

func (s *Store) ToggleKey(key string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Keys {
		if s.data.Keys[i].Key == key {
			s.data.Keys[i].Enabled = enabled
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// UpdateKey edits quota/allowlist of an existing key.
func (s *Store) UpdateKey(key string, maxReq *int64, allowed *[]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Keys {
		if s.data.Keys[i].Key == key {
			if maxReq != nil {
				s.data.Keys[i].MaxRequests = *maxReq
			}
			if allowed != nil {
				s.data.Keys[i].AllowedModels = *allowed
			}
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// RecordUsage updates per-key counters (called by the forwarder once the
// upstream attempt settled).
func (s *Store) RecordUsage(key string, inTok, outTok int64, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Keys {
		if s.data.Keys[i].Key == key {
			s.data.Keys[i].TokensIn += inTok
			s.data.Keys[i].TokensOut += outTok
			if failed {
				s.data.Keys[i].Errors++
			}
			s.dirty = true
			return
		}
	}
}

// RecordStat bumps global + per-provider + per-model + hourly counters.
func (s *Store) RecordStat(provider, model string, in, out int64, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Stats.record(provider, model, in, out, failed)
	s.dirty = true
}

func (s *Store) SnapshotStats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := *s.data.Stats
	// deep-ish copy of maps for safe json encoding
	provs := make(map[string]*ProviderStat, len(st.Providers))
	for k, v := range st.Providers {
		cp := *v
		provs[k] = &cp
	}
	models := make(map[string]*ModelStat, len(st.Models))
	for k, v := range st.Models {
		cp := *v
		models[k] = &cp
	}
	st.Providers = provs
	st.Models = models
	h := make([]HourBucket, len(st.Hourly))
	copy(h, st.Hourly)
	st.Hourly = h
	return st
}

// TopModels returns up to n most-requested model ids.
func (s *Store) TopModels(n int) []string {
	st := s.SnapshotStats()
	type kv struct {
		k string
		v int64
	}
	var list []kv
	for k, v := range st.Models {
		list = append(list, kv{k, v.Requests})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	var out []string
	for i, e := range list {
		if i >= n {
			break
		}
		out = append(out, e.k)
	}
	return out
}

// ---------- custom providers ----------

func (s *Store) ListProviders() []CustomProvider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CustomProvider, len(s.data.CustomProviders))
	copy(out, s.data.CustomProviders)
	return out
}

func (s *Store) AddProvider(p CustomProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Name == "" || p.BaseURL == "" {
		return fmt.Errorf("name and base_url are required")
	}
	for _, e := range s.data.CustomProviders {
		if strings.EqualFold(e.Name, p.Name) {
			return fmt.Errorf("provider %q already exists", p.Name)
		}
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	s.data.CustomProviders = append(s.data.CustomProviders, p)
	return s.saveLocked()
}

// UpdateProvider replaces an existing provider record (keeps CreatedAt).
func (s *Store) UpdateProvider(p CustomProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.CustomProviders {
		if strings.EqualFold(s.data.CustomProviders[i].Name, p.Name) {
			created := s.data.CustomProviders[i].CreatedAt
			p.CreatedAt = created
			s.data.CustomProviders[i] = p
			return s.saveLocked()
		}
	}
	return fmt.Errorf("provider %q not found", p.Name)
}

func (s *Store) DeleteProvider(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.CustomProviders {
		if strings.EqualFold(s.data.CustomProviders[i].Name, name) {
			s.data.CustomProviders = append(s.data.CustomProviders[:i], s.data.CustomProviders[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

func (s *Store) ToggleProvider(name string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.CustomProviders {
		if strings.EqualFold(s.data.CustomProviders[i].Name, name) {
			s.data.CustomProviders[i].Enabled = enabled
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
