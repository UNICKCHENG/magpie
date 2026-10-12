package fingerprint

import (
	_ "embed"
	"encoding/json"
	"maps"
	"runtime"
	"strings"
)

// Bundled identity remains for subscription signing and explicit Test-as.
// Custom-provider inference uses the native executable, never these headers.
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
	Terminals map[string]string `json:"terminals"`
	OSNames   map[string]string `json:"osNames"`
	ArchNames map[string]string `json:"archNames"`
}

var defaults = func() map[string]definition {
	var d map[string]definition
	if err := json.Unmarshal(defaultJSON, &d); err != nil {
		panic("invalid bundled fingerprints: " + err.Error())
	}
	return d
}()

// Snapshot owns a bundled identity for the existing subscription/Test-as path.
type Snapshot struct {
	profile Profile
}

func DefaultVersion(client string) string { return defaults[client].Version }

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
	return Snapshot{profile: p}
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

// hasIdentity recognizes both the native CLI and the Agent SDK entrypoint.
func hasIdentity(text string) bool {
	for _, prefix := range defaults[Claude].IdentityPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}
