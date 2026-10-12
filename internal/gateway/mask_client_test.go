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
