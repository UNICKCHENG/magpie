package gateway

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
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestNativeChatConversion(t *testing.T) {
	if nativeBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{provider.ClientClaudeCode, provider.ClientCodex} {
		for _, native := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/native=%v/stream=%v", client, native, stream), func(t *testing.T) { testNativeChat(t, client, native, stream, false) })
			}
		}
	}
}

func TestNativeChatRefusedDisplay(t *testing.T) {
	if nativeBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, native := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/stream=%v", native, stream), func(t *testing.T) { testNativeChat(t, provider.ClientClaudeCode, native, stream, true) })
		}
	}
}

// Exercise conversion and compatibility through the same server stack used by
// ordinary providers, with the real native executable at the outgoing boundary.
func testNativeChat(t *testing.T, client string, native, stream, refuseDisplay bool) {
	t.Helper()
	fresh(t)
	t.Setenv("PATH", nativeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, model, api := "codex", "gpt-5.6", "responses"
	if client == provider.ClientClaudeCode {
		bin, model, api = "claude", "claude-haiku-5-5", "anthropic"
	}
	fixture, err := os.ReadFile(filepath.Join("..", "nativeclient", "testdata", bin+".sse"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan map[string]json.RawMessage, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		select {
		case seen <- p:
		default:
			t.Error("too many upstream attempts")
			w.WriteHeader(500)
			return
		}
		if api == "responses" {
			if !strings.HasSuffix(r.URL.Path, "/responses") || len(p["input"]) == 0 || len(p["messages"]) > 0 {
				t.Error("Chat was not converted to Responses")
			}
			if !strings.Contains(string(p["instructions"]), "caller system") && !(native && strings.Contains(string(p["input"]), "caller system")) {
				t.Error("Responses lost the system instruction")
			}
		} else {
			if !strings.Contains(r.URL.Path, "/messages") || !strings.Contains(string(p["system"]), "caller system") {
				t.Error("Chat was not converted to Anthropic")
			}
			if native && !strings.Contains(string(p["system"]), "x-anthropic-billing-header") {
				t.Error("caller system replaced native Claude system")
			}
			var messages []struct{ Role string }
			if err := json.Unmarshal(p["messages"], &messages); err != nil {
				t.Error(err)
			}
			for _, m := range messages {
				if m.Role == "system" {
					t.Error("Chat system role leaked into Anthropic messages")
				}
			}
			if !strings.Contains(string(p["tools"]), "input_schema") {
				t.Error("Chat tool schema was not converted")
			}
		}
		if !strings.Contains(string(p["input"])+string(p["messages"]), "caller user") {
			t.Error("user content lost")
		}
		if refuseDisplay && strings.Contains(string(p["thinking"]), "display") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"thinking.adaptive.display: Extra inputs are not permitted"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer up.Close()
	p := provider.Provider{ID: "caller", Key: "synthetic-key", BaseAPI: api, Models: []string{model}, Proxy: "direct"}
	if api == "responses" {
		p.Responses = up.URL + "/v1"
	} else {
		p.Anthropic = up.URL
	}
	if native {
		p.MaskClient = client
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.native.Close()
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()
	body := map[string]any{"model": "caller/" + model, "stream": stream, "messages": []any{map[string]string{"role": "system", "content": "caller system"}, map[string]string{"role": "user", "content": "caller user"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "caller_tool", "description": "Synthetic tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}}
	if refuseDisplay {
		body["reasoning_effort"] = "medium"
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(data), "Native answer") {
		t.Fatalf("Chat response status=%d error=%v body=%s", res.StatusCode, err, data)
	}
	want := 1
	if refuseDisplay {
		want = 2
	}
	if len(seen) != want {
		t.Fatalf("upstream attempts=%d, want %d", len(seen), want)
	}
	if refuseDisplay {
		first, second := <-seen, <-seen
		if !strings.Contains(string(first["thinking"]), "display") || strings.Contains(string(second["thinking"]), "display") {
			t.Fatal("compatibility retry did not remove refused display")
		}
	}
}
