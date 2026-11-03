package broadcast

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/biga-sh/cli/internal/api"
	"github.com/biga-sh/cli/internal/ignore"
	"github.com/biga-sh/cli/internal/snapshot"
)

// maxCheckpointBytes keeps each upload under the server's default 8 MB
// request limit, which applies to the uncompressed body. Larger change sets
// are split across several checkpoints.
const maxCheckpointBytes = 6 << 20

// fileState is what a changed path looks like on disk now.
type fileState struct {
	rel string
	// exists is false when no shared regular file is at rel any more.
	exists bool
	file   snapshot.File
	err    error
}

// prior is a path's sync state before a flush, for rolling it back.
type prior struct {
	hash    string
	tracked bool
}

// flush syncs one batch of changed paths. The watcher only says a path may
// have changed; comparing content hashes here decides what actually did.
func (b *Broadcast) flush(ctx context.Context, paths []string) {
	// A changed ignore file changes what is shared anywhere beneath it:
	// re-check every synced file, and have the watcher report every file
	// that is now included.
	resync := false
	for _, p := range paths {
		if ignore.IsPatternFile(path.Base(p)) {
			b.filter.Invalidate(path.Dir(p))
			resync = true
		}
	}
	if resync {
		go b.watcher.Rescan()
	}
	if b.rescan.Swap(false) || resync {
		paths = append(paths, slices.Collect(maps.Keys(b.hashes))...)
	}
	paths = dedupe(paths)

	states := b.readAll(paths)
	// A missing path we never synced as a file may be a removed directory:
	// everything synced beneath it is gone too.
	if extra := b.trackedUnder(states, paths); len(extra) > 0 {
		states = append(states, b.readAll(extra)...)
	}

	entries, priors := b.apply(states)
	if len(entries) == 0 {
		return
	}

	chunks := chunk(entries, maxCheckpointBytes)
	for i, c := range chunks {
		sent, err := b.push(ctx, c)
		switch {
		case err == nil:
			b.touchContact()
			b.setOnline(true)
			changes := make([]Change, len(c))
			for j, e := range c {
				changes[j] = Change{Path: e.Path, Type: e.ChangeType}
			}
			b.events.Synced(Checkpoint{Seq: b.checkpoints.Add(1), Changes: changes, Bytes: sent})
		case api.IsPermanent(err):
			// Retrying cannot fix a rejected payload. Forget it was synced so a
			// later save of these files tries again, and carry on.
			b.events.Error(fmt.Sprintf("Checkpoint rejected by server: %v", err))
			b.rollback(c, priors)
		default:
			// The server is unreachable. Requeue everything not yet sent, so
			// the edits sync once it is back.
			rest := slices.Concat(chunks[i:]...)
			b.rollback(rest, priors)
			retry := make([]string, len(rest))
			for j, e := range rest {
				retry[j] = e.Path
			}
			b.batcher.Add(retry...)
			b.setOnline(false)
			return
		}
	}
}

// rollback restores the sync state entries had before this flush.
func (b *Broadcast) rollback(entries []api.CheckpointEntry, priors map[string]prior) {
	for _, e := range entries {
		if p := priors[e.Path]; p.tracked {
			b.hashes[e.Path] = p.hash
		} else {
			delete(b.hashes, e.Path)
		}
	}
}

// readAll reads the current state of each path, a bounded number at a time.
func (b *Broadcast) readAll(paths []string) []fileState {
	states := make([]fileState, len(paths))
	var g errgroup.Group
	g.SetLimit(snapshot.ReadConcurrency)
	for i, rel := range paths {
		g.Go(func() error {
			states[i] = b.read(rel)
			return nil
		})
	}
	_ = g.Wait() // read reports failures in fileState
	return states
}

func (b *Broadcast) read(rel string) fileState {
	// A file the ignore rules now exclude is gone as far as viewers go.
	if !b.filter.Accepts(rel, false) {
		return fileState{rel: rel}
	}
	f, err := snapshot.ReadFile(b.root, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{rel: rel}
	}
	if err != nil {
		return fileState{rel: rel, err: err}
	}
	return fileState{rel: rel, exists: true, file: f}
}

// trackedUnder returns the synced files beneath any missing path that was
// not itself a synced file, excluding paths already in the batch.
func (b *Broadcast) trackedUnder(states []fileState, batch []string) []string {
	var dirs []string
	for _, st := range states {
		if _, tracked := b.hashes[st.rel]; !st.exists && st.err == nil && !tracked {
			dirs = append(dirs, st.rel+"/")
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	inBatch := make(map[string]bool, len(batch))
	for _, p := range batch {
		inBatch[p] = true
	}
	var extra []string
	for rel := range b.hashes {
		if inBatch[rel] {
			continue
		}
		for _, dir := range dirs {
			if strings.HasPrefix(rel, dir) {
				extra = append(extra, rel)
				break
			}
		}
	}
	return extra
}

// apply compares each state with what was last synced, records the new
// hashes, and returns the resulting entries along with the previous state of
// every path it changed.
func (b *Broadcast) apply(states []fileState) ([]api.CheckpointEntry, map[string]prior) {
	var entries []api.CheckpointEntry
	priors := make(map[string]prior)
	for _, st := range states {
		old, tracked := b.hashes[st.rel]
		switch {
		case st.err != nil:
			b.events.Warn(fmt.Sprintf("Failed to read %s: %v", st.rel, st.err))
			continue
		case !st.exists:
			if !tracked {
				continue
			}
			delete(b.hashes, st.rel)
			entries = append(entries, snapshot.Deletion(st.rel))
		case tracked && old == st.file.Hash:
			continue
		default:
			change := api.ChangeAdded
			if tracked {
				change = api.ChangeModified
			}
			if st.file.Oversize {
				b.events.Info(fmt.Sprintf("%s is larger than %d MB; viewers see a placeholder.", st.rel, snapshot.MaxFileBytes>>20))
			}
			b.hashes[st.rel] = st.file.Hash
			entries = append(entries, st.file.Entry(st.rel, change))
		}
		priors[st.rel] = prior{hash: old, tracked: tracked}
	}
	slices.SortFunc(entries, func(a, b api.CheckpointEntry) int { return cmp.Compare(a.Path, b.Path) })
	return entries, priors
}

// sendAll uploads entries as one or more checkpoints, stopping at the first
// failure, and returns the bytes sent. It is used while starting, so these
// checkpoints are not counted among the live ones.
func (b *Broadcast) sendAll(ctx context.Context, entries []api.CheckpointEntry) (int, error) {
	total := 0
	for _, c := range chunk(entries, maxCheckpointBytes) {
		sent, err := b.push(ctx, c)
		if err != nil {
			return total, err
		}
		total += sent
	}
	return total, nil
}

func (b *Broadcast) push(ctx context.Context, entries []api.CheckpointEntry) (int, error) {
	upload := &api.CheckpointUpload{SessionID: b.sessionID, Entries: entries, Timestamp: time.Now().UnixMilli()}
	sent, err := b.opts.Client.PushCheckpoint(ctx, b.token(), upload)
	if !api.HasStatus(err, http.StatusNotFound, http.StatusConflict) {
		return sent, err
	}
	// The session ended while we were away; reactivate it and retry once
	// rather than dropping the edits.
	if reactivateErr := b.reactivate(ctx); reactivateErr != nil {
		return 0, err
	}
	b.events.Info(fmt.Sprintf("Session %s was reactivated.", b.sessionID))
	return b.opts.Client.PushCheckpoint(ctx, b.token(), upload)
}

// chunk splits entries into groups whose encoded size stays under limit. An
// entry larger than limit on its own still gets a group of its own.
func chunk(entries []api.CheckpointEntry, limit int) [][]api.CheckpointEntry {
	var chunks [][]api.CheckpointEntry
	var current []api.CheckpointEntry
	size := 0
	for _, e := range entries {
		n := encodedSize(e)
		if len(current) > 0 && size+n > limit {
			chunks = append(chunks, current)
			current, size = nil, 0
		}
		current = append(current, e)
		size += n
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// encodedSize is the entry's size in the uncompressed request body, which is
// what the server's limit applies to.
func encodedSize(e api.CheckpointEntry) int {
	var w countingWriter
	enc := json.NewEncoder(&w)
	enc.SetEscapeHTML(false) // as the client encodes it
	_ = enc.Encode(e)        // writes to w cannot fail
	return int(w)
}

type countingWriter int

func (w *countingWriter) Write(p []byte) (int, error) {
	*w += countingWriter(len(p))
	return len(p), nil
}

func dedupe(paths []string) []string {
	slices.Sort(paths)
	return slices.Compact(paths)
}
