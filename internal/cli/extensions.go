package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/biga-sh/cli/internal/api"
	"github.com/biga-sh/cli/internal/session"
)

func (a *app) extensionsCmd() *cobra.Command {
	var dir string
	var createJSON, listJSON bool
	cmd := &cobra.Command{
		Use:   "extensions",
		Short: "Manage checkpoint feed credentials for a directory's session",
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "broadcast directory (default: the current directory)")
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a read-only checkpoint feed token",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(ctx context.Context, args []string) error {
			rec, client, err := a.extensionSession(dir)
			if err != nil {
				return err
			}
			grant, err := client.CreateExtension(ctx, rec.SessionID, rec.Token, args[0])
			if err != nil {
				return err
			}
			result := struct {
				api.ExtensionGrant
				FeedURL string `json:"feedUrl"`
			}{ExtensionGrant: grant, FeedURL: rec.APIURL + "/api/sessions/" + rec.SessionID + "/extension-events"}
			if createJSON {
				return printJSON(result)
			}
			a.ui.Info("Extension ID: " + result.ID)
			a.ui.Info("Feed URL: " + result.FeedURL)
			a.ui.Info("Token (shown once): " + result.Token)
			return nil
		}),
	}
	create.Flags().BoolVar(&createJSON, "json", false, "print the credential and feed URL as JSON")
	list := &cobra.Command{
		Use:   "list",
		Short: "List extensions without revealing tokens",
		Args:  cobra.NoArgs,
		RunE: a.run(func(ctx context.Context, _ []string) error {
			rec, client, err := a.extensionSession(dir)
			if err != nil {
				return err
			}
			extensions, err := client.Extensions(ctx, rec.SessionID, rec.Token)
			if err != nil {
				return err
			}
			if listJSON {
				return printJSON(extensions)
			}
			if len(extensions) == 0 {
				a.ui.Info("No extensions for this session.")
				return nil
			}
			rows := make([][]string, 0, len(extensions))
			for _, extension := range extensions {
				rows = append(rows, []string{extension.ID, extension.Name, extension.CreatedAt})
			}
			a.ui.Table([]string{"ID", "NAME", "CREATED"}, rows)
			return nil
		}),
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print extensions as JSON")
	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an extension's checkpoint feed token",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(ctx context.Context, args []string) error {
			rec, client, err := a.extensionSession(dir)
			if err != nil {
				return err
			}
			return client.RevokeExtension(ctx, rec.SessionID, rec.Token, args[0])
		}),
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func (a *app) extensionSession(dir string) (session.Record, *api.Client, error) {
	target, err := targetDir(dir, nil)
	if err != nil {
		return session.Record{}, nil, err
	}
	rec, found, err := a.store.Lookup(target)
	if err != nil {
		return rec, nil, err
	}
	if !found {
		return rec, nil, errors.New("no session for this directory yet; run biga start first")
	}
	if rec.Token == "" || rec.APIURL == "" {
		return rec, nil, errors.New("the saved session has no owner credentials or server URL")
	}
	if a.apiURL != "" && a.cfg.APIURL != rec.APIURL {
		return rec, nil, fmt.Errorf("this session belongs to %s; omit --api-url or use that server", rec.APIURL)
	}
	return rec, a.client(rec.APIURL), nil
}
