//go:build !windows

package fingerprint

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func TestCaptureProcessTreeIsReaped(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := proc.Command("/bin/sh", "-c", "sleep 600 >/dev/null 2>&1 & echo $!; "+map[bool]string{true: "wait", false: "exit 0"}[cancelled])
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- runClient(ctx, cmd) }()
		line, _ := bufio.NewReader(out).ReadString('\n')
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if cancelled {
			cancel()
		}
		err = <-done
		cancel()
		if !cancelled && err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("capture left a child process running")
		}
	}
}

func TestRepeatedCaptureClosesItsListener(t *testing.T) {
	// A fake CLI records just the synthetic base URL, then exits. No model
	// request means validation fails; that path must still close the server.
	dir := t.TempDir()
	exe := filepath.Join(dir, "client")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s' \"$ANTHROPIC_BASE_URL\" > \"$HOME/base\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		work := t.TempDir()
		if _, err := capture(context.Background(), Claude, "2.1.296", exe, work); err == nil {
			t.Fatal("empty capture accepted")
		}
		files, err := filepath.Glob(filepath.Join(work, "home-*", "base"))
		if err != nil || len(files) != 1 {
			t.Fatal("fake CLI did not finish", err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		addr := strings.Split(strings.TrimPrefix(string(b), "http://"), "/")[0]
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatal("capture listener survived return")
		}
	}
}

func TestRepeatedUpdateRemovesTemporaryFiles(t *testing.T) {
	isolate(t)
	enable(t, true)
	var dirs []string
	collectProfile = func(_ context.Context, client, dir string, _ Profile) (Profile, error) {
		dirs = append(dirs, dir)
		f, err := os.Create(filepath.Join(dir, client))
		if err != nil {
			return Profile{}, err
		}
		_, err = io.WriteString(f, "synthetic temporary download")
		_ = f.Close()
		return sample(client, "1.2.3"), err
	}
	for range 20 {
		if err := update(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("temporary directory remains: %s", dir)
		}
	}
	if UpdateStatus().Running {
		t.Fatal("completed task still active")
	}
}
