package fingerprint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeVersionIsPrivateAndPinned(t *testing.T) {
	isolate(t)
	source := filepath.Join(t.TempDir(), "installed-cli")
	if err := os.WriteFile(source, []byte("first native version"), 0700); err != nil {
		t.Fatal(err)
	}
	first, err := storeBinary(Codex, "1.2.3", source)
	if err != nil {
		t.Fatal(err)
	}
	p := sample(Codex, "1.2.3")
	p.Binary = first
	if err := writeState(state{Profiles: map[string]Profile{Codex: p}}); err != nil {
		t.Fatal(err)
	}
	path, version, release, err := Acquire(context.Background(), Codex)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if path == source || version != "1.2.3" {
		t.Fatalf("run did not pin a private binary: %s %s", path, version)
	}
	if err := os.WriteFile(source, []byte("second native version"), 0700); err != nil {
		t.Fatal(err)
	}
	second, err := storeBinary(Codex, "1.2.4", source)
	if err != nil {
		t.Fatal(err)
	}
	p = sample(Codex, "1.2.4")
	p.Binary, p.Previous = second, first
	if err := writeState(state{Profiles: map[string]Profile{Codex: p}}); err != nil {
		t.Fatal(err)
	}
	enable(t, false)
	next, nextVersion, releaseNext, err := Acquire(context.Background(), Codex)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNext()
	if next == path || nextVersion != "1.2.4" {
		t.Fatal("next request did not select the updated executable")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "first native version" {
		t.Fatalf("update changed an active executable: %q %v", b, err)
	}
	release()
	release() // cleanup and cancellation may both release one run
}

func TestNativeRuntimeRejectsChangedBinaryAndEscapingManifest(t *testing.T) {
	isolate(t)
	source := filepath.Join(t.TempDir(), "native")
	if err := os.WriteFile(source, []byte("native cli"), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := storeBinary(Claude, "2.3.4", source)
	if err != nil {
		t.Fatal(err)
	}
	p := sample(Claude, "2.3.4")
	p.Binary = b
	if err := writeState(state{Profiles: map[string]Profile{Claude: p}}); err != nil {
		t.Fatal(err)
	}
	path, _ := binaryPath(Claude, *b)
	if err := os.WriteFile(path, []byte("tamper cli"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Acquire(context.Background(), Claude); err == nil {
		t.Fatal("a modified native CLI was selected")
	}
	for _, client := range []string{"../claude", "", "unknown"} {
		if _, err := binaryPath(client, *b); err == nil {
			t.Fatalf("manifest escaped its native client directory: %q", client)
		}
	}
	b.Version = "../../outside"
	if _, err := binaryPath(Claude, *b); err == nil {
		t.Fatal("version escaped the native version directory")
	}
}

func TestNativeStatusMatchesImportedRuntime(t *testing.T) {
	isolate(t)
	enable(t, false)
	source := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(source, []byte("review native runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := storeBinary(Codex, "0.162.1", source)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(runtimeRoot(), Codex, "installed.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, version, release, err := Acquire(context.Background(), Codex)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := UpdateStatus().Versions[Codex]; got != version {
		t.Fatalf("selected runtime=%s; displayed version=%s", version, got)
	}
}

func TestNativeCodexFindsPlatformBinaryBehindNPMLauncher(t *testing.T) {
	for _, tc := range []struct{ os, arch, launcher, triple, binary string }{
		{"darwin", "arm64", "bin/codex.js", "aarch64-apple-darwin", "codex"},
		{"linux", "amd64", "bin/codex.js", "x86_64-unknown-linux-musl", "codex"},
		{"windows", "amd64", "codex.cmd", "x86_64-pc-windows-msvc", "codex.exe"},
	} {
		t.Run(tc.os, func(t *testing.T) {
			root := t.TempDir()
			launcher := filepath.Join(root, filepath.FromSlash(tc.launcher))
			pkg := root
			if tc.os == "windows" {
				pkg = filepath.Join(root, "node_modules", "@openai", "codex")
			}
			binary := filepath.Join(pkg, "node_modules", "@openai", "codex-platform", "vendor", tc.triple, "bin", tc.binary)
			if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binary, []byte("platform executable"), 0700); err != nil {
				t.Fatal(err)
			}
			if got := codexExecutable(launcher, tc.os, tc.arch); got != binary {
				t.Fatalf("selected %q; want platform executable %q", got, binary)
			}
		})
	}
}

func TestNativePruneKeepsInFlightPreviousAndImportedBuilds(t *testing.T) {
	isolate(t)
	source := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(source, []byte("native build"), 0700); err != nil {
		t.Fatal(err)
	}
	builds := make([]*Binary, 4)
	for i, version := range []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3"} {
		var err error
		builds[i], err = storeBinary(Codex, version, source)
		if err != nil {
			t.Fatal(err)
		}
	}
	p := sample(Codex, "1.0.0")
	p.Binary = builds[0]
	if err := writeState(state{Profiles: map[string]Profile{Codex: p}}); err != nil {
		t.Fatal(err)
	}
	active, _, release, err := Acquire(context.Background(), Codex)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p = sample(Codex, "1.0.3")
	p.Binary, p.Previous = builds[3], builds[2]
	if err := writeState(state{Profiles: map[string]Profile{Codex: p}}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(builds[1])
	if err := os.WriteFile(filepath.Join(runtimeRoot(), Codex, "installed.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	PruneNative()
	for _, b := range builds {
		path, _ := binaryPath(Codex, *b)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live, previous or imported build removed: %v", err)
		}
	}
	release()
	PruneNative()
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatalf("unused old build remains: %v", err)
	}
}
