package broadcast

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/human"
	"github.com/biga-app/cli/internal/snapshot"
)

const (
	// maxHashesPerQuery bounds one "which blobs are missing" request.
	maxHashesPerQuery = 5000
	// maxBlobsPerBatch bounds one content upload, whatever the sizes.
	maxBlobsPerBatch = 2000
)

func (b *Broadcast) serverLimits() api.Limits {
	b.cred.Lock()
	defer b.cred.Unlock()
	return b.limits
}

// checkFileCount fails before anything is uploaded when the project has more
// files than the server keeps for one session.
func (b *Broadcast) checkFileCount(files int) error {
	if limit := b.serverLimits().MaxSnapshotFiles; limit > 0 && files > limit {
		return fmt.Errorf("%s has %s, but this server keeps at most %d per session; share part of the project with --only",
			b.opts.RootDir, human.Count(files, "file", "files"), limit)
	}
	return nil
}

// blobBatchBytes is how much content one upload carries: under the server's
// limit with room for the framing, and never more than a checkpoint.
func (b *Broadcast) blobBatchBytes() int {
	limit := maxCheckpointBytes
	if server := b.serverLimits().MaxBlobBatchBytes; server > 0 {
		limit = min(limit, int(server)*3/4)
	}
	return limit
}

// uploadBlobs sends the content of every text file the server does not have,
// so the snapshot that follows can carry hashes alone. It returns the bytes
// sent. Content the server already stored, for instance by an upload that was
// interrupted, is not sent again.
func (b *Broadcast) uploadBlobs(ctx context.Context, tree *api.FileNode, files []*api.FileNode) (int, error) {
	byHash := make(map[string]*api.FileNode)
	var order []string
	for _, n := range files {
		if n.Binary || n.Hash == snapshot.EmptyHash {
			continue
		}
		if _, seen := byHash[n.Hash]; !seen {
			byHash[n.Hash] = n
			order = append(order, n.Hash)
		}
	}

	b.events.Progress("Comparing with the server…")
	var missing []string
	for start := 0; start < len(order); start += maxHashesPerQuery {
		got, err := b.opts.Client.MissingBlobs(ctx, b.sessionID, b.token(), order[start:min(start+maxHashesPerQuery, len(order))])
		if err != nil {
			return 0, err
		}
		missing = append(missing, got...)
	}
	if len(missing) == 0 {
		return 0, nil
	}

	var total int64
	for _, h := range missing {
		if n := byHash[h]; n != nil {
			total += n.Size
		}
	}
	up := blobUpload{b: b, ctx: ctx, limit: b.blobBatchBytes(), total: total, count: len(missing)}
	gone := make(map[*api.FileNode]bool)
	for _, h := range missing {
		n := byHash[h]
		if n == nil {
			continue
		}
		blob, ok, err := b.readBlob(n)
		if err != nil {
			return up.sent, err
		}
		if !ok {
			gone[n] = true
			continue
		}
		if blob.Hash == "" {
			continue
		}
		if err := up.add(blob); err != nil {
			return up.sent, err
		}
	}
	if err := up.flush(); err != nil {
		return up.sent, err
	}
	if len(gone) > 0 {
		snapshot.Prune(tree, gone)
	}
	return up.sent, nil
}

// readBlob reads the content of a file the tree lists. If the file changed
// since it was hashed, the tree entry is updated to what is on disk now. ok is
// false when the file is gone; the returned blob has no hash when the file
// turned into a placeholder, such as a binary, that needs no content.
func (b *Broadcast) readBlob(n *api.FileNode) (blob api.Blob, ok bool, err error) {
	f, err := snapshot.ReadFile(b.root, n.Path, snapshot.ReadOptions{NoSecretScan: b.opts.NoSecretScan})
	if errors.Is(err, fs.ErrNotExist) {
		return api.Blob{}, false, nil
	}
	if err != nil {
		return api.Blob{}, false, fmt.Errorf("read %s: %w", n.Path, err)
	}
	n.Hash, n.Size, n.Binary = f.Hash, f.Size, f.Binary
	if f.Binary {
		return api.Blob{}, true, nil
	}
	return api.Blob{Hash: f.Hash, Content: f.Content}, true, nil
}

// blobUpload batches blobs under the server's request limit and reports
// progress.
type blobUpload struct {
	b     *Broadcast
	ctx   context.Context
	limit int
	total int64
	count int

	batch []api.Blob
	size  int
	done  int64
	sent  int
}

func (u *blobUpload) add(blob api.Blob) error {
	n := api.BlobEncodedSize(blob)
	if len(u.batch) > 0 && (u.size+n > u.limit || len(u.batch) >= maxBlobsPerBatch) {
		if err := u.flush(); err != nil {
			return err
		}
	}
	u.batch = append(u.batch, blob)
	u.size += n
	return nil
}

func (u *blobUpload) flush() error {
	if len(u.batch) == 0 {
		return nil
	}
	sent, err := u.b.opts.Client.UploadBlobs(u.ctx, u.b.sessionID, u.b.token(), u.batch)
	if err != nil {
		return err
	}
	u.sent += sent
	for _, blob := range u.batch {
		u.done += int64(len(blob.Content))
	}
	u.b.events.Progress(fmt.Sprintf("Uploading %s · %s / %s", human.Count(u.count, "file", "files"), human.Bytes(u.done), human.Bytes(u.total)))
	u.batch, u.size = nil, 0
	return nil
}
