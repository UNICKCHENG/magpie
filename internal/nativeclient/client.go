// Package nativeclient runs custom-provider inference through private native
// CLIs. It owns no subscription credentials, sign-in state or agent settings.
package nativeclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
)

// Options contains only the custom provider's connection. BeforeSend admits
// the model request after CLI startup; the native clients have retries off.
type Options struct {
	Client     string
	Executable string
	Base       string
	Key        string
	Headers    map[string]string
	Proxy      string
	Betas      []string
	BeforeSend func(context.Context) error
	// Pool reuses only fully reset processes within Scope. Empty Scope disables reuse.
	Pool  *Pool
	Scope string
	// Release owns the private executable pin; Do transfers it to a new process
	// or releases it when an existing process already holds that build.
	Release func()
}

var slots = make(chan struct{}, 4)

const Codex = "codex"
const Claude = "claude-code"

// Do adapts one native model response to its API. It never constructs client
// identity headers and never falls back to an HTTP inference implementation.
// Closing an unfinished Body cancels and reaps the process. Completed runs may
// be kept briefly by Pool after their conversation state has been cleared.
func Do(parent context.Context, opts Options, body []byte) (*http.Response, error) {
	handedOff := false
	defer func() {
		if !handedOff && opts.Release != nil {
			opts.Release()
		}
	}()
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("native request: %w", err)
	}
	if opts.Client != Codex && opts.Client != Claude {
		return nil, errors.New("unknown native client")
	}
	u, err := url.Parse(opts.Base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("native client requires an HTTP or HTTPS provider URL without userinfo")
	}
	if text(request["model"]) == "" {
		return nil, errors.New("native client requires a model")
	}
	if opts.Pool != nil {
		if err := opts.Pool.enter(); err != nil {
			return nil, err
		}
	}
	handedOff = true
	ctx, cancel := context.WithCancel(parent)
	reader, writer := io.Pipe()
	stopOwner := opts.Pool.watch(cancel)
	stopWrites := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
	ready := make(chan *http.Response, 1)
	done := make(chan struct{})
	stream := string(request["stream"]) == "true"
	go func() {
		defer close(done)
		if opts.Pool != nil {
			defer opts.Pool.leave()
		}
		defer stopOwner()
		defer cancel()
		defer stopWrites()
		defer writer.Close()
		opened := false
		open := func(status int) {
			if opened {
				return
			}
			opened = true
			contentType := "application/json"
			if stream && status == http.StatusOK {
				contentType = "text/event-stream"
			}
			ready <- &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: &bodyCloser{ReadCloser: reader, cancel: cancel, done: done}}
		}
		sequence := 0
		emit := func(event map[string]any) error {
			if !stream {
				return nil
			}
			open(http.StatusOK)
			if opts.Client == Codex {
				event["sequence_number"] = sequence
				sequence++
			}
			data, err := json.Marshal(event)
			if err == nil {
				_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], data)
			}
			return err
		}
		result, runErr := run(ctx, opts, request, emit)
		if runErr != nil {
			status := http.StatusBadGateway
			var e *clientError
			if errors.As(runErr, &e) && e.status >= 400 {
				status = e.status
			}
			message := runErr.Error()
			if opts.Key != "" {
				message = strings.ReplaceAll(message, opts.Key, "[redacted]")
			}
			payload := map[string]any{"type": "error", "error": map[string]any{"type": "native_client_error", "message": message}}
			if opened {
				_ = emit(payload)
			} else {
				open(status)
				_ = json.NewEncoder(writer).Encode(payload)
			}
			return
		}
		open(http.StatusOK)
		if !stream {
			_ = json.NewEncoder(writer).Encode(result)
		}
	}()
	select {
	case response := <-ready:
		return response, nil
	case <-parent.Done():
		cancel()
		reader.Close()
		<-done
		return nil, parent.Err()
	}
}

type bodyCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	done   <-chan struct{}
	once   sync.Once
}

func (b *bodyCloser) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.cancel(); <-b.done })
	return err
}

type clientError struct {
	status  int
	message string
}

func (e *clientError) Error() string { return e.message }

type process struct {
	in           io.WriteCloser
	out          io.ReadCloser
	scanner      *bufio.Scanner
	opts         Options
	tree         *proc.Tree
	home         string
	closeOnce    sync.Once
	initialized  bool
	claudeSystem string
	reset        func() error
	uses         int
	timer        *time.Timer
}

func (p *process) close() {
	p.closeOnce.Do(func() {
		p.tree.Kill()
		p.in.Close()
		p.out.Close()
		_ = p.tree.Wait()
		_ = os.RemoveAll(p.home)
		if p.opts.Release != nil {
			p.opts.Release()
		}
	})
}

// watch ends a process if its current owner cancels. The returned function
// waits for a racing cancellation before the process can return to the pool.
func (p *process) watch(ctx context.Context) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { p.tree.Kill(); close(done) })
	return sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})
}

func start(opts Options, body map[string]json.RawMessage) (_ *process, err error) {
	if opts.Executable == "" {
		return nil, errors.New("native client executable is unavailable")
	}
	home, err := os.MkdirTemp("", "magpie-native-request-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(home)
		}
	}()
	for _, name := range []string{".codex", ".claude", "config", "cache", "tmp"} {
		if err = os.Mkdir(filepath.Join(home, name), 0700); err != nil {
			return nil, err
		}
	}
	var system string
	if opts.Client == Claude {
		system, err = claudeSystem(body)
		if err != nil {
			return nil, err
		}
	}
	args, env, err := arguments(opts, body, home)
	if err != nil {
		return nil, err
	}
	cmd := proc.Command(opts.Executable, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = home, env, io.Discard
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			in.Close()
		}
	}()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			out.Close()
		}
	}()
	tree, err := proc.StartTree(cmd)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	return &process{in: in, out: out, scanner: scanner, opts: opts, tree: tree, home: home, claudeSystem: system}, nil
}

func (p *process) initialize() error {
	if p.initialized {
		return nil
	}
	if p.opts.Client == Claude {
		request := map[string]any{"subtype": "initialize"}
		if p.claudeSystem != "" {
			request["systemPrompt"] = []string{p.claudeSystem}
		}
		if _, err := p.claudeControl("initialize", request); err != nil {
			return err
		}
	} else {
		if _, err := p.rpc(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "magpie_native", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
			return err
		}
		if err := p.send(map[string]any{"method": "initialized"}); err != nil {
			return err
		}
	}
	p.initialized = true
	return nil
}

func run(ctx context.Context, opts Options, body map[string]json.RawMessage, emit func(map[string]any) error) (map[string]any, error) {
	// Always release the request's acquisition unless a new process owns it.
	owned := false
	defer func() {
		if !owned && opts.Release != nil {
			opts.Release()
		}
	}()
	if opts.Client == Codex {
		if err := validateCodex(body); err != nil {
			return nil, err
		}
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pool := opts.Pool
	key := poolKey(opts, body)
	p := pool.take(key)
	if p == nil {
		var err error
		p, err = start(opts, body)
		if err != nil {
			return nil, err
		}
		owned = true
		if !pool.track(p) {
			p.close()
			return nil, errors.New("native client pool is closed")
		}
	}
	keep := false
	defer func() {
		if !keep {
			pool.remove(p)
			p.close()
		}
	}()
	// Do not retain a previous request's admission callback or context.
	p.opts.BeforeSend = opts.BeforeSend
	defer func() {
		if !keep {
			p.opts.BeforeSend = nil
		}
	}()
	stop := p.watch(ctx)
	defer stop()
	if err := p.initialize(); err != nil {
		return nil, err
	}
	var result map[string]any
	var err error
	p.reset = nil
	if opts.Client == Claude {
		result, err = p.claude(ctx, body, emit)
	} else {
		result, err = p.codex(ctx, body, emit)
	}
	p.uses++
	if err == nil && pool != nil && opts.Scope != "" && p.uses < maxUses && p.reset != nil && ctx.Err() == nil {
		cleanup, cancel := context.WithTimeout(ctx, 2*time.Second)
		stopCleanup := p.watch(cleanup)
		cleanErr := p.reset()
		stopCleanup()
		clean := cleanErr == nil && cleanup.Err() == nil
		cancel()
		// The request must no longer be able to kill a subsequent borrower's run.
		stop()
		p.reset = nil
		p.opts.BeforeSend = nil
		if clean && ctx.Err() == nil {
			keep = pool.put(key, p)
		}
	}
	return result, err
}

// Warm initializes then exits without submitting a model turn. No provider
// credentials or persistent user state are needed to load the native runtime.
func Warm(ctx context.Context, client, executable string) error {
	model := "gpt-5.6"
	if client == Claude {
		model = "claude-haiku-5-5"
	}
	p, err := start(Options{Client: client, Executable: executable, Base: "http://127.0.0.1:1/v1", Key: uuid.NewString(), Proxy: "direct"}, map[string]json.RawMessage{"model": raw(model)})
	if err != nil {
		return err
	}
	defer p.close()
	stop := p.watch(ctx)
	defer stop()
	return p.initialize()
}

// Environment isolates a native CLI from user and subscription settings.
// Callers add only their own credentials to the returned environment.
func Environment(home, proxy string) []string {
	var env []string
	for _, name := range []string{"PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "SystemDrive", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	for name, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"APPDATA": filepath.Join(home, "config"), "LOCALAPPDATA": filepath.Join(home, "cache"), "TMPDIR": filepath.Join(home, "tmp"), "TMP": filepath.Join(home, "tmp"), "TEMP": filepath.Join(home, "tmp"),
		"CODEX_HOME": filepath.Join(home, ".codex"), "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "LANG": "en_US.UTF-8",
		"DISABLE_TELEMETRY": "1", "DISABLE_ERROR_REPORTING": "1", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "OTEL_SDK_DISABLED": "true",
	} {
		env = append(env, name+"="+value)
	}
	return netproxy.EnvWith(proxy, env)
}

func arguments(opts Options, body map[string]json.RawMessage, home string) ([]string, []string, error) {
	env := Environment(home, opts.Proxy)
	if opts.Client == Claude {
		env = append(env, "ANTHROPIC_API_KEY="+opts.Key, "ANTHROPIC_BASE_URL="+strings.TrimSuffix(opts.Base, "/v1"), "CLAUDE_CODE_MAX_RETRIES=0", "CLAUDE_CODE_NONSTREAMING_TIMEOUT_RETRIES=0")
		var headers []string
		for name, value := range opts.Headers {
			if strings.ContainsAny(name+value, "\r\n") {
				return nil, nil, errors.New("native provider header contains a newline")
			}
			headers = append(headers, name+": "+value)
		}
		if len(headers) > 0 {
			env = append(env, "ANTHROPIC_CUSTOM_HEADERS="+strings.Join(headers, "\n"))
		}
		args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--include-partial-messages", "--verbose", "--model", text(body["model"]), "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--permission-mode", "dontAsk", "--max-turns", "1"}
		if len(opts.Betas) > 0 {
			env = append(env, "ANTHROPIC_BETAS="+strings.Join(opts.Betas, ","))
		}
		return args, netproxy.EnvForBun(env), nil
	}
	return codexArguments(opts, body, env, home)
}

func (p *process) send(value any) error { return json.NewEncoder(p.in).Encode(value) }

func (p *process) next() (map[string]json.RawMessage, error) {
	for p.scanner.Scan() {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(p.scanner.Bytes(), &message); err != nil {
			return nil, errors.New("native CLI returned invalid protocol data")
		}
		return message, nil
	}
	if err := p.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("native CLI ended before completing the model response")
}

func text(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func object(raw json.RawMessage) map[string]any {
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	return value
}

func raw(value any) json.RawMessage {
	b, _ := json.Marshal(value)
	return b
}

func unsupported(fields []string) error {
	return &clientError{http.StatusBadRequest, "native CLI cannot preserve these request fields: " + strings.Join(fields, ", ") + "; disable this provider's client fingerprint to send them through its API"}
}
