package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// updateCheckTTL is how often we hit the GitHub API to look for a new
// release. Half a day is frequent enough to notice the same day a
// release ships without spamming the API on every zot launch.
const updateCheckTTL = 12 * time.Hour

// updateCheckFile is the on-disk cache, keyed to the current binary
// version. Writes a JSON blob with the last observed latest tag and
// when we checked, so we can skip the network call on most launches.
const updateCheckFile = "update-check.json"

// githubReleasesAPI is the REST endpoint we query. Using the API (not
// the HTML redirect) because the JSON response is stable and small.
const githubReleasesAPI = "https://api.github.com/repos/patriceckhart/zot/releases/latest"

// UpdateInfo describes the result of an update check. Zero-value means
// "no update available, no error, don't show anything".
type UpdateInfo struct {
	Current   string // e.g. "0.0.4"
	Latest    string // e.g. "0.0.5"
	Available bool   // true when latest > current
	URL       string // release page url for the changelog link
}

// updateCache is the on-disk structure written to $ZOT_HOME.
type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	// The version that was current when we last checked. Invalidates
	// the cache if the binary itself has been updated since.
	CurrentAt string `json:"current_at"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
}

// CheckForUpdate returns info about a newer release, using a cached
// result when one is fresh enough. Designed to be called at tui
// startup and rendered as a dismissible banner.
//
// Always returns a usable UpdateInfo (zero-value on error). The
// banner renderer skips the display when Available is false, so a
// network failure silently no-ops; we never block startup on this.
func CheckForUpdate(ctx context.Context, zotHome, currentVersion string) UpdateInfo {
	// Dev builds ("0.0.0") never have an update to offer. Skip.
	if currentVersion == "" || currentVersion == "dev" || currentVersion == "0.0.0" {
		return UpdateInfo{}
	}

	cachePath := filepath.Join(zotHome, updateCheckFile)
	if c, ok := readUpdateCache(cachePath); ok {
		// Cache is fresh and tracks the same binary version.
		if time.Since(c.CheckedAt) < updateCheckTTL && c.CurrentAt == currentVersion {
			return buildInfo(currentVersion, c.Latest, c.URL)
		}
	}

	latest, url, err := fetchLatestRelease(ctx)
	if err != nil {
		// Network or auth failure (common while the repo is private
		// and no GITHUB_TOKEN is set). Silent no-op; we'll try again
		// after the TTL on the next launch.
		return UpdateInfo{}
	}

	_ = writeUpdateCache(cachePath, updateCache{
		CheckedAt: time.Now().UTC(),
		CurrentAt: currentVersion,
		Latest:    latest,
		URL:       url,
	})

	return buildInfo(currentVersion, latest, url)
}

// CheckForUpdateAsync runs CheckForUpdate in a goroutine, delivers the
// result to the returned channel, and never blocks startup. The
// channel is always closed; receivers should `ok`-check.
func CheckForUpdateAsync(zotHome, currentVersion string) <-chan UpdateInfo {
	ch := make(chan UpdateInfo, 1)
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		ch <- CheckForUpdate(ctx, zotHome, currentVersion)
	}()
	return ch
}

func buildInfo(current, latest, url string) UpdateInfo {
	info := UpdateInfo{
		Current: current,
		Latest:  strings.TrimPrefix(latest, "v"),
		URL:     url,
	}
	info.Available = versionLess(info.Current, info.Latest)
	return info
}

// versionLess returns a < b for dotted semver-ish tags like "0.0.4".
// Non-numeric components compare as zero, which is fine for zot's
// x.y.z-only scheme.
func versionLess(a, b string) bool {
	as := splitVersion(a)
	bs := splitVersion(b)
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

func splitVersion(s string) []int {
	s = strings.TrimPrefix(s, "v")
	// Strip build-metadata suffix like "(abc1234, 2026-04-18)".
	if i := strings.IndexAny(s, " ("); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// fetchLatestRelease queries the GitHub API for the latest published
// release. Honours $GITHUB_TOKEN for private repos.
func fetchLatestRelease(ctx context.Context) (tag, url string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", githubReleasesAPI, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", "2022-11-28")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("github api %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}
	return body.TagName, body.HTMLURL, nil
}

// readUpdateCache loads the last check result. Returns ok=false on
// any error (missing file, bad json) so callers just treat it as a
// cache miss and re-fetch.
func readUpdateCache(path string) (updateCache, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateCache{}, false
	}
	var c updateCache
	if err := json.Unmarshal(b, &c); err != nil {
		return updateCache{}, false
	}
	return c, true
}

func writeUpdateCache(path string, c updateCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// cachedLatest, cachedLatestOnce: used elsewhere in the binary if we
// want a synchronous read without triggering a network call.
var (
	cachedLatest     string
	cachedLatestOnce sync.Once
)
