package config

import (
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvHome: "/tmp/biga"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
	if cfg.Home != "/tmp/biga" {
		t.Errorf("Home = %q", cfg.Home)
	}
	want := Timing{Idle: 120 * time.Millisecond, MaxWait: time.Second, Stability: 75 * time.Millisecond}
	if cfg.Timing != want {
		t.Errorf("Timing = %+v, want %+v", cfg.Timing, want)
	}
	if cfg.Heartbeat != 10*time.Second || !cfg.UpdateCheck {
		t.Errorf("Heartbeat = %v, UpdateCheck = %v", cfg.Heartbeat, cfg.UpdateCheck)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvHome:          "/h",
		EnvAPIURL:        "https://biga.example.com/",
		EnvIdleMS:        "300",
		EnvMaxWaitMS:     "nope",
		EnvStabilityMS:   "-5",
		EnvHeartbeatMS:   "5000",
		EnvNoUpdateCheck: "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "https://biga.example.com" {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
	if cfg.Timing.Idle != 300*time.Millisecond {
		t.Errorf("Idle = %v", cfg.Timing.Idle)
	}
	if cfg.Timing.MaxWait != time.Second || cfg.Timing.Stability != 75*time.Millisecond {
		t.Errorf("invalid values should fall back, got %+v", cfg.Timing)
	}
	if cfg.Heartbeat != 5*time.Second || cfg.UpdateCheck {
		t.Errorf("Heartbeat = %v, UpdateCheck = %v", cfg.Heartbeat, cfg.UpdateCheck)
	}
}

func TestLoadSecretScan(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"on", true},
		{"1", true},
		{"off", false},
		{" OFF ", false},
	}
	for _, tt := range tests {
		cfg, err := Load(env(map[string]string{EnvHome: "/h", EnvSecretScan: tt.value}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SecretScan != tt.want {
			t.Errorf("%s=%q: SecretScan = %v, want %v", EnvSecretScan, tt.value, cfg.SecretScan, tt.want)
		}
	}
}

func TestNormalizeAPIURL(t *testing.T) {
	for _, bad := range []string{"localhost:3000", "ftp://x", "https://", ""} {
		if _, err := NormalizeAPIURL(bad); err == nil {
			t.Errorf("NormalizeAPIURL(%q) succeeded, want error", bad)
		}
	}
	got, err := NormalizeAPIURL(" http://localhost:3000// ")
	if err != nil || got != "http://localhost:3000" {
		t.Errorf("got %q, %v", got, err)
	}
}
