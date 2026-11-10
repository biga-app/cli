package broadcast

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/biga-app/cli/internal/api"
	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
	"github.com/biga-app/cli/internal/config"
	"github.com/biga-app/cli/internal/session"
)

// fakeServer implements the session API in memory.
type fakeServer struct {
	t *testing.T

	mu          sync.Mutex
	upserts     int
	ended       bool
	heartbeats  int
	snapshots   []snapshotBody
	checkpoints []api.CheckpointUpload
	manifest    []api.ManifestEntry
	// failNext makes the next n checkpoint requests answer with failStatus.
	failNext   int
	failStatus int
	// tokens holds each session's owner token; an empty token marks a
	// session created before owner tokens, which refuses every write.
	tokens map[string]string
	// authFailures counts writes made without the right token.
	authFailures int
}

type snapshotBody struct {
	SessionID string
	Tree      treeNode
}

type treeNode struct {
	Type, Name, Path, Hash, Content string
	Size                            int64
	Binary, Ignored                 bool
	Children                        []treeNode
}

func (n treeNode) files() map[string]treeNode {
	out := map[string]treeNode{}
	var walk func(treeNode)
	walk = func(n treeNode) {
		if n.Type == "file" {
			out[n.Path] = n
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if s.tokens == nil {
		s.tokens = map[string]string{}
	}
	id := parts[0]
	token, exists := s.tokens[id]
	if r.Method == http.MethodPut && action == "" && !exists {
		token = fmt.Sprintf("bgs_%s_%d", id, len(s.tokens))
		s.tokens[id] = token
		s.upserts++
		s.ended = false
		writeProto(w, http.StatusCreated, sessionGrant(id, token))
		return
	}
	switch got := r.Header.Get("Authorization"); {
	case exists && token == "":
		s.authFailures++
		writeError(w, http.StatusForbidden, "session_locked", "locked")
		return
	case got == "":
		s.authFailures++
		writeError(w, http.StatusUnauthorized, "unauthorized", "token required")
		return
	case got != "Bearer "+token:
		s.authFailures++
		writeError(w, http.StatusForbidden, "forbidden", "wrong token")
		return
	}
	switch {
	case r.Method == http.MethodPut && action == "":
		s.upserts++
		s.ended = false
		writeProto(w, http.StatusOK, sessionGrant(id, ""))
	case r.Method == http.MethodDelete && action == "":
		s.ended = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "heartbeat":
		s.heartbeats++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && action == "manifest":
		files := make([]*bigav1.ManifestEntry, len(s.manifest))
		for i, f := range s.manifest {
			files[i] = &bigav1.ManifestEntry{Path: f.Path, Hash: f.Hash, Size: f.Size, Binary: f.Binary}
		}
		writeProto(w, http.StatusOK, &bigav1.Manifest{Files: files})
	case r.Method == http.MethodPost && action == "snapshot":
		var msg bigav1.FileSnapshot
		readProto(s.t, r, &msg)
		s.snapshots = append(s.snapshots, snapshotBody{SessionID: msg.GetSessionId(), Tree: treeFromPB(msg.GetTree())})
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPost && action == "checkpoint":
		if s.failNext > 0 {
			s.failNext--
			w.WriteHeader(s.failStatus)
			return
		}
		var msg bigav1.CheckpointUpload
		readProto(s.t, r, &msg)
		s.checkpoints = append(s.checkpoints, uploadFromPB(&msg))
		w.WriteHeader(http.StatusAccepted)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func readProto(t *testing.T, r *http.Request, m proto.Message) {
	t.Helper()
	if ct := r.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", ct)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
		return
	}
	if err := proto.Unmarshal(data, m); err != nil {
		t.Errorf("decode %T: %v", m, err)
	}
}

func writeProto(w http.ResponseWriter, status int, m proto.Message) {
	data, _ := proto.Marshal(m)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(status)
	w.Write(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeProto(w, status, &bigav1.Error{Code: code, Message: message})
}

func sessionGrant(id, token string) *bigav1.SessionGrant {
	return &bigav1.SessionGrant{
		Session: &bigav1.Session{
			Id: id, Title: "t", Status: bigav1.SessionStatus_SESSION_STATUS_ACTIVE,
			CreatedAt: "2026-01-01T00:00:00.000Z", UpdatedAt: "2026-01-01T00:00:00.000Z",
		},
		Token: token,
	}
}

func treeFromPB(n *bigav1.FileNode) treeNode {
	out := treeNode{Name: n.GetName(), Path: n.GetPath()}
	if f := n.GetFile(); f != nil {
		out.Type, out.Size, out.Hash, out.Binary, out.Content = "file", f.GetSize(), f.GetHash(), f.GetBinary(), string(f.GetContent())
		return out
	}
	out.Type, out.Ignored = "directory", n.GetDirectory().GetIgnored()
	for _, child := range n.GetDirectory().GetChildren() {
		out.Children = append(out.Children, treeFromPB(child))
	}
	return out
}

var changeTypes = map[bigav1.ChangeType]api.ChangeType{
	bigav1.ChangeType_CHANGE_TYPE_ADDED:    api.ChangeAdded,
	bigav1.ChangeType_CHANGE_TYPE_MODIFIED: api.ChangeModified,
	bigav1.ChangeType_CHANGE_TYPE_DELETED:  api.ChangeDeleted,
}

func uploadFromPB(u *bigav1.CheckpointUpload) api.CheckpointUpload {
	out := api.CheckpointUpload{SessionID: u.GetSessionId(), Timestamp: u.GetTimestamp()}
	for _, e := range u.GetEntries() {
		out.Entries = append(out.Entries, api.CheckpointEntry{
			Path: e.GetPath(), ChangeType: changeTypes[e.GetChangeType()], Hash: e.Hash,
			Size: e.GetSize(), Binary: e.GetBinary(), Content: string(e.GetContent()),
		})
	}
	return out
}

// changes flattens every checkpoint entry received so far into path→type.
func (s *fakeServer) changes() map[string]api.ChangeType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]api.ChangeType{}
	for _, c := range s.checkpoints {
		for _, e := range c.Entries {
			out[e.Path] = e.ChangeType
		}
	}
	return out
}

func (s *fakeServer) with(fn func(*fakeServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// events records what a broadcast reports.
type events struct {
	mu       sync.Mutex
	synced   []Checkpoint
	online   []bool
	progress []string
	warnings []string
}

func (e *events) Info(string)  {}
func (e *events) Error(string) {}

func (e *events) Warn(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.warnings = append(e.warnings, msg)
}

func (e *events) warned() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.warnings)
}

func (e *events) Progress(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.progress = append(e.progress, msg)
}

func (e *events) Synced(c Checkpoint) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.synced = append(e.synced, c)
}

func (e *events) Online(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.online = append(e.online, v)
}

func (e *events) onlineChanges() []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.online)
}

type harness struct {
	t      *testing.T
	root   string
	server *fakeServer
	store  *session.Store
	events *events
	opts   Options
}

func newHarness(t *testing.T, files map[string]string) *harness {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		write(t, root, rel, content)
	}
	server := &fakeServer{t: t}
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)

	store := session.NewStore(t.TempDir())
	ev := &events{}
	return &harness{
		t: t, root: root, server: server, store: store, events: ev,
		opts: Options{
			Client:    api.New(srv.URL, api.WithBackoff(func(int) time.Duration { return time.Millisecond })),
			Store:     store,
			Events:    ev,
			APIURL:    srv.URL,
			RootDir:   root,
			Timing:    config.Timing{Idle: 20 * time.Millisecond, MaxWait: 200 * time.Millisecond, Stability: 20 * time.Millisecond},
			Heartbeat: 50 * time.Millisecond,
		},
	}
}

func (h *harness) start() *Broadcast {
	h.t.Helper()
	b, err := New(h.opts)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := b.Start(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { b.Stop(h.t.Context()) })
	return b
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBroadcastDeletesFilesUnderReplacedDirectory(t *testing.T) {
	const rel = "shared/a.txt"
	h := newHarness(t, map[string]string{rel: "included"})
	h.opts.Timing.Idle = 250 * time.Millisecond
	h.start()

	outside := t.TempDir()
	write(t, outside, "a.txt", "outside")
	dir := filepath.Join(h.root, "shared")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("creating symlinks requires permission")
		}
		t.Fatal(err)
	}
	eventually(t, "checkpoint for the replaced directory", func() bool {
		return h.server.changes()[rel] != ""
	})
	h.server.with(func(s *fakeServer) {
		for _, checkpoint := range s.checkpoints {
			for _, entry := range checkpoint.Entries {
				if entry.Path == rel && entry.ChangeType != api.ChangeDeleted {
					t.Errorf("got %s for %s, want deletion", entry.ChangeType, rel)
				}
			}
		}
	})
}

func TestBroadcastStreamsChanges(t *testing.T) {
	h := newHarness(t, map[string]string{
		"main.go":        "package main",
		"lib/a.go":       "package lib",
		"lib/b.go":       "package lib",
		"node_modules/x": "ignored",
	})
	b := h.start()

	h.server.with(func(s *fakeServer) {
		if len(s.snapshots) != 1 {
			t.Fatalf("got %d snapshots, want 1", len(s.snapshots))
		}
		files := s.snapshots[0].Tree.files()
		if _, ok := files["main.go"]; !ok || len(files) != 3 {
			t.Errorf("snapshot files = %v", slices.Sorted(func(yield func(string) bool) {
				for k := range files {
					if !yield(k) {
						return
					}
				}
			}))
		}
	})

	rec, ok, err := h.store.Lookup(h.root)
	if err != nil || !ok || rec.SessionID != b.SessionID() || !rec.Live(time.Now()) {
		t.Fatalf("registry record = %+v, %v, %v", rec, ok, err)
	}

	write(t, h.root, "main.go", "package main // edited")
	write(t, h.root, "new.go", "package main")
	write(t, h.root, "node_modules/y", "still ignored")
	eventually(t, "edit and create", func() bool {
		c := h.server.changes()
		return c["main.go"] == api.ChangeModified && c["new.go"] == api.ChangeAdded
	})

	if err := os.RemoveAll(filepath.Join(h.root, "lib")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "directory removal", func() bool {
		c := h.server.changes()
		return c["lib/a.go"] == api.ChangeDeleted && c["lib/b.go"] == api.ChangeDeleted
	})

	for path := range h.server.changes() {
		if strings.HasPrefix(path, "node_modules") {
			t.Errorf("ignored path %s was synced", path)
		}
	}
	eventually(t, "heartbeats", func() bool {
		var n int
		h.server.with(func(s *fakeServer) { n = s.heartbeats })
		return n > 0
	})

	h.events.mu.Lock()
	var reported int
	for _, c := range h.events.synced {
		reported += len(c.Changes)
		if c.Bytes <= 0 {
			t.Errorf("checkpoint #%d reported %d bytes sent", c.Seq, c.Bytes)
		}
	}
	gotProgress := len(h.events.progress) > 0
	h.events.mu.Unlock()
	if reported < 4 || !gotProgress {
		t.Errorf("events reported %d changes, progress %v", reported, gotProgress)
	}

	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.server.with(func(s *fakeServer) {
		if !s.ended {
			t.Error("session was not ended")
		}
	})
	rec, _, _ = h.store.Lookup(h.root)
	if rec.Live(time.Now()) || rec.CheckpointsSynced == 0 || rec.LastSeenAt.IsZero() {
		t.Errorf("record after stop = %+v", rec)
	}
}

func TestBroadcastSkipsUnchangedSaves(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "same"})
	h.start()

	write(t, h.root, "a.txt", "same") // a save that changes nothing
	write(t, h.root, "b.txt", "marker")
	eventually(t, "marker", func() bool { return h.server.changes()["b.txt"] != "" })
	if _, ok := h.server.changes()["a.txt"]; ok {
		t.Error("an unchanged save produced a checkpoint entry")
	}
}

func TestBroadcastRejoinUploadsOnlyDifferences(t *testing.T) {
	h := newHarness(t, map[string]string{"same.txt": "same", "changed.txt": "new"})

	first := h.start()
	if err := first.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	var sameHash string
	h.server.with(func(s *fakeServer) {
		sameHash = s.snapshots[0].Tree.files()["same.txt"].Hash
		s.manifest = []api.ManifestEntry{
			{Path: "same.txt", Hash: sameHash},
			{Path: "changed.txt", Hash: strings.Repeat("0", 64)},
			{Path: "gone.txt", Hash: strings.Repeat("1", 64)},
		}
	})

	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Rejoined() || b.SessionID() != first.SessionID() {
		t.Fatalf("rejoin used session %q, want %q", b.SessionID(), first.SessionID())
	}
	res, err := b.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Stop(t.Context()) })

	if res.Sync != SyncedChanges || res.Files != 2 {
		t.Errorf("Result = %+v, want 2 changed files", res)
	}
	want := map[string]api.ChangeType{"changed.txt": api.ChangeModified, "gone.txt": api.ChangeDeleted}
	if got := h.server.changes(); len(got) != 2 || got["changed.txt"] != want["changed.txt"] || got["gone.txt"] != want["gone.txt"] {
		t.Errorf("rejoin changes = %v, want %v", got, want)
	}
	h.server.with(func(s *fakeServer) {
		if len(s.snapshots) != 1 {
			t.Errorf("rejoin uploaded a full snapshot")
		}
	})
}

func TestBroadcastRetriesWhileServerIsDown(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	h.start()

	// Enough failures to exhaust one request's retries, forcing a requeue.
	h.server.with(func(s *fakeServer) { s.failNext, s.failStatus = 6, http.StatusServiceUnavailable })
	write(t, h.root, "a.txt", "v2")
	eventually(t, "the edit to sync after the outage", func() bool {
		return h.server.changes()["a.txt"] == api.ChangeModified
	})
	if got := h.events.onlineChanges(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("online changes = %v, want [false true]", got)
	}
}

func (s *fakeServer) entry(path string) (api.CheckpointEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range slices.Backward(s.checkpoints) {
		for _, e := range c.Entries {
			if e.Path == path {
				return e, true
			}
		}
	}
	return api.CheckpointEntry{}, false
}

func TestBroadcastWithholdsSecrets(t *testing.T) {
	secret := "key = AKIA" + "IOSFODNN7EXAMPLE\n"
	h := newHarness(t, map[string]string{"ok.txt": "fine", "config.txt": secret})
	h.start()

	h.server.with(func(s *fakeServer) {
		files := s.snapshots[0].Tree.files()
		if n := files["config.txt"]; !n.Binary || n.Content != "" {
			t.Errorf("config.txt in snapshot = %+v", n)
		}
		if n := files["ok.txt"]; n.Content != "fine" {
			t.Errorf("ok.txt in snapshot = %+v", n)
		}
	})
	if got := h.events.warned(); len(got) != 1 || !strings.Contains(got[0], "Withheld config.txt") || !strings.Contains(got[0], "AWS access key on line 1") {
		t.Errorf("warnings = %q", got)
	}

	write(t, h.root, "later.txt", "token: ghp_"+strings.Repeat("a1B2", 9))
	eventually(t, "the new secret file", func() bool { return h.server.changes()["later.txt"] == api.ChangeAdded })
	if e, _ := h.server.entry("later.txt"); !e.Binary || e.Content != "" {
		t.Errorf("later.txt entry = %+v", e)
	}

	write(t, h.root, "config.txt", "key = redacted\n")
	eventually(t, "the cleaned file", func() bool { return h.server.changes()["config.txt"] == api.ChangeModified })
	if e, _ := h.server.entry("config.txt"); e.Binary || e.Content != "key = redacted\n" {
		t.Errorf("cleaned config.txt entry = %+v", e)
	}
}

func TestBroadcastSecretScanCanBeDisabled(t *testing.T) {
	secret := "key = AKIA" + "IOSFODNN7EXAMPLE\n"
	h := newHarness(t, map[string]string{"config.txt": secret})
	h.opts.NoSecretScan = true
	h.start()

	h.server.with(func(s *fakeServer) {
		if n := s.snapshots[0].Tree.files()["config.txt"]; n.Content != secret {
			t.Errorf("config.txt in snapshot = %+v", n)
		}
	})
	if got := h.events.warned(); len(got) != 0 {
		t.Errorf("warnings = %q", got)
	}
}

func TestBroadcastOnlySharesListedPaths(t *testing.T) {
	h := newHarness(t, map[string]string{"src/a.go": "package a", "docs/b.md": "b", "root.go": "package main"})
	h.opts.Only = []string{"src"}
	h.start()

	h.server.with(func(s *fakeServer) {
		files := s.snapshots[0].Tree.files()
		if _, ok := files["src/a.go"]; !ok || len(files) != 1 {
			t.Errorf("snapshot files = %v", files)
		}
	})

	write(t, h.root, "docs/c.md", "outside")
	write(t, h.root, "newdir/x.go", "outside")
	write(t, h.root, "src/d.go", "package a")
	eventually(t, "the edit inside the listed path", func() bool { return h.server.changes()["src/d.go"] == api.ChangeAdded })
	for path := range h.server.changes() {
		if !strings.HasPrefix(path, "src/") {
			t.Errorf("%s was synced outside --only", path)
		}
	}
}

func TestBroadcastReloadsIgnoreRules(t *testing.T) {
	h := newHarness(t, map[string]string{
		".gitignore":  "secret.txt\n",
		"secret.txt":  "hidden at first",
		"draft.md":    "shared at first",
		"pkg/keep.go": "package pkg",
	})
	h.start()

	write(t, h.root, ".gitignore", "draft.md\n")
	eventually(t, "the new rules to apply", func() bool {
		c := h.server.changes()
		return c["draft.md"] == api.ChangeDeleted && c["secret.txt"] == api.ChangeAdded
	})
	if _, ok := h.server.changes()["pkg/keep.go"]; ok {
		t.Error("an unaffected file was re-sent")
	}

	// A nested ignore file, created mid-session, applies beneath it.
	write(t, h.root, "pkg/.bigaignore", "keep.go\n")
	eventually(t, "the nested rules to apply", func() bool {
		return h.server.changes()["pkg/keep.go"] == api.ChangeDeleted
	})
}

func TestBroadcastReactivatesEndedSession(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	h.start()

	var before int
	h.server.with(func(s *fakeServer) {
		before = s.upserts
		s.failNext, s.failStatus = 1, http.StatusConflict
	})
	write(t, h.root, "a.txt", "v2")
	eventually(t, "the edit to sync after reactivation", func() bool {
		return h.server.changes()["a.txt"] == api.ChangeModified
	})
	h.server.with(func(s *fakeServer) {
		if s.upserts <= before {
			t.Error("the session was not reactivated")
		}
	})
}

func TestBroadcastHonorsStopRequests(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	b := h.start()
	if err := h.store.RequestStop(b.SessionID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.StopRequested():
	case <-time.After(5 * time.Second):
		t.Fatal("stop request was not noticed")
	}
}

func TestBroadcastRefusesDirectoryInUse(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.store.Save(session.Record{
		SessionID: "other", RootDir: h.root, PID: os.Getppid(), LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(h.opts); err == nil || !strings.Contains(err.Error(), "already being broadcast") {
		t.Fatalf("New = %v, want an already-broadcasting error", err)
	}
}

func TestChunkRespectsLimit(t *testing.T) {
	hash := strings.Repeat("a", 64)
	entry := func(size int) api.CheckpointEntry {
		return api.CheckpointEntry{Path: "f", ChangeType: api.ChangeAdded, Hash: &hash, Content: strings.Repeat("x", size)}
	}
	entries := []api.CheckpointEntry{entry(400), entry(400), entry(400), entry(2000)}
	// Each 400-byte entry encodes to roughly 530 bytes.
	chunks := chunk(entries, 1200)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if len(chunks[0]) != 2 || len(chunks[1]) != 1 || len(chunks[2]) != 1 {
		t.Errorf("chunk sizes = %d, %d, %d", len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
}

func TestBroadcastKeepsItsOwnerToken(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	b := h.start()
	write(t, h.root, "a.txt", "v2")
	eventually(t, "the edit to sync", func() bool { return h.server.changes()["a.txt"] == api.ChangeModified })

	rec, ok, err := h.store.Lookup(h.root)
	if err != nil || !ok {
		t.Fatalf("Lookup = %v, %v", ok, err)
	}
	var issued string
	h.server.with(func(s *fakeServer) { issued = s.tokens[b.SessionID()] })
	if rec.Token == "" || rec.Token != issued {
		t.Errorf("saved token %q, server issued %q", rec.Token, issued)
	}
	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The next run rejoins with the saved token.
	again := h.start()
	if !again.Rejoined() || again.SessionID() != b.SessionID() {
		t.Errorf("rejoin used session %q", again.SessionID())
	}
	h.server.with(func(s *fakeServer) {
		if s.authFailures != 0 {
			t.Errorf("%d writes were refused", s.authFailures)
		}
	})
}

func TestBroadcastMovesOffALockedSession(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	// A session from before owner tokens: saved locally, no token anywhere.
	if err := h.store.Save(session.Record{SessionID: "legacy01", RootDir: h.root, Title: "old"}); err != nil {
		t.Fatal(err)
	}
	h.server.with(func(s *fakeServer) { s.tokens = map[string]string{"legacy01": ""} })

	b := h.start()
	if b.SessionID() == "legacy01" || b.Rejoined() {
		t.Fatalf("broadcast stayed on the locked session (%s, rejoined=%v)", b.SessionID(), b.Rejoined())
	}
	h.server.with(func(s *fakeServer) {
		if len(s.snapshots) != 1 || s.snapshots[0].SessionID != b.SessionID() {
			t.Errorf("snapshots = %+v", s.snapshots)
		}
	})
	rec, _, _ := h.store.Lookup(h.root)
	if rec.SessionID != b.SessionID() || rec.Token == "" {
		t.Errorf("registry = %+v", rec)
	}
}

func TestBroadcastRefusesSomeoneElsesSession(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "v1"})
	h.server.with(func(s *fakeServer) { s.tokens = map[string]string{"taken": "bgs_theirs"} })
	h.opts.SessionID = "taken"
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "belongs to another broadcast") {
		t.Fatalf("Start = %v, want an ownership error", err)
	}
}
