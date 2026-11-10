// Package cli wires the biga commands together.
package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/biga-app/cli/internal/api"
	"github.com/biga-app/cli/internal/config"
	"github.com/biga-app/cli/internal/h3"
	"github.com/biga-app/cli/internal/session"
	"github.com/biga-app/cli/internal/ui"
	"github.com/biga-app/cli/internal/update"
	"github.com/biga-app/cli/internal/version"
)

// Exit codes.
const (
	exitOK          = 0
	exitError       = 1
	exitInterrupted = 130
)

// Execute runs the CLI with os.Args and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first interrupt arrives, restore the default handling so a
	// second Ctrl+C during a slow shutdown exits immediately.
	context.AfterFunc(ctx, stop)

	a := &app{ui: ui.New(os.Stdout), interrupt: stop}
	err := a.rootCmd().ExecuteContext(ctx)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, ui.ErrAborted):
		a.ui.Cancel("Cancelled.")
		return exitOK
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		a.ui.Cancel("Cancelled.")
		return exitInterrupted
	case a.framed:
		a.ui.Error(describe(err))
		return exitError
	case a.ran:
		fmt.Fprintf(os.Stderr, "Error: %s\n", describe(err))
		return exitError
	default:
		// The command line itself was wrong: an unknown command or flag.
		fmt.Fprintf(os.Stderr, "Error: %s\nRun 'biga --help' for usage.\n", describe(err))
		return exitError
	}
}

// describe adds what to do next to errors a user can act on.
func describe(err error) string {
	if api.IsUnreachable(err) {
		return fmt.Sprintf("%v\nIs the server running? Point biga at another one with --api-url or $%s.", err, config.EnvAPIURL)
	}
	return err.Error()
}

// app holds what commands share once the configuration is loaded.
type app struct {
	ui *ui.UI
	// ran and framed report whether a command body started, and whether the
	// output frame was opened, which decide how a failure is printed.
	ran, framed bool
	// interrupt behaves like a first Ctrl+C: it cancels the command's context
	// and restores default signal handling, so another Ctrl+C exits at once.
	interrupt context.CancelFunc

	apiURL string // --api-url
	cfg    config.Config
	store  *session.Store
	// transport is shared by every client so HTTP/3 connections are reused.
	transport http.RoundTripper
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "biga",
		Short: "Broadcast a local directory so others can watch it live",
		Long: `biga broadcasts a local directory so others can watch it change in real time.
There are no accounts or sign-in: the session ID is the share link.

Run biga without a command in a terminal to pick one interactively.`,
		Version:       version.String(),
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.ui.Interactive() || !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmd.Help()
			}
			return a.run(a.pick)(cmd, args)
		},
	}
	root.PersistentFlags().StringVar(&a.apiURL, "api-url", "",
		fmt.Sprintf("server to use (default $%s or %s)", config.EnvAPIURL, config.DefaultAPIURL))
	root.AddCommand(a.startCmd(), a.listCmd(), a.statusCmd(), a.stopCmd(), a.openCmd(), a.extensionsCmd(), a.checkpointCmd())
	return root
}

// run wraps a command body with configuration loading and the output frame.
// Commands printing --json skip the frame, so their output stays parseable.
func (a *app) run(fn func(ctx context.Context, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a.ran = true
		if err := a.setup(); err != nil {
			return err
		}
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			return fn(cmd.Context(), args)
		}

		a.ui.Intro("biga")
		a.framed = true
		a.notifyUpdate(cmd.Context())
		if err := fn(cmd.Context(), args); err != nil {
			return err
		}
		a.ui.Outro("Done")
		return nil
	}
}

func (a *app) setup() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if a.apiURL != "" {
		if cfg.APIURL, err = config.NormalizeAPIURL(a.apiURL); err != nil {
			return fmt.Errorf("--api-url: %w", err)
		}
	}
	a.cfg = cfg
	a.store = session.NewStore(cfg.Home)
	return nil
}

func (a *app) client(baseURL string) *api.Client {
	ua := fmt.Sprintf("biga-cli/%s (%s/%s)", version.String(), runtime.GOOS, runtime.GOARCH)
	if a.transport == nil {
		a.transport = h3.New(http.DefaultTransport, h3.Options{Mode: h3.ParseMode(a.cfg.HTTP3)})
	}
	return api.New(baseURL,
		api.WithUserAgent(ua),
		api.WithAccessKey(a.cfg.AccessKey),
		api.WithHTTPClient(&http.Client{Transport: a.transport}),
	)
}

// notifyUpdate mentions a newer release learned of by an earlier run, and
// refreshes that knowledge in the background without delaying the command.
func (a *app) notifyUpdate(ctx context.Context) {
	if !a.cfg.UpdateCheck || !a.ui.Interactive() {
		return
	}
	checker := &update.Checker{
		Current:   version.String(),
		CachePath: filepath.Join(a.cfg.Home, "update-check.json"),
	}
	if latest, ok := checker.Available(); ok {
		a.ui.Warn(fmt.Sprintf("Update available: %s → %s\nDownload it from %s", version.String(), strings.TrimPrefix(latest, "v"), update.ReleasesURL))
	}
	go func() { _ = checker.Refresh(context.WithoutCancel(ctx)) }()
}

// pick asks which command to run when biga is started without one.
func (a *app) pick(ctx context.Context, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	commands := []struct {
		ui.Choice
		run func(context.Context) error
	}{
		{ui.Choice{Value: "start", Label: "start", Hint: "Start broadcasting and watch files"},
			func(ctx context.Context) error { return a.start(ctx, startOptions{}) }},
		{ui.Choice{Value: "list", Label: "list", Hint: "List all known sessions and whether they are live"},
			func(context.Context) error { return a.list(false) }},
		{ui.Choice{Value: "status", Label: "status", Hint: "Show the session for this directory"},
			func(context.Context) error { return a.status(cwd, false) }},
		{ui.Choice{Value: "stop", Label: "stop", Hint: "Stop this directory's session"},
			func(ctx context.Context) error { return a.stop(ctx, cwd, false) }},
		{ui.Choice{Value: "open", Label: "open", Hint: "Open this directory's session in the browser"},
			func(context.Context) error { return a.open(cwd) }},
	}
	choices := make([]ui.Choice, len(commands))
	for i, c := range commands {
		choices[i] = c.Choice
	}

	picked, err := a.ui.Select(ctx, "What do you want to do?", choices)
	if err != nil {
		return err
	}
	for _, c := range commands {
		if c.Value == picked {
			return c.run(ctx)
		}
	}
	return fmt.Errorf("unknown command %q", picked)
}

// targetDir resolves the directory a status or stop command acts on, from
// either a positional argument or --dir, defaulting to the working directory.
func targetDir(flag string, args []string) (string, error) {
	dir := flag
	if len(args) > 0 {
		if dir != "" {
			return "", errors.New("pass the directory either as an argument or with --dir, not both")
		}
		dir = args[0]
	}
	if dir == "" {
		dir = "."
	}
	return filepath.Abs(dir)
}
