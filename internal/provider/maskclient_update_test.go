package provider

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/fingerprint"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMaskUsesLearnedFingerprintWithoutCopyingSessions(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	identity := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	profiles := map[string]fingerprint.Profile{
		ClientClaudeCode: {Version: "2.1.999", Headers: map[string]string{"User-Agent": "claude-cli/2.1.999 (external, sdk-cli)", "X-App": "cli", "Anthropic-Beta": "claude-code-20250219", "X-Stainless-Package-Version": "0.999.0"}, Identity: identity},
		ClientCodex:      {Version: "0.999.0", Headers: map[string]string{"User-Agent": "codex_exec/0.999.0 (Linux; x86_64)", "Originator": "codex_exec"}},
	}
	b, _ := json.Marshal(map[string]any{"schema": 1, "profiles": profiles})
	dir := filepath.Join(settings.Dir(), "fingerprints")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runtime.GOOS+"-"+runtime.GOARCH+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{ClientClaudeCode, ClientCodex} {
		h := http.Header{"Authorization": {"Bearer synthetic-key"}, "Session-Id": {"original-session"}, "Anthropic-Beta": {"caller-beta"}}
		MaskHeaders(client, h)
		if h.Get("User-Agent") != profiles[client].Headers["User-Agent"] || h.Get("Authorization") != "Bearer synthetic-key" || h.Get("Session-Id") != "original-session" || h.Get("Anthropic-Beta") != "caller-beta" {
			t.Fatalf("incorrect updated headers: %v", h)
		}
	}
	for _, body := range []string{`{"messages":[]}`, `{"system":[{"type":"text","text":"Business prompt","cache_control":{"type":"ephemeral"}}],"messages":[]}`} {
		got := string(MaskBody(ClientClaudeCode, "", Anthropic, []byte(body)))
		if !strings.Contains(got, identity) || strings.Contains(got, "cc_version=") {
			t.Fatalf("incoherent learned identity: %s", got)
		}
		if strings.Contains(body, "cache_control") && !strings.Contains(got, `"cache_control":{"type":"ephemeral"}`) {
			t.Fatal("caller cache control lost")
		}
	}
}
