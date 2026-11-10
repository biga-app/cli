package ignore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// stubDirs are build-output directories that always appear in the shared tree
// as empty, contents-hidden stubs, regardless of ignore files. Viewers can see
// the folder exists, but nothing inside it is watched, hashed, or uploaded.
var stubDirs = map[string]struct{}{
	"dist":     {},
	".next":    {},
	"build":    {},
	"out":      {},
	"coverage": {},
	".build":   {},
}

// defaultPatterns apply to every broadcast, beneath any ignore file. Share
// links are public, so common credential files are excluded; a .gitignore or
// .bigaignore can still re-include one with "!name".
var defaultPatterns = []string{
	"node_modules/",
	".git/",
	".DS_Store",
	".env",
	".env.*",
	"!.env.example",
	"!.env.sample",
	"!.env.template",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"id_rsa*",
	"id_dsa*",
	"id_ecdsa*",
	"id_ed25519*",
	".npmrc",
	".pypirc",
	".netrc",
	".ssh/",
	".aws/",
	".gnupg/",
	".kube/",
	".terraform/",
	"*.tfvars",
	"*.tfstate",
	"*.tfstate.*",
	".git-credentials",
	"*.jks",
	"*.keystore",
	"*.kdbx",
	"**/.docker/config.json",
}

// patternFiles are read from every directory, in order, so .bigaignore rules
// override .gitignore rules in the same directory.
var patternFiles = []string{".gitignore", ".bigaignore"}

// IsStubDir reports whether a directory with the given base name is shown as
// a contents-hidden stub.
func IsStubDir(name string) bool {
	_, ok := stubDirs[name]
	return ok
}

// IsPatternFile reports whether a file with the given base name holds ignore
// patterns, so a change to it changes what the filter accepts.
func IsPatternFile(name string) bool {
	for _, f := range patternFiles {
		if name == f {
			return true
		}
	}
	return false
}

// Filter decides which paths under a root directory are broadcast. It
// follows git's rules: every directory's .gitignore applies to the paths
// beneath it, deeper files override shallower ones, and nothing inside an
// excluded directory can be re-included. .bigaignore files work the same way
// for broadcast-only rules.
//
// Pattern files are read lazily and cached; call [Filter.Invalidate] when
// one changes. A Filter is safe for concurrent use.
type Filter struct {
	root       string
	ignoreCase bool
	base       *Matcher
	only       []string // slash-separated, folded, relative to root; empty means everything

	mu   sync.Mutex
	dirs map[string]*Matcher // by slash-separated directory, "" for the root; nil when it has no patterns
}

// NewFilter builds the filter for root. Beneath the per-directory files sit,
// in increasing precedence, the defaults, each of extraFiles (such as the
// user's global git excludes), and the repository's .git/info/exclude.
// Missing files are skipped. Matching follows the platform's usual git
// behavior: case-insensitive on macOS and Windows.
func NewFilter(root string, extraFiles ...string) (*Filter, error) {
	f := &Filter{
		root:       root,
		ignoreCase: runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		dirs:       make(map[string]*Matcher),
	}
	f.base = NewMatcher(f.ignoreCase)
	f.base.AddPatterns(defaultPatterns...)
	for _, file := range append(extraFiles, filepath.Join(root, ".git", "info", "exclude")) {
		if file == "" {
			continue
		}
		if err := addFile(f.base, file); err != nil {
			return nil, err
		}
	}

	// Load the root's own files now, so an unreadable .gitignore is an error
	// instead of silently sharing what it meant to hide.
	m, err := f.load("")
	if err != nil {
		return nil, err
	}
	f.dirs[""] = m
	return f, nil
}

// Restrict limits what is broadcast to the given files and directories,
// slash-separated and relative to the root. Everything else is rejected, and
// the ignore rules still apply inside them. It must be called before the
// filter is shared between goroutines. No paths, or the root itself, lift the
// restriction.
func (f *Filter) Restrict(roots ...string) {
	f.only = nil
	for _, r := range roots {
		r = path.Clean(filepath.ToSlash(r))
		if r == "." {
			f.only = nil
			return
		}
		f.only = append(f.only, f.fold(r))
	}
}

// InScope reports whether the path is within what [Filter.Restrict] allows:
// inside a listed path, or a directory leading to one. Without a restriction
// everything is in scope.
func (f *Filter) InScope(rel string, isDir bool) bool {
	if len(f.only) == 0 {
		return true
	}
	rel = f.fold(rel)
	for _, root := range f.only {
		if rel == root || strings.HasPrefix(rel, root+"/") {
			return true
		}
		if isDir && strings.HasPrefix(root, rel+"/") {
			return true
		}
	}
	return false
}

func (f *Filter) fold(s string) string {
	if f.ignoreCase {
		return strings.ToLower(s)
	}
	return s
}

// Accepts reports whether the slash-separated path, relative to the root, is
// broadcast. The root itself, paths outside it, and anything beneath a stub
// directory are never accepted. Stub directories themselves are rejected too;
// callers that render stubs check [IsStubDir] first.
func (f *Filter) Accepts(rel string, isDir bool) bool {
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return false
	}
	if !f.InScope(rel, isDir) {
		return false
	}
	dir, name := path.Split(rel)
	for seg := range strings.SplitSeq(strings.TrimSuffix(dir, "/"), "/") {
		if IsStubDir(seg) {
			return false
		}
	}
	if isDir && IsStubDir(name) {
		return false
	}
	for i := range len(rel) {
		if rel[i] == '/' && f.ignored(rel[:i], true) {
			return false
		}
	}
	return !f.ignored(rel, isDir)
}

// Invalidate drops the cached patterns of a directory, given as a
// slash-separated path relative to the root ("" or "." for the root).
func (f *Filter) Invalidate(dir string) {
	if dir == "." {
		dir = ""
	}
	f.mu.Lock()
	delete(f.dirs, dir)
	f.mu.Unlock()
}

// ignored evaluates rel itself, without its parents: the last matching rule
// wins, and rules from deeper directories come later.
func (f *Filter) ignored(rel string, isDir bool) bool {
	_, ignored := f.base.last(rel, isDir)
	start := 0
	for {
		dir := strings.TrimSuffix(rel[:start], "/")
		if m := f.matcher(dir); m != nil {
			if matched, ign := m.last(rel[start:], isDir); matched {
				ignored = ign
			}
		}
		next := strings.IndexByte(rel[start:], '/')
		if next < 0 {
			return ignored
		}
		start += next + 1
	}
}

func (f *Filter) matcher(dir string) *Matcher {
	f.mu.Lock()
	m, ok := f.dirs[dir]
	f.mu.Unlock()
	if ok {
		return m
	}
	// An unreadable pattern file below the root counts as absent.
	m, _ = f.load(dir)
	f.mu.Lock()
	f.dirs[dir] = m
	f.mu.Unlock()
	return m
}

// load reads the pattern files of dir, returning nil when it has none.
func (f *Filter) load(dir string) (*Matcher, error) {
	var m *Matcher
	for _, name := range patternFiles {
		data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(dir), name))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path.Join(dir, name), err)
		}
		if m == nil {
			m = NewMatcher(f.ignoreCase)
		}
		m.AddPatterns(strings.Split(string(data), "\n")...)
	}
	return m, nil
}

func addFile(m *Matcher, file string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	m.AddPatterns(strings.Split(string(data), "\n")...)
	return nil
}
