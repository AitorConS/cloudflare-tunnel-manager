// Package updater checks GitHub releases for a newer build and, when the
// installation allows it, replaces the running binary with it.
package updater

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBinarySize = 200 << 20

type Updater struct {
	Repo    string // "owner/name" on GitHub
	Current string // version of the running binary, e.g. "v0.1.0" or "dev"
	// Enabled allows replacing the binary in place. It is set by installs
	// where a supervisor restarts the process (the systemd unit); container
	// images are updated by pulling a new image instead.
	Enabled bool
	// Restart is called after a successful update to exit the process so the
	// supervisor starts the new binary.
	Restart func()

	APIBase string // GitHub API root; overridden in tests
	ExePath string // binary to replace; defaults to the running executable
	HTTP    *http.Client

	mu       sync.Mutex
	cached   *release
	cachedAt time.Time
}

type Info struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	URL             string `json:"url"`
	UpdateAvailable bool   `json:"updateAvailable"`
	CanSelfUpdate   bool   `json:"canSelfUpdate"`
	// Why holds the reason an available update cannot be applied from the panel.
	Why string `json:"why"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Assets []asset `json:"assets"`
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tunnel-manager-updater")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

// latest returns the newest published release, cached for a few minutes
// because GitHub rate-limits anonymous API calls.
func (u *Updater) latest(ctx context.Context, fresh bool) (*release, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !fresh && u.cached != nil && time.Since(u.cachedAt) < 10*time.Minute {
		return u.cached, nil
	}
	base := u.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	resp, err := u.get(ctx, base+"/repos/"+u.Repo+"/releases/latest")
	if err != nil {
		return nil, fmt.Errorf("could not check for updates: %w", err)
	}
	defer resp.Body.Close()
	rel := &release{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(rel); err != nil || rel.Tag == "" {
		return nil, errors.New("could not check for updates: unexpected response from GitHub")
	}
	u.cached, u.cachedAt = rel, time.Now()
	return rel, nil
}

// parseVersion reads "v1.2.3" (a "-suffix" is ignored) into its numbers.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func newer(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func assetName() string { return "ctm-" + runtime.GOOS + "-" + runtime.GOARCH }

func (rel *release) find(name string) *asset {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// Check reports the latest release and whether it can be installed from here.
func (u *Updater) Check(ctx context.Context, fresh bool) (*Info, error) {
	rel, err := u.latest(ctx, fresh)
	if err != nil {
		return nil, err
	}
	info := &Info{Current: u.Current, Latest: rel.Tag, URL: rel.URL, UpdateAvailable: newer(rel.Tag, u.Current)}
	switch {
	case !info.UpdateAvailable:
		if _, ok := parseVersion(u.Current); !ok {
			info.Why = "this is a development build, so versions cannot be compared"
		}
	case !u.Enabled:
		info.Why = "this installation is not set up for in-place updates (in Docker, pull or rebuild the image instead)"
	case rel.find(assetName()) == nil || rel.find("checksums.txt") == nil:
		info.Why = "the release has no build for " + runtime.GOOS + "/" + runtime.GOARCH
	default:
		info.CanSelfUpdate = true
	}
	return info, nil
}

func (u *Updater) download(ctx context.Context, url string, w io.Writer, limit int64) error {
	resp, err := u.get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("download is larger than expected")
	}
	return nil
}

// Apply downloads the latest release, verifies it against the release's
// checksum file, checks that it runs, and swaps it in for the current binary.
// The caller is expected to invoke Restart afterwards.
func (u *Updater) Apply(ctx context.Context) (string, error) {
	info, err := u.Check(ctx, true)
	if err != nil {
		return "", err
	}
	if !info.UpdateAvailable {
		return "", errors.New("already up to date")
	}
	if !info.CanSelfUpdate {
		return "", errors.New(info.Why)
	}
	rel, _ := u.latest(ctx, false)
	bin, sums := rel.find(assetName()), rel.find("checksums.txt")

	var sumFile strings.Builder
	if err := u.download(ctx, sums.URL, &sumFile, 1<<20); err != nil {
		return "", fmt.Errorf("could not download checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(sumFile.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == bin.Name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return "", errors.New("the release checksums do not list " + bin.Name)
	}

	exe := u.ExePath
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return "", err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return "", err
		}
	}
	// Same directory as the target so the final rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".ctm-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to the binary (%w); update it with the install script instead", err)
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	err = u.download(ctx, bin.URL, io.MultiWriter(tmp, hash), maxBinarySize)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("could not download the update: %w", err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return "", errors.New("the downloaded update does not match its checksum; nothing was changed")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	// Refuse a binary that does not start or is not the version we expect.
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmp.Name(), "--version").Output()
	if err != nil || !strings.Contains(string(out), rel.Tag) {
		return "", errors.New("the downloaded update failed its self-check; nothing was changed")
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", fmt.Errorf("could not replace the binary: %w", err)
	}
	return rel.Tag, nil
}
