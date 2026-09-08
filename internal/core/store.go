// store.go — persistent router state: client API keys + custom providers.
// A single JSON file (data/omnirouter.json), atomically rewritten on change.

package core

import (
	"strings"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

type CustomProvider struct {
	Name    string `json:"name"`              // unique slug, used as prefix "name/model"
	BaseURL string `json:"base_url"`          // OpenAI-compatible root (…/v1)
	APIKey  string `json:"api_key"`           // upstream key (Bearer)
	Models  []string `json:"models"`          // advertised models (or fetched live)
	Enabled bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

type storeData struct {
	Keys            []APIKey          `json:"keys"`
	CustomProviders []CustomProvider  `json:"custom_providers"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	data storeData
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
			return true
		}
	}
	return false
}

func (s *Store) CreateKey(name string) APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := APIKey{
		Key:       "sk-omni-" + randomHex(24),
		Name:      name,
		CreatedAt: time.Now().Unix(),
		Enabled:   true,
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

// RecordUsage updates per-key counters after each proxied request.
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
			_ = s.saveLocked()
			return
		}
	}
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
