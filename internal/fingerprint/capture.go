package fingerprint

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/nativeclient"
	"github.com/yetone/magpie/internal/proc"
)

func capture(parent context.Context, client, version, exe string) (Profile, error) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Profile{}, err
	}
	token := make([]byte, 24)
	if _, err = rand.Read(token); err != nil {
		l.Close()
		return Profile{}, err
	}
	prefix := "/" + hex.EncodeToString(token)
	profiles := make(chan Profile, 1)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := prefix + "/v1/responses"
		if client == Claude {
			endpoint = prefix + "/v1/messages"
		}
		if r.Method != http.MethodPost || r.URL.Path != endpoint {
			http.NotFound(w, r)
			return
		}
		body, err := captureBody(w, r)
		if err != nil {
			http.Error(w, "invalid capture request", 400)
			return
		}
		p, err := observed(client, version, r.Header, body)
		if err != nil {
			http.Error(w, "client identity missing", 400)
			return
		}
		select {
		case profiles <- p:
		default:
		}
		captureReply(w, client, body)
	})
	closed := make(chan struct{})
	go func() { defer close(closed); _ = srv.Serve(l) }()
	defer func() { _ = srv.Close(); <-closed }()
	base := "http://" + l.Addr().String() + prefix
	body := `{"model":"gpt-5.6","input":"Reply only OK.","stream":false}`
	endpoint := base + "/v1"
	if client == Claude {
		body = `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"Reply only OK."}],"max_tokens":32,"stream":false}`
		endpoint = base
	}
	res, err := nativeclient.Do(ctx, nativeclient.Options{Client: client, Executable: exe, Base: endpoint, Key: "synthetic-fingerprint-capture", Proxy: "direct"}, []byte(body))
	if err != nil {
		return Profile{}, fmt.Errorf("native protocol validation: %w", err)
	}
	result, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(result), `"OK"`) {
		return Profile{}, errors.New("native protocol validation did not complete a model response")
	}
	select {
	case p := <-profiles:
		return p, nil
	default:
		return Profile{}, errors.New("CLI sent no verifiable model request")
	}
}

// Wait reaps the CLI and sweeps its children even after a successful exit.
// A cancellation kills the whole tree, including Windows job members.
func runClient(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	tree, err := proc.StartTree(cmd)
	if err != nil {
		return err
	}
	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { tree.Kill(); close(killed) })
	err = tree.Wait()
	if !stop() {
		<-killed
	}
	return err
}

func captureBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, error) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, 4<<20)
	switch r.Header.Get("Content-Encoding") {
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	case "zstd":
		z, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(8<<20))
		if err != nil {
			return nil, err
		}
		defer z.Close()
		reader = z
	case "", "identity":
	default:
		return nil, errors.New("unknown content encoding")
	}
	b, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, errors.New("capture body too large")
	}
	var body map[string]json.RawMessage
	err = json.Unmarshal(b, &body)
	return body, err
}

func observed(client, version string, h http.Header, body map[string]json.RawMessage) (Profile, error) {
	p := Profile{Version: version, Headers: map[string]string{}}
	for _, k := range allowedHeaders(client) {
		if v := h.Get(k); v != "" {
			p.Headers[k] = v
		}
	}
	if client == Claude {
		var blocks []struct{ Type, Text string }
		_ = json.Unmarshal(body["system"], &blocks)
		for _, b := range blocks {
			if b.Type == "text" && hasIdentity(b.Text) {
				p.Identity = b.Text
				break
			}
		}
	}
	return p, validate(client, p)
}

func captureReply(w http.ResponseWriter, client string, body map[string]json.RawMessage) {
	var model string
	_ = json.Unmarshal(body["model"], &model)
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, fields map[string]any) {
		fields["type"] = kind
		b, _ := json.Marshal(fields)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
	}
	if client == Claude {
		message := map[string]any{"id": "msg_capture", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
		if string(body["stream"]) != "true" {
			w.Header().Set("Content-Type", "application/json")
			message["content"] = []any{map[string]string{"type": "text", "text": "OK"}}
			message["stop_reason"] = "end_turn"
			_ = json.NewEncoder(w).Encode(message)
			return
		}
		event("message_start", map[string]any{"message": message})
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "text_delta", "text": "OK"}})
		event("content_block_stop", map[string]any{"index": 0})
		event("message_delta", map[string]any{"delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 1}})
		event("message_stop", map[string]any{})
		return
	}
	item := map[string]any{"id": "msg_capture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}
	response := map[string]any{"id": "resp_capture", "object": "response", "created_at": 1, "status": "in_progress", "model": model, "output": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
	event("response.created", map[string]any{"response": response})
	event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_capture", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	event("response.output_text.delta", map[string]any{"item_id": "msg_capture", "output_index": 0, "content_index": 0, "delta": "OK"})
	event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	response["status"], response["output"] = "completed", []any{item}
	event("response.completed", map[string]any{"response": response})
}
