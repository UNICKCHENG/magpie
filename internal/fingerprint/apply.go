package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// SessionHeaders names the conversation a masked request belongs to the
// way the client does: the same conversation the same id every request, a
// new one new ids, as a real client's are. session is the conversation's
// stable id (the gateway's conversationID); it is hashed, never sent, so
// the relay can't read what it came from.
func SessionHeaders(client, session string, h http.Header) {
	u := stableUUID("mask " + client + " " + session)
	switch client {
	case Codex:
		// session, thread and client request all name the conversation, the
		// window its own id (observed on codex-cli 0.162.1)
		h.Set("session-id", u)
		h.Set("thread-id", u)
		h.Set("x-client-request-id", u)
		h.Set("x-codex-window-id", u+":0")
	case Claude:
		h.Set("x-claude-code-session-id", u)
	}
}

// stableUUID derives a UUID-shaped identity from seed: the same seed the
// same id across requests and restarts, a different seed a different one.
func stableUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

// ApplyBody fills the request's missing client identity using this snapshot.
// Body defaults come from the bundled definition; captures never supply prompts,
// tools, cache controls or session identifiers.
func (s Snapshot) ApplyBody(key, proto string, body []byte) []byte {
	if s.client != Codex && s.client != Claude {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	switch s.client {
	case Codex:
		if proto != "responses" {
			return body
		}
		for name, value := range defaults[Codex].Body {
			if _, exists := m[name]; !exists || slices.Contains(defaults[Codex].ForceBody, name) {
				m[name] = value
			}
		}
		if _, exists := m["instructions"]; !exists {
			m["instructions"], _ = json.Marshal(CodexInstructions)
		}
	case Claude:
		if proto != "anthropic" {
			return body
		}
		// Claude Code keeps its identity even with a caller's system prompt.
		// Relays can require that line as well as the client headers.
		if _, exists := m["system"]; !exists && s.billing != "" {
			m["system"] = json.RawMessage(`[{"type":"text","text":` + strconv.Quote(s.billing) +
				`},{"type":"text","text":` + strconv.Quote(s.profile.Identity) + `}]`)
		} else {
			m["system"] = addIdentity(m["system"], s.profile.Identity)
		}
		// one user per key, stable across requests and restarts, in the
		// shape a real request carries: a JSON string with a device id and
		// the session id
		if key != "" {
			if _, ok := m["metadata"]; !ok {
				device := sha256.Sum256([]byte("cc device " + key))
				id := `{"device_id":"` + hex.EncodeToString(device[:]) + `","account_uuid":"","session_id":"` + stableUUID("cc "+key) + `"}`
				m["metadata"] = json.RawMessage(`{"user_id":` + strconv.Quote(id) + `}`)
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// addIdentity preserves native CLI/SDK identities and the caller's cache controls.
func addIdentity(system json.RawMessage, identityText string) json.RawMessage {
	if len(system) == 0 {
		system = json.RawMessage("[]")
	}
	var blocks []json.RawMessage
	var text string
	if json.Unmarshal(system, &text) == nil {
		if text != "" {
			blocks = append(blocks, json.RawMessage(`{"type":"text","text":`+string(system)+`}`))
		}
	} else if json.Unmarshal(system, &blocks) != nil {
		return system
	}
	at := 0
	for i, raw := range blocks {
		var block struct{ Type, Text string }
		if json.Unmarshal(raw, &block) != nil || block.Type != "text" {
			continue
		}
		if hasIdentity(block.Text) {
			return system
		}
		if i == 0 && strings.HasPrefix(block.Text, "x-anthropic-billing-header:") {
			at = 1
		}
	}
	identity := json.RawMessage(`{"type":"text","text":` + strconv.Quote(identityText) + `}`)
	blocks = append(blocks, nil)
	copy(blocks[at+1:], blocks[at:])
	blocks[at] = identity
	out, err := json.Marshal(blocks)
	if err != nil {
		return system
	}
	return out
}
