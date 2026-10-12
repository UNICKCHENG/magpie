package fingerprint

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/nativeclient"
	"github.com/yetone/magpie/internal/proc"
)

var captureNativeDir = os.Getenv("MAGPIE_TEST_NATIVE_BIN_DIR")

func TestNativeCaptureInstalled(t *testing.T) {
	if captureNativeDir == "" {
		t.Skip("requires native executables")
	}
	for _, client := range []string{Codex, Claude} {
		t.Run(client, func(t *testing.T) {
			name := "codex"
			if client == Claude {
				name = "claude"
			}
			exe := filepath.Join(captureNativeDir, name)
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := proc.Command(exe, "--version")
			cmd.Dir, cmd.Env, cmd.Stderr = home, nativeclient.Environment(home, "direct"), io.Discard
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := runClient(ctx, cmd); err != nil {
				t.Fatal(err)
			}
			version := ""
			for _, word := range strings.Fields(out.String()) {
				if versionRE.MatchString(word) {
					version = word
					break
				}
			}
			p, err := capture(ctx, client, version, exe)
			if err != nil {
				t.Fatal(err)
			}
			if p.Version != version {
				t.Fatal("wrong captured build")
			}
		})
	}
}
