package nativeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

func (p *process) claudeControl(id string, request map[string]any) (map[string]any, error) {
	if err := p.send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	for {
		message, err := p.next()
		if err != nil {
			return nil, err
		}
		if text(message["type"]) != "control_response" {
			continue
		}
		var response struct {
			ID      string `json:"request_id"`
			Subtype string
			Error   string
			Result  map[string]any `json:"response"`
		}
		if json.Unmarshal(message["response"], &response) != nil || response.ID != id {
			continue
		}
		if response.Subtype != "success" {
			return nil, fmt.Errorf("native Claude Code rejected %s: %s", request["subtype"], response.Error)
		}
		return response.Result, nil
	}
}

func (p *process) claude(ctx context.Context, body map[string]json.RawMessage, emit func(map[string]any) error) (map[string]any, error) {
	// History and tools use the CLI's extra-body setting. System instructions
	// use native initialization; overriding system here erases the
	// CLI's own prompt and can make Claude-only credentials fail authentication.
	extra := maps.Clone(body)
	delete(extra, "model")
	delete(extra, "system")
	extra["stream"] = raw(true)
	if _, err := p.claudeControl("request-settings", map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{
		"env": map[string]string{"CLAUDE_CODE_EXTRA_BODY": string(raw(extra))},
	}}); err != nil {
		return nil, err
	}
	if p.opts.BeforeSend != nil {
		if err := p.opts.BeforeSend(ctx); err != nil {
			return nil, err
		}
	}
	turn := uuid.NewString()
	if err := p.send(map[string]any{"uuid": turn, "type": "user", "message": map[string]any{"role": "user", "content": "Answer the supplied conversation."}, "parent_tool_use_id": nil, "session_id": ""}); err != nil {
		return nil, err
	}
	var result map[string]any
	var blocks []map[string]any
	arguments := map[int]string{}
	for {
		message, err := p.next()
		if err != nil {
			return nil, err
		}
		if text(message["type"]) == "result" {
			status := http.StatusBadGateway
			_ = json.Unmarshal(message["api_error_status"], &status)
			if status < 400 {
				status = http.StatusBadGateway
			}
			return nil, &clientError{status, "native Claude Code: " + text(message["result"])}
		}
		if text(message["type"]) != "stream_event" {
			continue
		}
		event := object(message["event"])
		if event == nil {
			return nil, errors.New("native Claude Code returned an invalid stream event")
		}
		switch event["type"] {
		case "message_start":
			result, _ = event["message"].(map[string]any)
		case "content_block_start":
			index, _ := event["index"].(float64)
			if int(index) != len(blocks) {
				return nil, errors.New("native Claude Code returned an invalid content index")
			}
			block, _ := event["content_block"].(map[string]any)
			blocks = append(blocks, block)
		case "content_block_delta":
			index, _ := event["index"].(float64)
			if int(index) < 0 || int(index) >= len(blocks) {
				return nil, errors.New("native Claude Code returned a delta without its content block")
			}
			delta, _ := event["delta"].(map[string]any)
			for _, key := range []string{"text", "thinking", "signature"} {
				if value, ok := delta[key].(string); ok {
					before, _ := blocks[int(index)][key].(string)
					blocks[int(index)][key] = before + value
				}
			}
			if value, ok := delta["partial_json"].(string); ok {
				arguments[int(index)] += value
			}
		case "message_delta":
			if result == nil {
				return nil, errors.New("native Claude Code returned usage without its message")
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				for key, value := range delta {
					result[key] = value
				}
			}
			usage, _ := result["usage"].(map[string]any)
			if usage == nil {
				usage = map[string]any{}
			}
			if next, ok := event["usage"].(map[string]any); ok {
				for key, value := range next {
					usage[key] = value
				}
			}
			result["usage"] = usage
		case "error":
			detail, _ := event["error"].(map[string]any)
			return nil, fmt.Errorf("native Claude Code: %v", detail["message"])
		}
		if err := emit(event); err != nil {
			return nil, err
		}
		if event["type"] == "message_stop" {
			if result == nil {
				return nil, errors.New("native Claude Code ended an empty response")
			}
			for index, data := range arguments {
				var input any
				if err := json.Unmarshal([]byte(data), &input); err != nil {
					return nil, errors.New("native Claude Code returned invalid tool arguments")
				}
				blocks[index]["input"] = input
			}
			result["content"] = blocks
			p.reset = func() error {
				for {
					m, e := p.next()
					if e != nil {
						return e
					}
					if text(m["type"]) == "result" {
						break
					}
				}
				reply, e := p.claudeControl("rewind", map[string]any{"subtype": "rewind_conversation", "target_message_uuid": turn, "interrupt_if_running": true})
				if e != nil {
					return e
				}
				if reply["rewound"] != true {
					return errors.New("native Claude Code did not reset its conversation")
				}
				_, e = p.claudeControl("clear-settings", map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"env": map[string]string{"CLAUDE_CODE_EXTRA_BODY": "{}"}}})
				return e
			}
			return result, nil
		}
	}
}

// claudeSystem preserves instruction order while the native CLI owns system
// prompt composition and caching. Message and tool cache controls stay in extra.
func claudeSystem(body map[string]json.RawMessage) (string, error) {
	value := body["system"]
	if len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var prompt string
	if json.Unmarshal(value, &prompt) == nil {
		return prompt, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(value, &blocks) != nil {
		return "", &clientError{http.StatusBadRequest, "native Claude Code requires system text or text blocks"}
	}
	var parts []string
	for _, block := range blocks {
		var blockText *string
		if text(block["type"]) != "text" || json.Unmarshal(block["text"], &blockText) != nil || blockText == nil {
			return "", &clientError{http.StatusBadRequest, "native Claude Code requires system text blocks"}
		}
		for key := range block {
			if key != "type" && key != "text" && key != "cache_control" {
				return "", &clientError{http.StatusBadRequest, fmt.Sprintf("native Claude Code cannot use system field %q", key)}
			}
		}
		parts = append(parts, *blockText)
	}
	return strings.Join(parts, "\n\n"), nil
}
