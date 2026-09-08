// dsbridge_test.go — unit tests for the DeepSeek bridge internals.

package dsbridge

import (
	"strings"
	"testing"
)

func feed(t *testing.T, lines []string) []UpstreamResult {
	t.Helper()
	ch := make(chan UpstreamResult, 64)
	go func() {
		parseDSStream(strings.NewReader(strings.Join(lines, "\n")+"\n"), ch)
		close(ch)
	}()
	var out []UpstreamResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestParseDSStreamSnapshotAndAppends(t *testing.T) {
	events := feed(t, []string{
		`data: {"v":{"response":{"fragments":[{"type":"THINKING","content":"فکر "},{"type":"RESPONSE","content":"سلام "}]},"message_id":42}}`,
		`data: {"p":"response/fragments/1/content","o":"APPEND","v":"دنیا"}`,
		`data: {"v":"!"}`,
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":" بیشتر"}`,
		`data: [DONE]`,
	})
	var content, reasoning string
	for _, e := range events {
		if e.Err != nil {
			t.Fatalf("unexpected err: %v", e.Err)
		}
		reasoning += e.Reasoning
		content += e.Chunk
	}
	if content != "سلام دنیا! بیشتر" {
		t.Fatalf("content mismatch: %q", content)
	}
	if reasoning != "فکر " {
		t.Fatalf("reasoning mismatch: %q", reasoning)
	}
}

func TestParseDSStreamThinkingPath(t *testing.T) {
	events := feed(t, []string{
		`data: {"v":{"response":{"fragments":[{"type":"RESPONSE"},{"type":"THINKING"}]}}}`,
		`data: {"p":"response/fragments/1/content","o":"APPEND","v":"عمیق فکر می‌کنم"}`,
	})
	if len(events) != 1 || events[0].Reasoning != "عمیق فکر می‌کنم" {
		t.Fatalf("thinking path must yield reasoning: %+v", events)
	}
}

func TestParseDSStreamAPIError(t *testing.T) {
	events := feed(t, []string{`data: {"code":401,"msg":"unauthorized"}`})
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("api error frame must surface as Err: %+v", events)
	}
}

func TestResolveModel(t *testing.T) {
	mt, think := resolveModel("deepseek-chat")
	if mt != "default" || think {
		t.Fatalf("chat: %s %v", mt, think)
	}
	mt, think = resolveModel("deepseek-reasoner")
	if mt != "default" || !think {
		t.Fatalf("reasoner: %s %v", mt, think)
	}
	mt, _ = resolveModel("deepseek-expert")
	if mt != "expert" {
		t.Fatalf("expert: %s", mt)
	}
	if isKnownModel("gpt-4") {
		t.Fatal("unknown model must not be known")
	}
}

func TestParseTokensEnv(t *testing.T) {
	toks := parseTokensEnv("a, b;;c \n d,a")
	if len(toks) != 4 {
		t.Fatalf("expected 4 unique tokens, got %v", toks)
	}
}

func TestPowSolverInstantiates(t *testing.T) {
	p, err := getPow()
	if err != nil {
		t.Fatalf("PoW wasm must instantiate: %v", err)
	}
	if p == nil || p.solveFn == nil || p.memory == nil {
		t.Fatal("PoW exports missing")
	}
}
