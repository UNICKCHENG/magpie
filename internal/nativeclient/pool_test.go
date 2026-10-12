package nativeclient

import (
	"context"
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
)

func TestNativePoolIsolationAndRetirement(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			var calls atomic.Int32
			type request struct{ body, key string }
			seen := make(chan request, 64)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				b, _ := io.ReadAll(r.Body)
				seen <- request{string(b), r.Header.Get("Authorization") + r.Header.Get("X-Api-Key")}
				answer(w, client, "test-model")
			}))
			defer up.Close()
			pool := &Pool{}
			defer pool.Close()
			name, model := "codex", "gpt-5.6"
			if client == Claude {
				name, model = "claude", "claude-haiku-5-5"
			}
			opts := Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: up.URL + "/v1", Key: "synthetic-key-A", Proxy: "direct", Scope: "key-A/client-A/session-A", Pool: pool}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var released atomic.Int32
			var history []string
			var firstEntry *idleProcess
			var firstKey [32]byte
			for i := 0; i < 12; i++ {
				// Same scope reuses, while the previous complete body must never remain.
				if i == 2 {
					opts.Key = "synthetic-key-B"
					// Keep the scope identical: changing the upstream API key alone must isolate.
				}
				if i == 3 {
					opts.Key = "synthetic-key-A"
					opts.Scope = "key-A/client-B/session-A"
				}
				if i == 4 {
					opts.Scope = "key-A/client-B/session-B"
				}
				marker := fmt.Sprintf("PRIVATE_MESSAGE_%04d_END", i)
				body := map[string]any{"model": model, "input": marker}
				if client == Claude {
					body = map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": marker}}, "max_tokens": 64}
				}
				if i > 1 && i%2 == 0 {
					if client == Claude {
						body["system"] = marker
					} else {
						body["instructions"] = marker
					}
				}
				opts.Release = func() { released.Add(1) }
				res, err := Do(ctx, opts, raw(body))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 || !strings.Contains(string(data), "Native answer") {
					t.Fatalf("request %d status=%d err=%v body=%s", i, res.StatusCode, err, data)
				}
				wire := <-seen
				if !strings.Contains(wire.body, marker) || !strings.Contains(wire.key, opts.Key) {
					t.Fatal("lost current message or credential")
				}
				for _, old := range history {
					if strings.Contains(wire.body, old) {
						t.Fatalf("request %d leaked %s", i, old)
					}
				}
				history = append(history, marker)
				if i == 0 && released.Load() != 0 {
					t.Fatal("first executable pin released instead of retaining the idle process")
				}
				if i == 1 && released.Load() != 1 {
					t.Fatal("second request did not reuse the initialized CLI")
				}
				if i == 1 {
					pool.expire(firstKey, firstEntry)
					if released.Load() != 1 {
						t.Fatal("stale idle timer killed a reused process")
					}
				}
				pool.mu.Lock()
				if i == 0 {
					for k, v := range pool.idle {
						firstKey, firstEntry = k, v
					}
				}
				idle := len(pool.idle)
				all := len(pool.all)
				pool.mu.Unlock()
				if idle > maxIdle || all > maxIdle {
					t.Fatalf("idle retention exceeded bound: %d/%d", idle, all)
				}
			}
			if calls.Load() != 12 {
				t.Fatalf("extra inference requests: %d", calls.Load())
			}
			pool.Close()
			if released.Load() != 12 {
				t.Fatalf("executable pins remain after shutdown: %d", released.Load())
			}
		})
	}
}

func TestNativeWarmDoesNotInfer(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			name := "codex"
			if client == Claude {
				name = "claude"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := Warm(ctx, client, filepath.Join(liveBinDir, name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativePoolCancellationReapsAndReleases(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			sent := make(chan struct{}, 1)
			releaseServer := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-releaseServer:
				}
			}))
			defer up.Close()
			defer close(releaseServer)
			pool := &Pool{}
			defer pool.Close()
			name, model := "codex", "gpt-5.6"
			if client == Claude {
				name, model = "claude", "claude-haiku-5-5"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var releases atomic.Int32
			opts := Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: up.URL + "/v1", Key: "synthetic-key", Proxy: "direct", Scope: "session", Pool: pool, Release: func() { releases.Add(1) }}
			b := map[string]any{"model": model, "input": "wait"}
			if client == Claude {
				b = map[string]any{"model": model, "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "wait"}}}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				res, _ := Do(ctx, opts, raw(b))
				if res != nil {
					res.Body.Close()
				}
			}()
			select {
			case <-sent:
			case <-ctx.Done():
				t.Fatal("CLI never sent request")
			}
			pool.mu.Lock()
			var homes []string
			for p := range pool.all {
				homes = append(homes, p.home)
			}
			pool.mu.Unlock()
			cancel()
			<-done
			if releases.Load() != 1 {
				t.Fatal("cancellation kept executable pinned")
			}
			for _, home := range homes {
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatal("cancelled CLI retained its private state")
				}
			}
			pool.mu.Lock()
			n := len(pool.all) + len(pool.idle)
			pool.mu.Unlock()
			if n != 0 {
				t.Fatal("cancelled CLI returned to pool")
			}
		})
	}
}

func TestNativePoolCloseUnblocksUnreadStream(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w, client, "test-model") }))
			defer up.Close()
			pool := &Pool{}
			defer pool.Close()
			name, model := "codex", "gpt-5.6"
			if client == Claude {
				name, model = "claude", "claude-haiku-5-5"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			body := map[string]any{"model": model, "input": "hi", "stream": true}
			if client == Claude {
				body = map[string]any{"model": model, "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": true}
			}
			res, err := Do(ctx, Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: up.URL + "/v1", Key: "synthetic-key", Proxy: "direct", Scope: "session", Pool: pool}, raw(body))
			if err != nil {
				t.Fatal(err)
			}
			// Do has announced the response, but its first pipe write has no reader.
			done := make(chan struct{})
			go func() { pool.Close(); close(done) }()
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
				res.Body.Close()
			case <-timer.C:
				res.Body.Close()
				<-done
				t.Fatal("owner shutdown waited for an abandoned response body")
			}
		})
	}
}

func TestNativePoolIdleExpiryReleasesPrivateState(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed Codex CLI")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w, Codex, "gpt-5.6") }))
	defer up.Close()
	pool := &Pool{}
	defer pool.Close()
	released := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := Do(ctx, Options{Client: Codex, Executable: filepath.Join(liveBinDir, "codex"), Base: up.URL + "/v1", Key: "synthetic", Proxy: "direct", Pool: pool, Scope: "idle-session", Release: func() { released <- struct{}{} }}, raw(map[string]any{"model": "gpt-5.6", "input": "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	pool.mu.Lock()
	var home string
	for run := range pool.all {
		home = run.home
	}
	pool.mu.Unlock()
	if home == "" {
		t.Fatal("completed request was not retained")
	}
	// The 90 s deadline is only a hung-cleanup guard; normal expiry is 30 s.
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("idle process never released its executable")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("idle expiry left private state on disk")
	}
}

func TestNativePoolConcurrentRequestsStaySeparate(t *testing.T) {
	if liveBinDir == "" {
		t.Skip("requires installed native CLIs")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			var active, peak, sent atomic.Int32
			first := make(chan struct{}, 4)
			release := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.Count(string(b), "CONCURRENT_MESSAGE_") != 1 {
					t.Error("concurrent request mixed or lost messages")
				}
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				sent.Add(1)
				select {
				case first <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				answer(w, client, "test-model")
			}))
			defer up.Close()
			pool := &Pool{}
			defer pool.Close()
			name, model := "codex", "gpt-5.6"
			if client == Claude {
				name, model = "claude", "claude-haiku-5-5"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 8)
			for i := 0; i < 8; i++ {
				go func(i int) {
					marker := fmt.Sprintf("CONCURRENT_MESSAGE_%04d_END", i)
					body := map[string]any{"model": model, "input": marker}
					if client == Claude {
						body = map[string]any{"model": model, "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": marker}}}
					}
					res, err := Do(ctx, Options{Client: client, Executable: filepath.Join(liveBinDir, name), Base: up.URL + "/v1", Key: "synthetic", Proxy: "direct", Pool: pool, Scope: "same-session"}, raw(body))
					if err == nil {
						b, e := io.ReadAll(res.Body)
						res.Body.Close()
						err = e
						if res.StatusCode != 200 {
							err = fmt.Errorf("status %d: %s", res.StatusCode, b)
						}
					}
					done <- err
				}(i)
			}
			for range 4 {
				select {
				case <-first:
				case <-ctx.Done():
					close(release)
					t.Fatal("first native requests did not reach service")
				}
			}
			close(release)
			for range 8 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if peak.Load() > 4 || sent.Load() != 8 {
				t.Fatalf("unexpected concurrency or request count: peak=%d sent=%d", peak.Load(), sent.Load())
			}
		})
	}
}
