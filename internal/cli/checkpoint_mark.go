package cli

import (
	"context"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/biga-sh/cli/internal/api"
)

func (a *app) checkpointCmd() *cobra.Command {
	var dir string
	var asJSON bool
	cmd := &cobra.Command{Use: "checkpoint", Short: "Mark a point in a session's history"}
	mark := &cobra.Command{
		Use:   "mark <name>",
		Short: "Create a named checkpoint and print its share link",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(ctx context.Context, args []string) error {
			rec, client, err := a.ownedSession(dir)
			if err != nil {
				return err
			}
			link, err := url.Parse(rec.ShareURL())
			if err != nil {
				return err
			}
			checkpoint, err := client.MarkCheckpoint(ctx, rec.SessionID, rec.Token, args[0])
			if err != nil {
				return err
			}
			query := link.Query()
			query.Set("checkpoint", strconv.Itoa(checkpoint.Seq))
			link.RawQuery = query.Encode()
			if asJSON {
				return printJSON(struct {
					api.CheckpointMark
					URL string `json:"url"`
				}{checkpoint, link.String()})
			}
			a.ui.Info(link.String())
			return nil
		}),
	}
	mark.Flags().StringVar(&dir, "dir", "", "broadcast directory (default: the current directory)")
	mark.Flags().BoolVar(&asJSON, "json", false, "print the checkpoint and share link as JSON")
	cmd.AddCommand(mark)
	return cmd
}
