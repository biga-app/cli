package api

import (
	"context"
	"crypto/rand"
	"net/http"
)

type CheckpointMark struct {
	Seq   int    `json:"seq"`
	Label string `json:"label"`
}

func (c *Client) MarkCheckpoint(ctx context.Context, sessionID, ownerToken, name string) (CheckpointMark, error) {
	var mark CheckpointMark
	route := sessionPath(sessionID, "checkpoints/marks/"+rand.Text())
	_, err := c.do(ctx, http.MethodPut, route, map[string]string{"name": name}, &mark, request{attempts: 2, token: ownerToken})
	return mark, err
}
