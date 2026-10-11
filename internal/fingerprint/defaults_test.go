package fingerprint

import (
	"net/http"
	"strings"
	"testing"
)

func TestBundledFingerprintsAreComplete(t *testing.T) {
	for _, client := range []string{Codex, Claude} {
		s := Builtin(client, "")
		if err := validate(client, s.profile); err != nil {
			t.Fatalf("%s defaults: %v", client, err)
		}
		for k, v := range s.profile.Headers {
			if strings.ContainsAny(v, "{}") {
				t.Fatalf("unresolved %s header %s=%s", client, k, v)
			}
		}
	}
	if strings.TrimSpace(CodexInstructions) == "" {
		t.Fatal("Codex prompt was not bundled")
	}
	if got := Builtin(Codex, "1.2.3"); got.Header("Version") != "1.2.3" || !strings.Contains(got.Header("User-Agent"), "/1.2.3 ") {
		t.Fatal("observed version is not applied consistently")
	}
}

func TestFingerprintSnapshotSurvivesAnUpdate(t *testing.T) {
	isolate(t)
	before := sample(Claude, "2.1.296")
	if err := writeState(state{Profiles: map[string]Profile{Claude: before}}); err != nil {
		t.Fatal(err)
	}
	selected := Resolve(Claude, nil)
	after := sample(Claude, "2.1.999")
	after.Identity = "You are Claude Code, Anthropic's official CLI for Claude."
	if err := writeState(state{Profiles: map[string]Profile{Claude: after}}); err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	selected.ApplyHeaders(h)
	body := string(selected.ApplyBody("", "anthropic", []byte(`{"messages":[]}`)))
	if h.Get("User-Agent") != before.Headers["User-Agent"] || !strings.Contains(body, before.Identity) {
		t.Fatal("in-flight request mixed profiles across an update")
	}
	if got := Resolve(Claude, nil).Header("User-Agent"); got != after.Headers["User-Agent"] {
		t.Fatal("next request did not see new profile")
	}
}

func TestBrokenStoredFingerprintFallsBackToBundledDefinition(t *testing.T) {
	isolate(t)
	bad := sample(Claude, "2.1.999")
	bad.Headers["Authorization"] = "synthetic-secret"
	if err := writeState(state{Profiles: map[string]Profile{Claude: bad}}); err != nil {
		t.Fatal(err)
	}
	s := Resolve(Claude, nil)
	if s.Header("User-Agent") != Builtin(Claude, "").Header("User-Agent") || s.Header("Authorization") != "" {
		t.Fatal("unvalidated profile did not fall back")
	}
}
