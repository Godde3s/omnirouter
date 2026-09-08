// handlers.go — OpenAI + Anthropic HTTP surface for the DeepSeek bridge.
// Mirrors the Qwen bridge handler flow (agent shim, SSE, tool deltas) minus
// vision and the session pool: DeepSeek runs one throwaway session per call.

package dsbridge

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "strings"
        "sync"
        "time"
)

// processVisionMessagesAs — DeepSeek bridge has no vision support; the stub
// keeps the shared Anthropic handler code byte-identical to the other bridges.
func processVisionMessagesAs(ctx context.Context, acc *Account, raw json.RawMessage) (json.RawMessage, []string, error) {
        return raw, nil, nil
}

// AcquireStatelessSession / ReleaseStatelessSession — DeepSeek is stateless
// per request and its sessions are left server-side (no delete endpoint used
// by the reference client), so these are identity stubs that keep the shared
// Anthropic handler compiling and behaving identically.
func AcquireStatelessSession(ctx context.Context) (string, bool, error) {
        return generateID(), false, nil
}

func ReleaseStatelessSession(chatID string, pooled bool) {}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
        if r.Method != "POST" {
                http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                return
        }

        var body struct {
                Model           string          `json:"model"`
                Messages        json.RawMessage `json:"messages"`
                Stream          *bool           `json:"stream"`
                Reasoning       *bool           `json:"reasoning"`
                Thinking        json.RawMessage `json:"thinking"`
                WebSearch       *bool           `json:"webSearch"`
                Search          *bool           `json:"search"`
                Tools           json.RawMessage `json:"tools"`
                ToolChoice      json.RawMessage `json:"tool_choice"`
                ReasoningEffort string          `json:"reasoning_effort"`
        }
        bodyBytes, err := io.ReadAll(r.Body)
        if err != nil {
                writeJSON(w, 400, formatOpenAIError("Failed to read body", "invalid_request_error", nil))
                return
        }
        if err := json.Unmarshal(bodyBytes, &body); err != nil {
                writeJSON(w, 400, formatOpenAIError("Invalid JSON", "invalid_request_error", nil))
                return
        }

        model := body.Model
        if model == "" {
                model = "deepseek-chat"
        }
        modelType, thinkDefault := resolveModel(model)
        if modelType == "" {
                writeJSON(w, 404, formatOpenAIError(
                        fmt.Sprintf("The model `%s` does not exist. Available models: deepseek-chat, deepseek-reasoner, deepseek-expert", model),
                        "model_not_found", nil))
                return
        }

        var messages []Message
        if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
                writeJSON(w, 400, formatOpenAIError("messages is required and must be an array", "invalid_request_error", nil))
                return
        }

        acc, accErr := acquireAccountForRequest(r.Context())
        if accErr != nil {
                errType := "rate_limit_error"
                if statusFromError(accErr.Error()) == 401 {
                        errType = "authentication_error"
                }
                writeJSON(w, statusFromError(accErr.Error()), formatOpenAIError(accErr.Error(), errType, "account_pool_exhausted"))
                return
        }

        stream := true
        if body.Stream != nil {
                stream = *body.Stream
        }

        // Agent mode: transform tools & roles (modern XML shim by default).
        transformedMessages := body.Messages
        if config.AgentMode {
                if tm, err := agentTransformMessages(body.Messages, body.Tools); err == nil {
                        transformedMessages = tm
                        var localMsgs []Message
                        if err := json.Unmarshal(tm, &localMsgs); err == nil {
                                messages = localMsgs
                        }
                } else {
                        logError("agent transform failed: " + err.Error())
                }
        }

        prompt := messagesToPrompt(messages)

        // Thinking resolution: explicit reasoning/thinking fields win, then the
        // model default (deepseek-reasoner ⇒ true).
        thinking := thinkDefault
        if body.Reasoning != nil {
                thinking = *body.Reasoning
        } else if len(body.Thinking) > 0 {
                var thinkCfg struct {
                        Type string `json:"type"`
                }
                if err := json.Unmarshal(body.Thinking, &thinkCfg); err == nil && thinkCfg.Type != "" {
                        thinking = thinkCfg.Type == "enabled"
                }
        }

        opts := SendOptions{
                Model:             modelType,
                Thinking:          &thinking,
                WebSearch:         boolPtr(false),
                ClientMessagesRaw: transformedMessages,
                Account:           acc,
        }
        if body.WebSearch != nil {
                opts.WebSearch = body.WebSearch
        } else if body.Search != nil {
                opts.WebSearch = body.Search
        }

        requestId := generateID()

        if stream {
                w.Header().Set("Content-Type", "text/event-stream")
                w.Header().Set("Cache-Control", "no-cache")
                w.Header().Set("Connection", "keep-alive")
                w.Header().Set("X-Accel-Buffering", "no")

                flusher, _ := w.(http.Flusher)
                var writeMu sync.Mutex

                writeSSE := func(data string) {
                        writeMu.Lock()
                        defer writeMu.Unlock()
                        fmt.Fprintf(w, "data: %s\n\n", data)
                        if flusher != nil {
                                flusher.Flush()
                        }
                }

                initChunk := formatOpenAIResponse(ResponseResult{Content: ""}, model, requestId, true)
                writeSSE(toJSON(initChunk))

                fullContent := ""
                fullReasoning := ""

                var interceptor agentInterceptor
                if config.AgentMode {
                        interceptor = newAgentInterceptor()
                }
                toolCallEmitted := false

                emitToolCallDelta := func(tc map[string]interface{}) {
                        chunk := map[string]interface{}{
                                "id":      "chatcmpl-" + requestId,
                                "object":  "chat.completion.chunk",
                                "created": time.Now().Unix(),
                                "model":   model,
                                "choices": []map[string]interface{}{
                                        {
                                                "index":         0,
                                                "delta":         map[string]interface{}{"tool_calls": []map[string]interface{}{tc}},
                                                "finish_reason": nil,
                                        },
                                },
                        }
                        writeSSE(toJSON(chunk))
                }

                keepAliveStop := make(chan struct{})
                var wg sync.WaitGroup
                wg.Add(1)
                go func() {
                        defer wg.Done()
                        ticker := time.NewTicker(5 * time.Second)
                        defer ticker.Stop()
                        for {
                                select {
                                case <-ticker.C:
                                        ka := formatOpenAIResponse(ResponseResult{Content: ""}, model, requestId, true)
                                        writeSSE(toJSON(ka))
                                case <-keepAliveStop:
                                        return
                                }
                        }
                }()

                errored := false
                ch, err := sendUpstreamWithFailover(prompt, opts)
                if err != nil {
                        logError("[Stream] " + err.Error())
                        writeSSE(toJSON(formatOpenAIError(err.Error(), "api_error", statusFromError(err.Error()))))
                        writeSSE("[DONE]")
                        errored = true
                } else {
                        for result := range ch {
                                if result.Err != nil {
                                        logError("[Stream] " + result.Err.Error())
                                        writeSSE(toJSON(formatOpenAIError(result.Err.Error(), "api_error", statusFromError(result.Err.Error()))))
                                        writeSSE("[DONE]")
                                        errored = true
                                        break
                                }

                                if result.Reasoning != "" {
                                        fullReasoning += result.Reasoning
                                        rChunk := map[string]interface{}{
                                                "id":      "chatcmpl-" + requestId,
                                                "object":  "chat.completion.chunk",
                                                "created": time.Now().Unix(),
                                                "model":   model,
                                                "choices": []map[string]interface{}{
                                                        {
                                                                "index":         0,
                                                                "delta":         map[string]interface{}{"reasoning_content": result.Reasoning},
                                                                "finish_reason": nil,
                                                        },
                                                },
                                        }
                                        writeSSE(toJSON(rChunk))
                                        continue
                                }
                                fullContent += result.Chunk

                                delta := result.Chunk
                                if delta == "" {
                                        continue
                                }

                                if interceptor != nil {
                                        contentDelta, toolCalls := interceptor.feed(delta)
                                        if contentDelta != "" {
                                                c := formatOpenAIResponse(ResponseResult{Content: contentDelta}, model, requestId, true)
                                                writeSSE(toJSON(c))
                                        }
                                        for _, tc := range toolCalls {
                                                emitToolCallDelta(tc)
                                                toolCallEmitted = true
                                        }
                                } else {
                                        c := formatOpenAIResponse(ResponseResult{Content: delta}, model, requestId, true)
                                        writeSSE(toJSON(c))
                                }
                        }
                }

                if !errored {
                        if interceptor != nil {
                                rem, tailCalls := interceptor.finish()
                                if rem != "" && !toolCallEmitted {
                                        c := formatOpenAIResponse(ResponseResult{Content: rem}, model, requestId, true)
                                        writeSSE(toJSON(c))
                                }
                                for _, tc := range tailCalls {
                                        emitToolCallDelta(tc)
                                        toolCallEmitted = true
                                }

                                if !toolCallEmitted {
                                        fallbackCalls := agentExtractToolCalls(fullContent)
                                        if len(fallbackCalls) > 0 {
                                                for _, tc := range fallbackCalls {
                                                        emitToolCallDelta(tc)
                                                }
                                                toolCallEmitted = true
                                        }
                                }

                                if toolCallEmitted {
                                        finalChunk := map[string]interface{}{
                                                "id":      "chatcmpl-" + requestId,
                                                "object":  "chat.completion.chunk",
                                                "created": time.Now().Unix(),
                                                "model":   model,
                                                "choices": []map[string]interface{}{
                                                        {
                                                                "index":         0,
                                                                "delta":         map[string]interface{}{},
                                                                "finish_reason": "tool_calls",
                                                        },
                                                },
                                        }
                                        writeSSE(toJSON(finalChunk))
                                } else {
                                        finalChunk := formatOpenAIResponse(ResponseResult{Content: "", FinishReason: "stop"}, model, requestId, true)
                                        writeSSE(toJSON(finalChunk))
                                }
                        } else {
                                finalChunk := formatOpenAIResponse(ResponseResult{Content: "", FinishReason: "stop"}, model, requestId, true)
                                writeSSE(toJSON(finalChunk))
                        }
                        writeSSE("[DONE]")
                }

                close(keepAliveStop)
                wg.Wait()

        } else {
                ch, err := sendUpstreamWithFailover(prompt, opts)
                if err != nil {
                        logError("[API] " + err.Error())
                        writeJSON(w, statusFromError(err.Error()), formatOpenAIError(err.Error(), "api_error", nil))
                        return
                }

                fullContent := ""
                fullReasoning := ""
                for result := range ch {
                        if result.Err != nil {
                                logError("[API] " + result.Err.Error())
                                writeJSON(w, statusFromError(result.Err.Error()), formatOpenAIError(result.Err.Error(), "api_error", nil))
                                return
                        }
                        if result.Reasoning != "" {
                                fullReasoning += result.Reasoning
                                continue
                        }
                        fullContent += result.Chunk
                }

                if config.AgentMode {
                        toolCalls := agentExtractToolCalls(fullContent)
                        if len(toolCalls) > 0 {
                                stripped := agentStripToolCalls(fullContent)
                                writeJSON(w, 200, map[string]interface{}{
                                        "id":      "chatcmpl-" + requestId,
                                        "object":  "chat.completion",
                                        "created": time.Now().Unix(),
                                        "model":   model,
                                        "choices": []map[string]interface{}{
                                                {
                                                        "index": 0,
                                                        "message": func() map[string]interface{} {
                                                                m := map[string]interface{}{
                                                                        "role":       "assistant",
                                                                        "content":    stripped,
                                                                        "tool_calls": toolCalls,
                                                                }
                                                                if fullReasoning != "" {
                                                                        m["reasoning_content"] = fullReasoning
                                                                }
                                                                return m
                                                        }(),
                                                        "finish_reason": "tool_calls",
                                                },
                                        },
                                        "usage": map[string]interface{}{
                                                "prompt_tokens":     estimateTokens(prompt),
                                                "completion_tokens": estimateTokens(fullContent),
                                                "total_tokens":      estimateTokens(prompt) + estimateTokens(fullContent),
                                        },
                                })
                                return
                        }
                }

                writeJSON(w, 200, formatOpenAIResponse(ResponseResult{Content: fullContent, Reasoning: fullReasoning}, model, requestId, false))
        }
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
        healthy := accounts != nil && accounts.HealthyCount() > 0
        status := 200
        if !healthy {
                status = 503
        }
        body := map[string]interface{}{"healthy": healthy, "mode": "accounts"}
        if accounts != nil {
                body["accounts"] = map[string]interface{}{
                        "size":    accounts.Len(),
                        "healthy": accounts.HealthyCount(),
                }
        }
        writeJSON(w, status, body)
}

func clientsHandler(w http.ResponseWriter, r *http.Request) {
        writeJSON(w, 200, map[string]interface{}{"clients": []map[string]interface{}{}})
}

func injectHandler(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.Write([]byte(`{"message":"Direct mode"}`))
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
        writeJSON(w, 200, map[string]interface{}{"success": true, "message": "Stop acknowledged"})
}

var _ = strings.TrimSpace
