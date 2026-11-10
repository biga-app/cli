package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatcher(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		isDir    bool
		want     bool
	}{
		{"name matches at root", []string{"*.log"}, "debug.log", false, true},
		{"name matches nested", []string{"*.log"}, "a/b/debug.log", false, true},
		{"star stays in segment", []string{"a/*.go"}, "a/b/c.go", false, false},
		{"anchored by middle slash", []string{"docs/build"}, "x/docs/build", false, false},
		{"anchored by middle slash matches root", []string{"docs/build"}, "docs/build", false, true},
		{"leading slash anchors", []string{"/todo.txt"}, "sub/todo.txt", false, false},
		{"leading slash matches root", []string{"/todo.txt"}, "todo.txt", false, true},
		{"dir-only skips files", []string{"tmp/"}, "tmp", false, false},
		{"dir-only matches dirs", []string{"tmp/"}, "tmp", true, true},
		{"dir-only excludes contents", []string{"tmp/"}, "a/tmp/file.txt", false, true},
		{"negation re-includes", []string{"*.log", "!keep.log"}, "keep.log", false, false},
		{"last rule wins", []string{"!keep.log", "*.log"}, "keep.log", false, true},
		{"no re-include under excluded dir", []string{"logs/", "!logs/keep.log"}, "logs/keep.log", false, true},
		{"re-include under globbed contents", []string{"logs/**", "!logs/keep.log"}, "logs/keep.log", false, false},
		{"leading double star", []string{"**/cache"}, "a/b/cache", true, true},
		{"trailing double star", []string{"vendor/**"}, "vendor/x/y.go", false, true},
		{"trailing double star spares dir", []string{"vendor/**"}, "vendor", true, false},
		{"middle double star zero dirs", []string{"a/**/b"}, "a/b", false, true},
		{"middle double star many dirs", []string{"a/**/b"}, "a/x/y/b", false, true},
		{"question mark", []string{"?.txt"}, "a.txt", false, true},
		{"question mark not slash", []string{"a?b"}, "a/b", false, false},
		{"bracket class", []string{"[ab].txt"}, "b.txt", false, true},
		{"negated bracket class", []string{"[!ab].txt"}, "c.txt", false, true},
		{"negated bracket excludes members", []string{"[!ab].txt"}, "a.txt", false, false},
		{"escaped hash", []string{`\#notes`}, "#notes", false, true},
		{"comment ignored", []string{"#notes"}, "#notes", false, false},
		{"escaped bang", []string{`\!important`}, "!important", false, true},
		{"trailing spaces trimmed", []string{"foo.txt   "}, "foo.txt", false, true},
		{"escaped trailing space kept", []string{`foo\ `}, "foo ", false, true},
		{"literal dot", []string{"a.b"}, "aXb", false, false},
		{"unclosed bracket is literal", []string{"[abc"}, "[abc", false, true},
		{"empty path never matches", []string{"*"}, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMatcher(false)
			m.AddPatterns(tt.patterns...)
			if got := m.Match(tt.path, tt.isDir); got != tt.want {
				t.Errorf("Match(%q, %v) with %q = %v, want %v", tt.path, tt.isDir, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestMatcherIgnoreCase(t *testing.T) {
	sensitive := NewMatcher(false)
	sensitive.AddPatterns("Build.log")
	if sensitive.Match("build.log", false) {
		t.Error("case-sensitive matcher matched a different case")
	}

	insensitive := NewMatcher(true)
	insensitive.AddPatterns("Build.log")
	if !insensitive.Match("build.log", false) {
		t.Error("case-insensitive matcher missed a different case")
	}
}

func TestFilter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.txt\n*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := NewFilter(root)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"main.go", false, true},
		{"src/app.ts", false, true},
		{"secret.txt", false, false},
		{"a/b.tmp", false, false},
		{"node_modules", true, false},
		{"packages/web/node_modules/react/index.js", false, false},
		{".git", true, false},
		{".env", false, false},
		{".env.local", false, false},
		{"dist", true, false},
		{"dist/bundle.js", false, false},
		{"pkg/build/out.o", false, false},
		{"build", false, true}, // a file named like a stub dir is still a file
		{"", true, false},
		{"../outside.txt", false, false},
	}
	for _, tt := range tests {
		if got := f.Accepts(tt.path, tt.isDir); got != tt.want {
			t.Errorf("Accepts(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestFilterRestrict(t *testing.T) {
	f, err := NewFilter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.Restrict("src", "README.md", "docs/guide/", "./pkg/../lib")

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"src", true, true},
		{"src/app/main.go", false, true},
		{"src/.env", false, false}, // the usual rules still apply inside
		{"src/node_modules/x.js", false, false},
		{"README.md", false, true},
		{"README.md.bak", false, false},
		{"srcs/app.go", false, false},
		{"main.go", false, false},
		{"docs", true, true}, // leads to docs/guide
		{"docs", false, false},
		{"docs/guide/intro.md", false, true},
		{"docs/other.md", false, false},
		{"lib/x.go", false, true},
		{"other", true, false},
	}
	for _, tt := range tests {
		if got := f.Accepts(tt.path, tt.isDir); got != tt.want {
			t.Errorf("Accepts(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
	if f.InScope("dist", true) {
		t.Error("a stub directory outside the restriction is in scope")
	}

	f.Restrict(".")
	if !f.Accepts("main.go", false) {
		t.Error("restricting to the root should lift the restriction")
	}
	f.Restrict("src")
	f.Restrict()
	if !f.Accepts("main.go", false) {
		t.Error("an empty restriction should lift it")
	}
}

func TestFilterWithoutGitignore(t *testing.T) {
	f, err := NewFilter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !f.Accepts("README.md", false) {
		t.Error("README.md should be accepted")
	}
	if f.Accepts("node_modules/x.js", false) {
		t.Error("node_modules should be excluded by default")
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
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

func TestFilterNestedIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":         "*.log\n",
		"web/.gitignore":     ".turbo/\n!keep.log\n/local.txt\n",
		"web/.bigaignore":    "notes.md\n",
		"web/src/.gitignore": "*.gen.ts\n",
		".git/info/exclude":  "scratch/\n",
		"global-excludes":    ".idea/\n",
	})
	f, err := NewFilter(root, filepath.Join(root, "global-excludes"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, false},     // root .gitignore
		{"web/app.log", false, false}, // root rules reach down
		{"web/keep.log", false, true}, // a deeper file overrides
		{"keep.log", false, false},    // ...but only beneath itself
		{"web/.turbo", true, false},   // nested dir pattern
		{"web/.turbo/cache.bin", false, false},
		{"web/local.txt", false, false},    // anchored to web/
		{"web/sub/local.txt", false, true}, // ...not deeper
		{"local.txt", false, true},
		{"web/notes.md", false, false}, // .bigaignore
		{"notes.md", false, true},
		{"web/src/api.gen.ts", false, false},
		{"web/src/api.ts", false, true},
		{"scratch", true, false},              // .git/info/exclude
		{".idea/workspace.xml", false, false}, // extra (global) file
		{"web/src/main.go", false, true},
	}
	for _, tt := range tests {
		if got := f.Accepts(tt.path, tt.isDir); got != tt.want {
			t.Errorf("Accepts(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestFilterInvalidate(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"pkg/.gitignore": "a.txt\n"})
	f, err := NewFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Accepts("pkg/a.txt", false) {
		t.Fatal("pkg/a.txt should start ignored")
	}

	writeTree(t, root, map[string]string{"pkg/.gitignore": "b.txt\n"})
	if f.Accepts("pkg/a.txt", false) {
		t.Fatal("patterns should be cached until invalidated")
	}
	f.Invalidate("pkg")
	if !f.Accepts("pkg/a.txt", false) || f.Accepts("pkg/b.txt", false) {
		t.Error("Invalidate did not reload the directory's patterns")
	}
}

func TestFilterExcludesSecretsByDefault(t *testing.T) {
	root := t.TempDir()
	f, err := NewFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		".env", ".env.production", "certs/server.pem", "deploy/id_ed25519", ".npmrc", ".aws/credentials", "a/.DS_Store",
		"terraform.tfvars", "infra/prod.tfstate", "infra/prod.tfstate.backup", ".terraform/providers/x", ".kube/config",
		"release.jks", "app.keystore", "vault.kdbx", ".git-credentials", ".docker/config.json", "sub/.docker/config.json",
	} {
		if f.Accepts(secret, false) {
			t.Errorf("%s should be excluded by default", secret)
		}
	}
	for _, safe := range []string{".env.example", "config/.env.sample", "keys.go", "docker/config.json", "terraform.tfvars.example"} {
		if !f.Accepts(safe, false) {
			t.Errorf("%s should be shared", safe)
		}
	}

	// A project can opt back in.
	writeTree(t, root, map[string]string{".bigaignore": "!.npmrc\n"})
	f.Invalidate("")
	if !f.Accepts(".npmrc", false) {
		t.Error(".bigaignore could not re-include a default exclusion")
	}
}
