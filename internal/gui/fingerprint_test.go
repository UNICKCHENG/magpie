package gui

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/fingerprint"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestFingerprintUpdatesSettings(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	if !settings.Load().FingerprintUpdates {
		t.Fatal("fingerprint updates must default to on")
	}
	h := Handler(nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/settings", nil))
	var shown settingsJSON
	if err := json.Unmarshal(w.Body.Bytes(), &shown); err != nil || w.Code != 200 {
		t.Fatalf("read settings: code=%d err=%v", w.Code, err)
	}
	if !shown.FingerprintUpdates {
		t.Fatal("settings response did not show the default-on choice")
	}
	for _, client := range []string{fingerprint.Codex, fingerprint.Claude} {
		if got := shown.FingerprintUpdate.Versions[client]; got != fingerprint.DefaultVersion(client) {
			t.Fatalf("initial status has %s version %q, want %q", client, got, fingerprint.DefaultVersion(client))
		}
	}
	for _, tc := range []struct {
		body string
		on   bool
	}{
		{`{"theme":"dark"}`, true}, // older page leaves the default alone
		{`{"fingerprintUpdates":false}`, false},
		{`{"theme":"light"}`, false}, // omitted field also preserves opt-out
		{`{"fingerprintUpdates":true}`, true},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/settings", strings.NewReader(tc.body)))
		if w.Code != 200 || settings.Load().FingerprintUpdates != tc.on {
			t.Fatalf("save %s: code=%d enabled=%v", tc.body, w.Code, settings.Load().FingerprintUpdates)
		}
		if !strings.Contains(w.Body.String(), `"fingerprintUpdate":`) {
			t.Fatal("settings response omits update status")
		}
	}
}
