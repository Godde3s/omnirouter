// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Qwen bridge core (package gbridge).

package gbridge

import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "strings"
    "sync"
    "time"
)

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
        model = "gemini-3.6-flash"
    }

    var messages []Message
    if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
        writeJSON(w, 400, formatOpenAIError("messages is required and must be an array", "invalid_request_error", nil))
        return
    }

    // ── Multi-account: pick the account serving this request. Blocks
    // (bounded queue) while every account is rate-limited; nil account =
    // guest flow (no cookies needed — verified live).
    acc, accErr := acquireAccountForRequest(r.Context())
    if accErr != nil {
        writeJSON(w, 503, formatOpenAIError(accErr.Error(), "rate_limit_error", "account_pool_exhausted"))
        return
    }

    // ── Vision: extract image_url parts and upload them to Google's
    // content-push endpoint under the SAME identity that will serve the
    // completion. cleanedMessages is byte-identical to body.Messages when
    // the request carries no images (the common case).
    cleanedMessages, uploads, vErr := processVisionMessagesAs(r.Context(), acc, body.Messages)
    if vErr != nil {
        writeJSON(w, 400, formatOpenAIError(vErr.Error(), "invalid_request_error", nil))
        return
    }
    if len(uploads) > 0 {
        // Re-parse the cleaned (text-only) messages for prompt building.
        var localMsgs []Message
        if err := json.Unmarshal(cleanedMessages, &localMsgs); err == nil {
            messages = localMsgs
        }
    }

    stream := false
    if body.Stream != nil {
        stream = *body.Stream
    }

    // Stateless request: the Gemini web generate call runs with the
    // temporary-chat flag (inner[45]=1), so nothing lands in the account
    // history and no session cleanup is needed at all.
    requestId := generateID()

    // ── Agent mode: transform tools & roles for Qwen compatibility ──
    // Modern shim (default): one XML-sectioned prompt in a single user message.
    // Legacy shim: [ROLE: ...] rewritten user messages + tool contract message.
    // Operates on the cleaned (image-stripped) messages.
    var transformedMessages json.RawMessage = cleanedMessages
    if config.AgentMode {
        if tm, err := agentTransformMessages(cleanedMessages, body.Tools); err == nil {
            transformedMessages = tm
            // Re-parse so local `messages` reflects the rewritten content
            var localMsgs []Message
            if err := json.Unmarshal(tm, &localMsgs); err == nil {
                messages = localMsgs
            }
        } else {
            logError("agent transform failed: " + err.Error())
        }
    }

    prompt := messagesToPrompt(messages)

    // Features are resolved from the model registry; per-request overrides
    // only set if explicitly provided in the body.
    opts := SendOptions{
        Model:             model,
        ClientMessagesRaw: transformedMessages,
        ReasoningEffort:   body.ReasoningEffort,
        Account:           acc,
        FileData:          fileDataFromUploads(uploads),
    }

    // Parse thinking configuration:
    //   reasoning: true/false  ->  enable_thinking
    //   "thinking": {"type":"enabled"|"disabled"}  ->  enable_thinking
    if body.Reasoning != nil {
        opts.Thinking = body.Reasoning
    } else if len(body.Thinking) > 0 {
        var thinkCfg struct {
            Type string `json:"type"`
        }
        if err := json.Unmarshal(body.Thinking, &thinkCfg); err == nil {
            enabled := thinkCfg.Type == "enabled"
            opts.Thinking = &enabled
        }
    }

    if body.WebSearch != nil {
        opts.WebSearch = body.WebSearch
    } else if body.Search != nil {
        opts.WebSearch = body.Search
    }
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
            log.Printf("[Stream] Error: %s", err.Error())
            writeSSE(toJSON(formatOpenAIError(err.Error(), "api_error", statusFromError(err.Error()))))
            writeSSE("[DONE]")
            errored = true
        } else {
            for result := range ch {
                if result.Err != nil {
                    log.Printf("[Stream] Error: %s", result.Err.Error())
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
                if result.FullText != "" && !strings.HasPrefix(result.FullText, fullContent) {
                    // A deep edit_content rewrite rewound text that was
                    // already forwarded: the agent interceptor's view of
                    // the stream is stale, reset it (issue #23).
                    if interceptor != nil {
                        interceptor = newAgentInterceptor()
                    }
                }
                if result.FullText != "" {
                    fullContent = result.FullText
                } else {
                    fullContent += result.Chunk
                }

                // The parser emits the exact rune-safe delta to forward.
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
                // Drain the interceptor tail: trailing text plus any
                // tool call whose block only completed at end of stream
                // (the modern shim holds back a window while streaming).
                rem, tailCalls := interceptor.finish()
                if rem != "" && !toolCallEmitted {
                    c := formatOpenAIResponse(ResponseResult{Content: rem}, model, requestId, true)
                    writeSSE(toJSON(c))
                }
                for _, tc := range tailCalls {
                    emitToolCallDelta(tc)
                    toolCallEmitted = true
                }

                // Safety net: fallback tool call extraction at stream end
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
            log.Printf("[API] Error: %s", err.Error())
            writeJSON(w, statusFromError(err.Error()), formatOpenAIError(err.Error(), "api_error", nil))
            return
        }

        fullContent := ""
        fullReasoning := ""
        for result := range ch {
            if result.Err != nil {
                log.Printf("[API] Error: %s", result.Err.Error())
                writeJSON(w, statusFromError(result.Err.Error()), formatOpenAIError(result.Err.Error(), "api_error", nil))
                return
            }
            if result.Reasoning != "" {
                fullReasoning += result.Reasoning
                continue
            }
            if result.FullText != "" {
                fullContent = result.FullText
            } else {
                fullContent += result.Chunk
            }
        }

        // Agent-mode: parse out tool-call blocks for non-stream response
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

func statsHandler(w http.ResponseWriter, r *http.Request) {
    session.mu.Lock()
    initialized := session.Initialized
    session.mu.Unlock()

    totalClients := 0
    if initialized {
        totalClients = 1
    }

    writeJSON(w, 200, map[string]interface{}{
        "mode":         "direct",
        "totalClients": totalClients,
        "stats": map[string]interface{}{
            "totalRequests": 0,
        },
    })
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
    session.mu.Lock()
    healthy := session.Initialized
    session.mu.Unlock()

    // Account mode: the pool is healthy when at least one account can serve
    // a request right now (guest/session init is then irrelevant).
    poolOK := accounts != nil && accounts.HealthyCount() > 0
    if poolOK {
        healthy = true
    }

    status := 200
    if !healthy {
        status = 503
    }
    body := map[string]interface{}{"healthy": healthy, "mode": "direct"}
    if accounts != nil {
        body["accounts"] = map[string]interface{}{
            "size":    accounts.Len(),
            "healthy": accounts.HealthyCount(),
        }
    }
    writeJSON(w, status, body)
}

func clientsHandler(w http.ResponseWriter, r *http.Request) {
    session.mu.Lock()
    initialized := session.Initialized
    session.mu.Unlock()

    var clients []map[string]interface{}
    if initialized {
        clients = []map[string]interface{}{
            {"id": "session", "status": "idle"},
        }
    } else {
        clients = []map[string]interface{}{}
    }
    writeJSON(w, 200, map[string]interface{}{"clients": clients})
}

func injectHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"message":"Direct mode"}`))
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, 200, map[string]interface{}{
        "success": true,
        "message": "Stop acknowledged",
    })
}

