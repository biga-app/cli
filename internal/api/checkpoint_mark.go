package api

import (
	"context"
	"crypto/rand"
	"net/http"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

type CheckpointMark struct {
	Seq   int    `json:"seq"`
	Label string `json:"label"`
}

func (c *Client) MarkCheckpoint(ctx context.Context, sessionID, ownerToken, name string) (CheckpointMark, error) {
	var summary bigav1.CheckpointSummary
	route := sessionPath(sessionID, "checkpoints/marks/"+rand.Text())
	if _, err := c.do(ctx, http.MethodPut, route, &bigav1.MarkCheckpointRequest{Name: name}, &summary, request{attempts: 2, token: ownerToken}); err != nil {
		return CheckpointMark{}, err
	}
	return CheckpointMark{Seq: int(summary.GetSeq()), Label: summary.GetLabel()}, nil
}
