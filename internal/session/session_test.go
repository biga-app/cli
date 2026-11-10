package session

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	dir := t.TempDir()

	if _, ok, err := s.Lookup(dir); err != nil || ok {
		t.Fatalf("Lookup on empty store = %v, %v", ok, err)
	}

	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := Record{SessionID: "abc12345", Title: "demo", RootDir: dir, StartedAt: started, APIURL: "http://x"}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Lookup(dir)
	if err != nil || !ok {
		t.Fatalf("Lookup = %v, %v", ok, err)
	}
	if got.SessionID != rec.SessionID || !got.StartedAt.Equal(started) {
		t.Errorf("Lookup = %+v", got)
	}
	if got.ShareURL() != "http://x/session/abc12345" {
		t.Errorf("ShareURL = %q", got.ShareURL())
	}

	if err := s.Remove(dir); err != nil {
		t.Fatal(err)
	}
	records, err := s.List()
	if err != nil || len(records) != 0 {
		t.Fatalf("List after Remove = %v, %v", records, err)
	}
}

func TestStoreConcurrentSaves(t *testing.T) {
	s := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			dir := filepath.Join(t.TempDir(), "p")
			if err := s.Save(Record{SessionID: NewID(), RootDir: dir, Title: string(rune('a' + i))}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	records, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 20 {
		t.Errorf("got %d records, want 20: concurrent writes lost data", len(records))
	}
}

func TestStoreRecoversStaleLock(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := os.Mkdir(s.lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(s.lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Record{SessionID: "abc", RootDir: t.TempDir()}); err != nil {
		t.Fatalf("Save with stale lock: %v", err)
	}
}

func TestStoreRejectsCorruptRegistry(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := os.WriteFile(s.registryPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("List succeeded on a corrupt registry")
	}
}

func TestRecordLive(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		rec  Record
		want bool
	}{
		{"this process, fresh", Record{PID: os.Getpid(), LastSeenAt: now}, true},
		{"this process, stale", Record{PID: os.Getpid(), LastSeenAt: now.Add(-time.Minute)}, false},
		{"no pid", Record{LastSeenAt: now}, false},
		{"never seen", Record{PID: os.Getpid()}, false},
	}
	for _, tt := range tests {
		if got := tt.rec.Live(now); got != tt.want {
			t.Errorf("%s: Live = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStopRequests(t *testing.T) {
	s := NewStore(t.TempDir())
	if ok, err := s.ConsumeStop("abc"); err != nil || ok {
		t.Fatalf("ConsumeStop before request = %v, %v", ok, err)
	}
	if err := s.RequestStop("abc"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ConsumeStop("abc"); err != nil || !ok {
		t.Fatalf("ConsumeStop after request = %v, %v", ok, err)
	}
	if ok, _ := s.ConsumeStop("abc"); ok {
		t.Error("a stop request was consumed twice")
	}
	if err := s.RequestStop("../escape"); err == nil {
		t.Error("RequestStop accepted a path-traversing id")
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID()
		if len(id) != idLength {
			t.Fatalf("NewID() = %q, want %d chars", id, idLength)
		}
		if err := ValidateID(id); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"abc", "k3f9zq2a", "ws-abc", "My_Session-1"} {
		if err := ValidateID(ok); err != nil {
			t.Errorf("ValidateID(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-abc", "a/b", "a b", "../x", string(make([]byte, 65))} {
		if err := ValidateID(bad); err == nil {
			t.Errorf("ValidateID(%q) succeeded", bad)
		}
	}
}

func TestShareURLCarriesTheKeyInTheFragment(t *testing.T) {
	rec := Record{SessionID: "abc", APIURL: "https://biga.example/", Key: "KEYTEXT"}
	if got := rec.ShareURL(); got != "https://biga.example/session/abc#k=KEYTEXT" {
		t.Errorf("ShareURL = %q", got)
	}
	rec.ShareLink = "https://share.example/session/abc"
	if got := rec.ShareURL(); got != "https://share.example/session/abc#k=KEYTEXT" {
		t.Errorf("ShareURL with a server link = %q", got)
	}
	rec.Key = ""
	if got := rec.ShareURL(); got != "https://share.example/session/abc" {
		t.Errorf("an unencrypted link has no fragment: %q", got)
	}
	if WithKey("https://x/y", "") != "https://x/y" {
		t.Error("an empty key leaves the link alone")
	}
}

func TestTheKeyIsSavedInTheRegistryFileOnlyThisUserCanRead(t *testing.T) {
	home := t.TempDir()
	s := NewStore(home)
	dir := t.TempDir()
	if err := s.Save(Record{RootDir: dir, SessionID: "abc", Token: "bgs_x", Key: "KEYTEXT"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Lookup(dir)
	if err != nil || !ok || got.Key != "KEYTEXT" {
		t.Fatalf("lookup = %+v, %v, %v", got, ok, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != "sessions.json" {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("sessions.json mode = %v, %v", info.Mode(), err)
		}
	}
}
