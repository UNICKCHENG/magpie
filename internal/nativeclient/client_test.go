package nativeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

var liveBinDir = os.Getenv("MAGPIE_TEST_NATIVE_BIN_DIR")

func TestMain(m *testing.M) { testenv.Main(m) }

// This opt-in check uses installed, real CLIs with synthetic credentials and
// a loopback model service. It never connects to an actual inference provider.
func TestNativeInstalledClients(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("set MAGPIE_TEST_NATIVE_BIN_DIR to a directory containing native codex and claude executables")
	}
	for _, client := range []string{Codex, Claude} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", client, stream), func(t *testing.T) {
				t.Setenv("PATH", liveBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				var sent atomic.Int32
				seen := make(chan map[string]json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sent.Add(1)
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("native body: %v", err)
						w.WriteHeader(400)
						return
					}
					if r.Header.Get("Authorization") != "Bearer synthetic-native-key" && r.Header.Get("X-Api-Key") != "synthetic-native-key" {
						t.Error("native CLI did not use the custom provider key")
					}
					if r.Header.Get("User-Agent") == "" || strings.HasPrefix(r.Header.Get("User-Agent"), "magpie/") {
						t.Error("request was not constructed by the native CLI")
					}
					select {
					case seen <- body:
					default:
						t.Error("native client made an extra model request")
					}
					answer(w, client, text(body["model"]))
				}))
				defer server.Close()
				body := map[string]any{"model": "gpt-5.6", "stream": stream, "instructions": "Keep the caller's instruction.", "input": []any{
					map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "First question"}}},
					map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "Previous answer"}}},
					map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Next question"}}},
				}}
				base := server.URL + "/v1"
				if client == Claude {
					body = map[string]any{"model": "claude-haiku-5-5", "stream": stream, "max_tokens": 64, "system": "Keep the caller's instruction.", "messages": []any{
						map[string]string{"role": "user", "content": "First question"},
						map[string]string{"role": "assistant", "content": "Previous answer"},
						map[string]string{"role": "user", "content": "Next question"},
					}}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				name := "codex"
				if client == Claude {
					name = "claude"
				}
				response, err := Do(ctx, Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: base, Key: "synthetic-native-key", Proxy: "direct"}, raw(body))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != 200 || !strings.Contains(string(data), "Native answer") {
					t.Fatalf("native result: status=%d body=%s err=%v", response.StatusCode, data, err)
				}
				if client == Codex && stream {
					checkResponsesTextStream(t, data)
				}
				select {
				case request := <-seen:
					conversation := request["input"]
					if client == Claude {
						conversation = request["messages"]
						if !strings.Contains(string(request["system"]), "x-anthropic-billing-header") || !strings.Contains(string(request["system"]), "Keep the caller's instruction.") {
							t.Fatal("caller system instruction replaced the native Claude system prompt")
						}
						if string(request["max_tokens"]) != "64" {
							t.Fatalf("Claude changed the caller's output limit: %s", request["max_tokens"])
						}
					}
					for _, value := range []string{"First question", "Previous answer", "Next question"} {
						if !strings.Contains(string(conversation), value) {
							t.Fatalf("native request lost conversation text %q", value)
						}
					}
				case <-ctx.Done():
					t.Fatal("native client sent no model request")
				}
				if got := sent.Load(); got != 1 {
					t.Fatalf("native client sent %d requests for one inference", got)
				}
			})
		}
	}
}

func answer(w http.ResponseWriter, client, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, values map[string]any) {
		values["type"] = kind
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw(values))
		w.(http.Flusher).Flush()
	}
	if client == Claude {
		event("message_start", map[string]any{"message": map[string]any{"id": "msg_native", "type": "message", "role": "assistant", "model": model, "content": []any{}, "usage": map[string]int{"input_tokens": 12, "output_tokens": 0}}})
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "text_delta", "text": "Native answer"}})
		event("content_block_stop", map[string]any{"index": 0})
		event("message_delta", map[string]any{"delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 2}})
		event("message_stop", map[string]any{})
		return
	}
	item := map[string]any{"id": "msg_native", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Native answer", "annotations": []any{}}}}
	response := map[string]any{"id": "resp_native", "object": "response", "model": model, "created_at": 1, "status": "in_progress", "output": []any{}, "usage": map[string]int{"input_tokens": 12, "output_tokens": 2, "total_tokens": 14}}
	event("response.created", map[string]any{"response": response})
	event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_native", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	event("response.output_text.delta", map[string]any{"item_id": "msg_native", "output_index": 0, "content_index": 0, "delta": "Native answer"})
	event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	response["status"], response["output"] = "completed", []any{item}
	event("response.completed", map[string]any{"response": response})
}

func TestNativeCodexDoesNotDropUnknownParameters(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err := codexArguments(Options{}, map[string]json.RawMessage{"model": raw("gpt-5.6"), "max_output_tokens": raw(64)}, nil, home)
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatal("an unsupported output limit was silently discarded")
	}
}

func TestNativeEnvironmentIsPrivateAndUsesProviderProxy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "live-secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "live-secret")
	t.Setenv("CODEX_HOME", "live-config")
	t.Setenv("HTTPS_PROXY", "http://environment-proxy:3128")
	home := t.TempDir()
	for _, choice := range []string{"direct", "http://provider-proxy:3128"} {
		env := Environment(home, choice)
		values := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
			if strings.Contains(value, "live-secret") || strings.Contains(value, "live-config") {
				t.Fatal("native CLI inherited user credentials or configuration")
			}
		}
		if values["HOME"] != home || values["CODEX_HOME"] != filepath.Join(home, ".codex") || values["CLAUDE_CONFIG_DIR"] != filepath.Join(home, ".claude") {
			t.Fatal("native CLI configuration escaped its private home")
		}
		if choice == "direct" && values["HTTPS_PROXY"] != "" || choice != "direct" && values["HTTPS_PROXY"] != choice {
			t.Fatalf("provider proxy %s was not applied: %s", choice, values["HTTPS_PROXY"])
		}
	}
}

// A Responses consumer creates each content part before it can apply deltas.
// Rebuild the text from stream events, independently of the final response.
func checkResponsesTextStream(t *testing.T, data []byte) {
	t.Helper()
	parts := map[string]string{}
	finished := map[string]bool{}
	completed := false
	sequence := 0
	for _, line := range strings.Split(string(data), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			t.Fatal(err)
		}
		if e["sequence_number"] != float64(sequence) {
			t.Fatalf("nonsequential Responses event: %s", payload)
		}
		sequence++
		key := fmt.Sprint(e["output_index"], ":", e["content_index"])
		switch e["type"] {
		case "response.content_part.added":
			parts[key] = e["part"].(map[string]any)["text"].(string)
		case "response.output_text.delta":
			if _, ok := parts[key]; !ok {
				t.Fatalf("text delta arrived before content_part.added: %s", payload)
			}
			parts[key] += e["delta"].(string)
		case "response.output_text.done":
			if parts[key] != e["text"] {
				t.Fatalf("text.done differs from accumulated deltas: %s", payload)
			}
		case "response.content_part.done":
			if parts[key] != e["part"].(map[string]any)["text"] {
				t.Fatalf("completed content differs from accumulated deltas: %s", payload)
			}
			finished[key] = true
		case "response.completed":
			completed = true
		}
	}
	if !completed || len(parts) != 1 || len(finished) != 1 || parts["0:0"] != "Native answer" {
		t.Fatalf("incomplete Responses stream: parts=%v finished=%v completed=%v", parts, finished, completed)
	}
}
