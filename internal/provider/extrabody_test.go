package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Test and Detect apply extra fields after the client fingerprint, as the
// gateway does: ordinary fields fill gaps and !fields replace defaults.
func TestExtraBodyWithClientFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name, client, extra, field, want string
		proto                            Protocol
	}{
		{"codex default", ClientCodex, `{"include":["other"]}`, "include.0", "reasoning.encrypted_content", Responses},
		{"codex forced", ClientCodex, `{"!store":true}`, "store", "true", Responses},
		{"claude default", ClientClaudeCode, `{"metadata":{"user_id":"custom"}}`, "metadata.user_id", "device_id", Anthropic},
		{"claude forced", ClientClaudeCode, `{"!metadata":{"user_id":"custom"}}`, "metadata.user_id", "custom", Anthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detectHome(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, `{"data":[{"id":"m"}]}`)
					return
				}
				body, _ := io.ReadAll(r.Body)
				got := gjson.GetBytes(body, tc.field).String()
				if got != tc.want && !(tc.want == "device_id" && strings.Contains(got, `"device_id"`)) {
					http.Error(w, "extra body precedence: "+tc.field+" = "+got+", want "+tc.want, http.StatusBadRequest)
					return
				}
				if !strings.HasPrefix(gjson.GetBytes(body, "prompt_cache_key").String(), "magpie-test-") {
					http.Error(w, "missing test session", http.StatusBadRequest)
					return
				}
				io.WriteString(w, `{"status":"completed","output":[]}`)
			}))
			t.Cleanup(up.Close)
			p := Provider{ID: "relay", Key: "sk-relay", BaseAPI: string(tc.proto), MaskClient: tc.client, Models: []string{"m"}}
			if tc.proto == Responses {
				p.Responses = up.URL + "/v1"
			} else {
				p.Anthropic = up.URL
			}
			if err := json.Unmarshal([]byte(tc.extra), &p.ExtraBody); err != nil {
				t.Fatal(err)
			}
			p.ExtraBody["prompt_cache_key"] = json.RawMessage(`"${sessionId}"`)
			ctx := context.Background()
			for name, run := range map[string]func() []Result{
				"Test":       func() []Result { return p.Test(ctx) },
				"TestModels": func() []Result { return p.TestModels(ctx, []string{"m"}) },
			} {
				if rs := run(); len(rs) != 1 || !rs[0].OK {
					t.Errorf("%s: %+v", name, rs)
				}
			}
			ds, err := p.Detect(ctx, up.URL, "m")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range ds {
				if d.Protocol == tc.proto {
					found = true
					if !d.OK {
						t.Errorf("Detect: %+v", d)
					}
				}
			}
			if !found {
				t.Fatal("Detect skipped the masked protocol")
			}
		})
	}
}

// The editor's Test and Detect ask with the extra fields too, so a relay
// that turns a request away without a prompt_cache_key (#1604) answers
// them, on a session id of the test's own.
func TestExtraBodyTested(t *testing.T) {
	detectHome(t)
	const model = "gpt-test"
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
		case "POST /v1/responses":
			var in struct {
				Key string `json:"prompt_cache_key"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Key == "" {
				http.Error(w, `{"error":{"message":"prompt_cache_key is required"}}`, http.StatusBadRequest)
				return
			}
			keys = append(keys, in.Key)
			io.WriteString(w, `{"object":"response","status":"completed","output":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Key: "sk-relay", Responses: srv.URL + "/v1", Models: []string{model},
		ExtraBody: ExtraBody{"prompt_cache_key": json.RawMessage(`"${sessionId}"`)}}
	ctx := context.Background()
	if rs := p.Test(ctx); len(rs) != 1 || !rs[0].OK {
		t.Fatalf("Test: %+v", rs)
	}
	if rs := p.TestModels(ctx, []string{model}); len(rs) != 1 || !rs[0].OK {
		t.Fatalf("TestModels: %+v", rs)
	}
	rs, err := p.Detect(ctx, srv.URL, model)
	if err != nil || len(rs) != len(detectProtocols) || !rs[1].Result.OK {
		t.Fatalf("Detect: %v, %+v", err, rs)
	}
	if len(keys) != 3 || !strings.HasPrefix(keys[0], "magpie-test-") || keys[0] == keys[1] {
		t.Errorf("asked with %q", keys)
	}
}

func TestExtraBodyOK(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{`{}`, ""},
		{`{"prompt_cache_key":"${sessionId}","!service_tier":"flex","metadata":{"a":[1,2]}}`, ""},
		{`{"model":"x"}`, `can't set "model"`},
		{`{"!stream":true}`, `can't set "stream"`},
		{`{"messages":[]}`, `can't set "messages"`},
		{`{"input":"x"}`, `can't set "input"`},
		{`{"contents":[]}`, `can't set "contents"`},
		{`{"!":1}`, "needs a name"},
		{`{"user":"a","!user":"b"}`, `names "user" twice`},
	} {
		var e ExtraBody
		if err := json.Unmarshal([]byte(c.in), &e); err != nil {
			t.Fatal(err)
		}
		got := ExtraBodyOK(e)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.in, got, c.want)
		}
	}
	if got := ExtraBodyOK(ExtraBody{"a": json.RawMessage(`{`)}); !strings.Contains(got, "not JSON") {
		t.Errorf("bad value: %q", got)
	}
}

// A providers file written by hand with something other than an object, or
// with fields extraBody can't set, still reads: the rest of it is kept.
func TestExtraBodyReadByHand(t *testing.T) {
	var ps []Provider
	if err := json.Unmarshal([]byte(`[{"id":"a","extraBody":"oops"},{"id":"b","extraBody":{"model":"x","messages":[],"user":"u"," !tier ":"t"}}]`), &ps); err != nil {
		t.Fatal(err)
	}
	if ps[0].ExtraBody != nil {
		t.Errorf("a string read as %v", ps[0].ExtraBody)
	}
	got := cleanExtraBody(ps[1].ExtraBody)
	if len(got) != 2 || string(got["user"]) != `"u"` || string(got["!tier"]) != `"t"` {
		t.Errorf("cleaned to %v", got)
	}
	if cleanExtraBody(ExtraBody{"model": json.RawMessage(`1`)}) != nil {
		t.Error("nothing left isn't nil")
	}
}

func TestWithExtraBody(t *testing.T) {
	p := Provider{ExtraBody: ExtraBody{
		"prompt_cache_key": json.RawMessage(`"${sessionId}"`),
		"metadata":         json.RawMessage(`{"tags":["s-${sessionId}"],"n":1}`),
		"!service_tier":    json.RawMessage(`"flex"`),
		"model":            json.RawMessage(`"never"`),
		"big":              json.RawMessage(`12345678901234567890`),
	}}
	out := p.WithExtraBody([]byte(`{"model":"m","service_tier":"auto","input":"a<b>&c","prompt_cache_key":"mine"}`), "S1")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if m["model"] != "m" || m["prompt_cache_key"] != "mine" || m["service_tier"] != "flex" || m["input"] != "a<b>&c" {
		t.Errorf("merged %s", out)
	}
	if !strings.Contains(string(out), `"tags":["s-S1"]`) || !strings.Contains(string(out), `"big":12345678901234567890`) {
		t.Errorf("values %s", out)
	}
	if !strings.Contains(string(out), `"a<b>&c"`) {
		t.Errorf("escaped %s", out)
	}

	out = p.WithExtraBody([]byte(`{"model":"m"}`), "S2")
	if !strings.Contains(string(out), `"prompt_cache_key":"S2"`) {
		t.Errorf("filled %s", out)
	}

	// a body that isn't an object goes as it came
	for _, b := range []string{`[1]`, `not json`, ``} {
		if got := string(p.WithExtraBody([]byte(b), "S")); got != b {
			t.Errorf("%q became %q", b, got)
		}
	}
	// a signed-in account's body is the agent's own
	acct := p
	acct.Account = &Account{}
	if got := string(acct.WithExtraBody([]byte(`{"model":"m"}`), "S")); got != `{"model":"m"}` {
		t.Errorf("account body %s", got)
	}
	// none set: the body's bytes as they were
	if got := string((Provider{}).WithExtraBody([]byte(`{"b":1, "a":2}`), "S")); got != `{"b":1, "a":2}` {
		t.Errorf("unchanged body %s", got)
	}
}
