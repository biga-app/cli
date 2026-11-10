package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/biga-app/cli/internal/browser"
	"github.com/biga-app/cli/internal/human"
	"github.com/biga-app/cli/internal/session"
	"github.com/biga-app/cli/internal/ui"
)

const (
	// stopWait is how long `biga stop` waits for a broadcast to confirm it
	// shut down.
	stopWait     = 10 * time.Second
	stopPollTick = 250 * time.Millisecond
)

func (a *app) listCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all known sessions and whether they are live",
		Args:  cobra.NoArgs,
		RunE:  a.run(func(context.Context, []string) error { return a.list(asJSON) }),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the sessions as JSON")
	return cmd
}

func (a *app) statusCmd() *cobra.Command {
	var (
		dir    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "status [dir]",
		Short: "Show the session for a directory, or a summary of all sessions",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.run(func(_ context.Context, args []string) error {
			target, err := targetDir(dir, args)
			if err != nil {
				return err
			}
			return a.status(target, asJSON)
		}),
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to inspect (default: the current directory)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the session as JSON")
	cmd.MarkFlagDirname("dir")
	return cmd
}

func (a *app) stopCmd() *cobra.Command {
	var (
		dir string
		all bool
	)
	cmd := &cobra.Command{
		Use:   "stop [dir]",
		Short: "Stop a directory's session, or every session with --all",
		Long: `Stop a broadcast. A running broadcast is asked to shut down cleanly, so it
sends its final changes before ending the session. A session whose process
is gone is ended on the server and removed from the local registry.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.run(func(ctx context.Context, args []string) error {
			if all && (dir != "" || len(args) > 0) {
				return errors.New("--all cannot be combined with a directory")
			}
			target, err := targetDir(dir, args)
			if err != nil {
				return err
			}
			return a.stop(ctx, target, all)
		}),
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory whose session to stop (default: the current directory)")
	cmd.Flags().BoolVar(&all, "all", false, "stop every known session")
	cmd.MarkFlagDirname("dir")
	return cmd
}

func (a *app) openCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "open [dir]",
		Short: "Open a directory's session in the browser",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.run(func(_ context.Context, args []string) error {
			target, err := targetDir(dir, args)
			if err != nil {
				return err
			}
			return a.open(target)
		}),
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory whose session to open (default: the current directory)")
	cmd.MarkFlagDirname("dir")
	return cmd
}

// sessionJSON is the --json shape of a session record.
type sessionJSON struct {
	SessionID         string     `json:"sessionId"`
	Title             string     `json:"title"`
	RootDir           string     `json:"rootDir"`
	ShareURL          string     `json:"shareUrl"`
	APIURL            string     `json:"apiUrl"`
	Live              bool       `json:"live"`
	PID               int        `json:"pid,omitzero"`
	StartedAt         time.Time  `json:"startedAt"`
	LastSeenAt        *time.Time `json:"lastSeenAt,omitempty"`
	CheckpointsSynced int64      `json:"checkpointsSynced"`
}

func toJSON(r session.Record, now time.Time) sessionJSON {
	out := sessionJSON{
		SessionID:         r.SessionID,
		Title:             r.Title,
		RootDir:           r.RootDir,
		ShareURL:          r.ShareURL(),
		APIURL:            r.APIURL,
		Live:              r.Live(now),
		StartedAt:         r.StartedAt,
		CheckpointsSynced: r.CheckpointsSynced,
	}
	if out.Live {
		out.PID = r.PID
	}
	if !r.LastSeenAt.IsZero() {
		out.LastSeenAt = &r.LastSeenAt
	}
	return out
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) list(asJSON bool) error {
	records, err := a.store.List()
	if err != nil {
		return err
	}
	now := time.Now()
	if asJSON {
		out := make([]sessionJSON, len(records))
		for i, r := range records {
			out[i] = toJSON(r, now)
		}
		return printJSON(out)
	}

	if len(records) == 0 {
		a.ui.Info("No sessions yet. Run: biga start")
		return nil
	}
	var rows [][]string
	for _, r := range records {
		rows = append(rows, []string{state(r, now), r.Title, shortPath(r.RootDir), r.ShareURL(), lastActive(r, now)})
	}
	a.ui.Info(human.Count(len(records), "session", "sessions"))
	a.ui.Table([]string{"", "TITLE", "DIRECTORY", "LINK", "LAST ACTIVE"}, rows)
	return nil
}

func (a *app) status(dir string, asJSON bool) error {
	rec, ok, err := a.store.Lookup(dir)
	if err != nil {
		return err
	}
	now := time.Now()
	if asJSON {
		if !ok {
			return fmt.Errorf("no session for %s", dir)
		}
		return printJSON(toJSON(rec, now))
	}

	if ok {
		details := [][]string{
			{ui.Faint("Link"), rec.ShareURL()},
			{ui.Faint("Directory"), shortPath(rec.RootDir)},
			{ui.Faint("Started"), fmt.Sprintf("%s (%s)", rec.StartedAt.Local().Format("2006-01-02 15:04"), human.Ago(rec.StartedAt, now))},
			{ui.Faint("Synced"), human.Count(int(rec.CheckpointsSynced), "checkpoint", "checkpoints")},
		}
		if rec.Live(now) {
			details = append(details, []string{ui.Faint("Process"), "pid " + strconv.Itoa(rec.PID)})
		} else if !rec.LastSeenAt.IsZero() {
			details = append(details, []string{ui.Faint("Last active"), human.Ago(rec.LastSeenAt, now)})
		}
		a.ui.Info(fmt.Sprintf("%s (%s) · %s", rec.Title, rec.SessionID, state(rec, now)))
		a.ui.Table(nil, details)
		return nil
	}

	records, err := a.store.List()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		a.ui.Warn("No sessions found. Run: biga start")
		return nil
	}
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rel, err := filepath.Rel(dir, r.RootDir)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = shortPath(r.RootDir)
		}
		rows = append(rows, []string{state(r, now), r.Title, rel})
	}
	a.ui.Info("No session for this directory. Known sessions:")
	a.ui.Table(nil, rows)
	return nil
}

func (a *app) stop(ctx context.Context, dir string, all bool) error {
	targets, err := a.stopTargets(dir, all)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		if all {
			a.ui.Warn("No sessions to stop.")
		} else {
			a.ui.Warn("No session found for this directory.")
		}
		return nil
	}

	for _, r := range targets {
		if !r.Live(time.Now()) {
			a.cleanUp(ctx, r)
			continue
		}
		// A live broadcast owns its watcher and network state; asking it to
		// stop lets it send its final checkpoint and end the session itself.
		if err := a.store.RequestStop(r.SessionID); err != nil {
			return err
		}
		spin := a.ui.Spin(fmt.Sprintf("Stopping %s (%s)…", r.Title, r.SessionID))
		stopped, err := a.waitStopped(ctx, r.RootDir)
		switch {
		case err != nil:
			spin.Fail(fmt.Sprintf("Stopping %s was interrupted", r.Title))
			return err
		case stopped:
			spin.Stop("Stopped " + r.Title)
		default:
			spin.Fail(fmt.Sprintf("%s did not confirm in time; it stops on its next check", r.Title))
		}
	}
	return nil
}

func (a *app) open(dir string) error {
	rec, ok, err := a.store.Lookup(dir)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no session for this directory yet; run `biga start` first")
	}
	if err := browser.Open(rec.ShareURL()); err != nil {
		return fmt.Errorf("could not open a browser (%w); the link is %s", err, rec.ShareURL())
	}
	a.ui.Success("Opened " + rec.ShareURL())
	return nil
}

func (a *app) stopTargets(dir string, all bool) ([]session.Record, error) {
	if all {
		return a.store.List()
	}
	rec, ok, err := a.store.Lookup(dir)
	if err != nil || !ok {
		return nil, err
	}
	return []session.Record{rec}, nil
}

// cleanUp ends a session whose broadcasting process is gone and forgets it.
func (a *app) cleanUp(ctx context.Context, r session.Record) {
	endCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Best effort: the server may be unreachable, or the session long ended.
	_ = a.client(r.APIURL).EndSession(endCtx, r.SessionID, r.Token)
	_ = a.store.ClearStop(r.SessionID)
	if err := a.store.Remove(r.RootDir); err != nil {
		a.ui.Warn(fmt.Sprintf("Could not remove %s from the registry: %v", r.Title, err))
		return
	}
	a.ui.Success(fmt.Sprintf("Cleaned up ended session %s.", r.Title))
}

// waitStopped polls the registry until the session for rootDir is no longer
// live, reporting false if that takes longer than stopWait.
func (a *app) waitStopped(ctx context.Context, rootDir string) (bool, error) {
	ticker := time.NewTicker(stopPollTick)
	defer ticker.Stop()
	deadline := time.After(stopWait)
	for {
		rec, ok, err := a.store.Lookup(rootDir)
		if err != nil {
			return false, err
		}
		if !ok || !rec.Live(time.Now()) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline:
			return false, nil
		case <-ticker.C:
		}
	}
}

func state(r session.Record, now time.Time) string {
	if r.Live(now) {
		return ui.Good("● live")
	}
	return ui.Faint("○ stopped")
}

func lastActive(r session.Record, now time.Time) string {
	switch {
	case r.Live(now):
		return "now"
	case r.LastSeenAt.IsZero():
		return "—"
	default:
		return human.Ago(r.LastSeenAt, now)
	}
}

// shortPath abbreviates the home directory as ~.
func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		if rel == "." {
			return "~"
		}
		return filepath.Join("~", rel)
	}
	return p
}
