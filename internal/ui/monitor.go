package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/biga-app/cli/internal/human"
)

// ChangeKind is how a file changed.
type ChangeKind int

const (
	Added ChangeKind = iota
	Modified
	Deleted
)

// Change is one changed file in a synced checkpoint.
type Change struct {
	Path string
	Kind ChangeKind
}

// Level is the severity of a logged message.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Project is one broadcast shown by a [Monitor].
type Project struct {
	Name string
	URL  string
	Dir  string
}

// Monitor shows running broadcasts. Methods other than Wait are safe to call
// from any goroutine; project is an index into the projects it was made with.
type Monitor interface {
	// Progress describes what a starting project is doing.
	Progress(project int, msg string)
	// Started marks a project as live, with a one-line summary and the link
	// it is shared at, which can differ from the one known before it started.
	Started(project int, msg, url string)
	// Failed marks a project as having failed to start.
	Failed(project int, msg string)
	// Synced reports a checkpoint the server accepted.
	Synced(project int, changes []Change, bytes int)
	// Online reports a change in server reachability.
	Online(project int, online bool)
	// Log shows a message about a project.
	Log(project int, level Level, msg string)
	// Live reports that every project is broadcasting.
	Live()
	// Wait blocks until the user quits, ctx is done, or Close is called.
	Wait()
	// Close ends Wait.
	Close()
}

// maxListedChanges is how many files of one checkpoint are listed by name.
const maxListedChanges = 5

// Monitor returns an interactive live view when the UI is on a terminal, and
// otherwise one that prints plain lines.
func (u *UI) Monitor(ctx context.Context, projects []Project) Monitor {
	plain := newLines(ctx, u, projects)
	if !u.tty || !term.IsTerminal(int(os.Stdin.Fd())) {
		return plain
	}
	return newLive(ctx, u, projects, plain)
}

// Step logs a completed step.
func (u *UI) Step(msg string) { u.log(green.Render(u.sym.done), msg) }

// lines is a [Monitor] that prints each event as it happens.
type lines struct {
	ctx      context.Context
	ui       *UI
	projects []Project
	closed   chan struct{}
	once     sync.Once
}

func newLines(ctx context.Context, u *UI, projects []Project) *lines {
	return &lines{ctx: ctx, ui: u, projects: projects, closed: make(chan struct{})}
}

func (l *lines) prefix(project int) string {
	if len(l.projects) < 2 {
		return ""
	}
	return "[" + l.projects[project].Name + "] "
}

func (l *lines) Progress(int, string) {}

func (l *lines) Started(project int, msg, url string) {
	if url != "" {
		l.projects[project].URL = url
	}
	l.ui.Step(msg)
}

func (l *lines) Failed(_ int, msg string) { l.ui.Error(msg) }

func (l *lines) Synced(project int, changes []Change, _ int) {
	var name string
	if len(l.projects) > 1 {
		name = l.projects[project].Name
	}
	l.ui.print(formatChanges(l.ui.sym, name, changes, time.Now()))
}

func (l *lines) Online(project int, online bool) {
	if online {
		l.ui.Success(l.prefix(project) + "Back online.")
		return
	}
	l.ui.Warn(l.prefix(project) + "Server unreachable — changes are buffered and sync when it is back.")
}

func (l *lines) Log(project int, level Level, msg string) {
	switch level {
	case LevelWarn:
		l.ui.Warn(l.prefix(project) + msg)
	case LevelError:
		l.ui.Error(l.prefix(project) + msg)
	default:
		l.ui.Info(l.prefix(project) + msg)
	}
}

func (l *lines) Live() {
	if len(l.projects) == 1 {
		p := l.projects[0]
		l.ui.Info(fmt.Sprintf("Share: %s\nWatching %s. Press Ctrl+C to stop.", p.URL, p.Dir))
		return
	}
	rows := make([]string, 0, len(l.projects)+1)
	rows = append(rows, fmt.Sprintf("Broadcasting %d projects:", len(l.projects)))
	for _, p := range l.projects {
		rows = append(rows, fmt.Sprintf("  %s: %s", p.Name, p.URL))
	}
	l.ui.Success(strings.Join(rows, "\n"))
	l.ui.Info("Watching for changes. Press Ctrl+C to stop.")
}

func (l *lines) Wait() {
	select {
	case <-l.ctx.Done():
	case <-l.closed:
	}
}

func (l *lines) Close() { l.once.Do(func() { close(l.closed) }) }

// formatChanges renders a checkpoint as framed lines, one per file, for at
// most maxListedChanges files. name labels the project when there are several.
func formatChanges(sym symbols, name string, changes []Change, at time.Time) string {
	var b strings.Builder
	stamp := gray.Render(at.Format(time.TimeOnly))
	label := ""
	if name != "" {
		label = cyan.Render(name) + "  "
	}
	for i, c := range changes {
		if i == maxListedChanges {
			more := human.Count(len(changes)-i, "more file", "more files")
			b.WriteString(gray.Render(sym.bar) + "  " + stamp + "  " + label + gray.Render("… "+more) + "\n")
			break
		}
		b.WriteString(gray.Render(sym.bar) + "  " + stamp + "  " + label + changeMark(c.Kind) + " " + c.Path + "\n")
	}
	return b.String()
}

func changeMark(k ChangeKind) string {
	switch k {
	case Added:
		return green.Render("+")
	case Deleted:
		return red.Render("−")
	default:
		return yellow.Render("~")
	}
}
