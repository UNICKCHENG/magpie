package nativeclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeClaudeSystemPromptIsolation(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	seen := make(chan map[string]json.RawMessage, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		seen <- body
		answer(w, Claude, "claude-sonnet-5-5")
	}))
	defer up.Close()
	pool := &Pool{}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var released atomic.Int32
	opts := Options{Client: Claude, Executable: filepath.Join(liveBinDir, "claude"), Base: up.URL, Key: "synthetic-key", Proxy: "direct", Pool: pool, Scope: "key/client/session", Release: func() { released.Add(1) }}
	cache := map[string]string{"type": "ephemeral", "ttl": "1h"}
	systems := []any{
		"PRIVATE_SYSTEM_A: Follow the user's instructions.",
		"PRIVATE_SYSTEM_A: Follow the user's instructions.",
		[]any{map[string]any{"type": "text", "text": "PRIVATE_SYSTEM_B: First instruction.", "cache_control": cache}, map[string]string{"type": "text", "text": "Second instruction."}},
		nil, nil,
	}
	var nativeIdentity string
	for i, system := range systems {
		message := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Answer this request.", "cache_control": cache}}}
		tools := []any{map[string]any{"name": "caller_tool", "description": "A caller tool", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}, "cache_control": cache}}
		history := []any{
			map[string]any{"role": "user", "content": strings.Repeat("Keep this earlier context. ", 1024)},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "caller_history_tool", "name": "caller_tool", "input": map[string]string{"value": "earlier input"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "caller_history_tool", "content": "earlier result"}}},
			map[string]string{"role": "assistant", "content": "Earlier answer."},
			message,
		}
		body := map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 64, "messages": history, "tools": tools}
		if system != nil {
			body["system"] = system
		}
		res, err := Do(ctx, opts, raw(body))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("request %d status=%d err=%v body=%s", i, res.StatusCode, err, data)
		}
		wire := <-seen
		prompt := string(wire["system"])
		if !strings.Contains(prompt, "x-anthropic-billing-header") {
			t.Fatalf("request %d lost native Claude system", i)
		}
		var blocks []struct{ Text string }
		if err := json.Unmarshal(wire["system"], &blocks); err != nil {
			t.Fatal(err)
		}
		texts := []string{}
		for _, block := range blocks {
			texts = append(texts, block.Text)
		}
		joined := strings.Join(texts, "\n\n")
		// Native custom-system initialization must keep the caller's task
		// prompt free of the CLI's default task instructions.
		wantPrompt := ""
		if i < 2 {
			wantPrompt = systems[0].(string)
		} else if i == 2 {
			wantPrompt = "PRIVATE_SYSTEM_B: First instruction.\n\nSecond instruction."
		}
		if i == 0 {
			for _, block := range blocks {
				if block.Text != "" && block.Text != wantPrompt && !strings.Contains(block.Text, "x-anthropic-billing-header") {
					nativeIdentity = block.Text
					break
				}
			}
		}
		if nativeIdentity == "" || !strings.Contains(joined, nativeIdentity) {
			t.Fatal("caller prompt replaced native identity content")
		}
		if wantPrompt != "" {
			found := false
			for _, block := range blocks {
				if block.Text == wantPrompt {
					found = true
				}
			}
			if !found {
				t.Fatal("native default task instructions altered the caller system prompt")
			}
		}
		switch i {
		case 0, 1:
			if !strings.Contains(joined, systems[0].(string)) || strings.Contains(joined, "PRIVATE_SYSTEM_B") {
				t.Fatal("lost current system instruction or leaked another system")
			}
		case 2:
			if !strings.Contains(joined, "PRIVATE_SYSTEM_B: First instruction.\n\nSecond instruction.") || strings.Contains(joined, "PRIVATE_SYSTEM_A") {
				t.Fatal("system blocks were lost, reordered or leaked")
			}
		default:
			if strings.Contains(joined, "PRIVATE_SYSTEM_") {
				t.Fatal("a request without system inherited another request's instructions")
			}
		}
		for key, want := range map[string]any{"messages": history, "tools": tools} {
			var got, expected any
			if err := json.Unmarshal(wire[key], &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw(want), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("request %d changed caller %s or its cache controls", i, key)
			}
		}
		if i == 1 && released.Load() != 1 {
			t.Fatal("unchanged system did not reuse the native process")
		}
	}
	pool.Close()
	if released.Load() != int32(len(systems)) {
		t.Fatal("executable pins leaked after shutdown")
	}
}

// Invalid system input must fail before starting a CLI or spending an RPM ticket.
func TestNativeClaudeRejectsUnsupportedSystemBeforeStartup(t *testing.T) {
	for name, system := range map[string]any{
		"number":        12,
		"image":         []any{map[string]any{"type": "image", "source": map[string]string{"type": "url", "url": "https://example.invalid/image"}}},
		"null-text":     []any{map[string]any{"type": "text", "text": nil}},
		"unknown-field": []any{map[string]any{"type": "text", "text": "Keep me", "unknown": "must not disappear"}},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Do(context.Background(), Options{Client: Claude, Executable: "must-not-start", Base: "http://127.0.0.1:1"}, raw(map[string]any{"model": "claude-sonnet-5-5", "system": system, "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "Hi"}}}))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			data, err := io.ReadAll(res.Body)
			if err != nil || res.StatusCode != http.StatusBadRequest {
				t.Fatalf("unsupported system did not fail before startup: status=%d err=%v body=%s", res.StatusCode, err, data)
			}
		})
	}
}
