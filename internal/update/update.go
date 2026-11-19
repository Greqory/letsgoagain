// Package update replaces the running letsgoagain with the newest GitHub release: it finds the latest
// tag, downloads the binary for this machine, checks it against the release's checksums.txt and swaps
// it in atomically. Only when the user runs `letsgoagain update` (or `status`, which asks for the latest
// tag alone), never by itself.
package update

import (
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
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "Greqory/letsgoagain"

// maxBinary bounds a download; the binary is about 11 MB.
const maxBinary = 100 << 20

// Updater talks to GitHub. Its URLs are fields so tests can point it elsewhere.
type Updater struct {
	API      string // https://api.github.com
	Download string // https://github.com
	Arch     string
	HTTP     *http.Client
}

// New returns an updater for GitHub and this machine.
func New() *Updater {
	return &Updater{API: "https://api.github.com", Download: "https://github.com", Arch: runtime.GOARCH,
		HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// AssetName is the release file for an architecture.
func AssetName(arch string) string { return "letsgoagain_linux_" + arch }

// Latest returns the newest release tag.
func (u *Updater) Latest(ctx context.Context) (string, error) {
	body, err := u.get(ctx, u.API+"/repos/"+Repo+"/releases/latest", 1<<20)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil || rel.TagName == "" {
		return "", fmt.Errorf("latest release: no tag in the answer")
	}
	return rel.TagName, nil
}

// Fetch downloads the binary of a release and verifies it against the release's checksums.txt.
func (u *Updater) Fetch(ctx context.Context, tag string) ([]byte, error) {
	base := u.Download + "/" + Repo + "/releases/download/" + tag + "/"
	asset := AssetName(u.Arch)
	sums, err := u.get(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return nil, fmt.Errorf("checksums: %w", err)
	}
	want, err := checksumFor(string(sums), asset)
	if err != nil {
		return nil, err
	}
	bin, err := u.get(ctx, base+asset, maxBinary)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("download %s: checksum mismatch, nothing replaced", asset)
	}
	return bin, nil
}

func checksumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s (is %s supported?)", asset, runtime.GOARCH)
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: answer too large", url)
	}
	return b, nil
}

// Replace swaps the file at path for data: a temp file in the same directory, renamed over it, so a
// running copy keeps working and a failed write leaves the old binary in place.
func Replace(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".letsgoagain-update-*")
	if err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("replace: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("replace: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}

// Newer reports whether latest is a newer vMAJOR.MINOR.PATCH than current. Versions it cannot read
// count as newer when they differ, so a broken version string never blocks an update.
func Newer(current, latest string) bool {
	c, errC := parse(current)
	l, errL := parse(latest)
	if errC != nil || errL != nil {
		return current != latest
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, error) {
	var out [3]int
	core := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 { // pre-release or build metadata, e.g. v0.3.0+dirty
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, errors.New("not vMAJOR.MINOR.PATCH")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, err
		}
		out[i] = n
	}
	return out, nil
}
