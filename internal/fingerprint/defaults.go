package fingerprint

import (
	_ "embed"
	"encoding/json"
	"maps"
	"net/http"
	"runtime"
	"strings"
)

// defaults.json is the single source for the bundled client versions, headers,
// identity text and body defaults. Learned profiles override stable identity
// fields only; a failed update never requires editing a second set of constants.
//
//go:embed defaults.json
var defaultJSON []byte

// CodexInstructions is the shared upstream prompt, kept byte for byte from
// openai/codex core/gpt-5.2-codex_prompt.md (Apache-2.0).
//
//go:embed codex_prompt.md
var CodexInstructions string

type definition struct {
	IdentityPrefixes []string `json:"identityPrefixes"`
	Profile
	BillingHeader string                     `json:"billingHeader"`
	Body          map[string]json.RawMessage `json:"body"`
	ForceBody     []string                   `json:"forceBody"`
	Terminals     map[string]string          `json:"terminals"`
	OSNames       map[string]string          `json:"osNames"`
	ArchNames     map[string]string          `json:"archNames"`
}

var defaults = func() map[string]definition {
	var d map[string]definition
	if err := json.Unmarshal(defaultJSON, &d); err != nil {
		panic("invalid bundled fingerprints: " + err.Error())
	}
	return d
}()

// Snapshot is one selected identity, held for an entire outgoing request.
// It owns its header map, so an update cannot change a request halfway through.
type Snapshot struct {
	client  string
	profile Profile
	billing string
}

func DefaultVersion(client string) string { return defaults[client].Version }

// Resolve prefers a verified capture, then the bundled definition. Only the
// built-in Codex identity asks the provider for its observed/installed version.
func Resolve(client string, codexVersion func() string) Snapshot {
	if p, ok := Current(client); ok {
		return Snapshot{client: client, profile: p}
	}
	v := ""
	if client == Codex && codexVersion != nil {
		v = codexVersion()
	}
	return Builtin(client, v)
}

// Builtin also supplies the subscription path's identity without opting it
// into learned profiles. version, when set, is that path's observed version.
func Builtin(client, version string) Snapshot {
	d, ok := defaults[client]
	if !ok {
		return Snapshot{}
	}
	p := d.Profile
	p.Headers = maps.Clone(p.Headers)
	if version != "" {
		p.Version = version
	}
	osName := platformName(d.OSNames, runtime.GOOS)
	if client == Codex {
		codexUA.Do(func() { codexUA.os = codexOS() })
		osName = codexUA.os
	}
	replace := strings.NewReplacer("{version}", p.Version, "{os}", osName,
		"{arch}", platformName(d.ArchNames, runtime.GOARCH), "{terminal}", platformName(d.Terminals, runtime.GOOS))
	for k, v := range p.Headers {
		p.Headers[k] = replace.Replace(v)
	}
	return Snapshot{client: client, profile: p, billing: replace.Replace(d.BillingHeader)}
}

func platformName(names map[string]string, platform string) string {
	if v := names[platform]; v != "" {
		return v
	}
	if v := names["default"]; v != "" {
		return v
	}
	return platform
}

// Header reads a field by its spelling in the definition, without exposing a
// mutable shared map. Native subscription signing uses only its own fields.
func (s Snapshot) Header(name string) string { return s.profile.Headers[name] }

func (s Snapshot) ApplyHeaders(h http.Header) {
	for name, value := range s.profile.Headers {
		if (name == "Anthropic-Beta" || name == "Anthropic-Version") && h.Get(name) != "" {
			continue
		}
		h.Set(name, value)
	}
}

// hasIdentity recognizes both the native CLI and the Agent SDK entrypoint.
func hasIdentity(text string) bool {
	for _, prefix := range defaults[Claude].IdentityPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}
