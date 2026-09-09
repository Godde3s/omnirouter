// anthropic.go — Anthropic /v1/messages ⇄ OpenAI Zen translation for obridge.
//
// The Zen upstream is OpenAI-native, so unlike the other bridges (which
// translate from proprietary web formats) the work here is protocol
// translation only:
//
//      request:  Anthropic messages/system/tools → OpenAI messages/tools
//      response: OpenAI chat.completion (incl. reasoning_content) → Anthropic
//                content blocks (thinking / text / tool_use)
//      stream:   OpenAI chat.completion.chunk SSE → Anthropic SSE events
//                (message_start, content_block_start/delta/stop, message_delta,
//                message_stop)
//
// Zen emits thinking → text → tool_calls in order (verified live), which
// keeps the streaming state machine a simple sequential block counter.

package obridge

import (
        "bufio"
        "bytes"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "strings"
        "time"
)

// ---------- request: Anthropic → OpenAI ----------

func anthropicToOpenAIRequest(bodyBytes []byte) ([]byte, error) {
        var req map[string]interface{}
        if err := json.Unmarshal(bodyBytes, &req); err != nil {
                return nil, err
        }
        out := map[string]interface{}{}
        if m, ok := req["model"]; ok {
                out["model"] = m
        }
        if s, ok := req["stream"]; ok {
                out["stream"] = s
        }
        if mt, ok := req["max_tokens"]; ok {
                out["max_tokens"] = mt
        }
        if t, ok := req["temperature"]; ok {
                out["temperature"] = t
        }
        if t, ok := req["top_p"]; ok {
                out["top_p"] = t
        }
        if ss, ok := req["stop_sequences"]; ok {
                out["stop"] = ss
        }

        var messages []map[string]interface{}
        if sys, ok := req["system"]; ok {
                if s := anthropicTextOf(sys); s != "" {
                        messages = append(messages, map[string]interface{}{"role": "system", "content": s})
                }
        }

        if msgs, ok := req["messages"].([]interface{}); ok {
                for _, m := range msgs {
                        mm, ok := m.(map[string]interface{})
                        if !ok {
                                continue
                        }
                        role, _ := mm["role"].(string)
                        content := mm["content"]

                        // tool_result blocks → OpenAI tool messages
                        if arr, ok := content.([]interface{}); ok {
                                hasToolResult := false
                                for _, item := range arr {
                                        mp, ok := item.(map[string]interface{})
                                        if !ok {
                                                continue
                                        }
                                        if t, _ := mp["type"].(string); t == "tool_result" {
                                                hasToolResult = true
                                                id, _ := mp["tool_use_id"].(string)
                                                messages = append(messages, map[string]interface{}{
                                                        "role":         "tool",
                                                        "tool_call_id": id,
                                                        "content":      anthropicTextOf(mp["content"]),
                                                })
                                        }
                                }
                                if hasToolResult {
                                        continue
                                }
                        }

                        // assistant tool_use blocks → OpenAI tool_calls
                        if role == "assistant" {
                                if arr, ok := content.([]interface{}); ok {
                                        var text []string
                                        var calls []map[string]interface{}
                                        for _, item := range arr {
                                                mp, ok := item.(map[string]interface{})
                                                if !ok {
                                                        continue
                                                }
                                                switch t, _ := mp["type"].(string); t {
                                                case "text":
                                                        if s, ok := mp["text"].(string); ok {
                                                                text = append(text, s)
                                                        }
                                                case "tool_use":
                                                        id, _ := mp["id"].(string)
                                                        name, _ := mp["name"].(string)
                                                        args := mp["input"]
                                                        if args == nil {
                                                                args = map[string]interface{}{}
                                                        }
                                                        argsJSON, _ := json.Marshal(args)
                                                        calls = append(calls, map[string]interface{}{
                                                                "id":   id,
                                                                "type": "function",
                                                                "function": map[string]interface{}{
                                                                        "name":      name,
                                                                        "arguments": string(argsJSON),
                                                                },
                                                        })
                                                }
                                        }
                                        msg := map[string]interface{}{"role": "assistant", "content": strings.Join(text, "\n")}
                                        if len(calls) > 0 {
                                                msg["tool_calls"] = calls
                                        }
                                        messages = append(messages, msg)
                                        continue
                                }
                        }

                        messages = append(messages, map[string]interface{}{"role": role, "content": anthropicTextOf(content)})
                }
        }
        out["messages"] = messages

        if tools, ok := req["tools"].([]interface{}); ok && len(tools) > 0 {
                var oai []map[string]interface{}
                for _, t := range tools {
                        tm, ok := t.(map[string]interface{})
                        if !ok {
                                continue
                        }
                        name, _ := tm["name"].(string)
                        desc, _ := tm["description"].(string)
                        fn := map[string]interface{}{"name": name, "description": desc}
                        if is, ok := tm["input_schema"]; ok {
                                fn["parameters"] = is
                        }
                        oai = append(oai, map[string]interface{}{"type": "function", "function": fn})
                }
                out["tools"] = oai
        }
        if tc, ok := req["tool_choice"]; ok {
                if s, ok := tc.(string); ok {
                        switch s {
                        case "auto":
                                out["tool_choice"] = "auto"
                        case "any":
                                out["tool_choice"] = "required"
                        case "none":
                                out["tool_choice"] = "none"
                        }
                }
        }
        return json.Marshal(out)
}

// anthropicTextOf flattens an Anthropic content field (string or block array)
// into plain text.
func anthropicTextOf(content interface{}) string {
        switch v := content.(type) {
        case string:
                return v
        case []interface{}:
                var parts []string
                for _, item := range v {
                        if mp, ok := item.(map[string]interface{}); ok {
                                switch t, _ := mp["type"].(string); t {
                                case "text":
                                        if s, ok := mp["text"].(string); ok {
                                                parts = append(parts, s)
                                        }
                                case "tool_result":
                                        parts = append(parts, anthropicTextOf(mp["content"]))
                                }
                        } else if s, ok := item.(string); ok {
                                parts = append(parts, s)
                        }
                }
                return strings.Join(parts, "\n")
        }
        return ""
}

// ---------- response: OpenAI → Anthropic (non-stream) ----------

func stopReasonFromOpenAI(fr string) string {
        switch fr {
        case "length":
                return "max_tokens"
        case "tool_calls", "function_call":
                return "tool_use"
        default:
                return "end_turn"
        }
}

func openAIToAnthropicResponse(raw []byte, model string) map[string]interface{} {
        var oai struct {
                ID      string `json:"id"`
                Choices []struct {
                        FinishReason string `json:"finish_reason"`
                        Message      struct {
                                Content          interface{} `json:"content"`
                                ReasoningContent string      `json:"reasoning_content"`
                                ToolCalls        []struct {
                                        ID       string `json:"id"`
                                        Function struct {
                                                Name      string `json:"name"`
                                                Arguments string `json:"arguments"`
                                        } `json:"function"`
                                } `json:"tool_calls"`
                        } `json:"message"`
                } `json:"choices"`
                Usage struct {
                        PromptTokens     int64 `json:"prompt_tokens"`
                        CompletionTokens int64 `json:"completion_tokens"`
                } `json:"usage"`
        }
        _ = json.Unmarshal(raw, &oai)

        var blocks []map[string]interface{}
        var text strings.Builder
        stop := "end_turn"
        if len(oai.Choices) > 0 {
                c := oai.Choices[0]
                stop = stopReasonFromOpenAI(c.FinishReason)
                if c.Message.ReasoningContent != "" {
                        blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": c.Message.ReasoningContent})
                }
                if s, ok := c.Message.Content.(string); ok {
                        text.WriteString(s)
                }
                for _, tc := range c.Message.ToolCalls {
                        var input interface{} = map[string]interface{}{}
                        if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
                                input = map[string]interface{}{}
                        }
                        blocks = append(blocks, map[string]interface{}{
                                "type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
                        })
                }
        }
        blocks = append(blocks, map[string]interface{}{"type": "text", "text": text.String()})

        in := oai.Usage.PromptTokens
        if in == 0 {
                in = int64(len(raw) / 4)
        }
        return map[string]interface{}{
                "id":            "msg_oc_" + strings.TrimPrefix(oai.ID, "chatcmpl-"),
                "type":          "message",
                "role":          "assistant",
                "model":         model,
                "content":       blocks,
                "stop_reason":   stop,
                "stop_sequence": nil,
                "usage": map[string]interface{}{
                        "input_tokens":  in,
                        "output_tokens": oai.Usage.CompletionTokens,
                },
        }
}

func anthropicError(errType, message string) map[string]interface{} {
        return map[string]interface{}{
                "type": "error",
                "error": map[string]interface{}{
                        "type":    errType,
                        "message": message,
                },
        }
}

func mapAnthropicStatus(code int) int {
        if code >= 400 {
                return code
        }
        return 502
}

// ---------- handler ----------

func anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
        if r.Method != "POST" {
                http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                return
        }
        body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
        if err != nil {
                writeJSON(w, 400, anthropicError("invalid_request_error", "failed to read body"))
                return
        }
        var probe struct {
                Model  string `json:"model"`
                Stream bool   `json:"stream"`
        }
        _ = json.Unmarshal(body, &probe)

        converted, err := anthropicToOpenAIRequest(body)
        if err != nil {
                writeJSON(w, 400, anthropicError("invalid_request_error", "invalid Anthropic request: "+err.Error()))
                return
        }

        if probe.Stream {
                anthropicStreamResponse(w, r, converted, probe.Model)
                return
        }

        // Non-stream: force JSON upstream, translate the full body once.
        var payload map[string]json.RawMessage
        if json.Unmarshal(converted, &payload) == nil {
                payload["stream"], _ = json.Marshal(false)
                converted, _ = json.Marshal(payload)
        }
        resp, err := zenPost(r, converted, "/chat/completions")
        if err != nil {
                writeJSON(w, 502, anthropicError("api_error", "OpenCode Zen upstream unreachable: "+err.Error()))
                return
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
        if resp.StatusCode != 200 {
                writeJSON(w, mapAnthropicStatus(resp.StatusCode), anthropicError("api_error",
                        fmt.Sprintf("OpenCode Zen HTTP %d: %s", resp.StatusCode, snippet(raw))))
                return
        }
        writeJSON(w, 200, openAIToAnthropicResponse(raw, probe.Model))
}

func snippet(b []byte) string {
        s := strings.TrimSpace(string(b))
        if len(s) > 240 {
                s = s[:240] + "…"
        }
        return s
}

// zenPost posts a body to the Zen upstream with session/key enrichment.
func zenPost(r *http.Request, body []byte, path string) (*http.Response, error) {
        req, err := http.NewRequestWithContext(r.Context(), "POST", cfg.BaseURL+path, bytes.NewReader(body))
        if err != nil {
                return nil, err
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "application/json")
        req.Header.Set("x-session-id", sessionFor(r))
        req.Header.Set("x-opencode-session", sessionFor(r))
        if cfg.APIKey != "" {
                req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
        }
        return httpClient.Do(req)
}

// ---------- SSE: OpenAI chunks → Anthropic events ----------

type sseDecoder struct {
        sc *bufio.Scanner
}

func newSSEDecoder(r io.Reader) *sseDecoder {
        sc := bufio.NewScanner(r)
        sc.Buffer(make([]byte, 64*1024), 4<<20)
        return &sseDecoder{sc: sc}
}

func (d *sseDecoder) next() (string, bool) {
        for d.sc.Scan() {
                line := strings.TrimRight(d.sc.Text(), "\r")
                if strings.HasPrefix(line, "data:") {
                        return line, true
                }
        }
        return "", false
}

func anthropicStreamResponse(w http.ResponseWriter, r *http.Request, converted []byte, model string) {
        var payload map[string]json.RawMessage
        if json.Unmarshal(converted, &payload) == nil {
                payload["stream"], _ = json.Marshal(true)
                converted, _ = json.Marshal(payload)
        }

        resp, err := zenPost(r, converted, "/chat/completions")
        if err != nil {
                writeJSON(w, 502, anthropicError("api_error", "OpenCode Zen upstream unreachable: "+err.Error()))
                return
        }
        defer resp.Body.Close()
        if resp.StatusCode != 200 {
                raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
                writeJSON(w, mapAnthropicStatus(resp.StatusCode), anthropicError("api_error",
                        fmt.Sprintf("OpenCode Zen HTTP %d: %s", resp.StatusCode, snippet(raw))))
                return
        }

        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("X-Accel-Buffering", "no")
        flusher, _ := w.(http.Flusher)

        send := func(v map[string]interface{}) {
                b, _ := json.Marshal(v)
                fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
                if flusher != nil {
                        flusher.Flush()
                }
        }

        send(map[string]interface{}{
                "type": "message_start",
                "message": map[string]interface{}{
                        "id": fmt.Sprintf("msg_oc_%d", time.Now().UnixNano()), "type": "message",
                        "role": "assistant", "model": model,
                        "content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
                        "usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
                },
        })

        // Sequential block machine: 0=thinking (when seen), 1=text, then
        // one block per tool_call index. Zen order is thinking → text → tools.
        const blockThinkingIdx = 0
        const blockTextIdx = 1
        thinkingOpen, textOpen := false, false
        nextToolIdx := 2
        toolNames := map[int]string{}
        var outTok int64
        stop := "end_turn"

        closeOpen := func() {
                if textOpen {
                        send(map[string]interface{}{"type": "content_block_stop", "index": blockTextIdx})
                        textOpen = false
                }
                if thinkingOpen {
                        send(map[string]interface{}{"type": "content_block_stop", "index": blockThinkingIdx})
                        thinkingOpen = false
                }
        }

        dec := newSSEDecoder(resp.Body)
        for {
                line, ok := dec.next()
                if !ok {
                        break
                }
                data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
                if data == "" || data == "[DONE]" {
                        if data == "[DONE]" {
                                break
                        }
                        continue
                }
                var chunk struct {
                        Choices []struct {
                                FinishReason *string `json:"finish_reason"`
                                Delta        struct {
                                        Content          *string `json:"content"`
                                        ReasoningContent *string `json:"reasoning_content"`
                                        ToolCalls        []struct {
                                                Index    int    `json:"index"`
                                                ID       string `json:"id"`
                                                Function struct {
                                                        Name      string `json:"name"`
                                                        Arguments string `json:"arguments"`
                                                } `json:"function"`
                                        } `json:"tool_calls"`
                                } `json:"delta"`
                        } `json:"choices"`
                        Usage *struct {
                                CompletionTokens int64 `json:"completion_tokens"`
                        } `json:"usage"`
                }
                if json.Unmarshal([]byte(data), &chunk) != nil {
                        continue
                }
                if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
                        outTok = chunk.Usage.CompletionTokens
                }
                if len(chunk.Choices) == 0 {
                        continue
                }
                if chunk.Choices[0].FinishReason != nil {
                        stop = stopReasonFromOpenAI(*chunk.Choices[0].FinishReason)
                }
                d := chunk.Choices[0].Delta

                if d.ReasoningContent != nil && *d.ReasoningContent != "" {
                        if !thinkingOpen {
                                thinkingOpen = true
                                send(map[string]interface{}{
                                        "type":          "content_block_start",
                                        "index":         blockThinkingIdx,
                                        "content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
                                })
                        }
                        send(map[string]interface{}{
                                "type":  "content_block_delta",
                                "index": blockThinkingIdx,
                                "delta": map[string]interface{}{"type": "thinking_delta", "thinking": *d.ReasoningContent},
                        })
                        continue
                }
                if d.Content != nil && *d.Content != "" {
                        if thinkingOpen && !textOpen {
                                // thinking finished; close it before opening the text block
                                send(map[string]interface{}{"type": "content_block_stop", "index": blockThinkingIdx})
                                thinkingOpen = false
                        }
                        if !textOpen {
                                textOpen = true
                                send(map[string]interface{}{
                                        "type":          "content_block_start",
                                        "index":         blockTextIdx,
                                        "content_block": map[string]interface{}{"type": "text", "text": ""},
                                })
                        }
                        send(map[string]interface{}{
                                "type":  "content_block_delta",
                                "index": blockTextIdx,
                                "delta": map[string]interface{}{"type": "text_delta", "text": *d.Content},
                        })
                        continue
                }
                for _, tc := range d.ToolCalls {
                        if textOpen || thinkingOpen {
                                closeOpen()
                        }
                        idx := nextToolIdx + tc.Index
                        if _, seen := toolNames[idx]; !seen {
                                toolNames[idx] = tc.Function.Name
                                send(map[string]interface{}{
                                        "type":  "content_block_start",
                                        "index": idx,
                                        "content_block": map[string]interface{}{
                                                "type": "tool_use", "id": tc.ID, "name": tc.Function.Name,
                                                "input": map[string]interface{}{},
                                        },
                                })
                        }
                        if args := tc.Function.Arguments; args != "" {
                                send(map[string]interface{}{
                                        "type":  "content_block_delta",
                                        "index": idx,
                                        "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
                                })
                        }
                }
        }
        if textOpen {
                send(map[string]interface{}{"type": "content_block_stop", "index": blockTextIdx})
        }
        if thinkingOpen {
                send(map[string]interface{}{"type": "content_block_stop", "index": blockThinkingIdx})
        }
        for idx := range toolNames {
                send(map[string]interface{}{"type": "content_block_stop", "index": idx})
        }
        send(map[string]interface{}{
                "type":  "message_delta",
                "delta": map[string]interface{}{"stop_reason": stop, "stop_sequence": nil},
                "usage": map[string]interface{}{"output_tokens": outTok},
        })
        send(map[string]interface{}{"type": "message_stop"})
}
