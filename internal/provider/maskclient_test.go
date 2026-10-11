package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskClientScope(t *testing.T) {
	detectHome(t)
	for _, tc := range []struct {
		name, api, client, preset string
		want                      string
	}{
		{"responses", "responses", ClientCodex, "", ClientCodex},
		{"anthropic", "anthropic", ClientClaudeCode, "", ClientClaudeCode},
		{"off", "responses", "", "", ""},
		{"chat", "chat", ClientCodex, "", ""},
		{"wrong client", "responses", ClientClaudeCode, "", ""},
		{"preset", "responses", ClientCodex, "openai", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{ID: "relay", Name: "Relay", Key: "k", BaseAPI: tc.api, MaskClient: tc.client, Preset: tc.preset,
				Chat: "https://relay.example/v1", Responses: "https://relay.example/v1", Anthropic: "https://relay.example"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			got, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.MaskClient != tc.want {
				t.Fatalf("mask %q, want %q", got.MaskClient, tc.want)
			}
		})
	}
}

// The default test follows a model's API choice, then adds the fingerprint
// only on the matching API. Enabling Codex never reroutes a Chat model.
func TestMaskClientKeepsModelAPI(t *testing.T) {
	detectHome(t)
	var path, ua string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ua = r.URL.Path, r.UserAgent()
		if path == "/v1/responses" && !strings.HasPrefix(ua, "codex_cli_rs/") {
			http.Error(w, "missing Codex identity", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(up.Close)
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Chat: up.URL + "/v1", Responses: up.URL + "/v1", BaseAPI: "responses", MaskClient: ClientCodex, Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"chat", "responses"} {
		if err := SetModelAPI("relay/m", api); err != nil {
			t.Fatal(err)
		}
		p, err := Find("relay")
		if err != nil {
			t.Fatal(err)
		}
		rs := p.TestModels(context.Background(), []string{"m"})
		if len(rs) != 1 || !rs[0].OK || string(rs[0].Protocol) != api {
			t.Fatalf("%s: %+v", api, rs)
		}
		if api == "chat" && (path != "/v1/chat/completions" || strings.HasPrefix(ua, "codex_cli_rs/")) {
			t.Fatalf("Chat was masked or rerouted: %s %q", path, ua)
		}
	}
}

func TestMaskBodyKeepsUserFields(t *testing.T) {
	for _, tc := range []struct {
		client string
		proto  Protocol
		body   string
	}{
		{ClientCodex, Responses, `{"instructions":"mine","reasoning":{"effort":"low"},"include":[],"input":[]}`},
		{ClientClaudeCode, Anthropic, `{"system":"You are Claude Code, Anthropic's official CLI for Claude.\nmine","metadata":{"user_id":"mine"},"messages":[]}`},
	} {
		var before, after map[string]json.RawMessage
		json.Unmarshal([]byte(tc.body), &before)
		if err := json.Unmarshal(MaskBody(tc.client, "key", tc.proto, []byte(tc.body)), &after); err != nil {
			t.Fatal(err)
		}
		for k, v := range before {
			if string(after[k]) != string(v) {
				t.Errorf("%s changed %s: %s", tc.client, k, after[k])
			}
		}
	}
}

// Claude Code 2.1.296 puts its identity before a caller's own system prompt,
// including --system-prompt. Aigo refused the same request without it.
func TestMaskClaudeCodeKeepsIdentityWithSystem(t *testing.T) {
	const identity = "You are Claude Code, Anthropic's official CLI for Claude."
	for _, system := range []string{
		`"test"`,
		`""`,
		`null`,
		`[]`,
		`[{"type":"text","text":"test","cache_control":{"type":"ephemeral"}}]`,
		`[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.296.e95; cc_entrypoint=sdk-cli;"},{"type":"text","text":"test"}]`,
	} {
		t.Run(system, func(t *testing.T) {
			body := []byte(`{"system":` + system + `,"messages":[{"role":"user","content":"hi"}]}`)
			got := MaskBody(ClientClaudeCode, "key", Anthropic, body)
			var out struct {
				System []struct {
					Type, Text   string
					CacheControl json.RawMessage `json:"cache_control"`
				}
			}
			if err := json.Unmarshal(got, &out); err != nil {
				t.Fatalf("system was not made into blocks: %v", err)
			}
			count, own := 0, -1
			for i, b := range out.System {
				if b.Text == identity {
					count++
					if own >= 0 {
						t.Error("client identity follows the business prompt")
					}
				}
				if b.Text == "test" {
					own = i
				}
			}
			if count != 1 {
				t.Fatalf("Claude Code identity count = %d, want 1", count)
			}
			if strings.Contains(system, "test") && own < 0 {
				t.Fatal("business system prompt was lost")
			}
			if strings.Contains(system, "cache_control") && string(out.System[own].CacheControl) != `{"type":"ephemeral"}` {
				t.Fatal("business prompt's cache_control was lost")
			}
			if strings.Contains(system, "x-anthropic-billing-header") && !strings.HasPrefix(out.System[0].Text, "x-anthropic-billing-header:") {
				t.Fatal("the client's billing block no longer comes first")
			}
			if again := MaskBody(ClientClaudeCode, "key", Anthropic, got); string(again) != string(got) {
				t.Fatal("applying the fingerprint twice changed the request")
			}
		})
	}
}

func TestMaskClaudeCodeKeepsNativeSystem(t *testing.T) {
	for _, text := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	} {
		system, _ := json.Marshal([]map[string]any{{"type": "text", "text": text, "cache_control": map[string]string{"type": "ephemeral"}}})
		var out map[string]json.RawMessage
		json.Unmarshal(MaskBody(ClientClaudeCode, "key", Anthropic, []byte(`{"system":`+string(system)+`}`)), &out)
		if string(out["system"]) != string(system) {
			t.Errorf("native system changed: %s", out["system"])
		}
	}
}
