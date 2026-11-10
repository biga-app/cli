package api

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

func putSessionToPB(in CreateSessionInput) *bigav1.PutSessionRequest {
	req := &bigav1.PutSessionRequest{Encrypted: in.Encrypted}
	if in.Title != "" {
		req.Title = &in.Title
	}
	if in.GroupID != "" {
		req.GroupId = &in.GroupID
	}
	return req
}

func parseTime(field, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode response: %s is not a timestamp", field)
	}
	return t, nil
}

func sessionFromPB(g *bigav1.SessionGrant) (*Session, error) {
	s := g.GetSession()
	created, err := parseTime("createdAt", s.GetCreatedAt())
	if err != nil {
		return nil, err
	}
	updated, err := parseTime("updatedAt", s.GetUpdatedAt())
	if err != nil {
		return nil, err
	}
	out := &Session{
		ID: s.GetId(), Title: s.GetTitle(), Status: statusName(s.GetStatus()),
		CreatedAt: created, UpdatedAt: updated, GroupID: s.GroupId,
		Token: g.GetToken(), ShareURL: g.GetShareUrl(), Encrypted: s.GetEncrypted(),
		Limits: Limits{
			MaxSnapshotFiles:  int(g.GetLimits().GetMaxSnapshotFiles()),
			MaxFileBytes:      g.GetLimits().GetMaxFileBytes(),
			MaxBlobBatchBytes: g.GetLimits().GetMaxBlobBatchBytes(),
		},
	}
	if s.EndedAt != nil {
		ended, err := parseTime("endedAt", s.GetEndedAt())
		if err != nil {
			return nil, err
		}
		out.EndedAt = &ended
	}
	return out, nil
}

func statusName(s bigav1.SessionStatus) string {
	switch s {
	case bigav1.SessionStatus_SESSION_STATUS_ACTIVE:
		return "active"
	case bigav1.SessionStatus_SESSION_STATUS_ENDED:
		return "ended"
	}
	return ""
}

func changeToPB(c ChangeType) bigav1.ChangeType {
	switch c {
	case ChangeAdded:
		return bigav1.ChangeType_CHANGE_TYPE_ADDED
	case ChangeModified:
		return bigav1.ChangeType_CHANGE_TYPE_MODIFIED
	case ChangeDeleted:
		return bigav1.ChangeType_CHANGE_TYPE_DELETED
	}
	return bigav1.ChangeType_CHANGE_TYPE_UNSPECIFIED
}

func nodeToPB(n *FileNode) *bigav1.FileNode {
	out := &bigav1.FileNode{Name: n.Name, Path: n.Path}
	if n.Type == NodeFile {
		out.Kind = &bigav1.FileNode_File{File: &bigav1.FileInfo{
			Size: n.Size, Hash: n.Hash, Binary: n.Binary, Content: []byte(n.Content), Lines: int32(n.Lines),
		}}
		return out
	}
	dir := &bigav1.DirectoryInfo{Ignored: n.Ignored, Children: make([]*bigav1.FileNode, len(n.Children))}
	for i, child := range n.Children {
		dir.Children[i] = nodeToPB(child)
	}
	out.Kind = &bigav1.FileNode_Directory{Directory: dir}
	return out
}

func snapshotToPB(s *FileSnapshot) *bigav1.FileSnapshot {
	return &bigav1.FileSnapshot{
		SessionId: s.SessionID, Tree: nodeToPB(s.Tree),
		Timestamp: s.Timestamp, SequenceNum: int64(s.SequenceNum),
	}
}

func entryToPB(e CheckpointEntry) *bigav1.CheckpointEntry {
	return &bigav1.CheckpointEntry{
		Path: e.Path, ChangeType: changeToPB(e.ChangeType), Hash: e.Hash,
		Size: e.Size, Binary: e.Binary, Content: []byte(e.Content),
		Additions: int32(e.Additions), Deletions: int32(e.Deletions),
	}
}

func uploadToPB(u *CheckpointUpload) *bigav1.CheckpointUpload {
	entries := make([]*bigav1.CheckpointEntry, len(u.Entries))
	for i, e := range u.Entries {
		entries[i] = entryToPB(e)
	}
	return &bigav1.CheckpointUpload{SessionId: u.SessionID, Entries: entries, Timestamp: u.Timestamp}
}

func manifestFromPB(m *bigav1.Manifest) *Manifest {
	files := make([]ManifestEntry, len(m.GetFiles()))
	for i, f := range m.GetFiles() {
		files[i] = ManifestEntry{Path: f.GetPath(), Hash: f.GetHash(), Size: f.GetSize(), Binary: f.GetBinary()}
	}
	return &Manifest{Files: files}
}

func extensionFromPB(e *bigav1.Extension) Extension {
	return Extension{ID: e.GetId(), Name: e.GetName(), CreatedAt: e.GetCreatedAt()}
}

const entriesField = 2

// EncodedSize is the entry's size within an uncompressed upload body, which is
// what the server's limit applies to.
func (e CheckpointEntry) EncodedSize() int {
	return protowire.SizeTag(entriesField) + protowire.SizeBytes(proto.Size(entryToPB(e)))
}

func blobsToPB(blobs []Blob) *bigav1.UploadBlobsRequest {
	out := make([]*bigav1.Blob, len(blobs))
	for i, b := range blobs {
		out[i] = &bigav1.Blob{Hash: b.Hash, Content: []byte(b.Content)}
	}
	return &bigav1.UploadBlobsRequest{Blobs: out}
}

// BlobEncodedSize is a blob's size within an upload body.
func BlobEncodedSize(b Blob) int {
	return protowire.SizeTag(1) + protowire.SizeBytes(proto.Size(&bigav1.Blob{Hash: b.Hash, Content: []byte(b.Content)}))
}
