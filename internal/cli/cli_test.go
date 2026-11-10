package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/biga-app/cli/internal/workspace"
)

func TestResolveProjectsFromArgs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	projects, err := resolveProjects(startOptions{dirs: []string{a, b, a}})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].root != a || projects[1].root != b {
		t.Fatalf("projects = %+v, want %s and %s once each", projects, a, b)
	}
	if projects[0].group != "" {
		t.Error("positional directories should not be grouped")
	}
}

func TestResolveProjectsRejectsFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProjects(startOptions{dirs: []string{file}}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v", err)
	}
	if _, err := resolveProjects(startOptions{dirs: []string{filepath.Join(t.TempDir(), "missing")}}); err == nil {
		t.Fatal("a missing directory was accepted")
	}
}

func TestResolveProjectsFromWorkspace(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"api", "web"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := `{"workspaceId":"ws-test","projects":[{"dir":"api","title":"API"},{"dir":"web"}]}`
	if err := os.WriteFile(filepath.Join(dir, workspace.FileName), []byte(ws), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	projects, err := resolveProjects(startOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].title != "API" || projects[0].group != "ws-test" || projects[1].group != "ws-test" {
		t.Fatalf("projects = %+v", projects)
	}
	if _, err := resolveProjects(startOptions{title: "x"}); err == nil {
		t.Error("--title was accepted alongside a workspace file")
	}
}

func TestResolveProjectsDefaultsToWorkingDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	projects, err := resolveProjects(startOptions{title: "Demo"})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if len(projects) != 1 || projects[0].root != wd || projects[0].title != "Demo" {
		t.Fatalf("projects = %+v", projects)
	}
}

func TestTargetDir(t *testing.T) {
	dir := t.TempDir()
	if got, err := targetDir("", []string{dir}); err != nil || got != dir {
		t.Errorf("targetDir(arg) = %q, %v", got, err)
	}
	if got, err := targetDir(dir, nil); err != nil || got != dir {
		t.Errorf("targetDir(flag) = %q, %v", got, err)
	}
	if _, err := targetDir(dir, []string{dir}); err == nil {
		t.Error("both an argument and --dir were accepted")
	}
}
