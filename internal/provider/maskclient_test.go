package provider

import (
	"testing"
)

func TestMaskClientScope(t *testing.T) {
	detectHome(t)
	for _, tc := range []struct {
		name, api, client, preset string
		want                      string
	}{
		{"responses", "responses", ClientCodex, "", ClientCodex},
		{"anthropic", "anthropic", ClientClaudeCode, "", ClientClaudeCode},
		{"off", "responses", "", "", ""},
		{"chat", "chat", ClientCodex, "", ""},
		{"wrong client", "responses", ClientClaudeCode, "", ""},
		{"preset", "responses", ClientCodex, "openai", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{ID: "relay", Name: "Relay", Key: "k", BaseAPI: tc.api, MaskClient: tc.client, Preset: tc.preset,
				Chat: "https://relay.example/v1", Responses: "https://relay.example/v1", Anthropic: "https://relay.example"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			got, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.MaskClient != tc.want {
				t.Fatalf("mask %q, want %q", got.MaskClient, tc.want)
			}
		})
	}
}
