package broadcast

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/ignore"
	"github.com/biga-app/cli/internal/secrets"
	"github.com/biga-app/cli/internal/snapshot"
)

func withheldMessage(rel string, findings []secrets.Finding) string {
	return fmt.Sprintf("Withheld %s: possible %s. Remove it, or mark the line with %s.", rel, findings[0], secrets.AllowMarker)
}

type PreviewOptions struct {
	RootDir      string
	ExcludesFile string
	Only         []string
	NoSecretScan bool
}

type Group struct {
	Name  string
	Files int
	Bytes int64
}

type Withheld struct {
	Path     string
	Findings []secrets.Finding
}

type Report struct {
	Files    int
	Bytes    int64
	Groups   []Group
	Withheld []Withheld
	Oversize []string
}

func Preview(ctx context.Context, opts PreviewOptions) (Report, error) {
	filter, err := ignore.NewFilter(opts.RootDir, opts.ExcludesFile)
	if err != nil {
		return Report{}, err
	}
	filter.Restrict(opts.Only...)
	root, err := os.OpenRoot(opts.RootDir)
	if err != nil {
		return Report{}, err
	}
	defer root.Close()

	var report Report
	tree, err := snapshot.Build(ctx, root, filter, snapshot.BuildOptions{
		NoSecretScan: opts.NoSecretScan,
		OnWithheld: func(rel string, findings []secrets.Finding) {
			report.Withheld = append(report.Withheld, Withheld{Path: rel, Findings: findings})
		},
	})
	if err != nil {
		return Report{}, err
	}
	report.summarize(tree)
	return report, nil
}

func (r *Report) summarize(tree *api.FileNode) {
	groups := make(map[string]*Group)
	for _, child := range tree.Children {
		name := "./"
		if child.Type == api.NodeDirectory {
			name = child.Name + "/"
		}
		g := groups[name]
		if g == nil {
			g = &Group{Name: name}
			groups[name] = g
		}
		r.count(g, child)
	}
	for _, g := range groups {
		if g.Files > 0 {
			r.Groups = append(r.Groups, *g)
		}
	}
	slices.SortFunc(r.Groups, func(a, b Group) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.Name, b.Name))
	})
}

func (r *Report) count(g *Group, n *api.FileNode) {
	if n.Type == api.NodeDirectory {
		for _, child := range n.Children {
			r.count(g, child)
		}
		return
	}
	size := int64(len(n.Content))
	g.Files++
	g.Bytes += size
	r.Files++
	r.Bytes += size
	if n.Size > snapshot.MaxFileBytes {
		r.Oversize = append(r.Oversize, n.Path)
	}
}
