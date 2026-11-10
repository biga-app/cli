package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/biga-app/cli/internal/broadcast"
	"github.com/biga-app/cli/internal/config"
	"github.com/biga-app/cli/internal/human"
	"github.com/biga-app/cli/internal/snapshot"
)

const maxGroupRows = 8

func (a *app) dryRun(ctx context.Context, projects []project, only []string, noSecretScan bool) error {
	excludes := config.GitExcludesFile()
	for _, p := range projects {
		report, err := broadcast.Preview(ctx, broadcast.PreviewOptions{
			RootDir:      p.root,
			ExcludesFile: excludes,
			Only:         only,
			NoSecretScan: noSecretScan,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", p.root, err)
		}
		a.printReport(cmp.Or(p.title, filepath.Base(p.root)), report)
	}
	a.ui.Success("Dry run: nothing was uploaded.")
	return nil
}

func (a *app) printReport(name string, r broadcast.Report) {
	a.ui.Info(fmt.Sprintf("%s: would share %s, %s", name, human.Count(r.Files, "file", "files"), human.Bytes(r.Bytes)))
	for _, g := range r.Groups[:min(len(r.Groups), maxGroupRows)] {
		a.ui.Message(fmt.Sprintf("%-28s %s, %s", g.Name, human.Count(g.Files, "file", "files"), human.Bytes(g.Bytes)))
	}
	if extra := len(r.Groups) - maxGroupRows; extra > 0 {
		a.ui.Message(fmt.Sprintf("…and %d more", extra))
	}
	for _, w := range r.Withheld {
		a.ui.Warn(fmt.Sprintf("%s would be withheld: possible %s.", w.Path, w.Findings[0]))
	}
	for _, p := range r.Oversize {
		a.ui.Info(fmt.Sprintf("%s is larger than %d MB; viewers would see a placeholder.", p, snapshot.MaxFileBytes>>20))
	}
}

func onlyPaths(root string, paths []string) ([]string, error) {
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("--only %s: %w", p, err)
		}
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("--only %s: no such file or directory", p)
		} else if err != nil {
			return nil, fmt.Errorf("--only %s: %w", p, err)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("--only %s: not inside %s", p, root)
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	return rels, nil
}
