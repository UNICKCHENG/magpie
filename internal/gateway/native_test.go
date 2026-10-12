package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

var nativeBinDir = os.Getenv("MAGPIE_TEST_NATIVE_BIN_DIR")

func TestNativeScopeSeparatesCallersClientsAndSessions(t *testing.T) {
	p := provider.Provider{ID: "custom"}
	ctx := access.WithIdentity(context.Background(), access.Identity{KeyID: "caller-A"})
	h := http.Header{"User-Agent": {"app-A"}, SessionHeader: {"session-A"}}
	first := nativeScope(ctx, p, h)
	if first == "" {
		t.Fatal("explicit scope was not reusable")
	}
	for name, change := range map[string]func(context.Context, http.Header) (context.Context, http.Header){
		"key": func(ctx context.Context, h http.Header) (context.Context, http.Header) {
			return access.WithIdentity(ctx, access.Identity{KeyID: "caller-B"}), h
		},
		"client": func(ctx context.Context, h http.Header) (context.Context, http.Header) {
			h.Set("User-Agent", "app-B")
			return ctx, h
		},
		"session": func(ctx context.Context, h http.Header) (context.Context, http.Header) {
			h.Set(SessionHeader, "session-B")
			return ctx, h
		},
		"long session": func(ctx context.Context, h http.Header) (context.Context, http.Header) {
			h.Set(SessionHeader, strings.Repeat("x", 128)+"A")
			return ctx, h
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, next := change(ctx, h.Clone())
			if nativeScope(c, p, next) == first {
				t.Fatal("scope mixed identities")
			}
		})
	}
	h.Del(SessionHeader)
	if nativeScope(ctx, p, h) != "" {
		t.Fatal("missing session was inferred")
	}
	h.Set(SessionHeader, "session-A")
	h.Del("User-Agent")
	if nativeScope(ctx, p, h) != "" {
		t.Fatal("missing client identity was shared")
	}
}

func TestNativeFingerprintDoesNotFallBackToHTTP(t *testing.T) {
	fresh(t)
	var sent atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); io.WriteString(w, `{"output":[]}`) }))
	defer up.Close()
	p := provider.Provider{ID: "native", Key: "synthetic", BaseAPI: "responses", MaskClient: provider.ClientCodex, Responses: up.URL + "/v1", Models: []string{"gpt-5.6"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.native.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"native/gpt-5.6","input":"hello"}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 502 || sent.Load() != 0 || !strings.Contains(rec.Body.String(), "native") {
		t.Fatalf("missing CLI fell back to simulated HTTP: status=%d calls=%d body=%s", rec.Code, sent.Load(), rec.Body)
	}
}

func TestNativeCountTokensIsLocal(t *testing.T) {
	fresh(t)
	var sent atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); http.Error(w, "must be local", 500) }))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "native", Key: "synthetic", BaseAPI: "anthropic", MaskClient: provider.ClientClaudeCode, Anthropic: up.URL, Models: []string{"claude-haiku-5-5"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.native.Close()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"native/claude-haiku-5-5","messages":[{"role":"user","content":"hello world"}]}`)))
	if rec.Code != 200 || sent.Load() != 0 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("count started upstream: %d %s calls=%d", rec.Code, rec.Body, sent.Load())
	}
}

func TestNativeFingerprintGateway(t *testing.T) {
	if nativeBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{provider.ClientCodex, provider.ClientClaudeCode} {
		t.Run(client, func(t *testing.T) {
			fresh(t)
			t.Setenv("PATH", nativeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			name, model, path, api := "codex", "gpt-5.6", "/v1/responses", "responses"
			body := `{"model":"native/gpt-5.6","input":"gateway message","instructions":"gateway instruction"}`
			if client == provider.ClientClaudeCode {
				name, model, path, api = "claude", "claude-haiku-5-5", "/v1/messages", "anthropic"
				body = `{"model":"native/claude-haiku-5-5","messages":[{"role":"user","content":"gateway message"}],"system":"gateway instruction","max_tokens":64}`
			}
			fixture, err := os.ReadFile(filepath.Join("..", "nativeclient", "testdata", name+".sse"))
			if err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				b, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(b), "gateway message") || !strings.Contains(string(b), "gateway instruction") {
					t.Error("caller body was lost")
				}
				if client == provider.ClientClaudeCode && !strings.Contains(r.Header.Get("Anthropic-Beta"), "synthetic-beta-2026-01-01") {
					t.Error("caller beta did not reach the native CLI")
				}
				if r.Header.Get("X-Custom") != "retained" {
					t.Error("explicit provider header lost")
				}
				if r.UserAgent() == "caller-fake-agent" || strings.HasPrefix(r.UserAgent(), "magpie/") || r.Header.Get("Session-Id") == "caller-private-session" {
					t.Error("host/client identity passed as a native fingerprint")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(fixture)
			}))
			defer up.Close()
			p := provider.Provider{ID: "native", Key: "synthetic-key", BaseAPI: api, MaskClient: client, Responses: up.URL + "/v1", Anthropic: up.URL, Models: []string{model}, Headers: map[string]string{"X-Custom": "retained"}}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			s := New()
			defer s.native.Close()
			// Real HTTP server exercises decoding, routing, native execution and response adaptation.
			gateway := httptest.NewServer(s.Handler())
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+path, strings.NewReader(body))
			req.Header.Set("User-Agent", "caller-fake-agent")
			if client == provider.ClientClaudeCode {
				req.Header.Set("Anthropic-Beta", "synthetic-beta-2026-01-01")
			}
			req.Header.Set(SessionHeader, "caller-private-session")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode != 200 || !strings.Contains(string(data), "Native answer") || sent.Load() != 1 {
				t.Fatalf("native route status=%d calls=%d err=%v body=%s", res.StatusCode, sent.Load(), err, data)
			}
		})
	}
}

// Native admission spends the ticket the forward loop already reserved;
// a retry has to acquire a new place, just as an HTTP retry does.
func TestNativeRPMUsesExistingAdmission(t *testing.T) {
	s := &Server{}
	p := provider.Provider{ID: "native", Key: "synthetic", MaxRPM: 1}
	ctx := s.paidFor(context.Background(), p, "native-key")
	if err := payRPM(ctx); err != nil {
		t.Fatal(err)
	}
	ticket := ctx.Value(rpmKey{}).(*rpmTicket)
	if ticket.paid.Load() {
		t.Fatal("native inference left its prepaid ticket unused")
	}
	if _, err := s.rpms.wait(context.Background(), "native-key", 1, 0); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := payRPM(canceled); err == nil {
		t.Fatal("a second native turn bypassed cancellation/admission")
	}
}
