// fbridge_test.go — unit tests for the free-agent catalog parser.

package fbridge

import (
	"strings"
	"testing"
)

const sampleAgentsTS = `export const FREE_MODE_AGENT_MODELS: Record<string, Set<string>> = {
  'base2-free': new Set([
    FREEBUFF_MINIMAX_M3_MODEL_ID,
    'openai/gpt-5.6-luna',
    FREEBUFF_DEEPSEEK_V41_FLASH_MODEL_ID,
  ]),
  'base2-free-glm-5-3-flash': new Set([FREEBUFF_GLM_V53_FLASH_MODEL_ID]),
  'base2-free-luna-es': new Set([FREEBUFF_GPT_5_6_LUNA_ES_MODEL_ID]),
}`

const sampleModelsTS = `export const FREEBUFF_GLM_V53_FLASH_MODEL_ID =
  'z-ai/glm-5.3-flash'
export const FREEBUFF_DEEPSEEK_V41_FLASH_MODEL_ID =
  'deepseek/deepseek-v4.1-flash'
export const FREEBUFF_GPT_5_6_LUNA_ES_MODEL_ID = 'openai/gpt-5.6-luna-es'`

func TestMergeIDLiterals(t *testing.T) {
	ids := mergeIDLiterals(map[string]string{}, sampleModelsTS)
	want := map[string]string{
		"FREEBUFF_GLM_V53_FLASH_MODEL_ID":      "z-ai/glm-5.3-flash",
		"FREEBUFF_DEEPSEEK_V41_FLASH_MODEL_ID": "deepseek/deepseek-v4.1-flash",
		"FREEBUFF_GPT_5_6_LUNA_ES_MODEL_ID":    "openai/gpt-5.6-luna-es",
	}
	for k, v := range want {
		if ids[k] != v {
			t.Fatalf("mergeIDLiterals[%q] = %q, want %q", k, ids[k], v)
		}
	}
}

func TestFindAgentModelPairs(t *testing.T) {
	ids := mergeIDLiterals(map[string]string{}, sampleModelsTS)
	pairs := findAgentModelPairs(sampleAgentsTS, ids)
	got := map[string]string{}
	for _, p := range pairs {
		got[p[0]] = p[1]
	}
	expect := map[string]string{
		"openai/gpt-5.6-luna":          "base2-free",
		"z-ai/glm-5.3-flash":           "base2-free-glm-5-3-flash",
		"deepseek/deepseek-v4.1-flash": "base2-free",
		"openai/gpt-5.6-luna-es":       "base2-free-luna-es",
	}
	for m, a := range expect {
		if got[m] != a {
			t.Fatalf("model %q → agent %q, want %q (got %v)", m, got[m], a, got)
		}
	}
}

func TestClientSessionIDShape(t *testing.T) {
	id := clientSessionID()
	if len(id) != 13 {
		t.Fatalf("clientSessionID len = %d, want 13", len(id))
	}
	const alpha = "0123456789abcdefghijklmnopqrstuvwxyz"
	for _, c := range id {
		if !strings.ContainsRune(alpha, c) {
			t.Fatalf("clientSessionID has non-base36 char %q", c)
		}
	}
}

func TestTokensFromEnv(t *testing.T) {
	t.Setenv("FREEBUFF_TOKENS", " a ,\nb;;c ")
	got := TokensFromEnv()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("TokensFromEnv = %v", got)
	}
	t.Setenv("FREEBUFF_TOKENS", "   ")
	if TokensFromEnv() != nil {
		t.Fatal("empty env must yield nil")
	}
}

func TestSetTokensDedups(t *testing.T) {
	SetTokens([]string{"x", "x", " y "})
	all := pools.snapshot()
	if len(all) != 2 {
		t.Fatalf("pools = %d, want 2 (dedup)", len(all))
	}
	if all[1].token != "y" {
		t.Fatalf("second token = %q, want %q", all[1].token, "y")
	}
}
