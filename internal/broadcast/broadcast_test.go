package broadcast

import (
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/biga-app/cli/internal/e2ee"
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
	// blobs is the content uploaded ahead of snapshots, by hash.
	blobs          map[string]string
	blobBatches    [][]string
	missingQueries int
	// rejectSnapshots makes the next n snapshots fail as if content were missing.
	rejectSnapshots int
	snapshotTries   int
	// limits is reported when a session is created; zero fields are omitted.
	limits bigav1.Limits
	// encrypted records which sessions were created encrypted; ignoreEncrypt
	// makes the server answer as one that does not know about encryption.
	encrypted     map[string]bool
	ignoreEncrypt bool
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
	action, sub := "", ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 2 {
		sub = parts[2]
	}
	if s.tokens == nil {
		s.tokens = map[string]string{}
	}
	id := parts[0]
	token, exists := s.tokens[id]
	if r.Method == http.MethodPut && action == "" && !exists {
		if s.encrypted == nil {
			s.encrypted = map[string]bool{}
		}
		var put bigav1.PutSessionRequest
		if body, _ := io.ReadAll(r.Body); len(body) > 0 {
			_ = proto.Unmarshal(body, &put)
		}
		s.encrypted[id] = put.GetEncrypted() && !s.ignoreEncrypt
		token = fmt.Sprintf("bgs_%s_%d", id, len(s.tokens))
		s.tokens[id] = token
		s.upserts++
		s.ended = false
		writeProto(w, http.StatusCreated, s.grant(id, token))
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
		writeProto(w, http.StatusOK, s.grant(id, ""))
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
	case r.Method == http.MethodPost && action == "blobs" && sub == "missing":
		var msg bigav1.MissingBlobsRequest
		readProto(s.t, r, &msg)
		s.missingQueries++
		var missing []string
		for _, h := range msg.GetHashes() {
			if _, ok := s.blobs[h]; !ok {
				missing = append(missing, h)
			}
		}
		writeProto(w, http.StatusOK, &bigav1.MissingBlobsResponse{Hashes: missing})
	case r.Method == http.MethodPost && action == "blobs":
		var msg bigav1.UploadBlobsRequest
		readProto(s.t, r, &msg)
		if s.blobs == nil {
			s.blobs = map[string]string{}
		}
		var batch []string
		for _, b := range msg.GetBlobs() {
			sum := sha256.Sum256(b.GetContent())
			if !s.encrypted[id] && hex.EncodeToString(sum[:]) != b.GetHash() {
				s.t.Errorf("blob %s does not match its content", b.GetHash())
			}
			s.blobs[b.GetHash()] = string(b.GetContent())
			batch = append(batch, b.GetHash())
		}
		s.blobBatches = append(s.blobBatches, batch)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPost && action == "snapshot":
		var msg bigav1.FileSnapshot
		readProto(s.t, r, &msg)
		s.snapshotTries++
		tree := treeFromPB(msg.GetTree())
		if missing := s.resolve(&tree); len(missing) > 0 || s.rejectSnapshots > 0 {
			if s.rejectSnapshots > 0 {
				s.rejectSnapshots--
			}
			writeError(w, http.StatusConflict, "missing_blob", "content was not uploaded")
			return
		}
		s.snapshots = append(s.snapshots, snapshotBody{SessionID: msg.GetSessionId(), Tree: tree})
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

var emptyHash = func() string { sum := sha256.Sum256(nil); return hex.EncodeToString(sum[:]) }()

// resolve fills in the content of text files from the uploaded blobs, as the
// server does, and returns the hashes it has no blob for.
func (s *fakeServer) resolve(n *treeNode) (missing []string) {
	if n.Type == "file" {
		if n.Binary || n.Content != "" || n.Hash == emptyHash {
			return nil
		}
		if content, ok := s.blobs[n.Hash]; ok {
			n.Content = content
			return nil
		}
		return []string{n.Hash}
	}
	for i := range n.Children {
		missing = append(missing, s.resolve(&n.Children[i])...)
	}
	return missing
}

func (s *fakeServer) grant(id, token string) *bigav1.SessionGrant {
	g := sessionGrant(id, token)
	g.Session.Encrypted = s.encrypted[id]
	if s.limits.GetMaxSnapshotFiles() > 0 || s.limits.GetMaxBlobBatchBytes() > 0 {
		g.Limits = &bigav1.Limits{MaxSnapshotFiles: s.limits.GetMaxSnapshotFiles(), MaxBlobBatchBytes: s.limits.GetMaxBlobBatchBytes()}
	}
	return g
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
			Additions: int(e.GetAdditions()), Deletions: int(e.GetDeletions()),
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

func (s *fakeServer) uploadedBlobs() (batches [][]string, queries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.blobBatches), s.missingQueries
}

func TestBroadcastUploadsContentBeforeTheSnapshot(t *testing.T) {
	h := newHarness(t, map[string]string{
		"a.txt": "alpha\n", "b.txt": "beta\n", "copy.txt": "alpha\n", "empty.txt": "", "logo.bin": "\x00\x01",
	})
	h.start()

	batches, queries := h.server.uploadedBlobs()
	var uploaded []string
	for _, b := range batches {
		uploaded = append(uploaded, b...)
	}
	slices.Sort(uploaded)
	if queries == 0 || len(uploaded) != 2 {
		t.Fatalf("one blob each for alpha and beta, none for the copy, the empty file or the binary; got %d after %d queries", len(uploaded), queries)
	}
	h.server.with(func(s *fakeServer) {
		files := s.snapshots[0].Tree.files()
		if len(files) != 5 || files["a.txt"].Content != "alpha\n" || files["copy.txt"].Content != "alpha\n" || files["empty.txt"].Content != "" {
			t.Errorf("the snapshot should resolve to the uploaded content: %+v", files)
		}
	})
}

func TestBroadcastResumesAnInterruptedUpload(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n", "c.txt": "gamma\n"})
	sum := sha256.Sum256([]byte("alpha\n"))
	h.server.with(func(s *fakeServer) { s.blobs = map[string]string{hex.EncodeToString(sum[:]): "alpha\n"} })
	h.start()

	batches, _ := h.server.uploadedBlobs()
	var count int
	for _, b := range batches {
		count += len(b)
	}
	if count != 2 {
		t.Errorf("content the server already has is not sent again: %d blobs uploaded", count)
	}
}

func TestBroadcastBatchesBlobsUnderTheServerLimit(t *testing.T) {
	files := map[string]string{}
	for i := range 12 {
		files[fmt.Sprintf("f%02d.txt", i)] = strings.Repeat(string(rune('a'+i)), 700) + "\n"
	}
	h := newHarness(t, files)
	h.server.with(func(s *fakeServer) { s.limits.MaxBlobBatchBytes = 2400 })
	h.start()

	batches, _ := h.server.uploadedBlobs()
	if len(batches) < 4 {
		t.Fatalf("12 blobs of 700 bytes under a 2400 byte limit need several batches, got %d", len(batches))
	}
	total := 0
	for _, b := range batches {
		if len(b) > 2 {
			t.Errorf("a batch of %d blobs of 700 bytes does not fit in 1800 bytes", len(b))
		}
		total += len(b)
	}
	if total != 12 {
		t.Errorf("uploaded %d blobs", total)
	}
}

func TestBroadcastRefusesMoreFilesThanTheServerKeeps(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
	h.server.with(func(s *fakeServer) { s.limits.MaxSnapshotFiles = 2 })
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "3 files") || !strings.Contains(err.Error(), "at most 2") || !strings.Contains(err.Error(), "--only") {
		t.Fatalf("err = %v", err)
	}
	if batches, _ := h.server.uploadedBlobs(); len(batches) != 0 {
		t.Error("nothing should be uploaded for a project the server will refuse")
	}
	h.server.with(func(s *fakeServer) {
		if !s.ended {
			t.Error("the session was left running")
		}
	})
}

func TestBroadcastScansAgainWhenFilesChangeDuringTheUpload(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	h.server.with(func(s *fakeServer) { s.rejectSnapshots = 1 })
	h.start()
	h.server.with(func(s *fakeServer) {
		if s.snapshotTries != 2 || len(s.snapshots) != 1 {
			t.Errorf("snapshot tries = %d, accepted = %d", s.snapshotTries, len(s.snapshots))
		}
	})
}

func TestBroadcastGivesUpScanningAfterAFewTries(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	h.server.with(func(s *fakeServer) { s.rejectSnapshots = 10 })
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context()); !api.IsMissingBlob(err) {
		t.Fatalf("err = %v", err)
	}
	h.server.with(func(s *fakeServer) {
		if s.snapshotTries != maxScans {
			t.Errorf("snapshot tries = %d, want %d", s.snapshotTries, maxScans)
		}
	})
}

func TestReadBlobFollowsAFileThatChanged(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "old\n", "gone.txt": "x\n"})
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if b.root, err = os.OpenRoot(h.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.root.Close() })

	node := &api.FileNode{Type: api.NodeFile, Path: "a.txt", Hash: strings.Repeat("0", 64), Size: 4}
	write(t, h.root, "a.txt", "newer content\n")
	blob, ok, err := b.readBlob(node)
	if err != nil || !ok || blob.Content != "newer content\n" || node.Hash != blob.Hash || node.Size != int64(len("newer content\n")) {
		t.Fatalf("blob = %+v, ok = %v, err = %v, node = %+v", blob, ok, err, node)
	}

	write(t, h.root, "a.txt", "key = AKIA"+"IOSFODNN7EXAMPLE\n")
	blob, ok, err = b.readBlob(node)
	if err != nil || !ok || blob.Hash != "" || !node.Binary {
		t.Fatalf("a file that now looks like a secret becomes a placeholder: blob = %+v, ok = %v, err = %v, node = %+v", blob, ok, err, node)
	}

	if err := os.Remove(filepath.Join(h.root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := b.readBlob(&api.FileNode{Type: api.NodeFile, Path: "gone.txt"}); ok || err != nil {
		t.Errorf("a file that is gone: ok = %v, err = %v", ok, err)
	}
}

func TestFillReadsContentLazilyAndHandlesMissingFiles(t *testing.T) {
	h := newHarness(t, map[string]string{"changed.txt": "fresh\n"})
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if b.root, err = os.OpenRoot(h.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.root.Close() })

	stale := strings.Repeat("0", 64)
	entries := []api.CheckpointEntry{
		{Path: "changed.txt", ChangeType: api.ChangeModified, Hash: &stale, Size: 6},
		{Path: "vanished.txt", ChangeType: api.ChangeAdded, Hash: &stale, Size: 3},
		{Path: "gone.txt", ChangeType: api.ChangeDeleted},
		{Path: "image.png", ChangeType: api.ChangeAdded, Hash: &stale, Size: 9, Binary: true},
		{Path: "empty.txt", ChangeType: api.ChangeAdded, Hash: &stale},
	}
	if err := b.fill(entries); err != nil {
		t.Fatal(err)
	}
	if entries[0].Content != "fresh\n" || *entries[0].Hash == stale || entries[0].ChangeType != api.ChangeModified {
		t.Errorf("changed.txt = %+v", entries[0])
	}
	if entries[1].ChangeType != api.ChangeDeleted || entries[1].Hash != nil {
		t.Errorf("a file that vanished is sent as a deletion: %+v", entries[1])
	}
	if entries[2].Content != "" || entries[3].Content != "" || entries[4].Content != "" {
		t.Error("deletions, binaries and empty files are left as they are")
	}
}

func TestChunksCountContentThatIsStillToBeRead(t *testing.T) {
	hash := strings.Repeat("a", 64)
	unfilled := api.CheckpointEntry{Path: "a.txt", ChangeType: api.ChangeAdded, Hash: &hash, Size: 700_000}
	filled := unfilled
	filled.Content = strings.Repeat("x", 700_000)
	if entrySize(unfilled) < 700_000 || entrySize(filled) < entrySize(unfilled) || entrySize(filled)-entrySize(unfilled) > 16 {
		t.Errorf("sizes: unfilled %d, filled %d", entrySize(unfilled), entrySize(filled))
	}
	if got := len(chunk([]api.CheckpointEntry{unfilled, unfilled, unfilled}, 1_000_000)); got != 3 {
		t.Errorf("three 700 KB files do not share a 1 MB request, got %d chunks", got)
	}
}

func TestBroadcastKeepsItsTokenWhenTheUploadFails(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	h.server.with(func(s *fakeServer) { s.rejectSnapshots = 10 })
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context()); err == nil {
		t.Fatal("expected the upload to fail")
	}
	rec, ok, err := h.store.Lookup(h.root)
	if err != nil || !ok || rec.SessionID != b.SessionID() || !strings.HasPrefix(rec.Token, "bgs_") || rec.PID != 0 {
		t.Fatalf("the owner token is saved so the session can be resumed: %+v, %v, %v", rec, ok, err)
	}
}

func TestBroadcastResumesASessionWhoseFirstUploadNeverLanded(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"})
	h.server.with(func(s *fakeServer) { s.rejectSnapshots = 10 })
	failed, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Start(t.Context()); err == nil {
		t.Fatal("expected the first upload to fail")
	}
	batches, _ := h.server.uploadedBlobs()
	h.server.with(func(s *fakeServer) { s.rejectSnapshots = 0 })

	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Rejoined() || b.SessionID() != failed.SessionID() {
		t.Fatalf("the second run should rejoin the first run's session")
	}
	res, err := b.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Stop(t.Context()) })
	if res.Sync != SyncedSnapshot || res.Files != 2 {
		t.Errorf("an empty server manifest means a full snapshot, got %+v", res)
	}
	after, _ := h.server.uploadedBlobs()
	if len(after) != len(batches) {
		t.Errorf("content the server already had was sent again: %d batches, then %d", len(batches), len(after))
	}
	h.server.with(func(s *fakeServer) {
		if len(s.snapshots) != 1 || len(s.snapshots[0].Tree.files()) != 2 {
			t.Errorf("snapshots = %+v", s.snapshots)
		}
	})
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

func keyFromLink(t *testing.T, link string) *e2ee.Cipher {
	t.Helper()
	_, text, ok := strings.Cut(link, "#k=")
	if !ok {
		t.Fatalf("the share link carries no key: %s", link)
	}
	key, err := e2ee.ParseKey(text)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e2ee.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEncryptedBroadcastSendsOnlySealedContent(t *testing.T) {
	h := newHarness(t, map[string]string{
		"main.go":   "package main\n\nfunc main() {}\n",
		"notes.txt": "the launch code is 0000\n",
		"empty.txt": "",
	})
	h.opts.Encrypt = true
	b := h.start()

	link := b.ShareURL()
	c := keyFromLink(t, link)
	if !b.Encrypted() || !strings.HasPrefix(link, "http") || strings.Count(link, "#") != 1 {
		t.Fatalf("link = %s", link)
	}
	h.server.with(func(s *fakeServer) {
		if !s.encrypted[b.SessionID()] {
			t.Error("the session was not created encrypted")
		}
		files := s.snapshots[0].Tree.files()
		for path, plain := range map[string]string{"main.go": "package main\n\nfunc main() {}\n", "notes.txt": "the launch code is 0000\n", "empty.txt": ""} {
			n := files[path]
			if strings.Contains(n.Content, "launch") || strings.Contains(n.Content, "package") {
				t.Errorf("%s reached the server readable", path)
			}
			opened, err := c.Open(n.Hash, []byte(n.Content))
			if err != nil || string(opened) != plain {
				t.Errorf("%s opens to %q, %v", path, opened, err)
			}
			if n.Size != int64(len(plain)) {
				t.Errorf("%s size = %d, want the plaintext size %d", path, n.Size, len(plain))
			}
		}
		sum := sha256.Sum256([]byte("the launch code is 0000\n"))
		if files["notes.txt"].Hash == hex.EncodeToString(sum[:]) {
			t.Error("a file is named by its plain SHA-256, which a server could check a guess against")
		}
	})
	rec, ok, err := h.store.Lookup(h.root)
	if err != nil || !ok || rec.Key == "" || !strings.HasSuffix(rec.ShareURL(), "#k="+rec.Key) {
		t.Errorf("the key is saved with the session: %+v, %v, %v", rec, ok, err)
	}
}

func TestEncryptedBroadcastReportsLineCountsItself(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "one\ntwo\nthree\n", "gone.txt": "x\ny\n"})
	h.opts.Encrypt = true
	b := h.start()
	c := keyFromLink(t, b.ShareURL())

	write(t, h.root, "a.txt", "one\nTWO\nthree\nfour\n")
	write(t, h.root, "new.txt", "a\nb\nc\n")
	if err := os.Remove(filepath.Join(h.root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the edits", func() bool {
		ch := h.server.changes()
		return ch["a.txt"] == api.ChangeModified && ch["new.txt"] == api.ChangeAdded && ch["gone.txt"] == api.ChangeDeleted
	})

	if e, _ := h.server.entry("a.txt"); e.Additions != 2 || e.Deletions != 1 {
		t.Errorf("a.txt counts +%d -%d, want +2 -1", e.Additions, e.Deletions)
	}
	if e, _ := h.server.entry("new.txt"); e.Additions != 3 {
		t.Errorf("new.txt additions = %d", e.Additions)
	}
	if e, _ := h.server.entry("gone.txt"); e.Deletions != 2 {
		t.Errorf("gone.txt deletions = %d", e.Deletions)
	}
	e, _ := h.server.entry("a.txt")
	if strings.Contains(e.Content, "four") {
		t.Error("an edit reached the server readable")
	}
	if opened, err := c.Open(*e.Hash, []byte(e.Content)); err != nil || string(opened) != "one\nTWO\nthree\nfour\n" {
		t.Errorf("the edit opens to %q, %v", opened, err)
	}
}

func TestEncryptedSessionKeepsItsKeyWhenRejoined(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	h.opts.Encrypt = true
	first := h.start()
	link := first.ShareURL()
	if err := first.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	h.opts.Encrypt = false
	again, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Encrypted() || !again.Rejoined() || again.ShareURL() != link {
		t.Errorf("a saved encrypted session stays encrypted with the same link: %v %s", again.Encrypted(), again.ShareURL())
	}
	if _, err := again.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Stop(t.Context()) })
}

func TestEncryptingAnUnencryptedSessionIsRefused(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	first := h.start()
	if err := first.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.opts.Encrypt = true
	_, err := New(h.opts)
	if err == nil || !strings.Contains(err.Error(), "cannot become encrypted") {
		t.Fatalf("err = %v", err)
	}
}

func TestAServerThatIgnoresEncryptionIsNotTrusted(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	h.server.with(func(s *fakeServer) { s.ignoreEncrypt = true })
	h.opts.Encrypt = true
	b, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("err = %v", err)
	}
	batches, _ := h.server.uploadedBlobs()
	if len(batches) != 0 {
		t.Error("nothing should be uploaded to a session that cannot hold it encrypted")
	}
}

func TestAnUnencryptedBroadcastIsUnchanged(t *testing.T) {
	h := newHarness(t, map[string]string{"a.txt": "alpha\n"})
	b := h.start()
	if b.Encrypted() || strings.Contains(b.ShareURL(), "#") {
		t.Errorf("link = %s", b.ShareURL())
	}
	h.server.with(func(s *fakeServer) {
		if n := s.snapshots[0].Tree.files()["a.txt"]; n.Content != "alpha\n" {
			t.Errorf("content = %q", n.Content)
		}
	})
}

func TestPlainCacheKeepsRecentFilesWithinItsBudget(t *testing.T) {
	c := newPlainCache(100)
	c.put("a", strings.Repeat("a", 20))
	c.put("b", strings.Repeat("b", 20))
	c.put("huge", strings.Repeat("h", 60))
	if _, ok := c.get("huge"); ok {
		t.Error("a file over a quarter of the budget should not be kept")
	}
	for _, p := range []string{"c", "d", "e", "f", "g"} {
		c.put(p, strings.Repeat(p, 24))
	}
	if _, ok := c.get("a"); ok {
		t.Error("the oldest file should have been dropped")
	}
	if text, ok := c.get("g"); !ok || len(text) != 24 {
		t.Errorf("the newest file should be kept: %q, %v", text, ok)
	}
	c.put("g", "new")
	c.drop("g")
	if _, ok := c.get("g"); ok {
		t.Error("dropped")
	}
	if c.size < 0 || c.size > 100 {
		t.Errorf("size = %d", c.size)
	}
}
