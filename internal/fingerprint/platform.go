package fingerprint

import (
	"context"
	"github.com/yetone/magpie/internal/proc"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var codexUA struct {
	sync.Once
	os string
}

// CodexUserAgent is the User-Agent Codex CLI of version v sends:
// "codex_cli_rs/0.155.1 (Mac OS 26.6.0; arm64) Apple_Terminal/455".
func CodexUserAgent(v string) string { return Builtin(Codex, v).Header("User-Agent") }

var osVersionRE = regexp.MustCompile(`\d+(\.\d+)+`)

// codexOS names the system as Codex CLI does: "Mac OS 26.6.0",
// "Windows 10.0.26100", "Linux 6.8.0".
func codexOS() string {
	run := func(name string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, _ := proc.CommandContext(ctx, name, args...).Output()
		return osVersionRE.FindString(string(out))
	}
	three := func(v string) string {
		for strings.Count(v, ".") < 2 {
			v += ".0"
		}
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		if v := run("sw_vers", "-productVersion"); v != "" {
			return "Mac OS " + three(v)
		}
		return "Mac OS"
	case "windows":
		if v := run("cmd", "/c", "ver"); v != "" { // "Microsoft Windows [Version 10.0.26100.4652]"
			if parts := strings.Split(v, "."); len(parts) > 3 {
				v = strings.Join(parts[:3], ".")
			}
			return "Windows " + v
		}
		return "Windows"
	}
	if v := run("uname", "-r"); v != "" {
		return "Linux " + three(v)
	}
	return "Linux"
}
