package nativeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeRequest(t *testing.T, client string, body map[string]any, status int) (int, map[string]json.RawMessage) {
	t.Helper()
	if liveBinDir == "" {
		t.Skip("set MAGPIE_TEST_NATIVE_BIN_DIR for installed CLI checks")
	}
	seen := make(chan map[string]json.RawMessage, 1)
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		select {
		case seen <- request:
		default:
		}
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"synthetic provider rate limit","type":"rate_limit_error"}}`)
			return
		}
		answer(w, client, text(request["model"]))
	}))
	defer srv.Close()
	name := "codex"
	if client == Claude {
		name = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Do(ctx, Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: srv.URL + "/v1", Key: "synthetic-native-review", Proxy: "direct"}, raw(body))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Logf("adapter status %d: %s", res.StatusCode, data)
	}
	if got := sent.Load(); got != 1 {
		t.Fatalf("one inference sent %d model requests", got)
	}
	select {
	case request := <-seen:
		return res.StatusCode, request
	default:
		t.Fatal("no native request")
		return 0, nil
	}
}

func TestNativeCodexKeepsNoTools(t *testing.T) {
	_, request := nativeRequest(t, Codex, map[string]any{"model": "gpt-5.6", "input": "Hi", "tool_choice": "none", "tools": []any{}}, 200)
	var ts []map[string]any
	json.Unmarshal(request["tools"], &ts)
	var names []string
	for _, tool := range ts {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	if len(ts) > 0 && text(request["tool_choice"]) != "none" {
		t.Fatalf("caller disabled tools; native tool_choice=%s tools=%v", request["tool_choice"], names)
	}
}

func TestNativeCodexKeepsErrorStatus(t *testing.T) {
	for _, want := range []int{401, 403, 404, 429, 503} {
		t.Run(http.StatusText(want), func(t *testing.T) {
			status, _ := nativeRequest(t, Codex, map[string]any{"model": "gpt-5.6", "input": "Hi"}, want)
			if status != want {
				t.Fatalf("provider returned %d; adapter returned %d", want, status)
			}
		})
	}
}

func TestNativeCodexRefusesUnrepresentableFormatBeforeStartingCLI(t *testing.T) {
	res, err := Do(context.Background(), Options{Client: Codex, Executable: "must-not-start", Base: "http://127.0.0.1:1"},
		[]byte(`{"model":"gpt-5.6","input":"Return JSON","text":{"format":{"type":"json_object"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "text.format.type=json_object") {
		t.Fatalf("unsupported format was not refused before startup: status=%d body=%s error=%v", res.StatusCode, data, err)
	}
}

func TestNativeClaudeKeepsErrorStatus(t *testing.T) {
	for _, want := range []int{401, 403, 429, 503} {
		t.Run(http.StatusText(want), func(t *testing.T) {
			status, _ := nativeRequest(t, Claude, map[string]any{"model": "claude-haiku-5-5", "messages": []any{map[string]string{"role": "user", "content": "Hi"}}, "max_tokens": 64}, want)
			if status != want {
				t.Fatalf("provider returned %d; adapter returned %d", want, status)
			}
		})
	}
}

func TestNativeToolsStayWithCaller(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			var sent atomic.Int32
			seen := make(chan []byte, 4)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				seen <- b
				n := sent.Add(1)
				if n > 1 {
					answer(w, client, "test-model")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				event := func(v map[string]any) {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], raw(v))
					w.(http.Flusher).Flush()
				}
				if client == Claude {
					event(map[string]any{"type": "message_start", "message": map[string]any{"id": "tool-message", "type": "message", "role": "assistant", "model": "claude-haiku-5-5", "content": []any{}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0}}})
					event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call_external", "name": "external_tool", "input": map[string]any{}}})
					event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"value":"arg"}`}})
					event(map[string]any{"type": "content_block_stop", "index": 0})
					event(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 5}})
					event(map[string]any{"type": "message_stop"})
					return
				}
				item := map[string]any{"type": "function_call", "id": "fc_external", "call_id": "call_external", "name": "external_tool", "arguments": `{"value":"arg"}`, "status": "completed"}
				response := map[string]any{"id": "resp_tool", "object": "response", "status": "in_progress", "output": []any{}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
				event(map[string]any{"type": "response.created", "response": response})
				event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
				event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				response["status"], response["output"] = "completed", []any{item}
				event(map[string]any{"type": "response.completed", "response": response})
			}))
			defer up.Close()
			pool := &Pool{}
			defer pool.Close()
			name, model := "codex", "gpt-5.6"
			if client == Claude {
				name, model = "claude", "claude-haiku-5-5"
			}
			schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}}, "required": []string{"value"}, "additionalProperties": false}
			history := []any{map[string]any{"role": "user", "content": "Use external_tool"}}
			body := map[string]any{"model": model, "input": history, "tools": []any{map[string]any{"type": "function", "name": "external_tool", "parameters": schema}}}
			if client == Claude {
				body = map[string]any{"model": model, "messages": history, "max_tokens": 64, "tools": []any{map[string]any{"name": "external_tool", "input_schema": schema}}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			opts := Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: up.URL + "/v1", Key: "synthetic-key", Proxy: "direct", Pool: pool, Scope: "tool-session"}
			for step := 0; step < 2; step++ {
				res, err := Do(ctx, opts, raw(body))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("tool step %d: %d %s %v", step, res.StatusCode, data, err)
				}
				if step == 0 && (!strings.Contains(string(data), "call_external") || !strings.Contains(string(data), "arg")) {
					t.Fatalf("tool call lost: %s", data)
				}
				wire := <-seen
				if step == 1 && !strings.Contains(string(wire), "CALLER_TOOL_RESULT_END") {
					t.Fatal("caller tool result lost")
				}
				if client == Claude {
					body["messages"] = append(history, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_external", "name": "external_tool", "input": map[string]string{"value": "arg"}}}}, map[string]any{"role": "user", "content": []any{map[string]string{"type": "tool_result", "tool_use_id": "call_external", "content": "CALLER_TOOL_RESULT_END"}}})
				} else {
					body["input"] = append(history, map[string]any{"type": "function_call", "call_id": "call_external", "name": "external_tool", "arguments": `{"value":"arg"}`}, map[string]string{"type": "function_call_output", "call_id": "call_external", "output": "CALLER_TOOL_RESULT_END"})
				}
			}
			if sent.Load() != 2 {
				t.Fatalf("CLI continued inference without the caller: %d requests", sent.Load())
			}
		})
	}
}
