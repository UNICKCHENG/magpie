package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The client a custom provider masks as follows the Base URL's API
// (maskclient.go): the editor sends it with the Save, it is kept by a save
// that doesn't say, and it is dropped when the endpoint it speaks is gone.
func TestProviderMaskClientKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(action, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/"+action, strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body)
		}
	}
	shown := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/providers", nil))
		var got struct{ Providers []map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, p := range got.Providers {
			if p["id"] == "relay" {
				s, _ := p["maskClient"].(string)
				return s
			}
		}
		t.Fatalf("relay not listed: %s", w.Body)
		return ""
	}
	const u = "http://127.0.0.1:1"
	// added as the editor sends it: Anthropic picked, Claude Code ticked
	post("save", `{"id":"relay","name":"Relay","key":"sk-a","anthropic":"`+u+`","icon":"generic","baseAPI":"anthropic","maskClient":"claude-code","new":true}`)
	if got := shown(); got != "claude-code" {
		t.Fatalf("added with the mask, shown as %q", got)
	}
	// a save that leaves it out (an older window, another page) keeps it
	post("save", `{"id":"relay","from":"relay","name":"Relay","anthropic":"`+u+`","icon":"generic"}`)
	if got := shown(); got != "claude-code" {
		t.Fatalf("a save without the mask lost it: %q", got)
	}
	// Chat does not support a fingerprint, even if an API caller sets it.
	post("save", `{"id":"relay","from":"relay","name":"Relay","chat":"`+u+`/v1","icon":"generic","baseAPI":"chat","maskClient":"codex"}`)
	if got := shown(); got != "" {
		t.Fatalf("saved as Chat, mask %q", got)
	}
	// Responses picked instead: the mask the editor sends is Codex's
	post("save", `{"id":"relay","from":"relay","name":"Relay","responses":"`+u+`/v1","anthropic":"`+u+`","icon":"generic","baseAPI":"responses","maskClient":"codex"}`)
	if got := shown(); got != "codex" {
		t.Fatalf("saved as Responses, mask %q", got)
	}
	// unticked, or a protocol no coding client speaks: none
	post("save", `{"id":"relay","from":"relay","name":"Relay","responses":"`+u+`/v1","anthropic":"`+u+`","icon":"generic","baseAPI":"responses","maskClient":""}`)
	if got := shown(); got != "" {
		t.Fatalf("unticked, mask %q", got)
	}
	// the endpoint it speaks gone (cleared elsewhere, the CLI): no mask
	post("save", `{"id":"relay","from":"relay","name":"Relay","responses":"`+u+`/v1","icon":"generic","baseAPI":"responses","maskClient":"codex"}`)
	post("save", `{"id":"relay","from":"relay","name":"Relay","anthropic":"`+u+`","icon":"generic","baseAPI":"anthropic","maskClient":"codex"}`)
	if got := shown(); got != "" {
		t.Fatalf("a codex mask with no Responses endpoint kept: %q", got)
	}
}
