package provider

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

	"github.com/yetone/magpie/internal/fingerprint"
)

var nativeProbeBinDir = os.Getenv("MAGPIE_TEST_NATIVE_BIN_DIR")

func TestNativeProbeDoesNotSimulateAnUnavailableCLI(t *testing.T) {
	detectHome(t)
	var models, posts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			models.Add(1)
			io.WriteString(w, `{"data":[{"id":"gpt-5.6"}]}`)
		} else {
			posts.Add(1)
			io.WriteString(w, `{"output":[]}`)
		}
	}))
	defer up.Close()
	p := Provider{ID: "native", Key: "synthetic", Responses: up.URL + "/v1", BaseAPI: "responses", MaskClient: ClientCodex, Models: []string{"gpt-5.6"}}
	for name, run := range map[string]func() []Result{"models": func() []Result { return p.TestModels(context.Background(), p.Models) }, "provider": func() []Result { return p.Test(context.Background()) }} {
		t.Run(name, func(t *testing.T) {
			rs := run()
			if len(rs) != 1 || rs[0].OK || !strings.Contains(rs[0].Error, "native") {
				t.Fatalf("unavailable native CLI was simulated: %+v", rs)
			}
		})
	}
	if posts.Load() != 0 {
		t.Fatal("native probe used direct HTTP")
	}
	if models.Load() == 0 {
		t.Fatal("model list stopped using HTTP")
	}
}

func TestNativeProbesInstalled(t *testing.T) {
	if nativeProbeBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{ClientCodex, ClientClaudeCode} {
		t.Run(client, func(t *testing.T) {
			detectHome(t)
			t.Setenv("PATH", nativeProbeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			name, model, proto := "codex", "gpt-5.6", Responses
			if client == ClientClaudeCode {
				name, model, proto = "claude", "claude-haiku-5-5", Anthropic
			}
			fixture, err := os.ReadFile(filepath.Join("..", "nativeclient", "testdata", name+".sse"))
			if err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
					return
				}
				sent.Add(1)
				b, _ := io.ReadAll(r.Body)
				if strings.HasPrefix(r.UserAgent(), "magpie/") || r.UserAgent() == "Go-http-client/1.1" {
					t.Error("probe bypassed native CLI")
				}
				if strings.Contains(string(b), "max_output_tokens") {
					t.Error("internal Codex probe retained an unsupported output limit")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(fixture)
			}))
			defer up.Close()
			p := Provider{ID: "native", Key: "synthetic", BaseAPI: string(proto), MaskClient: client, Models: []string{model}}
			if proto == Responses {
				p.Responses = up.URL + "/v1"
			} else {
				p.Anthropic = up.URL
			}
			// This contract tests inference after startup warmup. Provisioning a
			// new private executable is tested separately; it must not consume
			// the probe's fixed 20 s model deadline on a loaded build runner.
			warm, cancelWarm := context.WithTimeout(context.Background(), time.Minute)
			began := time.Now()
			err = fingerprint.Warm(warm, client, map[string]bool{})
			cancelWarm()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("private native initialization: %v", time.Since(began))
			if sent.Load() != 0 {
				t.Fatal("warmup sent a model request")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for name, run := range map[string]func() []Result{"models": func() []Result { return p.TestModels(ctx, p.Models) }, "provider": func() []Result { return p.Test(ctx) }, "as native client": func() []Result { return p.Test(TestAs(ctx, client)) }} {
				t.Run(name, func(t *testing.T) {
					rs := run()
					if len(rs) != 1 || !rs[0].OK || rs[0].Protocol != proto {
						t.Fatalf("native probe: %+v", rs)
					}
				})
			}
			if sent.Load() != 3 {
				t.Fatalf("unexpected model calls: %d", sent.Load())
			}
		})
	}
}
