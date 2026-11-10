package broadcast

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/biga-app/cli/internal/snapshot"
)

func TestPreview(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.go":           "package main\n",
		"src/a.go":          "package a\n",
		"src/b.go":          "package b\n",
		"docs/guide.md":     "guide",
		"deploy/creds.txt":  "key = AKIA" + "IOSFODNN7EXAMPLE\n",
		".env":              "A=1",
		"node_modules/x.js": "x",
	}
	for rel, content := range files {
		write(t, root, rel, content)
	}
	big := strings.Repeat("a", snapshot.MaxFileBytes+1)
	write(t, root, "big.txt", big)

	report, err := Preview(t.Context(), PreviewOptions{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 6 || report.Bytes != int64(len("package main\n")+2*len("package a\n")+len("guide")) {
		t.Errorf("Files = %d, Bytes = %d", report.Files, report.Bytes)
	}
	if len(report.Withheld) != 1 || report.Withheld[0].Path != "deploy/creds.txt" {
		t.Errorf("Withheld = %+v", report.Withheld)
	}
	if len(report.Oversize) != 1 || report.Oversize[0] != "big.txt" {
		t.Errorf("Oversize = %v", report.Oversize)
	}
	groups := map[string]int{}
	for _, g := range report.Groups {
		groups[g.Name] = g.Files
	}
	want := map[string]int{"./": 2, "src/": 2, "docs/": 1, "deploy/": 1}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	for name, n := range want {
		if groups[name] != n {
			t.Errorf("group %s has %d files, want %d", name, groups[name], n)
		}
	}
	if report.Groups[0].Name != "src/" || report.Groups[1].Name != "./" || report.Groups[3].Name != "deploy/" {
		t.Errorf("groups should be largest first: %+v", report.Groups)
	}
}

func TestPreviewHonorsOnlyAndScanSetting(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.go", "package a")
	write(t, root, "src/creds.txt", "key = AKIA"+"IOSFODNN7EXAMPLE")
	write(t, root, "other/b.go", "package b")

	report, err := Preview(t.Context(), PreviewOptions{RootDir: root, Only: []string{"src"}, NoSecretScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 2 || len(report.Withheld) != 0 || len(report.Groups) != 1 || report.Groups[0].Name != "src/" {
		t.Errorf("report = %+v", report)
	}
}

func TestPreviewMissingRoot(t *testing.T) {
	if _, err := Preview(t.Context(), PreviewOptions{RootDir: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("previewing a missing directory should fail")
	}
}
