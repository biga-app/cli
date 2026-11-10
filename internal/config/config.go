// Package config resolves runtime settings from the environment.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the server used when BIGA_API_URL is unset. Release builds
// override it at link time:
//
//	go build -ldflags "-X github.com/biga-app/cli/internal/config.DefaultAPIURL=https://example.com"
var DefaultAPIURL = "http://localhost:3000"

// Environment variables read by [Load].
const (
	EnvAPIURL        = "BIGA_API_URL"
	EnvAccessKey     = "BIGA_ACCESS_KEY"
	EnvHome          = "BIGA_HOME"
	EnvIdleMS        = "BIGA_IDLE_MS"
	EnvMaxWaitMS     = "BIGA_MAX_WAIT_MS"
	EnvStabilityMS   = "BIGA_STABILITY_MS"
	EnvHeartbeatMS   = "BIGA_HEARTBEAT_MS"
	EnvNoUpdateCheck = "BIGA_NO_UPDATE_CHECK"
	EnvSecretScan    = "BIGA_SECRET_SCAN"
)

// Timing holds the knobs that dominate the latency between a save and a
// viewer seeing it. Defaults are tuned for a near-live feel; each can be raised
// for slow disks or filesystems that report noisy partial writes.
type Timing struct {
	// Idle is the quiet gap after the last change before a checkpoint is sent.
	Idle time.Duration
	// MaxWait caps how long a sustained burst of edits can delay a checkpoint.
	MaxWait time.Duration
	// Stability is how long a file must stop changing before it is read, which
	// guards against uploading a half-written file.
	Stability time.Duration
}

// Config is the resolved runtime configuration.
type Config struct {
	// APIURL is the server base URL without a trailing slash.
	APIURL string
	// AccessKey is sent to servers that only accept broadcasts from people
	// holding a key. Empty for open servers.
	AccessKey string
	// Home is the directory holding local state, normally ~/.biga.
	Home   string
	Timing Timing
	// Heartbeat is how often a broadcast tells the server it is still live. It
	// must stay well under the server's staleness window.
	Heartbeat time.Duration
	// UpdateCheck reports whether to look for newer releases.
	UpdateCheck bool
	// SecretScan reports whether files whose content looks like a secret are
	// withheld. BIGA_SECRET_SCAN=off turns it off.
	SecretScan bool
}

// Load reads the configuration using getenv, normally [os.Getenv].
func Load(getenv func(string) string) (Config, error) {
	apiURL, err := NormalizeAPIURL(cmp.Or(getenv(EnvAPIURL), DefaultAPIURL))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvAPIURL, err)
	}

	home := getenv(EnvHome)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("locate home directory (set %s to override): %w", EnvHome, err)
		}
		home = filepath.Join(userHome, ".biga")
	}

	return Config{
		APIURL:    apiURL,
		AccessKey: strings.TrimSpace(getenv(EnvAccessKey)),
		Home:      home,
		Timing: Timing{
			Idle:      millis(getenv(EnvIdleMS), 120*time.Millisecond),
			MaxWait:   millis(getenv(EnvMaxWaitMS), time.Second),
			Stability: millis(getenv(EnvStabilityMS), 75*time.Millisecond),
		},
		Heartbeat:   millis(getenv(EnvHeartbeatMS), 10*time.Second),
		UpdateCheck: getenv(EnvNoUpdateCheck) == "" && getenv("CI") == "",
		SecretScan:  !strings.EqualFold(strings.TrimSpace(getenv(EnvSecretScan)), "off"),
	}, nil
}

// NormalizeAPIURL validates an http(s) base URL and strips trailing slashes.
func NormalizeAPIURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q must be an http or https URL", raw)
	}
	if u.Host == "" {
		return "", errors.New("URL has no host")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// millis parses a positive integer count of milliseconds, falling back when
// the value is unset or invalid.
func millis(value string, fallback time.Duration) time.Duration {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Millisecond
}
