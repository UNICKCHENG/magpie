package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/codexcat"
)

// MaskProtocol is the custom provider API whose requests the user opted
// to identify as a coding client. Other APIs keep their usual behavior.
func (p Provider) MaskProtocol() Protocol {
	if p.Account != nil || p.Preset != "" {
		return ""
	}
	switch {
	case p.BaseAPI == "responses" && p.Responses != "" && p.MaskClient == ClientCodex:
		return Responses
	case p.BaseAPI == "anthropic" && p.Anthropic != "" && p.MaskClient == ClientClaudeCode:
		return Anthropic
	}
	return ""
}

// Fixed Claude Code identity; Codex reuses codexVersion and codexUserAgent.
const claudeCodeMaskVersion = "2.1.296"
const claudeCodeMaskIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
const claudeCodeMaskBetas = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24,dangerous-tool-use-2026-09-03,afk-mode-2026-01-31"

// MaskHeaders puts client's identity on h: the headers it tells itself by,
// which a relay that serves only it reads. anthropic-beta is set only when
// none is there: what the agent asked for (or the user set on the provider,
// which Sign applies after) stays as it is. What authenticates the request
// is not touched.
func MaskHeaders(client string, h http.Header) {
	if client != ClientCodex && client != ClientClaudeCode {
		return
	}
	switch client {
	case ClientCodex:
		v := codexVersion()
		h.Set("User-Agent", codexUserAgent(v))
		h.Set("originator", "codex_cli_rs")
		h.Set("version", v)
		// the beta flags this build turns on over plain HTTP
		h.Set("x-codex-beta-features", "remote_compaction_v2")
	case ClientClaudeCode:
		version := claudeCodeMaskVersion
		entrypoint := "cli"
		h.Set("User-Agent", claudeCodeUserAgent(version, entrypoint))
		h.Set("x-app", "cli")
		// the SDK's browser-access workaround, observed on a real request
		h.Set("anthropic-dangerous-direct-browser-access", "true")
		// a real Claude Code is built on the Anthropic TS SDK and says so;
		// the runtime version is pinned, not the host's, so it names a
		// coherent client
		h.Set("X-Stainless-Lang", "js")
		h.Set("X-Stainless-Package-Version", "0.128.0")
		h.Set("X-Stainless-OS", stainlessOS())
		h.Set("X-Stainless-Arch", stainlessArch())
		h.Set("X-Stainless-Runtime", "node")
		h.Set("X-Stainless-Runtime-Version", "v26.3.0")
		h.Set("X-Stainless-Retry-Count", "0")
		h.Set("X-Stainless-Timeout", "300")
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", "2023-06-01")
		}
		if h.Get("anthropic-beta") == "" {
			h.Set("anthropic-beta", claudeCodeMaskBetas)
		}
	}
}

// claudeCodeUserAgent is the User-Agent Claude Code of version v sends:
// "claude-cli/2.1.296 (external, cli)". The paren names the entrypoint
// (cli, or sdk-cli for `claude -p` style use).
func claudeCodeUserAgent(v, entrypoint string) string {
	return "claude-cli/" + v + " (external, " + entrypoint + ")"
}

func stainlessOS() string {
	if v, ok := map[string]string{"darwin": "MacOS", "windows": "Windows"}[runtime.GOOS]; ok {
		return v
	}
	return "Linux"
}

func stainlessArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

// MaskSessionHeaders names the conversation a masked request belongs to the
// way the client does: the same conversation the same id every request, a
// new one new ids, as a real client's are. session is the conversation's
// stable id (the gateway's conversationID); it is hashed, never sent, so
// the relay can't read what it came from.
func MaskSessionHeaders(client, session string, h http.Header) {
	u := stableUUID("mask " + client + " " + session)
	switch client {
	case ClientCodex:
		// session, thread and client request all name the conversation, the
		// window its own id (observed on codex-cli 0.162.1)
		h.Set("session-id", u)
		h.Set("thread-id", u)
		h.Set("x-client-request-id", u)
		h.Set("x-codex-window-id", u+":0")
	case ClientClaudeCode:
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

// MaskBody shapes a request body as client sends one on proto, filling in
// only what it lacks — a request the client itself made already has its
// own. key is the key the request goes with; it is hashed, never sent.
func MaskBody(client, key string, proto Protocol, body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	switch client {
	case ClientCodex:
		if proto != Responses {
			return body
		}
		// as Codex asks it: nothing kept on OpenAI's side, reasoning
		// payloads back and summarized, its base prompt as instructions
		m["store"] = json.RawMessage("false")
		if _, ok := m["include"]; !ok {
			m["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
		}
		if _, ok := m["reasoning"]; !ok {
			m["reasoning"] = json.RawMessage(`{"summary":"auto"}`)
		}
		if _, ok := m["instructions"]; !ok {
			if b, err := json.Marshal(codexcat.Prompt); err == nil {
				m["instructions"] = b
			}
		}
	case ClientClaudeCode:
		if proto != Anthropic {
			return body
		}
		// Claude Code keeps its identity even with a caller's system prompt.
		// Relays can require that line as well as the client headers.
		if _, ok := m["system"]; !ok {
			marker := "x-anthropic-billing-header: cc_version=" + claudeCodeMaskVersion + ".406; cc_entrypoint=cli;"
			m["system"] = json.RawMessage(`[{"type":"text","text":` + strconv.Quote(marker) +
				`},{"type":"text","text":` + strconv.Quote(claudeCodeMaskIdentity) + `}]`)
		} else {
			m["system"] = maskClaudeCodeSystem(m["system"])
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

// maskClaudeCodeSystem adds only the missing identity. Original blocks keep
// their order and cache controls; a native CLI or SDK identity stays as sent.
func maskClaudeCodeSystem(system json.RawMessage) json.RawMessage {
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
		if strings.HasPrefix(block.Text, "You are Claude Code, Anthropic's official CLI for Claude") ||
			strings.HasPrefix(block.Text, "You are a Claude agent, built on Anthropic's Claude Agent SDK.") {
			return system
		}
		if i == 0 && strings.HasPrefix(block.Text, "x-anthropic-billing-header:") {
			at = 1
		}
	}
	identity := json.RawMessage(`{"type":"text","text":` + strconv.Quote(claudeCodeMaskIdentity) + `}`)
	blocks = append(blocks, nil)
	copy(blocks[at+1:], blocks[at:])
	blocks[at] = identity
	out, err := json.Marshal(blocks)
	if err != nil {
		return system
	}
	return out
}
