package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/broadcast"
	"github.com/biga-app/cli/internal/browser"
	"github.com/biga-app/cli/internal/config"
	"github.com/biga-app/cli/internal/human"
	"github.com/biga-app/cli/internal/secrets"
	"github.com/biga-app/cli/internal/session"
	"github.com/biga-app/cli/internal/ui"
	"github.com/biga-app/cli/internal/workspace"
)

// shutdownTimeout bounds how long stopping waits for final uploads.
const shutdownTimeout = 10 * time.Second

type startOptions struct {
	dirs         []string
	title        string
	session      string
	open         bool
	only         []string
	dryRun       bool
	noSecretScan bool
}

func (a *app) startCmd() *cobra.Command {
	var opts startOptions
	cmd := &cobra.Command{
		Use:   "start [dir...]",
		Short: "Start broadcasting and watch files",
		Long: `Broadcast one or more directories and stream every saved change to viewers.

What gets broadcast, in order of precedence:
  1. directories given as arguments or with --dir
  2. the projects listed in ./` + workspace.FileName + `, linked as one workspace
  3. the current directory

Each directory remembers its session, so running start again in the same
place resumes the same share URL.

Files excluded by .gitignore files (at any depth), .bigaignore files, and
your global git excludes are never shared, and neither are node_modules,
.git, .env files, or private keys and credentials. Files whose content looks
like a secret (a private key, a cloud or API token) are withheld too; add
` + secrets.AllowMarker + ` to a line to share it anyway.

To share only part of a project, list it with --only; nothing else is read.
Use --dry-run to see what would be shared without contacting the server.

In a terminal, press c to copy the link, o to open it, and q to stop.`,
		Example: `  biga start
  biga start ./backend ./frontend
  biga start --only src --only README.md
  biga start --dry-run
  biga start --title "Live coding" --session my-talk --open`,
		RunE: a.run(func(ctx context.Context, args []string) error {
			opts.dirs = append(opts.dirs, args...)
			return a.start(ctx, opts)
		}),
	}
	cmd.Flags().StringArrayVar(&opts.dirs, "dir", nil, "directory to broadcast (repeatable)")
	cmd.Flags().StringVar(&opts.title, "title", "", "session title, when broadcasting a single directory")
	cmd.Flags().StringVar(&opts.session, "session", "", "session ID to use instead of the directory's saved one")
	cmd.Flags().BoolVar(&opts.open, "open", false, "open the share link in your browser once live")
	cmd.Flags().StringArrayVar(&opts.only, "only", nil, "share only this file or directory, relative to the current directory (repeatable)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "show what would be shared without contacting the server")
	cmd.Flags().BoolVar(&opts.noSecretScan, "no-secret-scan", false,
		fmt.Sprintf("share files even if they look like they contain secrets (or set %s=off)", config.EnvSecretScan))
	cmd.MarkFlagDirname("dir")
	return cmd
}

type project struct {
	root  string
	title string
	group string
}

func (a *app) start(ctx context.Context, opts startOptions) error {
	projects, err := resolveProjects(opts)
	if err != nil {
		return err
	}
	if len(projects) > 1 && (opts.title != "" || opts.session != "") {
		return errors.New("--title and --session can only be used when broadcasting a single directory")
	}
	if opts.session != "" {
		if err := session.ValidateID(opts.session); err != nil {
			return fmt.Errorf("--session: %w", err)
		}
	}
	if utf8.RuneCountInString(opts.title) > 120 {
		return errors.New("--title must be at most 120 characters")
	}
	if len(opts.only) > 0 && len(projects) > 1 {
		return errors.New("--only can only be used when broadcasting a single directory")
	}
	var only []string
	if len(opts.only) > 0 {
		if only, err = onlyPaths(projects[0].root, opts.only); err != nil {
			return err
		}
	}
	noSecretScan := opts.noSecretScan || !a.cfg.SecretScan
	if opts.dryRun {
		return a.dryRun(ctx, projects, only, noSecretScan)
	}

	client := a.client(a.cfg.APIURL)
	excludes := config.GitExcludesFile()
	relays := make([]*relay, len(projects))
	broadcasts := make([]*broadcast.Broadcast, len(projects))
	for i, p := range projects {
		relays[i] = &relay{index: i}
		b, err := broadcast.New(broadcast.Options{
			Client:       client,
			Store:        a.store,
			Events:       relays[i],
			APIURL:       a.cfg.APIURL,
			RootDir:      p.root,
			Title:        p.title,
			SessionID:    opts.session,
			GroupID:      p.group,
			ExcludesFile: excludes,
			Only:         only,
			NoSecretScan: noSecretScan,
			Timing:       a.cfg.Timing,
			Heartbeat:    a.cfg.Heartbeat,
		})
		if err != nil {
			return err
		}
		broadcasts[i] = b
	}

	shown := make([]ui.Project, len(broadcasts))
	for i, b := range broadcasts {
		shown[i] = ui.Project{Name: b.Title(), URL: b.ShareURL(), Dir: b.RootDir()}
	}
	view := a.ui.Monitor(ctx, shown)
	for _, r := range relays {
		r.view = view // before any broadcast starts, so before any event
	}

	work, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	var (
		started  []bool
		startErr error
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		defer view.Close()
		if started, startErr = startAll(work, broadcasts, view); startErr != nil {
			return
		}
		view.Live()
		if opts.open {
			for i, b := range broadcasts {
				if err := browser.Open(b.ShareURL()); err != nil {
					view.Log(i, ui.LevelWarn, fmt.Sprintf("Could not open a browser: %v", err))
				}
			}
		}
		waitForStop(work, broadcasts, view)
	}()

	view.Wait()
	// Quitting the live view counts as the first Ctrl+C, so another one
	// during the shutdown below exits immediately.
	a.interrupt()
	cancelWork()
	<-done
	a.stopAll(broadcasts, started)
	return startErr
}

// startAll starts every broadcast concurrently. If one fails, the rest are
// cancelled and the first failure is returned. started reports which ones
// are running.
func startAll(ctx context.Context, broadcasts []*broadcast.Broadcast, view ui.Monitor) (started []bool, err error) {
	started = make([]bool, len(broadcasts))
	g, ctx := errgroup.WithContext(ctx)
	for i, b := range broadcasts {
		g.Go(func() error {
			res, err := b.Start(ctx)
			switch {
			case errors.Is(err, context.Canceled):
				view.Failed(i, b.Title()+": cancelled")
				return err
			case err != nil:
				view.Failed(i, "Could not start "+b.Title())
				if len(broadcasts) > 1 {
					err = fmt.Errorf("%s: %w", b.Title(), err)
				}
				return err
			}
			started[i] = true
			view.Started(i, describeStart(b, res), b.ShareURL())
			return nil
		})
	}
	return started, g.Wait()
}

func describeStart(b *broadcast.Broadcast, res broadcast.Result) string {
	name := fmt.Sprintf("%s (%s)", b.Title(), b.SessionID())
	sent := fmt.Sprintf("%s, %s", human.Count(res.Files, "file", "files"), human.Bytes(int64(res.Bytes)))
	switch {
	case !b.Rejoined():
		return fmt.Sprintf("Started %s — uploaded %s", name, sent)
	case res.Sync == broadcast.SyncedNothing:
		return "Rejoined " + name + " — already in sync"
	case res.Sync == broadcast.SyncedChanges:
		return fmt.Sprintf("Rejoined %s — synced %s, %s", name,
			human.Count(res.Files, "changed file", "changed files"), human.Bytes(int64(res.Bytes)))
	default:
		return fmt.Sprintf("Rejoined %s — re-uploaded %s", name, sent)
	}
}

// waitForStop blocks until ctx is cancelled or `biga stop` has asked every
// broadcast to end. Broadcasts asked individually stop as soon as they are
// asked, while the rest keep running.
func waitForStop(ctx context.Context, broadcasts []*broadcast.Broadcast, view ui.Monitor) {
	type request struct {
		i int
		b *broadcast.Broadcast
	}
	requests := make(chan request)
	for i, b := range broadcasts {
		go func() {
			select {
			case <-b.StopRequested():
				select {
				case requests <- request{i, b}:
				case <-ctx.Done():
				}
			case <-ctx.Done():
			}
		}()
	}

	for remaining := len(broadcasts); remaining > 0; remaining-- {
		select {
		case <-ctx.Done():
			return
		case r := <-requests:
			stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = r.b.Stop(stopCtx) // the final summary reports how it went
			cancel()
			view.Log(r.i, ui.LevelInfo, "Stopped by `biga stop`.")
		}
	}
}

// stopAll stops the started broadcasts concurrently, giving their final
// uploads a bounded amount of time, and reports each one.
func (a *app) stopAll(broadcasts []*broadcast.Broadcast, started []bool) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	errs := make([]error, len(broadcasts))
	var wg sync.WaitGroup
	for i, b := range broadcasts {
		wg.Go(func() { errs[i] = b.Stop(ctx) })
	}
	wg.Wait()

	for i, b := range broadcasts {
		if i >= len(started) || !started[i] {
			continue
		}
		if errs[i] != nil {
			a.ui.Warn(fmt.Sprintf("Could not end %s on the server (%v). Its link resumes next time.", b.Title(), errs[i]))
			continue
		}
		a.ui.Success(fmt.Sprintf("Stopped %s · %s", b.Title(), human.Count(int(b.Checkpoints()), "checkpoint", "checkpoints")))
	}
}

// relay forwards one broadcast's events to the monitor.
type relay struct {
	view  ui.Monitor
	index int
}

func (r *relay) Info(msg string)     { r.view.Log(r.index, ui.LevelInfo, msg) }
func (r *relay) Warn(msg string)     { r.view.Log(r.index, ui.LevelWarn, msg) }
func (r *relay) Error(msg string)    { r.view.Log(r.index, ui.LevelError, msg) }
func (r *relay) Progress(msg string) { r.view.Progress(r.index, msg) }
func (r *relay) Online(online bool)  { r.view.Online(r.index, online) }

func (r *relay) Synced(c broadcast.Checkpoint) {
	changes := make([]ui.Change, len(c.Changes))
	for i, ch := range c.Changes {
		kind := ui.Modified
		switch ch.Type {
		case api.ChangeAdded:
			kind = ui.Added
		case api.ChangeDeleted:
			kind = ui.Deleted
		}
		changes[i] = ui.Change{Path: ch.Path, Kind: kind}
	}
	r.view.Synced(r.index, changes, c.Bytes)
}

// resolveProjects decides what to broadcast; see the start command's help.
// Only a workspace links its sessions with a shared group ID.
func resolveProjects(opts startOptions) ([]project, error) {
	if len(opts.dirs) > 0 {
		projects := make([]project, 0, len(opts.dirs))
		seen := make(map[string]bool)
		for _, dir := range opts.dirs {
			root, err := checkDir(dir)
			if err != nil {
				return nil, err
			}
			if !seen[root] {
				seen[root] = true
				projects = append(projects, project{root: root, title: opts.title})
			}
		}
		return projects, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	ws, err := workspace.Load(cwd)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return []project{{root: cwd, title: opts.title}}, nil
	}
	if opts.title != "" {
		return nil, fmt.Errorf("--title cannot be used with %s; set titles in the file instead", workspace.FileName)
	}
	projects := make([]project, 0, len(ws.Projects))
	for _, p := range ws.Projects {
		root, err := checkDir(p.RootDir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", workspace.FileName, err)
		}
		projects = append(projects, project{root: root, title: p.Title, group: ws.ID})
	}
	return projects, nil
}

// checkDir resolves dir to an absolute path and checks that it is a
// directory.
func checkDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("cannot broadcast %s: no such directory", dir)
	}
	if err != nil {
		return "", fmt.Errorf("cannot broadcast %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot broadcast %s: not a directory", dir)
	}
	return abs, nil
}
