package nativeclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	maxIdle      = 2
	maxUses      = 8
	idleLifetime = 30 * time.Second
)

type idleProcess struct {
	p     *process
	since time.Time
}

// Pool retains at most two idle CLIs for 30 seconds and retires each after
// eight requests. It never shares a conversation between scopes or runs two
// requests on one process. The zero value is ready; its owner must call Close.
type Pool struct {
	mu        sync.Mutex
	idle      map[[32]byte]*idleProcess
	all       map[*process]bool
	closed    bool
	runs      sync.WaitGroup
	reapers   sync.WaitGroup
	closeOnce sync.Once
	owner     context.Context
	cancel    context.CancelFunc
}

// watch also covers a caller that stops reading a stream without closing it:
// shutting down the owner cancels its pipe writes as well as its CLI process.
func (p *Pool) watch(cancel context.CancelFunc) func() {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		return func() {}
	}
	if p.owner == nil {
		p.owner, p.cancel = context.WithCancel(context.Background())
	}
	owner := p.owner
	p.mu.Unlock()
	stop := context.AfterFunc(owner, cancel)
	return func() { stop() }
}

func (p *Pool) enter() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("native client pool is closed")
	}
	p.runs.Add(1)
	return nil
}
func (p *Pool) leave() { p.runs.Done() }

func poolKey(opts Options, body map[string]json.RawMessage) [32]byte {
	// The resolved environment includes global/system proxy changes. Sorted
	// values make map iteration order irrelevant; secrets never become labels.
	env := Environment("", opts.Proxy)
	sort.Strings(env)
	// Claude's system prompt is fixed at initialization. Bind reuse to its
	// complete system value as well as the explicit caller scope below.
	var system json.RawMessage
	if opts.Client == Claude {
		system = body["system"]
	}
	b, _ := json.Marshal(struct {
		Client, Executable, Base, Key, Scope, Model string
		Headers                                     map[string]string
		Environment                                 []string
		Betas                                       []string
		System                                      json.RawMessage
	}{opts.Client, opts.Executable, opts.Base, opts.Key, opts.Scope, text(body["model"]), opts.Headers, env, opts.Betas, system})
	return sha256.Sum256(b)
}

func (p *Pool) take(key [32]byte) *process {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.idle[key]
	if !ok {
		return nil
	}
	delete(p.idle, key)
	v.p.timer.Stop()
	return v.p
}

func (p *Pool) track(run *process) bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if p.all == nil {
		p.all = map[*process]bool{}
	}
	p.all[run] = true
	return true
}

func (p *Pool) remove(run *process) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.all, run)
	p.mu.Unlock()
}

func (p *Pool) put(key [32]byte, run *process) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	if p.idle == nil {
		p.idle = map[[32]byte]*idleProcess{}
	}
	var old *process
	// A concurrent request in the same scope may already have returned.
	if v, ok := p.idle[key]; ok {
		old = v.p
		delete(p.idle, key)
	} else if len(p.idle) >= maxIdle {
		var oldest [32]byte
		var when time.Time
		for k, v := range p.idle {
			if when.IsZero() || v.since.Before(when) {
				oldest, when = k, v.since
			}
		}
		old = p.idle[oldest].p
		delete(p.idle, oldest)
	}
	if old != nil {
		old.timer.Stop()
		delete(p.all, old)
	}
	entry := &idleProcess{run, time.Now()}
	p.idle[key] = entry
	run.timer = time.AfterFunc(idleLifetime, func() { p.expire(key, entry) })
	p.mu.Unlock()
	if old != nil {
		old.close()
	}
	return true
}

func (p *Pool) expire(key [32]byte, entry *idleProcess) {
	p.mu.Lock()
	if p.idle[key] != entry {
		p.mu.Unlock()
		return
	}
	delete(p.idle, key)
	delete(p.all, entry.p)
	p.reapers.Add(1)
	p.mu.Unlock()
	defer p.reapers.Done()
	entry.p.close()
}

// Close stops active and idle processes, waits for request cleanup and releases
// every executable pin. No timer or request may repopulate a closed pool.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		if p.cancel != nil {
			p.cancel()
		}
		all := make([]*process, 0, len(p.all))
		for run := range p.all {
			if run.timer != nil {
				run.timer.Stop()
			}
			all = append(all, run)
		}
		clear(p.idle)
		clear(p.all)
		p.mu.Unlock()
		for _, run := range all {
			run.close()
		}
		p.runs.Wait()
		p.reapers.Wait()
	})
}

// WithPool attaches a gateway-owned pool to a request without giving model
// probes or update validation their own permanent worker goroutines.
type poolContextKey struct{}

func WithPool(ctx context.Context, pool *Pool) context.Context {
	return context.WithValue(ctx, poolContextKey{}, pool)
}
func FromContext(ctx context.Context) *Pool { p, _ := ctx.Value(poolContextKey{}).(*Pool); return p }
