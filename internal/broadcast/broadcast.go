// Package broadcast runs one project's live broadcast: the initial upload,
// file watching, checkpoint uploads, heartbeats, and a clean shutdown.
package broadcast

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/config"
	"github.com/biga-app/cli/internal/human"
	"github.com/biga-app/cli/internal/ignore"
	"github.com/biga-app/cli/internal/session"
	"github.com/biga-app/cli/internal/snapshot"
	"github.com/biga-app/cli/internal/watch"
)

const (
	// stopPollInterval is how often a broadcast checks for `biga stop`.
	stopPollInterval = 2 * time.Second
	// maxTitleLen is the longest title the server accepts.
	maxTitleLen = 120
	// offlineAfter is how many heartbeats in a row must fail before the
	// server counts as unreachable; a single dropped one is just noise.
	offlineAfter = 2
)

// Events receives what a broadcast is doing. Methods are called from
// several goroutines and must not block for long.
type Events interface {
	Info(msg string)
	Warn(msg string)
	Error(msg string)
	// Progress describes the step Start is on, e.g. "Uploading 2.1 / 8.4 MB".
	Progress(msg string)
	// Synced reports a checkpoint the server accepted.
	Synced(Checkpoint)
	// Online reports whether the server is reachable, each time it changes.
	Online(bool)
}

// Checkpoint is one batch of changes the server accepted.
type Checkpoint struct {
	// Seq counts this broadcast's checkpoints, starting at 1.
	Seq     int64
	Changes []Change
	// Bytes is the size of the upload as sent, after compression.
	Bytes int
}

// Change is one file in a checkpoint.
type Change struct {
	Path string
	Type api.ChangeType
}

// Options configures a [Broadcast].
type Options struct {
	Client *api.Client
	Store  *session.Store
	Events Events
	// APIURL is the server base URL; it is recorded with the session.
	APIURL string
	// RootDir is the absolute directory to broadcast.
	RootDir string
	// Title overrides the session title. Empty keeps the title saved for
	// RootDir, or falls back to the directory name.
	Title string
	// SessionID forces a session ID. Empty reuses the ID saved for RootDir,
	// which keeps its share URL stable, or generates a new one.
	SessionID string
	// GroupID links the session to the rest of its workspace.
	GroupID string
	// ExcludesFile is the user's global gitignore, if any.
	ExcludesFile string
	Timing       config.Timing
	Heartbeat    time.Duration
}

// Sync describes how [Broadcast.Start] brought the server up to date.
type Sync int

const (
	// SyncedSnapshot means the whole tree was uploaded.
	SyncedSnapshot Sync = iota
	// SyncedChanges means a rejoin uploaded only what differed.
	SyncedChanges
	// SyncedNothing means a rejoin found the server already up to date.
	SyncedNothing
)

// Result reports what [Broadcast.Start] uploaded.
type Result struct {
	Sync Sync
	// Files is the number of files uploaded: the whole tree for a snapshot,
	// or the changed files on a rejoin.
	Files int
	// Bytes is the size of the upload as sent, after compression.
	Bytes int
}

// Broadcast is one project's broadcast pipeline. Several can run in one
// process, each with its own session.
type Broadcast struct {
	opts      Options
	events    Events
	input     api.CreateSessionInput
	sessionID string
	startedAt time.Time
	rejoined  bool

	// cred guards the session's owner token and server-reported share link,
	// which a reactivation from the heartbeat loop can replace while uploads
	// read them.
	cred      sync.Mutex
	ownerTok  string
	shareLink string

	filter  *ignore.Filter
	root    *os.Root
	watcher *watch.Watcher
	batcher *watch.Batcher

	// hashes maps each synced file to its content hash. After Start it is
	// only touched by flush, and flushes never overlap.
	hashes      map[string]string
	rescan      atomic.Bool
	checkpoints atomic.Int64
	online      atomic.Bool
	// lastContact is when the server last accepted a request, in Unix
	// nanoseconds; a heartbeat is redundant right after a checkpoint.
	lastContact atomic.Int64

	stopLoops   context.CancelFunc
	cancelFlush context.CancelFunc
	loops       sync.WaitGroup

	stopRequested chan struct{}
	requestOnce   sync.Once
	stopOnce      sync.Once
	stopErr       error
}

// New prepares a broadcast of opts.RootDir, resolving its session from the
// local registry. It fails if another process is already broadcasting the
// directory.
func New(opts Options) (*Broadcast, error) {
	saved, found, err := opts.Store.Lookup(opts.RootDir)
	if err != nil {
		return nil, err
	}
	if found && saved.PID != os.Getpid() && saved.Live(time.Now()) {
		return nil, fmt.Errorf("%s is already being broadcast by process %d; run `biga stop` first", opts.RootDir, saved.PID)
	}

	b := &Broadcast{
		opts:          opts,
		events:        opts.Events,
		sessionID:     opts.SessionID,
		startedAt:     time.Now(),
		stopRequested: make(chan struct{}),
	}
	switch {
	case b.sessionID == "" && found:
		b.sessionID, b.rejoined = saved.SessionID, true
	case b.sessionID == "":
		b.sessionID = session.NewID()
	}
	if found && saved.SessionID == b.sessionID {
		b.ownerTok, b.shareLink = saved.Token, saved.ShareLink
		if !saved.StartedAt.IsZero() {
			b.startedAt = saved.StartedAt
		}
	}

	var savedTitle string
	if found {
		savedTitle = saved.Title
	}
	title := cmp.Or(strings.TrimSpace(opts.Title), savedTitle, filepath.Base(opts.RootDir))
	b.input = api.CreateSessionInput{Title: truncate(title, maxTitleLen), GroupID: opts.GroupID}
	return b, nil
}

// SessionID is the broadcast's session ID.
func (b *Broadcast) SessionID() string { return b.sessionID }

// Title is the session title.
func (b *Broadcast) Title() string { return b.input.Title }

// RootDir is the broadcast directory.
func (b *Broadcast) RootDir() string { return b.opts.RootDir }

// ShareURL is the link viewers open.
func (b *Broadcast) ShareURL() string {
	b.cred.Lock()
	defer b.cred.Unlock()
	if b.shareLink != "" {
		return b.shareLink
	}
	return session.ShareURL(b.opts.APIURL, b.sessionID)
}

// token is the session's owner token.
func (b *Broadcast) token() string {
	b.cred.Lock()
	defer b.cred.Unlock()
	return b.ownerTok
}

// adopt records what the server said about the session: a token when it
// just created it, and the share link when it knows its public address.
func (b *Broadcast) adopt(s *api.Session) (newToken bool) {
	b.cred.Lock()
	defer b.cred.Unlock()
	if s.Token != "" && s.Token != b.ownerTok {
		b.ownerTok, newToken = s.Token, true
	}
	if s.ShareURL != "" {
		b.shareLink = s.ShareURL
	}
	return newToken
}

// ensureSession creates or reactivates the session. A rejoin the server
// refuses because this machine cannot prove it owns the session (the token
// was lost, or the session predates owner tokens) moves the broadcast to a
// new session and link rather than failing.
func (b *Broadcast) ensureSession(ctx context.Context) error {
	s, err := b.opts.Client.EnsureSession(ctx, b.sessionID, b.token(), b.input)
	if api.IsOwnershipError(err) && b.rejoined && b.opts.SessionID == "" {
		old := b.sessionID
		b.sessionID, b.rejoined, b.startedAt = session.NewID(), false, time.Now()
		b.cred.Lock()
		b.ownerTok, b.shareLink = "", ""
		b.cred.Unlock()
		b.events.Warn(fmt.Sprintf("The server no longer accepts changes to session %s from this machine, so this broadcast gets a new link.", old))
		s, err = b.opts.Client.EnsureSession(ctx, b.sessionID, "", b.input)
	}
	if api.IsOwnershipError(err) {
		return fmt.Errorf("session %s belongs to another broadcast; choose a different --session", b.sessionID)
	}
	if api.IsAccessKeyError(err) {
		return fmt.Errorf("%w (set %s to a key from the server's operator)", err, config.EnvAccessKey)
	}
	if err != nil {
		return err
	}
	b.adopt(s)
	return nil
}

// Rejoined reports whether the broadcast resumes the directory's previous
// session rather than starting a new one.
func (b *Broadcast) Rejoined() bool { return b.rejoined }

// StopRequested is closed when `biga stop` asks this broadcast to end. The
// owner should then call [Broadcast.Stop].
func (b *Broadcast) StopRequested() <-chan struct{} { return b.stopRequested }

// Start uploads the directory and begins streaming changes.
func (b *Broadcast) Start(ctx context.Context) (res Result, err error) {
	b.events.Progress("Connecting…")
	if err := b.ensureSession(ctx); err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			b.abort()
		}
	}()

	if b.filter, err = ignore.NewFilter(b.opts.RootDir, b.opts.ExcludesFile); err != nil {
		return Result{}, err
	}
	if b.root, err = os.OpenRoot(b.opts.RootDir); err != nil {
		return Result{}, err
	}

	// Watch before reading the tree, so edits made during the initial upload
	// are queued rather than lost. The batcher holds them until it starts.
	b.batcher = watch.NewBatcher(b.opts.Timing.Idle, b.opts.Timing.MaxWait, b.flush)
	b.watcher, err = watch.New(watch.Options{
		Root:      b.opts.RootDir,
		Ignore:    func(rel string, isDir bool) bool { return !b.filter.Accepts(rel, isDir) },
		Stability: b.opts.Timing.Stability,
		OnChange:  func(rel string) { b.batcher.Add(rel) },
		OnError:   b.watchError,
	})
	if err != nil {
		return Result{}, fmt.Errorf("watch %s: %w", b.opts.RootDir, err)
	}

	b.events.Progress("Scanning files…")
	tree, err := snapshot.Build(ctx, b.root, b.filter, func(rel string, err error) {
		b.events.Warn(fmt.Sprintf("Skipped %s: %v", rel, err))
	})
	if err != nil {
		return Result{}, err
	}
	if res, err = b.upload(ctx, tree); err != nil {
		return Result{}, err
	}
	b.hashes = snapshot.Hashes(tree)
	if err := b.persist(true); err != nil {
		return Result{}, fmt.Errorf("save session: %w", err)
	}
	b.online.Store(true)
	b.touchContact()

	// Background work outlives ctx, which is cancelled on Ctrl+C: shutdown
	// still needs to flush the final edits and end the session.
	flushCtx, cancelFlush := context.WithCancel(context.WithoutCancel(ctx))
	loopCtx, stopLoops := context.WithCancel(flushCtx)
	b.cancelFlush, b.stopLoops = cancelFlush, stopLoops
	b.batcher.Start(flushCtx)
	b.loops.Go(func() { b.heartbeat(loopCtx) })
	b.loops.Go(func() { b.pollStop(loopCtx) })
	return res, nil
}

// upload brings the server in line with tree: a full snapshot for a new
// session, or only the differences when rejoining one.
func (b *Broadcast) upload(ctx context.Context, tree *api.FileNode) (Result, error) {
	if !b.rejoined {
		return b.uploadSnapshot(ctx, tree)
	}

	b.events.Progress("Comparing with the server…")
	manifest, err := b.opts.Client.Manifest(ctx, b.sessionID, b.token())
	if errors.Is(err, api.ErrManifestUnavailable) {
		return b.uploadSnapshot(ctx, tree)
	}
	if err != nil {
		return Result{}, err
	}
	entries := snapshot.Diff(manifest, tree)
	if len(entries) == 0 {
		return Result{Sync: SyncedNothing}, nil
	}
	b.events.Progress(fmt.Sprintf("Uploading %s…", human.Count(len(entries), "changed file", "changed files")))
	sent, err := b.sendAll(ctx, entries)
	if err != nil {
		return Result{}, err
	}
	return Result{Sync: SyncedChanges, Files: len(entries), Bytes: sent}, nil
}

func (b *Broadcast) uploadSnapshot(ctx context.Context, tree *api.FileNode) (Result, error) {
	snap := &api.FileSnapshot{SessionID: b.sessionID, Tree: tree, Timestamp: time.Now().UnixMilli()}
	files := len(snapshot.Hashes(tree))
	var last time.Time
	sent, err := b.opts.Client.UploadSnapshot(ctx, b.token(), snap, func(done, total int) {
		// Throttled: the body is read in small chunks.
		if now := time.Now(); now.Sub(last) > 100*time.Millisecond || done == total {
			last = now
			b.events.Progress(fmt.Sprintf("Uploading %s · %s / %s", human.Count(files, "file", "files"), human.Bytes(int64(done)), human.Bytes(int64(total))))
		}
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Sync: SyncedSnapshot, Files: files, Bytes: sent}, nil
}

// abort undoes a failed Start. The session already exists on the server, so
// it is ended rather than left looking live with nobody broadcasting.
func (b *Broadcast) abort() {
	if b.watcher != nil {
		b.watcher.Close()
	}
	if b.batcher != nil {
		b.batcher.Close()
	}
	if b.root != nil {
		_ = b.root.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = b.opts.Client.EndSession(ctx, b.sessionID, b.token())
}

// Stop ends the broadcast: it flushes edits saved just before shutdown, ends
// the session, and releases the watcher. It is safe to call more than once;
// if ctx expires, in-flight uploads are abandoned.
func (b *Broadcast) Stop(ctx context.Context) error {
	b.stopOnce.Do(func() { b.stopErr = b.shutdown(ctx) })
	return b.stopErr
}

func (b *Broadcast) shutdown(ctx context.Context) error {
	if b.stopLoops == nil {
		return nil // never started
	}
	defer context.AfterFunc(ctx, b.cancelFlush)()

	// Stop the heartbeat first so it cannot mark the record live again after
	// it is cleared below.
	b.stopLoops()
	b.loops.Wait()
	b.watcher.Close()
	b.batcher.Close()
	_ = b.root.Close()
	b.cancelFlush()

	if err := b.persist(false); err != nil {
		b.events.Warn(fmt.Sprintf("Could not update the local session record: %v", err))
	}
	err := b.opts.Client.EndSession(ctx, b.sessionID, b.token())
	if api.HasStatus(err, http.StatusNotFound, http.StatusConflict) {
		return nil // already ended
	}
	return err
}

// Checkpoints is the number of checkpoints the server has accepted.
func (b *Broadcast) Checkpoints() int64 { return b.checkpoints.Load() }

// persist writes the session's local record. A live record carries this
// process's PID, which `biga status` and `biga stop` use to find it.
func (b *Broadcast) persist(live bool) error {
	b.cred.Lock()
	token, link := b.ownerTok, b.shareLink
	b.cred.Unlock()
	rec := session.Record{
		SessionID:         b.sessionID,
		Title:             b.input.Title,
		RootDir:           b.opts.RootDir,
		StartedAt:         b.startedAt,
		APIURL:            b.opts.APIURL,
		CheckpointsSynced: b.checkpoints.Load(),
		LastSeenAt:        time.Now(),
		Token:             token,
		ShareLink:         link,
	}
	if live {
		rec.PID = os.Getpid()
	}
	return b.opts.Store.Save(rec)
}

// heartbeat keeps the session live on the server and in the local registry.
func (b *Broadcast) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(b.opts.Heartbeat)
	defer ticker.Stop()

	var failures int
	var warnedLocal bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// The local record tracks this process, which is alive whether or not
		// the server is reachable.
		if err := b.persist(true); err != nil && !warnedLocal {
			warnedLocal = true
			b.events.Warn(fmt.Sprintf("Could not update the local session record: %v", err))
		}

		// A checkpoint the server accepted recently already counts as one.
		if time.Since(time.Unix(0, b.lastContact.Load())) < b.opts.Heartbeat {
			continue
		}
		err := b.opts.Client.Heartbeat(ctx, b.sessionID, b.token())
		if api.HasStatus(err, http.StatusNotFound, http.StatusConflict) {
			// The session ended or expired while we were away, e.g. while
			// the machine slept. Reactivating keeps the same share URL.
			if err = b.reactivate(ctx); err == nil {
				b.events.Info(fmt.Sprintf("Session %s was reactivated.", b.sessionID))
			}
		}
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			failures = 0
			b.touchContact()
			b.setOnline(true)
		default:
			if failures++; failures >= offlineAfter {
				b.setOnline(false)
			}
		}
	}
}

// reactivate brings back a session that ended or expired while this
// broadcast was away. If the retention sweep deleted it, the server creates
// it afresh under the same ID with a new token, which is saved at once.
func (b *Broadcast) reactivate(ctx context.Context) error {
	s, err := b.opts.Client.EnsureSession(ctx, b.sessionID, b.token(), b.input)
	if err != nil {
		return err
	}
	if b.adopt(s) {
		if err := b.persist(true); err != nil {
			b.events.Warn(fmt.Sprintf("Could not save the new session token: %v", err))
		}
	}
	return nil
}

func (b *Broadcast) touchContact() { b.lastContact.Store(time.Now().UnixNano()) }

// setOnline records server reachability and reports changes.
func (b *Broadcast) setOnline(online bool) {
	if b.online.Swap(online) != online {
		b.events.Online(online)
	}
}

// pollStop watches for a `biga stop` request aimed at this session.
func (b *Broadcast) pollStop(ctx context.Context) {
	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if ok, _ := b.opts.Store.ConsumeStop(b.sessionID); ok {
			b.requestOnce.Do(func() { close(b.stopRequested) })
			return
		}
	}
}

func (b *Broadcast) watchError(err error) {
	if errors.Is(err, watch.ErrOverflow) {
		// Events were dropped. The watcher re-reports every existing file;
		// re-checking every synced one as well catches missed deletions.
		b.events.Warn("Too many file changes at once; rescanning the project.")
		b.rescan.Store(true)
		return
	}
	b.events.Error(fmt.Sprintf("Watcher error: %v", err))
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
