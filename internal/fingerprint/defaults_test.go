package fingerprint

import (
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
