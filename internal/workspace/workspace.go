// Package workspace loads biga.json, which lists the projects a bare
// `biga start` broadcasts together as one linked group.
package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/biga-app/cli/internal/session"
)

// FileName is the workspace file looked up in the working directory.
const FileName = "biga.json"

// Project is one directory to broadcast.
type Project struct {
	// RootDir is absolute.
	RootDir string
	// Title is empty when the file does not set one.
	Title string
}

// Workspace is a parsed workspace file.
type Workspace struct {
	// ID links the workspace's sessions on the server.
	ID       string
	Projects []Project
}

type file struct {
	// WorkspaceID is a pointer to tell a missing ID from an empty one.
	WorkspaceID *string `json:"workspaceId"`
	Projects    []struct {
		Dir   string `json:"dir"`
		Title string `json:"title"`
	} `json:"projects"`
}

// Load reads the workspace file in dir. It returns (nil, nil) when there is
// none, so callers can fall back to broadcasting dir itself. Project paths
// resolve relative to the file.
//
// A file without a workspaceId gets a generated one written back, so the
// group link stays stable across runs.
func Load(dir string) (*Workspace, error) {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", FileName, err)
	}

	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", FileName, err)
	}
	if len(f.Projects) == 0 {
		return nil, fmt.Errorf("invalid %s: \"projects\" must be a non-empty array", FileName)
	}

	ws := &Workspace{}
	seen := make(map[string]bool, len(f.Projects))
	for i, p := range f.Projects {
		if p.Dir == "" {
			return nil, fmt.Errorf("invalid %s: project %d must have a non-empty \"dir\"", FileName, i)
		}
		root := p.Dir
		if !filepath.IsAbs(root) {
			root = filepath.Join(dir, root)
		}
		root = filepath.Clean(root)
		if seen[root] {
			return nil, fmt.Errorf("invalid %s: project %d repeats %q", FileName, i, p.Dir)
		}
		seen[root] = true
		ws.Projects = append(ws.Projects, Project{RootDir: root, Title: p.Title})
	}

	if f.WorkspaceID == nil {
		ws.ID = session.NewWorkspaceID()
		// A read-only file is not fatal: this run uses the generated ID, it
		// just won't persist.
		_ = writeID(path, raw, ws.ID)
		return ws, nil
	}
	if err := session.ValidateID(*f.WorkspaceID); err != nil {
		return nil, fmt.Errorf("invalid %s: workspaceId: %w", FileName, err)
	}
	ws.ID = *f.WorkspaceID
	return ws, nil
}

// writeID inserts a workspaceId as the first member of the file's top-level
// object, leaving the rest of the user's formatting untouched.
func writeID(path string, raw []byte, id string) error {
	open := bytes.IndexByte(raw, '{')
	if open < 0 {
		return errors.New("no top-level object")
	}
	member := "\n  \"workspaceId\": " + strconv.Quote(id) + ","

	out := make([]byte, 0, len(raw)+len(member))
	out = append(out, raw[:open+1]...)
	out = append(out, member...)
	out = append(out, raw[open+1:]...)

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, info.Mode().Perm())
}
