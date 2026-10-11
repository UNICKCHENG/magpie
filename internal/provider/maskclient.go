package provider

import (
	"net/http"

	"github.com/yetone/magpie/internal/fingerprint"
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

// ClientFingerprint selects once per request so an update cannot mix the
// old body's identity with new headers. Client discovery still belongs here.
func ClientFingerprint(client string) fingerprint.Snapshot {
	return fingerprint.Resolve(client, func() string { return codexVersion() })
}

func MaskHeaders(client string, h http.Header) { ClientFingerprint(client).ApplyHeaders(h) }
func MaskBody(client, key string, proto Protocol, body []byte) []byte {
	return ClientFingerprint(client).ApplyBody(key, string(proto), body)
}
func MaskSessionHeaders(client, session string, h http.Header) {
	fingerprint.SessionHeaders(client, session, h)
}
