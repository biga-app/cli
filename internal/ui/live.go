package ui

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/biga-app/cli/internal/browser"
	"github.com/biga-app/cli/internal/human"
)

const (
	frameInterval = 100 * time.Millisecond
	noticeFor     = 2500 * time.Millisecond
)

// live is the interactive [Monitor]: a status panel pinned to the bottom of
// the terminal, with events scrolling above it and single-key shortcuts.
type live struct {
	plain *lines
	p     *tea.Program
	// exited is set once the program has finished, after which events are
	// printed as plain lines instead.
	exited atomic.Bool
}

func newLive(ctx context.Context, u *UI, projects []Project, plain *lines) *live {
	m := &liveModel{sym: u.sym, projects: make([]projectState, len(projects))}
	for i, p := range projects {
		m.projects[i] = projectState{Project: p}
	}
	return &live{
		plain: plain,
		// Interrupts arrive as key presses while the program runs; signals
		// from outside (SIGTERM) cancel ctx, which ends the program.
		p: tea.NewProgram(m, tea.WithContext(ctx), tea.WithoutSignalHandler()),
	}
}

func (l *live) send(msg tea.Msg, fallback func()) {
	if l.exited.Load() {
		fallback()
		return
	}
	l.p.Send(msg)
}

func (l *live) Progress(i int, msg string) {
	l.send(progressMsg{i, msg}, func() {})
}

func (l *live) Started(i int, msg, url string) {
	l.send(startedMsg{i, msg, url}, func() { l.plain.Started(i, msg, url) })
}

func (l *live) Failed(i int, msg string) {
	l.send(failedMsg{i, msg}, func() { l.plain.Failed(i, msg) })
}

func (l *live) Synced(i int, changes []Change, bytes int) {
	l.send(syncedMsg{i, changes, bytes, time.Now()}, func() { l.plain.Synced(i, changes, bytes) })
}

func (l *live) Online(i int, online bool) {
	l.send(onlineMsg{i, online}, func() { l.plain.Online(i, online) })
}

func (l *live) Log(i int, level Level, msg string) {
	l.send(logMsg{i, level, msg}, func() { l.plain.Log(i, level, msg) })
}

func (l *live) Live() { l.send(liveMsg{}, func() {}) }

func (l *live) Wait() {
	_, _ = l.p.Run() // a killed program (ctx done) is a normal way to end
	l.exited.Store(true)
}

func (l *live) Close() { l.send(closeMsg{}, func() {}) }

type (
	progressMsg struct {
		i   int
		msg string
	}
	startedMsg struct {
		i        int
		msg, url string
	}
	failedMsg struct {
		i   int
		msg string
	}
	syncedMsg struct {
		i       int
		changes []Change
		bytes   int
		at      time.Time
	}
	onlineMsg struct {
		i      int
		online bool
	}
	logMsg struct {
		i     int
		level Level
		msg   string
	}
	liveMsg            struct{}
	closeMsg           struct{}
	frameMsg           struct{}
	noticeMsg          string
	clipboardFailedMsg string
)

type projectStatus int

const (
	starting projectStatus = iota
	running
	failed
)

type projectState struct {
	Project
	status      projectStatus
	detail      string
	offline     bool
	checkpoints int
	files       int
	bytes       int64
	lastSync    time.Time
}

type liveModel struct {
	sym      symbols
	projects []projectState
	live     bool
	quitting bool
	width    int
	frame    int
	notice   string
	noticeAt time.Time
}

func frame() tea.Cmd {
	return tea.Tick(frameInterval, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m *liveModel) Init() tea.Cmd { return frame() }

func (m *liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case frameMsg:
		m.frame++
		return m, frame()
	case tea.KeyPressMsg:
		return m, m.key(msg.String())
	case noticeMsg:
		m.notice, m.noticeAt = string(msg), time.Now()
	case clipboardFailedMsg:
		// No system clipboard (e.g. over SSH): ask the terminal instead.
		return m, tea.Batch(tea.SetClipboard(string(msg)), notice("Copied through the terminal"))

	case progressMsg:
		m.projects[msg.i].detail = msg.msg
	case startedMsg:
		m.projects[msg.i].status, m.projects[msg.i].detail = running, ""
		if msg.url != "" {
			m.projects[msg.i].URL = msg.url
		}
		return m, tea.Println(entry(m.sym, green.Render(m.sym.done), msg.msg))
	case failedMsg:
		m.projects[msg.i].status, m.projects[msg.i].detail = failed, msg.msg
		return m, tea.Println(entry(m.sym, red.Render(m.sym.failure), msg.msg))
	case syncedMsg:
		p := &m.projects[msg.i]
		p.checkpoints++
		p.files += len(msg.changes)
		p.bytes += int64(msg.bytes)
		p.lastSync = msg.at
		return m, tea.Println(strings.TrimSuffix(formatChanges(m.sym, m.label(msg.i), msg.changes, msg.at), "\n"))
	case onlineMsg:
		m.projects[msg.i].offline = !msg.online
		if msg.online {
			return m, tea.Println(entry(m.sym, green.Render(m.sym.success), m.prefix(msg.i)+"Back online."))
		}
		return m, tea.Println(entry(m.sym, yellow.Render(m.sym.warn), m.prefix(msg.i)+"Server unreachable — changes are buffered and sync when it is back."))
	case logMsg:
		symbol := blue.Render(m.sym.info)
		switch msg.level {
		case LevelWarn:
			symbol = yellow.Render(m.sym.warn)
		case LevelError:
			symbol = red.Render(m.sym.failure)
		}
		return m, tea.Println(entry(m.sym, symbol, m.prefix(msg.i)+msg.msg))
	case liveMsg:
		m.live = true
	case closeMsg:
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *liveModel) key(k string) tea.Cmd {
	switch k {
	case "q", "ctrl+c", "ctrl+d":
		m.quitting = true
		return tea.Quit
	case "c":
		if links := m.links(); links != "" {
			return copyLinks(links, len(m.projects) > 1)
		}
	case "o":
		return openLinks(m.urls())
	}
	return nil
}

func notice(s string) tea.Cmd { return func() tea.Msg { return noticeMsg(s) } }

func copyLinks(text string, many bool) tea.Cmd {
	return func() tea.Msg {
		if err := clipboard.WriteAll(text); err != nil {
			return clipboardFailedMsg(text)
		}
		if many {
			return noticeMsg("Links copied")
		}
		return noticeMsg("Link copied")
	}
}

func openLinks(urls []string) tea.Cmd {
	return func() tea.Msg {
		for _, u := range urls {
			if err := browser.Open(u); err != nil {
				return noticeMsg("Could not open a browser: " + err.Error())
			}
		}
		return noticeMsg("Opened in your browser")
	}
}

func (m *liveModel) urls() []string {
	urls := make([]string, 0, len(m.projects))
	for _, p := range m.projects {
		if p.status == running {
			urls = append(urls, p.URL)
		}
	}
	return urls
}

func (m *liveModel) links() string { return strings.Join(m.urls(), "\n") }

// label names the project in event lines when there are several.
func (m *liveModel) label(i int) string {
	if len(m.projects) < 2 {
		return ""
	}
	return m.projects[i].Name
}

func (m *liveModel) prefix(i int) string {
	if len(m.projects) < 2 {
		return ""
	}
	return "[" + m.projects[i].Name + "] "
}

func (m *liveModel) View() tea.View {
	bar := gray.Render(m.sym.bar)
	rows := []string{bar}
	for _, p := range m.projects {
		rows = append(rows, m.projectRows(p)...)
	}
	if !m.quitting {
		rows = append(rows, bar, bar+"  "+m.hints())
	}
	if m.width > 0 {
		for i, r := range rows {
			rows[i] = ansi.Truncate(r, m.width, "…")
		}
	}
	return tea.NewView(strings.Join(rows, "\n"))
}

func (m *liveModel) spinner(style func(...string) string) string {
	return style(m.sym.frames[m.frame%len(m.sym.frames)])
}

func (m *liveModel) projectRows(p projectState) []string {
	bar := gray.Render(m.sym.bar)
	name := bold.Render(p.Name)
	switch p.status {
	case failed:
		return []string{red.Render(m.sym.failure) + "  " + name + "  " + red.Render(p.detail)}
	case starting:
		return []string{m.spinner(magenta.Render) + "  " + name + "  " + gray.Render(cmp.Or(p.detail, "Starting…"))}
	}

	symbol := green.Render(m.sym.info)
	switch {
	case m.quitting:
		symbol = gray.Render(m.sym.done)
	case p.offline:
		symbol = m.spinner(yellow.Render)
	}
	head := symbol + "  " + name + "  " + cyan.Render(p.URL)

	var stats string
	switch {
	case p.offline && !m.quitting:
		stats = yellow.Render("reconnecting — changes are buffered")
	case p.checkpoints == 0:
		stats = gray.Render("live · waiting for changes")
	default:
		stats = gray.Render(fmt.Sprintf("live · %s · %s · %s sent · last sync %s",
			human.Count(p.checkpoints, "checkpoint", "checkpoints"),
			human.Count(p.files, "file", "files"),
			human.Bytes(p.bytes),
			human.Ago(p.lastSync, time.Now())))
	}
	return []string{head, bar + "  " + stats}
}

func (m *liveModel) hints() string {
	if m.notice != "" && time.Since(m.noticeAt) < noticeFor {
		return green.Render(m.notice)
	}
	if !m.live {
		return gray.Render("q cancel")
	}
	link := "c copy link"
	if len(m.projects) > 1 {
		link = "c copy links"
	}
	return gray.Render(link + " · o open in browser · q stop")
}
