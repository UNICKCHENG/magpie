// Package fingerprint manages custom-provider client builds and legacy identity
// captures. Its saved state contains no credentials or conversation content.
package fingerprint

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/nativeclient"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

const Codex = nativeclient.Codex
const Claude = nativeclient.Claude

// Profile records the validated build and nonsecret evidence of its local
// smoke test. Inference never copies this identity onto requests. Previous
// keeps the last build available for rollback.
type Profile struct {
	Version  string            `json:"version"`
	Headers  map[string]string `json:"headers"`
	Identity string            `json:"identity,omitempty"`
	Binary   *Binary           `json:"binary,omitempty"`
	Previous *Binary           `json:"previous,omitempty"`
}

type state struct {
	Schema   int                `json:"schema"`
	Started  time.Time          `json:"started,omitzero"`
	Checked  time.Time          `json:"checked,omitzero"`
	Updated  time.Time          `json:"updated,omitzero"`
	Error    string             `json:"error,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

// Status is safe to show in Settings. The profiles themselves stay on disk.
type Status struct {
	Running  bool              `json:"running"`
	Checked  time.Time         `json:"checked,omitzero"`
	Updated  time.Time         `json:"updated,omitzero"`
	Next     time.Time         `json:"next,omitzero"`
	Error    string            `json:"error,omitempty"`
	Versions map[string]string `json:"versions,omitempty"`
}

func statePath() string {
	return filepath.Join(settings.Dir(), "fingerprints", runtime.GOOS+"-"+runtime.GOARCH+".json")
}

func readState() state {
	s, _ := filememo.Read("fingerprints", statePath(), func(b []byte) (state, error) {
		var s state
		if len(b) > 64<<10 || json.Unmarshal(b, &s) != nil || s.Schema != 1 {
			return state{}, errors.New("invalid fingerprint state")
		}
		for client, p := range s.Profiles {
			if err := validate(client, p); err != nil {
				return state{}, err
			}
		}
		return s, nil
	})
	return s
}

func writeState(s state) error {
	s.Schema = 1
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(statePath()), 0700); err != nil {
		return err
	}
	if err = steady.WriteFile(statePath(), b, 0600); err == nil {
		filememo.Forget()
	}
	return err
}

// Current keeps serving the last successful profile even after updates are off.
func Current(client string) (Profile, bool) {
	p, ok := readState().Profiles[client]
	p.Headers = maps.Clone(p.Headers)
	if p.Binary != nil {
		b := *p.Binary
		p.Binary = &b
	}
	if p.Previous != nil {
		b := *p.Previous
		p.Previous = &b
	}
	return p, ok
}

func UpdateStatus() Status {
	s := readState()
	worker.Lock()
	running := worker.cancel != nil || (!s.Started.IsZero() && time.Now().Before(s.Started.Add(10*time.Minute)))
	worker.Unlock()
	out := Status{Running: running, Checked: s.Checked, Updated: s.Updated, Error: s.Error,
		Versions: map[string]string{Codex: DefaultVersion(Codex), Claude: DefaultVersion(Claude)}}
	if settings.Load().FingerprintUpdates && !s.Checked.IsZero() {
		out.Next = s.Checked.Add(24 * time.Hour)
	}
	for c, p := range s.Profiles {
		out.Versions[c] = p.Version
	}
	for _, client := range []string{Codex, Claude} {
		if b := selectedBinary(client, s.Profiles[client]); b != nil {
			out.Versions[client] = b.Version
		}
	}
	return out
}

var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func allowedHeaders(client string) []string {
	if client == Codex {
		return []string{"User-Agent", "Originator", "Version", "X-Codex-Beta-Features"}
	}
	if client == Claude {
		return []string{"User-Agent", "X-App", "Anthropic-Version", "Anthropic-Beta", "Anthropic-Dangerous-Direct-Browser-Access", "X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Retry-Count", "X-Stainless-Timeout"}
	}
	return nil
}

func validate(client string, p Profile) error {
	if !versionRE.MatchString(p.Version) {
		return errors.New("invalid client version")
	}
	if p.Binary != nil {
		if _, err := binaryPath(client, *p.Binary); err != nil || p.Binary.Version != p.Version {
			return errors.New("invalid native client version")
		}
	}
	if p.Previous != nil {
		if _, err := binaryPath(client, *p.Previous); err != nil {
			return err
		}
	}
	allowed := map[string]bool{}
	for _, k := range allowedHeaders(client) {
		allowed[k] = true
	}
	for k, v := range p.Headers {
		if !allowed[k] || len(v) > 4096 || strings.ContainsAny(v, "\r\n") {
			return errors.New("invalid client identity header")
		}
	}
	h := http.Header{}
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	switch client {
	case Codex:
		if !(strings.HasPrefix(h.Get("User-Agent"), "codex") || strings.HasPrefix(h.Get("User-Agent"), "magpie_native/")) || !strings.Contains(h.Get("User-Agent"), "/"+p.Version+" ") || h.Get("Originator") == "" || p.Identity != "" {
			return errors.New("Codex identity was not captured")
		}
		if v := h.Get("Version"); v != "" && v != p.Version {
			return errors.New("Codex version headers disagree")
		}
	case Claude:
		if !strings.HasPrefix(h.Get("User-Agent"), "claude-cli/"+p.Version+" ") || h.Get("X-App") != "cli" || !strings.Contains(h.Get("Anthropic-Beta"), "claude-code-") || len(p.Identity) > 512 || !hasIdentity(p.Identity) {
			return errors.New("Claude Code identity was not captured")
		}
	default:
		return errors.New("unknown fingerprint client")
	}
	return nil
}
