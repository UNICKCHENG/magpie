package fingerprint

import (
	"archive/tar"
	"compress/gzip"
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

	"github.com/yetone/magpie/internal/claudecode"
)

const maxDownload = 512 << 20

var codexReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"

func downloadAndCapture(ctx context.Context, client, dir string, old Profile) (Profile, error) {
	var version, url, checksum, member string
	var size int64
	if client == Claude {
		r, err := claudecode.Latest(ctx)
		if err != nil {
			return Profile{}, err
		}
		version, url, checksum, size = r.Version, r.URL, r.Checksum, r.Size
	} else {
		var err error
		member, err = codexAsset(runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return Profile{}, err
		}
		b, err := get(ctx, codexReleaseURL, 8<<20)
		if err != nil {
			return Profile{}, err
		}
		var release struct {
			Tag    string `json:"tag_name"`
			Assets []struct {
				Name   string
				Digest string
				Size   int64
			} `json:"assets"`
		}
		if err = json.Unmarshal(b, &release); err != nil {
			return Profile{}, errors.New("invalid Codex release metadata")
		}
		version = strings.TrimPrefix(release.Tag, "rust-v")
		for _, a := range release.Assets {
			if a.Name == member+".tar.gz" {
				checksum, size = strings.TrimPrefix(a.Digest, "sha256:"), a.Size
				break
			}
		}
		url = "https://github.com/openai/codex/releases/download/rust-v" + version + "/" + member + ".tar.gz"
	}
	if !versionRE.MatchString(version) {
		return Profile{}, errors.New("release has no stable client version")
	}
	// Only a previously captured, validated version can skip the large download.
	if old.Version == version && validate(client, old) == nil && old.Binary != nil {
		if path, err := binaryPath(client, *old.Binary); err == nil && checkBinary(path, *old.Binary) == nil {
			return old, nil
		}
	}
	path := filepath.Join(dir, client)
	if err := download(ctx, url, checksum, size, path); err != nil {
		return Profile{}, err
	}
	if member != "" {
		exe := path + "-exe"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		if err := extract(path, member, exe); err != nil {
			return Profile{}, err
		}
		path = exe
	} else if runtime.GOOS == "windows" {
		if err := os.Rename(path, path+".exe"); err != nil {
			return Profile{}, err
		}
		path += ".exe"
	}
	p, err := capture(ctx, client, version, path)
	if err != nil {
		return Profile{}, err
	}
	p.Binary, err = storeBinary(client, version, path)
	if err == nil {
		p.Previous = old.Binary
	}
	return p, err
}

func codexAsset(goos, arch string) (string, error) {
	a := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[arch]
	p := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-musl", "windows": "pc-windows-msvc.exe"}[goos]
	if a == "" || p == "" {
		return "", fmt.Errorf("Codex has no supported build for %s/%s", goos, arch)
	}
	return "codex-" + a + "-" + p, nil
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	res, err := request(ctx, url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("release metadata is too large")
	}
	return b, err
}

func request(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "magpie-fingerprint-updater")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("download: HTTP %d", res.StatusCode)
	}
	return res, nil
}

// Nothing is executed until both size and SHA-256 match official metadata.
func download(ctx context.Context, url, checksum string, size int64, path string) error {
	want, err := hex.DecodeString(checksum)
	if err != nil || len(want) != sha256.Size || size <= 0 || size > maxDownload {
		return errors.New("release has no valid checksum or size")
	}
	res, err := request(ctx, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, size+1))
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil && (n != size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(checksum)) {
		err = errors.New("download does not match the release checksum and size")
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func extract(archive, member, to string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxDownload+1))
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("Codex executable missing from archive: %w", err)
		}
		if h.Name != member && h.Name != "./"+member {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxDownload {
			return errors.New("invalid Codex archive member")
		}
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if e := out.Close(); err == nil {
			err = e
		}
		return err
	}
}
