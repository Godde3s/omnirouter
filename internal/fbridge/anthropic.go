// anthropic.go — /v1/messages for the Freebuff bridge.
//
// The core proxy forwards Anthropic-protocol bodies to a provider's
// /v1/messages path untouched. Freebuff's upstream only speaks OpenAI, so
// this file translates both directions: Anthropic request → OpenAI body →
// Freebuff pipeline → OpenAI response/SSE → Anthropic response/SSE.
//
// Coverage: system prompts, multi-turn text messages, roles, max_tokens,
// temperature, stop_sequences and full SSE streaming of text deltas.
// Tool declarations are passed through as OpenAI tools; tool_use blocks in
// responses are translated for non-stream replies and surfaced as plain
// text deltas when streaming (Freebuff's free roots are chat-first, so
// tool-heavy Anthropic clients should use the OpenAI path instead).

package fbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------- Anthropic request types ----------

type anthropicReq struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []anthropicMsg  `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop_sequences,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		anthErr(w, 400, "failed to read body")
		return
	}
	var areq anthropicReq
	if json.Unmarshal(raw, &areq) != nil {
		anthErr(w, 400, "invalid JSON body")
		return
	}

	openaiBody := map[string]interface{}{
		"model":    areq.Model,
		"messages": anthropToOpenAIMessages(&areq),
		"stream":   areq.Stream,
	}
	if areq.MaxTokens > 0 {
		openaiBody["max_tokens"] = areq.MaxTokens
	}
	if areq.Temperature != nil {
		openaiBody["temperature"] = *areq.Temperature
	}
	if areq.TopP != nil {
		openaiBody["top_p"] = *areq.TopP
	}
	if len(areq.Stop) > 0 {
		openaiBody["stop"] = areq.Stop
	}
	if sys := anthropSystem(&areq); sys != "" {
		openaiBody["system"] = sys
	}
	if len(areq.Tools) > 0 {
		openaiBody["tools"] = json.RawMessage(areq.Tools)
	}

	body, err := json.Marshal(openaiBody)
	if err != nil {
		anthErr(w, 500, "encode request: "+err.Error())
		return
	}

	// Reuse the OpenAI pipeline verbatim: same sessions, runs, metadata.
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	if areq.Stream {
		r.Header.Set("Accept", "text/event-stream")
		// Capture the OpenAI SSE and re-emit as Anthropic SSE.
		capture := &sseCapture{ResponseWriter: w}
		chatHandler(capture, r)
		if capture.wrote {
			return // headers already gone out — nothing more to do
		}
		return
	}

	rec := &recorder{header: http.Header{}}
	chatHandler(rec, r)
	translateRecordedToAnthropic(w, rec, areq.Model)
}

// anthropToOpenAIMessages maps Anthropic content blocks → OpenAI messages.
func anthropToOpenAIMessages(a *anthropicReq) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(a.Messages)+1)
	for _, m := range a.Messages {
		var blocks []map[string]interface{}
		if json.Unmarshal(m.Content, &blocks) != nil {
			// Plain string content.
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				out = append(out, map[string]interface{}{"role": m.Role, "content": s})
			}
			continue
		}
		var text strings.Builder
		toolUses := []map[string]interface{}{}
		toolResults := []map[string]interface{}{}
		for _, b := range blocks {
			switch bt, _ := b["type"].(string); bt {
			case "text":
				if s, ok := b["text"].(string); ok {
					text.WriteString(s)
				}
			case "tool_use":
				toolUses = append(toolUses, b)
			case "tool_result":
				toolResults = append(toolResults, b)
			}
		}
		if len(toolResults) > 0 {
			for _, tr := range toolResults {
				content := ""
				if c, ok := tr["content"].(string); ok {
					content = c
				} else if cb, ok := tr["content"].([]interface{}); ok {
					var sb strings.Builder
					for _, item := range cb {
						if im, ok := item.(map[string]interface{}); ok {
							if t, ok := im["text"].(string); ok {
								sb.WriteString(t)
							}
						}
					}
					content = sb.String()
				}
				out = append(out, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": tr["tool_use_id"],
					"content":      content,
				})
			}
		} else if len(toolUses) > 0 {
			calls := make([]map[string]interface{}, 0, len(toolUses))
			for _, tu := range toolUses {
				calls = append(calls, map[string]interface{}{
					"id":   tu["id"],
					"type": "function",
					"function": map[string]interface{}{
						"name":      tu["name"],
						"arguments": mustJSON(tu["input"]),
					},
				})
			}
			msg := map[string]interface{}{"role": m.Role, "content": text.String()}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		} else {
			out = append(out, map[string]interface{}{"role": m.Role, "content": text.String()})
		}
	}
	return out
}

// anthropSystem flattens the Anthropic system field (string or blocks).
func anthropSystem(a *anthropicReq) string {
	if len(a.System) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(a.System, &s) == nil {
		return s
	}
	var blocks []map[string]interface{}
	if json.Unmarshal(a.System, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// ---------- non-stream response translation ----------

type recorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(status int)      { r.status = status }

func (r *recorder) contentType() string {
	if r.header.Get("Content-Type") == "" {
		return "application/json"
	}
	return r.header.Get("Content-Type")
}

func translateRecordedToAnthropic(w http.ResponseWriter, rec *recorder, model string) {
	ct := rec.contentType()
	if strings.Contains(ct, "text/event-stream") {
		// Upstream streamed although we asked for JSON — join the chunks.
		text := sseJoinText(rec.body.String())
		writeAnthropicText(w, model, text)
		return
	}
	var oai struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(rec.body.String()), &oai) != nil || len(oai.Choices) == 0 {
		anthErr(w, statusOr(rec.status, 502), "Freebuff upstream error: "+clipStr(rec.body.String()))
		return
	}
	msg := oai.Choices[0].Message
	content := []map[string]interface{}{}
	if msg.Content != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		var input json.RawMessage
		if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
			input = json.RawMessage(`{}`)
		}
		content = append(content, map[string]interface{}{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	stopReason := "end_turn"
	if fr := oai.Choices[0].FinishReason; fr == "tool_calls" || fr == "function_call" {
		stopReason = "tool_use"
	} else if fr == "length" {
		stopReason = "max_tokens"
	}
	writeJSON(w, 200, map[string]interface{}{
		"id":            "msg_" + clientSessionID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  oai.Usage.PromptTokens,
			"output_tokens": oai.Usage.CompletionTokens,
		},
	})
}

func writeAnthropicText(w http.ResponseWriter, model, text string) {
	writeJSON(w, 200, map[string]interface{}{
		"id": "msg_" + clientSessionID(), "type": "message", "role": "assistant", "model": model,
		"content":       []map[string]interface{}{{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
	})
}

func anthErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"type": "error", "error": map[string]string{"type": "api_error", "message": msg},
	})
}

func statusOr(got, def int) int {
	if got > 0 {
		return got
	}
	return def
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func clipStr(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// ---------- streaming translation ----------

// sseCapture tees an OpenAI SSE stream and re-emits Anthropic SSE events.
type sseCapture struct {
	http.ResponseWriter
	wrote        bool
	ended        bool
	blockStarted bool
}

// sseDatas splits an SSE chunk into its data payloads (multi-line data
// fields are joined with \n, per the SSE spec).
func sseDatas(chunk string) []string {
	var out []string
	for _, block := range strings.Split(chunk, "\n\n") {
		var lines []string
		for _, ln := range strings.Split(block, "\n") {
			ln = strings.TrimSuffix(ln, "\r")
			if strings.HasPrefix(ln, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(ln, "data:"), " "))
			}
		}
		if len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return out
}

// sseJoinText concatenates every text delta of a captured OpenAI SSE stream
// (used when the upstream streams despite a JSON Accept header).
func sseJoinText(raw string) string {
	var sb strings.Builder
	for _, data := range sseDatas(raw) {
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		for _, ch := range ev.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	return sb.String()
}

func (c *sseCapture) WriteHeader(status int) {
	if !c.wrote {
		c.wrote = true
		c.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		c.ResponseWriter.Header().Set("Cache-Control", "no-cache")
		c.ResponseWriter.Header().Set("X-Accel-Buffering", "no")
		c.ResponseWriter.WriteHeader(status)
		c.emitMessageStart()
	}
}

func (c *sseCapture) Write(p []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(200)
	}
	c.feed(string(p))
	return len(p), nil
}

func (c *sseCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *sseCapture) emitMessageStart() {
	if c.blockStarted {
		return
	}
	fmt.Fprintf(c.ResponseWriter, "event: message_start\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": "msg_" + clientSessionID(), "type": "message", "role": "assistant",
			"model": "", "content": []interface{}{}, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	}))
	c.blockStarted = true
	fmt.Fprintf(c.ResponseWriter, "event: content_block_start\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}))
}

func (c *sseCapture) feed(chunk string) {
	for _, data := range sseDatas(chunk) {
		if data == "[DONE]" {
			c.emitMessageEnd()
			return
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		for _, ch := range ev.Choices {
			if ch.Delta.Content != "" {
				fmt.Fprintf(c.ResponseWriter, "event: content_block_delta\ndata: %s\n\n", mustJSON(map[string]interface{}{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]interface{}{"type": "text_delta", "text": ch.Delta.Content},
				}))
				c.Flush()
			}
			if ch.FinishReason != nil {
				c.emitMessageEnd()
				return
			}
		}
	}
}

func (c *sseCapture) emitMessageEnd() {
	if c.ended {
		return
	}
	c.ended = true
	fmt.Fprintf(c.ResponseWriter, "event: content_block_stop\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "content_block_stop", "index": 0,
	}))
	fmt.Fprintf(c.ResponseWriter, "event: message_delta\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 0},
	}))
	fmt.Fprintf(c.ResponseWriter, "event: message_stop\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "message_stop",
	}))
	c.Flush()
}
