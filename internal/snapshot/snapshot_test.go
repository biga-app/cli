package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/ignore"
	"github.com/biga-app/cli/internal/secrets"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func build(t *testing.T, root string) *api.FileNode {
	t.Helper()
	filter, err := ignore.NewFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{KeepContent: true})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func openRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func index(tree *api.FileNode) map[string]*api.FileNode {
	nodes := map[string]*api.FileNode{}
	var visit func(*api.FileNode)
	visit = func(n *api.FileNode) {
		nodes[n.Path] = n
		for _, c := range n.Children {
			visit(c)
		}
	}
	visit(tree)
	return nodes
}

func TestBuild(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".gitignore":              "*.log\n",
		"main.go":                 "package main\n",
		"src/app/util.go":         "package app\n",
		"debug.log":               "noise",
		"image.png":               "\x89PNG\x00\x01",
		"node_modules/x/index.js": "x",
		"dist/bundle.js":          "bundle",
		"pkg/build/out.o":         "obj",
		".env":                    "SECRET=1",
		"invalid-utf8.txt":        "ok\xffok",
	})
	big := bytes.Repeat([]byte("a"), MaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	tree := build(t, root)
	nodes := index(tree)

	if tree.Path != "." || tree.Type != api.NodeDirectory {
		t.Errorf("root = %+v", tree)
	}
	for _, want := range []string{"main.go", "src", "src/app", "src/app/util.go", ".gitignore", "image.png", "big.txt"} {
		if nodes[want] == nil {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{"debug.log", "node_modules", ".env", "dist/bundle.js", "pkg/build/out.o"} {
		if nodes[unwanted] != nil {
			t.Errorf("%s should be excluded", unwanted)
		}
	}

	if n := nodes["main.go"]; n.Content != "package main\n" || n.Size != 13 || len(n.Hash) != 64 || n.Binary {
		t.Errorf("main.go = %+v", n)
	}
	if n := nodes["image.png"]; !n.Binary || n.Content != "" || n.Size != 6 {
		t.Errorf("image.png = %+v", n)
	}
	if n := nodes["big.txt"]; !n.Binary || n.Content != "" || n.Size != int64(len(big)) {
		t.Errorf("big.txt = binary %v, size %d", n.Binary, n.Size)
	}
	if n := nodes["invalid-utf8.txt"]; n.Content != "ok\xffok" || n.Hash != hashString("ok\xffok") {
		t.Errorf("invalid-utf8.txt = %q", n.Content)
	}
	for _, stub := range []string{"dist", "pkg/build"} {
		if n := nodes[stub]; n == nil || !n.Ignored || len(n.Children) != 0 {
			t.Errorf("%s should be an empty stub, got %+v", stub, n)
		}
	}
}

func TestBuildLeavesContentOutUnlessAsked(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "hello\n", "sub/b.txt": "world\n"})
	filter, _ := ignore.NewFilter(root)

	tree, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range Files(tree) {
		if n.Content != "" || n.Size == 0 || len(n.Hash) != 64 || n.Binary {
			t.Errorf("%s should carry a hash and size but no content: %+v", n.Path, n)
		}
	}
	if len(Files(tree)) != 2 {
		t.Errorf("Files = %d", len(Files(tree)))
	}
	if EmptyHash != hashString("") {
		t.Errorf("EmptyHash = %s", EmptyHash)
	}
}

func TestPruneRemovesListedFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	tree := build(t, root)
	gone := map[*api.FileNode]bool{}
	for _, n := range Files(tree) {
		if n.Path == "sub/b.txt" {
			gone[n] = true
		}
	}
	Prune(tree, gone)
	if got := Files(tree); len(got) != 1 || got[0].Path != "a.txt" {
		t.Errorf("files after pruning = %v", got)
	}
}

func TestBuildSkipsUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced")
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"ok.txt": "ok", "locked.txt": "secret"})
	if err := os.Chmod(filepath.Join(root, "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}

	filter, _ := ignore.NewFilter(root)
	var skipped []string
	tree, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{
		OnSkip: func(rel string, _ error) { skipped = append(skipped, rel) },
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := index(tree)
	if nodes["locked.txt"] != nil || nodes["ok.txt"] == nil {
		t.Errorf("tree = %v", nodes)
	}
	if len(skipped) != 1 || skipped[0] != "locked.txt" {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestReadFileWithholdsSecrets(t *testing.T) {
	root := t.TempDir()
	secret := "id = AKIA" + "IOSFODNN7EXAMPLE\n"
	writeFiles(t, root, map[string]string{"config.txt": secret})
	r := openRoot(t, root)

	f, err := ReadFile(r, "config.txt", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Binary || f.Content != "" || len(f.Withheld) != 1 || f.Size != int64(len(secret)) {
		t.Fatalf("file = %+v", f)
	}
	if f.Hash != hashString(fmt.Sprintf("withheld:%d:config.txt", len(secret))) {
		t.Errorf("hash = %s", f.Hash)
	}

	shared, err := ReadFile(r, "config.txt", ReadOptions{NoSecretScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if shared.Binary || shared.Content != secret || len(shared.Withheld) != 0 {
		t.Errorf("with scanning off, file = %+v", shared)
	}

	writeFiles(t, root, map[string]string{"config.txt": "id = redacted\n"})
	cleaned, err := ReadFile(r, "config.txt", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Binary || cleaned.Content == "" || cleaned.Hash == f.Hash {
		t.Errorf("after removing the secret, file = %+v", cleaned)
	}
}

func TestBuildReportsWithheldFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"ok.txt":           "fine",
		"deploy/creds.txt": "key = AKIA" + "IOSFODNN7EXAMPLE",
	})
	filter, _ := ignore.NewFilter(root)

	withheld := map[string]string{}
	tree, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{
		KeepContent: true,
		OnWithheld:  func(rel string, findings []secrets.Finding) { withheld[rel] = findings[0].Rule },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withheld) != 1 || withheld["deploy/creds.txt"] != "AWS access key" {
		t.Errorf("withheld = %v", withheld)
	}
	nodes := index(tree)
	if n := nodes["deploy/creds.txt"]; n == nil || !n.Binary || n.Content != "" {
		t.Errorf("creds.txt = %+v", n)
	}
	if n := nodes["ok.txt"]; n == nil || n.Content != "fine" {
		t.Errorf("ok.txt = %+v", n)
	}

	withheld = map[string]string{}
	if _, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{
		NoSecretScan: true,
		OnWithheld:   func(rel string, _ []secrets.Finding) { withheld[rel] = "x" },
	}); err != nil || len(withheld) != 0 {
		t.Errorf("with scanning off: withheld = %v, err = %v", withheld, err)
	}
}

func TestBuildRestrictedToPaths(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"src/app.go":     "package app",
		"src/.env":       "A=1",
		"docs/guide.md":  "guide",
		"dist/bundle.js": "bundle",
		"main.go":        "package main",
		"README.md":      "readme",
	})
	if err := os.MkdirAll(filepath.Join(root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	filter, _ := ignore.NewFilter(root)
	filter.Restrict("src", "README.md")

	tree, err := Build(t.Context(), openRoot(t, root), filter, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nodes := index(tree)
	for _, want := range []string{"src", "src/app.go", "README.md"} {
		if nodes[want] == nil {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{"docs", "docs/guide.md", "main.go", "src/.env", "dist", "build"} {
		if nodes[unwanted] != nil {
			t.Errorf("%s should be outside the restriction", unwanted)
		}
	}
}

func TestReadFileSkipsSymlinks(t *testing.T) {
	path := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, path, map[string]string{"shared/a.txt": "included"})
	writeFiles(t, outside, map[string]string{"a.txt": "outside"})
	for name, target := range map[string]string{
		"external": outside,
		"internal": "shared",
		"file.txt": filepath.Join(outside, "a.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(path, name)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skip("creating symlinks requires permission")
			}
			t.Fatal(err)
		}
	}
	root := openRoot(t, path)
	for _, rel := range []string{"external/a.txt", "internal/a.txt", "file.txt"} {
		if _, err := ReadFile(root, rel, ReadOptions{}); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadFile(%q) returned %v, want not found", rel, err)
		}
	}
}

func TestDiff(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"same.txt":    "same",
		"changed.txt": "new",
		"added.txt":   "added",
	})
	tree := build(t, root)
	hashes := Hashes(tree)

	manifest := &api.Manifest{Files: []api.ManifestEntry{
		{Path: "same.txt", Hash: hashes["same.txt"]},
		{Path: "changed.txt", Hash: strings.Repeat("0", 64)},
		{Path: "removed.txt", Hash: strings.Repeat("1", 64)},
	}}

	entries := Diff(manifest, tree)
	got := map[string]api.ChangeType{}
	for _, e := range entries {
		got[e.Path] = e.ChangeType
	}
	want := map[string]api.ChangeType{
		"added.txt":   api.ChangeAdded,
		"changed.txt": api.ChangeModified,
		"removed.txt": api.ChangeDeleted,
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for path, change := range want {
		if got[path] != change {
			t.Errorf("%s: %q, want %q", path, got[path], change)
		}
	}

	upload := api.CheckpointUpload{SessionID: "s", Entries: entries, Timestamp: 1}
	if err := upload.Validate(); err != nil {
		t.Errorf("diff produced an invalid checkpoint: %v", err)
	}
}

func TestDiffInSync(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "a"})
	tree := build(t, root)
	manifest := &api.Manifest{Files: []api.ManifestEntry{{Path: "a.txt", Hash: Hashes(tree)["a.txt"]}}}
	if entries := Diff(manifest, tree); entries != nil {
		t.Errorf("Diff = %v, want nil", entries)
	}
}
