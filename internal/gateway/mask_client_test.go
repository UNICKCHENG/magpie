package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// maskFake is a relay that records every request whole — its path with its
// query, its headers, its body — and replies with a scripted stream.
type maskFake struct {
	t     *testing.T
	reply string
	mu    sync.Mutex
	paths []string
	head  []http.Header
	body  []map[string]any
}

func (f *maskFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	f.head = append(f.head, r.Header.Clone())
	f.body = append(f.body, m)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, f.reply)
}

func (f *maskFake) last(t *testing.T) (string, http.Header, map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		t.Fatal("nothing was asked")
	}
	return f.paths[len(f.paths)-1], f.head[len(f.head)-1], f.body[len(f.body)-1]
}

// A provider that wears a client (provider.MaskClient) sends every request
// to it in that client's identity, whichever agent the request came from:
// here a plain client's Responses request reaches the relay as Codex CLI
// sends one, and one Codex itself sent keeps its own headers.
func TestMaskedRequestsWearCodex(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := &maskFake{t: t, reply: sse(
		`data: {"type":"response.created","response":{"id":"r1","model":"m1"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"},
		Responses: up.URL + "/v1", BaseAPI: "responses", MaskClient: provider.ClientCodex}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	ask := func(ua, text string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
			`{"model":"fake/m1","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}]}`))
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return rec.Code
	}
	ask("", "hello")
	_, head, body := f.last(t)
	if !strings.HasPrefix(head.Get("User-Agent"), "codex_cli_rs/") || head.Get("originator") != "codex_cli_rs" || head.Get("version") == "" {
		t.Fatalf("asked as %q / %q / %q", head.Get("User-Agent"), head.Get("originator"), head.Get("version"))
	}
	if body["store"] != false {
		t.Fatalf("store: %v", body["store"])
	}
	if ins, _ := body["instructions"].(string); ins == "" {
		t.Fatalf("instructions: %.60s", body["instructions"])
	}
	if head.Get("session-id") == "" || head.Get("session-id") != head.Get("thread-id") || head.Get("x-codex-window-id") != head.Get("session-id")+":0" {
		t.Fatalf("session: %q %q %q", head.Get("session-id"), head.Get("thread-id"), head.Get("x-codex-window-id"))
	}
	same := head.Get("session-id")
	// the same conversation keeps its id; a different one gets its own
	ask("", "hello")
	_, head, _ = f.last(t)
	if head.Get("session-id") != same {
		t.Fatalf("same conversation, id %q then %q", same, head.Get("session-id"))
	}
	ask("", "another thing")
	_, head, _ = f.last(t)
	if head.Get("session-id") == same {
		t.Fatalf("different conversation, same id %q", same)
	}
	// a request Codex itself sent goes as it sent it
	ask("codex_cli_rs/0.159.2 (Mac OS 26.6.0; arm64) Apple_Terminal/455", "hello")
	_, head, _ = f.last(t)
	if got := head.Get("User-Agent"); got != "codex_cli_rs/0.159.2 (Mac OS 26.6.0; arm64) Apple_Terminal/455" {
		t.Fatalf("Codex's own UA replaced: %q", got)
	}
}

// The Claude Code mask over an Anthropic endpoint: a plain client's
// request, and one translated from Codex's Responses, both reach the relay
// as Claude Code sends them — its headers, its billing-marker system line,
// a stable user id, ?beta=true — and one Claude Code itself sent keeps its
// own.
func TestMaskedRequestsWearClaudeCode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := &maskFake{t: t, reply: sse(
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-sonnet-5.5"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"claude-sonnet-5.5"},
		Anthropic: up.URL, BaseAPI: "anthropic", MaskClient: provider.ClientClaudeCode}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	ask := func(ua, betas, path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if betas != "" {
			req.Header.Set("anthropic-beta", betas)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s %s: status %d: %s", ua, path, rec.Code, rec.Body)
		}
	}
	check := func() (string, http.Header, map[string]any) {
		path, head, body := f.last(t)
		if !strings.HasPrefix(head.Get("User-Agent"), "claude-cli/") || head.Get("x-app") != "cli" {
			t.Fatalf("asked as %q / %q", head.Get("User-Agent"), head.Get("x-app"))
		}
		if !strings.Contains(head.Get("anthropic-beta"), "claude-code-20250219") {
			t.Fatalf("betas: %q", head.Get("anthropic-beta"))
		}
		if !strings.HasSuffix(path, "beta=true") {
			t.Fatalf("asked at %s", path)
		}
		sys, _ := body["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("system: %v", body["system"])
		}
		first, _ := sys[0].(map[string]any)
		if !strings.HasPrefix(first["text"].(string), "x-anthropic-billing-header: cc_version=") {
			t.Fatalf("billing marker: %v", first["text"])
		}
		return path, head, body
	}
	// a plain client's request
	ask("", "", "/v1/messages", `{"model":"fake/claude-sonnet-5.5","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, _, body := check()
	meta, _ := body["metadata"].(map[string]any)
	user := meta["user_id"]
	if user == nil {
		t.Fatalf("no user id: %v", body["metadata"])
	}
	// one Codex sent, translated to Anthropic on the way: the same mask
	ask("codex_cli_rs/0.159.2 (Mac OS 26.6.0; arm64) Apple_Terminal/455", "", "/v1/responses",
		`{"model":"fake/claude-sonnet-5.5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	_, _, body = check()
	if meta, _ = body["metadata"].(map[string]any); meta["user_id"] != user {
		t.Fatalf("user id not stable: %v then %v", user, meta["user_id"])
	}
	// Chat clients supply their own system and tools. The translation must
	// keep them while adding the identity a Claude Code relay requires.
	ask("ai-sdk", "", "/v1/chat/completions",
		`{"model":"fake/claude-sonnet-5.5","stream":true,"messages":[{"role":"system","content":"test"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"fs_read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}`)
	_, _, body = f.last(t)
	sys, _ := body["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("translated system lacks client identity or business prompt: %v", sys)
	}
	if sys[0].(map[string]any)["text"] != "You are Claude Code, Anthropic's official CLI for Claude." || sys[1].(map[string]any)["text"] != "test" {
		t.Fatalf("translated system: %v", sys)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "fs_read" {
		t.Fatalf("translated tools lost: %v", tools)
	}
	// one Claude Code itself sent keeps its own headers, betas and system,
	// and its ?beta=true goes on as it asked
	ask("claude-cli/2.1.290 (external, cli)", "claude-code-20250219,context-1m-2025-08-07", "/v1/messages?beta=true",
		`{"model":"fake/claude-sonnet-5.5","max_tokens":20,"stream":true,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"mine"}],"messages":[{"role":"user","content":"hi"}]}`)
	path, head, body := f.last(t)
	if head.Get("User-Agent") != "claude-cli/2.1.290 (external, cli)" || head.Get("x-app") != "" {
		t.Fatalf("Claude Code's own replaced: %q %q", head.Get("User-Agent"), head.Get("x-app"))
	}
	if head.Get("anthropic-beta") != "claude-code-20250219,context-1m-2025-08-07" {
		t.Fatalf("agent's betas replaced: %q", head.Get("anthropic-beta"))
	}
	if !strings.HasSuffix(path, "beta=true") {
		t.Fatalf("Claude Code's ?beta=true lost: %s", path)
	}
	if sys, _ := body["system"].([]any); len(sys) != 2 || sys[1].(map[string]any)["text"] != "mine" {
		t.Fatalf("own system replaced: %v", body["system"])
	}
}

// The mask's beta list is fitted as any other betas are: a provider that
// turns a beta it doesn't know away by name (betaRelay, as Bedrock does)
// is asked again without it — the mask doesn't put it back on the retry,
// and the rest of its list still goes.
func TestMaskedBetasRefusedAreAskedWithout(t *testing.T) {
	fresh(t)
	up := &betaRelay{refuses: []string{"dangerous-tool-use-2026-09-03"}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Anthropic: srv.URL,
		Models: []string{"claude-sonnet-5.5"}, BaseAPI: "anthropic", MaskClient: provider.ClientClaudeCode}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"relay/claude-sonnet-5.5","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	up.mu.Lock()
	seen := append([][]string{}, up.seen...)
	up.mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("%d asks, want the refused one and the ask-again", len(seen))
	}
	if !strings.Contains(seen[0][0], "dangerous-tool-use-2026-09-03") {
		t.Fatalf("the first ask didn't carry the mask's betas: %q", seen[0][0])
	}
	if strings.Contains(seen[1][0], "dangerous-tool-use-2026-09-03") {
		t.Fatalf("the refused beta asked again: %q", seen[1][0])
	}
	if !strings.Contains(seen[1][0], "claude-code-20250219") {
		t.Fatalf("the rest of the mask's list dropped: %q", seen[1][0])
	}
}

// A disabled fingerprint and a nonmatching outgoing API use the original
// path. In particular, a Responses opt-in never masks a Chat request.
func TestMaskClientOffAndChatUnchanged(t *testing.T) {
	fresh(t)
	f := &maskFake{reply: `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"output":[]}`}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	for _, tc := range []struct{ api, client, path, body string }{
		{"responses", "", "/v1/responses", `{"model":"fake/m","input":"hi"}`},
		{"anthropic", "", "/v1/messages", `{"model":"fake/m","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", provider.ClientCodex, "/v1/chat/completions", `{"model":"fake/m","messages":[{"role":"user","content":"hi"}]}`},
	} {
		p := provider.Provider{ID: "fake", Name: "Fake", Key: "k", Chat: up.URL + "/v1", Responses: up.URL + "/v1", Anthropic: up.URL, BaseAPI: tc.api, MaskClient: tc.client, Models: []string{"m"}}
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
		path, h, body := f.last(t)
		if path != tc.path || !strings.HasPrefix(h.Get("User-Agent"), "magpie/") || h.Get("originator") != "" || h.Get("x-app") != "" {
			t.Fatalf("unexpected fingerprint: %s %v", path, h)
		}
		for _, k := range []string{"instructions", "system", "metadata", "include"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s: added %s", tc.path, k)
			}
		}
	}
}
