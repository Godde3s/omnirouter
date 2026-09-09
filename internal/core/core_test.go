// core_test.go — unit tests for the router core (registry, store, retry,
// usage extraction, aliases, cooldown, quotas).

package core

import (
        "bytes"
        "testing"
        "time"
)

func TestResolveAutoChain(t *testing.T) {
        r := NewRegistry("test-token", nil)
        r.SetBridgeURL("qwen", "http://127.0.0.1:1")
        r.SetBridgeURL("glm", "http://127.0.0.1:2")
        r.SetBridgeURL("ds", "http://127.0.0.1:3")
        r.SetBridgeURL("gemini", "http://127.0.0.1:4")

        chain := r.Resolve("auto")
        if len(chain) != 4 {
                t.Fatalf("auto chain should have 4 candidates, got %d", len(chain))
        }
        if chain[0].ID != "qwen" || chain[1].ID != "ds" || chain[2].ID != "gemini" || chain[3].ID != "glm" {
                t.Fatalf("auto order wrong: %s, %s, %s, %s", chain[0].ID, chain[1].ID, chain[2].ID, chain[3].ID)
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

func TestAliases(t *testing.T) {
        r := NewRegistry("t", nil)
        r.SetBridgeURL("qwen", "http://127.0.0.1:1")
        r.mu.Lock()
        r.providers[0].Models = []string{"qwen3.8-max", "qwen3.7-plus"}
        r.mu.Unlock()

        // alias "smart" → qwen3.8-max on qwen
        if !r.SetAliases("qwen", map[string]string{"smart": "qwen3.8-max"}) {
                t.Fatal("SetAliases on bridge must succeed")
        }
        // prefix form: alias translated
        c := r.Resolve("qwen/smart")
        if len(c) != 1 || c[0].Models[0] != "qwen3.8-max" {
                t.Fatalf("alias prefix resolve broken: %+v", c)
        }
        // plain form: catalog lists alias and resolves to upstream id
        models, _ := r.Catalog()
        found := false
        for _, m := range models {
                if m["id"] == "smart" {
                        found = true
                        if m["alias_for"] != "qwen3.8-max" {
                                t.Fatalf("alias_for wrong: %v", m)
                        }
                }
        }
        if !found {
                t.Fatal("alias missing from catalog")
        }
        c = r.Resolve("smart")
        if len(c) != 1 || c[0].ID != "qwen" || c[0].Models[0] != "qwen3.8-max" {
                t.Fatalf("alias plain resolve broken: %+v", c)
        }
        // OwnerOf
        if r.OwnerOf("smart") != "qwen" {
                t.Fatal("OwnerOf broken for alias")
        }
}

func TestCooldown(t *testing.T) {
        r := NewRegistry("t", nil)
        if r.Cooling("qwen") {
                t.Fatal("fresh registry must not cool")
        }
        r.Cooldown("qwen", 1)
        if !r.Cooling("qwen") {
                t.Fatal("cooldown must be active")
        }
        time.Sleep(1100 * time.Millisecond)
        if r.Cooling("qwen") {
                t.Fatal("cooldown must expire")
        }
}

func TestStoreKeysProvidersStats(t *testing.T) {
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
        k2 := s.CreateKey("test", 2, []string{"qwen/*", "qwen3.8-max"})
        if !s.ValidateKey(k2.Key) {
                t.Fatal("created key must validate")
        }
        // quota: rec.Requests counts prior requests
        s.Flush(true)
        rec, ok := s.LookupKey(k2.Key)
        if !ok {
                t.Fatal("lookup must find key")
        }
        if rec.MaxRequests != 2 {
                t.Fatal("quota not stored")
        }
        if !s.DeleteKey(k2.Key) || s.ValidateKey(k2.Key) {
                t.Fatal("delete must remove key")
        }
        // update quota
        s.UpdateKey(key, nil, &[]string{"gemini/*"})
        rec, _ = s.LookupKey(key)
        if len(rec.AllowedModels) != 1 || rec.AllowedModels[0] != "gemini/*" {
                t.Fatalf("allowlist update broken: %+v", rec.AllowedModels)
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
        // upsert path
        if err := s.UpdateProvider(CustomProvider{Name: "gemini2", BaseURL: "https://z/v1", ModelMap: map[string]string{"fast": "flash"}}); err == nil {
                t.Fatal("update of unknown provider must fail")
        }
        _ = s.AddProvider(CustomProvider{Name: "gemini2", BaseURL: "https://z/v1"})
        if err := s.UpdateProvider(CustomProvider{Name: "gemini2", BaseURL: "https://z2/v1", ModelMap: map[string]string{"fast": "flash"}}); err != nil {
                t.Fatal(err)
        }
        pls := s.ListProviders()
        if len(pls) != 1 || pls[0].ModelMap["fast"] != "flash" {
                t.Fatal("provider model_map round-trip broken")
        }

        // stats
        s.RecordStat("qwen", "qwen3.8-max", 10, 20, false)
        s.RecordStat("qwen", "qwen3.8-max", 1, 2, true)
        s.RecordUsage(key, 10, 20, false)
        st := s.SnapshotStats()
        if st.TotalRequests != 2 || st.TotalErrors != 1 || st.TokensIn != 11 || st.TokensOut != 22 {
                t.Fatalf("stats totals wrong: %+v", st)
        }
        if st.Providers["qwen"].Requests != 2 || st.Models["qwen3.8-max"].Requests != 2 {
                t.Fatal("stats per-provider/model wrong")
        }
        if len(st.Hourly) != 1 || st.Hourly[0].Requests != 2 {
                t.Fatal("hourly buckets wrong")
        }
        s.Flush(true)
}

func TestModelAllowed(t *testing.T) {
        r := NewRegistry("t", nil)
        r.SetBridgeURL("qwen", "http://127.0.0.1:1")
        r.mu.Lock()
        r.providers[0].Models = []string{"qwen3.8-max"}
        r.rebuildCatalogLocked()
        r.mu.Unlock()

        if !modelAllowed(nil, "anything", r) {
                t.Fatal("empty allowlist must allow all")
        }
        rules := []string{"qwen/*", "glm-5"}
        if !modelAllowed(rules, "qwen/qwen3.8-max", r) {
                t.Fatal("provider wildcard must match prefix form")
        }
        if !modelAllowed(rules, "qwen3.8-max", r) {
                t.Fatal("provider wildcard must match plain form via catalog owner")
        }
        if modelAllowed(rules, "ds/deepseek-chat", r) {
                t.Fatal("ds must not pass qwen allowlist")
        }
        if modelAllowed(rules, "glm-5x", r) {
                t.Fatal("exact rule must not prefix-match")
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

func TestExtractUsage(t *testing.T) {
        // OpenAI non-stream
        in, out := extractUsage([]byte(`{"id":"x","usage":{"prompt_tokens":11,"completion_tokens":7}}`))
        if in != 11 || out != 7 {
                t.Fatalf("openai usage wrong: %d/%d", in, out)
        }
        // Anthropic stream: message_start + message_delta
        anth := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","delta":{},"usage":{"output_tokens":42}}

`)
        in, out = extractUsage(anth)
        if in != 25 || out != 42 {
                t.Fatalf("anthropic usage wrong: %d/%d", in, out)
        }
        // OpenAI stream final chunk
        in, out = extractUsage([]byte(`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9}}`))
        if in != 5 || out != 9 {
                t.Fatalf("openai stream usage wrong: %d/%d", in, out)
        }
        if in, out = extractUsage([]byte("no usage here")); in != 0 || out != 0 {
                t.Fatal("empty extraction expected")
        }
}

func TestTailTee(t *testing.T) {
        // simulate a long stream; tail must keep only the end
        chunk := make([]byte, 3000)
        for i := range chunk {
                chunk[i] = 'a'
        }
        tee := &tailTee{r: bytes.NewReader(chunk)}
        buf := make([]byte, 4096)
        tee.Read(buf)
        tee.r = bytes.NewReader(chunk)
        tee.Read(buf)
        tail := string(tee.tail)
        if len(tail) != 6000 {
                t.Fatalf("tail under limit should keep everything, got %d", len(tail))
        }
        big := make([]byte, 20000)
        for i := range big {
                big[i] = 'b'
        }
        tee.r = bytes.NewReader(big)
        tee.Read(buf)
        if len(tee.tail) != 8192 {
                t.Fatalf("tail must cap at 8192, got %d", len(tee.tail))
        }
        if tee.tail[len(tee.tail)-1] != 'b' {
                t.Fatal("tail must keep the newest bytes")
        }
}

func TestBridgeAuthHeaderFallback(t *testing.T) {
        f := newForwarder(nil, nil, "", 1, 20, 300)
        if f.bridgeAuthHeader("glm") != "Bearer Waguri" || f.bridgeAuthHeader("qwen") != "Bearer qwen" {
                t.Fatal("default bridge tokens broken")
        }
        if f.bridgeAuthHeader("gemini") != "Bearer gemini" {
                t.Fatal("gemini default token broken")
        }
        f2 := newForwarder(nil, nil, "shared-secret", 1, 20, 300)
        if f2.bridgeAuthHeader("ds") != "Bearer shared-secret" {
                t.Fatal("shared internal token must win")
        }
}

func TestEstimateTokens(t *testing.T) {
        if EstimateTokens("12345678") != 2 {
                t.Fatal("estimate must be len/4")
        }
        if EstimateTokens("") != 0 {
                t.Fatal("empty must be 0")
        }
}
