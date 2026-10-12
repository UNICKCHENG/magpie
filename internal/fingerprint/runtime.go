package fingerprint

// Native executables have their own immutable store. Neither subscription
// discovery nor the user's CLI installation selects from or writes this store.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/nativeclient"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/steady"
)

// Binary identifies the bytes validated for one CLI version. Paths are derived
// locally; an on-disk manifest cannot redirect execution to another program.
type Binary struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

var runtimes struct {
	sync.Mutex
	used     map[string]int
	verified map[string]os.FileInfo
}

// Do selects and pins the private build. A resident process keeps its pin
// until it exits, including across updates and after its first response closes.
func Do(ctx context.Context, opts nativeclient.Options, body []byte) (*http.Response, error) {
	path, _, release, err := Acquire(ctx, opts.Client)
	if err != nil {
		return nil, err
	}
	opts.Executable, opts.Release = path, release
	return nativeclient.Do(ctx, opts, body)
}

// Warm loads a selected private build without a model request. The caller owns
// warmed so restarting the gateway warms again even when the version is equal.
func Warm(ctx context.Context, client string, warmed map[string]bool) error {
	path, _, release, err := Acquire(ctx, client)
	if err != nil {
		return err
	}
	defer release()
	if warmed[path] {
		return nil
	}
	if err = nativeclient.Warm(ctx, client, path); err != nil {
		return err
	}
	warmed[path] = true
	return nil
}

func runtimeRoot() string {
	return filepath.Join(filepath.Dir(statePath()), "native", runtime.GOOS+"-"+runtime.GOARCH)
}

func binaryPath(client string, b Binary) (string, error) {
	digest, err := hex.DecodeString(b.SHA256)
	if (client != Codex && client != Claude) || !versionRE.MatchString(b.Version) || err != nil || len(digest) != sha256.Size || b.Size <= 0 || b.Size > maxDownload {
		return "", errors.New("invalid native CLI manifest")
	}
	name := "cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(runtimeRoot(), client, b.Version+"-"+b.SHA256, name), nil
}

func storeBinary(client, version, source string) (*Binary, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxDownload+1))
	if err != nil {
		return nil, err
	}
	b := &Binary{Version: version, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	path, err := binaryPath(client, *b)
	if err != nil {
		return nil, err
	}
	if err = checkBinary(path, *b); err == nil {
		return b, nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".install-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err = f.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(tmp, f)
	}
	if err == nil {
		err = tmp.Chmod(0700)
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = checkBinary(tmp.Name(), *b)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return b, err
}

func checkBinary(path string, b Binary) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != b.Size {
		return errors.New("native CLI size changed")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		return errors.New("native CLI checksum changed")
	}
	return nil
}

// Acquire pins a private copy until release. A background update affects only
// later acquisitions. With no downloaded version, an installed native binary
// can seed the store, including when automatic updates are disabled.
func Acquire(ctx context.Context, client string) (path, version string, release func(), err error) {
	runtimes.Lock()
	defer runtimes.Unlock()
	p, _ := Current(client)
	b := selectedBinary(client, p)
	if b == nil {
		b, err = installedBinary(ctx, client)
		if err != nil {
			return "", "", nil, err
		}
	}
	path, err = binaryPath(client, *b)
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(path)
		if err == nil {
			old := runtimes.verified[path]
			if old == nil || !os.SameFile(old, info) || old.Size() != info.Size() || !old.ModTime().Equal(info.ModTime()) {
				err = checkBinary(path, *b)
				if err == nil {
					if runtimes.verified == nil {
						runtimes.verified = map[string]os.FileInfo{}
					}
					runtimes.verified[path] = info
				}
			}
		}
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("native %s: %w", client, err)
	}
	if runtimes.used == nil {
		runtimes.used = map[string]int{}
	}
	runtimes.used[path]++
	var once sync.Once
	return path, b.Version, func() {
		once.Do(func() {
			runtimes.Lock()
			defer runtimes.Unlock()
			runtimes.used[path]--
			if runtimes.used[path] == 0 {
				delete(runtimes.used, path)
			}
		})
	}, nil
}

func installedBinary(ctx context.Context, client string) (*Binary, error) {
	if client != Codex && client != Claude {
		return nil, errors.New("unknown native CLI")
	}
	manifest := filepath.Join(runtimeRoot(), client, "installed.json")
	name := "codex"
	if client == Claude {
		name = "claude"
	}
	source := proc.FindTool(name)
	if source != "" {
		if real, err := filepath.EvalSymlinks(source); err == nil {
			source = real
		}
	}
	// npm's Codex entry point is a JavaScript launcher. Copy the native
	// platform package it launches, so later npm updates cannot change a run.
	if client == Codex {
		source = codexExecutable(source, runtime.GOOS, runtime.GOARCH)
	}
	if source == "" {
		return nil, fmt.Errorf("native %s is unavailable; install its CLI or enable fingerprint updates", client)
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	var magic [4]byte
	_, err = io.ReadFull(f, magic[:])
	f.Close()
	ext := strings.ToLower(filepath.Ext(source))
	if err != nil || bytes.HasPrefix(magic[:], []byte("#!")) || ext == ".cmd" || ext == ".js" || ext == ".ps1" {
		return nil, fmt.Errorf("%s is a launcher, not a standalone native CLI; enable fingerprint updates to install a private build", name)
	}
	home, err := os.MkdirTemp("", "magpie-native-version-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := proc.Command(source, "--version")
	cmd.Dir, cmd.Env = home, nativeclient.Environment(home, "direct")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err = runClient(ctx, cmd); err != nil {
		return nil, fmt.Errorf("read native %s version: %w", client, err)
	}
	version := ""
	for _, word := range strings.Fields(out.String()) {
		if versionRE.MatchString(word) {
			version = word
			break
		}
	}
	b, err := storeBinary(client, version, source)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(b)
	if err = steady.WriteFile(manifest, data, 0600); err != nil {
		return nil, err
	}
	return b, nil
}

// selectedBinary is shared by execution and status. Reading status never
// starts or imports a CLI. A selected but damaged build is not hidden by a
// different version: Acquire reports the integrity failure.
func selectedBinary(client string, p Profile) *Binary {
	if client != Codex && client != Claude {
		return nil
	}
	if p.Binary != nil {
		return p.Binary
	}
	data, err := os.ReadFile(filepath.Join(runtimeRoot(), client, "installed.json"))
	if err != nil {
		return nil
	}
	var b Binary
	if json.Unmarshal(data, &b) != nil {
		return nil
	}
	if _, err := binaryPath(client, b); err != nil {
		return nil
	}
	return &b
}

// npm launchers are never copied on their own: the private runtime needs the
// matching platform executable, not a script tied to npm's mutable folders.
func codexExecutable(source, goos, arch string) string {
	ext := strings.ToLower(filepath.Ext(source))
	if ext != ".js" && ext != ".cmd" && ext != ".ps1" {
		return source
	}
	root := filepath.Dir(filepath.Dir(source))
	if ext != ".js" {
		root = filepath.Join(filepath.Dir(source), "node_modules", "@openai", "codex")
	}
	cpu := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[arch]
	osName := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-musl", "windows": "pc-windows-msvc"}[goos]
	if cpu == "" || osName == "" {
		return source
	}
	triple := cpu + "-" + osName
	name := "codex"
	if goos == "windows" {
		name += ".exe"
	}
	for _, layout := range []string{"bin", "codex"} {
		for _, vendor := range []string{filepath.Join(root, "node_modules", "@openai", "codex-*", "vendor"), filepath.Join(root, "vendor")} {
			matches, _ := filepath.Glob(filepath.Join(vendor, triple, layout, name))
			if len(matches) == 1 {
				return matches[0]
			}
		}
	}
	return source
}

// PruneNative retains the selected, previous and locally imported builds, and
// every executable held by a request. Only this module's version folders are
// considered. Call after committing a complete update, never while staging it.
func PruneNative() {
	runtimes.Lock()
	defer runtimes.Unlock()
	s := readState()
	if s.Schema == 0 {
		return
	}
	keep := map[string]bool{}
	for path := range runtimes.used {
		keep[filepath.Dir(path)] = true
	}
	for _, client := range []string{Codex, Claude} {
		p := s.Profiles[client]
		for _, b := range []*Binary{p.Binary, p.Previous} {
			if b != nil {
				if path, err := binaryPath(client, *b); err == nil {
					keep[filepath.Dir(path)] = true
				}
			}
		}
		root := filepath.Join(runtimeRoot(), client)
		if data, err := os.ReadFile(filepath.Join(root, "installed.json")); err == nil {
			var b Binary
			if json.Unmarshal(data, &b) == nil {
				if path, err := binaryPath(client, b); err == nil {
					keep[filepath.Dir(path)] = true
				}
			}
		}
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			version, digest, ok := strings.Cut(entry.Name(), "-")
			if !ok {
				continue
			}
			path, err := binaryPath(client, Binary{Version: version, SHA256: digest, Size: 1})
			if err != nil || keep[filepath.Dir(path)] {
				continue
			}
			if os.RemoveAll(filepath.Dir(path)) == nil {
				delete(runtimes.verified, path)
			}
		}
	}
}
