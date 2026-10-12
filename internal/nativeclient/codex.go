package nativeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func codexArguments(opts Options, body map[string]json.RawMessage, env []string, home string) ([]string, []string, error) {
	if err := validateCodex(body); err != nil {
		return nil, nil, err
	}
	args := []string{"--no-daemon", "app-server", "--listen", "stdio://", "--strict-config"}
	config := map[string]any{
		"model_provider":  "magpie_custom",
		"model_providers": map[string]any{"magpie_custom": map[string]any{"name": "Magpie custom provider", "base_url": opts.Base, "wire_api": "responses", "env_key": "MAGPIE_NATIVE_API_KEY", "request_max_retries": 0, "stream_max_retries": 0, "http_headers": opts.Headers}},
		"features":        map[string]bool{"shell_tool": false, "apply_patch_freeform": false, "multi_agent": false, "goals": false, "collaboration_modes": false},
		"tools":           map[string]any{"experimental_request_user_input": map[string]bool{"enabled": false}, "update_plan": map[string]bool{"enabled": false}},
		"web_search":      "disabled", "project_doc_max_bytes": 0,
	}
	// One private config keeps credentials off process arguments. Strict
	// parsing makes an upstream configuration change fail before inference.
	configFile, err := toml.Marshal(config)
	if err != nil {
		return nil, nil, err
	}
	if err = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), configFile, 0600); err != nil {
		return nil, nil, err
	}
	env = append(env, "MAGPIE_NATIVE_API_KEY="+opts.Key)
	return args, env, nil
}

func validateCodex(body map[string]json.RawMessage) error {
	// A Responses API field is accepted only if the native protocol below
	// can express it. Unknown values must not silently disappear.
	known := map[string]bool{"model": true, "stream": true, "input": true, "instructions": true, "tools": true, "tool_choice": true, "reasoning": true, "text": true, "store": true, "include": true, "service_tier": true, "parallel_tool_calls": true}
	var missing []string
	for field := range body {
		if !known[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return unsupported(missing)
	}
	if string(body["store"]) == "true" {
		return unsupported([]string{"store=true"})
	}
	if choice := text(body["tool_choice"]); choice != "" && choice != "auto" && choice != "none" {
		return unsupported([]string{"tool_choice=" + choice})
	}
	if len(body["tool_choice"]) > 0 && text(body["tool_choice"]) == "" {
		return unsupported([]string{"tool_choice"})
	}
	if string(body["parallel_tool_calls"]) == "false" {
		return unsupported([]string{"parallel_tool_calls=false"})
	}
	var include []string
	_ = json.Unmarshal(body["include"], &include)
	for _, value := range include {
		if value != "reasoning.encrypted_content" {
			return unsupported([]string{"include=" + value})
		}
	}
	format := object(body["text"])
	if f, ok := format["format"].(map[string]any); ok {
		if f["type"] != "text" && f["type"] != "json_schema" {
			return unsupported([]string{"text.format.type=" + fmt.Sprint(f["type"])})
		}
	}
	return nil
}

func (p *process) rpc(id int, method string, params any) (map[string]json.RawMessage, error) {
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		message, err := p.next()
		if err != nil {
			return nil, err
		}
		if string(message["id"]) != fmt.Sprint(id) {
			continue
		}
		if value := object(message["error"]); value != nil {
			return nil, fmt.Errorf("native Codex rejected %s: %v", method, value["message"])
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(message["result"], &result); err != nil {
			return nil, err
		}
		return result, nil
	}
}

func (p *process) codex(ctx context.Context, body map[string]json.RawMessage, emit func(map[string]any) error) (map[string]any, error) {
	var tools []map[string]json.RawMessage
	if len(body["tools"]) > 0 && json.Unmarshal(body["tools"], &tools) != nil {
		return nil, errors.New("native Codex requires an array of tools")
	}
	dynamic := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if text(tool["type"]) != "function" {
			return nil, unsupported([]string{"tools.type=" + text(tool["type"])})
		}
		if text(body["tool_choice"]) != "none" {
			dynamic = append(dynamic, map[string]any{"type": "function", "name": text(tool["name"]), "description": text(tool["description"]), "inputSchema": tool["parameters"]})
		}
	}
	config := map[string]any{}
	reasoning := object(body["reasoning"])
	if value, ok := reasoning["effort"].(string); ok {
		config["model_reasoning_effort"] = value
	}
	if value, ok := reasoning["summary"].(string); ok {
		config["model_reasoning_summary"] = value
	}
	format := object(body["text"])
	if value, ok := format["verbosity"].(string); ok {
		config["model_verbosity"] = value
	}
	started, err := p.rpc(2, "thread/start", map[string]any{"model": text(body["model"]), "modelProvider": "magpie_custom", "ephemeral": true, "experimentalRawEvents": true, "approvalPolicy": "never", "sandbox": "read-only", "environments": []any{}, "dynamicTools": dynamic, "developerInstructions": text(body["instructions"]), "config": config})
	if err != nil {
		return nil, err
	}
	thread := object(started["thread"])
	tid, _ := thread["id"].(string)
	if tid == "" {
		return nil, errors.New("native Codex did not create a thread")
	}
	input, err := codexInput(body["input"])
	if err != nil {
		return nil, err
	}
	if _, err := p.rpc(3, "thread/inject_items", map[string]any{"threadId": tid, "items": input}); err != nil {
		return nil, err
	}
	turn := map[string]any{"threadId": tid, "input": []any{}}
	if tier := text(body["service_tier"]); tier != "" {
		turn["serviceTier"] = tier
	}
	if f, ok := format["format"].(map[string]any); ok && f["type"] == "json_schema" {
		turn["outputSchema"] = f["schema"]
	}
	if p.opts.BeforeSend != nil {
		if err := p.opts.BeforeSend(ctx); err != nil {
			return nil, err
		}
	}
	startedTurn, err := p.rpc(4, "turn/start", turn)
	if err != nil {
		return nil, err
	}
	turnID, _ := object(startedTurn["turn"])["id"].(string)
	if turnID == "" {
		return nil, errors.New("native Codex did not start a turn")
	}
	result := map[string]any{"id": "resp_" + tid, "object": "response", "created_at": time.Now().Unix(), "model": text(body["model"]), "status": "in_progress", "output": []any{}}
	output := []any{}
	indices := map[string]int{}
	opened := false
	open := func() error {
		if opened {
			return nil
		}
		opened = true
		return emit(map[string]any{"type": "response.created", "response": result})
	}
	for {
		message, err := p.next()
		if err != nil {
			return nil, err
		}
		params := object(message["params"])
		if id, _ := params["turnId"].(string); id != "" && id != turnID {
			continue
		}
		switch text(message["method"]) {
		case "error":
			detail, _ := params["error"].(map[string]any)
			return nil, codexError(detail)
		case "item/agentMessage/delta":
			if err := open(); err != nil {
				return nil, err
			}
			id, _ := params["itemId"].(string)
			index, exists := indices[id]
			if !exists {
				index = len(output)
				indices[id] = index
				output = append(output, nil)
				if err := emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
					return nil, err
				}
				if err := emit(map[string]any{"type": "response.content_part.added", "item_id": id, "output_index": index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}); err != nil {
					return nil, err
				}
			}
			if err := emit(map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": index, "content_index": 0, "delta": params["delta"]}); err != nil {
				return nil, err
			}
		case "rawResponseItem/completed":
			item, _ := params["item"].(map[string]any)
			if item["type"] == "message" && item["role"] != "assistant" {
				continue
			}
			if err := open(); err != nil {
				return nil, err
			}
			delete(item, "internal_chat_message_metadata_passthrough")
			id, _ := item["id"].(string)
			index, exists := indices[id]
			if !exists {
				index = len(output)
				indices[id] = index
				output = append(output, item)
				initial := maps.Clone(item)
				if initial["type"] == "message" {
					initial["content"], initial["status"] = []any{}, "in_progress"
				}
				if err := emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": initial}); err != nil {
					return nil, err
				}
			} else {
				output[index] = item
			}
			content, _ := item["content"].([]any)
			for n, value := range content {
				part, _ := value.(map[string]any)
				if part["type"] != "output_text" {
					continue
				}
				fields := func(kind string, more map[string]any) map[string]any {
					more["type"], more["item_id"], more["output_index"], more["content_index"] = kind, id, index, n
					return more
				}
				// The native CLI can complete a whole message without deltas.
				// Give both paths the same content-part lifecycle.
				if !exists || n > 0 {
					if err := emit(fields("response.content_part.added", map[string]any{"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})); err != nil {
						return nil, err
					}
					if err := emit(fields("response.output_text.delta", map[string]any{"delta": part["text"]})); err != nil {
						return nil, err
					}
				}
				if err := emit(fields("response.output_text.done", map[string]any{"text": part["text"], "logprobs": []any{}})); err != nil {
					return nil, err
				}
				if err := emit(fields("response.content_part.done", map[string]any{"part": part})); err != nil {
					return nil, err
				}
			}
			if err := emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}); err != nil {
				return nil, err
			}
		case "rawResponse/completed":
			usage, _ := params["usage"].(map[string]any)
			result["usage"] = map[string]any{"input_tokens": usage["inputTokens"], "output_tokens": usage["outputTokens"], "total_tokens": usage["totalTokens"], "input_tokens_details": map[string]any{"cached_tokens": usage["cachedInputTokens"]}, "output_tokens_details": map[string]any{"reasoning_tokens": usage["reasoningOutputTokens"]}}
			result["status"], result["output"] = "completed", output
			if err := open(); err != nil {
				return nil, err
			}
			if err := emit(map[string]any{"type": "response.completed", "response": result}); err != nil {
				return nil, err
			}
			// Dynamic tool calls are returned to the API caller. Never let
			// the agent execute them or make another inference on its own.
			reusable := true
			for _, v := range output {
				if item, ok := v.(map[string]any); ok && item["type"] == "function_call" {
					reusable = false
				}
			}
			if reusable {
				p.reset = func() error {
					for {
						m, e := p.next()
						if e != nil {
							return e
						}
						params := object(m["params"])
						if text(m["method"]) == "turn/completed" {
							turn, _ := params["turn"].(map[string]any)
							if turn["id"] == turnID {
								break
							}
						}
					}
					_, e := p.rpc(5, "thread/unsubscribe", map[string]any{"threadId": tid})
					return e
				}
			}
			return result, nil
		case "turn/completed":
			return nil, errors.New("native Codex ended without a complete Responses result")
		}
	}
}

// Responses accepts easy input messages with no type and string content;
// thread/inject_items requires explicit ResponseItems. Preserve tool/reasoning
// items and typed media blocks while expanding just that shorthand.
func codexInput(input json.RawMessage) (json.RawMessage, error) {
	if value := text(input); value != "" {
		input = raw([]any{map[string]any{"role": "user", "content": value}})
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, &clientError{http.StatusBadRequest, "native Codex requires text or an input array"}
	}
	for _, item := range items {
		if text(item["type"]) == "" && text(item["role"]) != "" {
			item["type"] = raw("message")
		}
		if text(item["type"]) != "message" {
			continue
		}
		var value string
		if json.Unmarshal(item["content"], &value) == nil {
			kind := "input_text"
			if text(item["role"]) == "assistant" {
				kind = "output_text"
			}
			item["content"] = raw([]any{map[string]string{"type": kind, "text": value}})
		}
	}
	return raw(items), nil
}

func codexError(detail map[string]any) error {
	status := http.StatusBadGateway
	if info, ok := detail["codexErrorInfo"].(map[string]any); ok {
		for _, variant := range info {
			fields, _ := variant.(map[string]any)
			if code, ok := fields["httpStatusCode"].(float64); ok && code >= 400 && code <= 599 && code == float64(int(code)) {
				status = int(code)
			}
		}
	} else {
		switch detail["codexErrorInfo"] {
		case "rateLimitExceeded", "usageLimitExceeded":
			status = http.StatusTooManyRequests
		case "unauthorized":
			status = http.StatusUnauthorized
		case "badRequest", "contextWindowExceeded":
			status = http.StatusBadRequest
		}
	}
	return &clientError{status, fmt.Sprintf("native Codex: %v", detail["message"])}
}
