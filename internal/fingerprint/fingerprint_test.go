package fingerprint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/nativeclient"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func sample(client, version string) Profile {
	if client == Codex {
		return Profile{Version: version, Headers: map[string]string{"User-Agent": "codex_exec/" + version + " (Mac OS 26.6.2; arm64) unknown (codex_exec; " + version + ")", "Originator": "codex_exec"}}
	}
	return Profile{Version: version, Headers: map[string]string{"User-Agent": "claude-cli/" + version + " (external, sdk-cli)", "X-App": "cli", "Anthropic-Beta": "claude-code-20250219"}, Identity: "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."}
}

func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	old := collectProfile
	t.Cleanup(func() { collectProfile = old; worker.Lock(); worker.force = false; worker.Unlock() })
}

func enable(t *testing.T, on bool) {
	t.Helper()
	s := settings.Load()
	s.FingerprintUpdates = on
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRespectsOffAndKeepsLastGoodPair(t *testing.T) {
	isolate(t)
	wantVersions := map[string]string{Codex: DefaultVersion(Codex), Claude: DefaultVersion(Claude)}
	checkVersions := func() {
		t.Helper()
		if got := UpdateStatus().Versions; !reflect.DeepEqual(got, wantVersions) {
			t.Fatalf("visible fingerprint versions = %v, want %v", got, wantVersions)
		}
	}
	checkVersions()
	calls := 0
	collectProfile = func(_ context.Context, client, dir string, _ Profile) (Profile, error) {
		checkVersions() // an unfinished capture still shows the bundled pair
		calls++
		return sample(client, "1.2.3"), nil
	}
	enable(t, false)
	if err := update(context.Background()); err != nil || calls != 0 {
		t.Fatalf("disabled update ran collector: calls=%d err=%v", calls, err)
	}
	enable(t, true)
	if err := update(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := readState()
	if calls != 2 || len(good.Profiles) != 2 || good.Updated.IsZero() || !good.Started.IsZero() {
		t.Fatalf("update did not install the pair: %+v", good)
	}
	wantVersions = map[string]string{Codex: "1.2.3", Claude: "1.2.3"}
	checkVersions()
	collectProfile = func(_ context.Context, client, dir string, _ Profile) (Profile, error) {
		if client == Claude {
			return Profile{}, errors.New("missing identity")
		}
		return sample(client, "1.2.4"), nil
	}
	if err := update(context.Background()); err == nil {
		t.Fatal("incomplete capture accepted")
	}
	after := readState()
	if !reflect.DeepEqual(after.Profiles, good.Profiles) || after.Error == "" || !after.Updated.Equal(good.Updated) {
		t.Fatalf("failed capture replaced working profiles: %+v", after)
	}
	checkVersions()
	enable(t, false)
	checkVersions()
	p, ok := Current(Codex)
	if !ok || p.Version != "1.2.3" {
		t.Fatal("off discarded saved profile")
	}
	p.Headers["User-Agent"] = "changed"
	if p, _ = Current(Codex); p.Headers["User-Agent"] == "changed" {
		t.Fatal("caller mutated shared profile")
	}
}

func TestDisableCancelsUpdateWithoutInstallingItsResult(t *testing.T) {
	isolate(t)
	enable(t, true)
	started := make(chan struct{})
	collectProfile = func(ctx context.Context, client, dir string, _ Profile) (Profile, error) {
		close(started)
		<-ctx.Done()
		return sample(client, "1.2.3"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- update(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-started
	if !UpdateStatus().Running {
		t.Fatal("active capture not shown")
	}
	enable(t, false)
	Changed()
	// Wait via a separate channel so cleanup can always wait too.
	result := <-done
	done <- result
	if result != nil {
		t.Fatal(result)
	}
	if _, ok := Current(Codex); ok {
		t.Fatal("disabled capture installed a profile")
	}
	if UpdateStatus().Running || !readState().Checked.IsZero() {
		t.Fatal("cancellation left an active task or postponed retry")
	}
}

func TestSchedulerStopsAndWaitsForItsWorker(t *testing.T) {
	isolate(t)
	if !settings.Load().FingerprintUpdates {
		t.Fatal("fresh settings did not enable the first fingerprint check")
	}
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	collectProfile = func(ctx context.Context, client, dir string, _ Profile) (Profile, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return Profile{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Keep(ctx) }()
	released := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { cancel(); released(); <-done })
	<-started
	cancel()
	<-cancelled
	select {
	case <-done:
		t.Fatal("scheduler returned before its worker cleaned up")
	default:
	}
	released()
	<-done
	if UpdateStatus().Running {
		t.Fatal("stopped scheduler left its worker active")
	}
}

func TestFingerprintScheduleSurvivesRestart(t *testing.T) {
	isolate(t)
	checked := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	if err := writeState(state{Checked: checked}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		elapsed time.Duration
		want    bool
	}{{-time.Hour, false}, {23 * time.Hour, false}, {24 * time.Hour, true}, {25 * time.Hour, true}} {
		if got := due(readState().Checked, checked.Add(tc.elapsed)); got != tc.want {
			t.Fatalf("elapsed %s: due=%v", tc.elapsed, got)
		}
	}
	if !due(time.Time{}, checked) {
		t.Fatal("first enable does not run")
	}
}

func TestUnchangedVersionDoesNotDownloadOrRunCLI(t *testing.T) {
	isolate(t)
	oldURL := codexReleaseURL
	t.Cleanup(func() { codexReleaseURL = oldURL })
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"tag_name":"rust-v0.162.1","assets":[]}`))
	}))
	defer srv.Close()
	codexReleaseURL = srv.URL
	old := sample(Codex, "0.162.1")
	source := filepath.Join(t.TempDir(), "native-client")
	if err := os.WriteFile(source, []byte("validated native executable"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	old.Binary, err = storeBinary(Codex, old.Version, source)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for range 24 {
		p, err := downloadAndCapture(context.Background(), Codex, dir, old)
		if err != nil || !reflect.DeepEqual(p, old) {
			t.Fatalf("unchanged release: %v", err)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 || requests != 24 {
		t.Fatalf("version checks downloaded files: files=%v requests=%d error=%v", files, requests, err)
	}
}

func TestCaptureKeepsIdentityOnly(t *testing.T) {
	p := sample(Claude, "2.1.296")
	h := http.Header{}
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Claude-Code-Session-Id", "Session-Id"} {
		h.Set(k, "SYNTHETIC_PRIVATE_VALUE")
	}
	body := map[string]json.RawMessage{"system": json.RawMessage(`[{"type":"text","text":"x-anthropic-billing-header: dynamic-value"},{"type":"text","text":"` + p.Identity + `"},{"type":"text","text":"business instruction"}]`), "metadata": json.RawMessage(`{"user_id":"private-session"}`)}
	got, err := observed(Claude, p.Version, h, body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("capture retained non-identity fields: %+v", got)
	}
	delete(body, "system")
	if _, err := observed(Claude, p.Version, h, body); err == nil {
		t.Fatal("missing identity accepted")
	}
	p.Headers["Authorization"] = "synthetic-key"
	if err := validate(Claude, p); err == nil {
		t.Fatal("credential in profile accepted")
	}
	t.Setenv("OPENAI_API_KEY", "SYNTHETIC_PRIVATE_VALUE")
	t.Setenv("ANTHROPIC_API_KEY", "SYNTHETIC_PRIVATE_VALUE")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "SYNTHETIC_PRIVATE_VALUE")
	if strings.Contains(strings.Join(nativeclient.Environment(t.TempDir(), "direct"), "\n"), "SYNTHETIC_PRIVATE_VALUE") {
		t.Fatal("real credentials inherited")
	}
}

func TestDownloadedClientMustMatchRelease(t *testing.T) {
	data := []byte("synthetic executable")
	h := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	for _, tc := range []struct {
		name, checksum string
		size           int64
		ok             bool
	}{
		{"valid", hex.EncodeToString(h[:]), int64(len(data)), true},
		{"wrong hash", strings.Repeat("0", 64), int64(len(data)), false},
		{"wrong size", hex.EncodeToString(h[:]), int64(len(data) + 1), false},
		{"no hash", "", int64(len(data)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client")
			err := download(context.Background(), srv.URL, tc.checksum, tc.size, path)
			if (err == nil) != tc.ok {
				t.Fatalf("download: %v", err)
			}
			if !tc.ok {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed download kept executable")
				}
			}
		})
	}
}

func TestCodexArchiveExtractsOnlyItsExecutable(t *testing.T) {
	for _, kind := range []byte{tar.TypeReg, tar.TypeSymlink} {
		t.Run(string(kind), func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			h := &tar.Header{Name: "codex-aarch64-apple-darwin", Mode: 0700, Typeflag: kind}
			if kind == tar.TypeReg {
				h.Size = 2
			} else {
				h.Linkname = "../../outside"
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if kind == tar.TypeReg {
				_, _ = tw.Write([]byte("ok"))
			}
			_ = tw.Close()
			_ = gz.Close()
			dir := t.TempDir()
			archive := filepath.Join(dir, "a.gz")
			if err := os.WriteFile(archive, buf.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			err := extract(archive, h.Name, filepath.Join(dir, "exe"))
			if (err == nil) != (kind == tar.TypeReg) {
				t.Fatalf("extract type %v: %v", kind, err)
			}
		})
	}
}

// Opt-in real release download and local capture, with synthetic credentials.
func TestOfficialClientsCapture(t *testing.T) {
	if os.Getenv("MAGPIE_TEST_FINGERPRINT_LIVE") != "1" {
		t.Skip("set MAGPIE_TEST_FINGERPRINT_LIVE=1 for official downloads")
	}
	isolate(t)
	enable(t, true)
	if proxy := os.Getenv("MAGPIE_TEST_FINGERPRINT_PROXY"); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			t.Fatal(err)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(u)
		previous := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: transport}
		t.Cleanup(func() { transport.CloseIdleConnections(); http.DefaultClient = previous })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := update(ctx); err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{Codex, Claude} {
		p, ok := Current(client)
		if !ok {
			t.Fatal("no profile", client)
		}
		t.Logf("%s %s captured and validated", client, p.Version)
	}
}
