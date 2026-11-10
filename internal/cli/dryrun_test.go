package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOnlyPaths(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"src/app", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	got, err := onlyPaths(root, []string{
		filepath.Join(root, "src"),
		filepath.Join(root, "src", "app") + string(filepath.Separator),
		filepath.Join(root, "README.md"),
		root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src", "src/app", "README.md", "."}; !slices.Equal(got, want) {
		t.Errorf("onlyPaths = %v, want %v", got, want)
	}

	if _, err := onlyPaths(root, []string{filepath.Join(root, "missing")}); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("missing path: err = %v", err)
	}
	if _, err := onlyPaths(root, []string{outside}); err == nil || !strings.Contains(err.Error(), "not inside") {
		t.Errorf("outside path: err = %v", err)
	}
	if _, err := onlyPaths(root, []string{filepath.Join(root, "docs", "..", "..")}); err == nil {
		t.Error("a path escaping the root with .. should fail")
	}
}
