package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ChangelogInfo is what FetchChangelog returns. Body is the markdown
// from the GitHub release page; URL points back to that page so the
// dialog can offer "open in browser".
type ChangelogInfo struct {
	Version string
	Body    string
	URL     string
}

// FetchChangelog hits the GitHub releases API for the given version
// (must already include the leading "v") and returns the release
// notes body. Returns an empty ChangelogInfo on any failure or when
// the body is empty; the caller treats either as "skip silently".
//
// Honours $GITHUB_TOKEN for private-repo access. Times out at 4s so
// startup never blocks on a flaky network.
func FetchChangelog(ctx context.Context, version string) (ChangelogInfo, error) {
	if version == "" || version == "dev" || version == "0.0.0" {
		return ChangelogInfo{}, nil
	}
	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	url := fmt.Sprintf("https://api.github.com/repos/patriceckhart/zot/releases/tags/%s", tag)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ChangelogInfo{}, err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", "2022-11-28")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ChangelogInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ChangelogInfo{}, fmt.Errorf("github api %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ChangelogInfo{}, err
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		return ChangelogInfo{}, nil
	}
	return ChangelogInfo{
		Version: strings.TrimPrefix(body.TagName, "v"),
		Body:    body.Body,
		URL:     body.HTMLURL,
	}, nil
}

// FetchChangelogAsync runs FetchChangelog on a goroutine and delivers
// the result on the returned channel. Channel always closes.
func FetchChangelogAsync(version string) <-chan ChangelogInfo {
	ch := make(chan ChangelogInfo, 1)
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		info, _ := FetchChangelog(ctx, version)
		ch <- info
	}()
	return ch
}

// ShouldShowChangelog reports whether the running binary version
// differs from the last version whose changelog the user dismissed.
// Returns false on dev builds (version "" / "dev" / "0.0.0") and on
// the first-ever launch (no LastChangelogShown stored — we don't
// dump release notes at someone who just installed).
func ShouldShowChangelog(currentVersion string, cfg Config) bool {
	if currentVersion == "" || currentVersion == "dev" || currentVersion == "0.0.0" {
		return false
	}
	if cfg.LastChangelogShown == "" {
		return false
	}
	return cfg.LastChangelogShown != currentVersion
}

// MarkChangelogShown persists the version whose changelog the user
// just dismissed. Idempotent; safe to call when the dialog wasn't
// actually shown (e.g. fetch failed) so we don't keep retrying.
func MarkChangelogShown(version string) error {
	cfg, _ := LoadConfig()
	if cfg.LastChangelogShown == version {
		return nil
	}
	cfg.LastChangelogShown = version
	return SaveConfig(cfg)
}

// SeedChangelogVersion sets LastChangelogShown if it's currently
// empty. Called once on first-ever launch so future upgrades
// correctly trigger the dialog while THIS launch (which is also
// "first-ever") doesn't.
func SeedChangelogVersion(version string) {
	if version == "" || version == "dev" || version == "0.0.0" {
		return
	}
	cfg, err := LoadConfig()
	if err != nil {
		return
	}
	if cfg.LastChangelogShown != "" {
		return
	}
	cfg.LastChangelogShown = version
	_ = SaveConfig(cfg)
}
