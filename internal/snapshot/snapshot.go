package snapshot

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/ignore"
	"github.com/biga-app/cli/internal/secrets"
)

// ReadConcurrency bounds how many files are read at once, so a large tree
// cannot exhaust file descriptors.
const ReadConcurrency = 16

// BuildOptions adjusts [Build]. The zero value scans for secrets and reports
// nothing.
type BuildOptions struct {
	NoSecretScan bool
	// OnSkip is told about files that could not be read.
	OnSkip func(rel string, err error)
	// OnWithheld is told about files kept out because they look like they
	// hold a secret. They stay in the tree as placeholders.
	OnWithheld func(rel string, findings []secrets.Finding)
}

// Build reads root into a tree, leaving out paths the filter rejects.
// Build-output directories appear as empty stubs. Files that cannot be read
// are left out and reported to opts.OnSkip.
//
// Symlinks and special files are skipped: following links risks cycles and
// escaping the root.
func Build(ctx context.Context, root *os.Root, filter *ignore.Filter, opts BuildOptions) (*api.FileNode, error) {
	tree := &api.FileNode{Type: api.NodeDirectory, Name: filepath.Base(root.Name()), Path: "."}
	var files []*fileJob
	if err := walk(root.FS(), tree, filter, &files); err != nil {
		return nil, err
	}

	// Directories are walked serially, which is cheap; file reads dominate
	// and run in parallel.
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(ReadConcurrency)
	for _, job := range files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f, err := ReadFile(root, job.node.Path, ReadOptions{NoSecretScan: opts.NoSecretScan})
			if err != nil {
				job.err = err
				return nil
			}
			job.node.Size, job.node.Hash, job.node.Content, job.node.Binary = f.Size, f.Hash, f.Content, f.Binary
			job.withheld = f.Withheld
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	failed := make(map[*api.FileNode]bool)
	for _, job := range files {
		switch {
		case job.err != nil:
			failed[job.node] = true
			if opts.OnSkip != nil {
				opts.OnSkip(job.node.Path, job.err)
			}
		case len(job.withheld) > 0 && opts.OnWithheld != nil:
			opts.OnWithheld(job.node.Path, job.withheld)
		}
	}
	if len(failed) > 0 {
		prune(tree, failed)
	}
	return tree, nil
}

type fileJob struct {
	node     *api.FileNode
	err      error
	withheld []secrets.Finding
}

func walk(root fs.FS, dir *api.FileNode, filter *ignore.Filter, files *[]*fileJob) error {
	entries, err := fs.ReadDir(root, dir.Path)
	if err != nil {
		if dir.Path == "." {
			return fmt.Errorf("read share root: %w", err)
		}
		// An unreadable subdirectory shows up empty rather than failing the
		// whole broadcast.
		return nil
	}

	for _, entry := range entries {
		name := entry.Name()
		childRel := name
		if dir.Path != "." {
			childRel = dir.Path + "/" + name
		}

		switch {
		case entry.IsDir() && ignore.IsStubDir(name) && filter.InScope(childRel, true):
			// Stubs take precedence over .gitignore so build output is still
			// visible as a folder when the project also ignores it.
			dir.Children = append(dir.Children, &api.FileNode{
				Type: api.NodeDirectory, Name: name, Path: childRel, Ignored: true,
			})
		case entry.IsDir():
			if !filter.Accepts(childRel, true) {
				continue
			}
			child := &api.FileNode{Type: api.NodeDirectory, Name: name, Path: childRel}
			if err := walk(root, child, filter, files); err != nil {
				return err
			}
			dir.Children = append(dir.Children, child)
		case entry.Type().IsRegular():
			if !filter.Accepts(childRel, false) {
				continue
			}
			child := &api.FileNode{Type: api.NodeFile, Name: name, Path: childRel}
			dir.Children = append(dir.Children, child)
			*files = append(*files, &fileJob{node: child})
		}
	}
	return nil
}

func prune(dir *api.FileNode, drop map[*api.FileNode]bool) {
	dir.Children = slices.DeleteFunc(dir.Children, func(n *api.FileNode) bool { return drop[n] })
	for _, child := range dir.Children {
		if child.Type == api.NodeDirectory {
			prune(child, drop)
		}
	}
}

// Hashes indexes every file in the tree by path.
func Hashes(tree *api.FileNode) map[string]string {
	hashes := make(map[string]string)
	walkFiles(tree, func(n *api.FileNode) { hashes[n.Path] = n.Hash })
	return hashes
}

func walkFiles(n *api.FileNode, fn func(*api.FileNode)) {
	if n.Type == api.NodeFile {
		fn(n)
		return
	}
	for _, child := range n.Children {
		walkFiles(child, fn)
	}
}

// Diff returns the checkpoint entries that bring the server's manifest in
// line with the local tree: new and changed files, plus deletions for files
// only the server still has. It returns nil when both already match.
func Diff(manifest *api.Manifest, tree *api.FileNode) []api.CheckpointEntry {
	remote := make(map[string]string, len(manifest.Files))
	for _, f := range manifest.Files {
		remote[f.Path] = f.Hash
	}

	var entries []api.CheckpointEntry
	local := make(map[string]bool)
	walkFiles(tree, func(n *api.FileNode) {
		local[n.Path] = true
		hash, ok := remote[n.Path]
		if ok && hash == n.Hash {
			return
		}
		change := api.ChangeModified
		if !ok {
			change = api.ChangeAdded
		}
		f := File{Size: n.Size, Hash: n.Hash, Content: n.Content, Binary: n.Binary}
		entries = append(entries, f.Entry(n.Path, change))
	})
	for _, f := range manifest.Files {
		if !local[f.Path] {
			entries = append(entries, Deletion(f.Path))
		}
	}
	slices.SortFunc(entries, func(a, b api.CheckpointEntry) int { return cmp.Compare(a.Path, b.Path) })
	return entries
}

// Entry converts the file into a checkpoint entry for path rel.
func (f File) Entry(rel string, change api.ChangeType) api.CheckpointEntry {
	return api.CheckpointEntry{
		Path:       rel,
		ChangeType: change,
		Hash:       new(f.Hash),
		Size:       f.Size,
		Binary:     f.Binary,
		Content:    f.Content,
	}
}

// Deletion is the checkpoint entry for a removed file.
func Deletion(rel string) api.CheckpointEntry {
	return api.CheckpointEntry{Path: rel, ChangeType: api.ChangeDeleted}
}
