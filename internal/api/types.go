package api

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// NodeType distinguishes files from directories in a snapshot tree.
type NodeType string

const (
	NodeFile      NodeType = "file"
	NodeDirectory NodeType = "directory"
)

// FileNode is one entry of a snapshot tree. Which fields are meaningful
// depends on Type: files carry Size, Hash, Content, and Binary; directories
// carry Children and Ignored.
type FileNode struct {
	Type NodeType
	Name string
	// Path is slash-separated and relative to the broadcast root; the root
	// itself is ".".
	Path string

	Size int64
	// Hash is the hex SHA-256 the server uses as the content's blob key.
	Hash string
	// Content is the file's text as stored on disk, empty for binary files.
	Content string
	// Binary marks files whose bytes are never uploaded (binary or oversize).
	Binary bool

	Children []*FileNode
	// Ignored marks build-output directories shown as empty stubs.
	Ignored bool
}

// FileSnapshot is the full tree uploaded when a broadcast starts.
type FileSnapshot struct {
	SessionID string
	Tree      *FileNode
	// Timestamp is in Unix milliseconds.
	Timestamp   int64
	SequenceNum int
}

// ChangeType describes how a file changed within a checkpoint.
type ChangeType string

const (
	ChangeAdded    ChangeType = "added"
	ChangeModified ChangeType = "modified"
	ChangeDeleted  ChangeType = "deleted"
)

// CheckpointEntry is one changed file in a checkpoint upload. The CLI sends
// only the new content and its hash; the server resolves the previous version
// and computes diff stats.
type CheckpointEntry struct {
	Path       string
	ChangeType ChangeType
	// Hash is nil for deletions.
	Hash *string
	Size int64
	// Binary entries carry no content.
	Binary  bool
	Content string
}

// CheckpointUpload is one save burst's worth of changes.
type CheckpointUpload struct {
	SessionID string
	Entries   []CheckpointEntry
	// Timestamp is in Unix milliseconds.
	Timestamp int64
}

// MaxEntryContentBytes caps a single entry's content, independently of the
// CLI's own file size limit, as a backstop against oversized uploads.
const MaxEntryContentBytes = 5 << 20

var hashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Validate checks the structural rules the server enforces, so a malformed
// payload fails locally with a precise message instead of a 400.
func (u *CheckpointUpload) Validate() error {
	if u.SessionID == "" {
		return errors.New("checkpoint has no session id")
	}
	if len(u.Entries) == 0 {
		return errors.New("checkpoint has no entries")
	}
	for _, e := range u.Entries {
		if err := e.validate(); err != nil {
			return fmt.Errorf("entry %q: %w", e.Path, err)
		}
	}
	return nil
}

func (e *CheckpointEntry) validate() error {
	switch {
	case e.Path == "":
		return errors.New("empty path")
	case strings.Contains(e.Path, `\`):
		return errors.New("path must use forward slashes")
	case path.IsAbs(e.Path):
		return errors.New("path must be relative")
	}
	if clean := path.Clean(e.Path); clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("path traversal is not allowed")
	}
	switch e.ChangeType {
	case ChangeDeleted:
		if e.Hash != nil {
			return errors.New("deletion must not carry a hash")
		}
	case ChangeAdded, ChangeModified:
		if e.Hash == nil || !hashPattern.MatchString(*e.Hash) {
			return errors.New("missing or malformed content hash")
		}
	default:
		return fmt.Errorf("unknown change type %q", e.ChangeType)
	}
	if len(e.Content) > MaxEntryContentBytes {
		return fmt.Errorf("content exceeds the %d MB limit", MaxEntryContentBytes>>20)
	}
	return nil
}

// CreateSessionInput is the body of the idempotent session upsert.
type CreateSessionInput struct {
	Title string
	// GroupID links sessions started together from one workspace file.
	GroupID string
}

// Session is the server's view of a broadcast session.
type Session struct {
	ID        string
	Title     string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	EndedAt   *time.Time
	GroupID   *string
	// Token is the session's owner credential. The server returns it only on
	// the response that created the session and keeps nothing but a hash, so
	// it must be saved: every later write needs it.
	Token string
	// ShareURL is the viewer link, when the server knows its public address.
	ShareURL string
	// Limits is what the server accepts, so an upload can fail early.
	Limits Limits
}

// Limits is what a server accepts. A zero field means the server did not say.
type Limits struct {
	// MaxSnapshotFiles is the most files one session may hold.
	MaxSnapshotFiles int
	// MaxFileBytes is the largest file the server stores.
	MaxFileBytes int64
	// MaxBlobBatchBytes is the largest request body for uploading content.
	MaxBlobBatchBytes int64
}

// Blob is file content uploaded ahead of the snapshot that refers to it.
type Blob struct {
	// Hash is the hex SHA-256 of Content.
	Hash    string
	Content string
}

// ManifestEntry identifies one file the server currently holds for a session.
type ManifestEntry struct {
	Path   string
	Hash   string
	Size   int64
	Binary bool
}

// Manifest lists every current file of a session, without content.
type Manifest struct {
	Files []ManifestEntry
}
