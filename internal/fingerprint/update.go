package fingerprint

import (
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

var worker struct {
	sync.Mutex
	cancel context.CancelFunc
	force  bool
}
var wake = make(chan struct{}, 1)

// Changed wakes the gateway's scheduler and cancels only its capture/download.
// Another process serving the gateway notices the setting on its minute tick.
func Changed() {
	worker.Lock()
	if worker.cancel != nil {
		worker.cancel()
	}
	worker.force = settings.Load().FingerprintUpdates
	worker.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Keep runs in the process owning the gateway. Last attempts survive restarts,
// and a disabled setting neither downloads nor starts a client.
func Keep(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var done chan struct{}
	wasEnabled := settings.Load().FingerprintUpdates
	var retryAfter time.Time
	pending := false
	for {
		enabled := settings.Load().FingerprintUpdates
		worker.Lock()
		pending = pending || worker.force || (enabled && !wasEnabled)
		worker.force = false
		worker.Unlock()
		wasEnabled = enabled
		if !enabled {
			pending = false
			worker.Lock()
			if worker.cancel != nil {
				worker.cancel()
			}
			worker.Unlock()
		} else if done == nil && (pending || (time.Now().After(retryAfter) && due(readState().Checked, time.Now()))) {
			pending = false
			retryAfter = time.Now().Add(time.Minute)
			done = make(chan struct{})
			go func(ch chan struct{}) {
				defer close(ch)
				if err := update(ctx); err != nil && ctx.Err() == nil {
					log.Printf("fingerprint update: %v", err)
				}
			}(done)
		}
		select {
		case <-ctx.Done():
			Changed()
			if done != nil {
				<-done
			}
			return
		case <-done:
			done = nil
		case <-ticker.C:
		case <-wake:
		}
	}
}

func due(last, now time.Time) bool { return last.IsZero() || !now.Before(last.Add(24*time.Hour)) }

var collectProfile = downloadAndCapture

func update(parent context.Context) error {
	worker.Lock()
	if worker.cancel != nil || !settings.Load().FingerprintUpdates {
		worker.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	worker.cancel = cancel
	worker.Unlock()
	defer func() { cancel(); worker.Lock(); worker.cancel = nil; worker.Unlock() }()
	s := readState()
	previous := s
	s.Checked = time.Now().UTC()
	s.Started = s.Checked
	s.Error = ""
	if err := writeState(s); err != nil {
		return err
	}
	profiles := maps.Clone(s.Profiles)
	if profiles == nil {
		profiles = map[string]Profile{}
	}
	dir, err := os.MkdirTemp("", "magpie-fingerprint-")
	if err == nil {
		defer os.RemoveAll(dir)
		for _, client := range []string{Codex, Claude} {
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			var p Profile
			p, err = collectProfile(ctx, client, dir, profiles[client])
			if err == nil {
				err = validate(client, p)
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", client, err)
				break
			}
			profiles[client] = p
		}
	}
	worker.Lock()
	defer worker.Unlock()
	// A disable/stop never installs the profile an interrupted run produced.
	if parent.Err() != nil || ctx.Err() == context.Canceled || !settings.Load().FingerprintUpdates {
		previous.Started = time.Time{}
		return writeState(previous)
	}
	s.Started = time.Time{}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		s.Error = err.Error()
	} else {
		s.Profiles = profiles
		s.Updated = time.Now().UTC()
	}
	if e := writeState(s); e != nil {
		return e
	}
	return err
}
