// saver_test.go — unit tests for the Token Saver (RTK compression, prompt
// modes) and named combos (9router parity features, v1.2.0).

package core

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func emptyHeader() http.Header { return http.Header{} }

// ---------- RTK compression ----------

func TestRTKCompressesOpenAIToolOutput(t *testing.T) {
	var diff strings.Builder
	for i := 0; i < 40; i++ {
		diff.WriteString("diff --git a/src/file" + string(rune('a'+i%26)) + ".go b/src/file.go\n")
		diff.WriteString("index 1111..2222 100644\n")
		for j := 0; j < 30; j++ {
			diff.WriteString("+added line that goes on and on with context that agents never need again\n")
		}
	}
	body := map[string]interface{}{
		"model": "qwen3.8-max",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "what changed?"},
			{"role": "assistant", "content": "running git diff"},
			{"role": "tool", "tool_call_id": "c1", "content": diff.String()},
		},
	}
	raw, _ := json.Marshal(body)
	out, saved := rtkCompressBody(raw)
	if saved <= 0 {
		t.Fatalf("expected savings, got %d", saved)
	}
	if saved >= int64(len(diff.String())) {
		t.Fatalf("saved %d must be smaller than payload %d", saved, len(diff.String()))
	}
	var check map[string]interface{}
	if json.Unmarshal(out, &check) != nil {
		t.Fatal("output must stay valid JSON")
	}
	msgs := check["messages"].([]interface{})
	tool := msgs[2].(map[string]interface{})
	content := tool["content"].(string)
	if !strings.Contains(content, "[rtk:git-diff") {
		t.Fatalf("compressed tool content should carry the rtk marker, got: %.120s", content)
	}
	if !strings.Contains(content, "files changed:") {
		t.Fatal("git-diff filter should keep the file summary")
	}
}

func TestRTKCompressesAnthropicToolResult(t *testing.T) {
	log := strings.Repeat("2026-01-01 ERROR service crashed again\n", 120)
	body := map[string]interface{}{
		"model":  "glm-5.3",
		"system": "you are helpful",
		"messages": []map[string]interface{}{
			{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": log},
			}},
		},
	}
	raw, _ := json.Marshal(body)
	out, saved := rtkCompressBody(raw)
	if saved <= 0 {
		t.Fatalf("expected savings, got %d", saved)
	}
	var check map[string]interface{}
	_ = json.Unmarshal(out, &check)
	block := check["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if c := block["content"].(string); !strings.Contains(c, "[rtk:") {
		t.Fatalf("anthropic tool_result not compressed: %.80s", c)
	}
}

func TestRTKLeavesSmallPayloadsAlone(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"tool","tool_call_id":"t","content":"short output"}]}`
	_, saved := rtkCompressBody([]byte(body))
	if saved != 0 {
		t.Fatalf("small payload must not be touched, saved=%d", saved)
	}
}

func TestRTKInvalidJSONPassesThrough(t *testing.T) {
	weird := []byte(`{"messages": "not-an-array"}`)
	out, saved := rtkCompressBody(weird)
	if saved != 0 || string(out) != string(weird) {
		t.Fatal("unparseable body must pass through untouched")
	}
}

// ---------- applySaver plumbing ----------

func TestApplySaverBypassHeader(t *testing.T) {
	diff := strings.Repeat("diff --git a/x b/x\n+aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", 60)
	body := `{"model":"m","messages":[{"role":"tool","tool_call_id":"t","content":"` + diff + `"}]}`
	h := http.Header{}
	h.Set("X-Omni-Token-Saver", "off")
	_, saved := applySaver([]byte(body), h)
	if saved != 0 {
		t.Fatalf("bypass header must disable RTK, saved=%d", saved)
	}
}

func TestApplySaverPromptModeOpenAI(t *testing.T) {
	saver.promptMode.Store("caveman")
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	out, _ := applySaver([]byte(body), emptyHeader())
	var check map[string]interface{}
	_ = json.Unmarshal(out, &check)
	msgs := check["messages"].([]interface{})
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "terse") {
		t.Fatalf("caveman system message must be prepended, got %v", first)
	}
}

func TestApplySaverPromptModeAnthropicSystemBlocks(t *testing.T) {
	saver.promptMode.Store("ponytail-full")
	body := `{"model":"m","system":[{"type":"text","text":"be nice"}],"messages":[{"role":"user","content":"hi"}]}`
	out, _ := applySaver([]byte(body), emptyHeader())
	var check map[string]interface{}
	_ = json.Unmarshal(out, &check)
	sys := check["system"].([]interface{})
	if len(sys) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(sys))
	}
	last := sys[1].(map[string]interface{})
	if !strings.Contains(last["text"].(string), "lazy senior developer") {
		t.Fatalf("ponytail instruction missing: %v", last["text"])
	}
}

func TestApplySaverPromptModeAnthropicSystemString(t *testing.T) {
	saver.promptMode.Store("ponytail-lite")
	body := `{"model":"m","system":"be nice","messages":[{"role":"user","content":"hi"}]}`
	out, _ := applySaver([]byte(body), emptyHeader())
	if !strings.Contains(string(out), "lazier alternative") {
		t.Fatalf("string system must gain the ponytail-lite line: %s", string(out))
	}
}

func TestSaverSnapshotDefaults(t *testing.T) {
	snap := SaverSnapshot()
	if rtk, ok := snap["rtk"].(bool); !ok || !rtk {
		t.Fatalf("RTK must default on, got %v", snap["rtk"])
	}
}

// ---------- combos ----------

func TestComboResolve(t *testing.T) {
	reg := NewRegistry("tok", nil)
	reg.SetCombo("my-stack", []string{"qwen/qwen3.8-max", "gemini/gemini-3.6-flash"})
	// wire minimal provider entries so resolvePrefixLocked finds them
	reg.mu.Lock()
	for _, p := range reg.providers {
		p.BaseURL = "http://127.0.0.1:1"
	}
	reg.mu.Unlock()

	chain := reg.Resolve("combo:my-stack")
	if len(chain) != 2 {
		t.Fatalf("combo must expand to 2 candidates, got %d", len(chain))
	}
	if chain[0].Models[0] != "qwen3.8-max" || chain[1].Models[0] != "gemini-3.6-flash" {
		t.Fatalf("combo models wrong: %v %v", chain[0].Models, chain[1].Models)
	}
	// slash form too
	if chain := reg.Resolve("combo/my-stack"); len(chain) != 2 {
		t.Fatalf("combo/ prefix must work, got %d", len(chain))
	}
	// unknown combo → empty
	if chain := reg.Resolve("combo:nothing"); len(chain) != 0 {
		t.Fatal("unknown combo must resolve to empty")
	}
	// catalog exposure
	reg.mu.Lock()
	reg.rebuildCatalogLocked()
	reg.mu.Unlock()
	found := false
	for _, e := range reg.catalog {
		if e.Model == "combo:my-stack" && e.Provider == "combo" {
			found = true
		}
	}
	if !found {
		t.Fatal("combo must appear in the catalog")
	}
}

func TestComboDelete(t *testing.T) {
	reg := NewRegistry("tok", nil)
	reg.SetCombo("tmp", []string{"qwen/qwen3.8-max"})
	if !reg.DeleteCombo("combo:tmp") {
		t.Fatal("delete must find the combo")
	}
	if reg.DeleteCombo("tmp") {
		t.Fatal("second delete must fail")
	}
	if chain := reg.Resolve("combo:tmp"); len(chain) != 0 {
		t.Fatal("deleted combo must not resolve")
	}
}

// ---------- obridge anthropic translation (via core-adjacent unit tests) ----

func TestPromptModeValidation(t *testing.T) {
	for _, m := range []string{"off", "caveman", "ponytail-lite", "ponytail-full", "ponytail-ultra"} {
		if !ValidPromptMode(m) {
			t.Fatalf("mode %s must be valid", m)
		}
	}
	if ValidPromptMode("banana") {
		t.Fatal("banana must be invalid")
	}
}
