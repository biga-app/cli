// Package update tells users when a newer biga release is available.
//
// Checks never delay a command: the latest known release is read from a
// local cache, and the cache is refreshed in the background at most once a
// day.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// ReleasesURL is where users download new versions.
	ReleasesURL = "https://github.com/biga-app/cli/releases/latest"

	latestReleaseAPI = "https://api.github.com/repos/biga-app/cli/releases/latest"
	checkInterval    = 24 * time.Hour
	fetchTimeout     = 5 * time.Second
)

// Checker looks up the latest release.
type Checker struct {
	// Current is the running version, e.g. "v1.2.3".
	Current string
	// CachePath is the JSON file the latest known release is stored in.
	CachePath string
	// Endpoint is the GitHub "latest release" API URL; empty uses the default.
	Endpoint string
	// HTTP is the client used for the lookup; nil uses http.DefaultClient.
	HTTP *http.Client
}

type cache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
}

// Available returns the newer release recorded in the cache, if any.
func (c *Checker) Available() (string, bool) {
	cached, err := c.read()
	if err != nil || !IsNewer(c.Current, cached.Latest) {
		return "", false
	}
	return cached.Latest, true
}

// Refresh fetches the latest release when the cache is older than a day.
func (c *Checker) Refresh(ctx context.Context) error {
	if cached, err := c.read(); err == nil && time.Since(cached.CheckedAt) < checkInterval {
		return nil
	}
	latest, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(cache{CheckedAt: time.Now(), Latest: latest})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.CachePath, data, 0o600)
}

func (c *Checker) read() (cache, error) {
	var cached cache
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return cached, err
	}
	err = json.Unmarshal(data, &cached)
	return cached, err
}

func (c *Checker) fetch(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = latestReleaseAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "biga-cli/"+c.Current)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release lookup: %s", resp.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	if !semver.IsValid(canonical(release.TagName)) {
		return "", errors.New("latest release has no semantic version tag")
	}
	return canonical(release.TagName), nil
}

// IsNewer reports whether latest is a newer release than current. Versions
// that are not semantic versions, such as "dev", never compare as older.
func IsNewer(current, latest string) bool {
	current, latest = canonical(current), canonical(latest)
	return semver.IsValid(current) && semver.IsValid(latest) && semver.Compare(latest, current) > 0
}

func canonical(v string) string {
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
}
