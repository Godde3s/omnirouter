// core_test.go — unit tests for the router core (registry, store, retry).

package core

import (
	"testing"
)

func TestResolveAutoChain(t *testing.T) {
	r := NewRegistry("test-token", nil)
	r.SetBridgeURL("qwen", "http://127.0.0.1:1")
	r.SetBridgeURL("glm", "http://127.0.0.1:2")
	r.SetBridgeURL("ds", "http://127.0.0.1:3")

	chain := r.Resolve("auto")
	if len(chain) != 3 {
		t.Fatalf("auto chain should have 3 candidates, got %d", len(chain))
	}
	if chain[0].ID != "qwen" || chain[1].ID != "ds" || chain[2].ID != "glm" {
		t.Fatalf("auto order wrong: %s, %s, %s", chain[0].ID, chain[1].ID, chain[2].ID)
	}
	if chain[0].Models[0] != "qwen3.8-max" {
		t.Fatalf("auto should pin qwen3.8-max, got %v", chain[0].Models)
	}
}

func TestResolvePrefixForm(t *testing.T) {
	r := NewRegistry("t", nil)
	r.SetBridgeURL("qwen", "http://127.0.0.1:1")

	c := r.Resolve("qwen/qwen3.7-plus")
	if len(c) != 1 || c[0].ID != "qwen" || c[0].Models[0] != "qwen3.7-plus" {
		t.Fatalf("prefix resolve broken: %+v", c)
	}
	// custom slug form
	r.providers = append(r.providers, &Provider{
		ID: "custom:gemini", Kind: KindCustom, BaseURL: "http://x", Enabled: true,
	})
	c = r.Resolve("gemini/gemini-2.5-pro")
	if len(c) != 1 || c[0].ID != "custom:gemini" || c[0].Models[0] != "gemini-2.5-pro" {
		t.Fatalf("custom prefix resolve broken: %+v", c)
	}
	// unknown provider
	if got := r.Resolve("nosuch/model"); got != nil {
		t.Fatalf("unknown provider should resolve to nil, got %+v", got)
	}
}

func TestResolveCatalogAndDuplicates(t *testing.T) {
	r := NewRegistry("t", nil)
	r.SetBridgeURL("qwen", "http://127.0.0.1:1")
	r.SetBridgeURL("glm", "http://127.0.0.1:2")
	r.mu.Lock()
	r.providers[0].Models = []string{"shared-model", "qwen3.8-max"}
	r.providers[1].Models = []string{"shared-model"}
	r.rebuildCatalogLocked()
	r.mu.Unlock()

	c := r.Resolve("shared-model")
	if len(c) != 2 || c[0].ID != "qwen" {
		t.Fatalf("catalog owner should be qwen with glm as backup, got %+v", c)
	}
	c = r.Resolve("qwen3.8-max")
	if len(c) != 1 || c[0].ID != "qwen" {
		t.Fatalf("single-owner resolve broken: %+v", c)
	}
}

func TestStoreKeysAndProviders(t *testing.T) {
	s := NewStore(t.TempDir())
	key, created := s.EnsureSeedKey("")
	if !created || key == "" {
		t.Fatal("seed key must be created")
	}
	if !s.ValidateKey(key) {
		t.Fatal("seed key must validate")
	}
	if s.ValidateKey("sk-wrong") {
		t.Fatal("wrong key must not validate")
	}
	k2 := s.CreateKey("test")
	if !s.ValidateKey(k2.Key) {
		t.Fatal("created key must validate")
	}
	if !s.DeleteKey(k2.Key) || s.ValidateKey(k2.Key) {
		t.Fatal("delete must remove key")
	}

	if err := s.AddProvider(CustomProvider{Name: "gemini", BaseURL: "https://x/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProvider(CustomProvider{Name: "GEMINI", BaseURL: "https://y/v1"}); err == nil {
		t.Fatal("duplicate name (case-insensitive) must be rejected")
	}
	if !s.ToggleProvider("gemini", false) || !s.DeleteProvider("gemini") {
		t.Fatal("toggle/delete provider broken")
	}
}

func TestRetryable(t *testing.T) {
	for _, st := range []int{401, 403, 429, 500, 502, 503} {
		if !retryable(st) {
			t.Fatalf("%d must be retryable", st)
		}
	}
	for _, st := range []int{200, 400, 404} {
		if retryable(st) {
			t.Fatalf("%d must NOT be retryable", st)
		}
	}
}

func TestBridgeAuthHeaderFallback(t *testing.T) {
	f := newForwarder(nil, "")
	if f.bridgeAuthHeader("glm") != "Bearer Waguri" || f.bridgeAuthHeader("qwen") != "Bearer qwen" {
		t.Fatal("default bridge tokens broken")
	}
	f2 := newForwarder(nil, "shared-secret")
	if f2.bridgeAuthHeader("ds") != "Bearer shared-secret" {
		t.Fatal("shared internal token must win")
	}
}
