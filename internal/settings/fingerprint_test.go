package settings

import (
	"os"
	"testing"
)

func TestFingerprintUpdatesDefaultOnAndKeepExplicitChoice(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"new settings", "", true},
		{"existing settings without a choice", `{"theme":"dark"}`, true},
		{"explicitly on", `{"fingerprintUpdates":true}`, true},
		{"explicitly off", `{"fingerprintUpdates":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if tc.body != "" {
				if err := os.MkdirAll(Dir(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(Path(), []byte(tc.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := Load()
			if s.FingerprintUpdates != tc.want {
				t.Fatalf("fingerprint updates = %v, want %v", s.FingerprintUpdates, tc.want)
			}
			for _, on := range []bool{false, true, false} {
				s.FingerprintUpdates = on
				if err := Save(s); err != nil {
					t.Fatal(err)
				}
				s = Load()
				if s.FingerprintUpdates != on {
					t.Fatalf("reloaded fingerprint updates = %v, saved %v", s.FingerprintUpdates, on)
				}
			}
		})
	}
}
