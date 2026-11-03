package api

import (
	"context"
	"net/http"
	"net/url"
)

type Extension struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

type ExtensionGrant struct {
	Extension
	Token string `json:"token"`
}

func (c *Client) CreateExtension(ctx context.Context, sessionID, ownerToken, name string) (ExtensionGrant, error) {
	var grant ExtensionGrant
	_, err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "extensions"), map[string]string{"name": name}, &grant, request{attempts: 1, token: ownerToken})
	return grant, err
}

func (c *Client) Extensions(ctx context.Context, sessionID, ownerToken string) ([]Extension, error) {
	var list struct {
		Extensions []Extension `json:"extensions"`
	}
	_, err := c.do(ctx, http.MethodGet, sessionPath(sessionID, "extensions"), nil, &list, request{attempts: 2, token: ownerToken})
	return list.Extensions, err
}

func (c *Client) RevokeExtension(ctx context.Context, sessionID, ownerToken, id string) error {
	_, err := c.do(ctx, http.MethodDelete, sessionPath(sessionID, "extensions/"+url.PathEscape(id)), nil, nil, request{attempts: 2, token: ownerToken})
	return err
}
